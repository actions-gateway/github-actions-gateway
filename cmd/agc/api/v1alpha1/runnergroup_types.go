package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PriorityTier maps a Kubernetes PriorityClass to a cumulative pod-count threshold.
// Thresholds must be in strictly ascending order.
type PriorityTier struct {
	// PriorityClassName is the name of an existing cluster-scoped PriorityClass.
	// +kubebuilder:validation:MaxLength=253
	PriorityClassName string `json:"priorityClassName"`

	// Threshold is the cumulative active-pod count at which this tier is exhausted.
	// +kubebuilder:validation:Minimum=1
	Threshold int32 `json:"threshold"`

	// PreemptionPolicy controls whether pods in this tier may evict lower-priority pods.
	// +kubebuilder:validation:Enum=PreemptLowerPriority;Never
	// +kubebuilder:validation:MaxLength=30
	// +kubebuilder:default=Never
	// +optional
	PreemptionPolicy string `json:"preemptionPolicy,omitempty"`
}

// RunnerGroupSpec defines the desired state of a RunnerGroup.
//
// +kubebuilder:validation:XValidation:rule="!has(self.maxWorkers) || !has(self.priorityTiers) || self.priorityTiers.size() == 0 || self.maxWorkers == self.priorityTiers[self.priorityTiers.size()-1].threshold",message="maxWorkers must equal the last priorityTiers threshold when both are set"
// +kubebuilder:validation:XValidation:rule="!has(self.evictionRetryDelay) || duration(self.evictionRetryDelay) >= duration('1s')",message="evictionRetryDelay must be at least 1s"
// +kubebuilder:validation:XValidation:rule="!has(self.quotaRetryDelay) || duration(self.quotaRetryDelay) >= duration('1s')",message="quotaRetryDelay must be at least 1s"
// +kubebuilder:validation:XValidation:rule="!has(self.completedPodTTL) || duration(self.completedPodTTL) >= duration('0s')",message="completedPodTTL must not be negative"
// +kubebuilder:validation:XValidation:rule="!has(self.pendingPodDeadline) || duration(self.pendingPodDeadline) >= duration('1s')",message="pendingPodDeadline must be at least 1s"
type RunnerGroupSpec struct {
	// MaxListeners is the maximum number of concurrent listener goroutines.
	// Each listener holds one open broker session; additional goroutines spawn
	// as jobs arrive (up to this ceiling) and shut down once the queue drains.
	// Raise this for high-throughput groups where multiple jobs arrive concurrently.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	MaxListeners int32 `json:"maxListeners,omitempty"`

	// MaxWorkers caps the number of worker pods this RunnerGroup may run concurrently.
	// This is a soft ceiling enforced in-process; pair it with a Kubernetes ResourceQuota
	// on the tenant namespace for a hard, cluster-enforced limit (D-6).
	// +optional
	// +kubebuilder:validation:Minimum=1
	MaxWorkers *int32 `json:"maxWorkers,omitempty"`

	// RunnerLabels is the label set matched against workflow runs-on values.
	// At least one label is required: an empty set would silently match every
	// workflow run. Each label must be non-empty and contain no whitespace or
	// commas (comma is the runs-on list separator).
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:items:MaxLength=256
	// +kubebuilder:validation:items:Pattern=`^[^,\s]+$`
	RunnerLabels []string `json:"runnerLabels"`

	// PriorityTiers defines PriorityClass assignments and cumulative pod-count thresholds.
	// Tiers must be in strictly ascending threshold order; the controller sets a Degraded
	// condition if they are not.
	// +optional
	// +kubebuilder:validation:MaxItems=10
	PriorityTiers []PriorityTier `json:"priorityTiers,omitempty"`

	// PodTemplate is the standard Kubernetes PodTemplateSpec for worker pods.
	// Privileged containers are rejected by the GMC admission webhook.
	PodTemplate corev1.PodTemplateSpec `json:"podTemplate"`

	// WorkerImage is the fully-qualified container image for the runner container.
	// +optional
	WorkerImage string `json:"workerImage,omitempty"`

	// MaxEvictionRetries controls how many times the AGC automatically re-queues a job
	// whose worker pod was evicted. Set to 0 to disable auto-retry entirely (useful for
	// GPU workloads where a failed job warrants manual inspection before rerunning).
	// Defaults to 2 when omitted.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=10
	MaxEvictionRetries *int32 `json:"maxEvictionRetries,omitempty"`

	// EvictionRetryDelay is the minimum time to wait before re-queuing an evicted job.
	// Must be at least 1s. Defaults to "5s" when omitted.
	// +optional
	EvictionRetryDelay *metav1.Duration `json:"evictionRetryDelay,omitempty"`

	// MaxQuotaRetries controls how many times the AGC retries pod creation when the
	// namespace ResourceQuota is exhausted. The AGC holds the job lock and waits for
	// quota to free up between attempts. Set to 0 to disable quota retry. Defaults to
	// 5 when omitted.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=20
	MaxQuotaRetries *int32 `json:"maxQuotaRetries,omitempty"`

	// QuotaRetryDelay is the time to wait between pod creation retries when the
	// namespace ResourceQuota is exhausted. Must be at least 1s. Defaults to "30s"
	// when omitted.
	// +optional
	QuotaRetryDelay *metav1.Duration `json:"quotaRetryDelay,omitempty"`

	// CompletedPodTTL is how long a worker pod that has reached a terminal phase
	// (Succeeded, Failed, or Unknown) is retained before the AGC deletes it.
	// Retention gives operators a window to inspect the pod of a failed job
	// (`kubectl logs`/`describe`) before it disappears; terminal pods consume no
	// compute and no ResourceQuota. Set to "0s" to delete worker pods immediately
	// on completion. Must not be negative. Defaults to "5m" when omitted.
	// +optional
	CompletedPodTTL *metav1.Duration `json:"completedPodTTL,omitempty"`

	// PendingPodDeadline is the maximum time a worker pod may remain Pending
	// (measured from its creation) before the AGC deletes it, releasing the
	// concurrency-ceiling slot the stuck pod was holding. Pending pods get stuck
	// on unpullable images or unschedulable constraints; deleting one resolves
	// its session goroutine and frees the listener for the next job. Each reap
	// emits a Warning Event (WorkerPodStuckPending) on the RunnerGroup. Raise
	// this on clusters where legitimate scheduling can be slow (e.g. autoscaled
	// GPU node pools). Must be at least 1s. Defaults to "10m" when omitted.
	// +optional
	PendingPodDeadline *metav1.Duration `json:"pendingPodDeadline,omitempty"`
}

// RunnerGroupStatus defines the observed state of a RunnerGroup.
type RunnerGroupStatus struct {
	// Conditions contains the current observed conditions of the runner group.
	// Known types: Ready, Degraded, RateLimited, RunnerVersionTooOld.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ActiveSessions is the number of currently open long-poll sessions.
	ActiveSessions int32 `json:"activeSessions"`

	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// RunnerGroup is a namespace-scoped CRD managed by the AGC. Each instance maps
// to an adaptive pool of listener goroutines backed by ephemeral worker pods.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=rg,categories=actions-gateway
// +kubebuilder:printcolumn:name="MaxListeners",type=integer,JSONPath=".spec.maxListeners"
// +kubebuilder:printcolumn:name="ActiveSessions",type=integer,JSONPath=".status.activeSessions"
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Reason",type=string,priority=1,JSONPath=".status.conditions[?(@.type=='Ready')].reason"
// +kubebuilder:printcolumn:name="ObservedGen",type=integer,priority=1,JSONPath=".status.observedGeneration"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"
type RunnerGroup struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RunnerGroupSpec   `json:"spec,omitempty"`
	Status RunnerGroupStatus `json:"status,omitempty"`
}

// RunnerGroupList contains a list of RunnerGroup.
// +kubebuilder:object:root=true
type RunnerGroupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RunnerGroup `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RunnerGroup{}, &RunnerGroupList{})
}
