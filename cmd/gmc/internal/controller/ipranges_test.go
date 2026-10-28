package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/githubapp/httpx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agcv1alpha1 "github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	gmcv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/api/v1alpha1"
)

// stubFetcher is a test double for GitHubIPRangeFetcher.
type stubFetcher struct {
	cidrs []net.IPNet
	err   error
}

func (s *stubFetcher) FetchIPRanges(_ context.Context) ([]net.IPNet, error) {
	return s.cidrs, s.err
}

func newIPRangeScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = gmcv1alpha1.AddToScheme(s)
	_ = agcv1alpha1.AddToScheme(s)
	return s
}

func parseCIDR(t *testing.T, s string) net.IPNet {
	t.Helper()
	_, cidr, err := net.ParseCIDR(s)
	require.NoError(t, err)
	return *cidr
}

func TestIPRangeReconciler_UpdatesNetworkPolicy(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	scheme := newIPRangeScheme(t)

	ag := &gmcv1alpha1.ActionsGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "team-a"},
		Spec: gmcv1alpha1.ActionsGatewaySpec{
			GitHubAppRef: gmcv1alpha1.SecretReference{Name: "s"},
		},
	}
	// Create a pre-existing proxy NetworkPolicy that the reconciler should update.
	np := buildProxyNetworkPolicy(ag, nil)

	fc := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(ag, np).
		Build()

	cidrs := []net.IPNet{parseCIDR(t, "140.82.112.0/20")}
	r := &IPRangeReconciler{
		Client:   fc,
		Fetcher:  &stubFetcher{cidrs: cidrs},
		Interval: time.Hour,
	}

	// Run one tick synchronously.
	_ = r.reconcileAll(ctx, slog.Default())

	// Check that the proxy NetworkPolicy was updated with the new CIDRs.
	var updated networkingv1.NetworkPolicy
	require.NoError(t, fc.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: npProxyName}, &updated))

	found := false
	for _, rule := range updated.Spec.Egress {
		for _, port := range rule.Ports {
			if port.Port != nil && port.Port.IntVal == 443 {
				for _, peer := range rule.To {
					if peer.IPBlock != nil && peer.IPBlock.CIDR == "140.82.112.0/20" {
						found = true
					}
				}
			}
		}
	}
	assert.True(t, found, "proxy NetworkPolicy should contain the updated GitHub CIDR")
}

func TestIPRangeReconciler_SkipsManagedFalse(t *testing.T) {
	ctx := context.Background()
	scheme := newIPRangeScheme(t)

	ag := &gmcv1alpha1.ActionsGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "team-a"},
		Spec: gmcv1alpha1.ActionsGatewaySpec{
			GitHubAppRef: gmcv1alpha1.SecretReference{Name: "s"},
			Proxy:        gmcv1alpha1.ProxyConfig{ManagedNetworkPolicy: ptr(false)},
		},
	}
	np := buildProxyNetworkPolicy(ag, nil)

	fc := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(ag, np).
		Build()

	cidrs := []net.IPNet{parseCIDR(t, "140.82.112.0/20")}
	r := &IPRangeReconciler{Client: fc, Fetcher: &stubFetcher{cidrs: cidrs}}
	_ = r.reconcileAll(ctx, slog.Default())

	// Proxy NetworkPolicy should not contain the GitHub CIDR.
	var updated networkingv1.NetworkPolicy
	require.NoError(t, fc.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: npProxyName}, &updated))

	for _, rule := range updated.Spec.Egress {
		for _, port := range rule.Ports {
			if port.Port != nil && port.Port.IntVal == 443 {
				for _, peer := range rule.To {
					if peer.IPBlock != nil {
						assert.NotEqual(t, "140.82.112.0/20", peer.IPBlock.CIDR, "should not patch when managedNetworkPolicy=false")
					}
				}
			}
		}
	}
}

func TestIPRangeReconciler_FetchError(t *testing.T) {
	ctx := context.Background()
	scheme := newIPRangeScheme(t)

	ag := &gmcv1alpha1.ActionsGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "team-a"},
		Spec:       gmcv1alpha1.ActionsGatewaySpec{GitHubAppRef: gmcv1alpha1.SecretReference{Name: "s"}},
	}
	np := buildProxyNetworkPolicy(ag, nil)
	originalEgress := np.Spec.Egress

	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ag, np).Build()

	r := &IPRangeReconciler{Client: fc, Fetcher: &stubFetcher{err: errors.New("network error")}}
	_ = r.reconcileAll(ctx, slog.Default()) // must not panic

	// Proxy NetworkPolicy should be unchanged.
	var updated networkingv1.NetworkPolicy
	require.NoError(t, fc.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: npProxyName}, &updated))
	assert.Equal(t, len(originalEgress), len(updated.Spec.Egress))
}

// TestIPRangeReconciler_WorkloadEgressPreservedAfterRefresh is a regression test for M-9:
// the IPRangeReconciler used to rebuild the (now-removed) single NetworkPolicy with an empty
// proxyClusterIP, which dropped the worker→proxy egress rule on every refresh. With the split
// into proxy and workload policies, the reconciler only patches the proxy NP; the workload NP
// is untouched by IP range refreshes.
func TestIPRangeReconciler_WorkloadEgressPreservedAfterRefresh(t *testing.T) {
	ctx := context.Background()
	scheme := newIPRangeScheme(t)

	ag := &gmcv1alpha1.ActionsGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "team-a"},
		Spec:       gmcv1alpha1.ActionsGatewaySpec{GitHubAppRef: gmcv1alpha1.SecretReference{Name: "s"}},
	}
	proxyNP := buildProxyNetworkPolicy(ag, nil)
	workloadNP := buildWorkloadNetworkPolicy(ag)

	fc := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(ag, proxyNP, workloadNP).
		Build()

	cidrs := []net.IPNet{parseCIDR(t, "140.82.112.0/20")}
	r := &IPRangeReconciler{Client: fc, Fetcher: &stubFetcher{cidrs: cidrs}}
	_ = r.reconcileAll(ctx, slog.Default())

	// Workload NP must still have the proxy egress rule (PodSelector on the proxy
	// app label) — the reconciler must not touch it.
	var updated networkingv1.NetworkPolicy
	require.NoError(t, fc.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: npWorkloadName}, &updated))

	found := false
	for _, rule := range updated.Spec.Egress {
		for _, port := range rule.Ports {
			if port.Port != nil && port.Port.IntVal == proxyPort {
				for _, peer := range rule.To {
					if peer.PodSelector != nil &&
						peer.PodSelector.MatchLabels["app"] == proxyAppName {
						found = true
					}
				}
			}
		}
	}
	assert.True(t, found, "workload NP proxy egress rule must survive an IP range reconcile (M-9 regression)")
}

// §3 — HTTPGitHubIPRangeFetcher production path

func TestHTTPFetcher_ParsesCIDRs(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"actions":["140.82.112.0/20","192.30.252.0/22"]}`)
	}))
	defer ts.Close()

	f := &HTTPGitHubIPRangeFetcher{APIURL: ts.URL}
	cidrs, err := f.FetchIPRanges(context.Background())
	require.NoError(t, err)
	assert.Len(t, cidrs, 2)
}

func TestHTTPFetcher_Non200Response(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	f := &HTTPGitHubIPRangeFetcher{APIURL: ts.URL}
	_, err := f.FetchIPRanges(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "500")
}

// TestHTTPFetcher_RateLimitHeaders pins the GitHub rate-limit contract the retry
// loop keys on (Q1203). The header shapes are those GitHub documents for the REST
// API, and the 403 + remaining=0 + reset case is what the 2026-10-24 kind
// measurement read from api.github.com/meta.
func TestHTTPFetcher_RateLimitHeaders(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	reset := strconv.FormatInt(now.Add(28*time.Minute).Unix(), 10)
	cases := []struct {
		name    string
		status  int
		headers map[string]string
		want    time.Duration // RetryAfter; -1 for no hint
	}{
		{"403 primary limit waits for reset", http.StatusForbidden,
			map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": reset}, 28*time.Minute + time.Second},
		{"429 primary limit waits for reset", http.StatusTooManyRequests,
			map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": reset}, 28*time.Minute + time.Second},
		{"Retry-After alone", http.StatusForbidden,
			map[string]string{"Retry-After": "90"}, 90 * time.Second},
		{"Retry-After wins over reset", http.StatusTooManyRequests,
			map[string]string{"Retry-After": "120", "X-RateLimit-Remaining": "0", "X-RateLimit-Reset": reset}, 2 * time.Minute},
		{"remaining 0 with reset missing", http.StatusForbidden,
			map[string]string{"X-RateLimit-Remaining": "0"}, -1},
		{"reset present but quota left", http.StatusForbidden,
			map[string]string{"X-RateLimit-Remaining": "12", "X-RateLimit-Reset": reset}, -1},
		{"reset already passed", http.StatusForbidden,
			map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": strconv.FormatInt(now.Add(-time.Minute).Unix(), 10)}, 0},
		{"429 with no headers", http.StatusTooManyRequests, nil, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				for k, v := range tc.headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
			}))
			defer ts.Close()

			f := &HTTPGitHubIPRangeFetcher{APIURL: ts.URL, now: func() time.Time { return now }}
			_, err := f.FetchIPRanges(context.Background())
			var rl *httpx.RateLimitError
			require.ErrorAs(t, err, &rl)
			assert.Equal(t, tc.want, rl.RetryAfter)
			assert.Contains(t, err.Error(), strconv.Itoa(tc.status), "the log line must still name the status")
		})
	}
}

// TestHTTPFetcher_SecondaryLimitBodyIsRateLimit classifies a header-less 403 by
// the message GitHub documents for a secondary limit.
func TestHTTPFetcher_SecondaryLimitBodyIsRateLimit(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again."}`))
	}))
	defer ts.Close()

	_, err := (&HTTPGitHubIPRangeFetcher{APIURL: ts.URL}).FetchIPRanges(context.Background())
	var rl *httpx.RateLimitError
	require.ErrorAs(t, err, &rl)
	assert.Equal(t, time.Duration(-1), rl.RetryAfter)
}

// TestHTTPFetcher_ProxyForbiddenIsNotRateLimit keeps a 403 with no GitHub
// rate-limit header or message, such as a forward proxy's deny page, on the
// ordinary backoff.
func TestHTTPFetcher_ProxyForbiddenIsNotRateLimit(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("<html><body>Access to api.github.com is denied by policy.</body></html>"))
	}))
	defer ts.Close()

	_, err := (&HTTPGitHubIPRangeFetcher{APIURL: ts.URL}).FetchIPRanges(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "403")
	var rl *httpx.RateLimitError
	assert.False(t, errors.As(err, &rl))
}

// TestHTTPFetcher_ServerErrorIsNotRateLimit keeps a 5xx on the ordinary backoff.
func TestHTTPFetcher_ServerErrorIsNotRateLimit(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer ts.Close()

	_, err := (&HTTPGitHubIPRangeFetcher{APIURL: ts.URL}).FetchIPRanges(context.Background())
	require.Error(t, err)
	var rl *httpx.RateLimitError
	assert.False(t, errors.As(err, &rl))
}

func TestHTTPFetcher_InvalidJSON(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `not json`)
	}))
	defer ts.Close()

	f := &HTTPGitHubIPRangeFetcher{APIURL: ts.URL}
	_, err := f.FetchIPRanges(context.Background())
	require.Error(t, err)
}

func TestHTTPFetcher_MalformedCIDRSkipped(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"actions":["140.82.112.0/20","not-a-cidr"]}`)
	}))
	defer ts.Close()

	f := &HTTPGitHubIPRangeFetcher{APIURL: ts.URL}
	cidrs, err := f.FetchIPRanges(context.Background())
	require.NoError(t, err)
	assert.Len(t, cidrs, 1)
}

func TestHTTPFetcher_ContextCancelled(t *testing.T) {
	// Server that blocks forever so the client must rely on context cancellation.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // blocks until the client cancels
	}))
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	f := &HTTPGitHubIPRangeFetcher{APIURL: ts.URL}
	_, err := f.FetchIPRanges(ctx)
	require.Error(t, err, "cancelled context should produce an error")
}

// TestHTTPFetcher_PerAttemptTimeout verifies that a stalled attempt is bounded
// by HTTPGitHubIPRangeFetcher.AttemptTimeout (Q62), not by the client's overall
// Timeout. The server blocks until the request context is cancelled; with a long
// overall budget on the parent context, FetchIPRanges must still return shortly
// after the per-attempt deadline so the Q61 backoff loop can retry.
func TestHTTPFetcher_PerAttemptTimeout(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // never responds; relies on the per-attempt deadline
	}))
	defer ts.Close()

	const attempt = 100 * time.Millisecond
	f := &HTTPGitHubIPRangeFetcher{APIURL: ts.URL, AttemptTimeout: attempt}

	// A parent context far longer than the per-attempt timeout: if the attempt
	// were bounded by the parent (or an unbounded client) it would block well
	// past the deadline below.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	_, err := f.FetchIPRanges(ctx)
	elapsed := time.Since(start)

	require.Error(t, err, "stalled attempt should fail at the per-attempt deadline")
	assert.NoError(t, ctx.Err(), "parent context must not be cancelled — only the per-attempt one")
	assert.Less(t, elapsed, 2*time.Second,
		"attempt should be cut near AttemptTimeout (%s), not run to the parent budget; took %s", attempt, elapsed)
}

// TestHTTPFetcher_RetriesProceedAfterStall verifies that once a stalled attempt
// is cut by the per-attempt timeout, a subsequent attempt against a healthy
// server succeeds — i.e. the Q61 backoff can make progress within the overall
// budget rather than being wedged on the first stalled attempt (Q62).
func TestHTTPFetcher_RetriesProceedAfterStall(t *testing.T) {
	var calls int
	var mu sync.Mutex
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			<-r.Context().Done() // first attempt stalls until the per-attempt deadline
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"actions":["140.82.112.0/20"]}`)
	}))
	defer ts.Close()

	f := &HTTPGitHubIPRangeFetcher{APIURL: ts.URL, AttemptTimeout: 100 * time.Millisecond}

	// First attempt: stalled, cut by the per-attempt timeout.
	_, err := f.FetchIPRanges(context.Background())
	require.Error(t, err, "first (stalled) attempt should fail")

	// Second attempt: healthy server responds, fetch succeeds.
	cidrs, err := f.FetchIPRanges(context.Background())
	require.NoError(t, err, "retry after a stalled attempt should succeed")
	assert.Len(t, cidrs, 1)
}

func TestHTTPFetcher_EmptyActions(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"actions":[]}`)
	}))
	defer ts.Close()

	f := &HTTPGitHubIPRangeFetcher{APIURL: ts.URL}
	cidrs, err := f.FetchIPRanges(context.Background())
	require.NoError(t, err)
	assert.Empty(t, cidrs)
}

// TestHTTPFetcher_MergesAllRanges is a regression test for PR #59
// (`fix(gmc): expand proxy egress to api + actions + web GitHub ranges`).
// Before that fix the fetcher merged only the `actions` field, silently
// blocking AGC traffic to api.github.com (token exchange, runner
// registration) and codeload/objects.githubusercontent.com (checkout,
// cache). The fixture mirrors the real /meta shape — each of api,
// actions, and web populated with a distinct CIDR — so the assertion
// proves every family was merged into the returned slice.
func TestHTTPFetcher_MergesAllRanges(t *testing.T) {
	const (
		apiCIDR     = "192.30.252.0/22"  // representative api.github.com range
		actionsCIDR = "4.175.114.0/23"   // representative *.actions.githubusercontent.com range
		webCIDR     = "185.199.108.0/22" // representative codeload/objects range
		noiseCIDR   = "20.201.28.0/22"   // unrelated field — must NOT appear
	)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{
			"verifiable_password_authentication": false,
			"ssh_key_fingerprints": {},
			"api":      [%q],
			"web":      [%q],
			"actions":  [%q],
			"git":      [%q],
			"packages": [%q]
		}`, apiCIDR, webCIDR, actionsCIDR, noiseCIDR, noiseCIDR)
	}))
	defer ts.Close()

	f := &HTTPGitHubIPRangeFetcher{APIURL: ts.URL}
	cidrs, err := f.FetchIPRanges(context.Background())
	require.NoError(t, err)

	got := make(map[string]struct{}, len(cidrs))
	for _, c := range cidrs {
		got[c.String()] = struct{}{}
	}
	assert.Contains(t, got, apiCIDR, "api range must be merged (AGC → api.github.com)")
	assert.Contains(t, got, actionsCIDR, "actions range must be merged (broker, job logs)")
	assert.Contains(t, got, webCIDR, "web range must be merged (codeload/objects for checkout, cache)")
	assert.NotContains(t, got, noiseCIDR, "unrelated /meta fields (git, packages) must not be merged")
}

// §7 — IPRangeReconciler.Start ticker loop

// countingFetcher records how many times FetchIPRanges has been called.
type countingFetcher struct {
	mu    sync.Mutex
	n     int
	cidrs []net.IPNet
}

func (f *countingFetcher) FetchIPRanges(_ context.Context) ([]net.IPNet, error) {
	f.mu.Lock()
	f.n++
	f.mu.Unlock()
	return f.cidrs, nil
}

func (f *countingFetcher) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n
}

func TestIPRangeReconciler_Start_RunsImmediately(t *testing.T) {
	scheme := newIPRangeScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()

	cf := &countingFetcher{}
	r := &IPRangeReconciler{
		Client:   fc,
		Fetcher:  cf,
		Interval: time.Hour, // long enough that the ticker never fires during this test
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()

	require.Eventually(t, func() bool { return cf.count() >= 1 }, time.Second, 5*time.Millisecond,
		"reconcile should run immediately before the first tick")
	cancel()
	require.NoError(t, <-done)
}

func TestIPRangeReconciler_Start_TickerFiresOnInterval(t *testing.T) {
	scheme := newIPRangeScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()

	cf := &countingFetcher{}
	r := &IPRangeReconciler{
		Client:   fc,
		Fetcher:  cf,
		Interval: 10 * time.Millisecond, // short enough to fire several times in a second
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()

	// Wait for at least two calls: the immediate one and at least one tick.
	require.Eventually(t, func() bool { return cf.count() >= 2 }, 2*time.Second, time.Millisecond,
		"ticker should fire at least once after the immediate reconcile")
	cancel()
	require.NoError(t, <-done)
}

// flakyFetcher fails its first failCount calls, then returns cidrs. It models a
// transient outage or stall on the initial api.github.com/meta fetch.
type flakyFetcher struct {
	mu        sync.Mutex
	calls     int
	failCount int
	cidrs     []net.IPNet
}

func (f *flakyFetcher) FetchIPRanges(_ context.Context) ([]net.IPNet, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.calls <= f.failCount {
		return nil, errors.New("transient fetch failure")
	}
	return f.cidrs, nil
}

func (f *flakyFetcher) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func proxyNPHasCIDR(t *testing.T, fc client.Client, ns, cidr string) bool {
	t.Helper()
	var np networkingv1.NetworkPolicy
	if err := fc.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: npProxyName}, &np); err != nil {
		return false
	}
	return hasGitHubCIDREgress(&np, cidr)
}

// TestIPRangeReconciler_Start_RetriesInitialFetch is the regression test for Q61.
// The IPRangeReconciler used to run the initial fetch exactly once on Start; a
// transient failure left the cache (and every proxy NetworkPolicy's ipBlock
// egress allowlist) empty until the next Interval tick — 24h in production —
// surfacing as the ProxyConnectWorks e2e flake. Start must now retry the initial
// fetch on a sub-Interval cadence until it succeeds and patches the proxy NP.
func TestIPRangeReconciler_Start_RetriesInitialFetch(t *testing.T) {
	scheme := newIPRangeScheme(t)

	ag := &gmcv1alpha1.ActionsGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "team-a"},
		Spec:       gmcv1alpha1.ActionsGatewaySpec{GitHubAppRef: gmcv1alpha1.SecretReference{Name: "s"}},
	}
	np := buildProxyNetworkPolicy(ag, nil)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ag, np).Build()

	const cidr = "140.82.112.0/20"
	ff := &flakyFetcher{failCount: 2, cidrs: []net.IPNet{parseCIDR(t, cidr)}}
	r := &IPRangeReconciler{
		Client:         fc,
		Fetcher:        ff,
		Cache:          &IPRangeCache{},
		Interval:       time.Hour, // ticker must not fire during the test — only the retry loop should
		InitialBackoff: time.Millisecond,
		MaxBackoff:     5 * time.Millisecond,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()

	require.Eventually(t, func() bool { return proxyNPHasCIDR(t, fc, "team-a", cidr) },
		2*time.Second, 5*time.Millisecond,
		"initial fetch should be retried until it succeeds and patches the proxy NP")
	assert.GreaterOrEqual(t, ff.count(), 3, "two failures plus the successful retry")

	cancel()
	require.NoError(t, <-done)
}

// TestIPRangeReconciler_Start_RetryStopsOnCancel ensures the initial-fetch retry
// loop honours context cancellation when the fetch never recovers, rather than
// spinning forever.
func TestIPRangeReconciler_Start_RetryStopsOnCancel(t *testing.T) {
	scheme := newIPRangeScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()

	r := &IPRangeReconciler{
		Client:         fc,
		Fetcher:        &stubFetcher{err: errors.New("always fails")},
		Interval:       time.Hour,
		InitialBackoff: 10 * time.Millisecond,
		MaxBackoff:     20 * time.Millisecond,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()

	// Let the retry loop run a few iterations, then cancel mid-retry.
	time.Sleep(30 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("Start did not exit within 1s of cancellation during the retry loop")
	}
}

func TestIPRangeReconciler_Start_CancelExitsCleanly(t *testing.T) {
	scheme := newIPRangeScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before Start is even called

	r := &IPRangeReconciler{
		Client:   fc,
		Fetcher:  &stubFetcher{},
		Interval: time.Hour,
	}

	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()

	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("Start did not exit within 1s after context cancellation")
	}
}

// scriptedFetcher returns errs in order, then cidrs.
type scriptedFetcher struct {
	mu    sync.Mutex
	errs  []error
	cidrs []net.IPNet
}

func (f *scriptedFetcher) FetchIPRanges(_ context.Context) ([]net.IPNet, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.errs) == 0 {
		return f.cidrs, nil
	}
	err := f.errs[0]
	f.errs = f.errs[1:]
	return nil, err
}

// TestIPRangeReconciler_RateLimitedInitialFetchWaits is the regression test for
// Q1203: a rate-limited /meta fetch must wait for GitHub's reset rather than
// retry on the 30s backoff, which kept an exhausted unauthenticated quota at zero.
func TestIPRangeReconciler_RateLimitedInitialFetchWaits(t *testing.T) {
	limited := func(d time.Duration) error {
		return fmt.Errorf("GitHub meta returned 403: %w", &httpx.RateLimitError{RetryAfter: d})
	}
	ff := &scriptedFetcher{
		errs: []error{
			limited(28 * time.Minute),  // reset hint
			limited(5 * time.Second),   // below the one-minute floor
			limited(3 * time.Hour),     // above the one-window ceiling
			limited(-1),                // no hint: one minute, then doubling
			limited(-1),                //
			errors.New("network down"), // ordinary failure keeps the short backoff
		},
		cidrs: []net.IPNet{parseCIDR(t, "140.82.112.0/20")},
	}
	var waits []time.Duration
	r := &IPRangeReconciler{
		Client:         fake.NewClientBuilder().WithScheme(newIPRangeScheme(t)).Build(),
		Fetcher:        ff,
		Cache:          &IPRangeCache{},
		InitialBackoff: time.Second,
		after: func(d time.Duration) <-chan time.Time {
			waits = append(waits, d)
			ch := make(chan time.Time, 1)
			ch <- time.Time{}
			return ch
		},
	}

	r.reconcileInitial(context.Background(), slog.Default())

	assert.Equal(t, []time.Duration{
		28 * time.Minute, time.Minute, time.Hour, time.Minute, 2 * time.Minute, time.Second,
	}, waits)
	assert.Len(t, r.Cache.Snapshot(), 1, "the loop must still finish once the fetch succeeds")
}
