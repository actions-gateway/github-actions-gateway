package controller

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/runnercore"
	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
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
			TemplateRef:  &v2alpha1.ObjectRef{Name: "tmpl"},
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
		ag.Spec.DefaultProxyRef = &v2alpha1.ProxyObjectRef{Name: proxyRef}
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
		_, res := resolveRunnerSetRefs(context.Background(), build(rs), nil, rs)
		assert.Equal(t, v2alpha1.ReasonGatewayNotFound, res.reason)
	})

	t.Run("template missing", func(t *testing.T) {
		rs := rsObj("set", ns, nil)
		_, res := resolveRunnerSetRefs(context.Background(), build(rs, gwObj("gw", ns, "shared")), nil, rs)
		assert.Equal(t, v2alpha1.ReasonTemplateNotFound, res.reason)
	})

	t.Run("proxy unset everywhere resolves to direct egress", func(t *testing.T) {
		// No proxyRef and no gateway defaultProxyRef ⇒ direct egress (Q168): resolved
		// with refs.proxy == nil, not ProxyNotFound.
		rs := rsObj("set", ns, nil)
		c := build(rs, gwObj("gw", ns, ""), tmplObj("tmpl", ns))
		refs, res := resolveRunnerSetRefs(context.Background(), c, nil, rs)
		require.True(t, res.resolved(), "no proxy ⇒ direct egress, not a failure")
		assert.Nil(t, refs.proxy, "direct egress: no resolved proxy")
	})

	t.Run("named but absent proxy still fails closed", func(t *testing.T) {
		// A gateway defaultProxyRef naming a *missing* proxy must not silently fall
		// back to direct egress — it fails closed with ProxyNotFound (Q168).
		rs := rsObj("set", ns, nil)
		c := build(rs, gwObj("gw", ns, "shared"), tmplObj("tmpl", ns))
		_, res := resolveRunnerSetRefs(context.Background(), c, nil, rs)
		assert.Equal(t, v2alpha1.ReasonProxyNotFound, res.reason)
	})

	t.Run("all resolved via gateway defaultProxyRef", func(t *testing.T) {
		rs := rsObj("set", ns, nil)
		ep := &v2alpha1.EgressProxy{ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: ns}}
		c := build(rs, gwObj("gw", ns, "shared"), tmplObj("tmpl", ns), ep)
		refs, res := resolveRunnerSetRefs(context.Background(), c, nil, rs)
		require.True(t, res.resolved())
		assert.Equal(t, "shared-proxy.team-a.svc.cluster.local", refs.proxy.host)
		assert.Equal(t, "runner:test", refs.template.WorkerImage)
	})

	t.Run("proxyRef overrides defaultProxyRef", func(t *testing.T) {
		rs := rsObj("set", ns, func(rs *v2alpha1.RunnerSet) {
			rs.Spec.ProxyRef = &v2alpha1.ProxyObjectRef{Name: "dedicated"}
		})
		ep := &v2alpha1.EgressProxy{ObjectMeta: metav1.ObjectMeta{Name: "dedicated", Namespace: ns}}
		c := build(rs, gwObj("gw", ns, "shared"), tmplObj("tmpl", ns), ep)
		refs, res := resolveRunnerSetRefs(context.Background(), c, nil, rs)
		require.True(t, res.resolved())
		assert.Equal(t, "dedicated-proxy.team-a.svc.cluster.local", refs.proxy.host)
	})

	t.Run("cluster template fails closed when absent", func(t *testing.T) {
		// A templateRef.kind=ClusterRunnerTemplate naming a missing object fails
		// closed with TemplateNotFound (the cluster-scoped read is authorized by the
		// per-gateway ClusterRoleBinding in M3b; until the referent exists the set
		// waits, §H.7).
		rs := rsObj("set", ns, func(rs *v2alpha1.RunnerSet) {
			rs.Spec.TemplateRef = &v2alpha1.ObjectRef{Name: "golden", Kind: "ClusterRunnerTemplate"}
		})
		c := build(rs, gwObj("gw", ns, "shared"))
		_, res := resolveRunnerSetRefs(context.Background(), c, nil, rs)
		assert.Equal(t, v2alpha1.ReasonTemplateNotFound, res.reason)
		assert.Contains(t, res.message, "ClusterRunnerTemplate")
	})

	t.Run("cluster template resolves when present (M3b)", func(t *testing.T) {
		// With the ClusterRunnerTemplate applied, the cluster-scoped read resolves it
		// and the references are complete (proxy via gateway defaultProxyRef).
		rs := rsObj("set", ns, func(rs *v2alpha1.RunnerSet) {
			rs.Spec.TemplateRef = &v2alpha1.ObjectRef{Name: "golden", Kind: "ClusterRunnerTemplate"}
		})
		crt := &v2alpha1.ClusterRunnerTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "golden"},
			Spec:       v2alpha1.RunnerTemplateSpec{WorkerImage: "golden:test"},
		}
		ep := &v2alpha1.EgressProxy{ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: ns}}
		c := build(rs, gwObj("gw", ns, "shared"), crt, ep)
		refs, res := resolveRunnerSetRefs(context.Background(), c, nil, rs)
		require.True(t, res.resolved())
		assert.Equal(t, "golden:test", refs.template.WorkerImage)
	})
}

// crtObj builds a ClusterRunnerTemplate, marking it the cluster default
// (IsDefaultTemplateAnnotation) when isDefault is set.
func crtObj(name, workerImage string, isDefault bool) *v2alpha1.ClusterRunnerTemplate {
	crt := &v2alpha1.ClusterRunnerTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v2alpha1.RunnerTemplateSpec{WorkerImage: workerImage},
	}
	if isDefault {
		crt.Annotations = map[string]string{v2alpha1.IsDefaultTemplateAnnotation: v2alpha1.IsDefaultTemplateValue}
	}
	return crt
}

// TestResolveTemplateChain covers the optional-templateRef fallback chain (Q172, §H.4):
// rs.templateRef → gateway.defaultTemplateRef → the single cluster-default
// ClusterRunnerTemplate → fail-closed TemplateNotFound, plus the ≤1-default enforcement.
func TestResolveTemplateChain(t *testing.T) {
	scheme := runnerSetTestScheme(t)
	ns := "team-a"
	// EgressProxy so resolution reaches Ready when the template resolves (gateway sets
	// defaultProxyRef "shared" in gwObj when passed; here proxy-less tests use "").
	build := func(objs ...client.Object) client.Client {
		return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	}
	// unsetTemplate clears the templateRef the rsObj helper sets by default.
	unsetTemplate := func(rs *v2alpha1.RunnerSet) { rs.Spec.TemplateRef = nil }

	t.Run("rung 1: explicit templateRef → source TemplateRef", func(t *testing.T) {
		rs := rsObj("set", ns, nil) // templateRef "tmpl"
		c := build(rs, gwObj("gw", ns, ""), tmplObj("tmpl", ns))
		refs, res := resolveRunnerSetRefs(context.Background(), c, nil, rs)
		require.True(t, res.resolved())
		assert.Equal(t, v2alpha1.TemplateSourceRef, refs.templateSource)
		assert.Equal(t, "runner:test", refs.template.WorkerImage)
	})

	t.Run("rung 2: unset templateRef inherits gateway.defaultTemplateRef (RunnerTemplate)", func(t *testing.T) {
		rs := rsObj("set", ns, unsetTemplate)
		gw := gwObj("gw", ns, "")
		gw.Spec.DefaultTemplateRef = &v2alpha1.ObjectRef{Name: "gw-default"}
		c := build(rs, gw, tmplObj("gw-default", ns))
		refs, res := resolveRunnerSetRefs(context.Background(), c, nil, rs)
		require.True(t, res.resolved())
		assert.Equal(t, v2alpha1.TemplateSourceGatewayDefault, refs.templateSource)
		assert.Equal(t, "runner:test", refs.template.WorkerImage)
	})

	t.Run("rung 2: gateway.defaultTemplateRef may point at a ClusterRunnerTemplate", func(t *testing.T) {
		rs := rsObj("set", ns, unsetTemplate)
		gw := gwObj("gw", ns, "")
		gw.Spec.DefaultTemplateRef = &v2alpha1.ObjectRef{Name: "golden", Kind: "ClusterRunnerTemplate"}
		c := build(rs, gw, crtObj("golden", "golden:test", false))
		refs, res := resolveRunnerSetRefs(context.Background(), c, nil, rs)
		require.True(t, res.resolved())
		assert.Equal(t, v2alpha1.TemplateSourceGatewayDefault, refs.templateSource)
		assert.Equal(t, "golden:test", refs.template.WorkerImage)
	})

	t.Run("rung 2: defaultTemplateRef naming a missing template fails closed", func(t *testing.T) {
		rs := rsObj("set", ns, unsetTemplate)
		gw := gwObj("gw", ns, "")
		gw.Spec.DefaultTemplateRef = &v2alpha1.ObjectRef{Name: "absent"}
		c := build(rs, gw)
		_, res := resolveRunnerSetRefs(context.Background(), c, nil, rs)
		assert.Equal(t, v2alpha1.ReasonTemplateNotFound, res.reason)
	})

	t.Run("rung 3: single cluster-default ClusterRunnerTemplate → source ClusterDefault", func(t *testing.T) {
		rs := rsObj("set", ns, unsetTemplate)
		c := build(rs, gwObj("gw", ns, ""), crtObj("platform-default", "default:test", true), crtObj("other", "other:test", false))
		refs, res := resolveRunnerSetRefs(context.Background(), c, nil, rs)
		require.True(t, res.resolved())
		assert.Equal(t, v2alpha1.TemplateSourceClusterDefault, refs.templateSource)
		assert.Equal(t, "default:test", refs.template.WorkerImage, "the marked default, not the unmarked one")
	})

	t.Run("rung 3: no marked cluster-default fails closed TemplateNotFound", func(t *testing.T) {
		rs := rsObj("set", ns, unsetTemplate)
		// A ClusterRunnerTemplate exists but is unmarked; nothing else resolves.
		c := build(rs, gwObj("gw", ns, ""), crtObj("unmarked", "x:test", false))
		_, res := resolveRunnerSetRefs(context.Background(), c, nil, rs)
		assert.Equal(t, v2alpha1.ReasonTemplateNotFound, res.reason)
	})

	t.Run("rung 3: two marked cluster-defaults fail closed AmbiguousDefault", func(t *testing.T) {
		rs := rsObj("set", ns, unsetTemplate)
		c := build(rs, gwObj("gw", ns, ""), crtObj("default-a", "a:test", true), crtObj("default-b", "b:test", true))
		_, res := resolveRunnerSetRefs(context.Background(), c, nil, rs)
		assert.Equal(t, v2alpha1.ReasonAmbiguousDefault, res.reason)
		// Message names the conflicting templates (sorted) so an operator can fix it.
		assert.Contains(t, res.message, "default-a")
		assert.Contains(t, res.message, "default-b")
	})

	t.Run("a namespaced RunnerTemplate cannot be the cluster default", func(t *testing.T) {
		// The default marker is honored only on the cluster-scoped kind: a tenant must
		// not self-elect a namespaced template cluster-wide. A namespaced RunnerTemplate
		// carrying the annotation is ignored by the cluster-default rung.
		rs := rsObj("set", ns, unsetTemplate)
		rt := tmplObj("tmpl", ns)
		rt.Annotations = map[string]string{v2alpha1.IsDefaultTemplateAnnotation: v2alpha1.IsDefaultTemplateValue}
		c := build(rs, gwObj("gw", ns, ""), rt)
		_, res := resolveRunnerSetRefs(context.Background(), c, nil, rs)
		assert.Equal(t, v2alpha1.ReasonTemplateNotFound, res.reason)
	})

	t.Run("explicit templateRef wins over both defaults", func(t *testing.T) {
		rs := rsObj("set", ns, nil) // explicit "tmpl"
		gw := gwObj("gw", ns, "")
		gw.Spec.DefaultTemplateRef = &v2alpha1.ObjectRef{Name: "gw-default"}
		c := build(rs, gw, tmplObj("tmpl", ns), tmplObj("gw-default", ns), crtObj("platform-default", "d:test", true))
		refs, res := resolveRunnerSetRefs(context.Background(), c, nil, rs)
		require.True(t, res.resolved())
		assert.Equal(t, v2alpha1.TemplateSourceRef, refs.templateSource)
		assert.Equal(t, "runner:test", refs.template.WorkerImage)
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
	// Worker exemptions are DNS + loopback only: workers hold no kubeconfig and
	// never dial the API server, so nothing here may be an IP range (Q465). The
	// kubeadm Service CIDR that used to be appended was wrong off kind and, on a
	// managed cluster, exempted arbitrary pod/node addresses from the tenant's
	// egress attribution.
	assert.Equal(t, "10.0.0.0/8,svc.cluster.local,localhost,127.0.0.1", spec.NoProxy)

	// A missing referent fails Resolve closed (no pod would be created).
	require.NoError(t, c.Delete(context.Background(), ep))
	_, err = target.Resolve(context.Background())
	assert.Error(t, err)
}

// TestRunnerSetTarget_ResolveDirectEgress: with no proxyRef/defaultProxyRef, Resolve
// returns a spec with empty proxy fields so the worker reaches GitHub directly — no
// HTTP(S)_PROXY env, no proxy-CA mount (Q168, §H.10).
func TestRunnerSetTarget_ResolveDirectEgress(t *testing.T) {
	scheme := runnerSetTestScheme(t)
	ns := "team-a"
	rs := rsObj("set", ns, nil)
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(rs, gwObj("gw", ns, ""), tmplObj("tmpl", ns)).Build()

	prov := provisioner.NewProvisioner(c, nil, slog.Default())
	target := &runnerSetTarget{client: c, prov: prov, key: client.ObjectKey{Namespace: ns, Name: "set"}, uid: "uid-1"}

	spec, err := target.Resolve(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "runner:test", spec.WorkerImage)
	assert.Empty(t, spec.HTTPProxy, "direct egress: no HTTP proxy")
	assert.Empty(t, spec.HTTPSProxy, "direct egress: no HTTPS proxy")
	assert.Empty(t, spec.ProxyTLSSecretName, "direct egress: no proxy-CA mount")
}

// TestRunnerSetTarget_ResolveCarriesGitHubCABundle: the GHES appliance's CA is a
// property of the gateway, not of the egress path, so Resolve carries it whether the
// worker egresses through a proxy or directly (Q536).
func TestRunnerSetTarget_ResolveCarriesGitHubCABundle(t *testing.T) {
	scheme := runnerSetTestScheme(t)
	ns := "team-a"
	rs := rsObj("set", ns, nil)
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(rs, gwObj("gw", ns, ""), tmplObj("tmpl", ns)).Build()

	prov := provisioner.NewProvisioner(c, nil, slog.Default())
	prov.GitHubCAConfigMapName = "ghes-ca"
	target := &runnerSetTarget{client: c, prov: prov, key: client.ObjectKey{Namespace: ns, Name: "set"}, uid: "uid-1"}

	spec, err := target.Resolve(context.Background())
	require.NoError(t, err)
	assert.Empty(t, spec.ProxyTLSSecretName, "direct egress, to prove the CA does not ride the proxy branch")
	assert.Equal(t, "ghes-ca", spec.GitHubCAConfigMapName)
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
		rs.Spec.TemplateRef = &v2alpha1.ObjectRef{Name: "tmpl2"}
		rs.Spec.ProxyRef = &v2alpha1.ProxyObjectRef{Name: "dedicated"}
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

	_, counts, err := r.reapWorkerPods(context.Background(), slog.Default(), rs)
	require.NoError(t, err)

	// The terminal pod is reaped; the running pod is left alone.
	assert.Error(t, c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "runner-done"}, &corev1.Pod{}))
	assert.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "runner-live"}, &corev1.Pod{}))

	// Running pod counted as active; no pending pods.
	assert.Equal(t, int32(1), counts.active, "Running pod must be counted in activeJobs")
	assert.Equal(t, int32(0), counts.pending, "no Pending pods")
}

// reapTestMetrics builds a Metrics carrying the collectors a reap path touches,
// unregistered (kept off the global Prometheus registry) for per-test isolation. The
// sidecar gauge is here because a reap driven through Reconcile also runs the status
// helpers, which set it unconditionally once Metrics is non-nil.
func reapTestMetrics() *runnercore.Metrics {
	return &runnercore.Metrics{
		WorkerPodsReaped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "t_runnerset_worker_pods_reaped_total",
		}, []string{"namespace", "runner_group", "runner_set", "reason"}),
		ReapBlockingSidecarTemplates: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "t_runnerset_reap_blocking_sidecar_templates",
		}, []string{"namespace", "runner_set"}),
	}
}

// TestRunnerSetReaper_ReapsOrphanedRunningPods covers Q420: a scale-set worker that
// is still Running after its job went terminal at GitHub gets a reap deadline from
// the completion annotation, while every other Running pod keeps the old no-deadline
// treatment.
func TestRunnerSetReaper_ReapsOrphanedRunningPods(t *testing.T) {
	scheme := runnerSetTestScheme(t)
	ns := "team-a"
	now := time.Now()
	rs := rsObj("set", ns, nil)

	runningPod := func(name string, completedAt string) *corev1.Pod {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name,
				Labels: map[string]string{provisioner.LabelRunnerSet: "set"}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		}
		if completedAt != "" {
			pod.Annotations = map[string]string{provisioner.AnnotationJobCompletedAt: completedAt}
		}
		return pod
	}
	stamp := func(d time.Duration) string { return now.Add(d).UTC().Format(time.RFC3339) }

	// Job over 6m ago, grace 5m → reaped.
	stuck := runningPod("runner-stuck", stamp(-6*time.Minute))
	// Job over 1m ago → retained, due in ~4m (the only pending deadline).
	finishing := runningPod("runner-finishing", stamp(-time.Minute))
	// Job still assigned: no stamp → no deadline, as before.
	live := runningPod("runner-live", "")
	// An unparseable stamp must never be read as "completed long ago".
	garbled := runningPod("runner-garbled", "yesterday")

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(rs, stuck, finishing, live, garbled).Build()
	rec := events.NewFakeRecorder(8)
	r := &RunnerSetReconciler{
		Client:   c,
		Log:      slog.Default(),
		Now:      func() time.Time { return now },
		Metrics:  reapTestMetrics(),
		Recorder: rec,
	}

	next, counts, err := r.reapWorkerPods(context.Background(), slog.Default(), rs)
	require.NoError(t, err)

	ctx := context.Background()
	get := func(name string) error {
		return c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &corev1.Pod{})
	}
	assert.Error(t, get("runner-stuck"), "a Running pod past its job's completion grace must be reaped")
	assert.NoError(t, get("runner-finishing"), "a Running pod within the grace must be retained")
	assert.NoError(t, get("runner-live"), "a Running pod with no completion stamp must never be reaped")
	assert.NoError(t, get("runner-garbled"), "an unparseable completion stamp must not reap the pod")

	assert.InDelta(t, (4 * time.Minute).Seconds(), next.Seconds(), 1.0,
		"the next due time must come from the retained within-grace pod")
	assert.Equal(t, int32(4), counts.active,
		"every Running pod counts as active, including the one reaped this pass")

	assert.Equal(t, 1.0, testutil.ToFloat64(
		r.Metrics.WorkerPodsReaped.WithLabelValues(ns, "set", "set", "orphaned_running")),
		"a scale-set reap must stamp the set name into both runner_group and runner_set")

	var orphanEvent string
	for len(rec.Events) > 0 {
		if e := <-rec.Events; strings.Contains(e, "WorkerPodOrphanedRunning") {
			orphanEvent = e
		}
	}
	require.NotEmpty(t, orphanEvent, "reaping an orphaned Running pod must emit a WorkerPodOrphanedRunning event")
	assert.Contains(t, orphanEvent, "runner-stuck")
	assert.Contains(t, orphanEvent, "Warning")
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

func TestRunnerSetDrainEvents_RecordsOwnSkipsOthers(t *testing.T) {
	rec := events.NewFakeRecorder(16)
	r := &RunnerSetReconciler{Log: slog.Default(), Recorder: rec}
	r.ensureMaps()
	rs := rsObj("set", "team-a", nil)

	// One event for this set, one for another set.
	r.eventCh <- eventRecord{namespace: "team-a", name: "set", eventtype: corev1.EventTypeWarning,
		reason: "QuotaRetriesExhausted", action: "ProvisionWorker", note: "quota exhausted"}
	r.eventCh <- eventRecord{namespace: "team-a", name: "other", eventtype: corev1.EventTypeWarning,
		reason: "EvictionRetriesExhausted", action: "RetryEvictedJob", note: "manual re-run"}

	r.drainEvents(rs)

	// Own event is recorded on the live RunnerSet.
	select {
	case e := <-rec.Events:
		assert.Contains(t, e, "QuotaRetriesExhausted")
	default:
		t.Fatal("expected an Event for this RunnerSet")
	}
	// The other set's event is re-enqueued, not recorded here.
	assert.Len(t, r.eventCh, 1)
}

func TestRunnerSetLocalState_PoolLifecycle(t *testing.T) {
	scheme := runnerSetTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &RunnerSetReconciler{Client: c, Log: slog.Default()}
	r.ensureMaps()
	key := types.NamespacedName{Namespace: "team-a", Name: "set"}

	rs := &v2alpha1.RunnerSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name, UID: "set-uid"},
		Spec:       v2alpha1.RunnerSetSpec{RunnerLabels: []string{"self-hosted"}},
	}
	pool := r.getOrCreatePool(key, rs)
	require.NotNil(t, pool)
	assert.Same(t, pool, r.getOrCreatePool(key, rs), "pool is cached")
	assert.Same(t, pool, r.getPool(key))

	// cleanupLocalState drops it (and is a no-op on the absent multiplexer / a re-call).
	r.cleanupLocalState(key)
	assert.Nil(t, r.getPool(key))
	r.cleanupLocalState(key) // idempotent
}

// TestReadyConditionForListeners covers the classic-path Ready decision (Q308): a start
// failure must surface as Ready=False/ListenerStartFailed, distinct from the benign
// NoActiveSessions state, while a running listener still wins over any stale start error.
func TestReadyConditionForListeners(t *testing.T) {
	t.Run("running listener ⇒ Ready/ListenerActive", func(t *testing.T) {
		ready, reason, msg := readyConditionForListeners(2, nil, v2alpha1.TemplateSourceRef)
		assert.True(t, ready)
		assert.Equal(t, v2alpha1.ReasonListenerActive, reason)
		assert.Contains(t, msg, "TemplateRef")
		assert.Contains(t, msg, "2 listener")
	})

	t.Run("start error, no listeners ⇒ Ready=False/ListenerStartFailed", func(t *testing.T) {
		ready, reason, msg := readyConditionForListeners(0, errors.New("boom"), v2alpha1.TemplateSourceRef)
		assert.False(t, ready)
		assert.Equal(t, v2alpha1.ReasonListenerStartFailed, reason)
		assert.Contains(t, msg, "boom")
	})

	t.Run("a running listener wins over a stale start error", func(t *testing.T) {
		ready, reason, _ := readyConditionForListeners(1, errors.New("boom"), v2alpha1.TemplateSourceRef)
		assert.True(t, ready)
		assert.Equal(t, v2alpha1.ReasonListenerActive, reason)
	})

	t.Run("no listeners, no error ⇒ Ready=False/NoActiveSessions", func(t *testing.T) {
		ready, reason, _ := readyConditionForListeners(0, nil, v2alpha1.TemplateSourceRef)
		assert.False(t, ready)
		assert.Equal(t, v2alpha1.ReasonNoActiveSessions, reason)
	})
}

func TestRunnerSetBaselineRecheckInterval(t *testing.T) {
	r := &RunnerSetReconciler{}
	assert.Equal(t, defaultBaselineRecheckInterval, r.baselineRecheckInterval())
	r.BaselineRecheckInterval = 3
	assert.Equal(t, 3, int(r.baselineRecheckInterval()))
}
