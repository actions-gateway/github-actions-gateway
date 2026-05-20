/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"errors"
	"log/slog"
	"net"
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
