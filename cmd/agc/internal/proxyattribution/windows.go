package proxyattribution

import (
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	// edgeMargin is how close to either end of a pod's address window a record
	// may fall and still be discarded rather than attributed. It absorbs clock
	// skew between the proxy, the kubelet and the AGC, and the gap between the
	// address being handed out and the condition that marks it.
	edgeMargin = 2 * time.Second
	// closedWindowRetention keeps a closed window for as long as the proxy keeps
	// a record, so one read after its pod is gone still joins.
	closedWindowRetention = 5 * time.Minute
)

// window is the interval one pod held one address. end is zero while it is open.
type window struct {
	uid   types.UID
	own   bool
	start time.Time
	end   time.Time
}

// addressWindows records, per address, the windows every pod in the namespace
// held it. Addresses are reused once a pod's sandbox is gone, so an address
// alone never names a pod; and a sibling gateway's pods share the namespace's
// records, so their windows are kept too, to see when one collides with one of
// this gateway's.
type addressWindows struct {
	mu   sync.Mutex
	byIP map[string][]*window
	live map[types.UID]*window
}

func newAddressWindows() *addressWindows {
	return &addressWindows{byIP: map[string][]*window{}, live: map[types.UID]*window{}}
}

// sandboxCondition returns pod's PodReadyToStartContainers condition: true once
// its sandbox, and so its address, exists, and false again once it is torn down.
func sandboxCondition(pod *corev1.Pod) *corev1.PodCondition {
	for i := range pod.Status.Conditions {
		if pod.Status.Conditions[i].Type == corev1.PodReadyToStartContainers {
			return &pod.Status.Conditions[i]
		}
	}
	return nil
}

// windowStart is when pod's address became live. Before the sandbox condition
// is reported, the kubelet's acknowledgement is the earliest bound there is.
func windowStart(pod *corev1.Pod) (time.Time, bool) {
	if c := sandboxCondition(pod); c != nil && c.Status == corev1.ConditionTrue {
		return c.LastTransitionTime.Time, true
	}
	if pod.Status.StartTime != nil {
		return pod.Status.StartTime.Time, true
	}
	return time.Time{}, false
}

// released reports whether pod has given its address up, and from when. A
// terminal pod's sandbox is torn down, so its address can be reused before the
// object is deleted. seen is when this AGC observed the pod, the latest the
// release can have happened.
func released(pod *corev1.Pod, seen time.Time) (time.Time, bool) {
	if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		return seen, true
	}
	if c := sandboxCondition(pod); c != nil && c.Status == corev1.ConditionFalse && !c.LastTransitionTime.IsZero() {
		return minTime(c.LastTransitionTime.Time, seen), true
	}
	return time.Time{}, false
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// observe opens pod's window the first time it is seen holding an address, and
// closes it once the pod has given the address up. A pod first seen before its
// RunnerSet could be read becomes own once a later event shows it is; it never
// stops being own, since a RunnerSet deleted under a running job leaves the job.
func (w *addressWindows) observe(pod *corev1.Pod, own bool, seen time.Time) {
	if at, ok := released(pod, seen); ok {
		w.close(pod.UID, at)
		return
	}
	if pod.Status.PodIP == "" {
		return
	}
	start, ok := windowStart(pod)
	if !ok {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if win, ok := w.live[pod.UID]; ok {
		win.own = win.own || own
		return
	}
	win := &window{uid: pod.UID, own: own, start: start}
	w.live[pod.UID] = win
	w.byIP[pod.Status.PodIP] = append(w.byIP[pod.Status.PodIP], win)
}

// close ends pod uid's window at the given time.
func (w *addressWindows) close(uid types.UID, at time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if win, ok := w.live[uid]; ok {
		win.end = at
		delete(w.live, uid)
	}
}

// attribute names this gateway's pod that held ip at t. A time within
// edgeMargin of a window's edge names nobody, and so does one inside more than
// one window, or inside the window of a pod that is not this gateway's.
func (w *addressWindows) attribute(ip string, t time.Time) (types.UID, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var match *window
	n := 0
	for _, win := range w.byIP[ip] {
		if t.Before(win.start.Add(edgeMargin)) {
			continue
		}
		if !win.end.IsZero() && t.After(win.end.Add(-edgeMargin)) {
			continue
		}
		match = win
		n++
	}
	if n != 1 || !match.own {
		return "", false
	}
	return match.uid, true
}

// sweep drops windows closed longer ago than closedWindowRetention.
func (w *addressWindows) sweep(now time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for ip, wins := range w.byIP {
		kept := wins[:0]
		for _, win := range wins {
			if win.end.IsZero() || now.Sub(win.end) < closedWindowRetention {
				kept = append(kept, win)
			}
		}
		if len(kept) == 0 {
			delete(w.byIP, ip)
			continue
		}
		w.byIP[ip] = kept
	}
}
