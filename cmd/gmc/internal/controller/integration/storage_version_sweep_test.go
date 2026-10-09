//go:build integration

package integration_test

import (
	"testing"
	"time"

	v2 "github.com/actions-gateway/github-actions-gateway/api/v2"
	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	v2beta1 "github.com/actions-gateway/github-actions-gateway/api/v2beta1"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/migrate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// These run gag-migrate's storage-version sweep (Q1086) against the real apiserver.
// The suite installs CRDs that already store v2, so each test first recreates the
// state every 1.x cluster is in: objects persisted at v2beta1 and v2beta1 listed in
// status.storedVersions.

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

func storedVersionsOf(t *testing.T, resource string) []string {
	t.Helper()
	stored, _, err := unstructured.NestedStringSlice(getCRD(t, resource).Object, "status", "storedVersions")
	require.NoError(t, err)
	return stored
}

// TestStorageVersionSweep stores a RunnerTemplate (converted by the GMC webhook) and
// a PriorityClassAllowlist (no webhook: the apiserver rewrites apiVersion) at
// v2beta1, advances storage to v2, and sweeps. A rewrite that re-encodes bumps the
// resourceVersion; the control object created after the advance is already stored
// at v2, so the same write is a no-op the apiserver skips.
func TestStorageVersionSweep(t *testing.T) {
	const ns = "storage-sweep"
	createNamespace(t, ns)
	for _, r := range []string{"runnertemplates", "priorityclassallowlists"} {
		t.Cleanup(func() { setStorageVersion(t, r, "v2") })
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

	reports, err := migrate.SweepStorageVersion(ctx, k8sClient, migrate.SweepOptions{Apply: true})
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

	var hub v2beta1.RunnerTemplate
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(oldRT), &hub))
	assert.Equal(t, "ghcr.io/example/runner:latest", hub.Spec.PodTemplate.Spec.Containers[0].Image, "v2beta1 still reads a v2-stored object")
}

// TestStorageVersionSweep_RefusesStoredAlias plants an EgressProxy naming a
// deprecated alias, which v2 storage holds as FQDN plus the conversion annotation,
// and confirms the sweep refuses before writing, then proceeds once the pool is
// migrated off the alias.
func TestStorageVersionSweep_RefusesStoredAlias(t *testing.T) {
	const ns = "storage-sweep-alias"
	createNamespace(t, ns)
	ep := &v2alpha1.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "pinned", Namespace: ns},
		Spec:       v2alpha1.EgressProxySpec{EgressPolicyMode: v2alpha1.EgressPolicyModeCiliumFQDN},
	}
	createStoredAliasProxy(t, ep)

	var hub v2beta1.EgressProxy
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(ep), &hub))
	require.Equal(t, v2beta1.EgressPolicyModeCiliumFQDN, hub.Spec.EgressPolicyMode, "v2 storage must keep the alias readable at v2beta1")
	var ga v2.EgressProxy
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(ep), &ga))
	assert.Equal(t, v2.EgressPolicyModeFQDN, ga.Spec.EgressPolicyMode)
	assert.Equal(t, "CiliumFQDN", ga.Annotations[egressPolicyModeAnnotation])

	_, err := migrate.SweepStorageVersion(ctx, k8sClient, migrate.SweepOptions{Apply: true})
	require.ErrorIs(t, err, migrate.ErrStoragePrecondition)
	assert.Contains(t, err.Error(), ns+"/pinned\tCiliumFQDN")

	hub.Spec.EgressPolicyMode = v2beta1.EgressPolicyModeFQDN
	require.NoError(t, k8sClient.Update(ctx, &hub))
	_, err = migrate.SweepStorageVersion(ctx, k8sClient, migrate.SweepOptions{Apply: true})
	require.NoError(t, err, "the sweep proceeds once no pool names an alias")
	assert.Equal(t, []string{"v2"}, storedVersionsOf(t, "egressproxies"))
}
