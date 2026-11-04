//go:build integration

package integration_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/actions-gateway/github-actions-gateway/api/v2"
	"github.com/actions-gateway/github-actions-gateway/scaleset/scalesettest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// scaleSetWorkerPods lists the provisioned worker pods ("runner-*") of the named set.
func scaleSetWorkerPods(t *testing.T, ns, setName string) []corev1.Pod {
	t.Helper()
	var pods corev1.PodList
	if err := k8sClient.List(ctx, &pods, client.InNamespace(ns),
		client.MatchingLabels{provisioner.LabelRunnerSet: setName}); err != nil {
		return nil
	}
	var workers []corev1.Pod
	for _, p := range pods.Items {
		if strings.HasPrefix(p.Name, "runner-") {
			workers = append(workers, p)
		}
	}
	return workers
}

// startScaleSetFixture applies a direct-egress gateway, a RunnerTemplate and the given
// ScaleSet RunnerSet, starts the reconciler against a fresh scale-set fake, and returns
// the fake and the registered scale set's ID.
func startScaleSetFixture(t *testing.T, ns string, tmpl *v2.RunnerTemplate, rs *v2.RunnerSet) (*scalesettest.Server, int) {
	t.Helper()
	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	require.NoError(t, k8sClient.Create(ctx, newGatewayForSet("gw", ns, "")))
	require.NoError(t, k8sClient.Create(ctx, tmpl))
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), rs)
		_ = k8sClient.Delete(context.Background(), tmpl)
		_ = k8sClient.Delete(context.Background(), &v2.ActionsGateway{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: ns}})
	})

	startRunnerSetReconcilerWithScaleSet(t, srv)

	label := rs.Spec.RunnerLabels[0]
	var ssID int
	require.Eventually(t, func() bool {
		id, ok := srv.ScaleSetIDByName(label)
		ssID = id
		return ok
	}, 20*time.Second, 100*time.Millisecond, "the listener must register a scale set named %q", label)
	waitForSetReadyReason(t, ns, rs.Name, metav1.ConditionTrue, v2.ReasonListenerActive)
	return srv, ssID
}

// TestV2_RunnerSet_ScaleSet_TemplateEditTakesEffectWithoutRestart is the Q117
// regression on the RunnerSet: a running listener must build the next worker from an
// edited RunnerTemplate, without restarting the AGC. NodeSelector is the
// discriminator: the provisioner copies the template's pod spec verbatim and never
// rewrites NodeSelector, so it reflects whichever template version was read at
// pod-build time.
func TestV2_RunnerSet_ScaleSet_TemplateEditTakesEffectWithoutRestart(t *testing.T) {
	const ns = "v2-rs-tmpl-edit"
	createNSForAGC(t, ns)

	tmpl := newRunnerTemplate("tmpl", ns)
	tmpl.Spec.PodTemplate.Spec.NodeSelector = map[string]string{"q117-template": "original"}
	srv, ssID := startScaleSetFixture(t, ns, tmpl, newScaleSetRunnerSet("tmpl-edit", ns, "gw", "linux", 20))

	srv.EnqueueJob(ssID)
	var first corev1.Pod
	require.Eventually(t, func() bool {
		pods := scaleSetWorkerPods(t, ns, "tmpl-edit")
		if len(pods) == 0 {
			return false
		}
		first = pods[0]
		return true
	}, 20*time.Second, 50*time.Millisecond, "a worker pod must be provisioned for the first job")
	assert.Equal(t, "original", first.Spec.NodeSelector["q117-template"], "the first worker uses the original template")

	require.NoError(t, retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var fetched v2.RunnerTemplate
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(tmpl), &fetched); err != nil {
			return err
		}
		fetched.Spec.PodTemplate.Spec.NodeSelector = map[string]string{"q117-template": "edited"}
		return k8sClient.Update(ctx, &fetched)
	}))

	// The provisioner reads the template through the manager's cache, so a job assigned
	// before the cache sees the edit legitimately builds from the original. Keep
	// assigning jobs until one lands after it: a listener holding a start-time snapshot
	// never produces an edited worker, however many jobs it takes.
	require.Eventually(t, func() bool {
		for _, p := range scaleSetWorkerPods(t, ns, "tmpl-edit") {
			if p.Spec.NodeSelector["q117-template"] == "edited" {
				return true
			}
		}
		srv.EnqueueJob(ssID)
		return false
	}, 20*time.Second, 500*time.Millisecond,
		"a worker built after the edit must use the edited template without an AGC restart")
}

// TestV2_RunnerSet_ScaleSet_PriorityTiers proves the provisioner assigns each worker
// the PriorityClass of the tier its cumulative active-pod count falls in: the first
// two workers sit in the threshold-2 tier.
func TestV2_RunnerSet_ScaleSet_PriorityTiers(t *testing.T) {
	const ns = "v2-rs-priority"
	createNSForAGC(t, ns)

	for _, pc := range []struct {
		name  string
		value int32
	}{
		{"critical-test", 1000},
		{"standard-test", 100},
	} {
		pcObj := &schedulingv1.PriorityClass{ObjectMeta: metav1.ObjectMeta{Name: pc.name}, Value: pc.value}
		if err := k8sClient.Create(ctx, pcObj); err != nil && !apierrors.IsAlreadyExists(err) {
			require.NoError(t, err, "create PriorityClass %q", pc.name)
		}
		t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), pcObj) })
	}

	rs := newScaleSetRunnerSet("priority-set", ns, "gw", "linux", 5)
	rs.Spec.PriorityTiers = []v2.PriorityTier{
		{PriorityClassName: "critical-test", Threshold: 2},
		{PriorityClassName: "standard-test", Threshold: 5},
	}
	srv, ssID := startScaleSetFixture(t, ns, newRunnerTemplate("tmpl", ns), rs)

	srv.EnqueueJob(ssID)
	srv.EnqueueJob(ssID)
	var workers []corev1.Pod
	require.Eventually(t, func() bool {
		workers = scaleSetWorkerPods(t, ns, "priority-set")
		return len(workers) >= 2
	}, 20*time.Second, 50*time.Millisecond, "two worker pods must be provisioned")

	for _, p := range workers {
		assert.Equal(t, "critical-test", p.Spec.PriorityClassName,
			"workers 1 and 2 must take the threshold-2 tier's PriorityClass")
	}
}
