package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func cm(data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{Data: data}
}

func TestParseEgressDestinationAllowlistConfigMap_BothKeys(t *testing.T) {
	fqdns, cidrs, err := parseEgressDestinationAllowlistConfigMap(cm(map[string]string{
		EgressDestinationFQDNsConfigMapKey: "proxy.golang.org, sum.golang.org\n*.npmjs.org",
		EgressDestinationCIDRsConfigMapKey: "10.0.0.0/8\n172.16.0.0/12",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// FQDNs are normalized (the *.npmjs.org wildcard becomes npmjs.org) and sorted.
	want := []string{"npmjs.org", "proxy.golang.org", "sum.golang.org"}
	if len(fqdns) != len(want) {
		t.Fatalf("fqdns = %v, want %v", fqdns, want)
	}
	for i := range want {
		if fqdns[i] != want[i] {
			t.Errorf("fqdns[%d] = %q, want %q", i, fqdns[i], want[i])
		}
	}
	if len(cidrs) != 2 {
		t.Fatalf("cidrs = %v, want 2 entries", cidrs)
	}
}

func TestParseEgressDestinationAllowlistConfigMap_BothKeysOptional(t *testing.T) {
	// A ConfigMap with neither key is valid and yields empty lists — distinct from
	// the PriorityClass reconciler, which requires its single key.
	fqdns, cidrs, err := parseEgressDestinationAllowlistConfigMap(cm(map[string]string{}))
	if err != nil {
		t.Fatalf("a ConfigMap with neither key must be valid, got error: %v", err)
	}
	if len(fqdns) != 0 || len(cidrs) != 0 {
		t.Errorf("empty ConfigMap must yield empty lists, got fqdns=%v cidrs=%v", fqdns, cidrs)
	}

	// Only one key present is also fine.
	fqdns, cidrs, err = parseEgressDestinationAllowlistConfigMap(cm(map[string]string{
		EgressDestinationCIDRsConfigMapKey: "10.0.0.0/8",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fqdns) != 0 || len(cidrs) != 1 {
		t.Errorf("got fqdns=%v cidrs=%v, want 0 fqdns and 1 cidr", fqdns, cidrs)
	}
}

func TestParseEgressDestinationAllowlistConfigMap_RejectsMalformed(t *testing.T) {
	if _, _, err := parseEgressDestinationAllowlistConfigMap(cm(map[string]string{
		EgressDestinationCIDRsConfigMapKey: "10.0.0.0/8, not-a-cidr",
	})); err == nil {
		t.Error("a malformed CIDR entry must fail the whole parse (fail-safe)")
	}
	if _, _, err := parseEgressDestinationAllowlistConfigMap(cm(map[string]string{
		EgressDestinationFQDNsConfigMapKey: "ok.example.com, bad_underscore!host",
	})); err == nil {
		t.Error("a malformed FQDN entry must fail the whole parse (fail-safe)")
	}
}
