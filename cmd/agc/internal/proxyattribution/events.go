package proxyattribution

import (
	"context"
	"net"
	"time"

	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

// maxDenialEventsPerPod caps the destinations one job's denials are reported
// for, so a job walking hosts cannot flood its namespace's Events.
const maxDenialEventsPerPod = 8

// denialEvents remembers which destinations each pod's denials were reported
// for. A pod's entry outlives its last denial by failureRetention, so a job
// hammering one host is reported once. Only the poll loop touches it.
type denialEvents struct {
	byPod map[types.UID]*podDenials
}

type podDenials struct {
	dests map[string]bool
	last  time.Time
}

func newDenialEvents() *denialEvents {
	return &denialEvents{byPod: map[types.UID]*podDenials{}}
}

// first reports whether a denial of dest for pod is one to report: the first
// for that destination, within the pod's cap.
func (d *denialEvents) first(pod types.UID, dest string, at time.Time) bool {
	pd := d.byPod[pod]
	if pd == nil {
		pd = &podDenials{dests: map[string]bool{}}
		d.byPod[pod] = pd
	}
	pd.last = maxTime(pd.last, at)
	if pd.dests[dest] || len(pd.dests) >= maxDenialEventsPerPod {
		return false
	}
	pd.dests[dest] = true
	return true
}

func (d *denialEvents) sweep(now time.Time) {
	for pod, pd := range d.byPod {
		if now.Sub(pd.last) >= failureRetention {
			delete(d.byPod, pod)
		}
	}
}

func (d *denialEvents) reset() { clear(d.byPod) }

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// reportDenial records a Warning Event naming the host one of this gateway's
// workers was refused, on the worker's RunnerSet with the pod as related, since
// the pod is deleted when its job ends, which can be before the record is read.
// A RunnerSet that cannot be read leaves the pod as the Event's subject.
func (p *Poller) reportDenial(ctx context.Context, pl pool, w worker, rec record) {
	dest := net.JoinHostPort(rec.Host, rec.Port)
	if p.Recorder == nil || !p.denials.first(w.uid, dest, rec.Time) {
		return
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: p.Namespace, Name: w.name, UID: w.uid}}
	var regarding, related runtime.Object = pod, nil
	var rs v2alpha1.RunnerSet
	if err := p.Client.Get(ctx, types.NamespacedName{Namespace: p.Namespace, Name: w.set}, &rs); err == nil {
		regarding, related = &rs, pod
	}
	p.Recorder.Eventf(regarding, related, corev1.EventTypeWarning, "EgressDestinationDenied", "AttributeProxyFailure",
		"EgressProxy %s/%s refused worker pod %s a CONNECT to %s: the destination is not on the pool's allowlist, so a retry is refused the same way",
		pl.namespace, pl.name, w.name, dest)
}
