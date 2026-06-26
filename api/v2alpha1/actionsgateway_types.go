package v2alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ActionsGatewaySpec is the desired state of a v2 ActionsGateway: the GitHub
// identity plus the AGC control plane only. v2 decomposes the v1alpha1 monolith —
// the inline proxy moves to a standalone EgressProxy and the bootstrap runner
// groups become explicit RunnerSet objects, both removed from this spec (§H.4).
// Multiple ActionsGateways are permitted per namespace in v2 (the singleton rule is
// dropped); the multi-gateway controller behavior lands in M3b.
//
// The Pod Security Admission level is NOT a field here. v1alpha1 hung securityProfile
// on this per-gateway object, but Pod Security Admission is a namespace-scoped control
// in Kubernetes, so under multi-gateway two gateways in one namespace would fight over
// the single namespace PSA label. v2 moves the profile to the namespace itself — the
// operator sets the actions-gateway.com/security-profile label on the tenant namespace
// and the GMC stamps the PSA labels from it (GMC-guarded, §H.16 #7). See
// docs/operations/security-operations.md.
type ActionsGatewaySpec struct {
	// Credentials configures how this gateway authenticates to GitHub. It is a
	// discriminated union keyed by credentials.type: exactly the member the discriminator
	// names is set (GitHubApp today; workload identity joins as an additive second member,
	// Q197). v2 nests the credential under this explicit-discriminator parent before the
	// v2beta1 freeze so adding an auth method never reshapes the spec again
	// (§H.15).
	Credentials GitHubCredentials `json:"credentials"`

	// GitHubURL is the GitHub organization, enterprise, or repository URL this
	// gateway's runners register against (e.g. "https://github.com/my-org"). It is
	// immutable: rebinding a running gateway's GitHub org is a footgun, so v2 freezes
	// it via a CEL transition rule (§H.15). Casing follows the v2 convention —
	// "github" is one lowercased word, the trailing initialism stays uppercase.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:Pattern=`^https://`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="githubURL is immutable; create a new ActionsGateway to bind a different GitHub org/enterprise/repo"
	GitHubURL string `json:"githubURL"`

	// DefaultProxyRef names an EgressProxy used for AGC control-plane egress and
	// inherited by RunnerSets under this gateway that do not set their own proxyRef.
	// Optional: unset means the control plane egresses directly (subject to
	// NetworkPolicy). Same-namespace unless the target EgressProxy grants
	// cross-namespace use (§H.4, §H.9).
	//
	// +optional
	DefaultProxyRef *LocalObjectRef `json:"defaultProxyRef,omitempty"`

	// DefaultTemplateRef names a RunnerTemplate (default) or ClusterRunnerTemplate
	// (set kind: ClusterRunnerTemplate) inherited as the worker pod shape by RunnerSets
	// under this gateway that set no spec.templateRef of their own (Q172). Optional: it
	// is the second rung of the template-resolution chain — an unset RunnerSet templateRef
	// resolves rs.templateRef → this defaultTemplateRef → the single cluster-default
	// ClusterRunnerTemplate → fail-closed TemplateNotFound (§H.4). Resolved at runtime in
	// the gateway's own namespace (for a RunnerTemplate); a ClusterRunnerTemplate referent
	// is cluster-scoped. A defaultTemplateRef that names a *missing* template fails the
	// inheriting set closed (TemplateNotFound), exactly like an explicit templateRef.
	//
	// +optional
	DefaultTemplateRef *ObjectRef `json:"defaultTemplateRef,omitempty"`

	// AGCResources tunes the CPU/memory requests and limits stamped on this
	// gateway's AGC control-plane container (Q171). It is an additive, per-key
	// override of the platform default — the documented Appendix A sizing of
	// requests {cpu: 500m, memory: 2Gi}, limits {cpu: 2, memory: 4Gi}. The GMC
	// starts from that default and overlays only the request/limit keys set
	// here, so a value that sets just one knob keeps the sensible default for
	// the others. Unset ⇒ the platform default unchanged. Changing it is a
	// rolling restart of the AGC, not a hot reload.
	//
	// Tune cautiously: the AGC is a single pod holding all listener-goroutine
	// state in memory. A memory limit below the AGC's working set OOMKills the
	// control plane, and a request larger than any node (or the namespace
	// ResourceQuota) leaves the AGC pod unschedulable (Pending). See
	// docs/operations/tenant-onboarding.md and
	// docs/design/appendix-e-capacity-planning.md for sizing guidance and the
	// recommended floor.
	//
	// +optional
	AGCResources *corev1.ResourceRequirements `json:"agcResources,omitempty"`

	// LogLevel controls the log verbosity of this tenant's AGC. Allowed values: info
	// (default), debug. Changing it is a rolling restart of the AGC, not a hot
	// reload. Use debug only for a bug repro.
	//
	// +optional
	// +kubebuilder:validation:Enum=info;debug
	// +kubebuilder:default=info
	LogLevel string `json:"logLevel,omitempty"`

	// Tracing configures opt-in OpenTelemetry distributed tracing for this tenant's
	// AGC. Tracing stays off unless tracing.endpoint is set.
	//
	// +optional
	Tracing TracingConfig `json:"tracing,omitempty"`
}

// CredentialType is the discriminator of the GitHubCredentials union: it names which
// authentication method a gateway uses. The member matching this value must be set and
// every other member absent. GitHubApp is the possession model (the App key lives in a
// namespace Secret); WorkloadIdentity is the delegation model (Q197) — no key in the
// cluster, an external signer signs the App JWT. WorkloadIdentity joined by extending
// this enum and adding a union member, a non-breaking addition — the whole reason the
// discriminated-union shape was fixed before the v2beta1 cut (§H.15).
//
// +kubebuilder:validation:Enum=GitHubApp;WorkloadIdentity
type CredentialType string

const (
	// CredentialTypeGitHubApp selects GitHub App authentication (the possession model):
	// the gateway holds the App's RSA private key in a namespace Secret named by
	// GitHubCredentials.GitHubApp.
	CredentialTypeGitHubApp CredentialType = "GitHubApp"

	// CredentialTypeWorkloadIdentity selects workload-identity authentication (the
	// delegation model, Q197): no App private key is held in the cluster. The AGC proves
	// its pod identity to an external trust anchor (Vault Kubernetes auth in the MVP) and
	// the anchor signs the App JWT via an external signer. Configured by
	// GitHubCredentials.WorkloadIdentity.
	CredentialTypeWorkloadIdentity CredentialType = "WorkloadIdentity"
)

// GitHubCredentials is the discriminated union of the ways a gateway authenticates to
// GitHub. Type is the explicit discriminator (k8s union convention): exactly the member
// it names is set. Today the only member is GitHubApp; workload identity joins as a
// second member without a breaking change (Q197) — the union shape exists so adding an
// auth method never reshapes the spec again after the v2beta1 freeze (§H.15).
// The "exactly the named member is set" invariant is enforced by
// CEL (the apiserver does not enforce native union semantics on CRDs): one per-member
// iff rule that each new member extends, never an N-way "exactly one of" that grows with
// the union.
//
// +kubebuilder:validation:XValidation:rule="has(self.githubApp) == (self.type == 'GitHubApp')",message="githubApp must be set when credentials.type is GitHubApp and unset otherwise"
// +kubebuilder:validation:XValidation:rule="has(self.workloadIdentity) == (self.type == 'WorkloadIdentity')",message="workloadIdentity must be set when credentials.type is WorkloadIdentity and unset otherwise"
type GitHubCredentials struct {
	// Type selects the active credential member (the union discriminator). The member it
	// names must be set and all others absent. It is required and explicit — an absent or
	// implicit discriminator would have to become required later, itself a breaking
	// change, so v2 fixes it before beta.
	//
	// +unionDiscriminator
	// +kubebuilder:validation:Required
	Type CredentialType `json:"type"`

	// GitHubApp configures GitHub App authentication: a name-only reference to the Secret
	// in this gateway's own namespace holding the App credentials ({appId, installationId,
	// privateKey}). Set iff Type is GitHubApp. The reference name is deliberately mutable —
	// changing it is the supported credential-rotation path.
	//
	// +optional
	GitHubApp *LocalSecretReference `json:"githubApp,omitempty"`

	// WorkloadIdentity configures workload-identity authentication (Q197): the App JWT is
	// signed by an external signer the AGC reaches by proving its pod identity, so no App
	// private key is ever held in the cluster (the delegation model). Set iff Type is
	// WorkloadIdentity. This is the on-strategy, no-PEM credential method; GitHubApp
	// remains the secure-by-default in-cluster option.
	//
	// +optional
	WorkloadIdentity *WorkloadIdentity `json:"workloadIdentity,omitempty"`
}

// WorkloadIdentity is the workload-identity credential member: the App's identity
// (appId/installationId, non-secret) plus an external signer that holds the App private
// key outside the cluster and signs the App JWT on the AGC's behalf. There is no
// privateKey field by design — the whole point of this method is that the key never
// enters the cluster (§H.15, docs/design/05-security.md).
type WorkloadIdentity struct {
	// AppID is the GitHub App's numeric ID (the JWT issuer). Non-secret; inline rather
	// than in a Secret because it identifies, not authenticates.
	//
	// +kubebuilder:validation:Minimum=1
	AppID int64 `json:"appId"`

	// InstallationID is the GitHub App installation's numeric ID. Non-secret; inline.
	//
	// +kubebuilder:validation:Minimum=1
	InstallationID int64 `json:"installationId"`

	// Signer configures the external signer that signs the App JWT. The signing key lives
	// in the signer's trust anchor (Vault transit in the MVP), never in the cluster.
	Signer ExternalSigner `json:"signer"`
}

// SignerProvider is the discriminator of the ExternalSigner union: it names the external
// signing backend. Vault (HashiCorp Vault transit) is the only provider in the MVP;
// cloud KMS backends (AWS/GCP/Azure) join as additive providers behind the same signer
// interface, by extending this enum and the union — no breaking change.
//
// +kubebuilder:validation:Enum=Vault
type SignerProvider string

const (
	// SignerProviderVault selects a HashiCorp Vault transit signer (the MVP). Configured
	// by ExternalSigner.Vault.
	SignerProviderVault SignerProvider = "Vault"
)

// ExternalSigner is the discriminated union of external signing backends. Provider is the
// explicit discriminator: exactly the member it names is set. The union exists so cloud
// KMS providers add as members without reshaping the spec — the same additive contract as
// the GitHubCredentials union above. The "exactly the named member is set" invariant is
// enforced by a per-provider CEL iff rule that each new provider extends.
//
// +kubebuilder:validation:XValidation:rule="has(self.vault) == (self.provider == 'Vault')",message="vault must be set when signer.provider is Vault and unset otherwise"
type ExternalSigner struct {
	// Provider selects the active signer backend (the union discriminator). The member it
	// names must be set and all others absent.
	//
	// +unionDiscriminator
	// +kubebuilder:validation:Required
	Provider SignerProvider `json:"provider"`

	// Vault configures the HashiCorp Vault transit signer. Set iff Provider is Vault.
	//
	// +optional
	Vault *VaultSigner `json:"vault,omitempty"`
}

// VaultSigner configures a HashiCorp Vault transit signer: the AGC authenticates to Vault
// with its pod identity (Vault Kubernetes auth) and asks Vault transit to sign the App
// JWT with a key Vault holds. No key or token is stored in the cluster — the AGC's
// projected ServiceAccount token is its only credential, and it is minted by the kubelet,
// not stored in a Secret.
type VaultSigner struct {
	// Address is the Vault API base URL (e.g. https://vault.vault.svc:8200). HTTPS is
	// required for production because the AGC's ServiceAccount token transits this channel
	// at login; a plaintext address is permitted only under an explicit dev/test opt-in in
	// the AGC (mirroring the GitHub token-exchange channel, docs/design/05-security.md).
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:Pattern=`^https?://`
	Address string `json:"address"`

	// TransitMount is the path the Vault transit secrets engine is mounted at. Optional;
	// defaults to "transit", Vault's conventional mount path.
	//
	// +optional
	// +kubebuilder:default=transit
	// +kubebuilder:validation:MaxLength=255
	TransitMount string `json:"transitMount,omitempty"`

	// KeyName is the name of the Vault transit key that signs the App JWT. The key must be
	// an RSA key (GitHub App keys are RSA); transit signs it as RS256
	// (pkcs1v15 + sha2-256).
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	KeyName string `json:"keyName"`

	// Auth configures how the AGC authenticates to Vault (Vault Kubernetes auth in the
	// MVP).
	Auth VaultKubernetesAuth `json:"auth"`

	// NetworkPolicy optionally tells the GMC how to reach Vault as a NetworkPolicy egress
	// peer (Q202). On a policy-enforcing CNI the per-tenant AGC NetworkPolicy default-denies
	// egress (DNS + GitHub + the kube API server); Vault is not otherwise expressible as a
	// peer because Address is an opaque URL, so set this and the GMC emits a scoped AGC→Vault
	// egress rule on the Vault API port (parsed from Address). Leave it unset on a non-
	// enforcing CNI (e.g. kindnet) or when the egress rule is managed out of band — the rule
	// is a strict tightening that is only ever added, never a broaden-to-all-egress. Set on a
	// GitHubApp gateway it has no effect (no Vault egress is emitted for the possession model).
	//
	// The value is a shared EgressPeer (Q204): the selector/CIDR peer the GMC scopes the rule
	// to. Its Port is left unset for Vault (the port is derived from Address); set it only to
	// override that derived port.
	//
	// +optional
	NetworkPolicy *EgressPeer `json:"networkPolicy,omitempty"`
}

// EgressPeer identifies a single destination the GMC may open in an otherwise default-deny
// per-tenant NetworkPolicy: a pod/namespace selector for an in-cluster peer, or a CIDR for an
// external one. Exactly one peer form must be given — a CIDR cannot carry a selector and vice
// versa — so the rule the GMC emits is always scoped to one peer, never a broaden-to-all-egress.
//
// This is the shared egress-peer descriptor that current and future NetworkPolicy egress holes
// reference, so the v2 API freezes one consistent shape rather than a per-feature near-duplicate
// (Q204). Today the Vault signer (VaultSigner.NetworkPolicy) is its only consumer; foreseen
// future consumers — cloud KMS signers, AGC telemetry endpoints — reuse it additively. See
// docs/design/appendix-g-future-enhancements.md §G.9.
//
// +kubebuilder:validation:XValidation:rule="(has(self.cidr) && size(self.cidr) > 0) != (has(self.podSelector) || has(self.namespaceSelector))",message="exactly one of cidr or a pod/namespace selector must be set"
type EgressPeer struct {
	// PodSelector selects the in-cluster peer pods the AGC may reach (e.g. {app.kubernetes.io/name:
	// vault}). Combined with NamespaceSelector when both are set, matching NetworkPolicy peer
	// semantics. Mutually exclusive with CIDR.
	//
	// +optional
	PodSelector *metav1.LabelSelector `json:"podSelector,omitempty"`

	// NamespaceSelector selects the namespace(s) the in-cluster peer runs in (e.g.
	// {kubernetes.io/metadata.name: vault}). Combined with PodSelector when both are set.
	// Mutually exclusive with CIDR.
	//
	// +optional
	NamespaceSelector *metav1.LabelSelector `json:"namespaceSelector,omitempty"`

	// CIDR is the IP block of an external (out-of-cluster) peer, e.g. "10.0.5.7/32". Mutually
	// exclusive with the pod/namespace selectors.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=43
	// +kubebuilder:validation:Pattern=`^([0-9]{1,3}\.){3}[0-9]{1,3}/[0-9]{1,2}$|^([0-9a-fA-F:]+)/[0-9]{1,3}$`
	CIDR string `json:"cidr,omitempty"`

	// Port optionally pins the destination port (1–65535) the egress rule permits. Leave it
	// unset for a peer whose port is derivable elsewhere — the Vault signer derives it from
	// VaultSigner.Address, so an unset Port preserves that behavior. Set it for peers whose
	// port is not otherwise derivable, or to override the derived port.
	//
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port *int32 `json:"port,omitempty"`
}

// VaultKubernetesAuth configures Vault Kubernetes auth: the AGC presents its projected
// ServiceAccount token, Vault verifies it against the cluster's token review API, and
// returns a short-lived Vault client token bound to Role. The pod-to-role binding is
// configured in Vault by the operator, out of band — only its name is named here.
type VaultKubernetesAuth struct {
	// Role is the Vault Kubernetes auth role the AGC logs in as. The operator binds this
	// role to the AGC ServiceAccount and namespace in Vault.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	Role string `json:"role"`

	// Mount is the path the Vault Kubernetes auth method is mounted at. Optional; defaults
	// to "kubernetes", Vault's conventional mount path.
	//
	// +optional
	// +kubebuilder:default=kubernetes
	// +kubebuilder:validation:MaxLength=255
	Mount string `json:"mount,omitempty"`
}

// GitHubAppSecretName returns the name of the Secret holding this gateway's GitHub App
// credentials, or "" when GitHub App authentication is not configured. Controllers use
// this single accessor so the union's nil-safety lives in one place: admission CEL
// guarantees githubApp is set whenever credentials.type is GitHubApp, but the in-memory
// fake clients some unit tests use bypass that, so the guard stays.
func (s *ActionsGatewaySpec) GitHubAppSecretName() string {
	if s.Credentials.GitHubApp == nil {
		return ""
	}
	return s.Credentials.GitHubApp.Name
}

// ActionsGatewayStatus is the observed state of an ActionsGateway, following the
// uniform v2 status/condition contract (§H.7).
type ActionsGatewayStatus struct {
	// Conditions are the observed conditions of the gateway. Known types: Ready,
	// AGCAvailable, CredentialUnavailable, Degraded.
	//
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ProxyMode records how this gateway's AGC control-plane egress reaches GitHub:
	// "Proxied" (through the EgressProxy named by defaultProxyRef, with stable
	// per-tenant egress IPs) or "Direct" (no defaultProxyRef, still NetworkPolicy-
	// restricted to GitHub + DNS + kube API but without per-tenant IP attribution).
	// Explicit so "no proxy" is an auditable state, not an inferred absence (§H.10).
	//
	// +optional
	// +kubebuilder:validation:Enum=Proxied;Direct
	ProxyMode string `json:"proxyMode,omitempty"`

	// ObservedGeneration is the .metadata.generation the most recent reconcile acted on.
	//
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// ActionsGateway is a namespace-scoped CRD reconciled by the GMC: it binds a GitHub
// identity and provisions the per-tenant AGC control plane. v2 permits multiple per
// namespace (multi-gateway support lands in M3b).
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=ag,categories=actions-gateway
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.spec.githubURL`
// +kubebuilder:printcolumn:name="Egress",type=string,JSONPath=`.status.proxyMode`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=='Ready')].status`
// +kubebuilder:printcolumn:name="Reason",type=string,priority=1,JSONPath=`.status.conditions[?(@.type=='Ready')].reason`
// +kubebuilder:printcolumn:name="ObservedGen",type=integer,priority=1,JSONPath=`.status.observedGeneration`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="self.metadata.name.size() <= 52",message="metadata.name must be at most 52 characters: the GMC derives child resource names as <name>-<suffix> and reserves the remainder of the 63-char label/Service-name budget"
type ActionsGateway struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ActionsGatewaySpec   `json:"spec,omitempty"`
	Status ActionsGatewayStatus `json:"status,omitempty"`
}

// ActionsGatewayList contains a list of ActionsGateway.
//
// +kubebuilder:object:root=true
type ActionsGatewayList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ActionsGateway `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ActionsGateway{}, &ActionsGatewayList{})
}
