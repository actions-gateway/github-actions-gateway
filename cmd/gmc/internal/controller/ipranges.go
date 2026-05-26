package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	gmcv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/api/v1alpha1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// IPRangeCache holds the most recently fetched GitHub IP CIDRs.
//
// The cache decouples reads from network I/O: the ActionsGatewayReconciler
// reads from the cache on every reconcile (non-blocking, in-memory) while
// IPRangeReconciler refreshes the cache periodically. Without this split,
// per-reconcile fetches of api.github.com/meta serialise behind a single
// goroutine and can stall the entire reconciler queue when the GitHub API
// is slow or unreachable — a real failure observed in e2e where multiple
// tenant CRs created at once would time out waiting for proxy readiness.
//
// Reconcile paths must tolerate an empty Snapshot. At startup the cache
// is empty until IPRangeReconciler.Start completes its initial fetch;
// any NetworkPolicies created with the empty snapshot will be patched
// by the periodic reconciler on its next tick.
type IPRangeCache struct {
	mu    sync.RWMutex
	cidrs []net.IPNet
}

// Snapshot returns a copy of the currently cached CIDRs. Safe to call
// concurrently; the returned slice is owned by the caller.
func (c *IPRangeCache) Snapshot() []net.IPNet {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]net.IPNet, len(c.cidrs))
	copy(out, c.cidrs)
	return out
}

// Set replaces the cached CIDRs with a copy of the provided slice.
// Called by IPRangeReconciler after each successful fetch.
func (c *IPRangeCache) Set(cidrs []net.IPNet) {
	out := make([]net.IPNet, len(cidrs))
	copy(out, cidrs)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cidrs = out
}

// GitHubIPRangeFetcher fetches the current GitHub IP ranges.
// The default implementation calls https://api.github.com/meta.
// Tests inject a stub that returns a fixed set of CIDRs.
type GitHubIPRangeFetcher interface {
	FetchIPRanges(ctx context.Context) ([]net.IPNet, error)
}

// HTTPGitHubIPRangeFetcher is the production fetcher that calls api.github.com/meta.
type HTTPGitHubIPRangeFetcher struct {
	Client *http.Client
	APIURL string // override in tests; default "https://api.github.com"
}

type githubMetaResponse struct {
	Actions []string `json:"actions"`
}

// FetchIPRanges fetches GitHub Actions IP ranges from the GitHub meta API.
func (f *HTTPGitHubIPRangeFetcher) FetchIPRanges(ctx context.Context) ([]net.IPNet, error) {
	apiURL := f.APIURL
	if apiURL == "" {
		apiURL = "https://api.github.com"
	}
	hc := f.Client
	if hc == nil {
		hc = http.DefaultClient
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL+"/meta", nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch GitHub meta: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub meta returned %d", resp.StatusCode)
	}

	var meta githubMetaResponse
	if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	cidrs := make([]net.IPNet, 0, len(meta.Actions))
	for _, s := range meta.Actions {
		_, cidr, err := net.ParseCIDR(s)
		if err != nil {
			continue
		}
		cidrs = append(cidrs, *cidr)
	}
	return cidrs, nil
}

// IPRangeReconciler is a controller-runtime Runnable that periodically
// refreshes NetworkPolicy egress rules for all managed ActionsGateway CRs.
//
// If Cache is non-nil, each successful fetch updates it. ActionsGatewayReconciler
// reads from the same cache instead of fetching on every reconcile.
type IPRangeReconciler struct {
	client.Client
	Fetcher  GitHubIPRangeFetcher
	Cache    *IPRangeCache
	Interval time.Duration
	Log      *slog.Logger
}

// Start implements manager.Runnable. It runs until ctx is cancelled.
func (r *IPRangeReconciler) Start(ctx context.Context) error {
	interval := r.Interval
	if interval == 0 {
		interval = 24 * time.Hour
	}
	log := r.Log
	if log == nil {
		log = slog.Default()
	}

	// Run once immediately on start, then on each tick.
	r.reconcileAll(ctx, log)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			r.reconcileAll(ctx, log)
		}
	}
}

// ReconcileNow triggers an immediate reconciliation of all ActionsGateway CRs.
// Intended for use in integration tests.
func (r *IPRangeReconciler) ReconcileNow(ctx context.Context) {
	r.reconcileAll(ctx, slog.Default())
}

func (r *IPRangeReconciler) reconcileAll(ctx context.Context, log *slog.Logger) {
	cidrs, err := r.Fetcher.FetchIPRanges(ctx)
	if err != nil {
		log.Error("failed to fetch GitHub IP ranges", "error", err)
		return
	}
	if r.Cache != nil {
		r.Cache.Set(cidrs)
	}

	var agList gmcv1alpha1.ActionsGatewayList
	if err := r.List(ctx, &agList); err != nil {
		log.Error("failed to list ActionsGateways", "error", err)
		return
	}

	for i := range agList.Items {
		ag := &agList.Items[i]
		if !ag.DeletionTimestamp.IsZero() {
			continue // skip CRs being deleted; their NetworkPolicy is already being removed
		}
		if ag.Spec.Proxy.ManagedNetworkPolicy != nil && !*ag.Spec.Proxy.ManagedNetworkPolicy {
			continue
		}
		if err := r.patchNetworkPolicy(ctx, ag, cidrs); err != nil {
			log.Error("failed to patch NetworkPolicy", "namespace", ag.Namespace, "name", ag.Name, "error", err)
		}
	}
}

func (r *IPRangeReconciler) patchNetworkPolicy(ctx context.Context, ag *gmcv1alpha1.ActionsGateway, cidrs []net.IPNet) error {
	var np networkingv1.NetworkPolicy
	if err := r.Get(ctx, types.NamespacedName{Namespace: ag.Namespace, Name: npProxyName}, &np); err != nil {
		return client.IgnoreNotFound(err) // NetworkPolicy may not exist yet or is being removed
	}

	desired := buildProxyNetworkPolicy(ag, cidrs)
	np.Spec.Egress = desired.Spec.Egress
	np.Spec.Ingress = desired.Spec.Ingress

	return r.Update(ctx, &np)
}
