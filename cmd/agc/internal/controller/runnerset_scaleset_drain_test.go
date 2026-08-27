package controller

import (
	"context"
	"log/slog"
	"testing"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/actions-gateway/github-actions-gateway/scaleset"
	"github.com/actions-gateway/github-actions-gateway/scaleset/scalesettest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// A scale set admits one session, and the drain deletes it precisely so the next AGC
// process can open its own (Q222). Nothing else ever deletes it: GitHub holds it until
// its owner does, so a session opened after the drain outlives the process that opened
// it and locks every successor out.
//
// The reconciler keeps serving queued reconciles while the manager shuts down, and each
// one calls ensureScaleSetListener, so the ordering below is not contrived — it is what
// the integration suite hits about one run in four (Q968).
func TestStopListeners_ThenEnsure_DoesNotLeaveASessionBehind(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	const ns, label = "team-a", "linux-drain"
	scheme := runnerSetTestScheme(t)
	gw := gwObj("gw", ns, "")
	tmpl := tmplObj("tmpl", ns)
	rs := rsObj("set", ns, func(rs *v2alpha1.RunnerSet) {
		rs.Spec.AcquisitionProtocol = v2alpha1.AcquisitionProtocolScaleSet
		rs.Spec.RunnerLabels = []string{label}
	})
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(rs, gw, tmpl).WithStatusSubresource(rs).Build()

	r := &RunnerSetReconciler{
		Client:      c,
		Log:         slog.Default(),
		Provisioner: provisioner.NewProvisioner(c, nil, slog.Default()),
		ScaleSetClientFactory: func(*v2alpha1.RunnerSet, *v2alpha1.ActionsGateway) (*scaleset.Client, error) {
			return newReapScaleSetClient(t, srv), nil
		},
	}
	r.ensureMaps()

	ctx := context.Background()
	key := types.NamespacedName{Namespace: ns, Name: "set"}
	refs := &resolvedRefs{gateway: gw, template: &tmpl.Spec}

	_, err := r.ensureScaleSetListener(ctx, slog.Default(), key, rs, refs)
	require.NoError(t, err)
	ssID, ok := srv.ScaleSetIDByName(label)
	require.True(t, ok, "the listener must have registered its scale set")
	require.True(t, srv.HasActiveSession(ssID), "the listener must hold a session to drain")

	<-r.stopListeners()
	require.False(t, srv.HasActiveSession(ssID),
		"the drain deletes the session; without this the rest of the test proves nothing")

	// The reconcile that was already queued when shutdown began.
	_, _ = r.ensureScaleSetListener(ctx, slog.Default(), key, rs, refs)

	assert.False(t, srv.HasActiveSession(ssID),
		"a listener started after the drain leaks its session: this process will not delete it, and the successor cannot open its own")
}
