//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/actions-gateway/github-actions-gateway/scaleset/scalesettest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The GitHub runner group is the forge-side authorization point for which repositories
// may target a tenant's runners (Q712). These two specs are the end-to-end halves the
// unit tests cannot see: that a group declared on a CR actually reaches the scale-set
// registration through the reconciler, and that a group the installation does not have
// stops the set with a reason an operator can act on rather than quietly registering it
// into the default group.

// TestV2_RunnerSet_ScaleSet_GatewayDefaultRunnerGroupReachesGitHub proves the
// inheritance chain end to end: the group is declared once on the ActionsGateway, the
// RunnerSet declares nothing, and the scale set registered at GitHub carries the
// gateway's group.
func TestV2_RunnerSet_ScaleSet_GatewayDefaultRunnerGroupReachesGitHub(t *testing.T) {
	const ns = "v2-rs-runnergroup-gw"
	const label = "linux-rg-gw"
	createNSForAGC(t, ns)

	srv := scalesettest.New()
	t.Cleanup(srv.Close)
	srv.SetRunnerGroups(map[string]int{"tenant-a": 42})

	gw := newGatewayForSet("gw", ns, "")
	gw.Spec.DefaultRunnerGroup = "tenant-a"
	require.NoError(t, k8sClient.Create(ctx, gw))
	require.NoError(t, k8sClient.Create(ctx, newRunnerTemplate("tmpl", ns)))
	rs := newScaleSetRunnerSet("ss-set", ns, "gw", label, 3)
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), gw)
		_ = k8sClient.Delete(context.Background(), &v2alpha1.RunnerTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: ns},
		})
	})

	startRunnerSetReconcilerWithScaleSet(t, srv)

	var ssID int
	require.Eventually(t, func() bool {
		id, ok := srv.ScaleSetIDByName(label)
		ssID = id
		return ok
	}, 20*time.Second, 100*time.Millisecond, "the listener must register the scale set")

	waitForSetReadyReason(t, ns, "ss-set", metav1.ConditionTrue, v2alpha1.ReasonListenerActive)

	group, ok := srv.ScaleSetGroupID(ssID)
	require.True(t, ok)
	assert.Equal(t, 42, group,
		"the gateway's defaultRunnerGroup must reach the scale-set registration, not GitHub's default group")
}

// TestV2_RunnerSet_ScaleSet_UnknownRunnerGroupFailsClosed is the security half at the
// controller boundary: a set naming a group the installation does not have registers
// nothing and reports Ready=False/RunnerGroupNotFound, distinct from the generic
// session-failure reason so an operator can tell misconfiguration from an outage.
func TestV2_RunnerSet_ScaleSet_UnknownRunnerGroupFailsClosed(t *testing.T) {
	const ns = "v2-rs-runnergroup-missing"
	const label = "linux-rg-missing"
	createNSForAGC(t, ns)

	srv := scalesettest.New()
	t.Cleanup(srv.Close)
	srv.SetRunnerGroups(map[string]int{"tenant-a": 42})

	gw := newGatewayForSet("gw", ns, "")
	require.NoError(t, k8sClient.Create(ctx, gw))
	require.NoError(t, k8sClient.Create(ctx, newRunnerTemplate("tmpl", ns)))
	rs := newScaleSetRunnerSet("ss-set", ns, "gw", label, 3)
	rs.Spec.RunnerGroup = "tenant-typo"
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), gw)
		_ = k8sClient.Delete(context.Background(), &v2alpha1.RunnerTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: ns},
		})
	})

	startRunnerSetReconcilerWithScaleSet(t, srv)

	waitForSetReadyReason(t, ns, "ss-set", metav1.ConditionFalse, v2alpha1.ReasonRunnerGroupNotFound)

	_, registered := srv.ScaleSetIDByName(label)
	assert.False(t, registered,
		"a set that cannot reach its declared runner group must not register a scale set anywhere")
}
