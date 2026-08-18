package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// workersUnschedulable carries the computed WorkersUnschedulable condition (Q157)
// plus the soonest re-check the reconciler should schedule.
type workersUnschedulable struct {
	unschedulable bool
	reason        string
	message       string
	// requeueAfter is the time until the earliest still-Pending worker pod crosses
	// the scheduling grace (0 = none pending). The reconciler folds it into its
	// RequeueAfter so the condition is re-evaluated when a pod becomes overdue —
	// the Pod watch fires only on phase changes, so a PodScheduled flip while the
	// pod stays Pending would otherwise never re-trigger a reconcile.
	requeueAfter time.Duration
	// stuckPods are the worker pods that produced the verdict: Pending past the
	// scheduling grace with the scheduler reporting Unschedulable, oldest first.
	//
	// Carried out so the capacity gate (Q406) reads pod Events for
	// exactly these pods and no others — the same "actually stuck" definition the
	// condition itself uses. Scoping matters: an Event read is uncached, and a broad
	// Event watch on a busy cluster is a real load problem, so the mode must only ever
	// look at pods that have already earned a verdict.
	stuckPods []*corev1.Pod
	// lastScheduledAt is the newest PodScheduled=True transition among the listed
	// pods (zero when none has one), whatever their phase — a pod that bound and is
	// pulling images, running, or already terminal all count. Carried out for the
	// capacity gate's latch (Q512): a pod that scheduled AFTER the gate declined is
	// the evidence that capacity returned, and the transition time rather than the
	// creation time is what lets a long-stuck pod that finally squeezes in count too.
	lastScheduledAt time.Time
	// lastStartedAt is the newest first-container-start among the listed pods (zero
	// when none ever ran), whatever their phase. It supersedes lastScheduledAt as the
	// latch's release evidence (Q714): binding was only a proxy for "a worker can run
	// here", and notStartingPods is the case that falsifies the proxy.
	lastStartedAt time.Time
	// notStartingPods are the worker pods the scheduler placed and the kubelet then
	// could not start — the sibling verdict to stuckPods, disjoint from it by
	// construction because the two read opposite halves of PodScheduled (Q714).
	//
	// Carried out for the capacity gate, like stuckPods: nothing else consumes it,
	// and WorkersUnschedulable deliberately stays silent about it, because a bound
	// pod is not a scheduling problem and reporting it there would send an operator
	// after a node, a taint, or a quota when the fix is an image.
	notStartingPods []notStartingPod
	// startupPending reports that at least one bound worker pod is still Pending with
	// no startup verdict either way. It is what the gate schedules a re-check on: the
	// Pod watch drops updates that change no phase (workerPodPhaseChangePredicate),
	// and a pod entering ImagePullBackOff changes none, so nothing would otherwise
	// wake the reconciler between the pod's creation and its pendingPodDeadline.
	startupPending bool
}

// notStartingPod is one worker pod the kubelet bound and could not start, with the
// kubelet's own waiting message. Name and detail rather than the object: the gate
// needs them for the condition message and for nothing else, and the pod-shaped
// verdict this signal produces has no per-pod follow-up read the way the autoscaler
// verdict's Event lookup does.
type notStartingPod struct {
	name   string
	detail string
}

// evalWorkersUnschedulable computes the WorkersUnschedulable condition (Q157) for
// a RunnerGroup: True when at least one worker pod has sat Pending past the
// scheduling grace because the scheduler could not place it
// (PodScheduled=False/Unschedulable). Quota exhaustion is deliberately NOT
// detected here — a quota rejection blocks pod admission so no pod is ever
// created; the WorkerQuota ladder (Q82) covers that case and the two never
// double-report.
//
// A list failure yields a schedulable (False) result: the absence of evidence is
// not an alarm, and the next reconcile retries.
func (r *RunnerGroupReconciler) evalWorkersUnschedulable(ctx context.Context, rg *v1alpha1.RunnerGroup) workersUnschedulable {
	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(rg.Namespace),
		client.MatchingLabels{provisioner.LabelRunnerGroup: rg.Name},
	); err != nil {
		return workersUnschedulable{
			reason:  v1alpha1.ReasonWorkersSchedulable,
			message: fmt.Sprintf("could not list worker pods: %v", err),
		}
	}
	return evalWorkersUnschedulableForPods(pods.Items, r.nowFunc()(), unschedulableGrace(rg),
		v1alpha1.ReasonWorkersSchedulable, v1alpha1.ReasonPodsUnschedulable)
}

// evalWorkersUnschedulableForPods is the owner-agnostic core of the
// WorkersUnschedulable computation (Q157): given a worker-pod list, the clock, and
// the scheduling grace, it reports whether any pod has sat Pending+Unschedulable
// past the grace and the soonest re-check for a still-within-grace Pending pod. The
// schedulable/unschedulable reason strings are passed in so the v1 RunnerGroup and
// v2 RunnerSet reconcilers reuse the identical logic while stamping their own
// package's reason constants (Q303).
func evalWorkersUnschedulableForPods(pods []corev1.Pod, now time.Time, grace time.Duration, schedulableReason, unschedulableReason string) workersUnschedulable {
	st := workersUnschedulable{
		reason:  schedulableReason,
		message: "all worker pods are schedulable",
	}

	var stuck []string
	var next time.Duration
	for i := range pods {
		pod := &pods[i]
		if at, ok := provisioner.PodScheduledAt(pod); ok && at.After(st.lastScheduledAt) {
			st.lastScheduledAt = at
		}
		if at, ok := podStartedAt(pod); ok && at.After(st.lastStartedAt) {
			st.lastStartedAt = at
		}
		if !pod.DeletionTimestamp.IsZero() || pod.Status.Phase != corev1.PodPending {
			continue
		}
		// The kubelet's verdict, taken with no grace of its own: unlike the
		// scheduler's, it is not a snapshot the next moment can overturn — the pod
		// is already in backoff, so the attempt has happened. A bound pod can never
		// carry PodScheduled=False, so this arm and the one below are exclusive.
		if backoff, detail := podStartupBackoff(pod); backoff {
			st.notStartingPods = append(st.notStartingPods, notStartingPod{name: pod.Name, detail: truncate(detail, 160)})
			continue
		}
		if podBound(pod) {
			st.startupPending = true
		}
		graceDue := pod.CreationTimestamp.Add(grace)
		if wait := graceDue.Sub(now); wait > 0 {
			// Not yet past the grace. Re-check at the crossing regardless of the
			// pod's current schedulability — it may only be marked Unschedulable
			// after this reconcile, and the phase-only Pod watch will not notice.
			if next == 0 || wait < next {
				next = wait
			}
			continue
		}
		if unsched, schedMsg := podUnschedulable(pod); unsched {
			stuck = append(stuck, fmt.Sprintf("%s (%s)", pod.Name, truncate(schedMsg, 160)))
			st.stuckPods = append(st.stuckPods, pod)
		}
	}
	// Oldest first: a pod that has been stuck longest is the one an autoscaler has had
	// the most time to reach a verdict on, so the Q406 gate reads the settled ones
	// first and its per-reconcile read budget is spent where it is most informative.
	slices.SortStableFunc(st.stuckPods, func(a, b *corev1.Pod) int {
		return a.CreationTimestamp.Time.Compare(b.CreationTimestamp.Time)
	})

	st.requeueAfter = next
	if len(stuck) > 0 {
		st.unschedulable = true
		st.reason = unschedulableReason
		st.message = fmt.Sprintf("%d worker pod(s) Pending and unschedulable for more than %s: %s",
			len(stuck), grace, strings.Join(stuck, "; "))
	}
	return st
}

// podUnschedulable reports whether a pod's scheduler verdict is Unschedulable —
// the PodScheduled condition is False with reason Unschedulable — and returns the
// scheduler's human-readable message. This is precisely a scheduler decision (no
// node can host the pod): a ResourceQuota rejection happens at admission and never
// produces a pod, so it can never present here.
func podUnschedulable(pod *corev1.Pod) (bool, string) {
	for i := range pod.Status.Conditions {
		c := &pod.Status.Conditions[i]
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse &&
			c.Reason == corev1.PodReasonUnschedulable {
			return true, c.Message
		}
	}
	return false, ""
}

// unschedulableGrace is how long a worker pod must sit Pending+Unschedulable
// before WorkersUnschedulable trips. It is half the group's pendingPodDeadline so
// the condition latches and stays stable for a window before the reaper deletes
// the pod at the full deadline (Q95): were the grace equal to the deadline, the
// condition would be set in the same pass the pod is reaped and clear on the next
// reconcile — flapping. The factor of one-half gives the operator a visible
// early-warning window while the unschedulable pod still exists.
func unschedulableGrace(rg *v1alpha1.RunnerGroup) time.Duration {
	d := provisioner.EffectivePendingPodDeadline(rg) / 2
	if d <= 0 {
		d = time.Second
	}
	return d
}

// truncate shortens s to at most max runes, appending an ellipsis when cut, so a
// long multi-line scheduler message stays readable inside a condition message.
func truncate(s string, max int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// --- metrics ---------------------------------------------------------------

// workersUnschedulableCollector exports the WorkersUnschedulable condition (Q157)
// as a gauge so operators can alert on stuck worker scheduling without
// kube-state-metrics scraping CRD conditions. Like the worker-quota collector it
// reads at scrape time from the cached reader: a deleted RunnerGroup stops being
// listed, so its series disappears with no reconcile-path cost. The value mirrors
// the condition the reconciler wrote to .status.conditions (1 when True, else 0).
type workersUnschedulableCollector struct {
	reader        client.Reader
	unschedulable *prometheus.Desc
}

func newWorkersUnschedulableCollector(reader client.Reader) *workersUnschedulableCollector {
	return &workersUnschedulableCollector{
		reader: reader,
		unschedulable: prometheus.NewDesc(
			"actions_gateway_workers_unschedulable",
			"1 when the RunnerGroup WorkersUnschedulable condition is True (worker pods are Pending and cannot be scheduled for a non-quota reason), else 0.",
			[]string{"namespace", "runner_group"}, nil,
		),
	}
}

func (c *workersUnschedulableCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.unschedulable
}

func (c *workersUnschedulableCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var list v1alpha1.RunnerGroupList
	if err := c.reader.List(ctx, &list); err != nil {
		return
	}
	for i := range list.Items {
		rg := &list.Items[i]
		if !rg.DeletionTimestamp.IsZero() {
			continue
		}
		ch <- prometheus.MustNewConstMetric(c.unschedulable, prometheus.GaugeValue,
			conditionGaugeValue(rg.Status.Conditions, v1alpha1.ConditionWorkersUnschedulable), rg.Namespace, rg.Name)
	}
}

// registerWorkersUnschedulableMetrics registers the WorkersUnschedulable collector
// with the controller-runtime registry. Like registerWorkerQuotaMetrics it
// tolerates double registration across test managers.
func registerWorkersUnschedulableMetrics(reader client.Reader) {
	if err := crmetrics.Registry.Register(newWorkersUnschedulableCollector(reader)); err != nil {
		if _, ok := err.(prometheus.AlreadyRegisteredError); !ok {
			panic(err)
		}
	}
}
