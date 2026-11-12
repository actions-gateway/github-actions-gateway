package controller

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/actions-gateway/github-actions-gateway/api/apilabels"
)

// WorkerSeparation is how the GMC keeps the pods it builds (the AGC and the egress
// proxy) off nodes that run worker pods. A worker that escapes its container reaches
// whatever its node holds, so a controller sharing that node hands over its
// credentials. It is a platform-wide choice set by --controller-worker-separation, so
// no tenant-authored spec.scheduling block can remove the term.
type WorkerSeparation string

const (
	// WorkerSeparationRequired is the secure default: a required pod anti-affinity
	// against worker pods in every namespace, on the node topology. The scheduler
	// enforces a required term from both sides, so it also keeps a new worker off a
	// node where a controller already runs.
	WorkerSeparationRequired WorkerSeparation = "required"
	// WorkerSeparationNone stamps no term. For clusters with one schedulable node,
	// where the required term would leave the controllers or the workers Pending.
	WorkerSeparationNone WorkerSeparation = "none"
)

// ParseWorkerSeparation validates a --controller-worker-separation value. Empty
// resolves to required.
func ParseWorkerSeparation(s string) (WorkerSeparation, error) {
	switch WorkerSeparation(s) {
	case "", WorkerSeparationRequired:
		return WorkerSeparationRequired, nil
	case WorkerSeparationNone:
		return WorkerSeparationNone, nil
	default:
		return "", fmt.Errorf("invalid --controller-worker-separation %q: want one of required, none", s)
	}
}

// workerPodSelector matches the worker pods the AGC provisions and nothing else. The
// component label alone also matches the AGC pod, so the runner component narrows it.
// The AGC stamps both labels itself and never copies tenant template labels onto a
// worker (cmd/agc/internal/provisioner, buildPod).
func workerPodSelector() *metav1.LabelSelector {
	return &metav1.LabelSelector{MatchLabels: map[string]string{
		labelComponent:      componentWorkload,
		apilabels.Component: componentRunnerLabel,
	}}
}

// workerSeparationTerm keeps a pod off any node that runs a worker pod, whatever
// namespace the worker is in.
func workerSeparationTerm() corev1.PodAffinityTerm {
	return corev1.PodAffinityTerm{
		LabelSelector:     workerPodSelector(),
		NamespaceSelector: &metav1.LabelSelector{},
		TopologyKey:       corev1.LabelHostname,
	}
}

// applyWorkerSeparation appends the worker separation term to the pod's required
// anti-affinity, composing with any affinity already set. Zero-valued mode is
// required, so a reconciler built without the field stays secure.
func applyWorkerSeparation(pod *corev1.PodSpec, mode WorkerSeparation) {
	if mode == WorkerSeparationNone {
		return
	}
	if pod.Affinity == nil {
		pod.Affinity = &corev1.Affinity{}
	}
	if pod.Affinity.PodAntiAffinity == nil {
		pod.Affinity.PodAntiAffinity = &corev1.PodAntiAffinity{}
	}
	anti := pod.Affinity.PodAntiAffinity
	anti.RequiredDuringSchedulingIgnoredDuringExecution = append(anti.RequiredDuringSchedulingIgnoredDuringExecution, workerSeparationTerm())
}
