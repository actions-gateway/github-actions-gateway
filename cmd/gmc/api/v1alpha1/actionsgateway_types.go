package v1alpha1

import (
	agcv1alpha1 "github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SecretReference is a pointer to a Kubernetes Secret.
//
// Cross-namespace references are not supported; Namespace must be left empty.
// The admission webhook enforces this at create/update time. A CEL XValidation
// rule is intentionally omitted: k8s ≤ 1.30 CEL cannot use has() on optional
// non-pointer string fields, so the rule would fail to install on those versions.
type SecretReference struct {
	Name string `json:"name"`
	// Namespace must be left empty. Cross-namespace Secret references are not
	// supported; the referenced Secret must reside in the ActionsGateway's own
	// namespace. This field is reserved for a future protocol extension.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// ProxyConfig configures the per-tenant egress proxy pool.
//
// +kubebuilder:validation:XValidation:rule="!has(self.minReplicas) || !has(self.maxReplicas) || self.minReplicas <= self.maxReplicas",message="minReplicas must not exceed maxReplicas"
type ProxyConfig struct {
	// +optional
	// +kubebuilder:default=2
	// +kubebuilder:validation:Minimum=1
	MinReplicas *int32 `json:"minReplicas,omitempty"`

	// +optional
	// +kubebuilder:default=10
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	MaxReplicas *int32 `json:"maxReplicas,omitempty"`

	// +optional
	// +kubebuilder:default=60
	TargetCPUUtilizationPercentage *int32 `json:"targetCPUUtilizationPercentage,omitempty"`

	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// NoProxyCIDRs lists destinations appended to the NO_PROXY environment
	// variable of the AGC and worker pods, excluding them from the per-tenant
	// egress proxy. Entries may be CIDR prefixes ("10.0.0.0/8"), bare IPs, or
	// NO_PROXY domain suffixes for internal destinations ("svc.cluster.local",
	// "internal.example.com"). The admission webhook rejects any entry that would
	// route the tenant's GitHub traffic around the proxy — a hostname matching
	// the configured gitHubURL host or the public GitHub domains (github.com,
	// githubusercontent.com, ghcr.io, …) — because that would silently defeat
	// per-tenant egress-IP attribution; never list GitHub here. A CIDR/IP that
	// happens to cover GitHub's (rotating) ranges is not detected and remains the
	// operator's responsibility. Cluster-internal destinations (the service CIDR,
	// localhost, svc.cluster.local) are appended automatically by the GMC and
	// need not be set here; override only to add a non-default service CIDR
	// (discover it via `kubectl cluster-info dump | grep -m1
	// service-cluster-ip-range`).
	// +optional
	NoProxyCIDRs []string `json:"noProxyCIDRs,omitempty"`

	// +optional
	// +kubebuilder:default=true
	ManagedNetworkPolicy *bool `json:"managedNetworkPolicy,omitempty"`
}

// TracingConfig configures opt-in OpenTelemetry distributed tracing for this
// tenant's AGC. Tracing stays off unless Endpoint is set. The GMC translates
// these fields into the standard OpenTelemetry OTEL_* environment variables on
// the AGC Deployment — the AGC reads only those (there is no bespoke flag
// surface). See docs/operations/observability.md.
//
// Authentication headers (OTEL_EXPORTER_OTLP_HEADERS) are intentionally not
// exposed here: they can carry bearer tokens, and this project keeps secrets
// out of environment variables (they leak into process listings and child
// processes). Authenticate the collector at the network layer instead — an
// in-cluster collector reached over the tenant's egress path, mutual TLS, or a
// service mesh.
type TracingConfig struct {
	// Endpoint is the OTLP/gRPC collector address (e.g.
	// "https://otel-collector.observability:4317"). Setting it enables tracing
	// on the AGC; leaving it empty keeps tracing off. Maps to
	// OTEL_EXPORTER_OTLP_TRACES_ENDPOINT.
	//
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// Insecure disables TLS for the OTLP/gRPC connection. Defaults to false (TLS
	// required) — set it true only for a plaintext in-cluster collector. Maps to
	// OTEL_EXPORTER_OTLP_TRACES_INSECURE.
	//
	// +optional
	Insecure *bool `json:"insecure,omitempty"`

	// Sampler selects the trace sampler. When empty the SDK default applies
	// (parentbased_always_on). Maps to OTEL_TRACES_SAMPLER. Ratio-based samplers
	// take their ratio from SamplerArg.
	//
	// +optional
	// +kubebuilder:validation:Enum=always_on;always_off;traceidratio;parentbased_always_on;parentbased_always_off;parentbased_traceidratio
	Sampler string `json:"sampler,omitempty"`

	// SamplerArg is the argument for the chosen Sampler — for the ratio-based
	// samplers it is the sampling probability in [0,1] (e.g. "0.1" for 10%).
	// Maps to OTEL_TRACES_SAMPLER_ARG.
	//
	// +optional
	SamplerArg string `json:"samplerArg,omitempty"`

	// ResourceAttributes are extra OpenTelemetry resource attributes merged onto
	// every AGC span (e.g. {"deployment.environment": "prod"}). Maps to
	// OTEL_RESOURCE_ATTRIBUTES. The AGC's own service.name/service.version
	// defaults take precedence over a service.name supplied here.
	//
	// +optional
	ResourceAttributes map[string]string `json:"resourceAttributes,omitempty"`
}

// AllowProfileDowngradeAnnotation is the annotation key that, when set to "true"
// on an ActionsGateway, permits an update that lowers spec.securityProfile to a
// less-restrictive level (e.g. restricted -> baseline). Without it, the GMC
// validating webhook rejects such downgrades so that a stray re-apply — or a
// manifest that drops the field and lets it re-default to baseline — cannot
// silently weaken a tenant's Pod Security Admission isolation. Relaxing
// isolation must be a deliberate, auditable act. See docs/design/05-security.md §5.3.
const AllowProfileDowngradeAnnotation = "actions-gateway.github.com/allow-profile-downgrade"

// PrivilegedProfileLabel is the namespace label that gates eligibility to run
// securityProfile: privileged. The GMC validating webhook rejects any
// ActionsGateway requesting securityProfile: privileged — at create OR update —
// unless its namespace carries this label set to PrivilegedProfileAllowed.
// Eligibility to run privileged workers is a platform decision: a platform
// administrator grants it by labelling the tenant namespace, exactly as they
// already apply the actions-gateway.github.com/tenant marker (see
// TenantNamespaceMarkerLabel). A tenant cannot self-grant it — the tenant owns
// the ActionsGateway CR but not its namespace's labels, and the
// namespace-psa-guard ValidatingAdmissionPolicy confines even the GMC to the PSA
// label keys. The gate is fail-closed: an absent label, or any value other than
// PrivilegedProfileAllowed, leaves privileged ineligible and the request is
// rejected. See docs/design/05-security.md §5.3.
const PrivilegedProfileLabel = "actions-gateway.github.com/privileged-profile"

// PrivilegedProfileAllowed is the only value of PrivilegedProfileLabel that
// grants privileged eligibility. Any other value — or an absent label — leaves
// the namespace ineligible for securityProfile: privileged (fail closed).
//
// It is the enum keyword "allowed" rather than a boolean-looking value
// deliberately: an unquoted `privileged-profile: true` is a YAML footgun (YAML
// 1.1 also coerces yes/no/on/off), and a Kubernetes label value must be a
// string — so a non-boolean enum keyword is both safer to author and
// self-documenting (its antonym is "denied"). The value is matched exactly, so
// the footgun cannot silently grant eligibility either way. See
// docs/development/kubernetes-conventions.md.
const PrivilegedProfileAllowed = "allowed"

// ActionsGatewaySpec is the desired state of an ActionsGateway.
//
// securityProfile changes are gated by the GMC validating webhook rather than a
// CRD CEL rule: an *upgrade* (e.g. baseline->restricted) is always allowed, but
// a *downgrade* is rejected unless the object carries
// AllowProfileDowngradeAnnotation set to "true". The webhook is used (not CEL)
// because the decision depends on metadata.annotations, which a spec-scoped CEL
// XValidation rule cannot read. gitHubAppRef.name is deliberately mutable —
// changing it is the supported credential-rotation mechanism, exercised by
// TestGMC_TenantProvisioning_CredentialRotation.
type ActionsGatewaySpec struct {
	GitHubAppRef SecretReference `json:"gitHubAppRef"`

	// GitHubURL is the GitHub organization, enterprise, or repository URL this
	// gateway's runners register against — e.g. "https://github.com/my-org",
	// "https://github.com/my-org/my-repo", or, for GitHub Enterprise Server,
	// "https://ghes.example.com/my-org". The AGC derives the runner-registration
	// REST endpoints from it (org-scoped vs repo-scoped) and pairs it with the
	// App credentials in gitHubAppRef, which must be installed on the same
	// org/enterprise. Required: a gateway with no URL has nothing to register
	// against. The GMC threads it to the AGC Deployment as the GITHUB_ORG_URL
	// environment variable — this is the production-supported replacement for the
	// testing-only --allow-agc-extra-env=AGC_EXTRA_GITHUB_ORG_URL injection path.
	//
	// Structural validation (https scheme, host present, an org/owner path
	// segment) is performed by the GMC validating webhook rather than a CRD CEL
	// rule so the error message can name the offending component; the Pattern
	// here is a cheap scheme guard that also applies if the webhook is bypassed.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:Pattern=`^https://`
	GitHubURL string `json:"gitHubURL"`

	// +optional
	Proxy ProxyConfig `json:"proxy,omitempty"`
	// RunnerGroups lists RunnerGroup specs bootstrapped in the tenant namespace.
	// +optional
	RunnerGroups []agcv1alpha1.RunnerGroupSpec `json:"runnerGroups,omitempty"`
	// The namespace ResourceQuota is platform-owned: the platform admin creates
	// and manages it (and any LimitRange) on the tenant namespace, and GAG
	// operates within it without ever creating or mutating it. There is
	// deliberately no tenant-authored quota field — a tenant-set quota is no real
	// cap (the tenant could raise it) and fights GitOps/platform ownership. See
	// docs/design/05-security.md and docs/operations/tenant-onboarding.md.

	// SecurityProfile controls the Pod Security Admission enforcement level
	// applied to the tenant namespace. Allowed values: baseline (default),
	// restricted, privileged. Use privileged only for workloads that run
	// Docker-in-Docker or require host-level capabilities.
	//
	// +optional
	// +kubebuilder:validation:Enum=baseline;restricted;privileged
	// +kubebuilder:default=baseline
	SecurityProfile string `json:"securityProfile,omitempty"`

	// LogLevel controls the log verbosity of this tenant's AGC and egress proxy.
	// Allowed values: info (default), debug. The GMC threads it to both
	// workloads as the LOG_LEVEL environment variable; changing it is a rolling
	// restart of the AGC and proxy (not a hot reload), so the new level takes
	// effect once the pods roll. Use debug only for a bug repro — at thousands of
	// concurrent sessions the per-session/per-job debug lines dominate log volume.
	// The default is info so a tenant never silently runs at debug verbosity.
	//
	// +optional
	// +kubebuilder:validation:Enum=info;debug
	// +kubebuilder:default=info
	LogLevel string `json:"logLevel,omitempty"`

	// Tracing configures opt-in OpenTelemetry distributed tracing for this
	// tenant's AGC. Tracing stays off unless tracing.endpoint is set.
	//
	// +optional
	Tracing TracingConfig `json:"tracing,omitempty"`
}

// ActionsGatewayStatus is the observed state of an ActionsGateway.
type ActionsGatewayStatus struct {
	// Conditions contains the current observed conditions of the gateway.
	// Known types: Ready, ProxyAvailable, AGCAvailable, CredentialUnavailable,
	// Degraded, ProxyQuotaPressure, ProxyQuotaExceeded, RunnerGroupsDegraded,
	// EgressRulesStale. The type and reason string constants are exported from this
	// package (see conditions.go).
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// +optional
	ProxyReadyReplicas int32 `json:"proxyReadyReplicas,omitempty"`
	// +optional
	ActiveSessions int32 `json:"activeSessions,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// ActionsGateway is a namespace-scoped CRD managed by the GMC.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=ag,categories=actions-gateway
// +kubebuilder:printcolumn:name="ProxyReady",type=integer,JSONPath=".status.proxyReadyReplicas"
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Reason",type=string,priority=1,JSONPath=".status.conditions[?(@.type=='Ready')].reason"
// +kubebuilder:printcolumn:name="ObservedGen",type=integer,priority=1,JSONPath=".status.observedGeneration"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"
type ActionsGateway struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ActionsGatewaySpec   `json:"spec,omitempty"`
	Status            ActionsGatewayStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ActionsGatewayList contains a list of ActionsGateway.
type ActionsGatewayList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ActionsGateway `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ActionsGateway{}, &ActionsGatewayList{})
}
