package provisioner

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// createPodWithQuotaRetry attempts to create pod, retrying up to maxRetries times
// when the namespace ResourceQuota is exhausted. Other errors are returned immediately.
// target is used to record an owner-scoped Event when the retry budget is exhausted.
func (p *Provisioner) createPodWithQuotaRetry(ctx context.Context, target Target, pod *corev1.Pod, maxRetries int, retryDelay time.Duration, log *slog.Logger) error {
	key := target.Key()
	for attempt := 0; ; attempt++ {
		err := p.Client.Create(ctx, pod)
		if err == nil {
			return nil
		}
		// Non-quota errors are never retried. Surface them on the owner object:
		// a worker pod the apiserver refuses is invisible at GitHub, which reports
		// only that the runner "lost communication" and points the operator at
		// networking (Q467). The apiserver's own message — an invalid name, a
		// rejecting admission webhook, a denied security profile — is the shortest
		// path to the real cause. An AlreadyExists is a replayed delivery finding
		// its own pod, which is success on the v2 path, so it is not reported.
		if !isQuotaError(err) {
			if !apierrors.IsAlreadyExists(err) {
				log.Warn("apiserver rejected worker pod", "pod", pod.Name, "error", err)
				target.RecordEvent(corev1.EventTypeWarning, "WorkerPodCreateFailed", "ProvisionWorker",
					fmt.Sprintf("the apiserver rejected worker pod %s: %v", pod.Name, err))
			}
			return err
		}
		// maxRetries==0 means quota retry is disabled; return immediately without
		// counting as "exhausted" (disabled is a policy choice, not a budget failure).
		if maxRetries == 0 || attempt >= maxRetries {
			if maxRetries > 0 {
				log.Warn("quota retry budget exhausted; abandoning pod creation",
					"pod", pod.Name, "attempts", attempt+1)
				if p.Metrics != nil {
					p.Metrics.QuotaRetriesExhausted.WithLabelValues(key.Namespace, key.Name).Inc()
				}
				target.RecordEvent(corev1.EventTypeWarning, "QuotaRetriesExhausted", "ProvisionWorker",
					fmt.Sprintf("worker pod creation abandoned after exhausting the namespace ResourceQuota retry budget (%d retries); raise the namespace ResourceQuota or lower the worker concurrency ceiling", maxRetries))
			}
			return err
		}
		log.Info("pod creation blocked by namespace quota; retrying",
			"pod", pod.Name, "attempt", attempt+1, "maxRetries", maxRetries, "delay", retryDelay)
		if p.Metrics != nil {
			p.Metrics.QuotaRetries.WithLabelValues(key.Namespace, key.Name).Inc()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(retryDelay):
		}
	}
}

// isQuotaError reports whether err is a Kubernetes API error caused by a namespace
// ResourceQuota being exceeded. Quota errors are Forbidden (403) and their message
// contains "exceeded quota".
func isQuotaError(err error) bool {
	return apierrors.IsForbidden(err) && strings.Contains(err.Error(), "exceeded quota")
}

// activePodCount returns the number of Running or Pending worker pods matching
// the owner's pod-identity label selector.
func (p *Provisioner) activePodCount(ctx context.Context, namespace string, selector map[string]string) (int32, error) {
	var podList corev1.PodList
	if err := p.Client.List(ctx, &podList,
		client.InNamespace(namespace),
		client.MatchingLabels(selector),
	); err != nil {
		return 0, err
	}
	var count int32
	for _, pod := range podList.Items {
		if pod.Status.Phase == corev1.PodRunning || pod.Status.Phase == corev1.PodPending {
			count++
		}
	}
	return count, nil
}

// ceilingCheck returns the PriorityClassName to assign (may be "") and whether
// the pod should be held due to a concurrency ceiling, from the resolved spec.
func ceilingCheck(spec *ResolvedSpec, activePods int32) (priorityClass string, held bool) {
	if len(spec.PriorityTiers) > 0 {
		for _, tier := range spec.PriorityTiers {
			if activePods < tier.Threshold {
				return tier.PriorityClassName, false
			}
		}
		// All tiers exhausted.
		return "", true
	}
	if spec.MaxWorkers != nil && activePods >= *spec.MaxWorkers {
		return "", true
	}
	return "", false
}
