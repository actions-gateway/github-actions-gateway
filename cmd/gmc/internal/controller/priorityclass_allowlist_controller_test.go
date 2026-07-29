package controller

import (
	"reflect"
	"testing"

	v2beta1 "github.com/actions-gateway/github-actions-gateway/api/v2beta1"
)

func pcaWith(names ...string) *v2beta1.PriorityClassAllowlist {
	return &v2beta1.PriorityClassAllowlist{
		Spec: v2beta1.PriorityClassAllowlistSpec{AllowedPriorityClasses: names},
	}
}

func TestParsePriorityClassAllowlist_SortsForStableLogging(t *testing.T) {
	got, err := parsePriorityClassAllowlist(pcaWith("runner-standard", "runner-bursty"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := []string{"runner-bursty", "runner-standard"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParsePriorityClassAllowlist_Deduplicates(t *testing.T) {
	// The CRD marks the list x-kubernetes-list-type: set, so the apiserver rejects
	// duplicates on write. This is the defence-in-depth path for an object stored
	// before that marker existed, or written through a path that skipped validation.
	got, err := parsePriorityClassAllowlist(pcaWith("runner-standard", "runner-standard"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := []string{"runner-standard"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParsePriorityClassAllowlist_EmptyListIsValid(t *testing.T) {
	got, err := parsePriorityClassAllowlist(pcaWith())
	if err != nil {
		t.Fatalf("an empty list must be valid (no dynamic additions): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected no names, got %v", got)
	}
}

func TestParsePriorityClassAllowlist_BlankEntriesSkipped(t *testing.T) {
	got, err := parsePriorityClassAllowlist(pcaWith("runner-standard", "   ", ""))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := []string{"runner-standard"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParsePriorityClassAllowlist_InvalidNameRejectsWholeObject(t *testing.T) {
	// A single malformed entry must fail the whole parse — the valid sibling must
	// NOT be partially applied, or a typo could smuggle a class in alongside junk.
	got, err := parsePriorityClassAllowlist(pcaWith("runner-standard", "Not A Valid Name!"))
	if err == nil {
		t.Fatalf("an invalid PriorityClass name must reject the whole object, got %v", got)
	}
}

func TestParsePriorityClassAllowlist_RejectsUppercase(t *testing.T) {
	if _, err := parsePriorityClassAllowlist(pcaWith("Runner-Standard")); err == nil {
		t.Errorf("an uppercase name is not a valid DNS-1123 subdomain and must be rejected")
	}
}
