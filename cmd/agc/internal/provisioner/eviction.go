package provisioner

import (
	"bytes"
	"context"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"net/http"
	neturl "net/url"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// evictionEntry is the value stored in evictionCounts. count is the number of
// reruns already reserved for the run (capped at maxRetries — Q106); lastUpdate
// is the time of the most recent eviction of the run, refreshed on every
// eviction whether or not a retry slot was granted. The sweeper reclaims an
// entry once lastUpdate is older than the TTL, by which point the run can no
// longer produce an evicted pod (see sweepEvictionCounts).
type evictionEntry struct {
	count      int
	lastUpdate time.Time
}

// evictionLockShards is the number of mutexes in the sharded eviction lock.
// Eviction is a rare, node-pressure-driven event, so a small fixed pool keeps
// contention between distinct run_ids negligible without unbounded growth.
const evictionLockShards = 64

const (
	// defaultEvictionCounterTTL bounds how long a per-run eviction-retry counter
	// is retained after the run's last eviction. It is chosen well beyond a
	// realistic GitHub Actions run lifetime: an entry is reclaimed only once its
	// run can no longer produce an evicted pod, because reclaiming a live run's
	// counter would reset it to zero and refill the retry budget — the Q106 bug.
	// (Q141)
	defaultEvictionCounterTTL = 24 * time.Hour
	// defaultEvictionSweepInterval is how often the background sweeper scans
	// evictionCounts for entries older than the TTL.
	defaultEvictionSweepInterval = time.Hour
)

// handleEviction reserves a slot from the run's retry budget and, if one remains,
// waits out retryDelay and asks GitHub to re-run the run's failed jobs. It is shared
// by both acquisition tiers: the classic path calls it inline from provision() once the
// worker pod it is watching turns up evicted, and the scale-set path from the owning
// reconciler's RecoverEvictedScaleSetWorkers pass. tier only labels the metrics — the
// budget is deliberately one budget, keyed by run_id alone, so the Q106 cap of
// maxRetries re-runs per run holds across both tiers together rather than once each.
func (p *Provisioner) handleEviction(ctx context.Context, target Target, owner, repo, runID string, log *slog.Logger, maxRetries int, retryDelay time.Duration, tier string) {
	key := target.Key()
	if runID == "0" || runID == "" {
		log.Warn("pod evicted but run_id unknown; skipping auto-retry")
		return
	}

	// Reserve a retry slot atomically. This guards against the read-modify-write
	// race where two concurrent evictions of the same run both read the same
	// count, both pass the budget check, and both fire a rerun — exceeding
	// maxRetries (Q106). At most maxRetries evictions ever pass the gate, so the
	// rerun API is called at most maxRetries times per run.
	attempt, ok := p.reserveEvictionRetry(runID, maxRetries)
	if !ok {
		log.Warn("eviction retry budget exhausted; manual rerun required",
			"runID", runID, "maxRetries", maxRetries)
		if p.Metrics != nil {
			p.Metrics.EvictionRetriesExhausted.WithLabelValues(key.Namespace, key.Name, tier).Inc()
		}
		target.RecordEvent(corev1.EventTypeWarning, "EvictionRetriesExhausted", "RetryEvictedJob",
			fmt.Sprintf("worker pod for run %s was evicted and the auto-retry budget (%d) is exhausted; a manual re-run is required", runID, maxRetries))
		return
	}

	log.Info("pod evicted; scheduling auto-retry", "runID", runID, "attempt", attempt, "tier", tier)
	if p.Metrics != nil {
		p.Metrics.EvictionRetries.WithLabelValues(key.Namespace, key.Name, tier).Inc()
	}

	// Brief delay before calling GitHub so any in-flight state settles.
	select {
	case <-ctx.Done():
		return
	case <-time.After(retryDelay):
	}

	if err := p.rerunFailedJobs(ctx, owner, repo, runID, log); err != nil {
		log.Error("eviction auto-retry failed; manual rerun may be required",
			"runID", runID, "error", err)
	} else {
		log.Info("eviction auto-retry triggered", "runID", runID, "attempt", attempt)
	}
}

// reserveEvictionRetry atomically checks the per-run eviction-retry budget and,
// if a slot remains, increments the count and returns the 1-based attempt
// number. It returns ok=false once the budget is exhausted. Serializing the
// check-and-increment per run_id — under the sharded evictionLocks — is what
// guarantees N concurrent evictions of the same run trigger at most maxRetries
// reruns (Q106). The lock is held only across the counter update, never across
// the retry delay or the GitHub API call.
func (p *Provisioner) reserveEvictionRetry(runID string, maxRetries int) (attempt int, ok bool) {
	mu := &p.evictionLocks[evictionShard(runID)]
	mu.Lock()
	defer mu.Unlock()

	now := p.nowFn()
	var count int
	if v, loaded := p.evictionCounts.Load(runID); loaded {
		count = v.(evictionEntry).count
	}
	if count >= maxRetries {
		// Budget is a hard lifetime cap: leave the count pinned so every later
		// eviction of this run is a no-op. We deliberately do NOT delete the
		// entry here — deleting reset the count to zero and let the budget refill
		// on the next eviction, which both defeats the cap and (combined with the
		// concurrent read-modify-write) is the Q106 over-budget bug. We DO refresh
		// lastUpdate: an exhausted but still-evicting run is provably live, so its
		// entry must not be a sweep candidate yet (Q141).
		p.evictionCounts.Store(runID, evictionEntry{count: count, lastUpdate: now})
		return 0, false
	}
	p.evictionCounts.Store(runID, evictionEntry{count: count + 1, lastUpdate: now})
	return count + 1, true
}

// nowFn returns the current time, honouring the test-injected p.now override.
func (p *Provisioner) nowFn() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// sweepEvictionCounts deletes per-run eviction-retry counters whose last
// eviction was more than ttl ago, returning the number of entries reclaimed.
//
// Correctness rests on a single fact: an evicted worker pod only ever exists for
// a live run, so a run that has produced no eviction for ttl can no longer
// produce one. With ttl chosen well beyond a realistic run lifetime, an entry is
// reclaimed only after its run is provably dead — so the LoadOrStore that a later
// eviction would do (refilling the budget to zero) can never happen for a run we
// swept. That preserves the Q106 invariant (at most maxRetries reruns per live
// run) while bounding evictionCounts to run_ids evicted within the trailing ttl
// window. The per-entry shard lock is taken (and lastUpdate re-checked under it)
// so a concurrent reserveEvictionRetry that just refreshed the entry is never
// raced away.
func (p *Provisioner) sweepEvictionCounts(ttl time.Duration) int {
	now := p.nowFn()
	var swept int
	p.evictionCounts.Range(func(key, value any) bool {
		if now.Sub(value.(evictionEntry).lastUpdate) <= ttl {
			return true
		}
		runID := key.(string)
		mu := &p.evictionLocks[evictionShard(runID)]
		mu.Lock()
		if v, loaded := p.evictionCounts.Load(runID); loaded &&
			now.Sub(v.(evictionEntry).lastUpdate) > ttl {
			p.evictionCounts.Delete(runID)
			swept++
		}
		mu.Unlock()
		return true
	})
	return swept
}

// EvictionSweeper periodically reclaims expired entries from a Provisioner's
// eviction-retry counter map. It implements sigs.k8s.io/controller-runtime/pkg/
// manager.Runnable; wire it with mgr.Add. Each AGC replica manages its own
// counters, so it runs on every replica (NeedLeaderElection is false).
type EvictionSweeper struct {
	p        *Provisioner
	interval time.Duration
	ttl      time.Duration
}

// NewEvictionSweeper returns an EvictionSweeper for p using the default sweep
// interval and counter TTL.
func NewEvictionSweeper(p *Provisioner) *EvictionSweeper {
	return &EvictionSweeper{
		p:        p,
		interval: defaultEvictionSweepInterval,
		ttl:      defaultEvictionCounterTTL,
	}
}

// Start runs the sweep loop until ctx is cancelled. It satisfies
// sigs.k8s.io/controller-runtime/pkg/manager.Runnable.
func (s *EvictionSweeper) Start(ctx context.Context) error {
	t := time.NewTicker(s.interval)
	defer t.Stop()
	log := s.p.logFor()
	log.Info("eviction-counter sweeper started", "interval", s.interval, "ttl", s.ttl)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if n := s.p.sweepEvictionCounts(s.ttl); n > 0 {
				log.Info("reclaimed expired eviction-retry counters", "count", n)
			}
		}
	}
}

// NeedLeaderElection reports that the sweeper runs on every replica, not only
// the leader: each AGC instance owns the eviction counters for the pods it
// provisioned.
func (s *EvictionSweeper) NeedLeaderElection() bool { return false }

// evictionShard maps a run_id to one of evictionLockShards mutex indices.
func evictionShard(runID string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(runID))
	return h.Sum32() % evictionLockShards
}

// rerunFailedJobs calls POST /repos/{owner}/{repo}/actions/runs/{run_id}/rerun-failed-jobs.
func (p *Provisioner) rerunFailedJobs(ctx context.Context, owner, repo, runID string, log *slog.Logger) error {
	if owner == "" || repo == "" {
		log.Warn("owner/repo unknown; cannot trigger rerun", "runID", runID)
		return nil
	}
	if !repoSegmentRE.MatchString(owner) || !repoSegmentRE.MatchString(repo) {
		return fmt.Errorf("invalid owner/repo characters: %q/%q", owner, repo)
	}
	if p.TokenFunc == nil {
		log.Warn("TokenFunc not configured; cannot trigger rerun", "runID", runID)
		return nil
	}

	token, err := p.TokenFunc(ctx)
	if err != nil {
		return fmt.Errorf("get installation token: %w", err)
	}

	// No silent default (Q504). This used to fall back to https://api.github.com when
	// unset, which is how the field being unassigned went unnoticed for so long: on a
	// GHES deployment the recovery posted a perfectly valid installation token — minted
	// against the configured endpoint — to a host that had never issued it, and the only
	// symptom was a 401 naming a server the operator had not configured. Sending an
	// installation credential somewhere it was not issued for is worth failing loudly
	// over, so an unset base URL is now a configuration error rather than a guess.
	if p.GitHubAPIURL == "" {
		return fmt.Errorf("GitHubAPIURL is not configured; refusing to guess an endpoint for the rerun call")
	}
	apiURL := fmt.Sprintf("%s/repos/%s/%s/actions/runs/%s/rerun-failed-jobs",
		p.GitHubAPIURL,
		neturl.PathEscape(owner),
		neturl.PathEscape(repo),
		neturl.PathEscape(runID))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader([]byte("{}")))
	if err != nil {
		return fmt.Errorf("build rerun request: %w", err)
	}
	req.Header.Set("Authorization", "token "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")

	hc := p.HTTPClient
	if hc == nil {
		hc = defaultProvisionerClient()
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("rerun API call: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	// GitHub returns 201 Created on success.
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("rerun API returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}
