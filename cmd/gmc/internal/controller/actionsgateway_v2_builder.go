package controller

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"time"

	agcnames "github.com/actions-gateway/github-actions-gateway/agc/names"
	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// v2 ActionsGateway resource derivation. Multi-gateway-per-namespace (M3b, §H.16
// #1): every AGC control-plane child is named per ActionsGateway — "<ag>-agc" for
// the Deployment/ServiceAccount/RoleBinding/Service/AGC NetworkPolicy (and the pod
// `app` label), "<ag>-worker" for the worker ServiceAccount, "<ag>-workload" for
// the workload NetworkPolicy, and "<ag>-agc-metrics-{tls,client}" for the metrics
// Secrets — so two gateways in one namespace never collide on a fixed name and
// deleting one GCs only its own children. The CR name carries a webhook/CEL
// maxLength of 52 (§H.6), leaving headroom for these suffixes under the 63-char
// label-value / Service-name ceiling ("<ag>-agc" ≤ 56 as a pod `app` label value).
//
// Unlike v1, the v2 ActionsGateway reconciler does NOT provision the egress proxy
// pool (now a standalone EgressProxy, M2) or stamp Pod Security Admission labels
// (now the NamespacePSAReconciler, Q175). It provisions only the AGC control plane
// and wires its egress through the resolved EgressProxy.

// gatewayComponentLabel carries the owning ActionsGateway's name on every child,
// so multi-gateway selectors have an identity to key on and operators can
// `kubectl get -l actions-gateway.com/gateway=<ag>` a gateway's resources.
const gatewayComponentLabel = "actions-gateway.com/gateway"

// Per-gateway resource-name suffixes (§H.16 #1). Kept under the §H.6 52-char CR
// name cap so the derived names stay within RFC 1123's 63-char label-value /
// Service-name ceiling.
const (
	agcResourceSuffix      = "-agc"
	agcWorkerSuffix        = "-worker"
	agcWorkloadNPSuffix    = "-workload"
	agcMetricsTLSSuffix    = "-agc-metrics-tls"
	agcMetricsClientSuffix = "-agc-metrics-client"
)

// agcNameV2 is the per-gateway AGC name: the Deployment, ServiceAccount,
// RoleBinding, Service, and AGC NetworkPolicy all use it, and it is the pod `app`
// label value so the NetworkPolicy and Service select exactly this gateway's AGC
// pods (never a sibling gateway's).
func agcNameV2(ag *gmcv2alpha1.ActionsGateway) string { return ag.Name + agcResourceSuffix }

// workerSANameV2 is the per-gateway worker ServiceAccount the AGC stamps on its
// worker pods (threaded via WORKER_SERVICE_ACCOUNT); owner-referenced to the
// gateway so deleting it GCs only this gateway's worker SA.
func workerSANameV2(ag *gmcv2alpha1.ActionsGateway) string { return ag.Name + agcWorkerSuffix }

// workloadNPNameV2 is the per-gateway workload NetworkPolicy name. Its selector
// stays the shared component=workload (§H.16 #1 is a naming change, not a policy
// rewrite), so co-located gateways' workload pods share the same egress lockdown.
func workloadNPNameV2(ag *gmcv2alpha1.ActionsGateway) string { return ag.Name + agcWorkloadNPSuffix }

// metricsTLSSecretNameV2 / metricsClientSecretNameV2 are the per-gateway metrics
// mTLS server/scraper Secrets, with per-gateway cert SANs for the "<ag>-agc"
// Service so two AGCs in one namespace never share a metrics identity.
func metricsTLSSecretNameV2(ag *gmcv2alpha1.ActionsGateway) string {
	return ag.Name + agcMetricsTLSSuffix
}
func metricsClientSecretNameV2(ag *gmcv2alpha1.ActionsGateway) string {
	return ag.Name + agcMetricsClientSuffix
}

// v2GatewayLabels returns the metadata labels stamped on every AGC control-plane
// child: the managed-by marker plus the per-gateway identity label.
func v2GatewayLabels(ag *gmcv2alpha1.ActionsGateway) map[string]string {
	return map[string]string{
		labelManagedBy:        labelManagerValue,
		gatewayComponentLabel: ag.Name,
	}
}

func buildAGCServiceAccountV2(ag *gmcv2alpha1.ActionsGateway) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: agcNameV2(ag), Namespace: ag.Namespace, Labels: v2GatewayLabels(ag)},
	}
}

func buildWorkerServiceAccountV2(ag *gmcv2alpha1.ActionsGateway) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: workerSANameV2(ag), Namespace: ag.Namespace, Labels: v2GatewayLabels(ag)},
	}
}

// clusterRunnerTemplateReaderRole is the shipped read-only ClusterRole (chart +
// GMC config) that grants get/list/watch on the cluster-scoped ClusterRunnerTemplate
// kind. The GMC holds only `bind` on this exact name (not create/escalate), so a
// compromised GMC cannot wire AGC SAs into arbitrary ClusterRoles.
const clusterRunnerTemplateReaderRole = "agc-clusterrunnertemplate-reader"

// clusterRunnerTemplateReaderBindingName is the per-gateway ClusterRoleBinding name.
// ClusterRoleBindings are cluster-scoped, so the name embeds both the namespace and
// the gateway to stay globally unique; the dot separators are collision-free because
// a namespace and a gateway name are each a single DNS label (no embedded dots).
func clusterRunnerTemplateReaderBindingName(ag *gmcv2alpha1.ActionsGateway) string {
	return fmt.Sprintf("%s.%s.%s", clusterRunnerTemplateReaderRole, ag.Namespace, ag.Name)
}

// buildClusterRunnerTemplateReaderBinding grants this gateway's AGC ServiceAccount
// cluster-scoped read of ClusterRunnerTemplate, the one AGC dependency a namespaced
// RoleBinding cannot authorize. It is least-privilege (read-only on a single
// cluster-scoped kind, bound to exactly this gateway's AGC SA). A cluster-scoped
// object cannot carry an OwnerReference to a namespaced ActionsGateway, so cascade
// GC does not apply: the reconciler deletes this binding explicitly on gateway
// deletion (reconcileDelete).
func buildClusterRunnerTemplateReaderBinding(ag *gmcv2alpha1.ActionsGateway) *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: clusterRunnerTemplateReaderBindingName(ag), Labels: v2GatewayLabels(ag)},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: clusterRunnerTemplateReaderRole},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: agcNameV2(ag), Namespace: ag.Namespace}},
	}
}

// buildAGCRoleBindingV2 binds the per-gateway AGC ServiceAccount to the shipped
// agc-tenant-role ClusterRole (scoped to this namespace), identical to v1 except
// the per-gateway SA/binding name.
func buildAGCRoleBindingV2(ag *gmcv2alpha1.ActionsGateway) *rbacv1.RoleBinding {
	name := agcNameV2(ag)
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ag.Namespace, Labels: v2GatewayLabels(ag)},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: agcTenantRoleName},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: name, Namespace: ag.Namespace}},
	}
}

// buildAGCServiceV2 builds the per-gateway AGC metrics Service (mTLS metrics port
// :8443), selecting this gateway's AGC pods so a ServiceMonitor scrapes the right
// listener. Mirrors v1's buildAGCService.
func buildAGCServiceV2(ag *gmcv2alpha1.ActionsGateway) *corev1.Service {
	name := agcNameV2(ag)
	labels := v2GatewayLabels(ag)
	labels["app"] = name
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ag.Namespace, Labels: labels},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": name},
			Ports:    []corev1.ServicePort{{Name: "metrics", Port: metricsPort, TargetPort: intstr.FromInt32(metricsPort), Protocol: corev1.ProtocolTCP}},
			Type:     corev1.ServiceTypeClusterIP,
		},
	}
}

// buildWorkloadNetworkPolicyV2 is the v2 workload egress lockdown: AGC and worker
// pods may reach DNS and the proxy on :8080; default-deny ingress. In the proxied
// case it is identical in shape to v1's buildWorkloadNetworkPolicy (the proxy
// podSelector "app: proxy" matches the EgressProxy pods), so worker egress-IP
// attribution is no weaker than v1.
//
// When the gateway has no defaultProxyRef (direct egress, §H.10) the policy
// additionally permits the GitHub CIDRs on 443 so proxy-less workers reach GitHub
// directly. The proxy rule is retained alongside it (harmless when no proxy pods
// exist) so a RunnerSet that sets its own proxyRef under a direct gateway still
// reaches its proxy. Restriction is preserved in every case: egress stays
// default-deny except DNS + the proxy + (in direct mode) the GitHub allowlist —
// never arbitrary internet. githubCIDRs comes from the shared IP-range cache; empty
// (pre-first-fetch) omits the GitHub rule, fail-closed, until the refresh patches it.
func buildWorkloadNetworkPolicyV2(ag *gmcv2alpha1.ActionsGateway, githubCIDRs []net.IPNet, direct bool) *networkingv1.NetworkPolicy {
	egress := []networkingv1.NetworkPolicyEgressRule{
		dnsEgressRule(),
		{
			Ports: []networkingv1.NetworkPolicyPort{{Port: ptr(intstr.FromInt32(proxyPort))}},
			To: []networkingv1.NetworkPolicyPeer{{
				PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": proxyAppName}},
			}},
		},
	}
	if direct {
		if rule, ok := githubCIDREgressRule(githubCIDRs); ok {
			egress = append(egress, rule)
		}
	}
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: workloadNPNameV2(ag), Namespace: ag.Namespace, Labels: v2GatewayLabels(ag)},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{labelComponent: componentWorkload}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress, networkingv1.PolicyTypeIngress},
			Egress:      egress,
			// No Ingress rules: PolicyTypeIngress with an empty rule set is default-deny.
		},
	}
}

// buildAGCNetworkPolicyV2 is the v2 AGC egress policy: additive to the workload
// policy, it also permits Kubernetes API server egress (443/6443) and admits
// monitoring-namespace metrics scrapes. In the proxied case it is identical in shape
// to v1's buildAGCNetworkPolicy (the AGC reaches GitHub through the proxy, admitted
// by the workload policy's :8080 rule).
//
// When the gateway has no defaultProxyRef (direct egress, §H.10) it additionally
// permits the GitHub CIDRs on 443 so the AGC control plane reaches GitHub directly
// (token exchange, broker long-poll, runner registration). The kube-API-server rule
// stays mandatory either way — the AGC must reach the apiserver regardless of egress
// mode. Restriction is preserved: DNS + kube API + (direct) the GitHub allowlist,
// never arbitrary internet.
func buildAGCNetworkPolicyV2(ag *gmcv2alpha1.ActionsGateway, apiServerCIDRs []string, githubCIDRs []net.IPNet, direct bool) *networkingv1.NetworkPolicy {
	name := agcNameV2(ag)
	np := buildAGCNetworkPolicyFrom(ag.Namespace, name, name, v2GatewayLabels(ag), apiServerCIDRs)
	if direct {
		if rule, ok := githubCIDREgressRule(githubCIDRs); ok {
			np.Spec.Egress = append(np.Spec.Egress, rule)
		}
	}
	if rule, ok := vaultEgressRule(ag); ok {
		np.Spec.Egress = append(np.Spec.Egress, rule)
	}
	return np
}

// vaultEgressRule returns the scoped AGC→Vault egress rule for a workload-identity gateway
// whose Vault signer declares a NetworkPolicy peer, and false otherwise (Q202). It is emitted
// only for credentials.type=WorkloadIdentity with a Vault signer that sets
// signer.vault.networkPolicy — never for a possession-model (githubApp) gateway, so the
// default-deny posture is preserved for PEM gateways. The rule is tightly scoped: a single
// pod/namespace selector peer (in-cluster Vault) or a CIDR peer (external Vault), on the Vault
// API port parsed from the signer Address — never a broaden-to-all-egress. Added additively
// to the AGC-only policy, so only the AGC reaches Vault; worker pods (which carry no AGC `app`
// label) do not.
func vaultEgressRule(ag *gmcv2alpha1.ActionsGateway) (networkingv1.NetworkPolicyEgressRule, bool) {
	creds := ag.Spec.Credentials
	if creds.Type != gmcv2alpha1.CredentialTypeWorkloadIdentity || creds.WorkloadIdentity == nil {
		return networkingv1.NetworkPolicyEgressRule{}, false
	}
	signer := creds.WorkloadIdentity.Signer
	if signer.Provider != gmcv2alpha1.SignerProviderVault || signer.Vault == nil || signer.Vault.NetworkPolicy == nil {
		return networkingv1.NetworkPolicyEgressRule{}, false
	}
	npc := signer.Vault.NetworkPolicy
	var peer networkingv1.NetworkPolicyPeer
	if npc.CIDR != "" {
		peer = networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: npc.CIDR}}
	} else {
		peer = networkingv1.NetworkPolicyPeer{
			PodSelector:       npc.PodSelector,
			NamespaceSelector: npc.NamespaceSelector,
		}
	}
	return networkingv1.NetworkPolicyEgressRule{
		Ports: []networkingv1.NetworkPolicyPort{{Port: ptr(intstr.FromInt32(vaultEgressPort(signer.Vault.Address)))}},
		To:    []networkingv1.NetworkPolicyPeer{peer},
	}, true
}

// vaultEgressPort parses the Vault API port from the signer Address for the egress rule. An
// explicit port wins; otherwise the scheme default (https→443, http→80). Falls back to Vault's
// conventional 8200 only if Address fails to parse — Address is CEL-validated as ^https?:// at
// admission, so the fallback is defensive.
func vaultEgressPort(address string) int32 {
	u, err := url.Parse(address)
	if err != nil {
		return 8200
	}
	if p := u.Port(); p != "" {
		if n, convErr := strconv.Atoi(p); convErr == nil {
			return int32(n)
		}
	}
	if u.Scheme == "http" {
		return 80
	}
	return 443
}

// v2ProxyServiceAddr is the in-cluster HTTPS proxy address the AGC egresses
// through: the resolved EgressProxy's Service. Mirrors v1's buildProxyServiceAddr
// but keyed on the EgressProxy name (<ep>-proxy) rather than the fixed v1 proxy
// Service.
func v2ProxyServiceAddr(namespace, epName string) string {
	return fmt.Sprintf("https://%s-proxy.%s.svc.cluster.local:%d", epName, namespace, proxyPort)
}

// v2ProxyTLSSecretName is the resolved EgressProxy's self-signed TLS Secret name,
// matching the EgressProxy reconciler's egressProxyTLSSecretName (<ep>-proxy-tls).
func v2ProxyTLSSecretName(epName string) string { return epName + egressProxyTLSSuffix }

// defaultAGCResources is the platform default resource footprint stamped on the
// v2 AGC control-plane container when spec.agcResources does not override a given
// request/limit. It encodes the documented Appendix A capacity sizing
// (docs/design/appendix-a-capacity-slos.md):
//
//   - memory request 2Gi  — generous scheduling reservation for the ~1,000-goroutine
//     peak burst (~60 MiB) with a large safety margin for Go runtime overhead and
//     heap churn during reconcile storms.
//   - memory limit 4Gi    — a hard cap set well above the working set, so transient
//     bursts don't OOMKill the single-pod control plane while a true runaway is
//     still bounded.
//   - cpu request 500m     — baseline scheduling weight; the AGC is predominantly
//     I/O-bound (long-poll blocked), so steady CPU draw is far below this.
//   - cpu limit 2 (cores)  — burst ceiling that absorbs reconcile churn / token-
//     refresh contention without throttling steady-state polling.
func defaultAGCResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("2Gi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("2"),
			corev1.ResourceMemory: resource.MustParse("4Gi"),
		},
	}
}

// agcResources returns the AGC container resource requirements: the platform
// defaults (defaultAGCResources) overlaid with any per-gateway spec.agcResources
// overrides, mirroring proxyResources. The overlay is per request/limit key, so a
// tenant that sets only one knob (e.g. limits.memory) keeps the sane default for
// every key it does not set — an unset agcResources reproduces the platform
// default unchanged.
func agcResources(override *corev1.ResourceRequirements) corev1.ResourceRequirements {
	res := defaultAGCResources()
	if override == nil {
		return res
	}
	for k, v := range override.Requests {
		res.Requests[k] = v
	}
	for k, v := range override.Limits {
		res.Limits[k] = v
	}
	return res
}

// buildAGCDeploymentV2 builds the AGC Deployment for a v2 ActionsGateway. It
// mirrors v1's buildAGCDeployment (credentials mounted as files never env, proxy
// CA pinning, metrics mTLS mount, hardened SecurityContext, probes) and reads the
// security profile from the namespace (PSA is namespace-scoped in v2).
//
// proxy is the resolved EgressProxy, or nil for direct egress (§H.10): when nil the
// AGC gets no HTTP(S)_PROXY/PROXY_TLS_SECRET_NAME env, so its own control-plane HTTP
// clients reach GitHub directly (governed by the direct-egress AGC NetworkPolicy),
// and no proxy-CA volume is mounted. securityProfile is the namespace's effective
// profile.
func buildAGCDeploymentV2(ag *gmcv2alpha1.ActionsGateway, agcImage string, proxy *gmcv2alpha1.EgressProxy, securityProfile string, extraEnv []corev1.EnvVar) *appsv1.Deployment {
	var proxyTLSSecret string

	env := []corev1.EnvVar{
		{Name: "POD_NAMESPACE", ValueFrom: fieldRef("metadata.namespace")},
		{Name: "WORKER_SERVICE_ACCOUNT", Value: workerSANameV2(ag)},
		// GATEWAY_NAME scopes this AGC to its own gateway: the RunnerSet reconciler
		// reconciles only the RunnerSets whose spec.gatewayRef.name matches it, via a
		// server-side field selector on the informer (§H.16 #1). Without it, N AGC
		// Deployments in one namespace would all reconcile every RunnerSet and fight.
		{Name: "GATEWAY_NAME", Value: ag.Name},
		{Name: "GITHUB_ORG_URL", Value: ag.Spec.GitHubURL},
		// SECURITY_PROFILE comes from the namespace security-profile label (v2 moved
		// PSA to the namespace, Q175). The reconciler reads the label; it does not
		// stamp the PSA labels — that is the NamespacePSAReconciler's job.
		{Name: "SECURITY_PROFILE", Value: securityProfile},
		{Name: "LOG_LEVEL", Value: logLevelOrDefault(ag.Spec.LogLevel)},
		{Name: "GITHUB_RUNNER_VERSION", Value: agcnames.RunnerVersion},
	}
	// Proxied: wire the AGC's own egress through the resolved EgressProxy. Direct
	// (proxy == nil): omit the proxy env entirely so the AGC's HTTP clients go direct.
	if proxy != nil {
		proxyAddr := v2ProxyServiceAddr(ag.Namespace, proxy.Name)
		proxyTLSSecret = v2ProxyTLSSecretName(proxy.Name)
		env = append(env,
			corev1.EnvVar{Name: "HTTP_PROXY", Value: proxyAddr},
			corev1.EnvVar{Name: "HTTPS_PROXY", Value: proxyAddr},
			corev1.EnvVar{Name: "NO_PROXY", Value: buildNoProxy(proxy.Spec.NoProxyCIDRs)},
			corev1.EnvVar{Name: "PROXY_TLS_SECRET_NAME", Value: proxyTLSSecret},
		)
	}
	// Credential method (Q196/Q197/Q201). GitHubApp threads no extra env (the AGC
	// reads the mounted Secret files); WorkloadIdentity threads the non-secret signer
	// config — the projected ServiceAccount token volume is added by
	// buildAGCDeploymentFrom when no credential Secret is named.
	env = append(env, credentialEnvV2(ag)...)
	env = append(env, tracingEnvV2(ag.Spec.Tracing)...)
	env = append(env, extraEnv...)

	// The pod spec (credential/proxy-CA/metrics mounts, hardened SecurityContext,
	// probes) is identical to v1; only the metadata labels, the per-gateway derived
	// names, the proxy CA Secret name, and the env differ. Share the assembly via
	// buildAGCDeploymentFrom.
	names := agcWorkloadNames{
		app:              agcNameV2(ag),
		serviceAccount:   agcNameV2(ag),
		metricsTLSSecret: metricsTLSSecretNameV2(ag),
	}
	return buildAGCDeploymentFrom(ag.Namespace, names, v2GatewayLabels(ag), ag.Spec.GitHubAppSecretName(), proxyTLSSecret, agcImage, env, agcResources(ag.Spec.AGCResources))
}

// credentialEnvV2 returns the AGC environment that selects and configures the
// gateway's credential method (Q201). The discriminator CREDENTIAL_TYPE tells the
// AGC which path to take; for WorkloadIdentity it threads the non-secret signer
// configuration (App identity + Vault address/mounts/role + the projected-token
// path). It threads NO secret: the App key never exists for this method, and the
// projected ServiceAccount token reaches the AGC as a file (the volume stamped by
// buildAGCDeploymentFrom), never an env var. GitHubApp threads nothing — the AGC
// reads the mounted Secret files as before, and an absent CREDENTIAL_TYPE defaults
// to that path.
func credentialEnvV2(ag *gmcv2alpha1.ActionsGateway) []corev1.EnvVar {
	wi := ag.Spec.Credentials.WorkloadIdentity
	if ag.Spec.Credentials.Type != gmcv2alpha1.CredentialTypeWorkloadIdentity || wi == nil {
		return nil
	}
	env := []corev1.EnvVar{
		{Name: "CREDENTIAL_TYPE", Value: string(gmcv2alpha1.CredentialTypeWorkloadIdentity)},
		{Name: "GITHUB_APP_ID", Value: strconv.FormatInt(wi.AppID, 10)},
		{Name: "GITHUB_INSTALLATION_ID", Value: strconv.FormatInt(wi.InstallationID, 10)},
		{Name: "VAULT_SA_TOKEN_PATH", Value: vaultTokenMountDir + "/" + vaultTokenFile},
	}
	if v := wi.Signer.Vault; v != nil {
		env = append(env,
			corev1.EnvVar{Name: "VAULT_ADDR", Value: v.Address},
			corev1.EnvVar{Name: "VAULT_TRANSIT_KEY", Value: v.KeyName},
			corev1.EnvVar{Name: "VAULT_AUTH_ROLE", Value: v.Auth.Role},
		)
		// Mounts are optional in the spec (the AGC defaults them to Vault's
		// conventional paths); thread them only when set so the env stays minimal.
		if v.TransitMount != "" {
			env = append(env, corev1.EnvVar{Name: "VAULT_TRANSIT_MOUNT", Value: v.TransitMount})
		}
		if v.Auth.Mount != "" {
			env = append(env, corev1.EnvVar{Name: "VAULT_AUTH_MOUNT", Value: v.Auth.Mount})
		}
	}
	return env
}

// tracingEnvV2 translates the v2 spec.tracing into the OTEL_* environment the AGC
// reads. Mirrors v1's tracingEnv (the field set is identical); auth headers are
// deliberately not mapped (this project keeps secrets out of env vars).
func tracingEnvV2(t gmcv2alpha1.TracingConfig) []corev1.EnvVar {
	if t.Endpoint == "" {
		return nil
	}
	env := []corev1.EnvVar{{Name: "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", Value: t.Endpoint}}
	if t.Insecure != nil && *t.Insecure {
		env = append(env, corev1.EnvVar{Name: "OTEL_EXPORTER_OTLP_TRACES_INSECURE", Value: "true"})
	}
	if t.Sampler != "" {
		env = append(env, corev1.EnvVar{Name: "OTEL_TRACES_SAMPLER", Value: t.Sampler})
	}
	if t.SamplerArg != "" {
		env = append(env, corev1.EnvVar{Name: "OTEL_TRACES_SAMPLER_ARG", Value: t.SamplerArg})
	}
	if attrs := formatResourceAttributes(t.ResourceAttributes); attrs != "" {
		env = append(env, corev1.EnvVar{Name: "OTEL_RESOURCE_ATTRIBUTES", Value: attrs})
	}
	return env
}

// --- metrics mTLS PKI (v2) ---

// metricsServerSANsV2 lists the in-cluster DNS names the v2 metrics server cert
// must be valid for: this gateway's AGC Service (short + FQDN forms). v1 also
// covered the proxy Service, but in v2 the proxy is a standalone EgressProxy whose
// metrics listener is wired separately; the AGC owns this bundle for its own
// /metrics endpoint. agcName is the per-gateway AGC Service name.
func metricsServerSANsV2(namespace, agcName string) []string {
	return []string{
		agcName,
		fmt.Sprintf("%s.%s", agcName, namespace),
		fmt.Sprintf("%s.%s.svc", agcName, namespace),
		fmt.Sprintf("%s.%s.svc.cluster.local", agcName, namespace),
	}
}

// generateMetricsCertsV2 builds the per-tenant metrics mTLS bundle (CA + server
// leaf for the AGC metrics listener + client leaf for the scraper). Mirrors v1's
// generateMetricsCerts, reusing the shared signLeaf/encodeCertPEM helpers; the CA
// key is not persisted (the whole bundle regenerates together on renewal).
func generateMetricsCertsV2(namespace, agcName string) (*metricsCertBundle, error) {
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generate CA key: %w", err)
	}
	caSerial, err := randSerial()
	if err != nil {
		return nil, err
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          caSerial,
		Subject:               pkix.Name{CommonName: fmt.Sprintf("actions-gateway-metrics-ca.%s", namespace)},
		NotBefore:             time.Now().Add(-1 * time.Minute),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("create CA cert: %w", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, fmt.Errorf("parse CA cert: %w", err)
	}
	caPEM := encodeCertPEM(caDER)

	serverCertPEM, serverKeyPEM, err := signLeaf(caCert, caKey, &x509.Certificate{
		Subject:     pkix.Name{CommonName: fmt.Sprintf("%s.%s.svc", agcName, namespace)},
		DNSNames:    metricsServerSANsV2(namespace, agcName),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	if err != nil {
		return nil, fmt.Errorf("sign server cert: %w", err)
	}

	clientCertPEM, clientKeyPEM, err := signLeaf(caCert, caKey, &x509.Certificate{
		Subject:     pkix.Name{CommonName: metricsScraperCN},
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
	if err != nil {
		return nil, fmt.Errorf("sign client cert: %w", err)
	}

	return &metricsCertBundle{
		caPEM:         caPEM,
		serverCertPEM: serverCertPEM,
		serverKeyPEM:  serverKeyPEM,
		clientCertPEM: clientCertPEM,
		clientKeyPEM:  clientKeyPEM,
	}, nil
}

func buildMetricsTLSSecretV2(ag *gmcv2alpha1.ActionsGateway, b *metricsCertBundle) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: metricsTLSSecretNameV2(ag), Namespace: ag.Namespace, Labels: v2GatewayLabels(ag)},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       b.serverCertPEM,
			corev1.TLSPrivateKeyKey: b.serverKeyPEM,
			metricsCACertKey:        b.caPEM,
		},
	}
}

func buildMetricsClientSecretV2(ag *gmcv2alpha1.ActionsGateway, b *metricsCertBundle) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: metricsClientSecretNameV2(ag), Namespace: ag.Namespace, Labels: v2GatewayLabels(ag)},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       b.clientCertPEM,
			corev1.TLSPrivateKeyKey: b.clientKeyPEM,
			metricsCACertKey:        b.caPEM,
		},
	}
}
