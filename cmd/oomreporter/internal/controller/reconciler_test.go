package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/actions-gateway/github-actions-gateway/oomreporter/internal/cgroup"
	"github.com/actions-gateway/github-actions-gateway/oomreporter/internal/report"
)

const ownNamespace = "oom-reporter"

var agentLabels = client.MatchingLabels{"app": "agent"}

// world is a Reconciler over a fake API server, and its clock.
type world struct {
	*Reconciler
	now    time.Time
	writes []string
	funcs  interceptor.Funcs
}

func newWorld(t *testing.T, objects ...client.Object) *world {
	t.Helper()
	w := &world{now: killedAt.Add(hold)}
	w.funcs = interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.CreateOption) error {
			w.writes = append(w.writes, "create "+kindOf(o))
			return c.Create(ctx, o, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.DeleteOption) error {
			w.writes = append(w.writes, "delete "+kindOf(o))
			return c.Delete(ctx, o, opts...)
		},
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, o client.Object, p client.Patch, opts ...client.SubResourcePatchOption) error {
			w.writes = append(w.writes, "patch "+kindOf(o)+"/"+sub)
			return c.SubResource(sub).Patch(ctx, o, p, opts...)
		},
	}
	c := fake.NewClientBuilder().WithObjects(objects...).WithStatusSubresource(&corev1.Pod{}).
		WithIndex(&corev1.Pod{}, uidIndex, byUID).WithInterceptorFuncs(w.funcs).Build()
	w.Reconciler = &Reconciler{Client: c, Namespace: ownNamespace, Agents: agentLabels, Now: func() time.Time { return w.now }}
	return w
}

func kindOf(o client.Object) string {
	switch o.(type) {
	case *corev1.Event:
		return "event"
	case *corev1.ConfigMap:
		return "report"
	}
	return "pod"
}

func agentOn(node string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ownNamespace, Name: "agent-" + node, Labels: agentLabels},
		Spec:       corev1.PodSpec{NodeName: node},
	}
}

func reportOf(t *testing.T, node string, kills ...report.Kill) *corev1.ConfigMap {
	t.Helper()
	data, err := report.Encode(kills)
	require.NoError(t, err)
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: ownNamespace, Name: report.Name(node), UID: "report"},
		Data:       map[string]string{report.Key: data},
	}
}

func (w *world) reconcile(t *testing.T, node string) reconcile.Result {
	t.Helper()
	res, err := w.Reconcile(t.Context(), reconcile.Request{NamespacedName: types.NamespacedName{Name: node}})
	require.NoError(t, err)
	return res
}

func (w *world) podNow(t *testing.T) *corev1.Pod {
	t.Helper()
	var p corev1.Pod
	require.NoError(t, w.Get(t.Context(), types.NamespacedName{Namespace: "team-a", Name: "worker"}, &p))
	return &p
}

func (w *world) eventsNow(t *testing.T) []corev1.Event {
	t.Helper()
	var l corev1.EventList
	require.NoError(t, w.List(t.Context(), &l))
	return l.Items
}

func (w *world) reports(t *testing.T) (names []string) {
	t.Helper()
	var l corev1.ConfigMapList
	require.NoError(t, w.List(t.Context(), &l))
	for _, cm := range l.Items {
		names = append(names, cm.Name)
	}
	return names
}

func TestReconcile_SaysOnThePodWhichLimitRanOut(t *testing.T) {
	p := pod(dead("runner", 1, killedAt))
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
	w := newWorld(t, agentOn("node-a"), reportOf(t, "node-a", own(killedAt, 1)), p)
	assert.Zero(t, w.reconcile(t, "node-a"))

	got := w.podNow(t)
	require.Len(t, got.Status.Conditions, 2)
	c, other := got.Status.Conditions[0], got.Status.Conditions[1]
	assert.Equal(t, corev1.PodReady, other.Type)
	assert.Equal(t, corev1.PodConditionType("oom.actions-gateway.com/runner"), c.Type)
	assert.Equal(t, corev1.ConditionTrue, c.Status)
	assert.Equal(t, "ContainerLimit", c.Reason)
	assert.Equal(t, "The container's own memory limit ran out, or those of the pod's containers together. "+
		"It ended at 2027-01-15T08:00:00Z, container ID "+id(1)+".", c.Message)
	assert.True(t, c.LastTransitionTime.Time.Equal(w.now))

	events := w.eventsNow(t)
	require.Len(t, events, 1)
	e := events[0]
	assert.Equal(t, "team-a", e.Namespace)
	assert.Equal(t, corev1.EventTypeWarning, e.Type)
	assert.Equal(t, "OOMContainerLimit", e.Reason)
	assert.Equal(t, "Container runner: The container's own memory limit ran out, or those of the pod's containers together.", e.Message)
	assert.Equal(t, corev1.ObjectReference{
		APIVersion: "v1", Kind: "Pod", Namespace: "team-a", Name: "worker", UID: podUID, FieldPath: "spec.containers{runner}",
	}, e.InvolvedObject)
	assert.True(t, e.LastTimestamp.Time.Equal(killedAt))

	w.writes = nil
	w.reconcile(t, "node-a")
	assert.Empty(t, w.writes)
}

func TestReconcile_WaitsForThePodsStatus(t *testing.T) {
	p := pod(alive("runner", 1))
	w := newWorld(t, agentOn("node-a"), reportOf(t, "node-a", own(killedAt, 1)), p)
	w.reconcile(t, "node-a")
	assert.Empty(t, w.writes)

	p = w.podNow(t)
	p.Status = pod(dead("runner", 1, killedAt)).Status
	require.NoError(t, w.Status().Update(t.Context(), p))
	w.writes = nil
	w.reconcile(t, "node-a")
	assert.Equal(t, []string{"patch pod/status", "create event"}, w.writes)
}

func TestReconcile_AConditionThatDiffersIsWrittenAgain(t *testing.T) {
	p := pod(dead("runner", 2, killedAt))
	was := corev1.PodCondition{
		Type: "oom.actions-gateway.com/runner", Status: corev1.ConditionTrue, Reason: "ContainerLimit", Message: "an earlier kill's",
		LastTransitionTime: metav1.NewTime(killedAt.Add(-time.Hour)),
	}
	for name, tc := range map[string]struct {
		kill          report.Kill
		reason        string
		status        corev1.ConditionStatus
		sameTransiton bool
	}{
		"for another kill of one reason": {own(killedAt, 2), "ContainerLimit", corev1.ConditionTrue, true},
		"of another reason":              {above(killedAt, 1), "AbovePods", corev1.ConditionTrue, true},
		"that claims nothing":            {above(killedAt, 2), "Unknown", corev1.ConditionUnknown, false},
	} {
		t.Run(name, func(t *testing.T) {
			p := p.DeepCopy()
			p.Status.Conditions = []corev1.PodCondition{was}
			w := newWorld(t, agentOn("node-a"), reportOf(t, "node-a", tc.kill), p)
			w.reconcile(t, "node-a")
			got := w.podNow(t).Status.Conditions
			require.Len(t, got, 1)
			assert.Equal(t, tc.reason, got[0].Reason)
			assert.Equal(t, tc.status, got[0].Status)
			assert.Equal(t, tc.sameTransiton, got[0].LastTransitionTime.Time.Equal(was.LastTransitionTime.Time))
		})
	}
}

func TestReconcile_AConditionOfOursThatAnotherChangedIsWrittenAgain(t *testing.T) {
	for name, change := range map[string]func(c *corev1.PodCondition){
		"its status":  func(c *corev1.PodCondition) { c.Status = corev1.ConditionFalse },
		"its reason":  func(c *corev1.PodCondition) { c.Reason = "AbovePods" },
		"its message": func(c *corev1.PodCondition) { c.Message = "the node's" },
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t, agentOn("node-a"), reportOf(t, "node-a", own(killedAt, 1)), pod(dead("runner", 1, killedAt)))
			w.reconcile(t, "node-a")
			p := w.podNow(t)
			want := p.Status.Conditions[0]
			change(&p.Status.Conditions[0])
			require.NoError(t, w.Status().Update(t.Context(), p))
			w.reconcile(t, "node-a")
			got := w.podNow(t).Status.Conditions
			require.Len(t, got, 1)
			assert.Equal(t, []string{string(want.Status), want.Reason, want.Message}, []string{string(got[0].Status), got[0].Reason, got[0].Message})
		})
	}
}

func TestReconcile_EachContainerHasItsCondition(t *testing.T) {
	w := newWorld(t, agentOn("node-a"), reportOf(t, "node-a", own(killedAt, 1), own(killedAt, 2)),
		pod(dead("runner", 1, killedAt), dead("dind", 2, killedAt)))
	w.reconcile(t, "node-a")
	var types []string
	for _, c := range w.podNow(t).Status.Conditions {
		types = append(types, string(c.Type))
	}
	assert.ElementsMatch(t, []string{"oom.actions-gateway.com/runner", "oom.actions-gateway.com/dind"}, types)
}

// An agent is held to its own node's pods.
func TestReconcile_DropsAKillOfAPodThatIsOnAnotherNode(t *testing.T) {
	w := newWorld(t, agentOn("node-b"), reportOf(t, "node-b", own(killedAt, 1), report.Kill{Pod: podUID, At: killedAt.UnixMilli(), Processes: 1}),
		pod(dead("runner", 1, killedAt)))
	w.reconcile(t, "node-b")
	assert.Empty(t, w.writes)
}

func TestReconcile_WritesOnlyInItsNamespaces(t *testing.T) {
	w := newWorld(t, agentOn("node-a"), reportOf(t, "node-a", own(killedAt, 1)), pod(dead("runner", 1, killedAt)))
	w.Namespaces = map[string]bool{"team-b": true}
	w.reconcile(t, "node-a")
	assert.Empty(t, w.writes)

	w.Namespaces["team-a"] = true
	w.reconcile(t, "node-a")
	assert.NotEmpty(t, w.writes)
}

func TestReconcile_SkipsWhatIsNoReportAndWhatNamesNoPod(t *testing.T) {
	bad := reportOf(t, "node-a")
	bad.Data[report.Key] = `{"version":2}`
	empty := reportOf(t, "node-a")
	empty.Data = nil
	gone := own(killedAt, 1)
	gone.Pod = "6fa459ea-ee8a-3ca4-894e-db77e160355e"
	for name, cm := range map[string]*corev1.ConfigMap{"another version": bad, "an empty one": empty, "a pod that is gone": reportOf(t, "node-a", gone)} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t, agentOn("node-a"), cm, pod(dead("runner", 1, killedAt)))
			w.reconcile(t, "node-a")
			assert.Empty(t, w.writes)
		})
	}
}

func TestReconcile_MakesAReportForANodeWithAnAgent(t *testing.T) {
	w := newWorld(t, agentOn("node-a"))
	w.reconcile(t, "node-a")
	assert.Equal(t, []string{"oom-report-node-a"}, w.reports(t))
	w.reconcile(t, "node-b")
	assert.Equal(t, []string{"oom-report-node-a"}, w.reports(t))
}

func TestReconcile_AnAgentIsInTheControllersNamespaceUnderItsLabels(t *testing.T) {
	foreign, plain := agentOn("node-a"), agentOn("node-b")
	foreign.Namespace = "team-a"
	plain.Labels = nil
	w := newWorld(t, foreign, plain)
	w.reconcile(t, "node-a")
	w.reconcile(t, "node-b")
	assert.Empty(t, w.reports(t))
}

func TestReconcile_TakesAReportMadeAMomentAgoForMade(t *testing.T) {
	w := newWorld(t, agentOn("node-a"))
	w.Client = interceptor.NewClient(w.Client.(client.WithWatch), interceptor.Funcs{
		Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
			return apierrors.NewAlreadyExists(schema.GroupResource{Resource: "configmaps"}, "oom-report-node-a")
		},
	})
	w.reconcile(t, "node-a")
}

func TestReconcile_DeletesTheReportOfANodeLongWithoutAnAgent(t *testing.T) {
	w := newWorld(t, reportOf(t, "node-a", own(killedAt, 1)), pod(dead("runner", 1, killedAt)))
	assert.Equal(t, idleFor, w.reconcile(t, "node-a").RequeueAfter)
	assert.Equal(t, []string{"patch pod/status", "create event"}, w.writes, "what it says is said meanwhile")

	w.now = w.now.Add(idleFor - time.Second)
	assert.Equal(t, time.Second, w.reconcile(t, "node-a").RequeueAfter)
	require.NoError(t, w.Create(t.Context(), agentOn("node-a")))
	assert.Zero(t, w.reconcile(t, "node-a").RequeueAfter)
	require.NoError(t, w.Delete(t.Context(), agentOn("node-a")))
	w.now = w.now.Add(time.Second)
	assert.Equal(t, idleFor, w.reconcile(t, "node-a").RequeueAfter, "counted anew")

	w.now = w.now.Add(idleFor)
	assert.Zero(t, w.reconcile(t, "node-a").RequeueAfter)
	assert.Empty(t, w.reports(t))
	assert.Empty(t, w.idle)
}

func TestReconcile_LeavesAReportThatIsMadeAgain(t *testing.T) {
	w := newWorld(t, reportOf(t, "node-a"))
	w.reconcile(t, "node-a")
	w.now = w.now.Add(idleFor)
	var opts client.DeleteOptions
	w.Client = interceptor.NewClient(w.Client.(client.WithWatch), interceptor.Funcs{
		Delete: func(_ context.Context, _ client.WithWatch, _ client.Object, o ...client.DeleteOption) error {
			opts.ApplyOptions(o)
			return nil
		},
	})
	w.reconcile(t, "node-a")
	require.NotNil(t, opts.Preconditions)
	assert.Equal(t, types.UID("report"), *opts.Preconditions.UID)
}

func TestWriteEvent_OneAMinuteForAContainerAndOneForNone(t *testing.T) {
	stray := func(at time.Time) report.Kill { return report.Kill{Pod: podUID, At: at.UnixMilli(), Processes: 1} }
	w := newWorld(t, agentOn("node-a"),
		reportOf(t, "node-a", own(killedAt, 1), own(killedAt, 2), stray(killedAt), stray(killedAt.Add(time.Second)), stray(killedAt.Add(-5*time.Minute))),
		pod(dead("runner", 1, killedAt), dead("dind", 2, killedAt)))
	w.reconcile(t, "node-a")
	count := map[string]int{}
	for _, e := range w.eventsNow(t) {
		count[e.Reason+" "+e.InvolvedObject.FieldPath]++
	}
	assert.Equal(t, map[string]int{
		"OOMContainerLimit spec.containers{runner}": 1, "OOMContainerLimit spec.containers{dind}": 1, "OOMProcessKilled ": 1,
	}, count)
	for _, e := range w.eventsNow(t) {
		if e.Reason == reasonProcess {
			assert.True(t, e.LastTimestamp.Time.Equal(killedAt.Add(time.Second)), "the newest")
		}
	}
}

// One of a time that no Event may be of stands for no other.
func TestWriteEvent_AProcessKilledAtNoTimeHidesNoOther(t *testing.T) {
	stray := func(at time.Time) report.Kill { return report.Kill{Pod: podUID, At: at.UnixMilli(), Processes: 1} }
	for name, tc := range map[string]struct {
		others []time.Duration
		want   int
	}{
		"one to come":                  {[]time.Duration{skew + time.Millisecond, 0}, 1},
		"one as far ahead as it may":   {[]time.Duration{skew}, 1},
		"one to come alone":            {[]time.Duration{skew + time.Millisecond}, 0},
		"one as long ago as it may be": {[]time.Duration{-eventWithin}, 1},
		"one longer ago":               {[]time.Duration{-eventWithin - time.Millisecond}, 0},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t, agentOn("node-a"), reportOf(t, "node-a"), pod(alive("runner", 1)))
			var kills []report.Kill
			for _, d := range tc.others {
				kills = append(kills, stray(w.now.Add(d)))
			}
			require.NoError(t, w.Update(t.Context(), reportOf(t, "node-a", kills...)))
			w.reconcile(t, "node-a")
			assert.Len(t, w.eventsNow(t), tc.want)
		})
	}
}

// The time of a kill that ended no container is the report's, and that of another its status's.
func TestWriteEvent_AReportsTimesMakeNoMoreEvents(t *testing.T) {
	w := newWorld(t, agentOn("node-a"), reportOf(t, "node-a"), pod(dead("runner", 1, killedAt)))
	say := func(minutes int) {
		at := w.now.Add(time.Duration(minutes) * time.Minute)
		was := reportOf(t, "node-a")
		require.NoError(t, w.Delete(t.Context(), was))
		next := reportOf(t, "node-a", own(killedAt.Add(time.Duration(minutes)*time.Second), 1), report.Kill{Pod: podUID, At: at.UnixMilli(), Processes: 1})
		require.NoError(t, w.Create(t.Context(), next))
		w.reconcile(t, "node-a")
	}
	for i := range 5 {
		say(-i)
	}
	assert.Len(t, w.eventsNow(t), 2)
	w.now = w.now.Add(writeEvery)
	say(0)
	assert.Len(t, w.eventsNow(t), 3)
}

// The condition says the last, and the Events say each.
func TestWriteEvent_OneForEachReason(t *testing.T) {
	w := newWorld(t, agentOn("node-a"), reportOf(t, "node-a", above(killedAt, 1)), pod(dead("runner", 1, killedAt)))
	w.reconcile(t, "node-a")
	require.NoError(t, w.Update(t.Context(), reportOf(t, "node-a", above(killedAt, 1), own(killedAt, 1))))
	w.reconcile(t, "node-a")
	var reasons []string
	for _, e := range w.eventsNow(t) {
		reasons = append(reasons, e.Reason)
	}
	assert.ElementsMatch(t, []string{"OOMAbovePods", "OOMContainerLimit"}, reasons)
}

func TestWriteEvent_OneForEachEndOfAContainer(t *testing.T) {
	w := newWorld(t, agentOn("node-a"), reportOf(t, "node-a", own(killedAt, 1)), pod(dead("runner", 1, killedAt)))
	w.reconcile(t, "node-a")
	p := w.podNow(t)
	p.Status.ContainerStatuses = []corev1.ContainerStatus{dead("runner", 2, killedAt.Add(time.Second))}
	require.NoError(t, w.Status().Update(t.Context(), p))
	require.NoError(t, w.Update(t.Context(), reportOf(t, "node-a", own(killedAt, 1), own(killedAt.Add(3*time.Second), 2))))
	w.reconcile(t, "node-a")
	var at []time.Time
	for _, e := range w.eventsNow(t) {
		at = append(at, e.LastTimestamp.UTC())
	}
	assert.ElementsMatch(t, []time.Time{killedAt, killedAt.Add(time.Second)}, at)
}

func TestWriteConditions_ThreeAMinute(t *testing.T) {
	w := newWorld(t, agentOn("node-a"), reportOf(t, "node-a"), pod(dead("runner", 1, killedAt), dead("dind", 2, killedAt)))
	say := func(kills ...report.Kill) (patches int, wait time.Duration) {
		w.writes = nil
		require.NoError(t, w.Update(t.Context(), reportOf(t, "node-a", kills...)))
		wait = w.reconcile(t, "node-a").RequeueAfter
		for _, write := range w.writes {
			if write == "patch pod/status" {
				patches++
			}
		}
		return patches, wait
	}
	it := func(t *testing.T, wantPatches int, wantWait time.Duration, kills ...report.Kill) {
		t.Helper()
		patches, wait := say(kills...)
		assert.Equal(t, wantPatches, patches)
		assert.Equal(t, wantWait, wait)
	}
	node := kill(killedAt, cgroup.Rise{Confirmed: true, GroupKills: 1, Containers: []string{id(1)}})
	it(t, 1, 0, own(killedAt, 1))
	w.now = w.now.Add(20 * time.Second)
	it(t, 0, 0, own(killedAt.Add(time.Second), 1)) // The time alone is no change.
	it(t, 1, 0, node)
	it(t, 1, 0, own(killedAt, 1))
	it(t, 0, 40*time.Second, node)
	w.now = w.now.Add(time.Second)
	it(t, 1, 39*time.Second, node, own(killedAt, 2)) // Another container's is not held up.
	it(t, 0, 39*time.Second, node, own(killedAt, 2))
	w.now = w.now.Add(39 * time.Second)
	it(t, 1, 0, node, own(killedAt, 2))
	w.now = w.now.Add(writeEvery)
	it(t, 0, 0, node, own(killedAt, 2))
	assert.Empty(t, w.written)
}

// One kill may be read before the one that ended a container, and be taken for it by its time.
func TestReconcile_WaitsForTheNextReportWhereOnlyTheTimeTells(t *testing.T) {
	first, next := above(killedAt.Add(-3*time.Second), 1), own(killedAt, 1)
	dind := kill(killedAt.Add(-3*time.Second), cgroup.Rise{Confirmed: true, GroupKills: 1, Containers: []string{id(2)}})
	for name, tc := range map[string]struct {
		now   time.Duration // Since the end.
		kills []report.Kill
		wait  time.Duration
		want  []string
	}{
		"at once":                              {0, []report.Kill{first}, hold, nil},
		"until then":                           {hold - time.Millisecond, []report.Kill{first}, time.Millisecond, nil},
		"from then on":                         {hold, []report.Kill{first}, 0, []string{"AbovePods"}},
		"what its counts show does not":        {0, []report.Kill{next}, 0, []string{"ContainerLimit"}},
		"nor what those of it and others show": {0, []report.Kill{first, next}, 0, []string{"ContainerLimit"}},
		"not where the clocks are not one":     {-time.Millisecond, []report.Kill{first}, 0, []string{"AbovePods"}},
		"another's says nothing of it":         {hold, []report.Kill{dind}, 0, nil},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t, agentOn("node-a"), reportOf(t, "node-a", tc.kills...), pod(dead("runner", 1, killedAt)))
			w.now = killedAt.Add(tc.now)
			assert.Equal(t, tc.wait, w.reconcile(t, "node-a").RequeueAfter)
			var got []string
			for _, c := range w.podNow(t).Status.Conditions {
				got = append(got, c.Reason)
			}
			assert.Equal(t, tc.want, got)
			assert.Len(t, w.eventsNow(t), len(tc.want))
		})
	}
}

func TestSooner(t *testing.T) {
	for _, tc := range [][3]time.Duration{{0, 0, 0}, {0, 2, 2}, {2, 0, 2}, {1, 2, 1}, {2, 1, 1}} {
		assert.Equal(t, tc[2], sooner(tc[0], tc[1]))
	}
}

func TestReconcile_AnAnswerStandsAgainstAKillThatSaysNothing(t *testing.T) {
	late := kill(killedAt, cgroup.Rise{Late: true, GroupKills: 1})
	known := func(id string) corev1.PodCondition {
		return corev1.PodCondition{Type: "oom.actions-gateway.com/runner", Status: corev1.ConditionTrue, Reason: "ContainerLimit", Message: "It ended, container ID " + id + "."}
	}
	for name, tc := range map[string]struct {
		was  corev1.PodCondition
		kill report.Kill
		want string
	}{
		"one the agent was late for":           {known(id(1)), late, "ContainerLimit"},
		"one that may be of any time":          {known(id(1)), own(killedAt.Add(time.Hour), 1), "ContainerLimit"},
		"but not that of a container before":   {known(id(2)), late, "Unknown"},
		"nor against one that may be it":       {known(id(1)), above(killedAt, 2), "Unknown"},
		"nor where there is no answer to keep": {corev1.PodCondition{Type: "oom.actions-gateway.com/runner", Status: corev1.ConditionFalse, Reason: "x", Message: "container ID " + id(1)}, late, "Unknown"},
	} {
		t.Run(name, func(t *testing.T) {
			p := pod(dead("runner", 1, killedAt))
			p.Status.Conditions = []corev1.PodCondition{tc.was}
			w := newWorld(t, agentOn("node-a"), reportOf(t, "node-a", tc.kill), p)
			w.reconcile(t, "node-a")
			assert.Equal(t, tc.want, w.podNow(t).Status.Conditions[0].Reason)
		})
	}
}

func TestWriteEvent_NotAgainAfterARestart(t *testing.T) {
	w := newWorld(t, agentOn("node-a"), reportOf(t, "node-a", own(killedAt, 1)), pod(dead("runner", 1, killedAt)))
	w.reconcile(t, "node-a")
	w.events = nil
	w.reconcile(t, "node-a")
	assert.Len(t, w.eventsNow(t), 1)
}

func TestWriteEvent_OnlyOfAKillOfNow(t *testing.T) {
	for name, tc := range map[string]struct {
		now  time.Duration
		want int
	}{
		"as old as may be":   {eventWithin, 1},
		"older":              {eventWithin + time.Second, 0},
		"as far ahead":       {-skew, 1},
		"further ahead":      {-skew - time.Second, 0},
		"of the same moment": {0, 1},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t, agentOn("node-a"), reportOf(t, "node-a", own(killedAt, 1)), pod(dead("runner", 1, killedAt)))
			w.now = killedAt.Add(tc.now)
			w.reconcile(t, "node-a")
			assert.Len(t, w.eventsNow(t), tc.want)
			assert.Len(t, w.podNow(t).Status.Conditions, 1)
		})
	}
}

func TestWriteEvent_ForgetsThoseOfLongAgo(t *testing.T) {
	w := newWorld(t, agentOn("node-a"), reportOf(t, "node-a", own(killedAt, 1)), pod(dead("runner", 1, killedAt)))
	w.reconcile(t, "node-a")
	require.Len(t, w.events, 1)
	w.now = w.now.Add(2*eventWithin + time.Second)
	w.reconcile(t, "node-a")
	assert.Empty(t, w.events)
}

func TestEventName_IsAName(t *testing.T) {
	p := pod()
	p.Name = strings.Repeat("a", 235) + ".-." + strings.Repeat("b", 15)
	name := eventName(p, "spec.containers{runner}", id(1), "OOMUnknown")
	assert.Len(t, name, 235+1+16)
	assert.NotContains(t, name, ".-")
	assert.NotEqual(t, name, eventName(p, "spec.containers{dind}", id(1), "OOMUnknown"))
	assert.NotEqual(t, name, eventName(p, "spec.containers{runner}", id(2), "OOMUnknown"))
	assert.NotEqual(t, name, eventName(p, "spec.containers{runner}", id(1), "OOMAbovePods"))
	p.UID = "another"
	assert.NotEqual(t, name, eventName(p, "spec.containers{runner}", id(1), "OOMUnknown"))
}

// failing has the API server refuse one kind of write with err.
func (w *world) failing(write string, err error) {
	w.Client = interceptor.NewClient(w.Client.(client.WithWatch), interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.CreateOption) error {
			if write == "create "+kindOf(o) {
				return err
			}
			return c.Create(ctx, o, opts...)
		},
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, o client.Object, p client.Patch, opts ...client.SubResourcePatchOption) error {
			if write == "patch "+kindOf(o)+"/"+sub && o.GetName() == "worker" {
				return err
			}
			return c.SubResource(sub).Patch(ctx, o, p, opts...)
		},
	})
}

func TestReconcile_ARefusedWriteIsTriedAgainAndHoldsUpNoOther(t *testing.T) {
	refused := apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "worker", errors.New("by policy"))
	second := pod(dead("runner", 2, killedAt))
	second.Name, second.UID = "worker-2", "6fa459ea-ee8a-3ca4-894e-db77e160355e"
	other := own(killedAt, 2)
	other.Pod = string(second.UID)
	for _, tc := range []struct {
		write string
		err   error
		want  string
	}{
		{"patch pod/status", refused, "by policy"},
		{"patch pod/status", invalid("labels"), "metadata.labels"},
		{"create event", refused, "by policy"},
	} {
		t.Run(tc.write+" "+tc.want, func(t *testing.T) {
			w := newWorld(t, agentOn("node-a"), reportOf(t, "node-a", own(killedAt, 1), other), pod(dead("runner", 1, killedAt)), second)
			w.failing(tc.write, tc.err)
			_, err := w.Reconcile(t.Context(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "node-a"}})
			require.ErrorContains(t, err, tc.want)
			var got corev1.Pod
			require.NoError(t, w.Get(t.Context(), types.NamespacedName{Namespace: "team-a", Name: "worker-2"}, &got))
			assert.Len(t, got.Status.Conditions, 1)
		})
	}
}

func TestReconcile_WhatCannotBeReadIsTriedAgainAndWritesNothing(t *testing.T) {
	refused := apierrors.NewForbidden(schema.GroupResource{}, "", errors.New("no grant"))
	for name, funcs := range map[string]interceptor.Funcs{
		"the agents": {List: func(ctx context.Context, c client.WithWatch, l client.ObjectList, opts ...client.ListOption) error {
			if (&client.ListOptions{}).ApplyOptions(opts).Namespace == ownNamespace {
				return refused
			}
			return c.List(ctx, l, opts...)
		}},
		"the pods": {List: func(ctx context.Context, c client.WithWatch, l client.ObjectList, opts ...client.ListOption) error {
			if (&client.ListOptions{}).ApplyOptions(opts).Namespace != ownNamespace {
				return refused
			}
			return c.List(ctx, l, opts...)
		}},
		"the report": {Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return refused
		}},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t, agentOn("node-a"), reportOf(t, "node-a", own(killedAt, 1)), pod(dead("runner", 1, killedAt)))
			w.Client = interceptor.NewClient(w.Client.(client.WithWatch), funcs)
			_, err := w.Reconcile(t.Context(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "node-a"}})
			require.ErrorContains(t, err, "no grant")
			assert.Empty(t, w.writes)
			assert.Empty(t, w.idle)
		})
	}
}

func TestReconcile_AReportThatCannotBeDeletedIsTriedAgain(t *testing.T) {
	w := newWorld(t, reportOf(t, "node-a"))
	w.reconcile(t, "node-a")
	w.now = w.now.Add(idleFor)
	w.Client = interceptor.NewClient(w.Client.(client.WithWatch), interceptor.Funcs{
		Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
			return errors.New("no grant")
		},
	})
	for range 2 {
		_, err := w.Reconcile(t.Context(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "node-a"}})
		require.ErrorContains(t, err, "no grant")
	}
}

func TestReconcile_ARefusedEventOfNoContainerIsTriedAgain(t *testing.T) {
	w := newWorld(t, agentOn("node-a"), reportOf(t, "node-a", report.Kill{Pod: podUID, At: killedAt.UnixMilli(), Processes: 1}), pod(alive("runner", 1)))
	w.failing("create event", errors.New("by policy"))
	_, err := w.Reconcile(t.Context(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "node-a"}})
	require.ErrorContains(t, err, "by policy")
}

func invalid(name string) error {
	return apierrors.NewInvalid(schema.GroupKind{Kind: "Pod"}, "worker", field.ErrorList{field.Invalid(field.NewPath("metadata", name), "x", "refused")})
}

func TestReconcile_LetsGoOfWhatHasGone(t *testing.T) {
	pods := schema.GroupResource{Resource: "pods"}
	terminating := apierrors.NewForbidden(schema.GroupResource{Resource: "events"}, "x", errors.New("the namespace is being terminated"))
	terminating.ErrStatus.Details.Causes = []metav1.StatusCause{{Type: corev1.NamespaceTerminatingCause}}
	for name, tc := range map[string]struct {
		write string
		err   error
	}{
		"the pod":                          {"patch pod/status", apierrors.NewNotFound(pods, "worker")},
		"the pod, one of its name is here": {"patch pod/status", invalid("uid")},
		"the namespace":                    {"create event", terminating},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t, agentOn("node-a"), reportOf(t, "node-a", own(killedAt, 1)), pod(dead("runner", 1, killedAt)))
			w.failing(tc.write, tc.err)
			w.reconcile(t, "node-a")
		})
	}
}

func TestPatch_IsOfThePodOfThatUIDAndNoMoreThanItsConditions(t *testing.T) {
	w := newWorld(t, agentOn("node-a"), reportOf(t, "node-a", own(killedAt, 1)), pod(dead("runner", 1, killedAt)))
	var body string
	w.Client = interceptor.NewClient(w.Client.(client.WithWatch), interceptor.Funcs{
		SubResourcePatch: func(_ context.Context, _ client.Client, _ string, o client.Object, p client.Patch, _ ...client.SubResourcePatchOption) error {
			assert.Equal(t, types.StrategicMergePatchType, p.Type())
			data, err := p.Data(o)
			body = string(data)
			return err
		},
	})
	w.reconcile(t, "node-a")
	assert.JSONEq(t, `{"metadata":{"uid":"`+podUID+`"},"status":{"conditions":[{
		"type":"oom.actions-gateway.com/runner","status":"True","reason":"ContainerLimit","lastProbeTime":null,
		"lastTransitionTime":"2027-01-15T08:00:10Z",
		"message":"The container's own memory limit ran out, or those of the pod's containers together. It ended at 2027-01-15T08:00:00Z, container ID `+id(1)+`."}]}}`, body)
}

func TestWatches(t *testing.T) {
	w := newWorld(t)
	cm := reportOf(t, "node-a")
	elsewhere := reportOf(t, "node-a")
	elsewhere.Namespace = "team-a"
	root := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: ownNamespace, Name: "kube-root-ca.crt"}}
	nodes := func(reqs []reconcile.Request) (names []string) {
		for _, r := range reqs {
			names = append(names, r.Name)
		}
		return names
	}
	assert.Equal(t, []string{"node-a"}, nodes(w.ofReport(t.Context(), cm)))
	assert.Empty(t, w.ofReport(t.Context(), elsewhere))
	assert.Empty(t, w.ofReport(t.Context(), root))

	assert.Equal(t, []string{"node-b"}, nodes(w.ofPod(t.Context(), agentOn("node-b"))))
	assert.Empty(t, w.ofPod(t.Context(), agentOn("")))
	foreign := agentOn("node-b")
	foreign.Namespace = "team-a"
	assert.Empty(t, w.ofPod(t.Context(), foreign))
	plain := agentOn("node-b")
	plain.Labels = nil
	assert.Empty(t, w.ofPod(t.Context(), plain))

	assert.Equal(t, []string{"node-a"}, nodes(w.ofPod(t.Context(), pod(dead("runner", 1, killedAt)))))
	assert.Empty(t, w.ofPod(t.Context(), pod(alive("runner", 1))))
	failed := pod(dead("runner", 1, killedAt))
	failed.Status.ContainerStatuses[0].State.Terminated.Reason = "Error"
	assert.Empty(t, w.ofPod(t.Context(), failed))
	assert.Empty(t, w.ofPod(t.Context(), cm))
}

func TestReconcile_ALateKillSaysThatNoLimitInsideRanOut(t *testing.T) {
	w := newWorld(t, agentOn("node-a"), reportOf(t, "node-a", kill(killedAt, cgroup.Rise{Late: true, GroupKills: 1})), pod(dead("runner", 1, killedAt)))
	w.reconcile(t, "node-a")
	c := w.podNow(t).Status.Conditions
	require.Len(t, c, 1)
	assert.Equal(t, corev1.ConditionUnknown, c[0].Status)
	assert.Contains(t, c[0].Message, "Which limit ran out is not known. No limit of the pod's, or of a container's, ran out with it. It ended at")
}
