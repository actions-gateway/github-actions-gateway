//go:build integration

package integration_test

import (
	"fmt"
	"testing"
	"time"

	v2beta1 "github.com/actions-gateway/github-actions-gateway/api/v2beta1"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/controller"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// TestCRDSchemaStale_PrunedBoundaryFieldIsDetected is the acceptance test for
// Q852, and measures the claim behind it against a real apiserver rather than
// asserting it: with the RunnerSet CRD aged by one field, a RunnerSet declaring
// spec.runnerGroup is ACCEPTED and comes back with the field gone. Nothing
// rejects it, nothing warns, and the tenant boundary Q712 shipped is inert —
// scalesetlistener resolveRunnerGroup reads "" as GitHub's installation-default
// runner group, which every repository in the organisation can route into.
//
// The v2 CRDs are the exposed ones because they are applied out-of-band (they
// exceed Helm's 1 MiB release-Secret limit), so `helm upgrade` never carries a
// schema change into them and a skipped apply step leaves them behind.
//
// It runs against its own envtest because it mutates a CRD the shared suite's
// other tests depend on, and writes at v2beta1 — the storage version, so the
// apiserver never calls the conversion webhook this env has no server for.
func TestCRDSchemaStale_PrunedBoundaryFieldIsDetected(t *testing.T) {
	env := &envtest.Environment{
		CRDDirectoryPaths: []string{
			"../../../config/crd/bases",
			"testdata/agc-crd",
			"testdata/crd",
		},
		ErrorIfCRDPathMissing: true,
		Scheme:                testScheme,
	}
	cfg, err := env.Start()
	require.NoError(t, err)
	t.Cleanup(func() { _ = env.Stop() })

	c, err := client.New(cfg, client.Options{Scheme: testScheme})
	require.NoError(t, err)

	const ns = "crd-schema-stale"
	require.NoError(t, c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))

	// Control, against the CRDs this repo ships: the probe passes and the field
	// survives the round trip.
	require.NoError(t, controller.VerifyCRDSchemas(ctx, c))

	require.NoError(t, c.Create(ctx, newBoundRunnerSet("current", ns)))

	var got v2beta1.RunnerSet
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "current"}, &got))
	require.Equal(t, "restricted", got.Spec.RunnerGroup,
		"the shipped CRD must round-trip spec.runnerGroup, or the pruning below proves nothing")

	// Age the stored CRD by exactly one field — what an apiserver still serving
	// the previous release's schema looks like after a skipped apply step.
	dropCRDProperty(t, c, "runnersets.actions-gateway.com", "spec", "runnerGroup")

	// The apiserver reloads a changed CRD schema asynchronously, so retry with a
	// fresh name until one create lands against the aged schema. A create that is
	// REJECTED would be the loud failure mode this test exists to rule out, so it
	// is logged rather than swallowed.
	var attempt int
	require.Eventually(t, func() bool {
		rs := newBoundRunnerSet(fmt.Sprintf("stale-%d", attempt), ns)
		attempt++
		if err := c.Create(ctx, rs); err != nil {
			t.Logf("create %s: %v", rs.Name, err)
			return false
		}
		var stored v2beta1.RunnerSet
		if err := c.Get(ctx, client.ObjectKeyFromObject(rs), &stored); err != nil {
			return false
		}
		return stored.Spec.RunnerGroup == ""
	}, 30*time.Second, 250*time.Millisecond,
		"a RunnerSet declaring spec.runnerGroup against the aged CRD must be accepted with the field pruned")

	// And the startup probe names it, with the remedy, so the operator is not left
	// to infer any of the above from an inert boundary.
	err = controller.VerifyCRDSchemas(ctx, c)
	require.Error(t, err)
	require.Contains(t, err.Error(), "runnersets.actions-gateway.com at v2beta1 does not declare spec.runnerGroup")
	require.Contains(t, err.Error(), "kubectl apply --server-side")
}

// newBoundRunnerSet builds a v2beta1 RunnerSet bound to a named GitHub runner
// group — the tenant boundary whose survival this test is about.
func newBoundRunnerSet(name, ns string) *v2beta1.RunnerSet {
	return &v2beta1.RunnerSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: v2beta1.RunnerSetSpec{
			GatewayRef:   v2beta1.ObjectRef{Name: "gw"},
			RunnerLabels: []string{"linux"},
			RunnerGroup:  "restricted",
		},
	}
}

// dropCRDProperty removes one property from every version of a CRD's schema,
// reproducing a stored CRD that predates the field.
func dropCRDProperty(t *testing.T, c client.Client, name string, path ...string) {
	t.Helper()
	crd := &unstructured.Unstructured{}
	crd.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"})
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: name}, crd))

	versions, ok, err := unstructured.NestedSlice(crd.Object, "spec", "versions")
	require.NoError(t, err)
	require.True(t, ok)

	var dropped int
	for _, v := range versions {
		ver := v.(map[string]any)
		node, ok, err := unstructured.NestedMap(ver, "schema", "openAPIV3Schema")
		require.NoError(t, err)
		require.True(t, ok)
		parent := node
		for _, segment := range path[:len(path)-1] {
			parent = parent["properties"].(map[string]any)[segment].(map[string]any)
		}
		props := parent["properties"].(map[string]any)
		leaf := path[len(path)-1]
		require.Contains(t, props, leaf, "%s must declare the property before it is dropped", name)
		delete(props, leaf)
		require.NoError(t, unstructured.SetNestedMap(ver, node, "schema", "openAPIV3Schema"))
		dropped++
	}
	require.NotZero(t, dropped)
	require.NoError(t, unstructured.SetNestedSlice(crd.Object, versions, "spec", "versions"))
	require.NoError(t, c.Update(ctx, crd))
}
