//go:build integration

package integration_test

import (
	"context"
	"strings"
	"testing"
	"time"

	agcv1alpha1 "github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	"github.com/actions-gateway/github-actions-gateway/api/apinames"
	"github.com/onsi/gomega"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestGMC_DerivedRunnerGroupNameFitsALabelValue lets a real API server judge the
// bound on the derived RunnerGroup name.
//
// The unit tests assert the name against apimachinery's label-value rules; this one
// asserts the thing that actually broke — the AGC stamps the derived name as
// actions-gateway/runner-group on every worker pod and agent Secret, and before the
// bound a 15-character gateway with a 40-character runner label produced a
// 64-character name. The RunnerGroup CR was created happily (253 is the object-name
// limit), and then every worker pod create failed on the label value, so the tenant
// ran no jobs while GitHub reported only that the runner had lost communication.
func TestGMC_DerivedRunnerGroupNameFitsALabelValue(t *testing.T) {
	const nsName = "team-name-budget"
	createNamespace(t, nsName)
	createGitHubAppSecret(t, nsName, "github-app")

	// 15 characters, and a 40-character runner label: the shortest gateway name that
	// overran the label-value limit before the bound.
	gwName := strings.Repeat("g", 15)
	runnerLabel := strings.Repeat("l", 40)

	ag := newActionsGateway(gwName, nsName, "github-app")
	ag.Spec.RunnerGroups = []agcv1alpha1.RunnerGroupSpec{{
		MaxListeners: 1,
		RunnerLabels: []string{runnerLabel},
		PodTemplate: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "runner", Image: "runner:test"}},
			},
		},
	}}
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ag) })

	startGMCReconciler(t, nil)

	g := gomega.NewWithT(t)
	var derived string
	g.Eventually(func() bool {
		var rgs agcv1alpha1.RunnerGroupList
		if err := k8sClient.List(ctx, &rgs, client.InNamespace(nsName)); err != nil {
			return false
		}
		if len(rgs.Items) != 1 {
			return false
		}
		derived = rgs.Items[0].Name
		return true
	}, 15*time.Second, 25*time.Millisecond).Should(gomega.BeTrue(),
		"the gateway's inline runnerGroups entry should materialize one RunnerGroup")

	assert.LessOrEqual(t, len(derived), apinames.MaxLabelValue,
		"derived RunnerGroup name %q must fit a label value", derived)

	// The assertion that matters: the API server accepts a pod carrying this name as
	// the owner label — the create the AGC makes for every job.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "worker-label-budget-probe",
			Namespace: nsName,
			Labels:    map[string]string{"actions-gateway/runner-group": derived},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "runner", Image: "runner:test"}},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, pod),
		"a worker pod labelled with the derived RunnerGroup name must be accepted")
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), pod) })
}
