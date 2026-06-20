//go:build load

package load

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// TestAGCLoad drives the AGC's listener-multiplexing core under a large number
// of concurrent virtual sessions and asserts the capacity SLOs. It runs only
// under `-tags load` (see the build constraint), via `make load-test-quick` /
// `make load-test-full`. Every parameter is overridable by environment
// variable so the same target scales on a bigger host without code edits:
//
//	LOAD_TENANTS                tenants (RunnerGroups)              [10]
//	LOAD_LISTENERS_PER_TENANT   listeners (= sessions) per tenant   [100]
//	LOAD_WARMUP                 ramp before sampling                [5s]
//	LOAD_DURATION               steady-state measurement window     [20s]
//	LOAD_JOB_DURATION           simulated worker-pod runtime        [100ms]
//	LOAD_THINK_TIME             gap between jobs per session        [0]
//	LOAD_LONGPOLL_HOLD          broker idle-poll hold               [2s]
//	LOAD_SAMPLE_INTERVAL        sampling cadence                    [250ms]
//	LOAD_RENEW_INTERVAL         per-job RenewJob cadence            [30s]
//	LOAD_REPORT                 path to write the Markdown report   [none]
func TestAGCLoad(t *testing.T) {
	cfg := Config{
		Tenants:            envInt(t, "LOAD_TENANTS", 10),
		ListenersPerTenant: envInt(t, "LOAD_LISTENERS_PER_TENANT", 100),
		Warmup:             envDur(t, "LOAD_WARMUP", 5*time.Second),
		Duration:           envDur(t, "LOAD_DURATION", 20*time.Second),
		JobDuration:        envDur(t, "LOAD_JOB_DURATION", 100*time.Millisecond),
		ThinkTime:          envDur(t, "LOAD_THINK_TIME", 0),
		LongPollHold:       envDur(t, "LOAD_LONGPOLL_HOLD", 2*time.Second),
		SampleInterval:     envDur(t, "LOAD_SAMPLE_INTERVAL", 250*time.Millisecond),
		RenewInterval:      envDur(t, "LOAD_RENEW_INTERVAL", 30*time.Second),
	}

	// Bound the run so a wedged harness fails loudly rather than hanging until the
	// outer -timeout. Generous margin over warmup + steady window for ramp and
	// teardown.
	budget := cfg.Warmup + cfg.Duration + 90*time.Second
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	t.Logf("starting load run: %d tenants × %d listeners = %d target sessions, steady window %s",
		cfg.Tenants, cfg.ListenersPerTenant, cfg.Tenants*cfg.ListenersPerTenant, cfg.Duration)

	res, err := cfg.Run(ctx, log)
	if err != nil {
		t.Fatalf("load run failed: %v", err)
	}

	t.Logf("\n%s", res.Summary())

	if path := os.Getenv("LOAD_REPORT"); path != "" {
		writeReport(t, path, res)
	}

	for _, slo := range res.SLOs() {
		if !slo.Pass {
			t.Errorf("SLO FAILED: %s — want %s, got %s", slo.Name, slo.Want, slo.Got)
		}
	}
}

func writeReport(t *testing.T, path string, res *Result) {
	t.Helper()
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Errorf("create report dir %q: %v", dir, err)
			return
		}
	}
	if err := os.WriteFile(path, []byte(res.Markdown(time.Now())), 0o644); err != nil { //nolint:gosec // G306: a human-readable report, not a secret.
		t.Errorf("write report %q: %v", path, err)
		return
	}
	t.Logf("wrote load report to %s", path)
}

func envInt(t *testing.T, key string, def int) int {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		t.Fatalf("invalid %s=%q: %v", key, v, err)
	}
	return n
}

func envDur(t *testing.T, key string, def time.Duration) time.Duration {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		t.Fatalf("invalid %s=%q: %v", key, v, err)
	}
	return d
}
