package controller

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func cmWithData(data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{Data: data}
}

func TestParsePriorityClassAllowlistConfigMap_CommaSeparated(t *testing.T) {
	cm := cmWithData(map[string]string{
		PriorityClassAllowlistConfigMapKey: "runner-standard, runner-bursty",
	})
	got, err := parsePriorityClassAllowlistConfigMap(cm)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := []string{"runner-bursty", "runner-standard"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParsePriorityClassAllowlistConfigMap_NewlineSeparated(t *testing.T) {
	cm := cmWithData(map[string]string{
		PriorityClassAllowlistConfigMapKey: "runner-standard\nrunner-bursty\n\nrunner-batch\n",
	})
	got, err := parsePriorityClassAllowlistConfigMap(cm)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := []string{"runner-batch", "runner-bursty", "runner-standard"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParsePriorityClassAllowlistConfigMap_Deduplicates(t *testing.T) {
	cm := cmWithData(map[string]string{
		PriorityClassAllowlistConfigMapKey: "runner-standard,runner-standard\nrunner-standard",
	})
	got, err := parsePriorityClassAllowlistConfigMap(cm)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := []string{"runner-standard"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParsePriorityClassAllowlistConfigMap_EmptyValueIsValid(t *testing.T) {
	cm := cmWithData(map[string]string{PriorityClassAllowlistConfigMapKey: "   \n  "})
	got, err := parsePriorityClassAllowlistConfigMap(cm)
	if err != nil {
		t.Fatalf("an empty value must be valid (no dynamic additions): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected no names, got %v", got)
	}
}

func TestParsePriorityClassAllowlistConfigMap_MissingKeyIsError(t *testing.T) {
	cm := cmWithData(map[string]string{"someOtherKey": "runner-standard"})
	if _, err := parsePriorityClassAllowlistConfigMap(cm); err == nil {
		t.Errorf("a ConfigMap missing the data key must be rejected (fail-safe)")
	}
}

func TestParsePriorityClassAllowlistConfigMap_InvalidNameRejectsWholeConfigMap(t *testing.T) {
	// A single malformed entry must fail the whole parse — the valid sibling must
	// NOT be partially applied, or a typo could smuggle a class in alongside junk.
	cm := cmWithData(map[string]string{
		PriorityClassAllowlistConfigMapKey: "runner-standard, Not A Valid Name!",
	})
	got, err := parsePriorityClassAllowlistConfigMap(cm)
	if err == nil {
		t.Fatalf("an invalid PriorityClass name must reject the whole ConfigMap, got %v", got)
	}
}

func TestParsePriorityClassAllowlistConfigMap_RejectsUppercase(t *testing.T) {
	cm := cmWithData(map[string]string{PriorityClassAllowlistConfigMapKey: "Runner-Standard"})
	if _, err := parsePriorityClassAllowlistConfigMap(cm); err == nil {
		t.Errorf("an uppercase name is not a valid DNS-1123 subdomain and must be rejected")
	}
}
