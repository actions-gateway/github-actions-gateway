package controller

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFormatResourceAttributes_Deterministic(t *testing.T) {
	attrs := map[string]string{"z": "1", "a": "2", "m": "3"}
	// Sorted-key rendering must be identical on repeated calls.
	assert.Equal(t, "a=2,m=3,z=1", formatResourceAttributes(attrs))
	assert.Equal(t, "a=2,m=3,z=1", formatResourceAttributes(attrs))
	assert.Equal(t, "", formatResourceAttributes(nil))
}

// §1 — buildNoProxy merge tests

// gkeAPIServerIP is the API server ClusterIP of the GKE cluster where Q465 was
// measured. It is outside 10.96.0.0/12 (the kind/kubeadm Service CIDR that used to
// be hardcoded), which is precisely why the AGC dialled the API server through the
// tenant's egress proxy there and CrashLoopBackOffed.
const gkeAPIServerIP = "34.118.224.1"

func TestBuildNoProxy_DefaultWhenEmpty(t *testing.T) {
	t.Setenv(apiServerHostEnv, gkeAPIServerIP)
	assert.Equal(t, defaultNoProxy+","+gkeAPIServerIP, buildNoProxy(nil))
}

func TestBuildNoProxy_UserCIDRsPrependedToDefaults(t *testing.T) {
	t.Setenv(apiServerHostEnv, gkeAPIServerIP)
	result := buildNoProxy([]string{"192.168.0.0/16"})
	assert.True(t, strings.HasPrefix(result, "192.168.0.0/16,"), "user CIDR should be first")
	for _, entry := range strings.Split(defaultNoProxy, ",") {
		assert.Contains(t, result, entry)
	}
}

func TestBuildNoProxy_AlwaysContainsClusterLocal(t *testing.T) {
	t.Setenv(apiServerHostEnv, gkeAPIServerIP)
	result := buildNoProxy([]string{"10.0.0.0/8"})
	// svc.cluster.local covers all Kubernetes Services, including
	// kubernetes.default.svc.cluster.local (the kube-apiserver).
	assert.Contains(t, result, "svc.cluster.local")
}

// TestBuildNoProxy_ExemptsAPIServerOnNonKindServiceCIDR is the Q465 regression: on
// every managed distribution the API server ClusterIP falls outside the kubeadm
// Service CIDR, so a hardcoded 10.96.0.0/12 left the AGC's own API traffic pointed
// at the tenant's egress proxy. The exemption must follow the cluster's actual API
// server address, and the stale range must be gone.
func TestBuildNoProxy_ExemptsAPIServerOnNonKindServiceCIDR(t *testing.T) {
	for name, host := range map[string]string{
		"gke":          gkeAPIServerIP,
		"eks":          "172.20.0.1",
		"aks":          "10.0.0.1",
		"kind/kubeadm": "10.96.0.1",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(apiServerHostEnv, host)
			result := buildNoProxy(nil)
			assert.Contains(t, strings.Split(result, ","), host,
				"NO_PROXY must exempt this cluster's API server ClusterIP")
			assert.NotContains(t, result, "10.96.0.0/12",
				"the kubeadm Service CIDR must not be hardcoded into NO_PROXY")
		})
	}
}

// TestBuildNoProxy_BracketsIPv6APIServer guards the IPv6 form: Go's NO_PROXY parser
// reads a bare "fd00:10:96::1" as host "fd00:10:96:" with port "1", and the entry would
// silently never match, putting the AGC right back on the proxy.
func TestBuildNoProxy_BracketsIPv6APIServer(t *testing.T) {
	t.Setenv(apiServerHostEnv, "fd00:10:96::1")
	assert.Contains(t, strings.Split(buildNoProxy(nil), ","), "[fd00:10:96::1]")

	// An already-bracketed value is passed through, never double-bracketed.
	t.Setenv(apiServerHostEnv, "[fd00:10:96::1]")
	assert.Contains(t, strings.Split(buildNoProxy(nil), ","), "[fd00:10:96::1]")
}

// TestBuildNoProxy_FallsBackToKubeletExpansion covers the GMC running outside the
// cluster (`make run`, envtest), where its own KUBERNETES_SERVICE_HOST is unset: the
// literal is emitted so the kubelet expands it from the AGC pod's own service env.
func TestBuildNoProxy_FallsBackToKubeletExpansion(t *testing.T) {
	t.Setenv(apiServerHostEnv, "")
	assert.Contains(t, strings.Split(buildNoProxy(nil), ","), "$(KUBERNETES_SERVICE_HOST)")
}

// TestBuildNoProxy_DefaultsAreMinimal pins the mandatory exemption set. NO_PROXY is
// an egress-control surface: every entry here is traffic that bypasses the tenant's
// egress proxy and so escapes per-tenant egress-IP attribution. Widening this set is
// a security decision, not a convenience one — this test makes it a deliberate edit.
func TestBuildNoProxy_DefaultsAreMinimal(t *testing.T) {
	t.Setenv(apiServerHostEnv, gkeAPIServerIP)
	assert.ElementsMatch(t, []string{
		"kubernetes.default.svc",
		"svc.cluster.local",
		"localhost",
		"127.0.0.1",
		gkeAPIServerIP,
	}, strings.Split(buildNoProxy(nil), ","))

	// No IP range is exempted by default: a CIDR covers hosts the operator never
	// named, and the only address we must exempt is a single API server ClusterIP.
	assert.NotContains(t, buildNoProxy(nil), "/", "the default exemptions must contain no CIDR")
}

// §2 — buildWorkloadNetworkPolicy egress and buildProxyNetworkPolicy DNS

// §4 — buildProxyServiceAddr format

// §5 — untested resource constructors

// The grace period is a claim about how long shutdown takes; this asserts the
// claim still covers every wait the sequence actually performs. Getting it wrong
// means SIGKILL lands mid-drain and silently undoes Q384.
func TestProxyShutdownBudget_GracePeriodCoversSequence(t *testing.T) {
	worstCase := proxyDrainBudgetSeconds + proxyDrainTailSeconds
	assert.Less(t, worstCase, proxyTerminationGracePeriodSeconds,
		"grace period must exceed the drain budget plus its force-close tail, with headroom to spare")
	assert.Equal(t, proxyTerminationGracePeriodSeconds-worstCase, proxyExitHeadroomSeconds,
		"headroom must be exactly what the grace period has left over")

	// Guards the cross-module mirror: cmd/proxy's defaultShutdownDrainTimeout is
	// 45s and cannot be imported here (separate Go module).
	assert.Equal(t, 45, proxyDrainBudgetSeconds,
		"must match defaultShutdownDrainTimeout in cmd/proxy/proxy.go")
}
