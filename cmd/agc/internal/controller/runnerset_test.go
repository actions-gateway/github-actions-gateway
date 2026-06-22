package controller

import (
	"context"
	"log/slog"
	"testing"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func runnerSetTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(s))
	require.NoError(t, v2alpha1.AddToScheme(s))
	return s
}

func rsObj(name, ns string, mut func(*v2alpha1.RunnerSet)) *v2alpha1.RunnerSet {
	rs := &v2alpha1.RunnerSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: v2alpha1.RunnerSetSpec{
			GatewayRef:   v2alpha1.ObjectRef{Name: "gw"},
			TemplateRef:  v2alpha1.ObjectRef{Name: "tmpl"},
			MaxListeners: 1,
			RunnerLabels: []string{"self-hosted"},
		},
	}
	if mut != nil {
		mut(rs)
	}
	return rs
}

func gwObj(name, ns, proxyRef string) *v2alpha1.ActionsGateway {
	ag := &v2alpha1.ActionsGateway{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       v2alpha1.ActionsGatewaySpec{GitHubURL: "https://github.com/x"},
	}
	if proxyRef != "" {
		ag.Spec.DefaultProxyRef = &v2alpha1.LocalObjectRef{Name: proxyRef}
	}
	return ag
}

func tmplObj(name, ns string) *v2alpha1.RunnerTemplate {
	return &v2alpha1.RunnerTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: v2alpha1.RunnerTemplateSpec{
			WorkerImage: "runner:test",
			PodTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "runner", Image: "runner:test"}},
			}},
		},
	}
}

func TestResolveRunnerSetRefs_Branches(t *testing.T) {
	scheme := runnerSetTestScheme(t)
	ns := "team-a"

	build := func(objs ...client.Object) client.Client {
		return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	}

	t.Run("gateway missing", func(t *testing.T) {
		rs := rsObj("set", ns, nil)
		_, res := resolveRunnerSetRefs(context.Background(), build(rs), rs)
		assert.Equal(t, v2alpha1.ReasonGatewayNotFound, res.reason)
	})

	t.Run("template missing", func(t *testing.T) {
		rs := rsObj("set", ns, nil)
		_, res := resolveRunnerSetRefs(context.Background(), build(rs, gwObj("gw", ns, "shared")), rs)
		assert.Equal(t, v2alpha1.ReasonTemplateNotFound, res.reason)
	})

	t.Run("proxy unset everywhere", func(t *testing.T) {
		rs := rsObj("set", ns, nil)
		c := build(rs, gwObj("gw", ns, ""), tmplObj("tmpl", ns))
		_, res := resolveRunnerSetRefs(context.Background(), c, rs)
		assert.Equal(t, v2alpha1.ReasonProxyNotFound, res.reason)
	})

	t.Run("proxy absent", func(t *testing.T) {
		rs := rsObj("set", ns, nil)
		c := build(rs, gwObj("gw", ns, "shared"), tmplObj("tmpl", ns))
		_, res := resolveRunnerSetRefs(context.Background(), c, rs)
		assert.Equal(t, v2alpha1.ReasonProxyNotFound, res.reason)
	})

	t.Run("all resolved via gateway defaultProxyRef", func(t *testing.T) {
		rs := rsObj("set", ns, nil)
		ep := &v2alpha1.EgressProxy{ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: ns}}
		c := build(rs, gwObj("gw", ns, "shared"), tmplObj("tmpl", ns), ep)
		refs, res := resolveRunnerSetRefs(context.Background(), c, rs)
		require.True(t, res.resolved())
		assert.Equal(t, "shared", refs.proxy.Name)
		assert.Equal(t, "runner:test", refs.template.WorkerImage)
	})

	t.Run("proxyRef overrides defaultProxyRef", func(t *testing.T) {
		rs := rsObj("set", ns, func(rs *v2alpha1.RunnerSet) {
			rs.Spec.ProxyRef = &v2alpha1.ObjectRef{Name: "dedicated"}
		})
		ep := &v2alpha1.EgressProxy{ObjectMeta: metav1.ObjectMeta{Name: "dedicated", Namespace: ns}}
		c := build(rs, gwObj("gw", ns, "shared"), tmplObj("tmpl", ns), ep)
		refs, res := resolveRunnerSetRefs(context.Background(), c, rs)
		require.True(t, res.resolved())
		assert.Equal(t, "dedicated", refs.proxy.Name)
	})

	t.Run("cluster template fails closed when absent", func(t *testing.T) {
		// A templateRef.kind=ClusterRunnerTemplate naming a missing object fails
		// closed with TemplateNotFound (the cluster-scoped read is authorized by the
		// per-gateway ClusterRoleBinding in M3b; until the referent exists the set
		// waits, §H.7).
		rs := rsObj("set", ns, func(rs *v2alpha1.RunnerSet) {
			rs.Spec.TemplateRef = v2alpha1.ObjectRef{Name: "golden", Kind: "ClusterRunnerTemplate"}
		})
		c := build(rs, gwObj("gw", ns, "shared"))
		_, res := resolveRunnerSetRefs(context.Background(), c, rs)
		assert.Equal(t, v2alpha1.ReasonTemplateNotFound, res.reason)
		assert.Contains(t, res.message, "ClusterRunnerTemplate")
	})

	t.Run("cluster template resolves when present (M3b)", func(t *testing.T) {
		// With the ClusterRunnerTemplate applied, the cluster-scoped read resolves it
		// and the references are complete (proxy via gateway defaultProxyRef).
		rs := rsObj("set", ns, func(rs *v2alpha1.RunnerSet) {
			rs.Spec.TemplateRef = v2alpha1.ObjectRef{Name: "golden", Kind: "ClusterRunnerTemplate"}
		})
		crt := &v2alpha1.ClusterRunnerTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "golden"},
			Spec:       v2alpha1.RunnerTemplateSpec{WorkerImage: "golden:test"},
		}
		ep := &v2alpha1.EgressProxy{ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: ns}}
		c := build(rs, gwObj("gw", ns, "shared"), crt, ep)
		refs, res := resolveRunnerSetRefs(context.Background(), c, rs)
		require.True(t, res.resolved())
		assert.Equal(t, "golden:test", refs.template.WorkerImage)
	})
}

func TestRunnerSetTarget_ResolveAndCeiling(t *testing.T) {
	scheme := runnerSetTestScheme(t)
	ns := "team-a"
	rs := rsObj("set", ns, func(rs *v2alpha1.RunnerSet) {
		rs.UID = "uid-123"
		rs.Spec.MaxWorkers = ptr.To(int32(7))
	})
	ep := &v2alpha1.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: ns},
		Spec:       v2alpha1.EgressProxySpec{NoProxyCIDRs: []string{"10.0.0.0/8"}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(rs, gwObj("gw", ns, "shared"), tmplObj("tmpl", ns), ep).Build()

	prov := provisioner.NewProvisioner(c, nil, slog.Default())
	prov.SecurityProfile = "restricted"
	target := &runnerSetTarget{client: c, prov: prov, key: client.ObjectKey{Namespace: ns, Name: "set"}, uid: "uid-123"}

	// OwnerRef points at the real RunnerSet.
	ref := target.OwnerRef()
	assert.Equal(t, "RunnerSet", ref.Kind)
	assert.Equal(t, types.UID("uid-123"), ref.UID)
	assert.True(t, *ref.Controller)
	assert.Equal(t, provisioner.LabelRunnerSet, firstKey(target.PodOwnerLabels()))

	// Ceiling reads the fresh spec.
	limit, bounded := target.Ceiling(context.Background())
	assert.True(t, bounded)
	assert.Equal(t, int32(7), limit)

	// Resolve wires the pod shape + proxy from the resolved referents.
	spec, err := target.Resolve(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "runner:test", spec.WorkerImage)
	assert.Equal(t, "https://shared-proxy.team-a.svc.cluster.local:8080", spec.HTTPSProxy)
	assert.Equal(t, "shared-proxy-tls", spec.ProxyTLSSecretName)
	assert.Equal(t, "restricted", spec.SecurityProfile)
	assert.Contains(t, spec.NoProxy, "10.0.0.0/8")

	// A missing referent fails Resolve closed (no pod would be created).
	require.NoError(t, c.Delete(context.Background(), ep))
	_, err = target.Resolve(context.Background())
	assert.Error(t, err)
}

func TestRunnerSetReconcile_FailsClosedWithGatewayNotFound(t *testing.T) {
	scheme := runnerSetTestScheme(t)
	ns := "team-a"
	rs := rsObj("set", ns, func(rs *v2alpha1.RunnerSet) {
		rs.Finalizers = []string{runnerSetFinalizer} // skip the finalizer-add requeue
	})
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(rs).WithStatusSubresource(rs).Build()

	r := &RunnerSetReconciler{Client: c, Log: slog.Default()}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "set"}})
	require.NoError(t, err)

	var got v2alpha1.RunnerSet
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "set"}, &got))
	ready := meta.FindStatusCondition(got.Status.Conditions, v2alpha1.ConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Equal(t, v2alpha1.ReasonGatewayNotFound, ready.Reason)
	assert.Equal(t, got.Generation, got.Status.ObservedGeneration)
}

func TestRunnerSetReconcile_RemovesFinalizerOnDelete(t *testing.T) {
	scheme := runnerSetTestScheme(t)
	ns := "team-a"
	now := metav1.Now()
	rs := rsObj("set", ns, func(rs *v2alpha1.RunnerSet) {
		rs.Finalizers = []string{runnerSetFinalizer}
		rs.DeletionTimestamp = &now
	})
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(rs).WithStatusSubresource(rs).Build()

	r := &RunnerSetReconciler{Client: c, Log: slog.Default()}
	r.ensureMaps()
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "set"}})
	require.NoError(t, err)
	var got v2alpha1.RunnerSet
	assert.Error(t, c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "set"}, &got))
}

func TestRunnerSetWatchMappers(t *testing.T) {
	scheme := runnerSetTestScheme(t)
	ns := "team-a"
	// Two sets: "set" → gw/tmpl/shared; "other" → gw2/tmpl2/proxyRef=dedicated.
	setA := rsObj("set", ns, nil)
	setB := rsObj("other", ns, func(rs *v2alpha1.RunnerSet) {
		rs.Spec.GatewayRef = v2alpha1.ObjectRef{Name: "gw2"}
		rs.Spec.TemplateRef = v2alpha1.ObjectRef{Name: "tmpl2"}
		rs.Spec.ProxyRef = &v2alpha1.ObjectRef{Name: "dedicated"}
	})
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(setA, setB).Build()
	r := &RunnerSetReconciler{Client: c, Log: slog.Default()}
	ctx := context.Background()

	gw := &v2alpha1.ActionsGateway{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: ns}}
	assert.Len(t, r.gatewayToRunnerSets(ctx, gw), 1, "gateway gw maps to set")

	tmpl := &v2alpha1.RunnerTemplate{ObjectMeta: metav1.ObjectMeta{Name: "tmpl2", Namespace: ns}}
	assert.Len(t, r.templateToRunnerSets(ctx, tmpl), 1, "template tmpl2 maps to other")

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: ns, Name: "runner-x",
		Labels: map[string]string{provisioner.LabelRunnerSet: "set"},
	}}
	reqs := r.podToRunnerSet(ctx, pod)
	require.Len(t, reqs, 1)
	assert.Equal(t, "set", reqs[0].Name)
	assert.Nil(t, r.podToRunnerSet(ctx, &corev1.Pod{}), "an unlabeled pod maps to nothing")

	// A direct-proxyRef set is matched by proxyToRunnerSets only via its name; an
	// unset-proxyRef set is always enqueued (re-resolve through the gateway).
	dedicated := &v2alpha1.EgressProxy{ObjectMeta: metav1.ObjectMeta{Name: "dedicated", Namespace: ns}}
	got := r.proxyToRunnerSets(ctx, dedicated)
	assert.GreaterOrEqual(t, len(got), 1, "proxy dedicated maps to at least the directly-referencing set")
}

func firstKey(m map[string]string) string {
	for k := range m {
		return k
	}
	return ""
}

func TestRunnerSetReaper_DeletesExpiredPods(t *testing.T) {
	scheme := runnerSetTestScheme(t)
	ns := "team-a"
	rs := rsObj("set", ns, func(rs *v2alpha1.RunnerSet) {
		rs.Spec.CompletedPodTTL = &metav1.Duration{Duration: 0} // delete terminal pods immediately
	})
	terminal := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "runner-done", Labels: map[string]string{provisioner.LabelRunnerSet: "set"}},
		Status:     corev1.PodStatus{Phase: corev1.PodSucceeded},
	}
	running := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "runner-live", Labels: map[string]string{provisioner.LabelRunnerSet: "set"}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(rs, terminal, running).Build()
	r := &RunnerSetReconciler{Client: c, Log: slog.Default()}

	_, err := r.reapWorkerPods(context.Background(), slog.Default(), rs)
	require.NoError(t, err)

	// The terminal pod is reaped; the running pod is left alone.
	assert.Error(t, c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "runner-done"}, &corev1.Pod{}))
	assert.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "runner-live"}, &corev1.Pod{}))
}

func TestRunnerSetDrainConditions_MergesOwnSkipsOthers(t *testing.T) {
	r := &RunnerSetReconciler{Log: slog.Default()}
	r.ensureMaps()
	rs := rsObj("set", "team-a", nil)

	// One condition for this set, one for another.
	r.conditionCh <- conditionUpdate{namespace: "team-a", name: "set", condition: metav1.Condition{
		Type: "Degraded", Status: metav1.ConditionTrue, Reason: "X", Message: "m"}}
	r.conditionCh <- conditionUpdate{namespace: "team-a", name: "other", condition: metav1.Condition{
		Type: "Degraded", Status: metav1.ConditionTrue, Reason: "Y", Message: "n"}}

	r.drainConditions(rs)

	assert.NotNil(t, meta.FindStatusCondition(rs.Status.Conditions, "Degraded"), "own condition merged")
	// The other set's condition is re-enqueued, not applied here.
	assert.Len(t, r.conditionCh, 1)
}

func TestRunnerSetLocalState_PoolLifecycle(t *testing.T) {
	scheme := runnerSetTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &RunnerSetReconciler{Client: c, Log: slog.Default()}
	r.ensureMaps()
	key := types.NamespacedName{Namespace: "team-a", Name: "set"}

	pool := r.getOrCreatePool(key, "team-a", "set", []string{"self-hosted"})
	require.NotNil(t, pool)
	assert.Same(t, pool, r.getOrCreatePool(key, "team-a", "set", nil), "pool is cached")
	assert.Same(t, pool, r.getPool(key))

	// cleanupLocalState drops it (and is a no-op on the absent multiplexer / a re-call).
	r.cleanupLocalState(key)
	assert.Nil(t, r.getPool(key))
	r.cleanupLocalState(key) // idempotent
}

func TestRunnerSetBaselineRecheckInterval(t *testing.T) {
	r := &RunnerSetReconciler{}
	assert.Equal(t, defaultBaselineRecheckInterval, r.baselineRecheckInterval())
	r.BaselineRecheckInterval = 3
	assert.Equal(t, 3, int(r.baselineRecheckInterval()))
}
