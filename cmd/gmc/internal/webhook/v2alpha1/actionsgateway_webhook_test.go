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

package v2alpha1

import (
	"context"
	"errors"
	"testing"

	agcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// fakeReader returns a client.Reader preloaded with the given objects, standing in
// for the manager's uncached API reader in the Q322 referrer-graph guards.
func fakeReader(t *testing.T, objs ...client.Object) client.Reader {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, agcv2alpha1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

// failingReader errors every read, for the fail-closed paths of the Q322 guards.
type failingReader struct{ client.Reader }

func (failingReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return errors.New("apiserver unavailable")
}

func (failingReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return errors.New("apiserver unavailable")
}

// v2Gateway builds a minimal v2 ActionsGateway bound to the given GitHub URL, with
// an optional defaultProxyRef.
func v2Gateway(namespace, name, gitHubURL, defaultProxy string) *agcv2alpha1.ActionsGateway {
	gw := &agcv2alpha1.ActionsGateway{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       agcv2alpha1.ActionsGatewaySpec{GitHubURL: gitHubURL},
	}
	if defaultProxy != "" {
		gw.Spec.DefaultProxyRef = &agcv2alpha1.LocalObjectRef{Name: defaultProxy}
	}
	return gw
}

// proxyWithNoProxy builds an EgressProxy carrying only noProxyCIDRs.
func proxyWithNoProxy(namespace, name string, entries ...string) *agcv2alpha1.EgressProxy {
	return &agcv2alpha1.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       agcv2alpha1.EgressProxySpec{NoProxyCIDRs: entries},
	}
}

// TestV2ActionsGatewayWebhook_RejectsGHESHostExcludedByDefaultProxy is the referrer
// side of the Q322 guard: creating (or re-pointing) a gateway whose gitHubURL host —
// here a GitHub Enterprise Server host — falls in its defaultProxyRef proxy's
// noProxyCIDRs must be rejected, or the pair would route GHES traffic around the
// per-tenant proxy.
func TestV2ActionsGatewayWebhook_RejectsGHESHostExcludedByDefaultProxy(t *testing.T) {
	ep := proxyWithNoProxy("team-a", "ep", "ghes.corp.example")
	v := &ActionsGatewayCustomValidator{reader: fakeReader(t, ep)}

	gw := v2Gateway("team-a", "gw", "https://ghes.corp.example/my-org", "ep")
	_, err := v.ValidateCreate(context.Background(), gw)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "spec.defaultProxyRef")
	assert.Contains(t, err.Error(), "ghes.corp.example")
	assert.Contains(t, err.Error(), "around the per-tenant egress proxy")

	// The same pair must be rejected when assembled by an update (defaultProxyRef
	// is mutable even though gitHubURL is not).
	_, err = v.ValidateUpdate(context.Background(), v2Gateway("team-a", "gw", "https://ghes.corp.example/my-org", ""), gw)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "spec.defaultProxyRef")
}

// TestV2ActionsGatewayWebhook_RejectsGHESHostExcludedByRunnerSetProxy closes the
// create-order gap: a RunnerSet + EgressProxy pair naming a not-yet-applied gateway
// admits unchecked on both sides (§H.7), so the arriving gateway is the first object
// that can see the conflict — its admission must check the proxies bound via its
// RunnerSets, not just its own defaultProxyRef.
func TestV2ActionsGatewayWebhook_RejectsGHESHostExcludedByRunnerSetProxy(t *testing.T) {
	ep := proxyWithNoProxy("team-a", "ep", ".corp.example")
	rs := classicRS("rs", "team-a", "gw", "linux")
	rs.Spec.ProxyRef = &agcv2alpha1.ObjectRef{Name: "ep"}
	v := &ActionsGatewayCustomValidator{reader: fakeReader(t, ep, rs)}

	_, err := v.ValidateCreate(context.Background(), v2Gateway("team-a", "gw", "https://ghes.corp.example/my-org", ""))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `RunnerSet "rs"`)
	assert.Contains(t, err.Error(), "around the per-tenant egress proxy")
}

// TestV2ActionsGatewayWebhook_AdmitsCompatibleProxyPairs asserts the guard is
// surgical: a missing proxy admits (referential integrity is a runtime condition,
// §H.7 — the proxy's own admission checks the pair when it arrives), as do proxies
// whose noProxyCIDRs are internal-only, gateways with no proxy bound at all, and a
// RunnerSet bound to a DIFFERENT gateway.
func TestV2ActionsGatewayWebhook_AdmitsCompatibleProxyPairs(t *testing.T) {
	ctx := context.Background()

	t.Run("missing proxy admits", func(t *testing.T) {
		v := &ActionsGatewayCustomValidator{reader: fakeReader(t)}
		_, err := v.ValidateCreate(ctx, v2Gateway("team-a", "gw", "https://ghes.corp.example/my-org", "ep"))
		require.NoError(t, err)
	})

	t.Run("internal-only noProxyCIDRs admit", func(t *testing.T) {
		ep := proxyWithNoProxy("team-a", "ep", "10.0.0.0/8", "svc.cluster.local", "internal.example.com")
		v := &ActionsGatewayCustomValidator{reader: fakeReader(t, ep)}
		_, err := v.ValidateCreate(ctx, v2Gateway("team-a", "gw", "https://ghes.corp.example/my-org", "ep"))
		require.NoError(t, err)
	})

	t.Run("no proxy bound admits", func(t *testing.T) {
		ep := proxyWithNoProxy("team-a", "ep", "ghes.corp.example")
		v := &ActionsGatewayCustomValidator{reader: fakeReader(t, ep)}
		_, err := v.ValidateCreate(ctx, v2Gateway("team-a", "gw", "https://ghes.corp.example/my-org", ""))
		require.NoError(t, err)
	})

	t.Run("RunnerSet under another gateway does not bind this host", func(t *testing.T) {
		ep := proxyWithNoProxy("team-a", "ep", "ghes.corp.example")
		rs := classicRS("rs", "team-a", "other-gw", "linux")
		rs.Spec.ProxyRef = &agcv2alpha1.ObjectRef{Name: "ep"}
		v := &ActionsGatewayCustomValidator{reader: fakeReader(t, ep, rs)}
		_, err := v.ValidateCreate(ctx, v2Gateway("team-a", "gw", "https://ghes.corp.example/my-org", ""))
		require.NoError(t, err)
	})

	t.Run("nil reader skips the check", func(t *testing.T) {
		v := &ActionsGatewayCustomValidator{}
		_, err := v.ValidateCreate(ctx, v2Gateway("team-a", "gw", "https://ghes.corp.example/my-org", "ep"))
		require.NoError(t, err)
	})
}

// TestV2ActionsGatewayWebhook_FailsClosedOnReadError asserts an unverifiable
// gitHubURL/noProxyCIDRs pair is rejected, not admitted on faith.
func TestV2ActionsGatewayWebhook_FailsClosedOnReadError(t *testing.T) {
	v := &ActionsGatewayCustomValidator{reader: failingReader{}}
	_, err := v.ValidateCreate(context.Background(), v2Gateway("team-a", "gw", "https://ghes.corp.example/my-org", "ep"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot verify")
}
