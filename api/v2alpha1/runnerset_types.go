package v2alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RunnerSetSpec is the desired state of a RunnerSet — the small scheduling/quota
// binder that replaces v1alpha1's RunnerGroup. The large PodTemplateSpec no longer
// lives here: it moves to a referenced RunnerTemplate / ClusterRunnerTemplate so a
// runner set stays a fixed-size object and a template is reused across many sets
// (docs/design/appendix-h-v2-api-decomposition.md §H.5). The scheduling and
// lifecycle fields are carried over from RunnerGroup unchanged in meaning.
//
// +kubebuilder:validation:XValidation:rule="!has(self.maxWorkers) || !has(self.priorityTiers) || self.priorityTiers.size() == 0 || self.maxWorkers == self.priorityTiers[self.priorityTiers.size()-1].threshold",message="maxWorkers must equal the last priorityTiers threshold when both are set"
// +kubebuilder:validation:XValidation:rule="!has(self.evictionRetryDelay) || duration(self.evictionRetryDelay) >= duration('1s')",message="evictionRetryDelay must be at least 1s"
// +kubebuilder:validation:XValidation:rule="!has(self.quotaRetryDelay) || duration(self.quotaRetryDelay) >= duration('1s')",message="quotaRetryDelay must be at least 1s"
// +kubebuilder:validation:XValidation:rule="!has(self.completedPodTTL) || duration(self.completedPodTTL) >= duration('0s')",message="completedPodTTL must not be negative"
// +kubebuilder:validation:XValidation:rule="!has(self.pendingPodDeadline) || duration(self.pendingPodDeadline) >= duration('1s')",message="pendingPodDeadline must be at least 1s"
type RunnerSetSpec struct {
	// GatewayRef names the ActionsGateway that supplies this runner set's GitHub
	// binding and control plane. Under multi-gateway-per-namespace each AGC
	// reconciles only the RunnerSets whose gatewayRef targets it — which is why
	// spec.gatewayRef.name is a CRD selectable field (KEP-4358), so that scoping
	// runs server-side (§H.7). Required: a runner set with no gateway has no GitHub
	// connection to register against. Resolved at runtime, not admission.
	GatewayRef ObjectRef `json:"gatewayRef"`

	// TemplateRef optionally names the RunnerTemplate (default) or ClusterRunnerTemplate
	// (set kind: ClusterRunnerTemplate) that supplies the worker pod shape. Unset means
	// inherit the gateway's defaultTemplateRef; both unset means the single cluster-default
	// ClusterRunnerTemplate (the one marked IsDefaultTemplateAnnotation). If none of the
	// three resolves the set fails closed Ready=False/TemplateNotFound — the AGC never
	// synthesizes a phantom worker pod without a pod shape (Q172, §H.4). This relaxes the
	// GA-era required templateRef to optional-with-a-default (a backward-compatible
	// required→optional change): a set that sets templateRef behaves exactly as before.
	// The referent is resolved at runtime; a set pointing at a not-yet-applied template
	// sits Ready=False/TemplateNotFound until it syncs (§H.7). status.templateSource
	// reports which rung resolved.
	//
	// +optional
	TemplateRef *ObjectRef `json:"templateRef,omitempty"`

	// ProxyRef optionally names the EgressProxy this runner set's traffic egresses
	// through. Unset means inherit the gateway's defaultProxyRef; both unset means
	// direct egress (still NetworkPolicy-restricted to DNS + GitHub) — a well-defined
	// behavior, so the dependency is simply droppable, which is why proxyRef is
	// optional where templateRef is required (§H.4, §H.10). Direct egress is reflected
	// in status as proxyMode=Direct plus an advisory EgressUnattributed condition; a
	// proxyRef/defaultProxyRef that names a *missing* proxy still fails closed
	// (ProxyNotFound), not direct egress (Q168).
	//
	// +optional
	ProxyRef *ObjectRef `json:"proxyRef,omitempty"`

	// MaxListeners is the maximum number of concurrent listener goroutines — a
	// concurrency ceiling with a permanent baseline of one poller; extra pollers
	// demand-spawn as jobs are acquired and idle-exit when the queue drains. The v2
	// default is 10 (v1 defaulted to 1): a higher ceiling costs nothing at idle and
	// avoids serializing job pickup, while maxWorkers and the namespace ResourceQuota
	// remain the binding resource guards (§H.15).
	//
	// +kubebuilder:default=10
	// +kubebuilder:validation:Minimum=1
	MaxListeners int32 `json:"maxListeners,omitempty"`

	// MaxWorkers caps the number of worker pods this RunnerSet may run concurrently.
	// A soft, in-process ceiling; pair it with a namespace ResourceQuota for a hard,
	// cluster-enforced limit.
	//
	// +optional
	// +kubebuilder:validation:Minimum=1
	MaxWorkers *int32 `json:"maxWorkers,omitempty"`

	// RunnerLabels is the label set matched against workflow runs-on values. At
	// least one label is required: an empty set would silently match every workflow
	// run. Each label must be non-empty and contain no whitespace or commas (comma
	// is the runs-on list separator).
	//
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:items:MaxLength=256
	// +kubebuilder:validation:items:Pattern=`^[^,\s]+$`
	RunnerLabels []string `json:"runnerLabels"`

	// PriorityTiers defines PriorityClass assignments and cumulative pod-count
	// thresholds. Tiers must be in strictly ascending threshold order.
	//
	// +optional
	// +kubebuilder:validation:MaxItems=10
	PriorityTiers []PriorityTier `json:"priorityTiers,omitempty"`

	// MaxEvictionRetries controls how many times the AGC automatically re-queues a
	// job whose worker pod was evicted. Set to 0 to disable auto-retry entirely.
	// Defaults to 2 when omitted.
	//
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=10
	MaxEvictionRetries *int32 `json:"maxEvictionRetries,omitempty"`

	// EvictionRetryDelay is the minimum time to wait before re-queuing an evicted
	// job. Must be at least 1s. Defaults to "5s" when omitted.
	//
	// +optional
	EvictionRetryDelay *metav1.Duration `json:"evictionRetryDelay,omitempty"`

	// MaxQuotaRetries controls how many times the AGC retries pod creation when the
	// namespace ResourceQuota is exhausted. Set to 0 to disable quota retry.
	// Defaults to 5 when omitted.
	//
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=20
	MaxQuotaRetries *int32 `json:"maxQuotaRetries,omitempty"`

	// QuotaRetryDelay is the time to wait between pod creation retries when the
	// namespace ResourceQuota is exhausted. Must be at least 1s. Defaults to "30s"
	// when omitted.
	//
	// +optional
	QuotaRetryDelay *metav1.Duration `json:"quotaRetryDelay,omitempty"`

	// CompletedPodTTL is how long a worker pod that has reached a terminal phase is
	// retained before the AGC deletes it. Set to "0s" to delete immediately on
	// completion. Must not be negative. Defaults to "5m" when omitted.
	//
	// +optional
	CompletedPodTTL *metav1.Duration `json:"completedPodTTL,omitempty"`

	// PendingPodDeadline is the maximum time a worker pod may remain Pending before
	// the AGC deletes it, releasing the concurrency-ceiling slot. Must be at least
	// 1s. Defaults to "10m" when omitted.
	//
	// +optional
	PendingPodDeadline *metav1.Duration `json:"pendingPodDeadline,omitempty"`
}

// RunnerSetStatus is the observed state of a RunnerSet. It follows the uniform v2
// status/condition contract (§H.7): a listType=map Conditions slice keyed on type,
// an ObservedGeneration, and a Ready condition with the shared reason vocabulary
// (see conditions.go). Reference-resolution failures surface as Ready=False with a
// specific reason (TemplateNotFound / ProxyNotFound / …) and a message naming the
// missing object.
type RunnerSetStatus struct {
	// Conditions are the observed conditions of the runner set. Known types: Ready.
	//
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ActiveSessions is the number of currently open long-poll sessions.
	//
	// +optional
	ActiveSessions int32 `json:"activeSessions,omitempty"`

	// ActiveJobs is the number of worker pods currently in the Running phase
	// (a job is actively executing). Derived from the worker pod phase count
	// during each reconcile; see also PendingJobs.
	//
	// +optional
	ActiveJobs int32 `json:"activeJobs,omitempty"`

	// PendingJobs is the number of worker pods currently in the Pending phase
	// (a job has been acquired and a pod spawned, but the pod is not yet
	// running — waiting on scheduling, image pull, or node readiness). Pods
	// that remain Pending past spec.pendingPodDeadline are deleted by the
	// controller; a sustained non-zero count warrants checking events and
	// scheduling constraints.
	//
	// +optional
	PendingJobs int32 `json:"pendingJobs,omitempty"`

	// ProxyMode records how this runner set's worker egress reaches GitHub:
	// "Proxied" (through the resolved EgressProxy, with stable per-tenant egress IPs)
	// or "Direct" (no proxyRef/defaultProxyRef, still NetworkPolicy-restricted to
	// GitHub + DNS but without per-tenant IP attribution). Explicit so "no proxy" is
	// an auditable state, not an inferred absence (§H.10). Paired with the advisory
	// EgressUnattributed condition when Direct.
	//
	// +optional
	// +kubebuilder:validation:Enum=Proxied;Direct
	ProxyMode string `json:"proxyMode,omitempty"`

	// TemplateSource records which rung of the template-resolution chain supplied this
	// runner set's worker pod shape (Q172, §H.4): "TemplateRef" (its own spec.templateRef),
	// "GatewayDefault" (the gateway's spec.defaultTemplateRef, inherited because templateRef
	// was unset), or "ClusterDefault" (the single cluster-default ClusterRunnerTemplate,
	// resolved because neither was set). Explicit so an operator can audit whether a set
	// runs on an explicit template or a default without inspecting the gateway and cluster
	// state. Empty until the references resolve.
	//
	// +optional
	// +kubebuilder:validation:Enum=TemplateRef;GatewayDefault;ClusterDefault
	TemplateSource string `json:"templateSource,omitempty"`

	// ObservedGeneration is the .metadata.generation the most recent reconcile acted on.
	//
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// RunnerSet is a namespace-scoped CRD reconciled by the AGC. It binds a worker pod
// shape (templateRef) and an optional egress proxy (proxyRef) to a GitHub gateway
// (gatewayRef), and carries the scheduling/quota knobs that were RunnerGroup's in
// v1alpha1. Worker pods are provisioned per acquired job and released on completion.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=rs,categories=actions-gateway
// +kubebuilder:selectablefield:JSONPath=`.spec.gatewayRef.name`
// +kubebuilder:printcolumn:name="Gateway",type=string,JSONPath=`.spec.gatewayRef.name`
// +kubebuilder:printcolumn:name="MaxListeners",type=integer,JSONPath=`.spec.maxListeners`
// +kubebuilder:printcolumn:name="ActiveSessions",type=integer,JSONPath=`.status.activeSessions`
// +kubebuilder:printcolumn:name="ActiveJobs",type=integer,JSONPath=`.status.activeJobs`
// +kubebuilder:printcolumn:name="PendingJobs",type=integer,JSONPath=`.status.pendingJobs`
// +kubebuilder:printcolumn:name="Egress",type=string,JSONPath=`.status.proxyMode`
// +kubebuilder:printcolumn:name="Template",type=string,priority=1,JSONPath=`.status.templateSource`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=='Ready')].status`
// +kubebuilder:printcolumn:name="Reason",type=string,priority=1,JSONPath=`.status.conditions[?(@.type=='Ready')].reason`
// +kubebuilder:printcolumn:name="ObservedGen",type=integer,priority=1,JSONPath=`.status.observedGeneration`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="self.metadata.name.size() <= 52",message="metadata.name must be at most 52 characters: v2 derives child resource names as <name>-<suffix> and reserves the remainder of the 63-char label/Service-name budget"
type RunnerSet struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RunnerSetSpec   `json:"spec,omitempty"`
	Status RunnerSetStatus `json:"status,omitempty"`
}

// RunnerSetList contains a list of RunnerSet.
//
// +kubebuilder:object:root=true
type RunnerSetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RunnerSet `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RunnerSet{}, &RunnerSetList{})
}
