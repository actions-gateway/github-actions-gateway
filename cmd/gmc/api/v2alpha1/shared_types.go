package v2alpha1

// Domain-prefixed identifiers for v2. All v2 label, annotation, and finalizer keys
// live on the actions-gateway.com domain — the group the project owns — from birth,
// rather than the v1alpha1 actions-gateway.github.com domain
// (docs/design/appendix-h-v2-api-decomposition.md §H.12, §H.15). The grandfathered
// boolean-looking "true" values are also aligned to self-documenting enum keywords
// here (tenant: managed, allow-profile-downgrade: allowed), matching the existing
// privileged-profile: allowed precedent. During v1/v2 coexistence the consumers
// (VAPs, the downgrade webhook) dual-read both domains and both values; the dual-read
// window closes at v1alpha1 removal (§H.12). The migration tool (M5) relabels live
// objects in one pass.
const (
	// TenantNamespaceMarkerLabel marks a namespace as a managed GAG tenant. The GMC
	// confinement ValidatingAdmissionPolicies key on this marker (not on resource
	// names), so it scales to multiple gateways per namespace unchanged.
	TenantNamespaceMarkerLabel = "actions-gateway.com/tenant"
	// TenantNamespaceMarkerValue is the value of TenantNamespaceMarkerLabel (Q147:
	// the enum keyword "managed" replaces v1's boolean-looking "true").
	TenantNamespaceMarkerValue = "managed"

	// AllowProfileDowngradeAnnotation, when set to AllowProfileDowngradeAllowed on a
	// tenant namespace, permits an update that lowers the namespace
	// SecurityProfileLabel to a less-restrictive level. v2 relocates the security
	// profile onto the namespace (§H.16 #7), so the downgrade opt-in moves there too:
	// the gmc-namespace-security-profile-guard ValidatingAdmissionPolicy rejects such
	// downgrades without it, so a stray re-apply cannot silently weaken a tenant's Pod
	// Security Admission isolation. See docs/design/05-security.md §5.3.
	AllowProfileDowngradeAnnotation = "actions-gateway.com/allow-profile-downgrade"
	// AllowProfileDowngradeAllowed is the only value of AllowProfileDowngradeAnnotation
	// that permits a downgrade (Q147: "allowed" replaces v1's "true").
	AllowProfileDowngradeAllowed = "allowed"

	// PrivilegedProfileLabel is the namespace label that gates eligibility to select
	// the privileged security profile. Only a platform administrator may apply it; a
	// tenant cannot self-grant it. The gate is fail-closed: an absent label, or any
	// value other than PrivilegedProfileAllowed, leaves privileged ineligible.
	PrivilegedProfileLabel = "actions-gateway.com/privileged-profile"
	// PrivilegedProfileAllowed is the only value of PrivilegedProfileLabel that
	// grants privileged eligibility (matched exactly; fail-closed otherwise).
	PrivilegedProfileAllowed = "allowed"

	// SecurityProfileLabel is the namespace label that selects the Pod Security
	// Admission enforcement level the GMC stamps on the tenant namespace. v2 moves the
	// security profile off the per-gateway ActionsGateway.spec (where v1 hung it) and
	// onto the namespace, because Pod Security Admission is a namespace-scoped control
	// (§H.16 #7): co-located gateways therefore always share one posture, and tenants
	// that need different postures use different namespaces — the natural PSA isolation
	// boundary. The operator sets this label; the GMC reconciles it into the six
	// pod-security.kubernetes.io/* labels (NamespacePSAReconciler), GMC-guarded against
	// silent downgrades and privileged self-grant by the namespace validating webhook.
	// Absent on a managed tenant namespace ⇒ SecurityProfileBaseline (secure default).
	SecurityProfileLabel = "actions-gateway.com/security-profile"
)

// Pod Security Admission profile values selectable via SecurityProfileLabel. These
// mirror the upstream Pod Security Standards levels and the v1alpha1
// ActionsGateway.spec.securityProfile enum exactly, so v2 is no weaker than v1.
const (
	// SecurityProfileBaseline is the default: a minimally restrictive policy that
	// prevents known privilege escalations.
	SecurityProfileBaseline = "baseline"
	// SecurityProfileRestricted is the most restrictive policy, following current
	// pod-hardening best practices.
	SecurityProfileRestricted = "restricted"
	// SecurityProfilePrivileged is the least restrictive (unrestricted) policy; it is
	// gated behind PrivilegedProfileLabel eligibility and is for workloads that run
	// Docker-in-Docker or require host-level capabilities.
	SecurityProfilePrivileged = "privileged"
)

// SecurityProfileRank orders the Pod Security Admission profiles from least to most
// restrictive. A downgrade is any change that lowers the rank. It is the v2 home of
// v1's webhook-local securityProfileRank, now keyed once per namespace (§H.16 #7).
var SecurityProfileRank = map[string]int{
	SecurityProfilePrivileged: 0,
	SecurityProfileBaseline:   1,
	SecurityProfileRestricted: 2,
}

// EffectiveSecurityProfile returns the profile, substituting the baseline default for
// an empty value so an absent SecurityProfileLabel maps to what the GMC actually
// stamps. Matches v1's effectiveProfile semantics.
func EffectiveSecurityProfile(profile string) string {
	if profile == "" {
		return SecurityProfileBaseline
	}
	return profile
}

// ActionsGatewayFinalizer is set by the GMC on an ActionsGateway so its
// per-gateway children (the AGC Deployment/SA/Role, NetworkPolicies) are cleaned up
// before the CR is removed. On the actions-gateway.com domain from birth. The
// reconciler that sets it lands in a later milestone (M3a); the key is fixed here.
// Shared/referenced data kinds (EgressProxy, RunnerTemplate) deliberately carry no
// finalizer — deletion degrades referrers rather than blocking (§H.8).
const ActionsGatewayFinalizer = "actions-gateway.com/gmc-cleanup"

// LocalSecretReference is a name-only reference to a Kubernetes Secret in the same
// namespace. v2 drops v1alpha1's SecretReference.namespace field: it was
// reserved-but-validated-empty and read like a cross-namespace reference that does
// not exist — a confused-deputy footgun. The referenced Secret must reside in the
// referrer's own namespace (§H.15).
type LocalSecretReference struct {
	// Name of the referenced Secret in the same namespace.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// ObjectRef is a name-only reference to another v2 object in the same namespace,
// except where the referent kind grants cross-namespace use. v2 references share
// this one shape for a uniform API surface (§H.6); referential integrity is a
// runtime condition, not an admission gate (§H.7).
type ObjectRef struct {
	// Name of the referenced object. Bounded by the v2 52-char name budget (§H.6).
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=52
	Name string `json:"name"`
}

// ProxySharing controls cross-namespace reference to an EgressProxy. Consent is
// always provider-side: the proxy owner publishes which namespaces may reference it
// (§H.9). A nil ProxySharing means same-namespace only (the default, secure). v2
// ships the inline allowlist only; ReferenceGrant support is additive later.
type ProxySharing struct {
	// AllowedNamespaces lists namespaces permitted to reference this proxy. A
	// consumer-side name alone never authorizes a cross-namespace reference.
	//
	// +optional
	// +listType=set
	AllowedNamespaces []string `json:"allowedNamespaces,omitempty"`
}

// TracingConfig configures opt-in OpenTelemetry distributed tracing for this
// tenant's AGC. Tracing stays off unless Endpoint is set. Unchanged from v1alpha1.
//
// Authentication headers are intentionally not exposed: they can carry bearer
// tokens, and this project keeps secrets out of environment variables. Authenticate
// the collector at the network layer instead. See docs/operations/observability.md.
type TracingConfig struct {
	// Endpoint is the OTLP/gRPC collector address. Setting it enables tracing on the
	// AGC; leaving it empty keeps tracing off. Maps to OTEL_EXPORTER_OTLP_TRACES_ENDPOINT.
	//
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// Insecure disables TLS for the OTLP/gRPC connection. Defaults to false (TLS
	// required). Maps to OTEL_EXPORTER_OTLP_TRACES_INSECURE.
	//
	// +optional
	Insecure *bool `json:"insecure,omitempty"`

	// Sampler selects the trace sampler. When empty the SDK default applies. Maps to
	// OTEL_TRACES_SAMPLER.
	//
	// +optional
	// +kubebuilder:validation:Enum=always_on;always_off;traceidratio;parentbased_always_on;parentbased_always_off;parentbased_traceidratio
	Sampler string `json:"sampler,omitempty"`

	// SamplerArg is the argument for the chosen Sampler — for ratio-based samplers
	// the sampling probability in [0,1]. Maps to OTEL_TRACES_SAMPLER_ARG.
	//
	// +optional
	SamplerArg string `json:"samplerArg,omitempty"`

	// ResourceAttributes are extra OpenTelemetry resource attributes merged onto
	// every AGC span. Maps to OTEL_RESOURCE_ATTRIBUTES.
	//
	// +optional
	ResourceAttributes map[string]string `json:"resourceAttributes,omitempty"`
}
