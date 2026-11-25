// Package rule names the limit that ran out when a pod's container was killed for memory.
package rule

import "github.com/actions-gateway/github-actions-gateway/oomreporter/internal/cgroup"

// Reason is the limit that ran out.
type Reason string

const (
	// ContainerLimit is a limit inside the pod, or the containers' limits together.
	ContainerLimit Reason = "ContainerLimit"
	// PodLimit is the pod's spec.resources memory limit.
	PodLimit Reason = "PodLimit"
	// PodsLimit is the limit all pods on the node share, or one QoS class's.
	PodsLimit Reason = "PodsLimit"
	// AbovePods is something above the pods: on an ordinary node, the node's memory.
	AbovePods Reason = "AbovePods"
	// Unknown claims nothing.
	Unknown Reason = "Unknown"
)

// Decide names the limit behind r. podLimit is whether the pod's spec.resources sets a
// memory limit: the kubelet caps a pod's cgroup without one too, at the sum of its
// containers' limits.
//
// A kill of one process ends a container or does not, so nothing is claimed for it. Every
// whole kill at a limit follows at least one allocation counted there, so a count
// smaller than the kills leaves one that met some other limit. A larger one proves nothing:
// several allocations can wait for one kill, and each is counted. A limit below a container
// kills no container whole, so what was counted there says nothing once the containers'
// own files have.
func Decide(r cgroup.Rise, podLimit bool) Reason {
	inside := r.Inside
	if r.Confirmed && r.Read {
		inside = r.Own
	}
	switch {
	case r.Late || r.Torn || r.Singles > 0:
		return Unknown
	case r.Pod+r.Pods == 0 && r.Own > 0 && r.Own == r.GroupKills:
		return ContainerLimit
	case inside+r.Pods == 0 && r.Pod > 0 && r.GroupKills == 1:
		if podLimit {
			return PodLimit
		}
		return ContainerLimit
	case !r.Confirmed:
		return Unknown
	case inside+r.Pod == 0 && r.Pods > 0 && r.Pods >= r.GroupKills+r.Peers:
		return PodsLimit
	case inside+r.Pod+r.Pods == 0 && !r.Recent:
		return AbovePods
	}
	return Unknown
}
