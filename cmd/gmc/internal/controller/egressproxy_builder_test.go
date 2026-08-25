package controller

import (
	"net"
	"reflect"
	"strings"
	"testing"

	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func newEP(name, ns string, mut func(*gmcv2alpha1.EgressProxy)) *gmcv2alpha1.EgressProxy {
	ep := &gmcv2alpha1.EgressProxy{}
	ep.Name = name
	ep.Namespace = ns
	if mut != nil {
		mut(ep)
	}
	return ep
}

func TestEgressProxyDerivedNames(t *testing.T) {
	ep := newEP("shared", "team-a", nil)
	assert.Equal(t, "shared-proxy", proxyResourceName(ep))
	assert.Equal(t, "shared-proxy-tls", egressProxyTLSSecretName(ep))
}

func TestEgressProxyLabelsAndSelector(t *testing.T) {
	ep := newEP("shared", "team-a", nil)

	labels := egressProxyLabels(ep)
	assert.Equal(t, labelManagerValue, labels[labelManagedBy])
	assert.Equal(t, "shared", labels[egressProxyComponentLabel])

	sel := egressProxyPodSelector(ep)
	assert.Equal(t, map[string]string{egressProxyComponentLabel: "shared"}, sel,
		"the per-EgressProxy identity is the whole selector; v1's bare app label would drag the pool "+
			"into v1's PDB, HPA, and anti-affinity during coexistence (Q582)")
}

func TestEgressProxyScalarDefaultsAndOverrides(t *testing.T) {
	// Defaults when spec is empty (a hand-built object that skipped apiserver defaulting).
	def := newEP("shared", "team-a", nil)
	assert.Equal(t, int32(2), egressProxyMinReplicas(def))
	assert.Equal(t, int32(10), egressProxyMaxReplicas(def))
	assert.Equal(t, int32(60), egressProxyTargetCPU(def))

	// Overrides win.
	over := newEP("shared", "team-a", func(ep *gmcv2alpha1.EgressProxy) {
		ep.Spec.MinReplicas = ptr(int32(3))
		ep.Spec.MaxReplicas = ptr(int32(7))
		ep.Spec.TargetCPUUtilizationPercentage = ptr(int32(55))
	})
	assert.Equal(t, int32(3), egressProxyMinReplicas(over))
	assert.Equal(t, int32(7), egressProxyMaxReplicas(over))
	assert.Equal(t, int32(55), egressProxyTargetCPU(over))
}

func TestEgressProxyResourcesMergeOverDefaults(t *testing.T) {
	ep := newEP("shared", "team-a", func(ep *gmcv2alpha1.EgressProxy) {
		ep.Spec.Resources = corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("25m")},
		}
	})
	res := egressProxyResources(ep)
	// Override applied.
	assert.Equal(t, "25m", res.Requests.Cpu().String())
	// Defaults preserved for the keys the override did not set.
	assert.Equal(t, "32Mi", res.Requests.Memory().String())
	assert.Equal(t, "500m", res.Limits.Cpu().String())
	assert.Equal(t, "64Mi", res.Limits.Memory().String())
}

func TestBuildEgressProxyDeployment(t *testing.T) {
	ep := newEP("shared", "team-a", func(ep *gmcv2alpha1.EgressProxy) {
		ep.Spec.MinReplicas = ptr(int32(3))
	})
	dep := buildEgressProxyDeployment(ep, "proxy:test", nil)

	assert.Equal(t, "shared-proxy", dep.Name)
	assert.Equal(t, "team-a", dep.Namespace)
	require.NotNil(t, dep.Spec.Replicas)
	assert.Equal(t, int32(3), *dep.Spec.Replicas)
	assert.Equal(t, "shared", dep.Spec.Selector.MatchLabels[egressProxyComponentLabel])
	assert.Equal(t, "shared", dep.Spec.Template.Labels[egressProxyComponentLabel])

	require.Len(t, dep.Spec.Template.Spec.Containers, 1)
	c := dep.Spec.Template.Spec.Containers[0]
	assert.Equal(t, "proxy:test", c.Image)
	// Hardened container + pod security contexts.
	require.NotNil(t, c.SecurityContext)
	require.NotNil(t, c.SecurityContext.RunAsNonRoot)
	assert.True(t, *c.SecurityContext.RunAsNonRoot)
	require.NotNil(t, c.SecurityContext.ReadOnlyRootFilesystem)
	assert.True(t, *c.SecurityContext.ReadOnlyRootFilesystem)
	require.NotNil(t, dep.Spec.Template.Spec.SecurityContext)

	// Proxy TLS cert + metrics-mTLS server bundle both mounted (Q324).
	require.Len(t, dep.Spec.Template.Spec.Volumes, 2)
	vols := map[string]string{}
	for _, v := range dep.Spec.Template.Spec.Volumes {
		require.NotNil(t, v.Secret)
		vols[v.Name] = v.Secret.SecretName
	}
	assert.Equal(t, "shared-proxy-tls", vols[proxyTLSVolumeName])
	assert.Equal(t, "shared-metrics-tls", vols[metricsTLSVolumeName])

	// Required anti-affinity keyed on the pool identity.
	require.NotNil(t, dep.Spec.Template.Spec.Affinity)
	require.NotNil(t, dep.Spec.Template.Spec.Affinity.PodAntiAffinity)
	terms := dep.Spec.Template.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	require.Len(t, terms, 1)
	assert.Equal(t, "shared", terms[0].LabelSelector.MatchLabels[egressProxyComponentLabel])

	// Proxy TLS env points at the mounted cert; no metrics env.
	envNames := map[string]string{}
	for _, e := range c.Env {
		envNames[e.Name] = e.Value
	}
	assert.Contains(t, envNames, "PROXY_TLS_CERT_FILE")
	assert.Contains(t, envNames, "PROXY_TLS_KEY_FILE")

	// Metrics mTLS env points every file at the mounted metrics bundle (Q324). All
	// three must be set — the proxy binary enables the dedicated mTLS listener only
	// when cert+key+client-CA are all present, else it serves plaintext metrics on
	// the health port (the regression this closes).
	assert.Equal(t, metricsTLSMountPath+"/"+corev1.TLSCertKey, envNames["PROXY_METRICS_TLS_CERT_FILE"])
	assert.Equal(t, metricsTLSMountPath+"/"+corev1.TLSPrivateKeyKey, envNames["PROXY_METRICS_TLS_KEY_FILE"])
	assert.Equal(t, metricsTLSMountPath+"/"+metricsCACertKey, envNames["PROXY_METRICS_CLIENT_CA_FILE"])
	assert.Equal(t, "8443", envNames["PROXY_METRICS_PORT"])

	// The container exposes the mTLS metrics port so the Service/ServiceMonitor can
	// target it by name.
	metricsPortFound := false
	for _, p := range c.Ports {
		if p.Name == "metrics" {
			metricsPortFound = true
			assert.Equal(t, metricsPort, p.ContainerPort)
		}
	}
	assert.True(t, metricsPortFound, "proxy container must expose the metrics port")

	// LOG_LEVEL defaults to info when spec.logLevel is unset (a hand-built object
	// that skipped apiserver defaulting) — never an empty value the proxy would
	// have to interpret (Q327).
	assert.Equal(t, "info", envNames["LOG_LEVEL"])
}

// TestBuildEgressProxyDeployment_LogLevelOverride asserts spec.logLevel lands on
// the proxy container as LOG_LEVEL — the per-pool debug knob, v1 parity (Q327).
func TestBuildEgressProxyDeployment_LogLevelOverride(t *testing.T) {
	ep := newEP("shared", "team-a", func(ep *gmcv2alpha1.EgressProxy) {
		ep.Spec.LogLevel = "debug"
	})
	dep := buildEgressProxyDeployment(ep, "proxy:test", nil)

	require.Len(t, dep.Spec.Template.Spec.Containers, 1)
	for _, e := range dep.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "LOG_LEVEL" {
			assert.Equal(t, "debug", e.Value)
			return
		}
	}
	t.Fatal("LOG_LEVEL env var not found on the proxy container")
}

func TestBuildEgressProxyServiceHPAPDB(t *testing.T) {
	ep := newEP("shared", "team-a", func(ep *gmcv2alpha1.EgressProxy) {
		ep.Spec.MinReplicas = ptr(int32(3))
		ep.Spec.MaxReplicas = ptr(int32(7))
		ep.Spec.TargetCPUUtilizationPercentage = ptr(int32(55))
	})

	svc := buildEgressProxyService(ep)
	assert.Equal(t, "shared-proxy", svc.Name)
	assert.Equal(t, "shared", svc.Spec.Selector[egressProxyComponentLabel])
	require.Len(t, svc.Spec.Ports, 2)
	svcPorts := map[string]int32{}
	for _, p := range svc.Spec.Ports {
		svcPorts[p.Name] = p.Port
	}
	assert.Equal(t, proxyPort, svcPorts["proxy"])
	assert.Equal(t, metricsPort, svcPorts["metrics"], "Service must front the mTLS metrics port (Q324)")

	hpa := buildEgressProxyHPA(ep)
	assert.Equal(t, "shared-proxy", hpa.Name)
	assert.Equal(t, "shared-proxy", hpa.Spec.ScaleTargetRef.Name)
	require.NotNil(t, hpa.Spec.MinReplicas)
	assert.Equal(t, int32(3), *hpa.Spec.MinReplicas)
	assert.Equal(t, int32(7), hpa.Spec.MaxReplicas)
	require.Len(t, hpa.Spec.Metrics, 1)
	require.NotNil(t, hpa.Spec.Metrics[0].Resource.Target.AverageUtilization)
	assert.Equal(t, int32(55), *hpa.Spec.Metrics[0].Resource.Target.AverageUtilization)

	pdb := buildEgressProxyPDB(ep)
	assert.Equal(t, "shared-proxy", pdb.Name)
	require.NotNil(t, pdb.Spec.MinAvailable)
	assert.Equal(t, int32(1), pdb.Spec.MinAvailable.IntVal)
	assert.Equal(t, "shared", pdb.Spec.Selector.MatchLabels[egressProxyComponentLabel])
}

func TestBuildEgressProxyNetworkPolicy(t *testing.T) {
	_, cidr, err := net.ParseCIDR("140.82.112.0/20")
	require.NoError(t, err)
	cidrs := []net.IPNet{*cidr}

	// Managed (default): DNS + GitHub CIDR egress, workload ingress.
	managed := newEP("shared", "team-a", nil)
	np := buildEgressProxyNetworkPolicy(managed, cidrs)
	assert.Equal(t, "shared-proxy", np.Name)
	assert.Equal(t, "shared", np.Spec.PodSelector.MatchLabels[egressProxyComponentLabel])
	assert.True(t, hasGitHubCIDREgress(np, "140.82.112.0/20"), "managed policy must allow egress to the GitHub CIDR")
	// Two ingress rules: workload pods → proxy port, monitoring namespaces → metrics
	// port (Q324).
	require.Len(t, np.Spec.Ingress, 2)
	assert.Equal(t, componentWorkload, np.Spec.Ingress[0].From[0].PodSelector.MatchLabels[labelComponent])
	assert.Equal(t, proxyPort, np.Spec.Ingress[0].Ports[0].Port.IntVal)
	metricsRule := np.Spec.Ingress[1]
	assert.Equal(t, metricsScrapeNamespaceValue, metricsRule.From[0].NamespaceSelector.MatchLabels[metricsScrapeNamespaceLabel])
	assert.Equal(t, metricsPort, metricsRule.Ports[0].Port.IntVal, "monitoring scrape must target the mTLS metrics port")

	// managedNetworkPolicy=false: GitHub CIDR egress omitted (additive model), DNS kept.
	unmanaged := newEP("shared", "team-a", func(ep *gmcv2alpha1.EgressProxy) {
		ep.Spec.ManagedNetworkPolicy = ptr(false)
	})
	npU := buildEgressProxyNetworkPolicy(unmanaged, cidrs)
	assert.False(t, hasGitHubCIDREgress(npU, "140.82.112.0/20"), "unmanaged policy must not add the GitHub CIDR egress rule")
	assert.NotEmpty(t, npU.Spec.Egress, "DNS egress is always present")
}

// TestBuildEgressProxyNetworkPolicy_GitHubRuleMatchesSharedHelper is the Q364
// drift guard. The egress-proxy policy's GitHub-CIDR 443 rule must be EXACTLY the
// output of the shared githubCIDREgressRule helper — byte-for-byte via DeepEqual,
// not merely "contains the CIDR somewhere" like the coarse check above. This pins
// the single-spelling invariant: the v1 proxy/workload policies and this v2 policy
// all render the GitHub allowlist through one helper, so a future edit that
// re-open-codes the rule and diverges (a different port, a namespaceSelector, an
// except block, reordered peers) fails here instead of shipping a policy split.
// It also proves the loop-1 consolidation did not change the emitted policy.
func TestBuildEgressProxyNetworkPolicy_GitHubRuleMatchesSharedHelper(t *testing.T) {
	_, cidr1, err := net.ParseCIDR("140.82.112.0/20")
	require.NoError(t, err)
	_, cidr2, err := net.ParseCIDR("143.55.64.0/20")
	require.NoError(t, err)
	cidrs := []net.IPNet{*cidr1, *cidr2}

	want, ok := githubCIDREgressRule(cidrs)
	require.True(t, ok, "helper must emit a rule for a non-empty CIDR set")

	np := buildEgressProxyNetworkPolicy(newEP("shared", "team-a", nil), cidrs)

	matches := 0
	for _, rule := range np.Spec.Egress {
		if reflect.DeepEqual(rule, want) {
			matches++
		}
	}
	assert.Equal(t, 1, matches,
		"the rendered GitHub-CIDR egress rule must be exactly githubCIDREgressRule's output (one spelling, Q364)")
}

// TestBuildEgressProxyNetworkPolicy_DestinationCIDRsStayDistinct locks in that
// destinationCIDRs — EXTRA, non-GitHub ranges (CRD doc) — render as a SEPARATE
// egress rule and are deliberately NOT routed through githubCIDREgressRule. The two
// rules differ in more than data: destinationCIDRs are gated WITHOUT egressUsesCIDR,
// so in an FQDN egress mode the GitHub rule is omitted while the destinationCIDRs
// rule survives. This guards against a later "consolidation" that would wrongly fold
// the two 443 allowlists into one (Q364).
func TestBuildEgressProxyNetworkPolicy_DestinationCIDRsStayDistinct(t *testing.T) {
	_, ghCIDR, err := net.ParseCIDR("140.82.112.0/20")
	require.NoError(t, err)
	githubCIDRs := []net.IPNet{*ghCIDR}
	const extraCIDR = "10.0.0.0/8"

	// CIDR mode (default): both rules present, and they are distinct rules — the
	// destinationCIDRs rule is not equal to the GitHub helper's output.
	ep := newEP("shared", "team-a", func(ep *gmcv2alpha1.EgressProxy) {
		ep.Spec.DestinationCIDRs = []string{extraCIDR}
	})
	np := buildEgressProxyNetworkPolicy(ep, githubCIDRs)

	ghRule, ok := githubCIDREgressRule(githubCIDRs)
	require.True(t, ok)

	var foundGitHub, foundExtra bool
	for _, rule := range np.Spec.Egress {
		if reflect.DeepEqual(rule, ghRule) {
			foundGitHub = true
			continue
		}
		for _, peer := range rule.To {
			if peer.IPBlock != nil && peer.IPBlock.CIDR == extraCIDR {
				foundExtra = true
				assert.False(t, reflect.DeepEqual(rule, ghRule),
					"destinationCIDRs must be a distinct rule, not the GitHub helper's output")
			}
		}
	}
	assert.True(t, foundGitHub, "GitHub-CIDR rule present in CIDR mode")
	assert.True(t, foundExtra, "destinationCIDRs rule present in CIDR mode")

	// FQDN mode: the GitHub CIDR rule is gated out (egressUsesCIDR=false), but the
	// destinationCIDRs rule persists — proof the two are governed by different gates.
	epFQDN := newEP("shared", "team-a", func(ep *gmcv2alpha1.EgressProxy) {
		ep.Spec.EgressPolicyMode = gmcv2alpha1.EgressPolicyModeFQDN
		ep.Spec.DestinationCIDRs = []string{extraCIDR}
	})
	npFQDN := buildEgressProxyNetworkPolicy(epFQDN, githubCIDRs)

	var ghInFQDN, extraInFQDN bool
	for _, rule := range npFQDN.Spec.Egress {
		if reflect.DeepEqual(rule, ghRule) {
			ghInFQDN = true
		}
		for _, peer := range rule.To {
			if peer.IPBlock != nil && peer.IPBlock.CIDR == extraCIDR {
				extraInFQDN = true
			}
		}
	}
	assert.False(t, ghInFQDN, "GitHub-CIDR rule must be omitted in FQDN mode")
	assert.True(t, extraInFQDN, "destinationCIDRs rule must survive FQDN mode")
}

func TestBuildEgressProxyCertSecret(t *testing.T) {
	ep := newEP("shared", "team-a", nil)
	sec := buildEgressProxyCertSecret(ep, []byte("cert"), []byte("key"))
	assert.Equal(t, "shared-proxy-tls", sec.Name)
	assert.Equal(t, corev1.SecretTypeTLS, sec.Type)
	assert.Equal(t, []byte("cert"), sec.Data[corev1.TLSCertKey])
	assert.Equal(t, []byte("key"), sec.Data[corev1.TLSPrivateKeyKey])
	assert.Equal(t, "shared", sec.Labels[egressProxyComponentLabel])
}

// TestProxyHostSuffix_StripsWildcardPrefix asserts the FQDN-policy wildcard
// convention ("*.example.com") is normalized to the bare parent domain the
// proxy's CONNECT suffix matcher expects, while a plain hostname is unchanged.
func TestProxyHostSuffix_StripsWildcardPrefix(t *testing.T) {
	assert.Equal(t, "actions.githubusercontent.com", proxyHostSuffix("*.actions.githubusercontent.com"))
	assert.Equal(t, "github.com", proxyHostSuffix("github.com"))
}

// TestProxyAllowlistEnv_NilWhenNoExtras locks in that the CONNECT allowlist env
// (Q242 G.1) is opt-in: an EgressProxy with no destinationFQDNs/destinationCIDRs
// must produce no env vars at all, keeping existing proxies byte-for-byte
// unchanged.
func TestProxyAllowlistEnv_NilWhenNoExtras(t *testing.T) {
	ep := newEP("shared", "team-a", nil)
	assert.Nil(t, proxyAllowlistEnv(ep, nil))
}

// TestProxyAllowlistEnv_FQDNsOnly asserts that opting in via destinationFQDNs
// alone produces PROXY_ALLOWED_HOST_SUFFIXES carrying the implicit GitHub host
// suffixes (wildcards normalized) plus the operator's extra FQDNs, and no
// PROXY_ALLOWED_CIDRS (no destinationCIDRs supplied).
func TestProxyAllowlistEnv_FQDNsOnly(t *testing.T) {
	ep := newEP("shared", "team-a", func(ep *gmcv2alpha1.EgressProxy) {
		ep.Spec.DestinationFQDNs = []string{"npm.pkg.example.com"}
	})
	env := proxyAllowlistEnv(ep, nil)
	require.Len(t, env, 1)
	assert.Equal(t, "PROXY_ALLOWED_HOST_SUFFIXES", env[0].Name)

	suffixes := strings.Split(env[0].Value, ",")
	assert.Contains(t, suffixes, "api.github.com")
	assert.Contains(t, suffixes, "github.com")
	assert.Contains(t, suffixes, "codeload.github.com")
	assert.Contains(t, suffixes, "objects.githubusercontent.com")
	// Wildcard GitHub entries must be normalized (leading "*." stripped).
	assert.Contains(t, suffixes, "actions.githubusercontent.com")
	assert.Contains(t, suffixes, "blob.core.windows.net")
	assert.NotContains(t, suffixes, "*.actions.githubusercontent.com")
	// Operator's extra FQDN is appended verbatim.
	assert.Contains(t, suffixes, "npm.pkg.example.com")
}

// TestProxyAllowlistEnv_CIDRsAddSecondEnvVar asserts that supplying
// destinationCIDRs adds PROXY_ALLOWED_CIDRS alongside the host-suffix env,
// carrying exactly the operator's CIDRs (never the GitHub CIDR set).
func TestProxyAllowlistEnv_CIDRsAddSecondEnvVar(t *testing.T) {
	ep := newEP("shared", "team-a", func(ep *gmcv2alpha1.EgressProxy) {
		ep.Spec.DestinationFQDNs = []string{"npm.pkg.example.com"}
		ep.Spec.DestinationCIDRs = []string{"10.0.0.0/8", "192.168.1.0/24"}
	})
	env := proxyAllowlistEnv(ep, nil)
	require.Len(t, env, 2)
	assert.Equal(t, "PROXY_ALLOWED_HOST_SUFFIXES", env[0].Name)
	assert.Equal(t, "PROXY_ALLOWED_CIDRS", env[1].Name)
	assert.Equal(t, "10.0.0.0/8,192.168.1.0/24", env[1].Value)
}

// TestProxyAllowlistEnv_CIDRsOnlyStillEmitsHostSuffixes asserts that opting in
// via destinationCIDRs alone still emits the GitHub host-suffix allowlist (the
// implicit GitHub set is unconditional whenever the env is emitted at all).
func TestProxyAllowlistEnv_CIDRsOnlyStillEmitsHostSuffixes(t *testing.T) {
	ep := newEP("shared", "team-a", func(ep *gmcv2alpha1.EgressProxy) {
		ep.Spec.DestinationCIDRs = []string{"10.0.0.0/8"}
	})
	env := proxyAllowlistEnv(ep, nil)
	require.Len(t, env, 2)
	assert.Equal(t, "PROXY_ALLOWED_HOST_SUFFIXES", env[0].Name)
	assert.Contains(t, env[0].Value, "github.com")
	assert.Equal(t, "PROXY_ALLOWED_CIDRS", env[1].Name)
	assert.Equal(t, "10.0.0.0/8", env[1].Value)
}

// TestProxyAuditEnv_NilWhenOff is the security control on the GMC side: a pool
// that has not opted in gets no audit env, so its pod template is byte-for-byte
// what it was before this field existed and a GMC upgrade rolls nothing. The
// empty case is the pre-defaulting one — a hand-built object, or a stored object
// written before the field — and must land on Off with the enum default.
func TestProxyAuditEnv_NilWhenOff(t *testing.T) {
	assert.Nil(t, proxyAuditEnv(newEP("shared", "team-a", nil)), "unset must inject nothing")
	assert.Nil(t, proxyAuditEnv(newEP("shared", "team-a", func(ep *gmcv2alpha1.EgressProxy) {
		ep.Spec.AuditLogging = "Off"
	})), "explicit Off must inject nothing")
}

// TestProxyAuditEnv_ConnectionsInjectsModeAndNamespace pins both halves of the
// opt-in: the mode the proxy parses, and the namespace it stamps on the record.
// The namespace must come from the downward API rather than a formatted value —
// the record's attribution is only trustworthy if the pod supplies it.
func TestProxyAuditEnv_ConnectionsInjectsModeAndNamespace(t *testing.T) {
	ep := newEP("shared", "team-a", func(ep *gmcv2alpha1.EgressProxy) {
		ep.Spec.AuditLogging = "Connections"
	})
	env := proxyAuditEnv(ep)
	require.Len(t, env, 2)

	assert.Equal(t, "PROXY_AUDIT_LOGGING", env[0].Name)
	assert.Equal(t, "Connections", env[0].Value)

	assert.Equal(t, "POD_NAMESPACE", env[1].Name)
	assert.Empty(t, env[1].Value, "the namespace must not be a formatted literal")
	require.NotNil(t, env[1].ValueFrom)
	require.NotNil(t, env[1].ValueFrom.FieldRef)
	assert.Equal(t, "metadata.namespace", env[1].ValueFrom.FieldRef.FieldPath)
}

// TestEgressProxyDeployment_AuditEnvReachesTheContainer closes the gap between
// the helper and the pod template: proxyAuditEnv can be correct and still not be
// wired in, and only the assembled Deployment says which.
func TestEgressProxyDeployment_AuditEnvReachesTheContainer(t *testing.T) {
	envNames := func(ep *gmcv2alpha1.EgressProxy) map[string]corev1.EnvVar {
		dep := buildEgressProxyDeployment(ep, "proxy:test", nil)
		require.Len(t, dep.Spec.Template.Spec.Containers, 1)
		out := map[string]corev1.EnvVar{}
		for _, e := range dep.Spec.Template.Spec.Containers[0].Env {
			out[e.Name] = e
		}
		return out
	}

	off := envNames(newEP("shared", "team-a", nil))
	assert.NotContains(t, off, "PROXY_AUDIT_LOGGING")
	assert.NotContains(t, off, "POD_NAMESPACE")

	on := envNames(newEP("shared", "team-a", func(ep *gmcv2alpha1.EgressProxy) {
		ep.Spec.AuditLogging = "Connections"
	}))
	require.Contains(t, on, "PROXY_AUDIT_LOGGING")
	assert.Equal(t, "Connections", on["PROXY_AUDIT_LOGGING"].Value)
	require.Contains(t, on, "POD_NAMESPACE")
	// The allowlist env is independent of the audit env: neither may displace
	// the other when both are appended to the same base slice.
	assert.Contains(t, on, "LOG_LEVEL")
	assert.Contains(t, on, "PROXY_TLS_CERT_FILE")
}

// TestEgressProxyDeployment_AuditAndAllowlistEnvCoexist asserts the append does
// not drop either optional block when a pool opts into both.
func TestEgressProxyDeployment_AuditAndAllowlistEnvCoexist(t *testing.T) {
	ep := newEP("shared", "team-a", func(ep *gmcv2alpha1.EgressProxy) {
		ep.Spec.AuditLogging = "Connections"
		ep.Spec.DestinationCIDRs = []string{"10.0.0.0/8"}
	})
	dep := buildEgressProxyDeployment(ep, "proxy:test", nil)
	names := map[string]bool{}
	for _, e := range dep.Spec.Template.Spec.Containers[0].Env {
		names[e.Name] = true
	}
	assert.True(t, names["PROXY_ALLOWED_HOST_SUFFIXES"])
	assert.True(t, names["PROXY_ALLOWED_CIDRS"])
	assert.True(t, names["PROXY_AUDIT_LOGGING"])
	assert.True(t, names["POD_NAMESPACE"])
}
