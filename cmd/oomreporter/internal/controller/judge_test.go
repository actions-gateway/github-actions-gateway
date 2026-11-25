package controller

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/actions-gateway/github-actions-gateway/oomreporter/internal/cgroup"
	"github.com/actions-gateway/github-actions-gateway/oomreporter/internal/report"
	"github.com/actions-gateway/github-actions-gateway/oomreporter/internal/rule"
)

const podUID = "1b4e28ba-2fa1-11d2-883f-0016d3cca427"

var killedAt = time.Date(2027, 1, 15, 8, 0, 0, 0, time.UTC)

func id(n int) string { return fmt.Sprintf("%064d", n) }

// over is a container's end as OOMKilled.
func over(n int, at time.Time) *corev1.ContainerStateTerminated {
	return &corev1.ContainerStateTerminated{
		Reason: "OOMKilled", ExitCode: 137, ContainerID: "containerd://" + id(n), FinishedAt: metav1.NewTime(at.Truncate(time.Second)),
	}
}

func dead(name string, n int, at time.Time) corev1.ContainerStatus {
	return corev1.ContainerStatus{
		Name: name, ContainerID: "containerd://" + id(n), State: corev1.ContainerState{Terminated: over(n, at)},
	}
}

func alive(name string, n int) corev1.ContainerStatus {
	return corev1.ContainerStatus{Name: name, ContainerID: "containerd://" + id(n), State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}
}

func pod(statuses ...corev1.ContainerStatus) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "worker", UID: podUID},
		Spec:       corev1.PodSpec{NodeName: "node-a"},
		Status:     corev1.PodStatus{ContainerStatuses: statuses},
	}
}

func kill(at time.Time, r cgroup.Rise) report.Kill {
	return report.Kill{Pod: podUID, At: at.UnixMilli(), Rise: &r}
}

// own is a kill at the own limit of the containers named, as their files show it.
func own(at time.Time, ns ...int) report.Kill {
	r := cgroup.Rise{Confirmed: true, GroupKills: uint64(len(ns)), Inside: uint64(len(ns)), Own: uint64(len(ns)), Read: true}
	for _, n := range ns {
		r.Containers = append(r.Containers, id(n))
	}
	return kill(at, r)
}

// above is a kill above the pod of n containers whose files were gone.
func above(at time.Time, n uint64) report.Kill {
	return kill(at, cgroup.Rise{Confirmed: true, GroupKills: n})
}

func judged(p *corev1.Pod, kills ...report.Kill) (got []string, processes []time.Time) {
	verdicts, processes := judge(p, kills)
	for _, v := range verdicts {
		got = append(got, fmt.Sprintf("%s %s", v.ended.name, v.reason))
	}
	return got, processes
}

func TestJudge_NamesTheContainer(t *testing.T) {
	later := killedAt.Add(30 * time.Second)
	inferred := func(seen int) report.Kill {
		return kill(killedAt, cgroup.Rise{Confirmed: true, GroupKills: 1, Inside: 1, Own: 1, Inferred: seen})
	}
	for name, tc := range map[string]struct {
		pod   *corev1.Pod
		kills []report.Kill
		want  []string
	}{
		"whose file showed it": {
			pod(dead("runner", 1, killedAt), dead("dind", 2, killedAt)), []report.Kill{own(killedAt, 2)}, []string{"dind ContainerLimit"}},
		"and both where both did": {
			pod(dead("runner", 1, killedAt), dead("dind", 2, killedAt)), []report.Kill{own(killedAt, 1, 2)}, []string{"runner ContainerLimit", "dind ContainerLimit"}},
		"that ended then":           {pod(dead("runner", 1, killedAt), alive("dind", 2)), []report.Kill{above(killedAt, 1)}, []string{"runner AbovePods"}},
		"that ended a while before": {pod(dead("runner", 1, killedAt.Add(-endedWithin-time.Second))), []report.Kill{above(killedAt, 1)}, []string{"runner AbovePods"}},
		"that ended a while after":  {pod(dead("runner", 1, killedAt.Add(endedWithin))), []report.Kill{above(killedAt, 1)}, []string{"runner AbovePods"}},
		"not one that ended long before": {
			pod(dead("runner", 1, killedAt.Add(-endedWithin-2*time.Second))), []report.Kill{above(killedAt, 1)}, nil},
		"not one that ended long after": {pod(dead("runner", 1, killedAt.Add(endedWithin+time.Second))), []report.Kill{above(killedAt, 1)}, nil},
		"not one that has not ended":    {pod(alive("runner", 1)), []report.Kill{own(killedAt, 1), above(killedAt, 1)}, nil},
		"not one that stopped by itself": {
			pod(func() corev1.ContainerStatus {
				s := dead("runner", 1, killedAt)
				s.State.Terminated.ExitCode = 143
				return s
			}()), []report.Kill{above(killedAt, 1)}, nil},
		"not where another ended so as another thing, which it may be of": {
			pod(dead("runner", 1, killedAt), func() corev1.ContainerStatus {
				s := dead("dind", 2, killedAt.Add(-2*time.Second))
				s.State.Terminated.Reason = "Error"
				return s
			}()), []report.Kill{above(killedAt.Add(-2*time.Second), 1)}, []string{"runner Unknown"}},
		"nor where another was made again since, and that it may be of": {
			pod(dead("runner", 1, killedAt), func() corev1.ContainerStatus {
				s := dead("dind", 3, killedAt.Add(time.Second))
				s.State.Terminated.ExitCode, s.State.Terminated.Reason = 0, "Completed"
				s.LastTerminationState.Terminated = over(2, killedAt.Add(-2*time.Second))
				return s
			}()), []report.Kill{above(killedAt.Add(-2*time.Second), 1)}, []string{"runner Unknown"}},
		"nor of one that another's file showed": {
			pod(dead("runner", 1, killedAt), func() corev1.ContainerStatus {
				s := dead("dind", 2, killedAt)
				s.State.Terminated.Reason = "Error"
				return s
			}()), []report.Kill{own(killedAt, 2), above(killedAt, 1)}, []string{"runner AbovePods"}},
		"not one that ended as another thing": {
			pod(func() corev1.ContainerStatus {
				s := dead("runner", 1, killedAt)
				s.State.Terminated.Reason = "Error"
				return s
			}()), []report.Kill{own(killedAt, 1), above(killedAt, 1)}, nil},
		"one of two that ended, by a file": {
			pod(dead("runner", 1, killedAt), dead("dind", 2, killedAt)), []report.Kill{own(killedAt, 1), above(killedAt, 1)},
			[]string{"runner ContainerLimit", "dind AbovePods"}},
		"of two that ended, for neither": {
			pod(dead("runner", 1, killedAt), dead("dind", 2, killedAt)), []report.Kill{above(killedAt, 1)}, []string{"runner Unknown", "dind Unknown"}},
		"of two killed, for neither": {
			pod(dead("runner", 1, killedAt), dead("dind", 2, killedAt)), []report.Kill{above(killedAt, 2)}, []string{"runner Unknown", "dind Unknown"}},
		"of two killed, one's end unseen": {
			pod(dead("runner", 1, killedAt), alive("dind", 2)), []report.Kill{above(killedAt, 2)}, []string{"runner Unknown"}},
		"of two kills that may be its, for neither": {
			pod(dead("runner", 1, killedAt)), []report.Kill{above(killedAt, 1), kill(killedAt, cgroup.Rise{Confirmed: true, GroupKills: 1, Pod: 1})},
			[]string{"runner Unknown"}},
		"of two kills that may be its, with one limit": {
			pod(dead("runner", 1, killedAt)), []report.Kill{above(killedAt, 1), above(killedAt.Add(time.Second), 1)}, []string{"runner AbovePods"}},
		"that was made again, for the one before": {
			pod(func() corev1.ContainerStatus {
				s := alive("dind", 3)
				s.LastTerminationState.Terminated = over(2, killedAt)
				return s
			}()), []report.Kill{own(killedAt, 2)}, []string{"dind ContainerLimit"}},
		"that was made again and ended, for the last": {
			pod(func() corev1.ContainerStatus {
				s := dead("dind", 3, later)
				s.LastTerminationState.Terminated = over(2, killedAt)
				return s
			}()), []report.Kill{own(killedAt, 2), above(later, 1)}, []string{"dind AbovePods"}},
		"of its file, told by its going":               {pod(dead("runner", 1, killedAt), alive("dind", 2)), []report.Kill{inferred(3)}, []string{"runner ContainerLimit"}},
		"not where another's cgroup is the pod's":      {pod(dead("runner", 1, killedAt), alive("dind", 2)), []report.Kill{inferred(4)}, []string{"runner Unknown"}},
		"whose one process was killed, and that ended": {pod(dead("runner", 1, killedAt)), []report.Kill{kill(killedAt, cgroup.Rise{Confirmed: true, Singles: 1, Containers: []string{id(1)}})}, []string{"runner Unknown"}},
		"beside one whose one process was killed": {
			pod(dead("runner", 1, killedAt), alive("dind", 2)),
			[]report.Kill{kill(killedAt, cgroup.Rise{Confirmed: true, GroupKills: 1, Singles: 1, Containers: []string{id(2)}})}, []string{"runner Unknown"}},
		"whose file showed it long before its end": {pod(dead("runner", 1, killedAt)), []report.Kill{own(killedAt.Add(-endedWithin-time.Second), 1)}, []string{"runner Unknown"}},
		"whose file showed it long after, beside its own kill": {
			pod(dead("runner", 1, killedAt)), []report.Kill{own(killedAt, 1), kill(killedAt.Add(time.Hour), cgroup.Rise{Confirmed: true, GroupKills: 1, Pod: 1, Containers: []string{id(1)}})},
			[]string{"runner ContainerLimit"}},
		"of two kills its file showed, for neither": {
			pod(dead("runner", 1, killedAt)), []report.Kill{own(killedAt, 1), kill(killedAt, cgroup.Rise{Confirmed: true, GroupKills: 1, Containers: []string{id(1)}})},
			[]string{"runner Unknown"}},
		"beside a kill the agent was late for": {
			pod(dead("runner", 1, killedAt)), []report.Kill{own(killedAt, 1), kill(killedAt, cgroup.Rise{Late: true, GroupKills: 1}), kill(killedAt, cgroup.Rise{Late: true, GroupKills: 1, Containers: []string{id(1)}})},
			[]string{"runner ContainerLimit"}},
		"and nothing of another pod's kinds": {pod(dead("runner", 1, killedAt)), nil, nil},
	} {
		t.Run(name, func(t *testing.T) {
			got, processes := judged(tc.pod, tc.kills...)
			assert.Equal(t, tc.want, got)
			assert.Empty(t, processes)
		})
	}
}

// A cgroup that is none of the pod's is one more than those the status has there at the kill.
func TestJudge_CountsTheContainersThereAtTheKill(t *testing.T) {
	started := func(s corev1.ContainerStatus, at time.Time) corev1.ContainerStatus {
		if s.State.Running != nil {
			s.State.Running.StartedAt = metav1.NewTime(at)
		} else {
			s.State.Terminated.StartedAt = metav1.NewTime(at)
		}
		return s
	}
	again := func(end, start time.Time) corev1.ContainerStatus {
		s := started(alive("dind", 3), start)
		s.LastTerminationState.Terminated = over(2, end)
		s.LastTerminationState.Terminated.StartedAt = metav1.NewTime(killedAt.Add(-time.Hour))
		return s
	}
	hour := killedAt.Add(-time.Hour)
	for name, tc := range map[string]struct {
		other corev1.ContainerStatus
		there int
	}{
		"one that runs":                        {started(alive("dind", 2), hour), 3},
		"one that runs from the second before": {started(alive("dind", 2), killedAt.Add(-time.Second)), 3},
		"not one from its second on":           {started(alive("dind", 2), killedAt), 2},
		"one that ended as it did":             {started(dead("dind", 2, killedAt), hour), 3},
		"one that ended just before":           {started(dead("dind", 2, killedAt.Add(-endedWithin)), hour), 3},
		"not one that ended long before":       {started(dead("dind", 2, killedAt.Add(-endedWithin-time.Second)), hour), 2},
		"one before it was made again":         {again(killedAt, killedAt.Add(time.Second)), 3},
		"or after":                             {again(killedAt.Add(-time.Minute), killedAt.Add(-30*time.Second)), 3},
		"not both, long before":                {again(killedAt.Add(-time.Minute), killedAt.Add(time.Second)), 2},
		"not one that started and ended after": {started(dead("dind", 2, killedAt.Add(3*time.Second)), killedAt.Add(time.Second)), 2},
		"nor one made again since": {func() corev1.ContainerStatus {
			s := again(killedAt.Add(2*time.Second), killedAt.Add(3*time.Second))
			s.LastTerminationState.Terminated.StartedAt = metav1.NewTime(killedAt.Add(time.Second))
			return s
		}(), 2},
		"not one that is not made": {corev1.ContainerStatus{Name: "dind"}, 2},
	} {
		t.Run(name, func(t *testing.T) {
			for _, kind := range []string{"a container", "an init container", "an ephemeral container"} {
				p := pod(started(dead("runner", 1, killedAt), hour))
				switch kind {
				case "a container":
					p.Status.ContainerStatuses = append(p.Status.ContainerStatuses, tc.other)
				case "an init container":
					p.Status.InitContainerStatuses = []corev1.ContainerStatus{tc.other}
				default:
					p.Status.EphemeralContainerStatuses = []corev1.ContainerStatus{tc.other}
				}
				for seen, want := range map[int]string{tc.there: "runner ContainerLimit", tc.there + 1: "runner Unknown"} {
					k := kill(killedAt.Add(900*time.Millisecond), cgroup.Rise{Confirmed: true, GroupKills: 1, Inside: 1, Own: 1, Inferred: seen, Containers: []string{id(1)}})
					got, _ := judged(p, k)
					assert.Contains(t, got, want, "%s, %d", kind, seen)
				}
			}
		})
	}
}

func TestJudge_TakesThePodsOwnLimitFromItsSpec(t *testing.T) {
	k := kill(killedAt, cgroup.Rise{Confirmed: true, GroupKills: 1, Pod: 1, Read: true, Containers: []string{id(1)}})
	limits := func(name corev1.ResourceName, q string) *corev1.ResourceRequirements {
		return &corev1.ResourceRequirements{Limits: corev1.ResourceList{name: resource.MustParse(q)}}
	}
	for name, tc := range map[string]struct {
		resources *corev1.ResourceRequirements
		want      string
	}{
		"none":        {nil, "runner ContainerLimit"},
		"a request":   {&corev1.ResourceRequirements{Requests: limits(corev1.ResourceMemory, "1Gi").Limits}, "runner ContainerLimit"},
		"another":     {limits(corev1.ResourceCPU, "1"), "runner ContainerLimit"},
		"zero":        {limits(corev1.ResourceMemory, "0"), "runner ContainerLimit"},
		"one of this": {limits(corev1.ResourceMemory, "1Gi"), "runner PodLimit"},
	} {
		t.Run(name, func(t *testing.T) {
			p := pod(dead("runner", 1, killedAt))
			p.Spec.Resources = tc.resources
			got, _ := judged(p, k)
			assert.Equal(t, []string{tc.want}, got)
		})
	}
}

func TestJudge_SaysOfAKillThatEndedNoContainer(t *testing.T) {
	single := cgroup.Rise{Confirmed: true, Singles: 1, Containers: []string{id(1)}}
	late := single
	late.Late = true
	for name, tc := range map[string]struct {
		kill report.Kill
		want []time.Time
	}{
		"a process's":                             {report.Kill{Pod: podUID, At: killedAt.UnixMilli(), Processes: 1}, []time.Time{killedAt}},
		"one of a container that is not killed":   {kill(killedAt, single), []time.Time{killedAt}},
		"but not where it may be of any time":     {kill(killedAt, late), nil},
		"nor a whole cgroup's that is no known's": {above(killedAt, 1), nil},
	} {
		t.Run(name, func(t *testing.T) {
			got, processes := judged(pod(alive("runner", 1)), tc.kill)
			assert.Empty(t, got)
			var utc []time.Time
			for _, at := range processes {
				utc = append(utc, at.UTC())
			}
			assert.Equal(t, tc.want, utc)
		})
	}
}

func TestJudge_SaysWhetherALimitInsideRanOutWithALateKill(t *testing.T) {
	for name, tc := range map[string]struct {
		rise cgroup.Rise
		want bool
	}{
		"none did":         {cgroup.Rise{Late: true, GroupKills: 1}, true},
		"the pod's did":    {cgroup.Rise{Late: true, GroupKills: 1, Pod: 1}, false},
		"one inside did":   {cgroup.Rise{Late: true, GroupKills: 1, Inside: 1}, false},
		"one may, half":    {cgroup.Rise{Late: true, GroupKills: 1, Torn: true}, false},
		"one had, before":  {cgroup.Rise{Late: true, GroupKills: 1, RecentOwn: true}, false},
		"it was in time":   {cgroup.Rise{Confirmed: true, GroupKills: 1, Recent: true}, false},
		"nor where needed": {cgroup.Rise{Confirmed: true, GroupKills: 1}, false},
	} {
		t.Run(name, func(t *testing.T) {
			verdicts, _ := judge(pod(dead("runner", 1, killedAt)), []report.Kill{kill(killedAt, tc.rise)})
			assert.Len(t, verdicts, 1)
			assert.Equal(t, tc.want, strings.Contains(verdicts[0].text(), wordsNone))
		})
	}
}

func TestWords_EveryReasonHasItsOwn(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range []rule.Reason{rule.ContainerLimit, rule.PodLimit, rule.PodsLimit, rule.AbovePods, rule.Unknown} {
		assert.NotEmpty(t, words[r])
		assert.False(t, seen[words[r]])
		seen[words[r]] = true
	}
	assert.Len(t, words, len(seen))
}
