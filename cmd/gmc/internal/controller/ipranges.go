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

	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/actions-gateway/github-actions-gateway/githubapp/httpx"
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
	// lastRefresh is the wall-clock time of the most recent successful IP-range
	// fetch (zero until the first completes). The ActionsGateway EgressRulesStale
	// condition (Q157) is derived from it: a refresh loop that has stalled past the
	// staleness window means GitHub may have rotated ranges out from under a frozen
	// allowlist. Guarded by mu alongside cidrs.
	lastRefresh time.Time
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

// MarkRefreshed records the time of a successful IP-range refresh. Called by
// IPRangeReconciler after each successful fetch so the ActionsGateway reconciler
// can detect a stalled refresh cycle and report EgressRulesStale (Q157). Kept
// separate from Set so the timestamp tracks fetch success, not CIDR mutation.
func (c *IPRangeCache) MarkRefreshed(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastRefresh = t
}

// LastRefresh returns the time of the most recent successful refresh and whether
// any refresh has completed yet (false, before the first fetch, means staleness
// cannot be asserted).
func (c *IPRangeCache) LastRefresh() (time.Time, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lastRefresh, !c.lastRefresh.IsZero()
}

// GitHubIPRangeFetcher fetches the current GitHub IP ranges.
// The default implementation calls https://api.github.com/meta.
// Tests inject a stub that returns a fixed set of CIDRs.
type GitHubIPRangeFetcher interface {
	FetchIPRanges(ctx context.Context) ([]net.IPNet, error)
}

// HTTPGitHubIPRangeFetcher is the production fetcher that calls api.github.com/meta.
type HTTPGitHubIPRangeFetcher struct {
	// Client makes the meta API call. nil uses a bounded httpx.NewClient() (Q138)
	// so a slow api.github.com cannot wedge the reconcile.
	Client *http.Client
	APIURL string // override in tests; default "https://api.github.com"

	// AttemptTimeout bounds a single FetchIPRanges call (one retry attempt).
	// Zero selects defaultFetchAttemptTimeout. See FetchIPRanges (Q62).
	AttemptTimeout time.Duration
}

// defaultFetchAttemptTimeout bounds one /meta fetch attempt. The reconcileInitial
// backoff loop (Q61) retries FetchIPRanges on failure; without a per-attempt
// deadline a stalled attempt would burn the client's full overall Timeout (Q138's
// httpx.DefaultTimeout, 30s) before the backoff can retry. A tighter per-attempt
// cap cuts a black-holed attempt quickly so several retries fit inside one
// overall budget. It is the per-attempt analogue of, and sits below, the client's
// overall Timeout (Q62).
const defaultFetchAttemptTimeout = 10 * time.Second

// defaultIPRangeClient is the bounded fallback used when Client is nil. Shared
// so the nil path does not allocate a connection pool per refresh.
var defaultIPRangeClient = httpx.NewClient()

type githubMetaResponse struct {
	// API is the api.github.com origin range; required for installation token
	// exchange, runner registration-token, and rerun-failed-jobs calls.
	API []string `json:"api"`
	// Actions is the Azure Pipelines range that fronts the broker
	// (pipelinesghubeus*.actions.githubusercontent.com) and the
	// *.actions.githubusercontent.com job log/blob endpoints.
	Actions []string `json:"actions"`
	// Web includes codeload/objects.githubusercontent.com, used by checkout
	// and any actions/cache I/O.
	Web []string `json:"web"`
}

// FetchIPRanges fetches GitHub Actions IP ranges from the GitHub meta API.
func (f *HTTPGitHubIPRangeFetcher) FetchIPRanges(ctx context.Context) ([]net.IPNet, error) {
	apiURL := f.APIURL
	if apiURL == "" {
		apiURL = "https://api.github.com"
	}
	hc := f.Client
	if hc == nil {
		hc = defaultIPRangeClient
	}

	// Bound this single attempt below the client's overall Timeout so a stalled
	// fetch is cut quickly and the Q61 backoff can retry within the overall
	// budget instead of burning the whole client Timeout on one black-holed
	// attempt (Q62).
	attemptTimeout := f.AttemptTimeout
	if attemptTimeout <= 0 {
		attemptTimeout = defaultFetchAttemptTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, attemptTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL+"/meta", nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch GitHub meta: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub meta returned %d", resp.StatusCode)
	}

	var meta githubMetaResponse
	if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	// Merge the api, actions, and web ranges. Each covers a distinct GitHub
	// endpoint family that the AGC or worker pods must reach: api.github.com
	// (token exchange, runner registration), *.actions.githubusercontent.com
	// (broker, job logs), and codeload/objects.githubusercontent.com (checkout,
	// cache). Restricting egress to any single range silently breaks one of them.
	combined := make([]string, 0, len(meta.API)+len(meta.Actions)+len(meta.Web))
	combined = append(combined, meta.API...)
	combined = append(combined, meta.Actions...)
	combined = append(combined, meta.Web...)
	cidrs := make([]net.IPNet, 0, len(combined))
	for _, s := range combined {
		_, cidr, err := net.ParseCIDR(s)
		if err != nil {
			continue
		}
		cidrs = append(cidrs, *cidr)
	}
	return cidrs, nil
}

// Default bounds for the initial-fetch retry loop in Start. See reconcileInitial.
const (
	defaultInitialBackoff = 1 * time.Second
	defaultMaxBackoff     = 30 * time.Second
)

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
	// Metrics, when non-nil, counts each successful NetworkPolicy patch via
	// actions_gateway_ip_range_updates_total. Optional so tests can omit it.
	Metrics *Metrics

	// APIServerCIDRs scopes the rebuilt direct-egress AGC NetworkPolicy's apiserver
	// rule (Q145), so a refresh preserves the operator's apiserver scoping rather than
	// resetting it to any-destination. Mirror of ActionsGatewayV2Reconciler.APIServerCIDRs;
	// empty keeps the rule any-destination (the secure default). Q168.
	APIServerCIDRs []string

	// InitialBackoff and MaxBackoff bound the capped exponential backoff used
	// to retry the initial fetch in Start (see reconcileInitial). Zero selects
	// defaultInitialBackoff / defaultMaxBackoff; tests set small values.
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
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

	// Run an initial reconcile immediately, retrying with backoff until the
	// first fetch succeeds, then refresh on each tick.
	r.reconcileInitial(ctx, log)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			_ = r.reconcileAll(ctx, log)
		}
	}
}

// reconcileInitial runs reconcileAll, retrying with capped exponential backoff
// until the first fetch succeeds or ctx is cancelled.
//
// The first successful fetch populates the IP-range cache. Every managed proxy
// NetworkPolicy's ipBlock egress allowlist is derived from that cache (by both
// ActionsGatewayReconciler at NP-creation time and reconcileAll's patch pass),
// so until the first fetch lands, proxy egress to GitHub is empty and silently
// dropped. Because the periodic refresh Interval is 24h in production, a single
// transient failure or stall on the very first fetch would otherwise leave
// egress broken for a full day — observed as the ProxyConnectWorks e2e flake
// (Q61). Retrying the initial fetch on a sub-Interval cadence closes that gap;
// the subsequent patch pass repairs any NP that was created with the empty
// cache during the retry window.
func (r *IPRangeReconciler) reconcileInitial(ctx context.Context, log *slog.Logger) {
	initial := r.InitialBackoff
	if initial <= 0 {
		initial = defaultInitialBackoff
	}
	maxBackoff := r.MaxBackoff
	if maxBackoff <= 0 {
		maxBackoff = defaultMaxBackoff
	}

	backoff := initial
	for {
		if err := r.reconcileAll(ctx, log); err == nil {
			return
		}
		log.Warn("retrying initial GitHub IP-range fetch", "backoff", backoff.String())
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

// ReconcileNow triggers an immediate reconciliation of all ActionsGateway CRs.
// Intended for use in integration tests.
func (r *IPRangeReconciler) ReconcileNow(ctx context.Context) {
	_ = r.reconcileAll(ctx, slog.Default())
}

// reconcileAll fetches the current GitHub IP ranges, updates the cache, and
// patches every managed proxy NetworkPolicy. It returns an error if the fetch
// or the ActionsGateway list fails — the two cases that leave no NetworkPolicy
// patched, which reconcileInitial retries. Per-CR patch failures are logged but
// not returned: the next tick and the per-CR reconcile recover individual CRs,
// and one bad CR must not block the retry loop from completing.
func (r *IPRangeReconciler) reconcileAll(ctx context.Context, log *slog.Logger) error {
	cidrs, err := r.Fetcher.FetchIPRanges(ctx)
	if err != nil {
		log.Error("failed to fetch GitHub IP ranges", "error", err)
		return err
	}
	if r.Cache != nil {
		r.Cache.Set(cidrs)
		// Stamp the successful fetch so the ActionsGateway reconciler can report
		// EgressRulesStale if this loop later stalls (Q157).
		r.Cache.MarkRefreshed(time.Now())
	}

	var agList gmcv1alpha1.ActionsGatewayList
	if err := r.List(ctx, &agList); err != nil {
		log.Error("failed to list ActionsGateways", "error", err)
		return err
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

	// v2: patch every managed EgressProxy NetworkPolicy too. Like the v1 proxy NP,
	// its GitHub-CIDR egress allowlist is derived from this cache, so it must be
	// refreshed when GitHub's published ranges rotate. A List failure is logged and
	// skipped rather than returned: a missing v2 CRD on a v1-only install must not
	// fail the whole refresh loop and starve the v1 NetworkPolicies of updates.
	var epList gmcv2alpha1.EgressProxyList
	if err := r.List(ctx, &epList); err != nil {
		log.Error("failed to list EgressProxies", "error", err)
		return nil
	}
	for i := range epList.Items {
		ep := &epList.Items[i]
		if !ep.DeletionTimestamp.IsZero() {
			continue
		}
		if ep.Spec.ManagedNetworkPolicy != nil && !*ep.Spec.ManagedNetworkPolicy {
			continue
		}
		if !egressUsesCIDR(ep.Spec) {
			continue // FQDN mode (Q208): the CNI-native policy carries no CIDRs to refresh
		}
		if err := r.patchEgressProxyNetworkPolicy(ctx, ep, cidrs); err != nil {
			log.Error("failed to patch EgressProxy NetworkPolicy", "namespace", ep.Namespace, "name", ep.Name, "error", err)
		}
	}

	// v2 direct egress (Q168, §H.10): a v2 ActionsGateway with no defaultProxyRef
	// egresses directly, and its AGC + workload NetworkPolicies carry the GitHub-CIDR
	// allowlist instead of a proxy rule. That allowlist is derived from this same
	// cache, so — like the proxy NetworkPolicies above — it must be refreshed when
	// GitHub rotates ranges. Proxied gateways have no GitHub rule on these policies,
	// so they are skipped. A v2 CRD missing on a v1-only install is tolerated.
	var v2agList gmcv2alpha1.ActionsGatewayList
	if err := r.List(ctx, &v2agList); err != nil {
		log.Error("failed to list v2 ActionsGateways", "error", err)
		return nil
	}
	for i := range v2agList.Items {
		v2ag := &v2agList.Items[i]
		if !v2ag.DeletionTimestamp.IsZero() {
			continue
		}
		if v2ag.Spec.DefaultProxyRef != nil {
			continue // proxied: AGC/workload NetworkPolicies carry no GitHub rule
		}
		if err := r.patchDirectEgressNetworkPolicies(ctx, v2ag, cidrs); err != nil {
			log.Error("failed to patch direct-egress NetworkPolicies", "namespace", v2ag.Namespace, "name", v2ag.Name, "error", err)
		}
	}
	return nil
}

// patchDirectEgressNetworkPolicies refreshes a direct-egress v2 ActionsGateway's AGC
// and workload NetworkPolicy egress rules from the current CIDR set, so the
// direct-egress GitHub allowlist stays current as GitHub rotates ranges (Q168). The
// v2 direct-mode analogue of patchNetworkPolicy / patchEgressProxyNetworkPolicy. A
// NetworkPolicy that does not exist yet (or is being removed) is skipped; the per-CR
// reconcile creates it. The patch is metrics-counted once per gateway.
func (r *IPRangeReconciler) patchDirectEgressNetworkPolicies(ctx context.Context, ag *gmcv2alpha1.ActionsGateway, cidrs []net.IPNet) error {
	patched := false
	patch := func(name string, desired *networkingv1.NetworkPolicy) error {
		var np networkingv1.NetworkPolicy
		if err := r.Get(ctx, types.NamespacedName{Namespace: ag.Namespace, Name: name}, &np); err != nil {
			return client.IgnoreNotFound(err)
		}
		np.Spec.Egress = desired.Spec.Egress
		np.Spec.Ingress = desired.Spec.Ingress
		if err := r.Update(ctx, &np); err != nil {
			return err
		}
		patched = true
		return nil
	}

	if err := patch(workloadNPNameV2(ag), buildWorkloadNetworkPolicyV2(ag, cidrs, true)); err != nil {
		return err
	}
	if err := patch(agcNameV2(ag), buildAGCNetworkPolicyV2(ag, r.APIServerCIDRs, cidrs, true)); err != nil {
		return err
	}
	if patched && r.Metrics != nil {
		r.Metrics.IPRangeUpdates.WithLabelValues(ag.Namespace).Inc()
	}
	return nil
}

// patchEgressProxyNetworkPolicy refreshes one EgressProxy's proxy NetworkPolicy
// egress/ingress rules from the current CIDR set. The v2 analogue of
// patchNetworkPolicy.
func (r *IPRangeReconciler) patchEgressProxyNetworkPolicy(ctx context.Context, ep *gmcv2alpha1.EgressProxy, cidrs []net.IPNet) error {
	var np networkingv1.NetworkPolicy
	if err := r.Get(ctx, types.NamespacedName{Namespace: ep.Namespace, Name: proxyResourceName(ep)}, &np); err != nil {
		return client.IgnoreNotFound(err) // NetworkPolicy may not exist yet or is being removed
	}

	desired := buildEgressProxyNetworkPolicy(ep, cidrs)
	np.Spec.Egress = desired.Spec.Egress
	np.Spec.Ingress = desired.Spec.Ingress

	if err := r.Update(ctx, &np); err != nil {
		return err
	}
	if r.Metrics != nil {
		r.Metrics.IPRangeUpdates.WithLabelValues(ep.Namespace).Inc()
	}
	return nil
}

func (r *IPRangeReconciler) patchNetworkPolicy(ctx context.Context, ag *gmcv1alpha1.ActionsGateway, cidrs []net.IPNet) error {
	var np networkingv1.NetworkPolicy
	if err := r.Get(ctx, types.NamespacedName{Namespace: ag.Namespace, Name: npProxyName}, &np); err != nil {
		return client.IgnoreNotFound(err) // NetworkPolicy may not exist yet or is being removed
	}

	desired := buildProxyNetworkPolicy(ag, cidrs)
	np.Spec.Egress = desired.Spec.Egress
	np.Spec.Ingress = desired.Spec.Ingress

	if err := r.Update(ctx, &np); err != nil {
		return err
	}
	if r.Metrics != nil {
		r.Metrics.IPRangeUpdates.WithLabelValues(ag.Namespace).Inc()
	}
	return nil
}
