// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get

package controller

import (
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var crdGVK = schema.GroupVersionKind{
	Group:   "apiextensions.k8s.io",
	Version: "v1",
	Kind:    "CustomResourceDefinition",
}

// boundaryField names one CRD property whose absence from the *installed*
// schema widens tenant access. A structural schema prunes an undeclared field
// on write with no error and no warning, so a CRD left behind the binary keeps
// the field settable in a manifest and inert in the cluster.
//
// The exposure is largest on the v2 CRDs, which are applied out-of-band: they
// exceed Helm's 1 MiB release-Secret limit and ship in their own chart, so
// `helm upgrade` of the main chart never touches them.
//
// Only fields that fail OPEN belong here. spec.runnerGroup pruned to "" sends
// the tenant's scale set into GitHub's installation-default runner group
// (scalesetlistener resolveRunnerGroup), which every repository in the
// organisation can route jobs into — the boundary Q712 shipped, silently gone.
// A field whose absence disables a feature or narrows what is permitted costs a
// restart to notice and does not justify refusing to start.
type boundaryField struct {
	crd     string   // CustomResourceDefinition metadata.name
	version string   // the served API version whose schema must declare it
	path    []string // property path from that version's schema root
}

// boundaryFields is curated, not derived: which fields fail open is a property
// of the code that reads them, not of the schema. crd_schema_test.go asserts
// every entry against the CRDs this repo ships, so a rename cannot leave an
// entry that refuses to start against a correct cluster.
var boundaryFields = []boundaryField{
	{crd: "runnersets.actions-gateway.com", version: "v2beta1", path: []string{"spec", "runnerGroup"}},
	{crd: "runnersets.actions-gateway.com", version: "v2alpha1", path: []string{"spec", "runnerGroup"}},
	{crd: "actionsgateways.actions-gateway.com", version: "v2beta1", path: []string{"spec", "defaultRunnerGroup"}},
	{crd: "actionsgateways.actions-gateway.com", version: "v2alpha1", path: []string{"spec", "defaultRunnerGroup"}},
}

// VerifyCRDSchemas reports whether any installed CRD's schema predates this
// binary on a field that bounds tenant access. The GMC calls it once at startup
// and refuses to start on a finding: the failure it detects is silent by
// construction, and a controller that keeps provisioning tenants against an
// absent boundary widens the exposure it was asked to enforce.
//
// A CRD that is not installed at all is skipped — that is the supported v1-only
// install, which V2alpha1Installed already decides. Every other read error is
// returned rather than swallowed, so a missing RBAC grant or an apiserver
// hiccup fails closed instead of passing the check by accident.
func VerifyCRDSchemas(ctx context.Context, reader client.Reader) error {
	seen := map[string]*unstructured.Unstructured{}
	var stale []string
	for _, f := range boundaryFields {
		crd, ok := seen[f.crd]
		if !ok {
			var err error
			if crd, err = getCRD(ctx, reader, f.crd); err != nil {
				return err
			}
			seen[f.crd] = crd
		}
		if crd == nil {
			continue
		}
		if !versionDeclares(crd, f.version, f.path) {
			stale = append(stale, fmt.Sprintf("  - %s at %s does not declare %s",
				f.crd, f.version, strings.Join(f.path, ".")))
		}
	}
	if len(stale) == 0 {
		return nil
	}
	return fmt.Errorf("the CustomResourceDefinitions installed in this cluster are older than this GMC "+
		"and no longer declare a field that bounds tenant access:\n%s\n"+
		"A structural schema prunes an undeclared field on write with no error, so the field stays "+
		"settable in a manifest and does nothing in the cluster. The v2 CRDs are applied out-of-band and "+
		"`helm upgrade` never touches them, so re-apply them at the release this GMC ships in and restart:\n\n"+
		"    kubectl apply --server-side -f "+
		"https://github.com/actions-gateway/github-actions-gateway/releases/download/vX.Y.Z/"+
		"actions-gateway-crds-v2.yaml\n\n"+
		"Detail: docs/operations/upgrade.md, section \"the v2 API CRDs\"", strings.Join(stale, "\n"))
}

// getCRD reads one CustomResourceDefinition by name, returning (nil, nil) when
// the CRD — or the apiextensions API itself — is absent.
func getCRD(ctx context.Context, reader client.Reader, name string) (*unstructured.Unstructured, error) {
	crd := &unstructured.Unstructured{}
	crd.SetGroupVersionKind(crdGVK)
	if err := reader.Get(ctx, types.NamespacedName{Name: name}, crd); err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading CustomResourceDefinition %s: %w", name, err)
	}
	return crd, nil
}

// versionDeclares reports whether the named version of crd is served and
// declares the property at path. An absent or unserved version reports false:
// this binary serves it, so a CRD that does not is behind by a whole version.
func versionDeclares(crd *unstructured.Unstructured, version string, path []string) bool {
	versions, ok, err := unstructured.NestedSlice(crd.Object, "spec", "versions")
	if err != nil || !ok {
		return false
	}
	for _, v := range versions {
		ver, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if name, _, _ := unstructured.NestedString(ver, "name"); name != version {
			continue
		}
		if served, _, _ := unstructured.NestedBool(ver, "served"); !served {
			return false
		}
		root, ok, err := unstructured.NestedMap(ver, "schema", "openAPIV3Schema")
		if err != nil || !ok {
			return false
		}
		return schemaDeclares(root, path)
	}
	return false
}

// schemaDeclares walks an OpenAPI v3 schema down path, one `properties` hop per
// element, and reports whether the leaf is declared.
func schemaDeclares(node map[string]any, path []string) bool {
	for _, name := range path {
		props, ok, err := unstructured.NestedMap(node, "properties")
		if err != nil || !ok {
			return false
		}
		next, ok := props[name].(map[string]any)
		if !ok {
			return false
		}
		node = next
	}
	return true
}
