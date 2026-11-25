// Package controller says on each pod which limit ran out when a container of it was killed for
// memory, from its node's report.
package controller

import (
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/actions-gateway/github-actions-gateway/oomreporter/internal/report"
	"github.com/actions-gateway/github-actions-gateway/oomreporter/internal/rule"
)

const (
	// endedWithin bounds how far apart a kill and the end of a container it may be of are, the
	// clock of both being the node's. A status has whole seconds.
	endedWithin = 5 * time.Second

	reasonOOMKilled = "OOMKilled"
	exitSIGKILL     = 137
)

// verdict is which limit ran out when a container was killed.
type verdict struct {
	ended  ended
	reason rule.Reason
	at     time.Time
	// none: no limit of the pod or inside it ran out with the kill.
	none bool
	// weak: it takes back nothing that another kill says of the container.
	weak bool
}

// ended is a container whose last end was as OOMKilled.
type ended struct {
	name, id string
	field    string
	code     int32
	at       time.Time
}

// judge returns pod's verdicts by container, and when kills ended no container.
func judge(pod *corev1.Pod, kills []report.Kill) (verdicts []verdict, processes []time.Time) {
	all, total := containers(pod)
	// A container whose own counts show a kill is not looked for behind another.
	shown := map[string]bool{}
	for _, k := range kills {
		if k.Rise != nil {
			for _, id := range k.Rise.Containers {
				shown[id] = true
			}
		}
	}
	by, weak := map[string]verdict{}, map[string]verdict{}
	add := func(v verdict, e ended, late bool) {
		v.ended = e
		if late || !near(v.at, e.at) {
			v.reason, v.weak = rule.Unknown, true
			weak[e.name] = v
			return
		}
		if was, ok := by[e.name]; ok && was.reason != v.reason {
			v.reason = rule.Unknown
		}
		by[e.name] = v
	}
	for _, k := range kills {
		if k.Rise == nil {
			processes = append(processes, k.Time())
			continue
		}
		v := verdict{
			reason: rule.Decide(*k.Rise, podLimit(pod)), at: k.Time(),
			none: k.Rise.Late && !k.Rise.Torn && k.Rise.Inside+k.Rise.Pod == 0,
		}
		// More cgroups than containers and the one sandbox: one is another's, as a job with the
		// host's view of the tree can make it, and may be what has gone.
		if k.Rise.Inferred > total+1 {
			v.reason = rule.Unknown
		}
		var of, maybe []ended
		for _, e := range all {
			switch {
			case slices.Contains(k.Rise.Containers, e.id):
				of = append(of, e)
			case !shown[e.id] && e.code == exitSIGKILL && near(v.at, e.at):
				maybe = append(maybe, e)
			}
		}
		for _, e := range of {
			add(v, e, k.Rise.Late)
		}
		// Those of single processes are among Containers too.
		whole := uint64(len(k.Rise.Containers))
		if k.Rise.Singles > 0 {
			whole = 0
		}
		if k.Rise.GroupKills <= whole {
			maybe = nil
		}
		if k.Rise.GroupKills != whole+1 || len(maybe) != 1 {
			v.reason = rule.Unknown
		}
		for _, e := range maybe {
			add(v, e, k.Rise.Late)
		}
		// One seen as the agent started may be of any time.
		if k.Rise.GroupKills == 0 && len(of) == 0 && !k.Rise.Late {
			processes = append(processes, v.at)
		}
	}
	for _, e := range all {
		if v, ok := by[e.name]; ok {
			verdicts = append(verdicts, v)
		} else if v, ok := weak[e.name]; ok {
			verdicts = append(verdicts, v)
		}
	}
	return verdicts, processes
}

func near(kill, end time.Time) bool {
	d := kill.Sub(end)
	return d >= -endedWithin && d <= endedWithin+time.Second
}

// containers returns pod's containers whose last end was as OOMKilled, and how many it has had
// that its status still names.
func containers(pod *corev1.Pod) (all []ended, total int) {
	for _, g := range []struct {
		field    string
		statuses []corev1.ContainerStatus
	}{
		{"spec.initContainers", pod.Status.InitContainerStatuses},
		{"spec.containers", pod.Status.ContainerStatuses},
		{"spec.ephemeralContainers", pod.Status.EphemeralContainerStatuses},
	} {
		for _, s := range g.statuses {
			ids := map[string]bool{s.ContainerID: true}
			last := s.State.Terminated
			if was := s.LastTerminationState.Terminated; was != nil {
				ids[was.ContainerID] = true
				if last == nil {
					last = was
				}
			}
			delete(ids, "")
			total += len(ids)
			if last == nil || last.Reason != reasonOOMKilled {
				continue
			}
			_, id, _ := strings.Cut(last.ContainerID, "://")
			all = append(all, ended{
				name: s.Name, id: id, field: fmt.Sprintf("%s{%s}", g.field, s.Name),
				code: last.ExitCode, at: last.FinishedAt.Time,
			})
		}
	}
	return all, total
}

// podLimit reports whether pod's spec.resources sets a memory limit.
func podLimit(pod *corev1.Pod) bool {
	if pod.Spec.Resources == nil {
		return false
	}
	limit, ok := pod.Spec.Resources.Limits[corev1.ResourceMemory]
	return ok && !limit.IsZero()
}
