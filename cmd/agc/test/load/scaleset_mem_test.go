//go:build load

package load

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/scalesetlistener"
	"github.com/actions-gateway/github-actions-gateway/scaleset"
)

// scalesetMemSlackBytesPerSet is the upper bound the isolated scale-set per-set
// machinery cost must stay under, as a coarse regression guard. Same role and same
// generous headroom as memSlackBytesPerSession on the classic path: the measured
// figure sits well below it, and the slack absorbs Go runtime drift in goroutine
// stack growth without making the test brittle. Past it is a real regression, and
// the density claim in appendix-a must be re-derived.
const scalesetMemSlackBytesPerSet = 128 * 1024

// scalesetMemTokenProvider is the installation-token provider the probe's clients
// bootstrap from. The transport never inspects the value.
type scalesetMemTokenProvider struct{}

func (scalesetMemTokenProvider) Token(context.Context) (string, error) {
	return "scaleset-mem-installation", nil
}

// TestScaleSetPerListenerMemory isolates the AGC's own per-scale-set memory
// footprint on the **scale-set** acquisition tier, which is the default protocol
// and the one the density claim on the marketing surfaces had never been measured
// against (Q722).
//
// The unit differs from the classic tier's, which is the substance of the finding
// rather than a detail of the harness. On the classic tier one listener goroutine
// holds one virtual runner session, so per-session and per-goroutine are the same
// quantity and TestAGCPerSessionMemory measures it directly. On the scale-set tier
// one Listener holds one *scale set's* acquisition session and multiplexes every
// job assigned to that set through it, so the resident cost scales with the number
// of RunnerSets, not with the number of concurrent jobs. A fleet's session count is
// therefore its RunnerSet count, which is orders of magnitude smaller.
//
// Methodology mirrors TestAGCPerSessionMemory's three-point heap+stack differential
// so the two figures are comparable:
//
//   - mBase     — shared infra only (in-process transport, http.Client).
//   - mClients  — N scaleset.Clients constructed, N Listeners built, but no Start
//     called, so no session exists and no goroutine has been launched.
//   - mFull     — all N Listeners started and parked in their message long-poll,
//     the steady state of a scale set with nothing to deliver.
//
// The headline is (mFull-mClients)/N for heap+stack: the marginal cost of holding
// one more live scale-set session given its client already exists. Nothing from the
// server side is in it, because scalesetMemTransport answers every call in-process
// with no server, socket, or per-session server-side state — the same reason the
// classic probe does not use the Q13 broker stub, and the reason this one does not
// use scalesettest, whose httptest.Server would hold a parked goroutine and its
// read/write buffers per session in this same process.
//
// Runs only under `-tags load`, via `make scaleset-mem-profile`. Knobs (env):
//
//	SCALESET_MEM_SETS   scale sets (= Listeners = sessions)  [200]
//	SCALESET_MEM_SETTLE settle wait after sessions park      [1s]
func TestScaleSetPerListenerMemory(t *testing.T) {
	sets := envInt(t, "SCALESET_MEM_SETS", 200)
	settle := envDur(t, "SCALESET_MEM_SETTLE", time.Second)
	if sets <= 0 {
		t.Fatalf("set count must be positive, got %d", sets)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	// Shared infra: one in-process transport answers every control-plane call and
	// every long poll, for every scale set, with zero server-side state.
	transport := &scalesetMemTransport{}
	httpClient := &http.Client{Transport: transport}

	mBase := readMemSample()

	// Phase 1: build N clients and N Listeners, starting nothing.
	listeners := make([]*scalesetlistener.Listener, 0, sets)
	for i := 0; i < sets; i++ {
		name := fmt.Sprintf("mem-set-%d", i)
		client, err := scaleset.New(scaleset.Config{
			TokenProvider: scalesetMemTokenProvider{},
			ConfigURL:     "https://github.com/mem-org",
			APIBase:       scalesetMemBase,
			HTTPClient:    httpClient,
			PollClient:    httpClient,
		})
		if err != nil {
			t.Fatalf("scale set %s: build client: %v", name, err)
		}
		l, err := scalesetlistener.New(scalesetlistener.Config{
			Client:       client,
			ScaleSetName: name,
			OwnerName:    fmt.Sprintf("mem-gateway/%s", name),
			// The probe never delivers a job, so Provision is never called; it is
			// required, so it must be non-nil.
			Provision: func(context.Context, scalesetlistener.Job) error { return nil },
			// A fixed positive capacity keeps the set advertising, which is the
			// resting state being measured. Nothing consumes it.
			Capacity: func(context.Context) int { return 1 },
			Log:      log,
		})
		if err != nil {
			t.Fatalf("scale set %s: build listener: %v", name, err)
		}
		listeners = append(listeners, l)
	}

	mClients := readMemSample()

	// Phase 2: open every session and let each Listener reach its resting long poll.
	dones := make([]<-chan struct{}, 0, sets)
	for i, l := range listeners {
		done, err := l.Start(ctx)
		if err != nil {
			t.Fatalf("scale set %d: start: %v", i, err)
		}
		dones = append(dones, done)
	}

	// Wait for every listener to reach its resting long poll, then confirm the full
	// count is held. A goroutine count cannot tell a parked poll from one spinning
	// in a backoff retry, and the figure divides by the set count, so a short parked
	// count would compute it over the wrong denominator.
	if !waitForParkedPolls(ctx, transport, sets, 30*time.Second) {
		t.Fatalf("only %d/%d message polls parked within deadline (%d sessions opened)",
			transport.parked(), sets, transport.sessions())
	}
	time.Sleep(settle)

	if parked := transport.parked(); parked != int64(sets) {
		t.Fatalf("expected %d parked polls, got %d", sets, parked)
	}
	if opened := transport.sessions(); opened != int64(sets) {
		t.Fatalf("expected %d sessions opened, got %d", sets, opened)
	}

	mFull := readMemSample()

	// Tear down before reporting so a failed assertion still releases goroutines.
	cancel()
	for _, done := range dones {
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("a listener did not stop within the teardown deadline")
		}
	}
	runtime.KeepAlive(listeners)

	machineryHeap := perSession(mFull.heapAlloc, mClients.heapAlloc, sets)
	machineryStack := perSession(mFull.stackInuse, mClients.stackInuse, sets)
	machineryTotal := machineryHeap + machineryStack
	clientBucketHeap := perSession(mClients.heapAlloc, mBase.heapAlloc, sets)
	startedGoroutines := mFull.goroutines - mClients.goroutines

	var b strings.Builder
	fmt.Fprintf(&b, "\nAGC per-scale-set memory, scale-set tier (isolated; in-process transport, no protocol stub)\n")
	fmt.Fprintf(&b, "  scale sets (= sessions)  : %d\n", sets)
	fmt.Fprintf(&b, "  goroutines added         : %d (%.2f per set)\n", startedGoroutines, float64(startedGoroutines)/float64(sets))
	fmt.Fprintf(&b, "  --- AGC-only per scale set (headline) ---\n")
	fmt.Fprintf(&b, "  goroutine stack          : %s\n", bytesHf(machineryStack))
	fmt.Fprintf(&b, "  heap (session state)     : %s\n", bytesHf(machineryHeap))
	fmt.Fprintf(&b, "  TOTAL machinery / set    : %s\n", bytesHf(machineryTotal))
	fmt.Fprintf(&b, "  --- context ---\n")
	fmt.Fprintf(&b, "  client + listener struct : %s / set (built before any session opens)\n", bytesHf(clientBucketHeap))
	fmt.Fprintf(&b, "  heapAlloc  base/clients/full: %s / %s / %s\n", bytesH(mBase.heapAlloc), bytesH(mClients.heapAlloc), bytesH(mFull.heapAlloc))
	fmt.Fprintf(&b, "  stackInuse base/clients/full: %s / %s / %s\n", bytesH(mBase.stackInuse), bytesH(mClients.stackInuse), bytesH(mFull.stackInuse))
	fmt.Fprintf(&b, "  sys (OS-reserved) full     : %s\n", bytesH(mFull.sys))
	t.Logf("%s", b.String())

	// Regression guard + sanity floor, matching the classic probe's pair. The floor
	// is what stops a harness that measured nothing at all from reading as a win.
	if machineryTotal <= 0 {
		t.Errorf("per-set machinery measured as %s; expected a positive footprint", bytesHf(machineryTotal))
	}
	if machineryTotal > scalesetMemSlackBytesPerSet {
		t.Errorf("per-set machinery %s exceeds guard %s — per-set footprint regressed; re-derive the density claim in appendix-a",
			bytesHf(machineryTotal), bytesHf(float64(scalesetMemSlackBytesPerSet)))
	}
	if startedGoroutines < sets {
		t.Errorf("expected ≥ %d goroutines, got %d — not every scale set reached its long-poll", sets, startedGoroutines)
	}
}

// waitForParkedPolls polls until every scale set's message long-poll is blocked in
// the transport, or the deadline elapses. Returns true on reaching target.
func waitForParkedPolls(ctx context.Context, tr *scalesetMemTransport, target int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return false
		}
		if tr.parked() >= int64(target) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return tr.parked() >= int64(target)
}
