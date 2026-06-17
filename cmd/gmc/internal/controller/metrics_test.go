package controller

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gmcv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/api/v1alpha1"
)

// testIPRangeUpdates returns an unregistered Metrics with just the counter, so
// tests do not touch the global controller-runtime registry.
func testIPRangeUpdates() *Metrics {
	return &Metrics{
		IPRangeUpdates: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "test_ip_range_updates_total",
		}, []string{"namespace"}),
	}
}

// A successful NetworkPolicy patch must increment the per-namespace counter.
func TestIPRangeReconciler_IncrementsUpdateCounter(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	scheme := newIPRangeScheme(t)
	ag := &gmcv1alpha1.ActionsGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "team-a"},
		Spec: gmcv1alpha1.ActionsGatewaySpec{
			GitHubAppRef: gmcv1alpha1.SecretReference{Name: "s"},
		},
	}
	np := buildProxyNetworkPolicy(ag, nil)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ag, np).Build()

	m := testIPRangeUpdates()
	r := &IPRangeReconciler{
		Client:   fc,
		Fetcher:  &stubFetcher{cidrs: []net.IPNet{parseCIDR(t, "140.82.112.0/20")}},
		Interval: time.Hour,
		Metrics:  m,
	}

	require.NoError(t, r.reconcileAll(ctx, slogDefault()))
	assert.Equal(t, 1.0, testutil.ToFloat64(m.IPRangeUpdates.WithLabelValues("team-a")),
		"one successful patch should record one update")

	// A second refresh patches again.
	require.NoError(t, r.reconcileAll(ctx, slogDefault()))
	assert.Equal(t, 2.0, testutil.ToFloat64(m.IPRangeUpdates.WithLabelValues("team-a")))
}

// A missing NetworkPolicy is a no-op and must not record an update.
func TestIPRangeReconciler_NoUpdateWhenNetworkPolicyMissing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	scheme := newIPRangeScheme(t)
	ag := &gmcv1alpha1.ActionsGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "team-a"},
		Spec: gmcv1alpha1.ActionsGatewaySpec{
			GitHubAppRef: gmcv1alpha1.SecretReference{Name: "s"},
		},
	}
	// No NetworkPolicy seeded: patchNetworkPolicy hits NotFound and returns nil.
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ag).Build()

	m := testIPRangeUpdates()
	r := &IPRangeReconciler{
		Client:   fc,
		Fetcher:  &stubFetcher{cidrs: []net.IPNet{parseCIDR(t, "140.82.112.0/20")}},
		Interval: time.Hour,
		Metrics:  m,
	}

	require.NoError(t, r.reconcileAll(ctx, slogDefault()))
	assert.Equal(t, 0.0, testutil.ToFloat64(m.IPRangeUpdates.WithLabelValues("team-a")),
		"a missing NetworkPolicy must not record an update")
}

func managedGateway(name string, deleting bool) *gmcv1alpha1.ActionsGateway {
	ag := &gmcv1alpha1.ActionsGateway{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: name},
		Spec: gmcv1alpha1.ActionsGatewaySpec{
			GitHubAppRef: gmcv1alpha1.SecretReference{Name: "s"},
		},
	}
	if deleting {
		now := metav1.Now()
		ag.DeletionTimestamp = &now
		// A deletion timestamp only persists in the fake client with a finalizer.
		ag.Finalizers = []string{"actions-gateway/test"}
	}
	return ag
}

// The managed-gateways collector must report the count of non-deleting CRs.
func TestManagedGatewaysCollector(t *testing.T) {
	scheme := newIPRangeScheme(t)

	t.Run("none", func(t *testing.T) {
		fc := fake.NewClientBuilder().WithScheme(scheme).Build()
		c := newManagedGatewaysCollector(fc)
		assert.Equal(t, 0.0, testutil.ToFloat64(c))
	})

	t.Run("counts active, excludes deleting", func(t *testing.T) {
		fc := fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(
				managedGateway("a", false),
				managedGateway("b", false),
				managedGateway("c", true),
			).Build()
		c := newManagedGatewaysCollector(fc)
		assert.Equal(t, 2.0, testutil.ToFloat64(c),
			"two active gateways; the deleting one is excluded")
	})
}
