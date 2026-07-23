// Package apisidecar holds the version-neutral worker-pod sidecar contract: the
// runner container's name, the operator's self-exiting-sidecar acknowledgment
// annotation, and the reap-blocking-sidecar heuristic that the AGC's RunnerSet
// reconciler and the GMC's RunnerTemplate webhook both evaluate.
//
// It lives in the neutral api module, outside any versioned package, because none of
// it is schema: the annotation key is metadata, and the heuristic reads a
// corev1.PodSpec that every version's RunnerTemplateSpec embeds unchanged. Declaring
// it once removes the drift class rather than detecting it — the version packages
// (api/v2alpha1, api/v2beta1) keep a thin RunnerTemplateSpec-typed wrapper so
// existing call sites are unaffected, but the logic lives here alone (Q374).
package apisidecar

import (
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// RunnerContainerName is the name of the worker pod's runner container — the one
// the AGC provisioner gap-fills and injects the job payload, egress-proxy env, and
// proxy-CA mount into (cmd/agc/internal/provisioner/provisioner.go: runnerContainer).
// The reap-blocking-sidecar heuristic uses it to tell the runner container apart
// from operator-authored sidecars, so the webhook and the AGC reconciler agree on
// which container is the runner. Its value must match the provisioner's constant.
const RunnerContainerName = "runner"

// SelfExitingSidecarsAnnotation names the worker-pod sidecar containers the operator
// asserts exit cleanly when the runner container finishes, so they never keep the
// worker pod alive past the job. Set it on a RunnerTemplate / ClusterRunnerTemplate
// as a comma-separated container-name list:
//
//	metadata:
//	  annotations:
//	    actions-gateway.com/self-exiting-sidecars: "metrics-agent,log-shipper"
//
// It is a name-list, not a boolean: a newly added, unacknowledged sidecar still
// warns, so a blanket opt-out cannot let the next footgun through silently. Naming a
// sidecar here silences all three reap-blocking-sidecar outlets (the admission
// warning, the RunnerSet PossibleReapBlockingSidecar condition, and the metric) for
// that sidecar only. It mirrors the allow-profile-downgrade / privileged-profile
// acknowledgment pattern: the operator must consciously assert "this one exits" (or
// convert it to a native sidecar) rather than disable the check wholesale.
const SelfExitingSidecarsAnnotation = "actions-gateway.com/self-exiting-sidecars"

// ReapBlocking returns the names (sorted) of regular sidecar containers in a worker
// pod spec that may prevent the pod from reaping when the runner container exits — a
// Kubernetes pod terminates only once every regular spec.containers[] entry has
// exited, so a regular sidecar that runs forever (e.g. a DinD dockerd) leaves the pod
// lingering and the AGC counting the runner slot as active (Q249, the Q247 stranding
// class).
//
// A container is reported when it is a regular spec.containers[] entry, is not the
// runner container, and is not named in the SelfExitingSidecarsAnnotation opt-out.
// Native sidecars — restartPolicy: Always init containers (KEP-753), which Kubernetes
// tears down when the main container exits — live in spec.initContainers[] and are
// therefore never reported: they do not block reaping. The check is a heuristic
// (nothing in a pod spec says a container "runs forever"), which is why every outlet
// gated on it is a non-blocking warning, never a rejection.
func ReapBlocking(podSpec *corev1.PodSpec, annotations map[string]string) []string {
	if podSpec == nil {
		return nil
	}
	acked := acknowledgedSelfExitingSidecars(annotations)
	var names []string
	for _, c := range podSpec.Containers {
		if c.Name == RunnerContainerName {
			continue
		}
		if _, ok := acked[c.Name]; ok {
			continue
		}
		names = append(names, c.Name)
	}
	sort.Strings(names)
	return names
}

// acknowledgedSelfExitingSidecars parses the SelfExitingSidecarsAnnotation into a set
// of container names the operator asserts exit cleanly. Entries are comma-separated;
// surrounding whitespace is trimmed and empty entries are dropped, so a trailing
// comma or spaced list is tolerated.
func acknowledgedSelfExitingSidecars(annotations map[string]string) map[string]struct{} {
	raw := annotations[SelfExitingSidecarsAnnotation]
	if raw == "" {
		return nil
	}
	acked := make(map[string]struct{})
	for _, name := range strings.Split(raw, ",") {
		if name = strings.TrimSpace(name); name != "" {
			acked[name] = struct{}{}
		}
	}
	return acked
}
