package controller

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agcnames "github.com/actions-gateway/github-actions-gateway/agc/names"
	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
)

// §1 — buildNoProxy merge tests

// gkeAPIServerIP is the API server ClusterIP of the GKE cluster where Q465 was
// measured. It is outside 10.96.0.0/12 (the kind/kubeadm Service CIDR that used to
// be hardcoded), which is precisely why the AGC dialled the API server through the
// tenant's egress proxy there and CrashLoopBackOffed.
const gkeAPIServerIP = "34.118.224.1"

// --- buildNoProxy ---

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

// --- buildAGCDeploymentV2: the pod spec shared via buildAGCDeploymentFrom ---

// proxiedAGCDeploymentV2 renders ag's AGC Deployment behind a same-namespace pool
// named "shared", the shape that mounts the proxy CA from the pool's TLS Secret.
func proxiedAGCDeploymentV2(ag *gmcv2alpha1.ActionsGateway, extraEnv []corev1.EnvVar) *appsv1.Deployment {
	proxy := &gmcv2alpha1.EgressProxy{ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: ag.Namespace}}
	return buildAGCDeploymentV2(ag, "agc:test", proxy, gmcv2alpha1.SecurityProfileBaseline, extraEnv)
}

// lastEnvValue returns the value of the last env entry named name — the one
// Kubernetes honors when a name is duplicated.
func lastEnvValue(t *testing.T, dep *appsv1.Deployment, name string) string {
	t.Helper()
	var vals []string
	for _, e := range dep.Spec.Template.Spec.Containers[0].Env {
		if e.Name == name {
			vals = append(vals, e.Value)
		}
	}
	require.NotEmpty(t, vals, "env %s must be set", name)
	return vals[len(vals)-1]
}

func findVolume(vols []corev1.Volume, name string) *corev1.Volume {
	for i := range vols {
		if vols[i].Name == name {
			return &vols[i]
		}
	}
	return nil
}

func findMount(mounts []corev1.VolumeMount, name string) *corev1.VolumeMount {
	for i := range mounts {
		if mounts[i].Name == name {
			return &mounts[i]
		}
	}
	return nil
}

func hasPort(ports []corev1.ContainerPort, name string, port int32) bool {
	for _, p := range ports {
		if p.Name == name && p.ContainerPort == port {
			return true
		}
	}
	return false
}

func TestBuildAGCDeploymentV2_CredentialMount(t *testing.T) {
	dep := proxiedAGCDeploymentV2(v2Gateway("gw", "team-a", "github-app", "shared"), nil)
	require.Len(t, dep.Spec.Template.Spec.Containers, 1)
	c := dep.Spec.Template.Spec.Containers[0]
	env := agcEnv(dep)

	// GitHub App credentials must NOT appear as env vars.
	for _, key := range []string{"GITHUB_APP_ID", "GITHUB_APP_PRIVATE_KEY", "GITHUB_APP_INSTALLATION_ID"} {
		assert.NotContains(t, env, key, "credential env var %s must not be present; use file mount instead", key)
	}

	credVol := findVolume(dep.Spec.Template.Spec.Volumes, agcCredsVolumeName)
	require.NotNil(t, credVol, "github-app-credentials volume must be present")
	require.NotNil(t, credVol.Secret, "volume source must be Secret")
	assert.Equal(t, "github-app", credVol.Secret.SecretName)
	require.NotNil(t, credVol.Secret.DefaultMode)
	// 0o440 paired with fsGroup so the non-root distroless AGC user reads
	// credentials via group ownership; 0o400 alone is unreadable for non-root.
	assert.Equal(t, int32(0o440), *credVol.Secret.DefaultMode, "credentials must be mounted group-readable (0440)")
	require.NotNil(t, dep.Spec.Template.Spec.SecurityContext, "PodSpec must set fsGroup")
	require.NotNil(t, dep.Spec.Template.Spec.SecurityContext.FSGroup)
	assert.Equal(t, int64(65532), *dep.Spec.Template.Spec.SecurityContext.FSGroup)

	credMount := findMount(c.VolumeMounts, agcCredsVolumeName)
	require.NotNil(t, credMount, "github-app-credentials VolumeMount must be present")
	assert.Equal(t, agcCredsMountPath, credMount.MountPath)
	assert.True(t, credMount.ReadOnly, "credential mount must be read-only")
}

func TestBuildAGCDeploymentV2_SecurityContext(t *testing.T) {
	dep := proxiedAGCDeploymentV2(v2Gateway("gw", "team-a", "github-app", "shared"), nil)
	sc := dep.Spec.Template.Spec.Containers[0].SecurityContext
	require.NotNil(t, sc)
	require.NotNil(t, sc.RunAsNonRoot)
	assert.True(t, *sc.RunAsNonRoot)
	require.NotNil(t, sc.ReadOnlyRootFilesystem)
	assert.True(t, *sc.ReadOnlyRootFilesystem)
	require.NotNil(t, sc.AllowPrivilegeEscalation)
	assert.False(t, *sc.AllowPrivilegeEscalation)
	require.NotNil(t, sc.Capabilities)
	assert.Contains(t, sc.Capabilities.Drop, corev1.Capability("ALL"))
	require.NotNil(t, sc.SeccompProfile)
	assert.Equal(t, corev1.SeccompProfileTypeRuntimeDefault, sc.SeccompProfile.Type)
}

func TestBuildAGCDeploymentV2_ProxyEnv(t *testing.T) {
	dep := proxiedAGCDeploymentV2(v2Gateway("gw", "team-a", "github-app", "shared"), nil)
	env := agcEnv(dep)

	proxyAddr := "https://shared-proxy.team-a.svc.cluster.local:8080"
	assert.Equal(t, proxyAddr, env["HTTP_PROXY"])
	assert.Equal(t, proxyAddr, env["HTTPS_PROXY"])
	// NO_PROXY must contain cluster-internal entries even with no user CIDRs.
	for _, entry := range strings.Split(defaultNoProxy, ",") {
		assert.Contains(t, strings.Split(env["NO_PROXY"], ","), entry,
			"NO_PROXY missing mandatory cluster-internal entry %q", entry)
	}
}

func TestBuildAGCDeploymentV2_TracingOffByDefault(t *testing.T) {
	dep := proxiedAGCDeploymentV2(v2Gateway("gw", "team-a", "github-app", "shared"), nil)
	// With no spec.tracing.endpoint, no OTEL_* env may be set — tracing stays
	// off and the AGC keeps its no-op tracer provider.
	for name := range agcEnv(dep) {
		assert.False(t, strings.HasPrefix(name, "OTEL_"),
			"tracing off (empty endpoint) must produce no OTEL_* env; got %q", name)
	}
}

func TestBuildAGCDeploymentV2_TracingEnabled(t *testing.T) {
	ag := v2Gateway("gw", "team-a", "github-app", "shared")
	ag.Spec.Tracing = gmcv2alpha1.TracingConfig{
		Endpoint:   "https://otel-collector.observability:4317",
		Insecure:   ptr(true),
		Sampler:    "parentbased_traceidratio",
		SamplerArg: "0.1",
		ResourceAttributes: map[string]string{
			"deployment.environment": "prod",
			"team":                   "a",
		},
	}
	env := agcEnv(proxiedAGCDeploymentV2(ag, nil))

	assert.Equal(t, "https://otel-collector.observability:4317", env["OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"])
	assert.Equal(t, "true", env["OTEL_EXPORTER_OTLP_TRACES_INSECURE"])
	assert.Equal(t, "parentbased_traceidratio", env["OTEL_TRACES_SAMPLER"])
	assert.Equal(t, "0.1", env["OTEL_TRACES_SAMPLER_ARG"])
	// Resource attributes are rendered with keys sorted so the value is stable
	// across reconciles (random map order would churn the Deployment).
	assert.Equal(t, "deployment.environment=prod,team=a", env["OTEL_RESOURCE_ATTRIBUTES"])

	// Auth headers must never be plumbed through env, regardless of config.
	assert.NotContains(t, env, "OTEL_EXPORTER_OTLP_HEADERS", "OTLP auth headers must not be set via env")
}

func TestBuildAGCDeploymentV2_TracingOmitsUnsetKnobs(t *testing.T) {
	ag := v2Gateway("gw", "team-a", "github-app", "shared")
	// Endpoint only: TLS stays on (no INSECURE), and the optional knobs are absent.
	ag.Spec.Tracing = gmcv2alpha1.TracingConfig{Endpoint: "otel:4317"}
	env := agcEnv(proxiedAGCDeploymentV2(ag, nil))

	assert.Equal(t, "otel:4317", env["OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"])
	for _, name := range []string{
		"OTEL_EXPORTER_OTLP_TRACES_INSECURE",
		"OTEL_TRACES_SAMPLER",
		"OTEL_TRACES_SAMPLER_ARG",
		"OTEL_RESOURCE_ATTRIBUTES",
	} {
		assert.NotContains(t, env, name, "unset tracing knob must not emit %q", name)
	}
}

// TestBuildAGCDeploymentV2_ExtraEnvWins: the testing-gated AGC_EXTRA_* passthrough
// is appended after the spec-derived env, so a duplicate key supplied there is the
// last entry and the one Kubernetes honors. The e2e fakegithub flows rely on it.
func TestBuildAGCDeploymentV2_ExtraEnvWins(t *testing.T) {
	for _, tc := range []struct{ name, extra string }{
		{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "extra-endpoint:4317"},
		{"GITHUB_ORG_URL", "https://github.com/extra-org"},
		{"GITHUB_API_BASE_URL", "http://fakegithub.e2e-infra.svc.cluster.local:8080"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ag := v2Gateway("gw", "team-a", "github-app", "shared")
			ag.Spec.GitHubURL = "https://github.com/spec-org"
			ag.Spec.Tracing = gmcv2alpha1.TracingConfig{Endpoint: "spec-endpoint:4317"}
			dep := proxiedAGCDeploymentV2(ag, []corev1.EnvVar{{Name: tc.name, Value: tc.extra}})
			assert.Equal(t, tc.extra, lastEnvValue(t, dep, tc.name), "AGC_EXTRA_ override must be last")
		})
	}
}

func TestBuildAGCDeploymentV2_NilExtraEnvNoLeaks(t *testing.T) {
	for name := range agcEnv(proxiedAGCDeploymentV2(v2Gateway("gw", "team-a", "github-app", "shared"), nil)) {
		assert.False(t, strings.HasPrefix(name, "AGC_EXTRA_"),
			"nil extraEnv must not produce any AGC_EXTRA_* env var; got %q", name)
	}
}

func TestBuildAGCDeploymentV2_CredentialAnnotation(t *testing.T) {
	dep := proxiedAGCDeploymentV2(v2Gateway("gw", "team-a", "github-app", "shared"), nil)
	ann := dep.Spec.Template.Annotations
	require.NotNil(t, ann, "pod template must have annotations")
	assert.Equal(t, "github-app", ann["actions-gateway/github-app-secret"],
		"pod template annotation must record the referenced Secret name for kubectl rollout history")
}

// TestBuildAGCDeploymentV2_RunnerVersionEnv asserts the GMC injects the pinned
// runner version so the AGC's CreateSession sends a non-empty agent.version that
// GitHub validates (Q71/Q118).
func TestBuildAGCDeploymentV2_RunnerVersionEnv(t *testing.T) {
	env := agcEnv(proxiedAGCDeploymentV2(v2Gateway("gw", "team-a", "github-app", "shared"), nil))
	require.Contains(t, env, "GITHUB_RUNNER_VERSION", "GITHUB_RUNNER_VERSION must be set on the AGC Deployment")
	assert.NotEmpty(t, env["GITHUB_RUNNER_VERSION"])
	assert.Equal(t, agcnames.RunnerVersion, env["GITHUB_RUNNER_VERSION"],
		"GITHUB_RUNNER_VERSION must equal the pinned agcnames.RunnerVersion")
}

// TestBuildAGCDeploymentV2_LogLevel verifies spec.logLevel is threaded to the AGC
// Deployment as the LOG_LEVEL env var: info by default (so a CR without the
// field never silently runs at debug verbosity) and the explicit value when set.
func TestBuildAGCDeploymentV2_LogLevel(t *testing.T) {
	def := proxiedAGCDeploymentV2(v2Gateway("gw", "team-a", "github-app", "shared"), nil)
	assert.Equal(t, "info", agcEnv(def)["LOG_LEVEL"], "LOG_LEVEL must default to info when spec.logLevel is unset")

	dbg := v2Gateway("gw", "team-a", "github-app", "shared")
	dbg.Spec.LogLevel = "debug"
	assert.Equal(t, "debug", agcEnv(proxiedAGCDeploymentV2(dbg, nil))["LOG_LEVEL"],
		"LOG_LEVEL must reflect spec.logLevel=debug")
}

// TestBuildAGCDeploymentV2_GitHubURL verifies that spec.githubURL is threaded to
// the AGC Deployment as the GITHUB_ORG_URL env var.
func TestBuildAGCDeploymentV2_GitHubURL(t *testing.T) {
	ag := v2Gateway("gw", "team-a", "github-app", "shared")
	ag.Spec.GitHubURL = "https://github.com/acme-org"
	assert.Equal(t, "https://github.com/acme-org", agcEnv(proxiedAGCDeploymentV2(ag, nil))["GITHUB_ORG_URL"])
}

func TestBuildAGCDeploymentV2_TerminationGracePeriod(t *testing.T) {
	dep := proxiedAGCDeploymentV2(v2Gateway("gw", "team-a", "github-app", "shared"), nil)
	require.NotNil(t, dep.Spec.Template.Spec.TerminationGracePeriodSeconds)
	assert.Equal(t, int64(60), *dep.Spec.Template.Spec.TerminationGracePeriodSeconds)
}

func TestBuildAGCDeploymentV2_Probes(t *testing.T) {
	dep := proxiedAGCDeploymentV2(v2Gateway("gw", "team-a", "github-app", "shared"), nil)
	c := dep.Spec.Template.Spec.Containers[0]

	// Health listener port must be declared on the container.
	assert.True(t, hasPort(c.Ports, "health", healthMetricsPort),
		"AGC container must declare the health port the kubelet probes")

	require.NotNil(t, c.LivenessProbe)
	require.NotNil(t, c.LivenessProbe.HTTPGet)
	assert.Equal(t, "/healthz", c.LivenessProbe.HTTPGet.Path)
	assert.Equal(t, healthMetricsPort, c.LivenessProbe.HTTPGet.Port.IntVal)

	require.NotNil(t, c.ReadinessProbe)
	require.NotNil(t, c.ReadinessProbe.HTTPGet)
	assert.Equal(t, "/readyz", c.ReadinessProbe.HTTPGet.Path)
	assert.Equal(t, healthMetricsPort, c.ReadinessProbe.HTTPGet.Port.IntVal)

	// The startupProbe gives the AGC manager's informer cache room to sync
	// before liveness takes over; its budget (failureThreshold × periodSeconds)
	// should match the GMC manager's 150s grace.
	require.NotNil(t, c.StartupProbe)
	require.NotNil(t, c.StartupProbe.HTTPGet)
	assert.Equal(t, "/healthz", c.StartupProbe.HTTPGet.Path)
	assert.Equal(t, healthMetricsPort, c.StartupProbe.HTTPGet.Port.IntVal)
	budget := c.StartupProbe.FailureThreshold * c.StartupProbe.PeriodSeconds
	assert.GreaterOrEqual(t, budget, int32(150),
		"startupProbe budget must give the informer cache room to sync")
}

func TestBuildAGCDeploymentV2_MetricsTLSMount(t *testing.T) {
	ag := v2Gateway("gw", "team-a", "github-app", "shared")
	dep := proxiedAGCDeploymentV2(ag, nil)

	vol := findVolume(dep.Spec.Template.Spec.Volumes, metricsTLSVolumeName)
	require.NotNil(t, vol, "metrics-tls volume must be present on the AGC")
	require.NotNil(t, vol.Secret)
	assert.Equal(t, metricsTLSSecretNameV2(ag), vol.Secret.SecretName)
	// The AGC mounts the full server bundle (no Items projection) — it needs the
	// server key to serve mTLS, unlike the proxy-CA mount which is cert-only.
	assert.Empty(t, vol.Secret.Items, "AGC metrics-tls mount must include the server key, not just the cert")

	c := dep.Spec.Template.Spec.Containers[0]
	mount := findMount(c.VolumeMounts, metricsTLSVolumeName)
	require.NotNil(t, mount, "metrics-tls VolumeMount must be present on the AGC")
	assert.Equal(t, metricsTLSMountPath, mount.MountPath)
	assert.True(t, mount.ReadOnly)

	assert.True(t, hasPort(c.Ports, "metrics", metricsPort), "AGC must declare the metrics containerPort on metricsPort")
}

func TestBuildAGCDeploymentV2_ProxyCACertMount(t *testing.T) {
	dep := proxiedAGCDeploymentV2(v2Gateway("gw", "team-a", "github-app", "shared"), nil)

	// CA cert volume must project only tls.crt from the pool's TLS Secret —
	// the private key must NOT reach the AGC container.
	caVol := findVolume(dep.Spec.Template.Spec.Volumes, proxyCACertVolumeName)
	require.NotNil(t, caVol, "proxy CA cert volume must be present on AGC Deployment")
	require.NotNil(t, caVol.Secret, "CA cert volume source must be a Secret")
	assert.Equal(t, v2ProxyTLSSecretName("shared"), caVol.Secret.SecretName)

	// Items projection must expose only tls.crt — never tls.key.
	require.Len(t, caVol.Secret.Items, 1, "only tls.crt must be projected into the AGC — not tls.key")
	assert.Equal(t, corev1.TLSCertKey, caVol.Secret.Items[0].Key)
	assert.Equal(t, corev1.TLSCertKey, caVol.Secret.Items[0].Path)

	caMount := findMount(dep.Spec.Template.Spec.Containers[0].VolumeMounts, proxyCACertVolumeName)
	require.NotNil(t, caMount, "proxy CA cert VolumeMount must be present")
	assert.Equal(t, proxyCACertMountPath, caMount.MountPath)
	assert.True(t, caMount.ReadOnly)
}
