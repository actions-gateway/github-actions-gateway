package controller

import (
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/actions-gateway/github-actions-gateway/oomreporter/internal/rule"
)

// Domain is that of every condition the controller writes: a container's is of the type
// Domain, a slash, and the container's name.
const Domain = "oom.actions-gateway.com"

// eventPrefix starts the reason of every Event of a container that was killed.
const eventPrefix = "OOM"

// reasonProcess is that of an Event of a kill that ended no container.
const reasonProcess = eventPrefix + "ProcessKilled"

var words = map[rule.Reason]string{
	rule.ContainerLimit: "The container's own memory limit ran out, or those of the pod's containers together.",
	rule.PodLimit:       "The memory limit in the pod's spec.resources ran out.",
	rule.PodsLimit:      "The memory limit that all pods of the node share ran out, or that of those of this pod's QoS class.",
	rule.AbovePods:      "No limit of the pod's or the pods' ran out: memory ran out above them, which on an ordinary node is the node's.",
	rule.Unknown:        "Which limit ran out is not known.",
}

const (
	wordsNone    = " No limit of the pod's, or of a container's, ran out with it."
	wordsProcess = "The kernel killed a process in the pod's cgroup for memory, and no container whole. Which limit ran out is not known."
)

func (v verdict) text() string {
	if v.none {
		return words[v.reason] + wordsNone
	}
	return words[v.reason]
}

func (e ended) ref() string { return "container ID " + e.id }

// condition is v's. One that is there, and holds the same status, gives it its transition time.
func (v verdict) condition(pod *corev1.Pod, now time.Time) (c corev1.PodCondition, changed bool) {
	c = corev1.PodCondition{
		Type:   corev1.PodConditionType(Domain + "/" + v.ended.name),
		Status: corev1.ConditionTrue, Reason: string(v.reason),
		Message: fmt.Sprintf("%s It ended at %s, %s.", v.text(), v.ended.at.UTC().Format(time.RFC3339), v.ended.ref()),
	}
	if v.reason == rule.Unknown {
		c.Status = corev1.ConditionUnknown
	}
	c.LastTransitionTime.Time = now
	for _, was := range pod.Status.Conditions {
		if was.Type != c.Type {
			continue
		}
		if v.weak && was.Status == corev1.ConditionTrue && strings.Contains(was.Message, v.ended.ref()) {
			return was, false
		}
		if was.Status == c.Status {
			c.LastTransitionTime = was.LastTransitionTime
		}
		return c, was.Status != c.Status || was.Reason != c.Reason || was.Message != c.Message
	}
	return c, true
}
