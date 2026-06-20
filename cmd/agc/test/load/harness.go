//go:build load

package load

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/agentpool"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/listener"
	"github.com/actions-gateway/github-actions-gateway/broker"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// loadToken is the placeholder installation token threaded through pool
// operations. The in-memory registrar ignores it.
const loadToken = "load-token"

// Config parameterises a load run. Zero fields take documented defaults via
// withDefaults.
type Config struct {
	Tenants            int           // independent RunnerGroups
	ListenersPerTenant int           // maxListeners per RunnerGroup (= sustained sessions per tenant)
	Warmup             time.Duration // ramp time before steady-state sampling begins
	Duration           time.Duration // steady-state measurement window
	JobDuration        time.Duration // simulated worker-pod runtime (JobHandler hold)
	ThinkTime          time.Duration // gap between jobs per session (0 = saturated)
	LongPollHold       time.Duration // broker idle-poll hold
	SampleInterval     time.Duration // sampling cadence for sessions/goroutines/memory
	RenewInterval      time.Duration // per-job RenewJob cadence
}

func (c Config) withDefaults() Config {
	if c.Tenants == 0 {
		c.Tenants = 10
	}
	if c.ListenersPerTenant == 0 {
		c.ListenersPerTenant = 100
	}
	if c.Warmup == 0 {
		c.Warmup = 5 * time.Second
	}
	if c.Duration == 0 {
		c.Duration = 20 * time.Second
	}
	if c.JobDuration == 0 {
		c.JobDuration = 100 * time.Millisecond
	}
	if c.LongPollHold == 0 {
		c.LongPollHold = 2 * time.Second
	}
	if c.SampleInterval == 0 {
		c.SampleInterval = 250 * time.Millisecond
	}
	if c.RenewInterval == 0 {
		// Larger than any realistic JobDuration so the renew loop does not fire
		// mid-job and add request noise the harness is not measuring.
		c.RenewInterval = 30 * time.Second
	}
	return c
}

// TargetSessions is the total concurrent virtual sessions the run aims to hold.
func (c Config) TargetSessions() int { return c.Tenants * c.ListenersPerTenant }

// Result is the outcome of a load run.
type Result struct {
	Config Config

	// Concurrency (sampled over the steady window).
	SessionsMin int
	SessionsMax int
	SessionsAvg float64

	// Goroutines.
	BaselineGoroutines int
	PeakGoroutines     int
	LeakedGoroutines   int

	// Throughput (over the steady window).
	JobsAcquired float64 // jobs acquired during the steady window
	Throughput   float64 // jobs/sec

	// Per-job re-registration cost (Q114).
	Recycles       int64
	RecycleErrors  int64
	RecyclesPerJob float64
	RecycleP50     time.Duration
	RecycleP95     time.Duration
	RecycleP99     time.Duration
	RecycleMax     time.Duration

	// Memory (steady window).
	HeapInuseBytes  uint64 // peak
	SysBytes        uint64 // peak
	BytesPerSession float64
}

// recorder accumulates per-job recycle latencies concurrently.
type recorder struct {
	mu        sync.Mutex
	latencies []time.Duration
	recycles  atomic.Int64
	errors    atomic.Int64
}

func (r *recorder) add(d time.Duration) {
	r.recycles.Add(1)
	r.mu.Lock()
	r.latencies = append(r.latencies, d)
	r.mu.Unlock()
}

// resetLatencies drops latencies accumulated during warmup so the reported
// percentiles reflect only the steady-state window.
func (r *recorder) resetLatencies() {
	r.mu.Lock()
	r.latencies = r.latencies[:0]
	r.mu.Unlock()
}

// tenant bundles the per-RunnerGroup machinery, mirroring what
// RunnerGroupReconciler builds for one group.
type tenant struct {
	name string
	pool *agentpool.Pool
	mux  *listener.Multiplexer
}

// Run drives the load and returns the measured Result. It owns the broker stub
// lifecycle for the run.
func (c Config) Run(ctx context.Context, log *slog.Logger) (*Result, error) {
	cfg := c.withDefaults()
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	stub := newBrokerStub(cfg.LongPollHold, cfg.ThinkTime)
	stubClosed := false
	defer func() {
		if !stubClosed {
			stub.Close()
		}
	}()

	// Baseline goroutine count after the stub server is up, so the leak check
	// excludes the stub's own listener goroutines.
	runtime.GC()
	baseline := runtime.NumGoroutine()

	scheme := k8sruntime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("build scheme: %w", err)
	}
	reg := newLoadRegistrar(stub)
	rec := &recorder{}

	// One shared HTTP client across all listener goroutines, mirroring the
	// production AGC (every goroutine's broker.Client uses the single
	// BrokerConfig.HTTPClient). Size the idle-connection pool to the full session
	// count so steady-state connections are reused instead of churned: httptest's
	// default client caps idle conns per host at 2, which under thousands of
	// concurrent sessions forces an open/close per request and floods the process
	// with transient connection goroutines — an artifact of the in-process stub,
	// not the AGC's real per-session cost.
	httpClient := loadHTTPClient(cfg.TargetSessions())

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	tenants, err := startTenants(runCtx, cfg, scheme, reg, stub, httpClient, rec, log)
	if err != nil {
		return nil, err
	}

	// Warm up: let the pools ramp toward ListenersPerTenant before sampling.
	if !sleepCtx(runCtx, cfg.Warmup) {
		return nil, runCtx.Err()
	}

	// Steady-state sampling window. Snapshot the cumulative counters at the
	// window boundaries so throughput and recycles/job are measured over the same
	// interval (warmup ramp recycles would otherwise inflate the ratio), and drop
	// warmup latencies so the percentiles are steady-state.
	rec.resetLatencies()
	steadyStart := time.Now()
	acquiresAtStart := stub.AcquireCount()
	recyclesAtStart := rec.recycles.Load()
	recycleErrsAtStart := rec.errors.Load()
	samp := sample(runCtx, tenants, cfg.SampleInterval, cfg.Duration)
	steadyDur := time.Since(steadyStart)
	acquiresAtEnd := stub.AcquireCount()
	recyclesSteady := rec.recycles.Load() - recyclesAtStart
	recycleErrsSteady := rec.errors.Load() - recycleErrsAtStart

	// Tear down all multiplexers, then close the broker stub before the leak
	// check: the stub's HTTP server/connection goroutines are an artifact of
	// running the broker in-process (a real AGC talks to a remote broker), so
	// they must not count against the AGC goroutine-leak check.
	for _, t := range tenants {
		t.mux.Stop()
	}
	cancel()
	stub.Close()
	stubClosed = true
	settleGoroutines(baseline)
	runtime.GC()
	leaked := runtime.NumGoroutine() - baseline
	if leaked < 0 {
		leaked = 0
	}

	res := &Result{
		Config:             cfg,
		SessionsMin:        samp.sessionsMin,
		SessionsMax:        samp.sessionsMax,
		SessionsAvg:        samp.sessionsAvg,
		BaselineGoroutines: baseline,
		PeakGoroutines:     samp.peakGoroutines,
		LeakedGoroutines:   leaked,
		JobsAcquired:       float64(acquiresAtEnd - acquiresAtStart),
		Recycles:           recyclesSteady,
		RecycleErrors:      recycleErrsSteady,
		HeapInuseBytes:     samp.peakHeapInuse,
		SysBytes:           samp.peakSys,
	}
	if steadyDur > 0 {
		res.Throughput = res.JobsAcquired / steadyDur.Seconds()
	}
	if res.JobsAcquired > 0 {
		res.RecyclesPerJob = float64(res.Recycles) / res.JobsAcquired
	}
	if samp.sessionsAvg > 0 {
		res.BytesPerSession = float64(samp.peakHeapInuse) / samp.sessionsAvg
	}
	res.RecycleP50, res.RecycleP95, res.RecycleP99, res.RecycleMax = percentiles(rec)

	return res, nil
}

// startTenants builds and starts one tenant (pool + multiplexer) per requested
// RunnerGroup, pre-registering each pool to ListenersPerTenant agents.
func startTenants(ctx context.Context, cfg Config, scheme *k8sruntime.Scheme, reg agentpool.Registrar, stub *brokerStub, httpClient *http.Client, rec *recorder, log *slog.Logger) ([]*tenant, error) {
	tenants := make([]*tenant, 0, cfg.Tenants)
	for i := 0; i < cfg.Tenants; i++ {
		name := fmt.Sprintf("rg-%d", i)
		namespace := fmt.Sprintf("tenant-%d", i)
		// One fake client per tenant: a tenant's agent-Secret reads/writes serialize
		// on that client's internal lock, so a single shared client would funnel all
		// tenants' recycle I/O through one mutex — an artifact, since in production
		// each tenant pool hits the (concurrent, sharded) apiserver independently.
		k8s := fake.NewClientBuilder().WithScheme(scheme).Build()
		pool := agentpool.NewPool(k8s, namespace, name, "2.335.1", nil, reg, agentpool.KeyTypeEd25519)
		if err := pool.EnsureAgents(ctx, int32(cfg.ListenersPerTenant), loadToken); err != nil {
			return nil, fmt.Errorf("tenant %s EnsureAgents: %w", name, err)
		}

		factory := tenantFactory(cfg, name, namespace, pool, stub, httpClient, rec, log)
		mux := listener.NewMultiplexer(factory, int32(cfg.ListenersPerTenant), log)
		if err := mux.Start(ctx); err != nil {
			return nil, fmt.Errorf("tenant %s multiplexer start: %w", name, err)
		}
		tenants = append(tenants, &tenant{name: name, pool: pool, mux: mux})
	}
	return tenants, nil
}

// tenantFactory returns a listener.ConfigFactory that mirrors
// RunnerGroupReconciler.getOrCreateMultiplexer: it claims a pool agent, builds a
// per-goroutine broker client, and wires the single-use JIT lifecycle callbacks
// (MarkConsumed / Recycle), timing the recycle so the harness can report the
// per-job re-registration cost.
func tenantFactory(cfg Config, name, namespace string, pool *agentpool.Pool, stub *brokerStub, httpClient *http.Client, rec *recorder, log *slog.Logger) listener.ConfigFactory {
	return func(int) listener.Config {
		agent := pool.ClaimAgent()
		if agent == nil {
			return listener.Config{Group: name, Namespace: namespace}
		}
		brokerURL := agent.BrokerURL
		if brokerURL == "" {
			brokerURL = stub.URL
		}
		bc := &broker.Client{
			BrokerURL:     brokerURL,
			RunnerVersion: "2.335.1",
			RunnerOS:      "linux",
			UseV2Flow:     true,
			HTTPClient:    httpClient,
		}
		return listener.Config{
			Group:     name,
			Namespace: namespace,
			Agent:     agent,
			Broker:    bc,
			// Share the tuned client for the OAuth token fetch too. The production
			// reconciler leaves Config.HTTPClient nil, so FetchRunnerOAuthToken falls
			// back to a fresh httpx.NewClient() per call — a new connection per
			// session and per per-job recycle (Q114) that is never reused. The
			// harness sets it so it measures the multiplexing core's inherent cost
			// rather than that incidental churn; the production reuse gap is flagged
			// as a Queue item.
			HTTPClient:    httpClient,
			RunnerOS:      "linux",
			Log:           log,
			JobHandler:    jobHandler(cfg.JobDuration),
			RenewInterval: cfg.RenewInterval,
			// No idle shutdown: in the saturated model a job is always ready, so a
			// goroutine never accumulates empty polls — but set a high threshold so
			// any transient empty poll during a recycle window cannot collapse the
			// pool below target.
			IdleThreshold:     1 << 30,
			ReleaseAgent:      func() { pool.ReleaseAgent(agent) },
			MarkAgentConsumed: func() { pool.MarkConsumed(agent) },
			RecycleAgent: func(ctx context.Context) (*agentpool.Agent, error) {
				start := time.Now()
				fresh, err := pool.Recycle(ctx, agent, loadToken)
				if err != nil {
					rec.errors.Add(1)
					return nil, err
				}
				rec.add(time.Since(start))
				return fresh, nil
			},
		}
	}
}

// loadHTTPClient builds the shared broker HTTP client. The idle-connection pool
// is sized to the total session count so steady-state connections are reused
// rather than churned. No per-request response-header timeout is set, so the
// broker long-poll can hold open when ThinkTime > 0.
func loadHTTPClient(sessions int) *http.Client {
	t := &http.Transport{
		MaxIdleConns:        0, // unlimited
		MaxIdleConnsPerHost: sessions + 16,
		IdleConnTimeout:     90 * time.Second,
	}
	return &http.Client{Transport: t}
}

// jobHandler simulates a worker pod running for d, holding the listener slot
// (and thus the virtual session) for the duration. It returns promptly on
// context cancellation so teardown is not blocked by an in-flight job.
func jobHandler(d time.Duration) listener.JobHandlerFunc {
	return func(ctx context.Context, _, _ string, _ []byte, _ string) error {
		if d <= 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(d):
			return nil
		}
	}
}

// samples holds the aggregated output of the steady-state sampler.
type samples struct {
	sessionsMin    int
	sessionsMax    int
	sessionsAvg    float64
	peakGoroutines int
	peakHeapInuse  uint64
	peakSys        uint64
}

// sample ticks every interval for the given duration, recording the summed
// active session count across tenants, the process goroutine count, and peak
// memory.
func sample(ctx context.Context, tenants []*tenant, interval, duration time.Duration) samples {
	out := samples{sessionsMin: 1 << 30}
	deadline := time.Now().Add(duration)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var total int64
	var n int64
	var ms runtime.MemStats
	for {
		select {
		case <-ctx.Done():
			return finalizeSamples(out, total, n)
		case <-ticker.C:
			sessions := 0
			for _, t := range tenants {
				sessions += int(t.mux.ActiveCount())
			}
			if sessions < out.sessionsMin {
				out.sessionsMin = sessions
			}
			if sessions > out.sessionsMax {
				out.sessionsMax = sessions
			}
			total += int64(sessions)
			n++

			if g := runtime.NumGoroutine(); g > out.peakGoroutines {
				out.peakGoroutines = g
			}
			runtime.ReadMemStats(&ms)
			if ms.HeapInuse > out.peakHeapInuse {
				out.peakHeapInuse = ms.HeapInuse
			}
			if ms.Sys > out.peakSys {
				out.peakSys = ms.Sys
			}

			if time.Now().After(deadline) {
				return finalizeSamples(out, total, n)
			}
		}
	}
}

func finalizeSamples(out samples, total, n int64) samples {
	if n > 0 {
		out.sessionsAvg = float64(total) / float64(n)
	}
	if out.sessionsMin == 1<<30 {
		out.sessionsMin = 0
	}
	return out
}

// percentiles computes recycle-latency percentiles from the recorder.
func percentiles(rec *recorder) (p50, p95, p99, max time.Duration) {
	rec.mu.Lock()
	lat := make([]time.Duration, len(rec.latencies))
	copy(lat, rec.latencies)
	rec.mu.Unlock()
	if len(lat) == 0 {
		return 0, 0, 0, 0
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	pick := func(q float64) time.Duration {
		idx := int(q * float64(len(lat)-1))
		return lat[idx]
	}
	return pick(0.50), pick(0.95), pick(0.99), lat[len(lat)-1]
}

// sleepCtx sleeps for d unless ctx is cancelled first. Returns false on
// cancellation.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// settleGoroutines waits up to a bound for the live goroutine count to return
// toward the baseline, so the leak check is not tripped by goroutines still
// unwinding immediately after Stop.
func settleGoroutines(baseline int) {
	const (
		bound = 5 * time.Second
		slack = 10
	)
	deadline := time.Now().Add(bound)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baseline+slack {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}
