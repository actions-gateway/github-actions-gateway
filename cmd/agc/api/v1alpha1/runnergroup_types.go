package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PriorityTier maps a Kubernetes PriorityClass to a cumulative pod-count threshold.
// Thresholds must be in strictly ascending order.
type PriorityTier struct {
	// PriorityClassName is the name of an existing cluster-scoped PriorityClass.
	PriorityClassName string `json:"priorityClassName"`

	// Threshold is the cumulative active-pod count at which this tier is exhausted.
	// +kubebuilder:validation:Minimum=1
	Threshold int32 `json:"threshold"`

	// PreemptionPolicy controls whether pods in this tier may evict lower-priority pods.
	// +kubebuilder:validation:Enum=PreemptLowerPriority;Never
	// +kubebuilder:default=Never
	// +optional
	PreemptionPolicy string `json:"preemptionPolicy,omitempty"`
}

// RunnerGroupSpec defines the desired state of a RunnerGroup.
//
// +kubebuilder:validation:XValidation:rule="!has(self.maxWorkers) || self.priorityTiers.size() == 0 || self.maxWorkers == self.priorityTiers[self.priorityTiers.size()-1].threshold",message="maxWorkers must equal the last priorityTiers threshold when both are set"
type RunnerGroupSpec struct {
	// MaxListeners is the maximum number of concurrent listener goroutines.
	// The AGC always keeps at least one listener running; additional goroutines
	// spawn as jobs arrive and shut down once the queue drains.
	// +kubebuilder:default=10
	// +kubebuilder:validation:Minimum=1
	MaxListeners int32 `json:"maxListeners,omitempty"`

	// MaxWorkers caps the number of worker pods this RunnerGroup may run concurrently.
	// +optional
	// +kubebuilder:validation:Minimum=1
	MaxWorkers *int32 `json:"maxWorkers,omitempty"`

	// RunnerLabels is the label set matched against workflow runs-on values.
	RunnerLabels []string `json:"runnerLabels"`

	// PriorityTiers defines PriorityClass assignments and cumulative pod-count thresholds.
	// +optional
	// +kubebuilder:validation:MaxItems=10
	// +kubebuilder:validation:XValidation:rule="self.size() <= 1 || (self.size() < 2 || self[0].threshold < self[1].threshold) && (self.size() < 3 || self[1].threshold < self[2].threshold) && (self.size() < 4 || self[2].threshold < self[3].threshold) && (self.size() < 5 || self[3].threshold < self[4].threshold) && (self.size() < 6 || self[4].threshold < self[5].threshold) && (self.size() < 7 || self[5].threshold < self[6].threshold) && (self.size() < 8 || self[6].threshold < self[7].threshold) && (self.size() < 9 || self[7].threshold < self[8].threshold) && (self.size() < 10 || self[8].threshold < self[9].threshold)",message="priorityTiers must be in strictly ascending threshold order"
	PriorityTiers []PriorityTier `json:"priorityTiers,omitempty"`

	// PodTemplate is the standard Kubernetes PodTemplateSpec for worker pods.
	PodTemplate corev1.PodTemplateSpec `json:"podTemplate"`

	// WorkerImage is the fully-qualified container image for the runner container.
	// +optional
	WorkerImage string `json:"workerImage,omitempty"`
}

// RunnerGroupStatus defines the observed state of a RunnerGroup.
type RunnerGroupStatus struct {
	// Conditions contains the current observed conditions of the runner group.
	// Known types: Ready, Degraded, RateLimited, RunnerVersionTooOld.
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
// +kubebuilder:resource:scope=Namespaced,shortName=rg
// +kubebuilder:printcolumn:name="MaxListeners",type=integer,JSONPath=".spec.maxListeners"
// +kubebuilder:printcolumn:name="ActiveSessions",type=integer,JSONPath=".status.activeSessions"
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].status"
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
