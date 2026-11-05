//go:build integration

package integration_test

import (
	"slices"
	"testing"
	"time"

	v2 "github.com/actions-gateway/github-actions-gateway/api/v2"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/migrate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// These run gag-migrate's storage-version sweep (Q1086) against the real apiserver.
// The suite installs CRDs that serve v2 alone, so a test of a 1.x cluster first
// recreates that state on the CRD it needs: v2beta1 served again (serveV2beta1),
// objects persisted at it, and v2beta1 listed in status.storedVersions.

// storageVersionHash reads the discovery storageVersionHash the apiserver publishes
// for resource, which changes when the version it encodes to changes.
func storageVersionHash(t *testing.T, resource string) string {
	t.Helper()
	dc, err := discovery.NewDiscoveryClientForConfig(testEnv.Config)
	require.NoError(t, err)
	list, err := dc.ServerResourcesForGroupVersion("actions-gateway.com/v2")
	require.NoError(t, err)
	for _, r := range list.APIResources {
		if r.Name == resource {
			return r.StorageVersionHash
		}
	}
	t.Fatalf("%s is not served at actions-gateway.com/v2", resource)
	return ""
}

// setStorageVersion marks version the storage version of resource's CRD and waits
// until discovery reports the apiserver encoding to it. ServerResources and the
// handler that writes objects watch the same CRD, so a matching hash is a signal
// the switch has propagated, not a guarantee; the resourceVersion assertions in
// TestStorageVersionSweep are what fail if a create landed at the other version.
func setStorageVersion(t *testing.T, resource, version string) {
	t.Helper()
	before := storageVersionHash(t, resource)
	crd := getCRD(t, resource)
	versions, _, err := unstructured.NestedSlice(crd.Object, "spec", "versions")
	require.NoError(t, err)
	changed := false
	for _, raw := range versions {
		v := raw.(map[string]any)
		want := v["name"] == version
		changed = changed || v["storage"] != want
		v["storage"] = want
	}
	if !changed {
		return
	}
	require.NoError(t, unstructured.SetNestedSlice(crd.Object, versions, "spec", "versions"))
	require.NoError(t, k8sClient.Update(ctx, crd))
	require.Eventually(t, func() bool { return storageVersionHash(t, resource) != before },
		30*time.Second, 100*time.Millisecond, "discovery never reported %s storing %s", resource, version)
}

func getCRD(t *testing.T, resource string) *unstructured.Unstructured {
	t.Helper()
	crd := &unstructured.Unstructured{}
	crd.SetGroupVersionKind(schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"})
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKey{Name: resource + ".actions-gateway.com"}, crd))
	return crd
}

// serveV2beta1 adds v2beta1 to resource's CRD as a copy of v2 and takes it away again
// on cleanup, once the test's own objects are gone. The copy has no conversion webhook,
// so the apiserver converts by rewriting apiVersion; the sweep reads through
// unstructured objects and cannot tell. widen edits the copy's schema, for a value v2
// does not admit.
func serveV2beta1(t *testing.T, resource string, widen func(schema map[string]any)) {
	t.Helper()
	crd := getCRD(t, resource)
	versions, _, err := unstructured.NestedSlice(crd.Object, "spec", "versions")
	require.NoError(t, err)
	require.Len(t, versions, 1, "%s must serve v2 alone before the test adds v2beta1", resource)
	beta := runtime.DeepCopyJSONValue(versions[0]).(map[string]any)
	beta["name"], beta["storage"] = "v2beta1", false
	if widen != nil {
		widen(beta["schema"].(map[string]any)["openAPIV3Schema"].(map[string]any))
	}
	require.NoError(t, unstructured.SetNestedSlice(crd.Object, append(versions, beta), "spec", "versions"))
	require.NoError(t, k8sClient.Update(ctx, crd))

	dc, err := discovery.NewDiscoveryClientForConfig(testEnv.Config)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		list, err := dc.ServerResourcesForGroupVersion("actions-gateway.com/v2beta1")
		if err != nil {
			return false
		}
		return slices.ContainsFunc(list.APIResources, func(r metav1.APIResource) bool { return r.Name == resource })
	}, 30*time.Second, 100*time.Millisecond, "%s never served at v2beta1", resource)

	t.Cleanup(func() {
		setStorageVersion(t, resource, "v2")
		crd := getCRD(t, resource)
		require.NoError(t, unstructured.SetNestedStringSlice(crd.Object, []string{"v2"}, "status", "storedVersions"))
		require.NoError(t, k8sClient.Status().Update(ctx, crd))
		crd = getCRD(t, resource)
		versions, _, err := unstructured.NestedSlice(crd.Object, "spec", "versions")
		require.NoError(t, err)
		kept := slices.DeleteFunc(versions, func(v any) bool { return v.(map[string]any)["name"] == "v2beta1" })
		require.NoError(t, unstructured.SetNestedSlice(crd.Object, kept, "spec", "versions"))
		require.NoError(t, k8sClient.Update(ctx, crd))
	})
}

func storedVersionsOf(t *testing.T, resource string) []string {
	t.Helper()
	stored, _, err := unstructured.NestedStringSlice(getCRD(t, resource).Object, "status", "storedVersions")
	require.NoError(t, err)
	return stored
}

// TestStorageVersionSweep stores a RunnerTemplate and a PriorityClassAllowlist at
// v2beta1, advances storage to v2, and sweeps. A rewrite that re-encodes bumps the
// resourceVersion; the control object created after the advance is already stored
// at v2, so the same write is a no-op the apiserver skips.
func TestStorageVersionSweep(t *testing.T) {
	const ns = "storage-sweep"
	createNamespace(t, ns)
	for _, r := range []string{"runnertemplates", "priorityclassallowlists"} {
		serveV2beta1(t, r, nil)
	}

	rt := func(name string) *v2.RunnerTemplate {
		return &v2.RunnerTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: v2.RunnerTemplateSpec{PodTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "runner", Image: "ghcr.io/example/runner:latest"}},
			}}},
		}
	}
	oldRT, newRT := rt("stored-v2beta1"), rt("stored-v2")
	oldPCA := &v2.PriorityClassAllowlist{
		ObjectMeta: metav1.ObjectMeta{Name: "storage-sweep-v2beta1"},
		Spec:       v2.PriorityClassAllowlistSpec{AllowedPriorityClasses: []string{"tenant-high"}},
	}

	setStorageVersion(t, "runnertemplates", "v2beta1")
	setStorageVersion(t, "priorityclassallowlists", "v2beta1")
	for _, o := range []client.Object{oldRT, oldPCA} {
		require.NoError(t, k8sClient.Create(ctx, o))
		t.Cleanup(func() { _ = k8sClient.Delete(ctx, o) })
	}
	setStorageVersion(t, "runnertemplates", "v2")
	setStorageVersion(t, "priorityclassallowlists", "v2")
	require.NoError(t, k8sClient.Create(ctx, newRT))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, newRT) })

	for _, r := range []string{"runnertemplates", "priorityclassallowlists"} {
		require.ElementsMatch(t, []string{"v2", "v2beta1"}, storedVersionsOf(t, r),
			"%s must list v2beta1 before the sweep, as every 1.x cluster does", r)
	}
	rv := map[string]string{}
	for _, o := range []client.Object{oldRT, newRT, oldPCA} {
		require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(o), o))
		rv[o.GetName()] = o.GetResourceVersion()
	}

	// PageSize 1 puts the two RunnerTemplates on separate pages, so a sweep that
	// stopped at the first page would leave stored-v2beta1 unrewritten.
	reports, err := migrate.SweepStorageVersion(ctx, k8sClient, migrate.SweepOptions{Apply: true, PageSize: 1})
	require.NoError(t, err)

	for _, r := range []string{"runnertemplates", "priorityclassallowlists"} {
		assert.Equal(t, []string{"v2"}, storedVersionsOf(t, r), "%s storedVersions after the sweep", r)
	}
	for _, r := range reports {
		assert.True(t, r.Pruned(), "%s: storedVersions read back as %v", r.CRD, r.StoredAfter)
	}
	for _, o := range []client.Object{oldRT, newRT, oldPCA} {
		require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(o), o))
	}
	assert.NotEqual(t, rv[oldRT.Name], oldRT.ResourceVersion, "a RunnerTemplate stored at v2beta1 must be re-encoded")
	assert.NotEqual(t, rv[oldPCA.Name], oldPCA.ResourceVersion, "a PriorityClassAllowlist stored at v2beta1 must be re-encoded")
	assert.Equal(t, rv[newRT.Name], newRT.ResourceVersion, "a RunnerTemplate already stored at v2 is a no-op write")
}

// TestStorageVersionSweep_RefusesStoredAlias plants an EgressProxy naming a deprecated
// alias, widening the v2beta1 copy's enum as 1.10's v2beta1 declares it, and confirms
// the sweep refuses before writing, then proceeds once the pool is migrated off it.
func TestStorageVersionSweep_RefusesStoredAlias(t *testing.T) {
	const ns = "storage-sweep-alias"
	createNamespace(t, ns)
	serveV2beta1(t, "egressproxies", func(schema map[string]any) {
		mode := schema["properties"].(map[string]any)["spec"].(map[string]any)["properties"].(map[string]any)["egressPolicyMode"].(map[string]any)
		mode["enum"] = append(mode["enum"].([]any), "CiliumFQDN", "CalicoFQDN")
	})
	// A storage round trip lists v2beta1 in storedVersions with no object stored
	// there, so the prune after the alias clears has something to remove.
	setStorageVersion(t, "egressproxies", "v2beta1")
	setStorageVersion(t, "egressproxies", "v2")
	require.Contains(t, storedVersionsOf(t, "egressproxies"), "v2beta1")

	ep := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "actions-gateway.com/v2beta1",
		"kind":       "EgressProxy",
		"metadata":   map[string]any{"name": "pinned", "namespace": ns},
		"spec":       map[string]any{"egressPolicyMode": "CiliumFQDN"},
	}}
	require.Eventually(t, func() bool { return k8sClient.Create(ctx, ep) == nil },
		30*time.Second, 100*time.Millisecond, "plant an EgressProxy naming an alias at v2beta1")
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ep) })

	_, err := migrate.SweepStorageVersion(ctx, k8sClient, migrate.SweepOptions{Apply: true})
	require.ErrorIs(t, err, migrate.ErrStoragePrecondition)
	assert.Contains(t, err.Error(), ns+"/pinned\tCiliumFQDN")

	require.NoError(t, unstructured.SetNestedField(ep.Object, "FQDN", "spec", "egressPolicyMode"))
	require.NoError(t, k8sClient.Update(ctx, ep))
	_, err = migrate.SweepStorageVersion(ctx, k8sClient, migrate.SweepOptions{Apply: true})
	require.NoError(t, err, "the sweep proceeds once no pool names an alias")
	assert.Equal(t, []string{"v2"}, storedVersionsOf(t, "egressproxies"))
}

// TestStorageVersionSweep_V2OnlyCRDs runs the sweep against the CRD set this tree
// ships, which is the v2.0.0 candidate gate's storage leg: it applies these CRDs and
// then runs this sweep, on a cluster where v2beta1 is no longer served.
func TestStorageVersionSweep_V2OnlyCRDs(t *testing.T) {
	dc, err := discovery.NewDiscoveryClientForConfig(testEnv.Config)
	require.NoError(t, err)
	_, err = dc.ServerResourcesForGroupVersion("actions-gateway.com/v2beta1")
	require.True(t, apierrors.IsNotFound(err), "this test needs v2beta1 not served, got %v", err)

	reports, err := migrate.SweepStorageVersion(ctx, k8sClient, migrate.SweepOptions{Apply: true})
	require.NoError(t, err)
	for _, r := range reports {
		assert.True(t, r.Pruned(), "%s: storedVersions read back as %v", r.CRD, r.StoredAfter)
	}
}
