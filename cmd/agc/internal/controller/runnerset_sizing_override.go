package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
)

// The Throughput sizing profile's mechanism is the ABSENCE of a runner-container
// CPU limit: requests come from the measured history, no CPU limit is set, and jobs
// burst into idle node capacity. Anything that puts that limit back at admission
// cancels the profile and rejects nothing — the pod is admitted,
// status.sizingProfileState still reports Active, every other signal looks correct,
// and jobs simply stop bursting (Q489).
//
// This reports the EFFECT, not a cause. The obvious cause is a namespace LimitRange
// with a Container-type cpu default, and reading LimitRanges was the first design;
// it was wrong twice over. It infers an effect from a policy — and it sees only that
// one policy, while a mutating admission webhook, a policy engine's mutate rule, or a
// VPA in Auto mode injects the same limit just as silently and would go unreported.
//
// What the AGC actually has is better than either: it built the pod without a CPU
// limit and the apiserver admitted it with one. Comparing the two needs no new RBAC
// (worker pods are already granted, listed, and watched), no LimitRange informer, and
// no inference — a pod running a CPU limit the profile removed IS the failure, not
// evidence of it. The existing worker-pod watch makes it event-driven for free: the
// pod event that carries the injected limit is itself what re-reconciles the set.
//
// The one thing the pod cannot tell us is WHO injected the limit, so the message says
// what was observed and names the usual suspects rather than asserting a cause.

// applySizingProfileOverride upserts the SizingProfileOverridden condition by
// comparing what the Throughput profile built against what the apiserver admitted.
// It is evaluated only under Throughput — the one profile whose contract an
// admission mutation can quietly void, because it is the only one whose mechanism is
// the absence of a value — and the condition is REMOVED under any other profile, so
// switching to Binpack (which sets its own CPU limit, leaving nothing to inject) does
// not strand a True alarm.
//
// Only pods carrying provisioner.AnnotationSizingProfile == Throughput are examined:
// those are the ones this profile actually derived. A pod created moments before the
// operator selected Throughput is still legitimately running the template's CPU limit
// and must not read as an override.
//
// A pod list failure leaves the condition untouched rather than writing a verdict
// from evidence it does not have; the next reconcile retries.
func (r *RunnerSetReconciler) applySizingProfileOverride(ctx context.Context, rs *v2alpha1.RunnerSet, template *v2alpha1.RunnerTemplateSpec) {
	if rs.Spec.Sizing == nil || rs.Spec.Sizing.Profile != v2alpha1.SizingProfileThroughput {
		meta.RemoveStatusCondition(&rs.Status.Conditions, v2alpha1.ConditionSizingProfileOverridden)
		return
	}

	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(rs.Namespace),
		client.MatchingLabels{provisioner.LabelRunnerSet: rs.Name},
	); err != nil {
		return
	}

	observed, injected := scanForInjectedCPULimit(pods.Items, template)

	cond := metav1.Condition{
		Type:               v2alpha1.ConditionSizingProfileOverridden,
		Status:             metav1.ConditionFalse,
		Reason:             v2alpha1.ReasonAwaitingWorkerPods,
		Message:            "the Throughput profile has built no worker pod yet; nothing to compare against what the apiserver admits",
		ObservedGeneration: rs.Generation,
	}
	switch {
	case injected != nil:
		cond.Status = metav1.ConditionTrue
		cond.Reason = v2alpha1.ReasonCPULimitInjected
		cond.Message = fmt.Sprintf(
			"worker pod %s was built with no CPU limit on container %s and admitted with one (%s), so jobs are capped and the Throughput profile has no effect. "+
				"Something mutates the pod at admission — most often a namespace LimitRange with a Container-type cpu default, otherwise a mutating webhook or policy engine. "+
				"Check `kubectl get limitrange -n %s -o yaml` first; then drop the cpu default, or select Binpack (which sets its own limit)",
			injected.pod, injected.container, injected.cpu, rs.Namespace)
	case observed > 0:
		cond.Reason = v2alpha1.ReasonNoCPULimitInjected
		cond.Message = fmt.Sprintf(
			"%d profile-built worker pod(s) reached the kubelet as built, with no CPU limit on the profile's containers; jobs burst as Throughput intends", observed)
	}
	meta.SetStatusCondition(&rs.Status.Conditions, cond)
}

// injectedCPULimit names the first profile-built container found running a CPU limit
// the profile did not set.
type injectedCPULimit struct {
	pod       string
	container string
	cpu       string
}

// scanForInjectedCPULimit returns how many profile-built worker pods were examined
// and the first injected CPU limit among them, if any.
//
// Only containers the template declares are checked. The profile writes cpu/memory on
// exactly those, so a sidecar injected by the same admission chain that may have added
// the limit is not the profile's business and must not trip the condition. Pods being
// deleted are skipped: a pod on its way out cannot mislead an operator about what runs
// now, and its spec is frozen evidence of an older state.
func scanForInjectedCPULimit(pods []corev1.Pod, template *v2alpha1.RunnerTemplateSpec) (observed int, found *injectedCPULimit) {
	if template == nil {
		return 0, nil
	}
	built := make(map[string]struct{}, len(template.PodTemplate.Spec.Containers))
	for i := range template.PodTemplate.Spec.Containers {
		built[template.PodTemplate.Spec.Containers[i].Name] = struct{}{}
	}

	for i := range pods {
		pod := &pods[i]
		if !pod.DeletionTimestamp.IsZero() {
			continue
		}
		if pod.Annotations[provisioner.AnnotationSizingProfile] != v2alpha1.SizingProfileThroughput {
			continue
		}
		observed++
		if found != nil {
			continue
		}
		for j := range pod.Spec.Containers {
			c := &pod.Spec.Containers[j]
			if _, ok := built[c.Name]; !ok {
				continue
			}
			if q, ok := c.Resources.Limits[corev1.ResourceCPU]; ok {
				found = &injectedCPULimit{pod: pod.Name, container: c.Name, cpu: q.String()}
				break
			}
		}
	}
	return observed, found
}
