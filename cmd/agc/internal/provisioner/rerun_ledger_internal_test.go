package provisioner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/runnercore"
)

// Q1182: the scale-set re-run budget in the per-set failure ledger. These tests pin the
// two defects it fixes — a restart refilling the budget, and two disruptions of one
// attempt landing two re-runs — plus the reservation's four outcomes, GAG's own
// force-cancel record, and the version rules that let two AGC versions share the object.

// ledgerRunAPI fakes the run GET and the rerun POST. The GET answers status, conclusion
// and run_attempt from the fields, which a test may change between calls.
type ledgerRunAPI struct {
	mu         sync.Mutex
	conclusion string
	attempt    int
	reruns     atomic.Int64
}

func (a *ledgerRunAPI) set(conclusion string, attempt int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.conclusion, a.attempt = conclusion, attempt
}

func (a *ledgerRunAPI) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			a.mu.Lock()
			body := fmt.Sprintf(`{"status":"completed","conclusion":%q,"run_attempt":%d}`, a.conclusion, a.attempt)
			a.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, body)
			return
		}
		a.reruns.Add(1)
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func ledgerMetrics() *runnercore.Metrics {
	return &runnercore.Metrics{
		EvictionRetries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "t_q1182_eviction_retries_total",
		}, []string{"namespace", "runner_group", "tier", "cause"}),
		EvictionRetriesExhausted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "t_q1182_eviction_retries_exhausted_total",
		}, []string{"namespace", "runner_group", "tier", "cause"}),
		EvictionRerunFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "t_q1182_eviction_rerun_failures_total",
		}, []string{"namespace", "runner_group", "tier", "cause", "reason"}),
		EvictionRerunWithheld: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "t_q1182_eviction_rerun_withheld_total",
		}, []string{"namespace", "runner_group", "tier", "cause", "reason"}),
		AbandonedRunForceCancels: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "t_q1182_abandoned_run_force_cancels_total",
		}, []string{"namespace", "runner_group", "tier", "outcome"}),
	}
}

// ledgerProvisioner is one AGC process against a shared fake cluster and GitHub. Two
// built on one client are a restart, or two replicas.
func ledgerProvisioner(c client.Client, srv *httptest.Server) *Provisioner {
	p := NewProvisioner(c, ledgerMetrics(), nil)
	p.TokenFunc = func(context.Context) (string, error) { return "tok", nil }
	p.GitHubAPIURL = srv.URL
	p.HTTPClient = srv.Client()
	p.EvictionRerunRetryInterval = time.Millisecond
	return p
}

func ledgerFixture(t *testing.T) (client.Client, *ledgerRunAPI, *httptest.Server, *stubTarget) {
	t.Helper()
	api := &ledgerRunAPI{conclusion: "failure", attempt: 1}
	srv := api.server(t)
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build()
	target := &stubTarget{key: client.ObjectKey{Namespace: "team-a", Name: "gpu"}}
	return c, api, srv, target
}

var discardLog = slog.New(slog.NewTextHandler(io.Discard, nil))

func evictScaleSet(p *Provisioner, target Target, runID string, maxRetries int, cause string, d rerunDisruption) {
	<-p.handleEviction(context.Background(), target, "myorg", "myrepo", runID, discardLog,
		maxRetries, 0, evictionTierScaleSet, cause, d)
}

// TestScaleSetBudget_SurvivesARestart is the headline defect: the budget used to be
// process memory, so a restarted AGC re-ran a run whose budget was spent. A second
// Provisioner on the same cluster is the restart.
func TestScaleSetBudget_SurvivesARestart(t *testing.T) {
	c, api, srv, target := ledgerFixture(t)

	before := ledgerProvisioner(c, srv)
	evictScaleSet(before, target, "4242", 1, recoveryCauseEviction, rerunDisruption{key: "pod-a"})
	require.Equal(t, int64(1), api.reruns.Load())

	// GAG's re-run landed and its attempt failed again, after the restart.
	api.set("failure", 2)
	after := ledgerProvisioner(c, srv)
	evictScaleSet(after, target, "4242", 1, recoveryCauseEviction, rerunDisruption{key: "pod-b"})

	assert.Equal(t, int64(1), api.reruns.Load(), "a restart must not refill the run's budget")
	assert.Equal(t, float64(1), testutil.ToFloat64(after.Metrics.EvictionRetriesExhausted.WithLabelValues(
		"team-a", "gpu", evictionTierScaleSet, recoveryCauseEviction)))
	assert.Contains(t, target.events, "EvictionRetriesExhausted")
}

// TestScaleSetBudget_ConcurrentDisruptionsOfOneAttemptMerge runs many disruptions of one
// attempt at once, across two replicas, under -race: exactly one re-run and one slot,
// with every disruption recorded against it.
func TestScaleSetBudget_ConcurrentDisruptionsOfOneAttemptMerge(t *testing.T) {
	const disruptions = 16
	c, api, srv, target := ledgerFixture(t)
	replicas := []*Provisioner{ledgerProvisioner(c, srv), ledgerProvisioner(c, srv)}

	var wg sync.WaitGroup
	for i := range disruptions {
		wg.Add(1)
		go func() {
			defer wg.Done()
			evictScaleSet(replicas[i%2], target, "4242", 2, recoveryCauseEviction,
				rerunDisruption{key: fmt.Sprintf("pod-%d", i)})
		}()
	}
	wg.Wait()

	assert.Equal(t, int64(1), api.reruns.Load(), "one attempt's disruptions share one re-run")
	for _, p := range replicas {
		assert.Equal(t, float64(0), testutil.ToFloat64(p.Metrics.EvictionRerunFailures.WithLabelValues(
			"team-a", "gpu", evictionTierScaleSet, recoveryCauseEviction, rerunFailureReasonBudgetUnrecorded)),
			"a replica's burst of writes must not exhaust the other's conflict retries")
	}
	l, _, err := replicas[0].readRerunLedger(context.Background(), target)
	require.NoError(t, err)
	require.Contains(t, l.Reruns, "4242")
	assert.Equal(t, 1, l.Reruns["4242"].Count)
	assert.Len(t, l.Reruns["4242"].Attempts["1"].Disruptions, disruptions)
}

// TestScaleSetBudget_ReservationOutcomes walks resume, merge, refuse and reserve.
func TestScaleSetBudget_ReservationOutcomes(t *testing.T) {
	ctx := context.Background()
	c, _, srv, target := ledgerFixture(t)
	p := ledgerProvisioner(c, srv)
	reserve := func(key, job string, attempt int) (rerunReservation, int) {
		t.Helper()
		got, n, err := p.reserveScaleSetRerun(ctx, target, "77", rerunDisruption{key: key, job: job, failedAttempt: attempt}, recoveryCauseEviction, 2)
		require.NoError(t, err)
		return got, n
	}

	got, n := reserve("pod-a", "build", 1)
	assert.Equal(t, rerunReserved, got)
	assert.Equal(t, 1, n)

	got, n = reserve("pod-a", "build", 1)
	assert.Equal(t, rerunResumed, got, "the same disruption re-uses its reservation")
	assert.Equal(t, 1, n)

	got, _ = reserve("pod-b", "test", 1)
	assert.Equal(t, rerunMerged, got, "a FailedJobs re-run of attempt 1 covers every job that failed in it")

	got, n = reserve("pod-c", "build", 2)
	assert.Equal(t, rerunReserved, got, "a failure in GAG's re-run is a new attempt")
	assert.Equal(t, 2, n)

	got, _ = reserve("pod-d", "build", 3)
	assert.Equal(t, rerunRefused, got)

	got, _ = reserve("pod-e", "lint", 2)
	assert.Equal(t, rerunMerged, got, "a merge spends no slot, so it is allowed at an exhausted budget")

	l, _, err := p.readRerunLedger(ctx, target)
	require.NoError(t, err)
	r := l.Reruns["77"]
	assert.Equal(t, 2, r.Count)
	assert.Equal(t, []string{"pod-a", "pod-b"}, r.Attempts["1"].Disruptions)
	assert.Equal(t, []string{"build", "test"}, r.Attempts["1"].Jobs)
	assert.NotContains(t, r.Attempts, "3", "a refused disruption records nothing")
	assert.WithinDuration(t, p.nowFn().Add(rerunRecordTTL), r.ExpiresAt, time.Minute)
}

// TestScaleSetBudget_ASingleJobRerunDoesNotAbsorbAnotherJob pins scope: a re-run that
// covered one job does not cover a second job's disruption in the same attempt.
func TestScaleSetBudget_ASingleJobRerunDoesNotAbsorbAnotherJob(t *testing.T) {
	ctx := context.Background()
	c, _, srv, target := ledgerFixture(t)
	p := ledgerProvisioner(c, srv)
	require.NoError(t, p.updateRerunLedger(ctx, target, func(l *rerunLedger, now time.Time) bool {
		l.Reruns["77"] = &rerunRecord{ExpiresAt: now.Add(time.Hour), Count: 1, Attempts: map[string]*rerunAttempt{
			"1": {Scope: rerunScopeJob, Cause: "oom", Jobs: []string{"build"}, Disruptions: []string{"77/build"}},
		}}
		return true
	}))

	got, _, err := p.reserveScaleSetRerun(ctx, target, "77", rerunDisruption{key: "pod-b", job: "test", failedAttempt: 1}, recoveryCauseEviction, 2)
	require.NoError(t, err)
	assert.Equal(t, rerunReserved, got, "the second job reserves its own re-run")

	got, _, err = p.reserveScaleSetRerun(ctx, target, "78", rerunDisruption{key: "pod-c", job: "build", failedAttempt: 1}, recoveryCauseEviction, 2)
	require.NoError(t, err)
	assert.Equal(t, rerunReserved, got, "another run's record does not cover this one")
}

// TestScaleSetBudget_SelfCancelIsNotAUsersCancel pins the per-attempt record of GAG's
// own force-cancel: the deletion arm withholds a cancelled run, except the attempt GAG
// cancelled itself. A user's cancel of a later attempt is still withheld.
func TestScaleSetBudget_SelfCancelIsNotAUsersCancel(t *testing.T) {
	for _, tc := range []struct {
		name          string
		selfCancelled int
		wantReruns    int64
	}{
		{"GAG cancelled this attempt", 3, 1},
		{"a user cancelled a later attempt", 2, 0},
		{"nothing recorded", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			c, api, srv, target := ledgerFixture(t)
			api.set("cancelled", 3)
			p := ledgerProvisioner(c, srv)
			if tc.selfCancelled > 0 {
				require.NoError(t, p.recordSelfCancel(ctx, target, "811", tc.selfCancelled))
			}

			evictScaleSet(p, target, "811", 2, recoveryCauseDeletion, rerunDisruption{key: "pod-a"})

			assert.Equal(t, tc.wantReruns, api.reruns.Load())
			assert.Equal(t, float64(1-tc.wantReruns), testutil.ToFloat64(p.Metrics.EvictionRerunWithheld.WithLabelValues(
				"team-a", "gpu", evictionTierScaleSet, recoveryCauseDeletion, rerunWithheldReasonRunCancelled)))
		})
	}
}

// TestScaleSetBudget_AbandonedArmRecordsItsCancel pins the write side: the scale-set
// abandoned arm records the attempt it force-cancels before cancelling, and the re-run
// it registers is judged against that attempt.
func TestScaleSetBudget_AbandonedArmRecordsItsCancel(t *testing.T) {
	ctx := context.Background()
	c, api, srv, target := ledgerFixture(t)
	api.set("", 4)
	p := ledgerProvisioner(c, srv)

	p.forceCancelAbandonedRun(ctx, target, "myorg", "myrepo", "900", evictionTierScaleSet, discardLog,
		rerunDisruption{key: "pod-a"})

	assert.Equal(t, 4, p.selfCancelledAttempt(ctx, target, "900"))
	e, ok := p.peekAbandonedRerun(abandonedRerunKey{owner: target.Key(), runID: "900"})
	require.True(t, ok)
	assert.Equal(t, rerunDisruption{key: "pod-a", failedAttempt: 4}, e.disruption)
}

// TestScaleSetBudget_UnwritableLedgerWithholds pins the failure direction: a decision
// that cannot be recorded withholds the re-run, counted as budget_unrecorded.
func TestScaleSetBudget_UnwritableLedgerWithholds(t *testing.T) {
	c, api, srv, target := ledgerFixture(t)
	p := ledgerProvisioner(c, srv)
	writeLedger(t, c, target, `{"version":2,"minWriterVersion":2,"reruns":{}}`)

	evictScaleSet(p, target, "4242", 2, recoveryCauseEviction, rerunDisruption{key: "pod-a"})

	assert.Equal(t, int64(0), api.reruns.Load(), "never a re-run without a recorded slot")
	assert.Equal(t, float64(1), testutil.ToFloat64(p.Metrics.EvictionRerunFailures.WithLabelValues(
		"team-a", "gpu", evictionTierScaleSet, recoveryCauseEviction, rerunFailureReasonBudgetUnrecorded)))
	assert.Contains(t, target.events, "EvictionRerunFailed")
}

// TestRerunLedger_OlderWriterPreservesWhatItDoesNotKnow is the rollback case: a newer
// AGC's fields and maps survive this version's compare-and-swap, and a map it does not
// understand is still aged by each entry's expiresAt.
func TestRerunLedger_OlderWriterPreservesWhatItDoesNotKnow(t *testing.T) {
	ctx := context.Background()
	c, _, srv, target := ledgerFixture(t)
	p := ledgerProvisioner(c, srv)
	live := p.nowFn().Add(time.Hour).UTC().Format(time.RFC3339)
	expired := p.nowFn().Add(-time.Hour).UTC().Format(time.RFC3339)
	writeLedger(t, c, target, `{
		"version": 3, "minWriterVersion": 1, "futureTop": {"a": 1},
		"reruns": {"77": {"expiresAt": "`+live+`", "count": 1, "futureRecord": true,
			"attempts": {"1": {"scope": "FailedJobs", "cause": "oom", "disruptions": ["x"], "futureAttempt": "y"}}}},
		"failures": {"keep": {"expiresAt": "`+live+`", "jobID": "j1"},
		             "drop": {"expiresAt": "`+expired+`", "jobID": "j2"},
		             "undated": {"jobID": "j3"}}
	}`)

	got, _, err := p.reserveScaleSetRerun(ctx, target, "78", rerunDisruption{key: "pod-a", failedAttempt: 1}, recoveryCauseEviction, 2)
	require.NoError(t, err)
	require.Equal(t, rerunReserved, got)

	var doc map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(readLedgerData(t, c, target)), &doc))
	assert.JSONEq(t, `3`, string(doc["version"]), "an older writer does not downgrade the schema")
	assert.JSONEq(t, `{"a": 1}`, string(doc["futureTop"]))
	assert.JSONEq(t, `{"keep": {"expiresAt": "`+live+`", "jobID": "j1"}, "undated": {"jobID": "j3"}}`, string(doc["failures"]),
		"an unknown map keeps its live and undated entries and loses its expired one")
	var reruns map[string]map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(doc["reruns"], &reruns))
	assert.JSONEq(t, `true`, string(reruns["77"]["futureRecord"]))
	assert.Contains(t, string(reruns["77"]["attempts"]), `"futureAttempt":"y"`)
	assert.Contains(t, reruns, "78")
}

// TestRerunLedger_ExpiredBudgetsAreDropped ages the budget map by expiresAt.
func TestRerunLedger_ExpiredBudgetsAreDropped(t *testing.T) {
	ctx := context.Background()
	c, _, srv, target := ledgerFixture(t)
	p := ledgerProvisioner(c, srv)
	expired := p.nowFn().Add(-time.Minute).UTC().Format(time.RFC3339)
	writeLedger(t, c, target, `{"version":1,"minWriterVersion":1,"reruns":{"old":{"expiresAt":"`+expired+`","count":2}}}`)

	_, _, err := p.reserveScaleSetRerun(ctx, target, "new", rerunDisruption{key: "pod-a", failedAttempt: 1}, recoveryCauseEviction, 2)
	require.NoError(t, err)

	l, _, err := p.readRerunLedger(ctx, target)
	require.NoError(t, err)
	assert.NotContains(t, l.Reruns, "old")
	assert.Contains(t, l.Reruns, "new")
}

// TestRerunLedger_CapRefusesRatherThanEvicts pins the cap: a write that would pass it
// fails, and no budget is dropped to make room.
func TestRerunLedger_CapRefusesRatherThanEvicts(t *testing.T) {
	ctx := context.Background()
	c, _, srv, target := ledgerFixture(t)
	p := ledgerProvisioner(c, srv)
	live := p.nowFn().Add(time.Hour).UTC().Format(time.RFC3339)
	writeLedger(t, c, target, `{"version":1,"minWriterVersion":1,"padding":"`+strings.Repeat("x", rerunLedgerMaxBytes)+`",
		"reruns":{"77":{"expiresAt":"`+live+`","count":1}}}`)
	before := readLedgerData(t, c, target)

	_, _, err := p.reserveScaleSetRerun(ctx, target, "78", rerunDisruption{key: "pod-a", failedAttempt: 1}, recoveryCauseEviction, 2)
	require.ErrorIs(t, err, errRerunLedgerFull)
	assert.Equal(t, before, readLedgerData(t, c, target), "nothing is written past the cap")
}

// TestRerunLedger_CapHoldsEnoughBudgets measures how many runs' budgets fit under the
// cap before a reservation is refused, for a record at the field's ceiling
// (maxEvictionRetries: 10, every attempt recorded, long names) and at the default
// (2). The floors are what the operator docs quote.
func TestRerunLedger_CapHoldsEnoughBudgets(t *testing.T) {
	for _, tc := range []struct {
		name               string
		reruns, nameLength int
		floor              int
	}{
		{"maximal", 10, 100, 200},
		{"default", 2, 40, 1_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const sample = 100
			l := &rerunLedger{Version: rerunLedgerVersion, MinWriterVersion: rerunLedgerMinWriterVersion, Reruns: map[string]*rerunRecord{}}
			for i := range sample {
				r := &rerunRecord{ExpiresAt: time.Now(), Count: tc.reruns, SelfCancelledAttempt: 49, Attempts: map[string]*rerunAttempt{}}
				for a := 1; a <= tc.reruns; a++ {
					r.Attempts[fmt.Sprint(40+a)] = &rerunAttempt{Scope: rerunScopeFailedJobs, Cause: recoveryCauseAbandoned,
						Jobs: []string{strings.Repeat("j", tc.nameLength)}, Disruptions: []string{strings.Repeat("p", 63)}}
				}
				l.Reruns[fmt.Sprint(20_000_000_000+i)] = r
			}
			b, err := json.Marshal(l)
			require.NoError(t, err)
			fit := rerunLedgerMaxBytes * sample / len(b)
			t.Logf("%d bytes per run, %d runs fit", len(b)/sample, fit)
			assert.GreaterOrEqual(t, fit, tc.floor)
		})
	}
}

func writeLedger(t *testing.T, c client.Client, target *stubTarget, data string) {
	t.Helper()
	require.NoError(t, c.Create(context.Background(), &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: target.key.Namespace, Name: scaleSetRerunLedgerConfigMapName(target.key.Name)},
		Data:       map[string]string{rerunLedgerDataKey: data},
	}))
}

func readLedgerData(t *testing.T, c client.Client, target *stubTarget) string {
	t.Helper()
	var cm corev1.ConfigMap
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{
		Namespace: target.key.Namespace, Name: scaleSetRerunLedgerConfigMapName(target.key.Name)}, &cm))
	return cm.Data[rerunLedgerDataKey]
}
