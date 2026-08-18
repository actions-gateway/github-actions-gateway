package controller

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

// crdReader serves CustomResourceDefinitions from a map keyed by name; an
// unknown name is NotFound, matching an apiserver where the CRD is not
// installed. Only Get is exercised.
type crdReader struct {
	client.Reader
	crds map[string]*unstructured.Unstructured
	err  error
}

func (r crdReader) Get(_ context.Context, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	if r.err != nil {
		return r.err
	}
	crd, ok := r.crds[key.Name]
	if !ok {
		return apierrors.NewNotFound(schema.GroupResource{
			Group: "apiextensions.k8s.io", Resource: "customresourcedefinitions"}, key.Name)
	}
	obj.(*unstructured.Unstructured).Object = crd.Object
	return nil
}

// loadShippedCRD reads the controller-gen CRD this repo ships for name, e.g.
// runnersets.actions-gateway.com -> api/config/crd/actions-gateway.com_runnersets.yaml.
// testdata/crd is an in-module symlink to that directory: the api module sits
// outside the cmd/gmc module root, and go drops reads that leave it from the
// test-cache key, so `make manifests` output would replay a cached green
// (testing.md § The out-of-module test read gate).
func loadShippedCRD(t *testing.T, name string) *unstructured.Unstructured {
	t.Helper()
	plural, group, ok := strings.Cut(name, ".")
	require.True(t, ok, "CRD name %q must be <plural>.<group>", name)
	data, err := os.ReadFile("testdata/crd/" + group + "_" + plural + ".yaml")
	require.NoError(t, err, "shipped CRD for %s must exist; run 'make manifests'", name)
	crd := &unstructured.Unstructured{}
	require.NoError(t, yaml.Unmarshal(data, &crd.Object))
	return crd
}

// TestBoundaryFields_DeclaredByShippedCRDs is the drift half of the check: every
// curated entry must be declared by the CRD this repo ships, or the startup
// probe refuses to start against a perfectly current cluster. A renamed or moved
// field fails here rather than in the field.
func TestBoundaryFields_DeclaredByShippedCRDs(t *testing.T) {
	require.NotEmpty(t, boundaryFields)
	for _, f := range boundaryFields {
		crd := loadShippedCRD(t, f.crd)
		require.True(t, versionDeclares(crd, f.version, f.path),
			"%s at %s must declare %s", f.crd, f.version, strings.Join(f.path, "."))
	}
}

func TestVerifyCRDSchemas_ShippedCRDsPass(t *testing.T) {
	crds := map[string]*unstructured.Unstructured{}
	for _, f := range boundaryFields {
		crds[f.crd] = loadShippedCRD(t, f.crd)
	}
	require.NoError(t, VerifyCRDSchemas(context.Background(), crdReader{crds: crds}))
}

// TestVerifyCRDSchemas_AbsentCRDIsNotStale covers the supported v1-only install:
// the v2 CRDs are simply not there, which V2alpha1Installed decides, not this.
func TestVerifyCRDSchemas_AbsentCRDIsNotStale(t *testing.T) {
	require.NoError(t, VerifyCRDSchemas(context.Background(), crdReader{}))
}

// TestVerifyCRDSchemas_PrunedFieldReported deletes spec.runnerGroup from the
// shipped RunnerSet schema — what an apiserver still serving the previous
// release's CRD looks like — and requires the probe to name it.
func TestVerifyCRDSchemas_PrunedFieldReported(t *testing.T) {
	crds := map[string]*unstructured.Unstructured{}
	for _, f := range boundaryFields {
		crds[f.crd] = loadShippedCRD(t, f.crd)
	}
	dropProperty(t, crds["runnersets.actions-gateway.com"], "v2beta1", "spec", "runnerGroup")

	err := VerifyCRDSchemas(context.Background(), crdReader{crds: crds})
	require.Error(t, err)
	require.Contains(t, err.Error(), "runnersets.actions-gateway.com at v2beta1 does not declare spec.runnerGroup")
	// The other declared entries stay quiet, so the message names the actual gap.
	require.NotContains(t, err.Error(), "defaultRunnerGroup")
	// The remedy travels with the finding: an operator reading only the log line
	// must be able to act on it.
	require.Contains(t, err.Error(), "kubectl apply --server-side")
}

// TestVerifyCRDSchemas_ReadErrorFailsClosed: a denied or failed read must not
// pass the check by looking like an absent CRD.
func TestVerifyCRDSchemas_ReadErrorFailsClosed(t *testing.T) {
	denied := apierrors.NewForbidden(schema.GroupResource{
		Group: "apiextensions.k8s.io", Resource: "customresourcedefinitions"}, "runnersets", nil)
	err := VerifyCRDSchemas(context.Background(), crdReader{err: denied})
	require.Error(t, err)
	require.Contains(t, err.Error(), "reading CustomResourceDefinition")
}

func TestVersionDeclares(t *testing.T) {
	crd := loadShippedCRD(t, "runnersets.actions-gateway.com")
	require.True(t, versionDeclares(crd, "v2beta1", []string{"spec", "runnerGroup"}))
	require.False(t, versionDeclares(crd, "v3", []string{"spec", "runnerGroup"}),
		"a version the CRD does not carry is behind, not declared")
	require.False(t, versionDeclares(crd, "v2beta1", []string{"spec", "noSuchField"}))

	unserved := crd.DeepCopy()
	setServed(t, unserved, "v2beta1", false)
	require.False(t, versionDeclares(unserved, "v2beta1", []string{"spec", "runnerGroup"}),
		"an unserved version cannot carry a field a tenant can set")
}

func TestSchemaDeclares(t *testing.T) {
	node := map[string]any{"properties": map[string]any{
		"spec": map[string]any{"properties": map[string]any{
			"runnerGroup": map[string]any{"type": "string"},
		}},
	}}
	require.True(t, schemaDeclares(node, []string{"spec"}))
	require.True(t, schemaDeclares(node, []string{"spec", "runnerGroup"}))
	require.False(t, schemaDeclares(node, []string{"status"}))
	require.False(t, schemaDeclares(node, []string{"spec", "absent"}))
	// A leaf has no properties of its own, so the walk stops rather than reporting
	// a deeper path as declared.
	require.False(t, schemaDeclares(node, []string{"spec", "runnerGroup", "deeper"}))
	require.False(t, schemaDeclares(map[string]any{}, []string{"spec"}))
}

// dropProperty removes one property from a CRD version's schema, reproducing a
// stored CRD that predates the field.
func dropProperty(t *testing.T, crd *unstructured.Unstructured, version string, path ...string) {
	t.Helper()
	versions, ok, err := unstructured.NestedSlice(crd.Object, "spec", "versions")
	require.NoError(t, err)
	require.True(t, ok)
	var dropped bool
	for _, v := range versions {
		ver := v.(map[string]any)
		if name, _, _ := unstructured.NestedString(ver, "name"); name != version {
			continue
		}
		node, ok, err := unstructured.NestedMap(ver, "schema", "openAPIV3Schema")
		require.NoError(t, err)
		require.True(t, ok)
		parent := node
		for _, name := range path[:len(path)-1] {
			parent = parent["properties"].(map[string]any)[name].(map[string]any)
		}
		props := parent["properties"].(map[string]any)
		require.Contains(t, props, path[len(path)-1])
		delete(props, path[len(path)-1])
		require.NoError(t, unstructured.SetNestedMap(ver, node, "schema", "openAPIV3Schema"))
		dropped = true
	}
	require.True(t, dropped, "version %s not found", version)
	require.NoError(t, unstructured.SetNestedSlice(crd.Object, versions, "spec", "versions"))
}

func setServed(t *testing.T, crd *unstructured.Unstructured, version string, served bool) {
	t.Helper()
	versions, ok, err := unstructured.NestedSlice(crd.Object, "spec", "versions")
	require.NoError(t, err)
	require.True(t, ok)
	for _, v := range versions {
		ver := v.(map[string]any)
		if name, _, _ := unstructured.NestedString(ver, "name"); name == version {
			ver["served"] = served
		}
	}
	require.NoError(t, unstructured.SetNestedSlice(crd.Object, versions, "spec", "versions"))
}
