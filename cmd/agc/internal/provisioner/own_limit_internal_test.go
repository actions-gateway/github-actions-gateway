package provisioner

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

// Q1177. The messages and conditions below are the kubelet's, captured verbatim from
// kind v1.35.5 (docs/design/04-operational-flows.md §4.2), not composed from its format
// strings: a fixture written from the format would pass against a discriminator that
// drifted from what the kubelet actually writes.
const (
	kubeletPodLimitMessage       = "Pod ephemeral local storage usage exceeds the total limit of containers 16Mi. "
	kubeletContainerLimitMessage = `Container w exceeded its local ephemeral storage limit "16Mi". `
	kubeletEmptyDirLimitMessage  = `Usage of EmptyDir volume "scratch" exceeds the limit "16Mi". `
	kubeletNodePressureMessage   = "The node was low on resource: memory. Threshold quantity: 6400Mi, available: 6181172Ki. Container w was using 1574540Ki, request is 32Mi, has larger consumption of memory. "
)

// limitEvicted marks pod as the kubelet does when it evicts it for exceeding its own
// limit: Failed/Evicted with the limit message and no DisruptionTarget condition.
func limitEvicted(pod *corev1.Pod, message string) *corev1.Pod {
	pod.Status.Phase = corev1.PodFailed
	pod.Status.Reason = podReasonEvicted
	pod.Status.Message = message
	return pod
}

// pressureEvicted marks pod as the kubelet's node-pressure path does: the same
// Failed/Evicted, plus the TerminationByKubelet condition carrying the same message.
func pressureEvicted(pod *corev1.Pod) *corev1.Pod {
	limitEvicted(pod, kubeletNodePressureMessage)
	pod.Status.Conditions = append(pod.Status.Conditions, corev1.PodCondition{
		Type:    corev1.DisruptionTarget,
		Status:  corev1.ConditionTrue,
		Reason:  corev1.PodReasonTerminationByKubelet,
		Message: kubeletNodePressureMessage,
	})
	return pod
}

func TestOwnLimitEviction(t *testing.T) {
	pod := func() *corev1.Pod { return scaleSetWorkerPod("w", nil) }
	cases := []struct {
		name string
		pod  *corev1.Pod
		own  bool
	}{
		{"pod total over its limit", limitEvicted(pod(), kubeletPodLimitMessage), true},
		{"one container over its limit", limitEvicted(pod(), kubeletContainerLimitMessage), true},
		{"emptyDir over its sizeLimit", limitEvicted(pod(), kubeletEmptyDirLimitMessage), true},
		{"node pressure", pressureEvicted(pod()), false},
		// The two signals must agree. A limit message under the pressure condition is
		// not a shape the kubelet writes; read it as the disruption, which keeps the
		// re-run, rather than as the tenant's fault, which loses it.
		{"limit message with the pressure condition", func() *corev1.Pod {
			p := pressureEvicted(pod())
			p.Status.Message = kubeletPodLimitMessage
			return p
		}(), false},
		// A kubelet that stamps no condition and writes an unrecognised message keeps
		// today's re-run: an unknown shape fails open.
		{"unrecognised message, no condition", limitEvicted(pod(), "The node had condition: [DiskPressure]. "), false},
		{"evicted with no message", evicted(pod()), false},
		{"limit message but not Failed/Evicted", func() *corev1.Pod {
			p := limitEvicted(pod(), kubeletPodLimitMessage)
			p.Status.Reason = ""
			return p
		}(), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ownLimitEviction(tc.pod)
			if tc.own {
				assert.Equal(t, tc.pod.Status.Message, got)
			} else {
				assert.Empty(t, got)
			}
		})
	}
}

// TestRecoverEvictedScaleSetWorkers_OwnLimitIsWithheld: a scale-set worker evicted for
// its own limit is claimed like any eviction, so a later reconcile skips it, but no
// budget slot is reserved and no re-run is requested.
func TestRecoverEvictedScaleSetWorkers_OwnLimitIsWithheld(t *testing.T) {
	ctx := context.Background()
	p, target, m, rerunCount, _ := recoveryFixture(t,
		limitEvicted(scaleSetWorkerPod("runner-gpu-job1", identityAnnotations()), kubeletPodLimitMessage))

	for i := 0; i < 3; i++ {
		done, err := p.RecoverEvictedScaleSetWorkers(ctx, target)
		require.NoError(t, err)
		<-done
	}

	assert.Zero(t, rerunCount.Load(), "an own-limit eviction must not be re-run")
	assert.Zero(t, testutil.ToFloat64(
		m.EvictionRetries.WithLabelValues("team-a", "gpu", evictionTierScaleSet, recoveryCauseEviction)),
		"no budget slot may be reserved for a re-run that is withheld")
	assert.Equal(t, float64(1), testutil.ToFloat64(
		m.EvictionRerunWithheld.WithLabelValues("team-a", "gpu", evictionTierScaleSet, recoveryCauseEviction, rerunWithheldReasonOwnLimit)),
		"repeated reconciles of one pod must withhold once")
	assert.Equal(t, []string{"EvictionRerunWithheld"}, target.events)
}

// TestRecoverEvictedScaleSetWorkers_NodePressureIsStillRerun is the inverse: the
// kubelet's real node-pressure shape keeps its recovery.
func TestRecoverEvictedScaleSetWorkers_NodePressureIsStillRerun(t *testing.T) {
	ctx := context.Background()
	p, target, m, rerunCount, _ := recoveryFixture(t,
		pressureEvicted(scaleSetWorkerPod("runner-gpu-job1", identityAnnotations())))

	done, err := p.RecoverEvictedScaleSetWorkers(ctx, target)
	require.NoError(t, err)
	<-done

	assert.Equal(t, int64(1), rerunCount.Load())
	assert.Zero(t, testutil.ToFloat64(
		m.EvictionRerunWithheld.WithLabelValues("team-a", "gpu", evictionTierScaleSet, recoveryCauseEviction, rerunWithheldReasonOwnLimit)))
}
