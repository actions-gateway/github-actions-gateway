package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/actions-gateway/github-actions-gateway/gmc/internal/allowlist"
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

// TestEgressDestinationAllowlistReconcile_SignalsChanged covers the wake the
// EgressProxy reconciler depends on (Q1322): every outcome that replaces the dynamic
// set sends one event, after the replacement, and a second replacement with the first
// event still buffered neither blocks nor is lost to the reader.
func TestEgressDestinationAllowlistReconcile_SignalsChanged(t *testing.T) {
	const ns, name = "gmc-system", "egress-allowlist"
	configMap := func(fqdns string) *corev1.ConfigMap {
		return &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
			Data:       map[string]string{EgressDestinationFQDNsConfigMapKey: fqdns},
		}
	}
	cases := []struct {
		name       string
		objs       []client.Object
		wantCovers bool
	}{
		{"applied", []client.Object{configMap("ghes.corp.example")}, true},
		{"absent", nil, false},
		{"malformed", []client.Object{configMap("not a host!")}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			al := allowlist.NewEgressDestination(nil, nil)
			al.AwaitDynamic()
			changed := make(chan event.TypedGenericEvent[struct{}], 1)
			r := &EgressDestinationAllowlistReconciler{
				Client:        fake.NewClientBuilder().WithScheme(egressProxyTestScheme(t)).WithObjects(tc.objs...).Build(),
				ConfigMapName: name,
				Namespace:     ns,
				Allowlist:     al,
				Changed:       changed,
			}
			for range 2 {
				_, err := r.Reconcile(context.Background(), ctrl.Request{})
				require.NoError(t, err)
			}
			require.Len(t, changed, 1, "one buffered wake covers both replacements")
			assert.False(t, al.DynamicPending(), "the event is sent after the set it announces is in place")
			assert.Equal(t, tc.wantCovers, al.CoversFQDN("ghes.corp.example"))
		})
	}

	t.Run("no channel wired", func(t *testing.T) {
		r := &EgressDestinationAllowlistReconciler{
			Client:        fake.NewClientBuilder().WithScheme(egressProxyTestScheme(t)).Build(),
			ConfigMapName: name,
			Namespace:     ns,
			Allowlist:     allowlist.NewEgressDestination(nil, nil),
		}
		_, err := r.Reconcile(context.Background(), ctrl.Request{})
		require.NoError(t, err)
	})
}
