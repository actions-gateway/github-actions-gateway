package controller

import (
	"context"
	"log/slog"
	"testing"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/actions-gateway/github-actions-gateway/scaleset"
	"github.com/actions-gateway/github-actions-gateway/scaleset/scalesettest"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// egressAuditProxy is an EgressProxy whose spec.auditLogging is mode.
func egressAuditProxy(name, ns, mode string) *v2alpha1.EgressProxy {
	return &v2alpha1.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       v2alpha1.EgressProxySpec{AuditLogging: mode},
	}
}

// reconcileEgressAuditSets runs one Reconcile per named set and returns the client.
// The ScaleSet tier returns before the installation-token step, so no broker
// scaffolding is needed to reach the egress-status writes.
func reconcileEgressAuditSets(t *testing.T, objs []client.Object, sets ...*v2alpha1.RunnerSet) client.Client {
	t.Helper()
	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	all := append([]client.Object{}, objs...)
	for _, rs := range sets {
		rs.Finalizers = []string{runnerSetFinalizer}
		rs.Spec.AcquisitionProtocol = v2alpha1.AcquisitionProtocolScaleSet
		rs.Spec.RunnerLabels = []string{"egress-audit-" + rs.Name}
		all = append(all, rs)
	}
	c := fake.NewClientBuilder().WithScheme(runnerSetTestScheme(t)).
		WithObjects(all...).WithStatusSubresource(&v2alpha1.RunnerSet{}).Build()

	p := provisioner.NewProvisioner(c, nil, slog.Default())
	p.APIReader = c
	r := &RunnerSetReconciler{
		Client:      c,
		Log:         slog.Default(),
		Provisioner: p,
		ScaleSetClientFactory: func(*v2alpha1.RunnerSet, *v2alpha1.ActionsGateway) (*scaleset.Client, error) {
			return newReapScaleSetClient(t, srv), nil
		},
	}
	r.ensureMaps()
	t.Cleanup(func() { <-r.stopListeners() })

	for _, rs := range sets {
		_, err := r.Reconcile(context.Background(),
			ctrl.Request{NamespacedName: types.NamespacedName{Namespace: rs.Namespace, Name: rs.Name}})
		require.NoError(t, err, "reconcile %s", rs.Name)
	}
	return c
}

func egressAuditCondition(t *testing.T, c client.Client, ns, name string) *metav1.Condition {
	t.Helper()
	var got v2alpha1.RunnerSet
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &got))
	cond := meta.FindStatusCondition(got.Status.Conditions, v2alpha1.ConditionEgressAuditUnattributed)
	require.NotNil(t, cond, "set %s carries no %s condition", name, v2alpha1.ConditionEgressAuditUnattributed)
	return cond
}

// egressAuditSeries gathers the gauge and returns value by runner_set, plus each
// set's reason label.
func egressAuditSeries(t *testing.T, c client.Client) (map[string]float64, map[string]string) {
	t.Helper()
	reg := prometheus.NewRegistry()
	require.NoError(t, reg.Register(NewRunnerSetEgressAuditCollector(c)))
	families, err := reg.Gather()
	require.NoError(t, err)
	values, reasons := map[string]float64{}, map[string]string{}
	for _, mf := range families {
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			set := labels["runner_set"]
			values[set] = m.GetGauge().GetValue()
			reasons[set] = labels["reason"]
		}
	}
	return values, reasons
}

// The false 0 is the direction Q1062 exists to prevent: a gateway whose
// defaultProxyRef pool logs ConnectionsWithSource reads joined, while a set that
// overrides proxyRef to a pool with the source half off sends its workers' traffic
// through records nothing can join. The sibling set, which inherits the default, is
// the control: it shows both halves really are on for the default pool, so the only
// thing separating the two verdicts is the set's own proxyRef.
func TestRunnerSetEgressAudit_SetProxyRefOverridesDefault(t *testing.T) {
	const ns = "team-a"
	gw := gwObj("gw", ns, "default-pool")
	gw.Spec.AuditLogging = string(provisioner.WorkerAuditAddresses)
	overriding := rsObj("overriding", ns, func(rs *v2alpha1.RunnerSet) {
		rs.Spec.ProxyRef = &v2alpha1.ProxyObjectRef{Name: "dedicated"}
	})
	inheriting := rsObj("inheriting", ns, nil)

	c := reconcileEgressAuditSets(t, []client.Object{
		gw, tmplObj("tmpl", ns),
		egressAuditProxy("default-pool", ns, proxyAuditConnectionsWithSource),
		egressAuditProxy("dedicated", ns, "Off"),
	}, overriding, inheriting)

	control := egressAuditCondition(t, c, ns, "inheriting")
	require.Equal(t, v2alpha1.ReasonEgressAuditJoined, control.Reason,
		"the control set must read joined off the default pool, or the override case proves nothing")
	assert.Equal(t, metav1.ConditionFalse, control.Status)

	cond := egressAuditCondition(t, c, ns, "overriding")
	assert.Equal(t, metav1.ConditionTrue, cond.Status,
		"a set egressing through a pool that does not log ConnectionsWithSource must not read joined")
	assert.Equal(t, v2alpha1.ReasonProxySourceAuditDisabled, cond.Reason)
	assert.Contains(t, cond.Message, `"dedicated"`, "the message must name the pool the set actually uses")

	values, reasons := egressAuditSeries(t, c)
	assert.Equal(t, 1.0, values["overriding"])
	assert.Equal(t, v2alpha1.ReasonProxySourceAuditDisabled, reasons["overriding"])
	assert.Equal(t, 0.0, values["inheriting"])
	assert.Equal(t, v2alpha1.ReasonEgressAuditJoined, reasons["inheriting"])
}

// The false DirectEgress: a gateway with no defaultProxyRef reads DirectEgress at the
// gateway, but a set naming its own pool egresses through it, and the set's verdict
// must come from that pool.
func TestRunnerSetEgressAudit_ProxyRefWithoutGatewayDefault(t *testing.T) {
	const ns = "team-a"
	gw := gwObj("gw", ns, "")
	gw.Spec.AuditLogging = string(provisioner.WorkerAuditAddresses)
	proxied := rsObj("proxied", ns, func(rs *v2alpha1.RunnerSet) {
		rs.Spec.ProxyRef = &v2alpha1.ProxyObjectRef{Name: "dedicated"}
	})
	direct := rsObj("direct", ns, nil)

	c := reconcileEgressAuditSets(t, []client.Object{
		gw, tmplObj("tmpl", ns),
		egressAuditProxy("dedicated", ns, proxyAuditConnectionsWithSource),
	}, proxied, direct)

	cond := egressAuditCondition(t, c, ns, "proxied")
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, v2alpha1.ReasonEgressAuditJoined, cond.Reason)

	cond = egressAuditCondition(t, c, ns, "direct")
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, v2alpha1.ReasonDirectEgress, cond.Reason)
}

// A shared pool is never read by the AGC, so the source half arrives in the
// projection the GMC writes. A projection without the key — one an older GMC wrote —
// must read Off, because a missing fact cannot clear the flag.
func TestRunnerSetEgressAudit_SharedPoolReadsTheProjection(t *testing.T) {
	const ns, proxyNS, proxyName = "team-a", "platform-egress", "shared"
	share := func(data map[string]string) *corev1.ConfigMap {
		base := map[string]string{
			"ca.crt":     "-----BEGIN CERTIFICATE-----\nstub\n-----END CERTIFICATE-----\n",
			"proxy-host": proxyName + "-proxy." + proxyNS + ".svc.cluster.local",
			"proxy-port": "8080",
		}
		for k, v := range data {
			base[k] = v
		}
		return &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: proxyShareConfigMapName(proxyNS, proxyName), Namespace: ns},
			Data:       base,
		}
	}
	for _, tc := range []struct {
		name       string
		data       map[string]string
		wantReason string
	}{
		{"source half projected", map[string]string{proxyShareAuditLoggingKey: proxyAuditConnectionsWithSource}, v2alpha1.ReasonEgressAuditJoined},
		{"source half off", map[string]string{proxyShareAuditLoggingKey: "Off"}, v2alpha1.ReasonProxySourceAuditDisabled},
		{"key absent", nil, v2alpha1.ReasonProxySourceAuditDisabled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gw := gwObj("gw", ns, "")
			gw.Spec.AuditLogging = string(provisioner.WorkerAuditAddresses)
			rs := rsObj("set", ns, func(rs *v2alpha1.RunnerSet) {
				rs.Spec.ProxyRef = &v2alpha1.ProxyObjectRef{Name: proxyName, Namespace: proxyNS}
			})
			c := reconcileEgressAuditSets(t, []client.Object{gw, tmplObj("tmpl", ns), share(tc.data)}, rs)
			cond := egressAuditCondition(t, c, ns, "set")
			assert.Equal(t, tc.wantReason, cond.Reason)
			assert.Contains(t, cond.Message, `"shared"`)
		})
	}
}

// Every reason names the half that is off, and only the joined pair clears.
func TestRunnerSetEgressAudit_Reasons(t *testing.T) {
	pool := func(mode string) *resolvedProxy { return &resolvedProxy{name: "pool", auditLogging: mode} }
	gw := func(mode string) *v2alpha1.ActionsGateway {
		return &v2alpha1.ActionsGateway{ObjectMeta: metav1.ObjectMeta{Name: "gw"},
			Spec: v2alpha1.ActionsGatewaySpec{AuditLogging: mode}}
	}
	const on, src = "WorkerAddresses", proxyAuditConnectionsWithSource
	for _, tc := range []struct {
		name       string
		gw         *v2alpha1.ActionsGateway
		proxy      *resolvedProxy
		wantReason string
		wantInMsg  string
	}{
		{"joined", gw(on), pool(src), v2alpha1.ReasonEgressAuditJoined, src},
		{"worker half off", gw(""), pool(src), v2alpha1.ReasonWorkerAuditDisabled, `"Off"`},
		{"source half off", gw(on), pool("Connections"), v2alpha1.ReasonProxySourceAuditDisabled, `"Connections"`},
		{"neither", gw("Off"), pool(""), v2alpha1.ReasonEgressAuditDisabled, `"pool"`},
		{"direct", gw(on), nil, v2alpha1.ReasonDirectEgress, "direct"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reason, msg := runnerSetEgressAudit(tc.gw, tc.proxy)
			assert.Equal(t, tc.wantReason, reason)
			assert.Contains(t, msg, tc.wantInMsg)
		})
	}
}

// A set whose references never resolved has no pool to judge, so it has no series:
// a 0 there would read as joined.
func TestRunnerSetEgressAuditCollector_SkipsSetsWithoutTheCondition(t *testing.T) {
	rs := rsObj("unresolved", "team-a", nil)
	c := fake.NewClientBuilder().WithScheme(runnerSetTestScheme(t)).WithObjects(rs).Build()
	values, _ := egressAuditSeries(t, c)
	assert.Empty(t, values)
}
