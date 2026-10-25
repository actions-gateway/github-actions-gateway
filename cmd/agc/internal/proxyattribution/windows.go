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
	// closedWindowRetention keeps a deleted pod's window for as long as the proxy
	// keeps a record, so one read after its pod is gone still joins.
	closedWindowRetention = 5 * time.Minute
)

// window is the interval one pod held one address. end is zero while it is open.
type window struct {
	pod   string
	start time.Time
	end   time.Time
}

// addressWindows records, per address, the windows this gateway's own worker
// pods held it. Addresses are reused once a pod is gone, so an address alone
// never names a pod.
type addressWindows struct {
	mu   sync.Mutex
	byIP map[string][]*window
	live map[types.UID]*window
}

func newAddressWindows() *addressWindows {
	return &addressWindows{byIP: map[string][]*window{}, live: map[types.UID]*window{}}
}

// windowStart is when pod's address became live: its sandbox, and so its
// address, exists once PodReadyToStartContainers is true. Before that condition
// is reported, the kubelet's acknowledgement is the earliest bound there is.
func windowStart(pod *corev1.Pod) (time.Time, bool) {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReadyToStartContainers && c.Status == corev1.ConditionTrue {
			return c.LastTransitionTime.Time, true
		}
	}
	if pod.Status.StartTime != nil {
		return pod.Status.StartTime.Time, true
	}
	return time.Time{}, false
}

// open starts pod's window the first time it is seen with an address.
func (w *addressWindows) open(pod *corev1.Pod) {
	if pod.Status.PodIP == "" {
		return
	}
	start, ok := windowStart(pod)
	if !ok {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.live[pod.UID]; ok {
		return
	}
	win := &window{pod: pod.Name, start: start}
	w.live[pod.UID] = win
	w.byIP[pod.Status.PodIP] = append(w.byIP[pod.Status.PodIP], win)
}

// close ends pod uid's window at the time its deletion was observed.
func (w *addressWindows) close(uid types.UID, at time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if win, ok := w.live[uid]; ok {
		win.end = at
		delete(w.live, uid)
	}
}

// attribute names the pod that held ip at t. A time within edgeMargin of a
// window's edge, or inside more than one window, names nobody.
func (w *addressWindows) attribute(ip string, t time.Time) (string, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var pod string
	n := 0
	for _, win := range w.byIP[ip] {
		if t.Before(win.start.Add(edgeMargin)) {
			continue
		}
		if !win.end.IsZero() && t.After(win.end.Add(-edgeMargin)) {
			continue
		}
		pod = win.pod
		n++
	}
	return pod, n == 1
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
