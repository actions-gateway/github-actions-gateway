package proxyattribution

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type recordedEvent struct {
	regarding, related              client.Object
	eventtype, reason, action, note string
}

// captureRecorder keeps each Event with its subjects, which the client-go fake
// drops unless Verbose, and then only as a kind.
type captureRecorder struct {
	mu     sync.Mutex
	events []recordedEvent
}

func (r *captureRecorder) Eventf(regarding, related runtime.Object, eventtype, reason, action, note string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ev := recordedEvent{eventtype: eventtype, reason: reason, action: action, note: fmt.Sprintf(note, args...)}
	ev.regarding, _ = regarding.(client.Object)
	ev.related, _ = related.(client.Object)
	r.events = append(r.events, ev)
}

func (r *captureRecorder) recorded() []recordedEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedEvent(nil), r.events...)
}

func denied(seq uint64, at time.Duration, ip, host string) record {
	return record{Seq: seq, Time: t0.Add(at), SourceIP: ip, Outcome: "denied", Host: host, Port: "443"}
}

// An attributed denial is reported once, on the worker's RunnerSet with the pod
// as related, naming the host and the pool. A dial failure, and a denial that
// is discarded, report nothing.
func TestPoller_ReportsAnAttributedDenialOnce(t *testing.T) {
	p := newPKI(t)
	f, addr := startReplica(t, p, "e1",
		denied(1, 30*time.Second, "10.1.0.5", "evil.example"),
		denied(2, 31*time.Second, "10.1.0.5", "evil.example"),
		record{Seq: 3, Time: t0.Add(32 * time.Second), SourceIP: "10.1.0.5", Outcome: "dial-failed", DialClass: "timeout", Host: "api.github.com", Port: "443"},
		denied(4, 40*time.Second, "10.1.0.6", "sibling.example"),
		denied(5, 41*time.Second, "10.1.0.9", "nobody.example"),
	)
	h := newHarness(t, p, addr)
	ctx := context.Background()
	h.p.onPod(ctx, workerPod("own-1", "u1", "own", "10.1.0.5", t0))
	h.p.onPod(ctx, workerPod("sib-1", "u2", "sibling", "10.1.0.6", t0))

	require.NoError(t, h.p.poll(ctx))
	require.Equal(t, 2.0, h.records("denied", "discarded"), "the sibling's and the unknown address's denials are read and discarded")
	got := h.rec.recorded()
	require.Len(t, got, 1, "one Event: the repeat, the dial failure and the discarded denials report nothing")
	ev := got[0]
	assert.Equal(t, corev1.EventTypeWarning, ev.eventtype)
	assert.Equal(t, "EgressDestinationDenied", ev.reason)
	assert.Equal(t, "AttributeProxyFailure", ev.action)
	assert.Equal(t, "EgressProxy provider/shared refused worker pod own-1 a CONNECT to evil.example:443: the destination is not on the pool's allowlist, so a retry is refused the same way", ev.note)
	require.IsType(t, &v2alpha1.RunnerSet{}, ev.regarding)
	assert.Equal(t, "own", ev.regarding.GetName())
	require.IsType(t, &corev1.Pod{}, ev.related)
	assert.Equal(t, "own-1", ev.related.GetName())
	assert.Equal(t, "u1", string(ev.related.GetUID()))

	f.records.Store([]record{denied(6, 60*time.Second, "10.1.0.5", "evil.example"), denied(7, 61*time.Second, "10.1.0.5", "other.example")})
	require.NoError(t, h.p.poll(ctx))
	got = h.rec.recorded()
	require.Len(t, got, 2, "a later poll repeats nothing already reported and reports a new destination")
	assert.Contains(t, got[1].note, "other.example:443")
}

// One job's denials are reported for at most maxDenialEventsPerPod destinations,
// however many it is refused.
func TestPoller_CapsTheDestinationsOneJobIsReportedFor(t *testing.T) {
	p := newPKI(t)
	var recs []record
	for i := range 3 * maxDenialEventsPerPod {
		host := "h" + strconv.Itoa(i) + ".example"
		recs = append(recs, denied(uint64(2*i+1), 30*time.Second, "10.1.0.5", host), denied(uint64(2*i+2), 31*time.Second, "10.1.0.5", host))
	}
	_, addr := startReplica(t, p, "e1", recs...)
	h := newHarness(t, p, addr)
	ctx := context.Background()
	h.p.onPod(ctx, workerPod("own-1", "u1", "own", "10.1.0.5", t0))
	h.p.onPod(ctx, workerPod("own-2", "u3", "own", "10.1.0.7", t0))

	require.NoError(t, h.p.poll(ctx))
	assert.Len(t, h.rec.recorded(), maxDenialEventsPerPod)
	assert.Equal(t, float64(len(recs)), h.records("denied", "attributed"), "every denial is still attributed and counted")

	h.p.ingest(ctx, pool{"provider", "shared"}, denied(1000, 40*time.Second, "10.1.0.7", "h0.example"))
	assert.Len(t, h.rec.recorded(), maxDenialEventsPerPod+1, "the cap is per job")
}

// A RunnerSet that cannot be read, as one deleted under a running job, leaves
// the pod as the Event's subject.
func TestPoller_ReportsOnThePodWhenItsRunnerSetIsGone(t *testing.T) {
	p := newPKI(t)
	_, addr := startReplica(t, p, "e1", denied(1, 30*time.Second, "10.1.0.5", "evil.example"))
	h := newHarness(t, p, addr)
	ctx := context.Background()
	h.p.onPod(ctx, workerPod("own-1", "u1", "own", "10.1.0.5", t0))
	keep := &v2alpha1.RunnerSet{ObjectMeta: metav1.ObjectMeta{Name: "keep", Namespace: ns}}
	keep.Spec.GatewayRef.Name = "gw"
	require.NoError(t, h.c.Create(ctx, keep), "a second set keeps the pool in use")
	require.NoError(t, h.c.Delete(ctx, &v2alpha1.RunnerSet{ObjectMeta: metav1.ObjectMeta{Name: "own", Namespace: ns}}))

	require.NoError(t, h.p.poll(ctx))
	got := h.rec.recorded()
	require.Len(t, got, 1)
	require.IsType(t, &corev1.Pod{}, got[0].regarding)
	assert.Equal(t, "own-1", got[0].regarding.GetName())
	assert.Nil(t, got[0].related)
}

// A job's reported destinations are forgotten once it has been quiet for
// failureRetention, and when the gateway opts out.
func TestDenialEvents_ForgetAQuietJob(t *testing.T) {
	d := newDenialEvents()
	assert.True(t, d.first("u1", "h:443", t0))
	assert.False(t, d.first("u1", "h:443", t0.Add(time.Minute)))

	d.sweep(t0.Add(time.Minute + failureRetention - time.Second))
	assert.False(t, d.first("u1", "h:443", t0.Add(2*time.Minute)), "a repeat keeps the job's entry alive")
	d.sweep(t0.Add(2*time.Minute + failureRetention))
	assert.True(t, d.first("u1", "h:443", t0.Add(13*time.Minute)), "a job quiet for failureRetention is reported again")

	d.reset()
	assert.True(t, d.first("u1", "h:443", t0.Add(14*time.Minute)))
}
