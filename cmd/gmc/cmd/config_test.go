package main

import (
	"flag"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/cache"

	v2beta1 "github.com/actions-gateway/github-actions-gateway/api/v2beta1"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/controller"
)

func fakeGetenv(env map[string]string) func(string) string {
	return func(k string) string { return env[k] }
}

// TestAddFlags verifies the flag surface binds with the documented defaults and
// parses overrides, so the ~25 declarations extracted from main() stay
// test-reachable (F8 / Q367).
func TestAddFlags(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		fs := flag.NewFlagSet("gmc", flag.ContinueOnError)
		f := addFlags(fs)
		if err := fs.Parse(nil); err != nil {
			t.Fatalf("parse: %v", err)
		}
		if f.metricsAddr != "0" {
			t.Errorf("metricsAddr default = %q, want \"0\"", f.metricsAddr)
		}
		if f.probeAddr != ":8081" {
			t.Errorf("probeAddr default = %q, want \":8081\"", f.probeAddr)
		}
		if !f.secureMetrics {
			t.Error("secureMetrics must default to true (secure by default)")
		}
		if f.enableHTTP2 {
			t.Error("enableHTTP2 must default to false (CVE mitigation)")
		}
		if f.enableLeaderElection {
			t.Error("enableLeaderElection must default to false")
		}
		if !f.leaderElectReleaseOnCancel {
			t.Error("leaderElectReleaseOnCancel must default to true")
		}
		if f.leaderElectLeaseDuration != 15*time.Second ||
			f.leaderElectRenewDeadline != 10*time.Second ||
			f.leaderElectRetryPeriod != 2*time.Second {
			t.Errorf("leader-election defaults = %s/%s/%s, want 15s/10s/2s",
				f.leaderElectLeaseDuration, f.leaderElectRenewDeadline, f.leaderElectRetryPeriod)
		}
		if f.fqdnPolicyBackend != string(controller.FQDNBackendNone) {
			t.Errorf("fqdnPolicyBackend default = %q, want %q", f.fqdnPolicyBackend, controller.FQDNBackendNone)
		}
		if f.webhookCertName != "tls.crt" || f.webhookCertKey != "tls.key" {
			t.Errorf("webhook cert name/key defaults = %q/%q, want tls.crt/tls.key",
				f.webhookCertName, f.webhookCertKey)
		}
		if f.allowFloatingImageTags || f.allowAgcExtraEnv || f.enableTenantServiceMonitors {
			t.Error("dev/test opt-in flags must all default to false")
		}
	})

	t.Run("overrides", func(t *testing.T) {
		fs := flag.NewFlagSet("gmc", flag.ContinueOnError)
		f := addFlags(fs)
		err := fs.Parse([]string{
			"--metrics-secure=false",
			"--enable-http2=true",
			"--leader-elect=true",
			"--allowed-priority-classes=high",
			"--fqdn-policy-backend=cilium",
			"--allow-floating-image-tags=true",
		})
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if f.secureMetrics {
			t.Error("--metrics-secure=false must set secureMetrics false")
		}
		if !f.enableHTTP2 {
			t.Error("--enable-http2=true must set enableHTTP2 true")
		}
		if !f.enableLeaderElection {
			t.Error("--leader-elect=true must set enableLeaderElection true")
		}
		if f.allowedPriorityClasses != "high" {
			t.Errorf("allowedPriorityClasses = %q, want \"high\"", f.allowedPriorityClasses)
		}
		if f.fqdnPolicyBackend != "cilium" {
			t.Errorf("fqdnPolicyBackend = %q, want \"cilium\"", f.fqdnPolicyBackend)
		}
		if !f.allowFloatingImageTags {
			t.Error("--allow-floating-image-tags=true must set the flag")
		}
	})
}

// TestResolveConfig covers the cross-flag validation resolveConfig centralizes:
// the happy path, allowlist disjointness, malformed CIDR/FQDN inputs, and the
// POD_NAMESPACE requirement for a ConfigMap watch.
func TestResolveConfig(t *testing.T) {
	t.Run("happy path with disjoint allowlists", func(t *testing.T) {
		cfg := &gmcFlags{
			allowedPriorityClasses:      "worker-high",
			allowedInfraPriorityClasses: "infra-high",
			fqdnPolicyBackend:           string(controller.FQDNBackendNone),
		}
		rc, err := resolveConfig(cfg, fakeGetenv(map[string]string{"POD_NAMESPACE": "gmc-system"}))
		if err != nil {
			t.Fatalf("resolveConfig: %v", err)
		}
		if rc.priorityClassAllowlist == nil || rc.infraPriorityClassAllowlist == nil ||
			rc.egressDestinationAllowlist == nil {
			t.Error("allowlists must be constructed")
		}
		if rc.fqdnBackend != controller.FQDNBackendNone {
			t.Errorf("fqdnBackend = %q, want none", rc.fqdnBackend)
		}
		if rc.podNamespace != "gmc-system" {
			t.Errorf("podNamespace = %q, want gmc-system", rc.podNamespace)
		}
		if rc.cacheOptions.ByObject != nil {
			t.Error("no ConfigMap watch configured, so cache ByObject must be nil")
		}
	})

	t.Run("intersecting priority-class allowlists are rejected", func(t *testing.T) {
		cfg := &gmcFlags{
			allowedPriorityClasses:      "shared,worker",
			allowedInfraPriorityClasses: "shared,infra",
			fqdnPolicyBackend:           string(controller.FQDNBackendNone),
		}
		if _, err := resolveConfig(cfg, fakeGetenv(nil)); err == nil {
			t.Error("a class on both allowlists must fail startup (Q284 priority-inversion guard)")
		}
	})

	t.Run("malformed egress CIDR fails closed", func(t *testing.T) {
		cfg := &gmcFlags{
			allowedEgressCIDRs: "not-a-cidr",
			fqdnPolicyBackend:  string(controller.FQDNBackendNone),
		}
		if _, err := resolveConfig(cfg, fakeGetenv(nil)); err == nil {
			t.Error("a malformed --allowed-egress-cidrs must fail startup")
		}
	})

	t.Run("unrecognized fqdn backend fails closed", func(t *testing.T) {
		cfg := &gmcFlags{fqdnPolicyBackend: "bogus"}
		if _, err := resolveConfig(cfg, fakeGetenv(nil)); err == nil {
			t.Error("an unrecognized --fqdn-policy-backend must fail startup")
		}
	})

	t.Run("ConfigMap watch without POD_NAMESPACE fails closed", func(t *testing.T) {
		cfg := &gmcFlags{
			egressDestinationAllowlistConfigMap: "egress-allowlist",
			fqdnPolicyBackend:                   string(controller.FQDNBackendNone),
		}
		if _, err := resolveConfig(cfg, fakeGetenv(nil)); err == nil {
			t.Error("a ConfigMap watch with no POD_NAMESPACE must fail startup")
		}
	})

	// The PriorityClassAllowlist watch reads a CLUSTER-scoped CR (Q492), so unlike
	// the ConfigMap watch above it needs no POD_NAMESPACE to locate its object.
	t.Run("PriorityClassAllowlist watch without POD_NAMESPACE starts", func(t *testing.T) {
		cfg := &gmcFlags{
			priorityClassAllowlistName: "gag-priorityclass-allowlist",
			fqdnPolicyBackend:          string(controller.FQDNBackendNone),
		}
		if _, err := resolveConfig(cfg, fakeGetenv(nil)); err != nil {
			t.Errorf("a cluster-scoped allowlist watch must not require POD_NAMESPACE: %v", err)
		}
	})
}

// TestBuildCacheOptions verifies that each watched-allowlist feature narrows its
// informer to the single object it reads, and that neither feature grants a broad
// read: no watch → no ByObject; the egress ConfigMap watch is scoped to
// POD_NAMESPACE and pinned by name; the cluster-scoped PriorityClassAllowlist
// watch is pinned by name with no namespace (Q492).
func TestBuildCacheOptions(t *testing.T) {
	t.Run("no watch yields empty options", func(t *testing.T) {
		opts, err := buildCacheOptions(&gmcFlags{}, "gmc-system")
		if err != nil {
			t.Fatalf("buildCacheOptions: %v", err)
		}
		if opts.ByObject != nil {
			t.Error("no watch configured, ByObject must be nil")
		}
	})

	t.Run("egress ConfigMap watch is namespace-scoped and name-pinned", func(t *testing.T) {
		opts, err := buildCacheOptions(&gmcFlags{egressDestinationAllowlistConfigMap: "egress"}, "gmc-system")
		if err != nil {
			t.Fatalf("buildCacheOptions: %v", err)
		}
		// ByObject is keyed by a freshly allocated pointer, so it can only be
		// looked up by type, never by an equal key.
		var byObj cache.ByObject
		var ok bool
		for k, v := range opts.ByObject {
			if _, isCM := k.(*corev1.ConfigMap); isCM {
				byObj, ok = v, true
			}
		}
		if !ok {
			t.Fatal("the egress watch must scope the ConfigMap informer")
		}
		nsCfg, inNS := byObj.Namespaces["gmc-system"]
		if !inNS {
			t.Fatal("informer must be scoped to POD_NAMESPACE")
		}
		if nsCfg.FieldSelector == nil {
			t.Error("the watch must pin the informer to the ConfigMap by name")
		}
	})

	t.Run("PriorityClassAllowlist watch is name-pinned and cluster-scoped", func(t *testing.T) {
		opts, err := buildCacheOptions(&gmcFlags{priorityClassAllowlistName: "pc"}, "gmc-system")
		if err != nil {
			t.Fatalf("buildCacheOptions: %v", err)
		}
		var found bool
		for k, v := range opts.ByObject {
			if _, isPCA := k.(*v2beta1.PriorityClassAllowlist); !isPCA {
				continue
			}
			found = true
			if v.Field == nil {
				t.Error("the watch must pin the informer to the object by name")
			}
			if len(v.Namespaces) != 0 {
				t.Error("a cluster-scoped kind must not be namespace-scoped")
			}
		}
		if !found {
			t.Fatal("the watch must scope the PriorityClassAllowlist informer")
		}
	})

	t.Run("both watches scope independently", func(t *testing.T) {
		opts, err := buildCacheOptions(&gmcFlags{
			priorityClassAllowlistName:          "pc",
			egressDestinationAllowlistConfigMap: "egress",
		}, "gmc-system")
		if err != nil {
			t.Fatalf("buildCacheOptions: %v", err)
		}
		if len(opts.ByObject) != 2 {
			t.Errorf("both watches must scope their own informer; got %d entries", len(opts.ByObject))
		}
	})
}

// TestResolveImages covers required-env enforcement, digest pinning (and its
// dev/test opt-out), and AGC_EXTRA_*/WRAPPER_IMAGE forwarding.
func TestResolveImages(t *testing.T) {
	const digest = "@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	noEnviron := func() []string { return nil }

	t.Run("missing AGC_IMAGE is an error", func(t *testing.T) {
		_, err := resolveImages(&gmcFlags{}, fakeGetenv(nil), noEnviron)
		if err == nil {
			t.Error("resolveImages must require AGC_IMAGE")
		}
	})

	t.Run("floating tag rejected without the opt-out", func(t *testing.T) {
		env := map[string]string{
			"AGC_IMAGE":   "ghcr.io/org/agc:v1",
			"PROXY_IMAGE": "ghcr.io/org/proxy:v1",
		}
		if _, err := resolveImages(&gmcFlags{}, fakeGetenv(env), noEnviron); err == nil {
			t.Error("a floating (non-digest) tag must be rejected by default")
		}
	})

	t.Run("floating tag allowed with the opt-out", func(t *testing.T) {
		env := map[string]string{
			"AGC_IMAGE":   "ghcr.io/org/agc:v1",
			"PROXY_IMAGE": "ghcr.io/org/proxy:v1",
		}
		if _, err := resolveImages(&gmcFlags{allowFloatingImageTags: true}, fakeGetenv(env), noEnviron); err != nil {
			t.Errorf("--allow-floating-image-tags must permit floating tags: %v", err)
		}
	})

	t.Run("digest-pinned images with AGC_EXTRA and wrapper", func(t *testing.T) {
		env := map[string]string{
			"AGC_IMAGE":        "ghcr.io/org/agc" + digest,
			"PROXY_IMAGE":      "ghcr.io/org/proxy" + digest,
			"WRAPPER_IMAGE":    "ghcr.io/org/wrapper" + digest,
			"WRAPPER_DELIVERY": "imagevolume",
		}
		environ := func() []string {
			return []string{"AGC_EXTRA_FOO=bar", "UNRELATED=x", "AGC_EXTRA_BAZ=qux"}
		}
		img, err := resolveImages(&gmcFlags{allowAgcExtraEnv: true}, fakeGetenv(env), environ)
		if err != nil {
			t.Fatalf("resolveImages: %v", err)
		}
		if img.agcImage != env["AGC_IMAGE"] || img.proxyImage != env["PROXY_IMAGE"] {
			t.Errorf("image passthrough mismatch: %+v", img)
		}
		got := map[string]string{}
		for _, e := range img.agcExtraEnv {
			got[e.Name] = e.Value
		}
		if got["FOO"] != "bar" || got["BAZ"] != "qux" {
			t.Errorf("AGC_EXTRA_* not forwarded (prefix-stripped): %v", got)
		}
		if _, unrelated := got["UNRELATED"]; unrelated {
			t.Error("non-AGC_EXTRA_ env must not be forwarded")
		}
		if got["WRAPPER_IMAGE"] != env["WRAPPER_IMAGE"] || got["WRAPPER_DELIVERY"] != "imagevolume" {
			t.Errorf("WRAPPER_IMAGE/WRAPPER_DELIVERY not forwarded: %v", got)
		}
	})

	t.Run("AGC_EXTRA not forwarded without the opt-in flag", func(t *testing.T) {
		env := map[string]string{
			"AGC_IMAGE":   "ghcr.io/org/agc" + digest,
			"PROXY_IMAGE": "ghcr.io/org/proxy" + digest,
		}
		environ := func() []string { return []string{"AGC_EXTRA_FOO=bar"} }
		img, err := resolveImages(&gmcFlags{}, fakeGetenv(env), environ)
		if err != nil {
			t.Fatalf("resolveImages: %v", err)
		}
		for _, e := range img.agcExtraEnv {
			if e.Name == "FOO" {
				t.Error("AGC_EXTRA_* must not be forwarded without --allow-agc-extra-env")
			}
		}
	})

	t.Run("floating wrapper image rejected without the opt-out", func(t *testing.T) {
		env := map[string]string{
			"AGC_IMAGE":     "ghcr.io/org/agc" + digest,
			"PROXY_IMAGE":   "ghcr.io/org/proxy" + digest,
			"WRAPPER_IMAGE": "ghcr.io/org/wrapper:v1",
		}
		if _, err := resolveImages(&gmcFlags{}, fakeGetenv(env), noEnviron); err == nil {
			t.Error("a floating WRAPPER_IMAGE must be rejected by default")
		}
	})
}
