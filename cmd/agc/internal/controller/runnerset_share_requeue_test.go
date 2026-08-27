package controller

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/actions-gateway/github-actions-gateway/scaleset"
	"github.com/actions-gateway/github-actions-gateway/scaleset/scalesettest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// The revoke direction has no other detector. A withdrawn grant deletes a ConfigMap
// the AGC cannot watch, so the only thing that fails the set closed is this requeue
// riding out of a *resolved* reconcile — and the integration revoke leg is satisfied
// by an incidental listener wake, so it stays green with the fold removed (Q999).
//
// Asserted on Reconcile's returned Result rather than on a condition flipping,
// because the Result is the whole mechanism: nothing else on this path can produce
// the cadence, so it cannot pass for the wrong reason.
//
// The ScaleSet tier is what makes this reachable without a manager: its route returns
// before the installation-token step, so no broker or registrar scaffolding is needed.
// It pins the ScaleSet fold site; the classic tier's fold is the same call, one line
// apart, and is not pinned by this or anything else.
func TestRunnerSetReconcile_ShareResolvedSetKeepsRecheckingTheGrant(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	const ns, label, proxyNS, proxyName = "team-a", "linux-share", "platform-egress", "shared"
	const recheck = 42 * time.Second

	scheme := runnerSetTestScheme(t)
	gw := gwObj("gw", ns, "")
	tmpl := tmplObj("tmpl", ns)
	rs := rsObj("set", ns, func(rs *v2alpha1.RunnerSet) {
		rs.Finalizers = []string{runnerSetFinalizer} // skip the finalizer-add requeue
		rs.Spec.AcquisitionProtocol = v2alpha1.AcquisitionProtocolScaleSet
		rs.Spec.RunnerLabels = []string{label}
		rs.Spec.ProxyRef = &v2alpha1.ProxyObjectRef{Name: proxyName, Namespace: proxyNS}
	})
	// What the GMC projects into a granted consumer namespace; its presence is the grant.
	share := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      proxyShareConfigMapName(proxyNS, proxyName),
			Namespace: ns,
			Labels:    map[string]string{"actions-gateway/proxy-share": "true"},
		},
		Data: map[string]string{
			"ca.crt":     "-----BEGIN CERTIFICATE-----\nstub\n-----END CERTIFICATE-----\n",
			"proxy-host": proxyName + "-proxy." + proxyNS + ".svc.cluster.local",
			"proxy-port": "8080",
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(rs, gw, tmpl, share).WithStatusSubresource(rs).Build()

	p := provisioner.NewProvisioner(c, nil, slog.Default())
	p.APIReader = c
	r := &RunnerSetReconciler{
		Client:      c,
		Log:         slog.Default(),
		Provisioner: p,
		ScaleSetClientFactory: func(*v2alpha1.RunnerSet, *v2alpha1.ActionsGateway) (*scaleset.Client, error) {
			return newReapScaleSetClient(t, srv), nil
		},
		ProxyShareRecheckInterval: recheck,
	}
	r.ensureMaps()
	t.Cleanup(func() { <-r.stopListeners() })

	res, err := r.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "set"}})
	require.NoError(t, err)

	var got v2alpha1.RunnerSet
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Namespace: ns, Name: "set"}, &got))
	require.Equal(t, v2alpha1.ProxyModeProxied, got.Status.ProxyMode,
		"the set must have resolved through the projection, or this asserts nothing")

	assert.Equal(t, recheck, res.RequeueAfter,
		"a set whose egress resolved through a projection must keep re-reading it; nothing else fails it closed when the grant is withdrawn")
}
