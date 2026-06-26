package apilabels_test

import (
	"testing"

	"github.com/actions-gateway/github-actions-gateway/api/apilabels"
)

func TestRecommended_OmitsEmptyVersion(t *testing.T) {
	l := apilabels.Recommended("actions-runner", "rg-1", "runner", "", "actions-gateway-controller")
	want := map[string]string{
		apilabels.Name:      "actions-runner",
		apilabels.Instance:  "rg-1",
		apilabels.Component: "runner",
		apilabels.PartOf:    apilabels.PartOfValue,
		apilabels.ManagedBy: "actions-gateway-controller",
	}
	for k, v := range want {
		if l[k] != v {
			t.Errorf("Recommended()[%q] = %q, want %q", k, l[k], v)
		}
	}
	if _, ok := l[apilabels.Version]; ok {
		t.Errorf("empty version must be omitted, got %q", l[apilabels.Version])
	}
}

func TestRecommended_IncludesVersionWhenSet(t *testing.T) {
	l := apilabels.Recommended("actions-runner", "rg-1", "runner", "2.335.1", "actions-gateway-controller")
	if l[apilabels.Version] != "2.335.1" {
		t.Errorf("version = %q, want 2.335.1", l[apilabels.Version])
	}
}

// TestMerge_PreservesFunctionalLabels is the load-bearing invariant: recommended
// metadata must never clobber an existing functional/selector label.
func TestMerge_PreservesFunctionalLabels(t *testing.T) {
	dst := map[string]string{
		"app":                       "actions-gateway-proxy",
		"actions-gateway/component": "workload",
		apilabels.Component:         "preexisting", // must win over the recommended value
	}
	apilabels.Merge(dst, "actions-gateway-proxy", "ep-1", "proxy", "", "actions-gateway-gmc")

	if dst["app"] != "actions-gateway-proxy" {
		t.Errorf("functional app label was clobbered: %q", dst["app"])
	}
	if dst["actions-gateway/component"] != "workload" {
		t.Errorf("functional component selector was clobbered: %q", dst["actions-gateway/component"])
	}
	if dst[apilabels.Component] != "preexisting" {
		t.Errorf("Merge overwrote an existing key: %q", dst[apilabels.Component])
	}
	if dst[apilabels.Name] != "actions-gateway-proxy" {
		t.Errorf("Merge did not add a missing recommended key: name=%q", dst[apilabels.Name])
	}
	if dst[apilabels.PartOf] != apilabels.PartOfValue {
		t.Errorf("Merge did not add part-of: %q", dst[apilabels.PartOf])
	}
}

func TestMerge_NilDst(t *testing.T) {
	l := apilabels.Merge(nil, "actions-runner", "rg-1", "runner", "2.335.1", "actions-gateway-controller")
	if l[apilabels.Name] != "actions-runner" || l[apilabels.Version] != "2.335.1" {
		t.Errorf("Merge(nil) did not allocate and populate: %+v", l)
	}
}
