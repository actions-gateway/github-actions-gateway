package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agcv1alpha1 "github.com/karlkfi/github-actions-gateway/agc/api/v1alpha1"
	gmcv1alpha1 "github.com/karlkfi/github-actions-gateway/gmc/api/v1alpha1"
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
	// Create a pre-existing NetworkPolicy that the reconciler should update.
	np := buildNetworkPolicy(ag, "", nil)

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
	r.reconcileAll(ctx, slogDefault())

	// Check that the NetworkPolicy was updated with the new CIDRs.
	var updated networkingv1.NetworkPolicy
	require.NoError(t, fc.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: "actions-gateway"}, &updated))

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
	assert.True(t, found, "NetworkPolicy should contain the updated GitHub CIDR")
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
	np := buildNetworkPolicy(ag, "", nil)

	fc := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(ag, np).
		Build()

	cidrs := []net.IPNet{parseCIDR(t, "140.82.112.0/20")}
	r := &IPRangeReconciler{Client: fc, Fetcher: &stubFetcher{cidrs: cidrs}}
	r.reconcileAll(ctx, slogDefault())

	// NetworkPolicy should not contain the GitHub CIDR.
	var updated networkingv1.NetworkPolicy
	require.NoError(t, fc.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: "actions-gateway"}, &updated))

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
	np := buildNetworkPolicy(ag, "", nil)
	originalEgress := np.Spec.Egress

	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ag, np).Build()

	r := &IPRangeReconciler{Client: fc, Fetcher: &stubFetcher{err: errors.New("network error")}}
	r.reconcileAll(ctx, slogDefault()) // must not panic

	// NetworkPolicy should be unchanged.
	var updated networkingv1.NetworkPolicy
	require.NoError(t, fc.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: "actions-gateway"}, &updated))
	assert.Equal(t, len(originalEgress), len(updated.Spec.Egress))
}

func slogDefault() *slog.Logger { return slog.Default() }

// §3 — HTTPGitHubIPRangeFetcher production path

func TestHTTPFetcher_ParsesCIDRs(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"actions":["140.82.112.0/20","192.30.252.0/22"]}`)
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

func TestHTTPFetcher_InvalidJSON(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `not json`)
	}))
	defer ts.Close()

	f := &HTTPGitHubIPRangeFetcher{APIURL: ts.URL}
	_, err := f.FetchIPRanges(context.Background())
	require.Error(t, err)
}

func TestHTTPFetcher_MalformedCIDRSkipped(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"actions":["140.82.112.0/20","not-a-cidr"]}`)
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

func TestHTTPFetcher_EmptyActions(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"actions":[]}`)
	}))
	defer ts.Close()

	f := &HTTPGitHubIPRangeFetcher{APIURL: ts.URL}
	cidrs, err := f.FetchIPRanges(context.Background())
	require.NoError(t, err)
	assert.Empty(t, cidrs)
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
