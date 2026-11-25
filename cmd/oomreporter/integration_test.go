//go:build integration

package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/actions-gateway/github-actions-gateway/oomreporter/internal/cgroup"
	"github.com/actions-gateway/github-actions-gateway/oomreporter/internal/report"
)

const (
	release    = "oom-reporter"
	reporter   = "oom-system"
	tenant     = "team-a"
	digest     = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	apiTimeout = 30 * time.Second
)

// cluster is an API server with the chart's grants and policies on it, as helm renders them.
type cluster struct {
	cfg        *rest.Config
	admin      kubernetes.Interface
	agent      appsv1.DaemonSet
	controller appsv1.Deployment
}

var shared *cluster

func TestMain(m *testing.M) {
	ctrl.SetLogger(logr.Discard())
	code, err := withCluster(m.Run)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
}

func withCluster(run func() int) (int, error) {
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		return 0, err
	}
	defer func() { _ = env.Stop() }()
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return 0, err
	}
	shared = &cluster{cfg: cfg, admin: kube}
	if err := shared.install(context.Background()); err != nil {
		return 0, err
	}
	return run(), nil
}

func (c *cluster) install(ctx context.Context) error {
	for _, name := range []string{reporter, tenant} {
		if _, err := c.admin.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}, metav1.CreateOptions{}); err != nil {
			return err
		}
	}
	for _, name := range []string{"node-a", "node-b"} {
		if _, err := c.admin.CoreV1().Nodes().Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}, metav1.CreateOptions{}); err != nil {
			return err
		}
	}
	// Read by this process, so that the test cache sees a change of it.
	dir, err := os.MkdirTemp("", "chart-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := os.CopyFS(dir, os.DirFS("testdata/chart")); err != nil {
		return err
	}
	helm := exec.CommandContext(ctx, "helm", "template", release, ".",
		"--namespace", reporter, "--set", "namespaces={"+tenant+"}", "--set", "image.digest="+digest)
	helm.Dir = dir
	out, err := helm.Output()
	if err != nil {
		if exit, ok := errors.AsType[*exec.ExitError](err); ok {
			return fmt.Errorf("helm template: %w: %s", err, exit.Stderr)
		}
		return fmt.Errorf("helm template: %w", err)
	}
	objects, err := client.New(c.cfg, client.Options{})
	if err != nil {
		return err
	}
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(out), 4096)
	for {
		var u unstructured.Unstructured
		if err := dec.Decode(&u); errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return err
		}
		if len(u.Object) == 0 {
			continue
		}
		var err error
		switch u.GetKind() {
		case "DaemonSet":
			err = runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &c.agent)
		case "Deployment":
			err = runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &c.controller)
		}
		if err == nil {
			err = objects.Create(ctx, &u)
		}
		if err != nil {
			return fmt.Errorf("%s %s: %w", u.GetKind(), u.GetName(), err)
		}
	}
}

// as is a client of a service account of the reporter's namespace, with a token as a pod of it on
// node has, or one of no pod where node is empty.
func (c *cluster) as(t *testing.T, account, node string) (*rest.Config, kubernetes.Interface) {
	t.Helper()
	req := &authenticationv1.TokenRequest{}
	if node != "" {
		p, err := c.admin.CoreV1().Pods(reporter).Create(t.Context(), &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "of-"},
			Spec:       corev1.PodSpec{NodeName: node, ServiceAccountName: account, Containers: []corev1.Container{{Name: "x", Image: "x"}}},
		}, metav1.CreateOptions{})
		require.NoError(t, err)
		req.Spec.BoundObjectRef = &authenticationv1.BoundObjectReference{Kind: "Pod", APIVersion: "v1", Name: p.Name, UID: p.UID}
	}
	token, err := c.admin.CoreV1().ServiceAccounts(reporter).CreateToken(t.Context(), account, req, metav1.CreateOptions{})
	require.NoError(t, err)
	cfg := rest.AnonymousClientConfig(c.cfg)
	cfg.BearerToken = token.Status.Token
	kube, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err)
	return cfg, kube
}

// grant has an account of the reporter's namespace hold more than the chart gives it, in ns.
func (c *cluster) grant(t *testing.T, ns, account string, rules ...rbacv1.PolicyRule) {
	t.Helper()
	role, err := c.admin.RbacV1().Roles(ns).Create(t.Context(), &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{GenerateName: "more-"}, Rules: rules}, metav1.CreateOptions{})
	require.NoError(t, err)
	binding, err := c.admin.RbacV1().RoleBindings(ns).Create(t.Context(), &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "more-"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role.Name},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: account, Namespace: reporter}},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, c.admin.RbacV1().RoleBindings(ns).Delete(context.WithoutCancel(t.Context()), binding.Name, metav1.DeleteOptions{}))
	})
}

// dry is for what is tried until it is refused, since one let through would leave nothing to refuse.
var dry = []string{metav1.DryRunAll}

// enforced waits until do is refused by a policy, which is a while after its binding is made.
func enforced(t *testing.T, do func() error) {
	t.Helper()
	require.Eventually(t, func() bool { return refusal(do()) != "" }, apiTimeout, 100*time.Millisecond)
}

func reportsGuarded(t *testing.T) {
	t.Helper()
	enforced(t, func() error {
		_, err := shared.admin.CoreV1().ConfigMaps(reporter).Create(t.Context(), configMap(report.Name("none")), metav1.CreateOptions{DryRun: dry})
		return err
	})
}

// refusal is what a policy of the chart said, or empty where none refused.
func refusal(err error) string {
	if err != nil && apierrors.IsForbidden(err) && strings.Contains(err.Error(), "ValidatingAdmissionPolicy '"+release) {
		return err.Error()
	}
	return ""
}

const (
	agentAccount      = release + "-agent"
	controllerAccount = release + "-controller"
)

func configMap(name string, with ...func(*corev1.ConfigMap)) *corev1.ConfigMap {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: reporter, Name: name}, Data: map[string]string{report.Key: ""}}
	for _, w := range with {
		w(cm)
	}
	return cm
}

func TestPolicy_AReportIsWrittenOnlyByItsNodesAgent(t *testing.T) {
	ctx, c := t.Context(), shared
	_, controller := c.as(t, controllerAccount, "")
	for _, node := range []string{"node-a", "node-b"} {
		_, err := controller.CoreV1().ConfigMaps(reporter).Create(ctx, configMap(report.Name(node)), metav1.CreateOptions{})
		require.NoError(t, err)
		t.Cleanup(func() {
			assert.NoError(t, controller.CoreV1().ConfigMaps(reporter).Delete(context.WithoutCancel(ctx), report.Name(node), metav1.DeleteOptions{}))
		})
	}
	_, err := c.admin.CoreV1().ConfigMaps(reporter).Create(ctx, configMap("another"), metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = c.admin.CoreV1().ServiceAccounts(reporter).Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "other"}}, metav1.CreateOptions{})
	require.NoError(t, err)
	c.grant(t, reporter, "other", rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"*"}})
	c.grant(t, reporter, agentAccount, rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"create", "patch"}})

	_, agentA := c.as(t, agentAccount, "node-a")
	_, unbound := c.as(t, agentAccount, "")
	_, other := c.as(t, "other", "node-a")
	put := func(kube kubernetes.Interface, cm *corev1.ConfigMap) error {
		_, err := kube.CoreV1().ConfigMaps(reporter).Update(ctx, cm, metav1.UpdateOptions{})
		return err
	}
	create := func(kube kubernetes.Interface, cm *corev1.ConfigMap) error {
		_, err := kube.CoreV1().ConfigMaps(reporter).Create(ctx, cm, metav1.CreateOptions{})
		return err
	}
	reportsGuarded(t)
	const notItsNodes, notAReport, kept = "whose token names its node", "no ConfigMap but its node's report", "no immutable, finalizers or ownerReferences"
	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"its own":                        {put(agentA, configMap(report.Name("node-a"))), ""},
		"another node's":                 {put(agentA, configMap(report.Name("node-b"))), notItsNodes},
		"with no node in its token":      {put(unbound, configMap(report.Name("node-a"))), notItsNodes},
		"another's":                      {put(agentA, configMap("another")), notAReport},
		"a new one that is not a report": {create(agentA, configMap("new")), notAReport},
		"a new one, of another node":     {create(agentA, configMap(report.Name("node-c"))), notItsNodes},
		"with an owner": {put(agentA, configMap(report.Name("node-a"), func(cm *corev1.ConfigMap) {
			cm.OwnerReferences = []metav1.OwnerReference{{APIVersion: "v1", Kind: "Node", Name: "node-a", UID: "x"}}
		})), kept},
		"with a finalizer":                {put(agentA, configMap(report.Name("node-a"), func(cm *corev1.ConfigMap) { cm.Finalizers = []string{"example.com/x"} })), kept},
		"that may not change again":       {put(agentA, configMap(report.Name("node-a"), func(cm *corev1.ConfigMap) { cm.Immutable = new(true) })), kept},
		"by another, on that node":        {put(other, configMap(report.Name("node-a"))), notItsNodes},
		"made by another, on that node":   {create(other, configMap(report.Name("node-c"))), notItsNodes},
		"by an administrator":             {put(c.admin, configMap(report.Name("node-a"))), notItsNodes},
		"another's, by its own":           {put(other, configMap("another")), ""},
		"made by another, of no report":   {create(other, configMap("others")), ""},
		"made by another, for good":       {create(other, configMap("kept", func(cm *corev1.ConfigMap) { cm.Immutable = new(true) })), ""},
		"made by another, of near a name": {create(other, configMap("oom-reports")), ""},
	} {
		t.Run(name, func(t *testing.T) {
			if tc.want == "" {
				require.NoError(t, tc.err)
				return
			}
			assert.Contains(t, refusal(tc.err), tc.want)
		})
	}
	// With RBAC as the chart has it, and no more.
	require.NoError(t, put(agentA, configMap(report.Name("node-a"))))
	_, err = agentA.CoreV1().ConfigMaps(reporter).Get(ctx, report.Name("node-a"), metav1.GetOptions{})
	assert.True(t, apierrors.IsForbidden(err))
	_, err = agentA.CoreV1().Pods(tenant).List(ctx, metav1.ListOptions{})
	assert.True(t, apierrors.IsForbidden(err))
}

// The control plane writes it to each namespace, and both components' tokens are mounted with it.
func TestPolicy_TheRootCAIsStillPublished(t *testing.T) {
	reportsGuarded(t)
	cfg := rest.CopyConfig(shared.cfg)
	cfg.Impersonate = rest.ImpersonationConfig{UserName: "system:serviceaccount:kube-system:root-ca-cert-publisher"}
	publisher, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err)
	cms := publisher.CoreV1().ConfigMaps(reporter)
	cm, err := cms.Create(t.Context(), configMap("kube-root-ca.crt"), metav1.CreateOptions{})
	require.NoError(t, err)
	cm.Data["ca.crt"] = "changed"
	_, err = cms.Update(t.Context(), cm, metav1.UpdateOptions{})
	require.NoError(t, err)
}

func (c *cluster) worker(t *testing.T, name string) *corev1.Pod {
	t.Helper()
	pods := c.admin.CoreV1().Pods(tenant)
	p, err := pods.Create(t.Context(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"a": "b"}, Annotations: map[string]string{"a": "b"}},
		Spec:       corev1.PodSpec{NodeName: "node-a", Containers: []corev1.Container{{Name: "runner", Image: "x"}, {Name: "dind", Image: "x"}}},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, pods.Delete(context.WithoutCancel(t.Context()), name, metav1.DeleteOptions{GracePeriodSeconds: new(int64(0))}))
	})
	p.Status.Phase = corev1.PodRunning
	p.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.PodReady, Status: corev1.ConditionFalse, Reason: "Is"},
		{Type: "not.oom.actions-gateway.com/ours", Status: corev1.ConditionTrue},
	}
	p, err = pods.UpdateStatus(t.Context(), p, metav1.UpdateOptions{})
	require.NoError(t, err)
	return p
}

func TestPolicy_TheControllerChangesNothingOfAPodButItsConditions(t *testing.T) {
	ctx, c := t.Context(), shared
	_, controller := c.as(t, controllerAccount, "")
	p := c.worker(t, "guarded")
	patch := func(kube kubernetes.Interface, kind types.PatchType, body string) error {
		_, err := kube.CoreV1().Pods(tenant).Patch(ctx, p.Name, kind, []byte(body), metav1.PatchOptions{}, "status")
		return err
	}
	merge := func(body string) error { return patch(controller, types.StrategicMergePatchType, body) }
	condition := func(of, status string) string {
		return fmt.Sprintf(`{"status":{"conditions":[{"type":%q,"status":%q,"reason":"R"}]}}`, of, status)
	}
	enforced(t, func() error {
		_, err := controller.CoreV1().Pods(tenant).Patch(ctx, p.Name, types.MergePatchType, []byte(`{"metadata":{"labels":{"c":"d"}}}`), metav1.PatchOptions{DryRun: dry}, "status")
		return err
	})

	for _, of := range []string{"runner", strings.Repeat("a", 63)} {
		for _, status := range []string{"True", "Unknown", "True"} {
			require.NoError(t, merge(condition("oom.actions-gateway.com/"+of, status)))
		}
	}
	got, err := c.admin.CoreV1().Pods(tenant).Get(ctx, p.Name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Len(t, got.Status.Conditions, 4)
	require.NoError(t, merge(`{"status":{"conditions":[{"$patch":"delete","type":"oom.actions-gateway.com/runner"}]}}`))

	const conditions = "no pod condition outside oom.actions-gateway.com"
	for body, want := range map[string]string{
		condition("Ready", "True"):                                                                  conditions,
		condition("DisruptionTarget", "True"):                                                       conditions,
		condition("oom.actions-gateway.com", "True"):                                                conditions,
		condition("oom.actions-gateway.com.evil/x", "True"):                                         conditions,
		condition("x.oom.actions-gateway.com/x", "True"):                                            conditions,
		`{"status":{"conditions":[{"$patch":"delete","type":"not.oom.actions-gateway.com/ours"}]}}`: conditions,
		`{"status":{"conditions":[{"$patch":"delete","type":"Ready"}]}}`:                            conditions,
		`{"metadata":{"labels":{"c":"d"}}}`:                                                         "metadata.labels",
		`{"metadata":{"labels":null}}`:                                                              "metadata.labels",
		`{"metadata":{"annotations":{"a":"c"}}}`:                                                    "metadata.annotations",
		`{"metadata":{"finalizers":["example.com/x"]}}`:                                             "metadata.finalizers",
		`{"status":{"phase":"Failed"}}`:                                                             "status.phase",
		`{"status":{"message":"m"}}`:                                                                "status.message",
		`{"status":{"reason":"Evicted"}}`:                                                           "status.reason",
		`{"status":{"hostIP":"10.0.0.1","hostIPs":[{"ip":"10.0.0.1"}]}}`:                            "status.hostIP",
		`{"status":{"podIP":"10.0.0.2","podIPs":[{"ip":"10.0.0.2"}]}}`:                              "status.podIP",
		`{"status":{"startTime":"2026-11-25T00:00:00Z"}}`:                                           "status.startTime",
		`{"status":{"containerStatuses":[{"name":"runner","image":"x","imageID":"x","ready":false,"restartCount":0,"state":{"terminated":{"exitCode":137,"reason":"OOMKilled"}}}]}}`: "status.containerStatuses",
		`{"status":{"initContainerStatuses":[{"name":"i","image":"x","imageID":"x","ready":false,"restartCount":0}]}}`:                                                               "status.initContainerStatuses",
	} {
		t.Run(body, func(t *testing.T) {
			err := merge(body)
			assert.Contains(t, refusal(err), want, "%v", err)
		})
	}
	// One beside it, and the same again.
	assert.Contains(t, refusal(patch(controller, types.JSONPatchType, `[{"op":"add","path":"/status/conditions/-","value":{"type":"Ready","status":"False","reason":"Is"}}]`)), conditions)
	// The API server takes two of one type, and neither may become the other.
	for _, was := range [][2]corev1.ConditionStatus{{corev1.ConditionFalse, corev1.ConditionTrue}, {corev1.ConditionFalse, corev1.ConditionFalse}} {
		doubled := []corev1.PodCondition{{Type: corev1.PodReady, Status: was[0]}, {Type: corev1.PodReady, Status: was[1]}}
		got, err := c.admin.CoreV1().Pods(tenant).Get(ctx, p.Name, metav1.GetOptions{})
		require.NoError(t, err)
		got.Status.Conditions = doubled
		got, err = c.admin.CoreV1().Pods(tenant).UpdateStatus(ctx, got, metav1.UpdateOptions{})
		require.NoError(t, err)
		require.Len(t, got.Status.Conditions, 2)
		to := map[corev1.ConditionStatus]string{corev1.ConditionTrue: "False", corev1.ConditionFalse: "True"}[was[1]]
		assert.Contains(t, refusal(patch(controller, types.JSONPatchType, `[{"op":"replace","path":"/status/conditions/1/status","value":"`+to+`"}]`)), conditions)
	}
	// None is refused what it may do already.
	require.NoError(t, patch(c.admin, types.StrategicMergePatchType, `{"metadata":{"labels":{"c":"d"}},"status":{"phase":"Failed","conditions":[{"type":"Ready","status":"False","reason":"Was"}]}}`))
	require.NoError(t, merge(condition("oom.actions-gateway.com/dind", "True")), "of a pod that ended")
	// RBAC, beside the policy.
	_, err = controller.CoreV1().Pods(tenant).Patch(ctx, p.Name, types.MergePatchType, []byte(`{"metadata":{"labels":{"e":"f"}}}`), metav1.PatchOptions{})
	assert.True(t, apierrors.IsForbidden(err))
	_, err = controller.CoreV1().Pods("default").List(ctx, metav1.ListOptions{})
	assert.True(t, apierrors.IsForbidden(err))
}

func TestPolicy_TheControllersEventsCarryItsOwnReasons(t *testing.T) {
	ctx, c := t.Context(), shared
	c.grant(t, tenant, controllerAccount, rbacv1.PolicyRule{APIGroups: []string{"", "events.k8s.io"}, Resources: []string{"events"}, Verbs: []string{"create", "update", "patch"}})
	_, controller := c.as(t, controllerAccount, "")
	about := corev1.ObjectReference{Kind: "Pod", Namespace: tenant, Name: "x"}
	create := func(kube kubernetes.Interface, reason string) (string, error) {
		e, err := kube.CoreV1().Events(tenant).Create(ctx, &corev1.Event{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "x-", Namespace: tenant}, InvolvedObject: about, Reason: reason, Type: "Warning",
		}, metav1.CreateOptions{})
		return e.GetName(), err
	}
	createNew := func(reason string) error {
		_, err := controller.EventsV1().Events(tenant).Create(ctx, &eventsv1.Event{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "x-", Namespace: tenant}, Regarding: about, Reason: reason, Type: "Warning",
			EventTime: metav1.NowMicro(), ReportingController: "x", ReportingInstance: "x", Action: "x",
		}, metav1.CreateOptions{})
		return err
	}
	enforced(t, func() error {
		_, err := controller.CoreV1().Events(tenant).Create(ctx, &corev1.Event{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "x-", Namespace: tenant}, InvolvedObject: about, Reason: "NotTriggerScaleUp", Type: "Warning",
		}, metav1.CreateOptions{DryRun: dry})
		return err
	})
	const want = "Events of its own reasons alone"
	var made string
	for _, reason := range []string{"OOMContainerLimit", "OOMPodLimit", "OOMPodsLimit", "OOMAbovePods", "OOMUnknown", "OOMProcessKilled"} {
		var err error
		made, err = create(controller, reason)
		require.NoError(t, err)
		require.NoError(t, createNew(reason))
	}
	for _, reason := range []string{"NotTriggerScaleUp", "TriggeredScaleUp", "OOM", "OOMKilled", "", "oomunknown"} {
		_, err := create(controller, reason)
		assert.Contains(t, refusal(err), want, reason)
		if reason != "" {
			assert.Contains(t, refusal(createNew(reason)), want, reason)
		}
	}
	_, err := controller.CoreV1().Events(tenant).Patch(ctx, made, types.MergePatchType, []byte(`{"reason":"TriggeredScaleUp"}`), metav1.PatchOptions{})
	assert.Contains(t, refusal(err), want)
	_, err = create(c.admin, "TriggeredScaleUp")
	require.NoError(t, err)
}

// counted counts the writes a client makes.
type counted struct {
	http.RoundTripper
	patches, events atomic.Int32
}

func (c *counted) RoundTrip(r *http.Request) (*http.Response, error) {
	res, err := c.RoundTripper.RoundTrip(r)
	if err == nil && res.StatusCode < 300 {
		switch {
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/status"):
			c.patches.Add(1)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/events"):
			c.events.Add(1)
		}
	}
	return res, err
}

func (c *cluster) ended(t *testing.T, p *corev1.Pod) {
	t.Helper()
	p.Status.Phase = corev1.PodFailed
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: "runner", Image: "x", ImageID: "x", ContainerID: "containerd://" + runnerID, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			Reason: "OOMKilled", ExitCode: 137, ContainerID: "containerd://" + runnerID, StartedAt: metav1.Now(), FinishedAt: metav1.Now(),
		}},
	}}
	_, err := c.admin.CoreV1().Pods(tenant).UpdateStatus(t.Context(), p, metav1.UpdateOptions{})
	require.NoError(t, err)
}

// start runs the controller as the chart's, and the pod of an agent of node-a, and returns the
// client of that agent and the count of what the controller writes.
func (c *cluster) start(t *testing.T) (*rest.Config, *counted) {
	t.Helper()
	_, err := c.admin.CoreV1().Pods(reporter).Create(t.Context(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-" + strings.ToLower(strings.ReplaceAll(t.Name(), "_", "-")), Labels: c.agent.Spec.Template.Labels},
		Spec:       corev1.PodSpec{NodeName: "node-a", Containers: []corev1.Container{{Name: "x", Image: "x"}}},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	agentCfg, _ := c.as(t, agentAccount, "node-a")
	cfg, _ := c.as(t, controllerAccount, "")
	n := &counted{}
	cfg.WrapTransport = func(rt http.RoundTripper) http.RoundTripper { n.RoundTripper = rt; return n }
	ctx, stop := context.WithCancel(t.Context())
	done := make(chan error)
	go func() {
		done <- run(ctx, c.controller.Spec.Template.Spec.Containers[0].Args, env(map[string]string{"POD_NAMESPACE": reporter}),
			func() (*rest.Config, error) { return cfg, nil }, noLog)
	}()
	t.Cleanup(func() {
		stop()
		assert.NoError(t, <-done)
	})
	require.Eventually(t, func() bool {
		_, err := c.admin.CoreV1().ConfigMaps(reporter).Get(t.Context(), report.Name("node-a"), metav1.GetOptions{})
		return err == nil
	}, apiTimeout, 50*time.Millisecond)
	return agentCfg, n
}

// From a node's counters to the pod, by the programs themselves with the chart's grants.
func TestReporter_SaysOnThePodWhichLimitRanOut(t *testing.T) {
	c := shared
	p := c.worker(t, "killed")
	agentCfg, _ := c.start(t)
	dir, counts := mount(t)
	of, _ := mount(t)
	require.NoError(t, os.Rename(filepath.Join(of, "kubepods/burstable/pod"+podUID), filepath.Join(dir, "kubepods/burstable/pod"+string(p.UID))))
	const killed = "oom 1\noom_kill 1\noom_group_kill 1\n"
	counts(killed, killed)
	o := agentOptions{node: "node-a", namespace: reporter, mount: dir, poll: 10 * time.Millisecond, mountinfo: info(t, "rw")}
	flags := flag.NewFlagSet("agent", flag.ContinueOnError)
	agentFlags(flags, env(nil))
	require.NoError(t, flags.Parse(c.agent.Spec.Template.Spec.Containers[0].Args[1:]))
	agent, err := kubernetes.NewForConfig(agentCfg)
	require.NoError(t, err)
	ctx, stop := context.WithCancel(t.Context())
	done := make(chan error)
	go func() { done <- o.run(ctx, agent, noLog) }()
	t.Cleanup(func() {
		stop()
		assert.NoError(t, <-done)
	})
	require.Eventually(t, func() bool {
		cm, err := c.admin.CoreV1().ConfigMaps(reporter).Get(ctx, report.Name("node-a"), metav1.GetOptions{})
		return err == nil && strings.Contains(cm.Data[report.Key], podUID)
	}, apiTimeout, 50*time.Millisecond, "another pod's, of before the agent")
	cg := filepath.Join(dir, "kubepods/burstable/pod"+string(p.UID))
	require.NoError(t, os.WriteFile(filepath.Join(cg, runnerID, "memory.events.local"), []byte(killed), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(cg, "memory.events"), []byte(killed), 0o600))
	c.ended(t, p)

	require.Eventually(t, func() bool {
		got, err := c.admin.CoreV1().Pods(tenant).Get(ctx, p.Name, metav1.GetOptions{})
		return err == nil && slices.ContainsFunc(got.Status.Conditions, func(c corev1.PodCondition) bool {
			return c.Type == "oom.actions-gateway.com/runner" && c.Reason == "ContainerLimit"
		})
	}, apiTimeout, 50*time.Millisecond)
	require.Eventually(t, func() bool {
		events, err := c.admin.CoreV1().Events(tenant).List(ctx, metav1.ListOptions{FieldSelector: "involvedObject.uid=" + string(p.UID)})
		return err == nil && len(events.Items) == 1 && events.Items[0].Reason == "OOMContainerLimit"
	}, apiTimeout, 50*time.Millisecond)
}

// An agent that says otherwise each time makes the controller write no more than its limits.
func TestReporter_WritesNoMoreThanItsLimits(t *testing.T) {
	c := shared
	p := c.worker(t, "flapping")
	agentCfg, n := c.start(t)
	agent, err := kubernetes.NewForConfig(agentCfg)
	require.NoError(t, err)
	c.ended(t, p)
	for i := range 40 {
		rise := cgroup.Rise{Confirmed: true, GroupKills: 1, Containers: []string{runnerID}, Read: true}
		if i%2 == 0 {
			rise.Own, rise.Inside = 1, 1
		}
		data, err := report.Encode([]report.Kill{
			{Pod: string(p.UID), At: time.Now().UnixMilli(), Rise: &rise},
			{Pod: string(p.UID), At: time.Now().Add(-time.Duration(i) * 7 * time.Second).UnixMilli(), Processes: uint64(i + 1)},
		})
		require.NoError(t, err)
		_, err = agent.CoreV1().ConfigMaps(reporter).Update(t.Context(), configMap(report.Name("node-a"), func(cm *corev1.ConfigMap) { cm.Data[report.Key] = data }), metav1.UpdateOptions{})
		require.NoError(t, err)
		time.Sleep(100 * time.Millisecond)
	}
	assert.Equal(t, int32(3), n.patches.Load(), "one condition, three times a minute")
	assert.Equal(t, int32(3), n.events.Load(), "one for each of two reasons, and one for the processes")
}
