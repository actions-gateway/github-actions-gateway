package main

import (
	"sort"
	"testing"
)

// crdWith renders a one-version CRD carrying the given spec properties, in the
// shape controller-gen emits.
const crdBefore = `
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: runnersets.actions-gateway.com
spec:
  names:
    kind: RunnerSet
  versions:
    - name: v2beta1
      schema:
        openAPIV3Schema:
          properties:
            spec:
              properties:
                minRunners: {type: integer}
                capacityGate:
                  properties:
                    mode: {type: string}
                templates:
                  items:
                    properties:
                      image: {type: string}
                  type: array
`

const crdAfter = `
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: runnersets.actions-gateway.com
spec:
  names:
    kind: RunnerSet
  versions:
    - name: v2beta1
      schema:
        openAPIV3Schema:
          properties:
            spec:
              properties:
                minRunners: {type: integer}
                capacityGate:
                  properties:
                    method: {type: string}
                templates:
                  items:
                    properties:
                      image: {type: string}
                      digest: {type: string}
                  type: array
`

func propsOf(t *testing.T, blob string) map[string]bool {
	t.Helper()
	props := map[string]bool{}
	parsed, err := propertiesFromYAML([]byte(blob), props)
	if err != nil {
		t.Fatalf("propertiesFromYAML: %v", err)
	}
	if !parsed {
		t.Fatal("no CustomResourceDefinition parsed")
	}
	return props
}

func TestPropertiesFromYAML(t *testing.T) {
	props := propsOf(t, crdBefore)
	want := []string{
		"RunnerSet.v2beta1.spec",
		"RunnerSet.v2beta1.spec.capacityGate",
		"RunnerSet.v2beta1.spec.capacityGate.mode",
		"RunnerSet.v2beta1.spec.minRunners",
		"RunnerSet.v2beta1.spec.templates",
		// An array element's fields hang off the array's own path: the index is
		// not part of the contract.
		"RunnerSet.v2beta1.spec.templates.image",
	}
	var got []string
	for p := range props {
		got = append(got, p)
	}
	sort.Strings(got)
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestPropertiesFromYAMLIgnoresNonCRDDocuments(t *testing.T) {
	props := map[string]bool{}
	parsed, err := propertiesFromYAML([]byte("apiVersion: v1\nkind: Kustomization\n"), props)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if parsed {
		t.Error("a Kustomization must not count as a CRD schema read")
	}
	if len(props) != 0 {
		t.Errorf("props = %v, want none", props)
	}
}

// TestCRDSurfaceRenameShowsAsRemoval is the direction the repo's own history
// has never exercised: every breaking marker to date changed surface the FROM
// tag never published, so nothing was ever removed from it. A rename of a
// *published* field is the case that must read as a removal.
func TestCRDSurfaceRenameShowsAsRemoval(t *testing.T) {
	before, after := propsOf(t, crdBefore), propsOf(t, crdAfter)

	var removed, added []string
	for p := range before {
		if !after[p] {
			removed = append(removed, p)
		}
	}
	for p := range after {
		if !before[p] {
			added = append(added, p)
		}
	}
	sort.Strings(removed)
	sort.Strings(added)

	if len(removed) != 1 || removed[0] != "RunnerSet.v2beta1.spec.capacityGate.mode" {
		t.Errorf("removed = %v, want the renamed-away field", removed)
	}
	if len(added) != 2 {
		t.Errorf("added = %v, want the new name and the new array field", added)
	}
}

// A window that adds only is the shape every release to date has had, and must
// report no removals.
func TestCRDSurfaceAdditiveWindowRemovesNothing(t *testing.T) {
	before := propsOf(t, crdBefore)
	after := propsOf(t, crdBefore)
	after["RunnerSet.v2beta1.spec.newField"] = true

	for p := range before {
		if !after[p] {
			t.Errorf("%q went missing from a purely additive window", p)
		}
	}
}
