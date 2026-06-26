//go:build load

package load

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"runtime/pprof"
	"strings"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/agentpool"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/listener"
	"github.com/actions-gateway/github-actions-gateway/broker"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// memSlackBytesPerSession is the upper bound the isolated AGC-only per-session
// machinery cost must stay under, as a coarse regression guard. The measured
// figure (listener goroutine stack + broker.Client + live session state) sits
// well below this; the headroom absorbs Go runtime/version drift in goroutine
// stack growth without making the test brittle. If a change pushes past it,
// that is a real per-session-footprint regression worth investigating — and the
// density claim in appendix-a must be re-derived.
const memSlackBytesPerSession = 64 * 1024

// memSample is a heap+stack snapshot taken after a forced GC.
type memSample struct {
	heapAlloc  uint64 // live heap objects
	stackInuse uint64 // goroutine stacks in use
	sys        uint64 // total OS-reserved (context only)
	goroutines int
}

// readMemSample forces GC and reads a stable heap+stack snapshot. It GCs twice
// with a brief pause so finalizers run and the stack scavenger settles before
// the read, so the delta between two samples reflects retained structures rather
// than transient allocation.
func readMemSample() memSample {
	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return memSample{
		heapAlloc:  ms.HeapAlloc,
		stackInuse: ms.StackInuse,
		sys:        ms.Sys,
		goroutines: runtime.NumGoroutine(),
	}
}

// TestAGCPerSessionMemory isolates the AGC's own per-session memory footprint —
// the figure behind the "thousands of sessions in one pod, far denser than
// pod-per-runner" claim — WITHOUT a cluster, a real broker, or the in-process
// broker stub's allocations that inflate the Q13 load harness's ~127 KiB/session
// upper bound.
//
// Methodology (three-point heap+stack differential):
//
//   - mBase   — shared infra only (in-process transport, http.Client, registrar).
//   - mAgents — N pre-registered agents materialised in N pools, plus N empty
//     Multiplexers, but NO listener goroutines started. The delta mAgents-mBase
//     therefore holds the pooled *Agent structs AND the fake k8s client's
//     retained agent Secrets — the latter an apiserver-side cost in production,
//     not AGC memory, which is exactly why it is held OUT of the headline figure.
//   - mFull   — all N listener goroutines started and parked in their GetMessage
//     long-poll (the steady idle-session state). The delta mFull-mAgents is the
//     marginal cost the AGC pays to hold one more concurrent virtual session
//     given its agent already exists: the listener goroutine's stack, its
//     broker.Client, and its live session state (sessionID, AES key, scoped
//     logger) — and nothing from the broker server side, because memTransport
//     answers every call in-process with no server, socket, or per-session
//     server-side state.
//
// The headline AGC-only per-session number is (mFull-mAgents)/N for heap+stack.
// It is the right quantity to multiply by the session count for the density
// comparison, since the agent pool is sized to the peak ceiling regardless of how
// many sessions are active. The pre-registered agent itself adds a small
// per-slot amount (Ed25519 key + creds, no JIT blob in this path: sub-KiB),
// reported separately for completeness.
//
// Runs only under `-tags load`, via `make mem-profile`. Knobs (env):
//
//	MEM_TENANTS                pools (RunnerGroups)               [10]
//	MEM_LISTENERS_PER_TENANT   listeners (= sessions) per pool    [100]
//	MEM_SETTLE                 settle wait after sessions park    [1s]
//	MEM_HEAP_PROFILE           path to write a pprof heap profile [none]
func TestAGCPerSessionMemory(t *testing.T) {
	tenants := envInt(t, "MEM_TENANTS", 10)
	listenersPerTenant := envInt(t, "MEM_LISTENERS_PER_TENANT", 100)
	settle := envDur(t, "MEM_SETTLE", time.Second)
	target := tenants * listenersPerTenant
	if target <= 0 {
		t.Fatalf("target sessions must be positive, got %d (%d × %d)", target, tenants, listenersPerTenant)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	scheme := k8sruntime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("build scheme: %v", err)
	}

	// Shared infra: one in-process transport + http.Client answers every broker
	// and OAuth call for every session, with zero server-side state.
	transport := &memTransport{}
	httpClient := &http.Client{Transport: transport}
	reg := &memRegistrar{}

	mBase := readMemSample()

	// Phase 1: materialise N agents across N pools and build empty Multiplexers,
	// but start no goroutines.
	pools := make([]*agentpool.Pool, 0, tenants)
	muxes := make([]*listener.Multiplexer, 0, tenants)
	for i := 0; i < tenants; i++ {
		name := fmt.Sprintf("rg-%d", i)
		namespace := fmt.Sprintf("tenant-%d", i)
		k8s := fake.NewClientBuilder().WithScheme(scheme).Build()
		pool := agentpool.NewPool(k8s, namespace, name, "2.335.1", nil, reg, agentpool.KeyTypeEd25519)
		if err := pool.EnsureAgents(ctx, int32(listenersPerTenant), loadToken); err != nil {
			t.Fatalf("tenant %s EnsureAgents: %v", name, err)
		}
		factory := memListenerFactory(name, namespace, pool, httpClient, log)
		mux := listener.NewMultiplexer(factory, int32(listenersPerTenant), log)
		pools = append(pools, pool)
		muxes = append(muxes, mux)
	}

	mAgents := readMemSample()

	// Phase 2: start the listener goroutines and fill each pool to its ceiling.
	// Start() launches the permanent baseline; SpawnReplacement fills the rest
	// (the production replacement path) without needing a delivered job.
	for _, mux := range muxes {
		if err := mux.Start(ctx); err != nil {
			t.Fatalf("multiplexer start: %v", err)
		}
		for mux.ActiveCount() < int32(listenersPerTenant) {
			mux.SpawnReplacement(ctx)
		}
	}

	// Wait for every session to reach its resting long-poll, then confirm the
	// full target is held — a short session count means the figure would be
	// computed over the wrong denominator.
	if !waitForSessions(ctx, muxes, target, 30*time.Second) {
		active := totalActive(muxes)
		stopAll(muxes)
		t.Fatalf("only %d/%d sessions parked within deadline", active, target)
	}
	time.Sleep(settle)

	if active := totalActive(muxes); active != target {
		stopAll(muxes)
		t.Fatalf("expected %d parked sessions, got %d", target, active)
	}

	mFull := readMemSample()

	if path := os.Getenv("MEM_HEAP_PROFILE"); path != "" {
		writeHeapProfile(t, path)
	}

	// Tear down before reporting so a failed assertion still releases goroutines.
	stopAll(muxes)
	runtime.KeepAlive(pools)

	// Headline: marginal AGC-only cost per concurrent session (machinery only).
	machineryHeap := perSession(mFull.heapAlloc, mAgents.heapAlloc, target)
	machineryStack := perSession(mFull.stackInuse, mAgents.stackInuse, target)
	machineryTotal := machineryHeap + machineryStack

	// Secondary: agent-pool footprint, which still carries the fake k8s client's
	// retained Secrets — held out of the headline and labelled as such.
	agentBucketHeap := perSession(mAgents.heapAlloc, mBase.heapAlloc, target)

	startedGoroutines := mFull.goroutines - mAgents.goroutines

	var b strings.Builder
	fmt.Fprintf(&b, "\nAGC per-session memory (isolated; in-process transport, no broker stub)\n")
	fmt.Fprintf(&b, "  target sessions          : %d (%d tenants × %d listeners)\n", target, tenants, listenersPerTenant)
	fmt.Fprintf(&b, "  listener goroutines added: %d (%.2f per session)\n", startedGoroutines, float64(startedGoroutines)/float64(target))
	fmt.Fprintf(&b, "  --- AGC-only per session (headline) ---\n")
	fmt.Fprintf(&b, "  goroutine stack          : %s\n", bytesHf(machineryStack))
	fmt.Fprintf(&b, "  heap (client + session)  : %s\n", bytesHf(machineryHeap))
	fmt.Fprintf(&b, "  TOTAL machinery / session: %s\n", bytesHf(machineryTotal))
	fmt.Fprintf(&b, "  --- context ---\n")
	fmt.Fprintf(&b, "  agent pool + test Secret : %s / session (apiserver-side in prod; excluded from headline)\n", bytesHf(agentBucketHeap))
	fmt.Fprintf(&b, "  heapAlloc  base/agents/full: %s / %s / %s\n", bytesH(mBase.heapAlloc), bytesH(mAgents.heapAlloc), bytesH(mFull.heapAlloc))
	fmt.Fprintf(&b, "  stackInuse base/agents/full: %s / %s / %s\n", bytesH(mBase.stackInuse), bytesH(mAgents.stackInuse), bytesH(mFull.stackInuse))
	fmt.Fprintf(&b, "  sys (OS-reserved) full     : %s\n", bytesH(mFull.sys))
	t.Logf("%s", b.String())

	// Regression guard + sanity floor.
	if machineryTotal <= 0 {
		t.Errorf("per-session machinery measured as %s; expected a positive footprint", bytesHf(machineryTotal))
	}
	if machineryTotal > memSlackBytesPerSession {
		t.Errorf("per-session machinery %s exceeds guard %s — per-session footprint regressed; re-derive the density claim in appendix-a",
			bytesHf(machineryTotal), bytesHf(float64(memSlackBytesPerSession)))
	}
	if startedGoroutines < target {
		t.Errorf("expected ≥ %d listener goroutines, got %d — not every session reached its long-poll", target, startedGoroutines)
	}
}

// memListenerFactory builds a listener.Config that reaches the resting long-poll
// and nothing more: it claims a pool agent and wires a broker.Client over the
// in-process transport. No JobHandler or RecycleAgent is set — the probe never
// delivers a job — and IdleThreshold is set high so a transient empty poll can
// never idle-exit a session out of the measured set.
func memListenerFactory(name, namespace string, pool *agentpool.Pool, httpClient *http.Client, log *slog.Logger) listener.ConfigFactory {
	return func(int) listener.Config {
		agent := pool.ClaimAgent()
		if agent == nil {
			return listener.Config{Group: name, Namespace: namespace}
		}
		bc := &broker.Client{
			BrokerURL:     agent.BrokerURL,
			RunnerVersion: "2.335.1",
			RunnerOS:      "linux",
			UseV2Flow:     true,
			HTTPClient:    httpClient,
		}
		return listener.Config{
			Group:         name,
			Namespace:     namespace,
			Agent:         agent,
			Broker:        bc,
			HTTPClient:    httpClient,
			RunnerOS:      "linux",
			Log:           log,
			IdleThreshold: 1 << 30,
			ReleaseAgent:  func() { pool.ReleaseAgent(agent) },
		}
	}
}

// perSession returns (after-before)/n, clamped at zero so GC jitter that makes a
// later sample momentarily smaller does not surface a negative figure.
func perSession(after, before uint64, n int) float64 {
	if after <= before || n <= 0 {
		return 0
	}
	return float64(after-before) / float64(n)
}

// waitForSessions polls until the summed active count across all multiplexers
// reaches target or the deadline elapses. Returns true on reaching target.
func waitForSessions(ctx context.Context, muxes []*listener.Multiplexer, target int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return false
		}
		if totalActive(muxes) >= target {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return totalActive(muxes) >= target
}

func totalActive(muxes []*listener.Multiplexer) int {
	n := 0
	for _, mux := range muxes {
		n += int(mux.ActiveCount())
	}
	return n
}

func stopAll(muxes []*listener.Multiplexer) {
	for _, mux := range muxes {
		mux.Stop()
	}
}

// writeHeapProfile dumps a pprof heap profile so the methodology is verifiable
// with `go tool pprof` (e.g. inspecting inuse_space by package).
func writeHeapProfile(t *testing.T, path string) {
	t.Helper()
	f, err := os.Create(path) //nolint:gosec // operator-supplied diagnostic path
	if err != nil {
		t.Errorf("create heap profile %q: %v", path, err)
		return
	}
	defer func() { _ = f.Close() }()
	runtime.GC()
	if err := pprof.WriteHeapProfile(f); err != nil {
		t.Errorf("write heap profile %q: %v", path, err)
		return
	}
	t.Logf("wrote heap profile to %s", path)
}
