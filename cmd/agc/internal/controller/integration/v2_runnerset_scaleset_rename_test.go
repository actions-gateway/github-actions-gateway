//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/api/v2"
	"github.com/actions-gateway/github-actions-gateway/scaleset/scalesettest"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
)

// TestV2_RunnerSet_ScaleSet_FirstLabelChangeMovesTheSession pins Q1325's second lead.
// The first runnerLabel is the scale-set name, and admission releases the old name's
// claim the moment the edit is stored, so another tenant in the same GitHub scope may
// then claim it. A listener left on the old scale set keeps its session there and
// takes that tenant's jobs. The edit has to move the session: the old set's session is
// deleted and the new name is registered and listened on.
func TestV2_RunnerSet_ScaleSet_FirstLabelChangeMovesTheSession(t *testing.T) {
	const ns = "v2-rs-ssname-rename"
	const (
		oldName = "linux-rename-old"
		newName = "linux-rename-new"
	)
	createNSForAGC(t, ns)

	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	gw := newGatewayForSet("gw", ns, "")
	require.NoError(t, k8sClient.Create(ctx, gw))
	require.NoError(t, k8sClient.Create(ctx, newRunnerTemplate("tmpl", ns)))
	rs := newScaleSetRunnerSet("ss-set", ns, "gw", oldName, 3)
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), rs)
		_ = k8sClient.Delete(context.Background(), gw)
		_ = k8sClient.Delete(context.Background(), &v2.RunnerTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: ns},
		})
	})

	startRunnerSetReconcilerWithScaleSet(t, srv)

	var oldID int
	require.Eventually(t, func() bool {
		id, ok := srv.ScaleSetIDByName(oldName)
		oldID = id
		return ok && srv.HasActiveSession(id)
	}, 20*time.Second, 100*time.Millisecond, "the listener must register and session on the first name")
	waitForSetReadyReason(t, ns, "ss-set", metav1.ConditionTrue, v2.ReasonListenerActive)

	key := types.NamespacedName{Namespace: ns, Name: "ss-set"}
	require.NoError(t, retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var cur v2.RunnerSet
		if err := k8sClient.Get(ctx, key, &cur); err != nil {
			return err
		}
		cur.Spec.RunnerLabels = []string{newName}
		return k8sClient.Update(ctx, &cur)
	}))

	require.Eventually(t, func() bool {
		id, ok := srv.ScaleSetIDByName(newName)
		return ok && srv.HasActiveSession(id)
	}, 20*time.Second, 100*time.Millisecond,
		"a first-label edit must register the new scale-set name and open its session there")
	require.Eventually(t, func() bool { return !srv.HasActiveSession(oldID) },
		10*time.Second, 100*time.Millisecond,
		"the old scale set's session must be released, or the listener keeps taking jobs on a name admission has released")
}
