package provisioner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Flaky-job detection on the scale-set tier (Q555). A failed JobCompleted records the
// job in the failure ledger's failures map with the worker's platform context; a later
// completion of the same job (same run, same display name, a new job ID, and a re-run by
// the run's job list) pairs with the record and is attributed to whoever re-ran it, from
// GAG's own re-runs of the run in the same ledger. Detection changes no behaviour. The
// operator-facing account is docs/design/04-operational-flows.md §4.2.

// JobCompleted results. "canceled" is GitHub's spelling on this wire.
const (
	jobResultSucceeded = "succeeded"
	jobResultCanceled  = "canceled"
)

// Who re-ran a failed job, the trigger label.
const (
	// rerunTriggerGAG: the job was in GAG's own disruption re-run.
	rerunTriggerGAG = "gag"
	// rerunTriggerGAGCollateral: GAG's rerun-failed-jobs re-ran it beside a disrupted job.
	rerunTriggerGAGCollateral = "gag_collateral"
	// rerunTriggerOther: a human or another automation re-ran it.
	rerunTriggerOther = "other"
)

// maxWorkerSnapshots bounds the snapshots awaiting their job's completion, and the
// recorded failures awaiting their worker's finish. Each is consumed by what it waits
// for, so the bound only binds when those stop arriving.
const maxWorkerSnapshots = 1024

// failureAmendBudget bounds one amendFailedJob off the pod watch.
const failureAmendBudget = 30 * time.Second

// snapshotMessageMax truncates the pod's status message in a snapshot.
const snapshotMessageMax = 256

// failureRecord is one failed job awaiting a later attempt, keyed by failureKey.
type failureRecord struct {
	ExpiresAt time.Time `json:"expiresAt"`
	// FailedAt and Context are the first failure's; a later failed attempt updates the
	// rest.
	FailedAt time.Time `json:"failedAt"`
	// JobID is the latest failed attempt's job UUID: a completion carrying it again is
	// a replay and changes nothing.
	JobID string `json:"jobID"`
	// Runner is the latest failed attempt's runner, which is how GitHub's job list names
	// it. Records written before v2.1 have none.
	Runner string `json:"runner,omitempty"`
	// Cause is the disruption a recovery claim names for the latest failed attempt, ""
	// where none did. A failure with a cause is never counted flaky.
	Cause       string `json:"cause,omitempty"`
	Event       string `json:"event,omitempty"`
	WorkflowRef string `json:"workflowRef,omitempty"`
	// Context is the worker snapshot, kept as raw JSON so a version that knows fewer of
	// its fields rewrites it whole. Absent means unknown: no snapshot and no pod.
	Context json.RawMessage `json:"context,omitempty"`
	extra   map[string]json.RawMessage
}

// failureKey keys a job across its attempts: the job UUID changes on a re-run, the run
// ID and the display name do not. Jobs of one run that share a display name share a key.
func failureKey(runID, jobName string) string {
	return runID + "/" + jobName
}

// workerSnapshot is the platform context of a worker at its terminal transition, or as
// its job's completion found it, from objects the AGC already reads. Node conditions are
// not in it: the AGC has no nodes RBAC, and the pod's DisruptionTarget condition carries
// what a node loss shows.
type workerSnapshot struct {
	// Proxy is the set's egress proxy pool, nil for direct egress or where no reader
	// was supplied.
	Proxy      *ProxySnapshot      `json:"proxy,omitempty"`
	Pod        string              `json:"pod"`
	Phase      string              `json:"podPhase,omitempty"`
	Reason     string              `json:"reason,omitempty"`
	Message    string              `json:"message,omitempty"`
	Node       string              `json:"node,omitempty"`
	PodIP      string              `json:"podIP,omitempty"`
	StartTime  *time.Time          `json:"startTime,omitempty"`
	Deleting   bool                `json:"deleting,omitempty"`
	Conditions []snapshotCondition `json:"conditions,omitempty"`
	Containers []containerSnapshot `json:"containers,omitempty"`

	// runID and jobName are the pod's run-identity annotations, for a completion that
	// carries no identity of its own.
	runID, jobName string
	takenAt        time.Time
	// gone: the pod's deletion event, after which nothing reports on the worker.
	gone bool
}

// Proxy modes in a snapshot.
const (
	// ProxyModeColocated: the pool is in the set's namespace, and its readiness is read.
	ProxyModeColocated = "colocated"
	// ProxyModeShared: the pool is in another namespace, which the AGC's cache cannot
	// see, so its readiness is absent.
	ProxyModeShared = "shared"
)

// ProxySnapshot is the readiness of a worker's egress proxy pool. Ready and
// ReadyReplicas are set only for a colocated pool the cache holds.
type ProxySnapshot struct {
	Name string `json:"name"`
	Mode string `json:"mode"`
	// Ready is the EgressProxy's Ready condition status, "" where it has none.
	Ready         string `json:"ready,omitempty"`
	Reason        string `json:"readyReason,omitempty"`
	ReadyReplicas *int32 `json:"readyReplicas,omitempty"`
}

// EgressProxyReader reads the readiness of a set's egress proxy pool from the AGC's
// cache, nil for direct egress. The v2 Target implements it; v1 has no scale-set tier.
type EgressProxyReader interface {
	EgressProxyReadiness(ctx context.Context) *ProxySnapshot
}

type snapshotCondition struct {
	Type   string `json:"type"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
	at     time.Time
}

// containerSnapshot is one container's last termination, init containers and native
// sidecars included, and the one before it where a restarted container has since exited:
// the crash, which the pod's own shutdown replaces. The kubelet drops that one from the
// status it writes as the pod finishes, so it comes from an earlier event
// (testdata/sidecar_restart_statuses.json, kubelet v1.35.5).
type containerSnapshot struct {
	Name             string `json:"name"`
	Reason           string `json:"reason,omitempty"`
	ExitCode         int32  `json:"exitCode"`
	RestartCount     int32  `json:"restartCount,omitempty"`
	PreviousReason   string `json:"previousReason,omitempty"`
	PreviousExitCode int32  `json:"previousExitCode,omitempty"`
	// OOMReason is the limit a pod condition says ran out (oom_reason.go), only ever one of
	// the reasons oomReasonWords holds.
	OOMReason              string `json:"oomReason,omitempty"`
	id                     string
	sidecar                bool
	finishedAt, previousAt time.Time
}

// oomKilled reports whether a termination of c was c's own kill for memory. The runtime
// sets the reason for a kill anywhere in the container's cgroup, its nested containers'
// included, and keeps it for whatever exit follows (containerd 2.3.1), so a container
// other than the runner also needs a SIGKILL's exit code.
func (c containerSnapshot) oomKilled(reason string, exitCode int32) bool {
	if reason != containerReasonOOMKilled {
		return false
	}
	return exitCode == exitCodeSIGKILL || c.Name == runnerContainer && !c.sidecar
}

// oomFault is the fault of c's kill for memory.
func (c containerSnapshot) oomFault() string { return "container " + c.Name + " OOMKilled" }

// snapshotOf reads pod into a snapshot.
func snapshotOf(pod *corev1.Pod, now time.Time) *workerSnapshot {
	s := &workerSnapshot{
		Pod:      pod.Name,
		Phase:    string(pod.Status.Phase),
		Reason:   pod.Status.Reason,
		Message:  truncate(pod.Status.Message, snapshotMessageMax),
		Node:     pod.Spec.NodeName,
		PodIP:    pod.Status.PodIP,
		Deleting: pod.DeletionTimestamp != nil,
		runID:    pod.Annotations[AnnotationRunID],
		jobName:  pod.Annotations[annotationJobName],
		takenAt:  now,
	}
	if pod.Status.StartTime != nil {
		t := pod.Status.StartTime.UTC()
		s.StartTime = &t
	}
	for _, c := range pod.Status.Conditions {
		s.Conditions = append(s.Conditions, snapshotCondition{
			Type: string(c.Type), Status: string(c.Status), Reason: c.Reason, at: c.LastTransitionTime.Time})
	}
	add := func(statuses []corev1.ContainerStatus, sidecar bool) {
		for _, cs := range statuses {
			c := containerSnapshot{Name: cs.Name, RestartCount: cs.RestartCount, sidecar: sidecar}
			term, prev := cs.State.Terminated, cs.LastTerminationState.Terminated
			if term == nil {
				term, prev = prev, nil
			}
			if term != nil {
				c.Reason, c.ExitCode, c.finishedAt, c.id = term.Reason, term.ExitCode, term.FinishedAt.Time, term.ContainerID
			}
			if prev != nil {
				c.PreviousReason, c.PreviousExitCode, c.previousAt = prev.Reason, prev.ExitCode, prev.FinishedAt.Time
			}
			s.Containers = append(s.Containers, c)
		}
	}
	add(pod.Status.InitContainerStatuses, true)
	add(pod.Status.ContainerStatuses, false)
	for _, c := range pod.Status.Conditions {
		if container, reason, ok := oomReason(c); ok {
			s.noteOOMReason(container, reason)
		}
	}
	return s
}

// finished reports whether the worker had reached a terminal phase.
func (s *workerSnapshot) finished() bool {
	return s.Phase == string(corev1.PodSucceeded) || s.Phase == string(corev1.PodFailed)
}

// ended reports whether nothing after s can say more of the worker.
func (s *workerSnapshot) ended() bool {
	return s.finished() || s.gone
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// carryTerminations adds the terminations earlier read that s's own status has dropped,
// and keeps the reasons it read of the ones that stay.
func (s *workerSnapshot) carryTerminations(earlier *workerSnapshot) {
	for i := range s.Containers {
		c := &s.Containers[i]
		for _, e := range earlier.Containers {
			switch {
			case e.Name != c.Name || e.finishedAt.IsZero():
			case c.finishedAt.IsZero():
				c.Reason, c.ExitCode, c.finishedAt, c.id = e.Reason, e.ExitCode, e.finishedAt, e.id
				c.PreviousReason, c.PreviousExitCode, c.previousAt = e.PreviousReason, e.PreviousExitCode, e.previousAt
			case !c.previousAt.IsZero():
			case e.finishedAt.Equal(c.finishedAt):
				c.PreviousReason, c.PreviousExitCode, c.previousAt = e.PreviousReason, e.PreviousExitCode, e.previousAt
			default:
				c.PreviousReason, c.PreviousExitCode, c.previousAt = e.Reason, e.ExitCode, e.finishedAt
			}
		}
	}
	s.carryOOMReasons(earlier)
}

// faults lists what in the snapshot points at the platform rather than the job: a
// failed pod, a DisruptionTarget condition, a container killed for memory, the
// runner container exiting non-zero, or a colocated proxy pool that is not Ready or
// has no ready replica. A sidecar's non-zero exit is not one, since sidecars are
// routinely killed when the runner finishes.
func (s *workerSnapshot) faults() []string {
	return s.faultsBy(time.Time{})
}

// faultsBy is faults limited to what had happened by end, the zero time for no limit.
// The pod's phase carries no date, so a limit leaves it out.
func (s *workerSnapshot) faultsBy(end time.Time) []string {
	if s == nil {
		return nil
	}
	by := func(t time.Time) bool { return end.IsZero() || (!t.IsZero() && !t.After(end)) }
	var out []string
	if s.Phase == string(corev1.PodFailed) && end.IsZero() {
		out = append(out, strings.TrimSpace("pod Failed "+s.Reason))
	}
	for _, c := range s.Conditions {
		if c.Type == string(corev1.DisruptionTarget) && c.Status == string(corev1.ConditionTrue) && by(c.at) {
			out = append(out, "DisruptionTarget "+c.Reason)
		}
	}
	for _, c := range s.Containers {
		switch {
		case c.oomKilled(c.Reason, c.ExitCode) && by(c.finishedAt), c.oomKilled(c.PreviousReason, c.PreviousExitCode) && by(c.previousAt):
			out = append(out, c.oomFault())
		case c.Name == runnerContainer && !c.sidecar && c.ExitCode != 0 && by(c.finishedAt):
			out = append(out, fmt.Sprintf("container %s exited %d", c.Name, c.ExitCode))
		}
	}
	if px := s.Proxy; px != nil && px.Mode == ProxyModeColocated {
		switch {
		case px.Ready == string(metav1.ConditionFalse):
			fault := strings.TrimSpace("EgressProxy " + px.Name + " not Ready " + px.Reason)
			if px.ReadyReplicas != nil {
				fault += fmt.Sprintf(", %d ready replicas", *px.ReadyReplicas)
			}
			out = append(out, fault)
		case px.ReadyReplicas != nil && *px.ReadyReplicas == 0:
			out = append(out, "EgressProxy "+px.Name+" has no ready replicas")
		}
	}
	return out
}

// recordedFailure is a failed job whose completion found its worker not yet ended.
type recordedFailure struct {
	target                       Target
	runID, jobName, jobID, cause string
	// snap is the worker as the completion found it at that time, and context its JSON.
	// last is the latest read since, with the terminations every read before it showed.
	snap, last *workerSnapshot
	context    json.RawMessage
	at         time.Time
}

// workerSnapshots joins a scale-set worker's terminal snapshot to its job's completion,
// keyed by namespace/runner name: byID holds the snapshots that arrived first, and
// recorded the failures. A job that ends on a step's result completes before its worker
// finishes, and one whose runner was killed after (measured on dogfood, 2026-11-22,
// docs/design/04-operational-flows.md §4.2). A restart loses both.
type workerSnapshots struct {
	mu       sync.Mutex
	byID     map[string]*workerSnapshot
	recorded map[string]*recordedFailure
}

// awaits reports whether a recorded failure waits for the worker to finish.
func (w *workerSnapshots) awaits(key string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.recorded[key] != nil
}

// put returns the recorded failure whose worker s ends, or keeps s as the latest read of
// one whose worker runs on. With none waiting it stores s where it is the worker's
// terminal transition, unless the worker's job has completed and nothing will take it. A
// later read of a finished worker keeps the proxy readiness of the first, the nearer to
// the job's end.
func (w *workerSnapshots) put(key string, s *workerSnapshot, jobCompleted bool) *recordedFailure {
	w.mu.Lock()
	defer w.mu.Unlock()
	if r := w.recorded[key]; r != nil {
		s.carryTerminations(r.last)
		r.last = s
		if !s.ended() {
			return nil
		}
		delete(w.recorded, key)
		return r
	}
	// A finished pod's deletion is its reap, which says nothing its finish did not;
	// storing it again would hold a slot no completion takes.
	if jobCompleted || s.finished() == s.gone {
		return nil
	}
	if w.byID == nil {
		w.byID = map[string]*workerSnapshot{}
	}
	if held := w.byID[key]; held != nil && held.Pod == s.Pod {
		s.Proxy = held.Proxy
		s.carryOOMReasons(held)
	}
	putBounded(w.byID, key, s, maxWorkerSnapshots, func(s *workerSnapshot) time.Time { return s.takenAt })
	return nil
}

func (w *workerSnapshots) take(key string) *workerSnapshot {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.byID[key]
	delete(w.byID, key)
	return s
}

// hold keeps r until put brings its worker's end, or returns the snapshot of a worker that
// ended since the completion looked.
func (w *workerSnapshots) hold(key string, r *recordedFailure) *workerSnapshot {
	w.mu.Lock()
	defer w.mu.Unlock()
	if s := w.byID[key]; s != nil {
		delete(w.byID, key)
		s.carryTerminations(r.last)
		return s
	}
	if w.recorded == nil {
		w.recorded = map[string]*recordedFailure{}
	}
	putBounded(w.recorded, key, r, maxWorkerSnapshots, func(r *recordedFailure) time.Time { return r.at })
	return nil
}

// putBounded sets m[key], dropping the oldest entry past limit.
func putBounded[T any](m map[string]T, key string, v T, limit int, at func(T) time.Time) {
	if _, ok := m[key]; !ok && len(m) >= limit {
		oldest := ""
		for k, v := range m {
			if oldest == "" || at(v).Before(at(m[oldest])) {
				oldest = k
			}
		}
		delete(m, oldest)
	}
	m[key] = v
}

// failureIndex is the failure keys each set's ledger held at this process's last read
// or write of it, so a succeeded completion, the common case, costs no API call when
// its job has no record. A record written by another replica since is missed, and the
// pairing goes uncounted. floorsBySet is each set's memoryFloors at the same read, for
// the out-of-memory class's provisioning lookup (oom_class.go), and causesBySet the
// cause of GAG's latest re-run of each run, for the rerun-cause annotation.
type failureIndex struct {
	mu          sync.Mutex
	bySet       map[types.NamespacedName]map[string]bool
	floorsBySet map[types.NamespacedName]map[string]memoryFloor
	causesBySet map[types.NamespacedName]map[string]string
}

func (x *failureIndex) note(set types.NamespacedName, l *rerunLedger) {
	keys := make(map[string]bool, len(l.Failures))
	for k := range l.Failures {
		keys[k] = true
	}
	floors := make(map[string]memoryFloor, len(l.MemoryFloors))
	for k, m := range l.MemoryFloors {
		if m != nil {
			floors[k] = *m
		}
	}
	causes := map[string]string{}
	for runID, r := range l.Reruns {
		if r == nil || r.Count == 0 {
			continue
		}
		latest := -1
		for k, a := range r.Attempts {
			if n, err := strconv.Atoi(k); err == nil && a != nil && a.Cause != "" && n > latest {
				latest, causes[runID] = n, a.Cause
			}
		}
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.bySet == nil {
		x.bySet = map[types.NamespacedName]map[string]bool{}
		x.floorsBySet = map[types.NamespacedName]map[string]memoryFloor{}
		x.causesBySet = map[types.NamespacedName]map[string]string{}
	}
	x.bySet[set] = keys
	x.floorsBySet[set] = floors
	x.causesBySet[set] = causes
}

// rerunCause returns the cause of GAG's latest re-run of runID on set, "" where this
// process read none.
func (x *failureIndex) rerunCause(set types.NamespacedName, runID string) string {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.causesBySet[set][runID]
}

// floor returns set's memory floor for key, nil where it holds none, and whether this
// process has read the set's ledger at all.
func (x *failureIndex) floor(set types.NamespacedName, key string) (*memoryFloor, bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	floors, loaded := x.floorsBySet[set]
	if f, ok := floors[key]; ok {
		return &f, true
	}
	return nil, loaded
}

// lacks reports whether set's ledger is known not to hold key: false when it holds it
// or when this process has not read the ledger yet.
func (x *failureIndex) lacks(set types.NamespacedName, key string) bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	keys, loaded := x.bySet[set]
	return loaded && !keys[key]
}

// evictOldestFailure drops the failure record with the earliest failedAt, reporting
// false when there is none left to drop.
func evictOldestFailure(l *rerunLedger) bool {
	oldest := ""
	for k, f := range l.Failures {
		if oldest == "" || f.FailedAt.Before(l.Failures[oldest].FailedAt) {
			oldest = k
		}
	}
	if oldest == "" {
		return false
	}
	delete(l.Failures, oldest)
	return true
}

// pairFailure attributes the attempt that re-ran job in r's run after the attempt failed,
// from what GAG re-ran of that failed attempt. It returns the trigger and the cause GAG
// re-ran the job for, "" unless the trigger is gag. A single-job re-run takes the job's
// dependents with it, so it has collateral too.
func pairFailure(r *rerunRecord, failed int, job string) (trigger, cause string) {
	if r == nil {
		return rerunTriggerOther, ""
	}
	a := r.Attempts[strconv.Itoa(failed)]
	switch {
	case a == nil:
		return rerunTriggerOther, ""
	case slices.Contains(a.Jobs, job):
		return rerunTriggerGAG, a.Cause
	}
	return rerunTriggerGAGCollateral, ""
}

// rerunOutcomeEvents are the triggering events the outcome metric names; any other
// is "other", and none at all "unknown", which keeps the label bounded.
var rerunOutcomeEvents = []string{
	"push", "pull_request", "pull_request_target", "merge_group", "schedule",
	"workflow_dispatch", "workflow_run", "repository_dispatch", "release",
}

func rerunOutcomeEvent(event string) string {
	switch {
	case event == "":
		return "unknown"
	case slices.Contains(rerunOutcomeEvents, event):
		return event
	default:
		return "other"
	}
}

// completionPairing is what one completion did to the failures map.
type completionPairing struct {
	// recorded: a failed attempt was written, new or over an earlier one.
	recorded bool
	// matched: the key holds a record of another job UUID, which the completion is not
	// known to re-run. It changed nothing.
	matched bool
	// paired: a later attempt of a recorded failure completed.
	paired  bool
	trigger string
	cause   string
	event   string
	flaky   bool
	// timedOut: the earlier attempt was not a failure but a cancel by its own limit.
	timedOut bool
}

// SnapshotScaleSetWorker records a scale-set worker's platform context at its terminal
// transition: a terminal phase, or the deletion event of a pod that never reached one,
// which gone names. The snapshot waits for its job's completion, or amends the record of
// a job that failed first on its own goroutine, whose end closes the returned channel.
// proxy, which may be nil, supplies the set's egress proxy readiness.
func (p *Provisioner) SnapshotScaleSetWorker(ctx context.Context, proxy EgressProxyReader, pod *corev1.Pod, gone bool) <-chan struct{} {
	key, ok := snapshotKey(pod)
	if !ok {
		return closedChan()
	}
	s := snapshotOf(pod, p.nowFn())
	s.gone = gone
	if !s.ended() && !p.workerSnapshots.awaits(key) {
		return closedChan()
	}
	if proxy != nil {
		s.Proxy = proxy.EgressProxyReadiness(ctx)
	}
	r := p.workerSnapshots.put(key, s, ownJobCompleted(pod))
	if r == nil {
		return closedChan()
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		// A watch handler's context is cancelled when the handler returns.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), failureAmendBudget)
		defer cancel()
		p.amendFailedJob(ctx, r, s)
	}()
	return done
}

// snapshotKey is the key of pod's snapshot, ok only for a scale-set worker.
func snapshotKey(pod *corev1.Pod) (key string, ok bool) {
	runner := pod.Annotations[AnnotationRunnerName]
	if pod.Labels[LabelAcquisitionProtocol] != AcquisitionProtocolScaleSet || runner == "" {
		return "", false
	}
	return pod.Namespace + "/" + runner, true
}

// ownJobCompleted reports whether pod carries the completion stamp of a job its runner
// held. A surplus stamp is another job's, and its runner can still be given one.
func ownJobCompleted(pod *corev1.Pod) bool {
	_, stamped := pod.Annotations[AnnotationJobCompletedAt]
	_, surplus := pod.Annotations[AnnotationSurplusForJob]
	return stamped && !surplus
}

// RecordScaleSetJobCompleted records a scale-set job's completion in target's failure
// ledger: a failed or disrupted completion records the job and its worker's context,
// and a later attempt of a recorded job is attributed to whoever re-ran it. A cancelled
// completion with no disruption behind it records nothing: it is a user's cancel or a job
// that timed out, and is held until a later attempt succeeds (timed_out_jobs.go). It is
// idempotent per job.JobID.
//
// A failed job whose worker still runs is recorded at once, so a restart cannot lose its
// pairing, and amended when the worker finishes. A completion that matches a record is
// paired off this call, once the run's jobs are read (queuePairing); the channel closes
// when that ends.
func (p *Provisioner) RecordScaleSetJobCompleted(ctx context.Context, target Target, job ScaleSetJob, result string) (<-chan struct{}, error) {
	key := target.Key()
	at := p.nowFn()
	workerKey := key.Namespace + "/" + job.RunnerName
	snap := p.workerSnapshots.take(workerKey)
	runID, jobName := job.RunID, job.JobName
	if snap != nil {
		runID, jobName = firstNonEmpty(runID, snap.runID), firstNonEmpty(jobName, snap.jobName)
	}
	if runID == "" || jobName == "" {
		p.logForKey(key).Debug("scale-set job completion names no run or job; not recorded for flaky-job detection",
			"jobID", job.JobID, "result", result)
		return closedChan(), nil
	}
	failed, fkey := result != jobResultSucceeded, failureKey(runID, jobName)
	c := &jobCompletion{target: target, job: job, runID: runID, jobName: jobName, result: result}
	if !failed && job.RunnerName != "" {
		c.hold = p.cancelHolds.find(key, fkey, at)
	}
	if !failed && p.failureIndex.lacks(key, fkey) {
		if c.hold == nil {
			return closedChan(), nil
		}
		return p.queuePairing(ctx, c), nil
	}

	if failed {
		c.cause = p.disruptionCause(ctx, target, job.JobID)
		if result == jobResultCanceled && c.cause == "" {
			if job.RunnerName != "" {
				p.cancelHolds.put(key, fkey, &cancelHold{jobID: job.JobID, runner: job.RunnerName, event: job.EventName, at: at})
			}
			return closedChan(), nil
		}
		if snap == nil && job.RunnerName != "" {
			if pod, err := p.scaleSetWorkerByRunner(ctx, target, job.RunnerName); err == nil && pod != nil {
				snap = snapshotOf(pod, p.nowFn())
				if proxy, ok := target.(EgressProxyReader); ok {
					snap.Proxy = proxy.EgressProxyReadiness(ctx)
				}
			}
		}
		if snap != nil {
			c.context, _ = json.Marshal(snap)
		}
	}

	var o completionPairing
	err := p.updateRerunLedger(ctx, target, func(l *rerunLedger, now time.Time) (changed bool) {
		o, changed = c.apply(l, now, nil)
		return changed
	})
	done := closedChan()
	if (o.matched && job.RunnerName != "") || c.hold != nil {
		done = p.queuePairing(ctx, c)
	}
	if err != nil {
		return done, err
	}
	reported := failed && (o.recorded || o.matched)
	if reported {
		p.reportFailedJob(target, runID, jobName, c.cause, snap, snap.faults(), p.completionLog(c))
	}
	if !reported || snap == nil || snap.ended() {
		return done, nil
	}
	r := &recordedFailure{target: target, runID: runID, jobName: jobName, jobID: job.JobID, cause: c.cause,
		snap: snap, last: snap, context: c.context, at: at}
	s := p.workerSnapshots.hold(workerKey, r)
	if s == nil {
		// The pod can have changed since the completion read it, with no failure held yet
		// for its event to reach.
		if pod, err := p.scaleSetWorkerByRunner(ctx, target, job.RunnerName); err == nil && pod != nil {
			s = snapshotOf(pod, p.nowFn())
			if p.workerSnapshots.put(workerKey, s, true) == nil {
				s = nil
			}
		}
	}
	if s != nil {
		p.amendFailedJob(ctx, r, s)
	}
	return done, nil
}

// amendFailedJob gives r's record s, its worker's terminal snapshot, and reports the
// faults s adds: those dated before the completion, since what happens to a worker after
// its job has a result did not cause it. The proxy readiness stays the completion's, the
// nearer reading to the job's end. A record paired since, or holding an earlier
// attempt's context, is left as it is.
func (p *Provisioner) amendFailedJob(ctx context.Context, r *recordedFailure, s *workerSnapshot) {
	s.Proxy = r.snap.Proxy
	contextJSON, _ := json.Marshal(s)
	log := p.logForKey(r.target.Key()).With("runID", r.runID, "job", r.jobName, "jobID", r.jobID, "workerFinished", true)
	err := p.updateRerunLedger(ctx, r.target, func(l *rerunLedger, _ time.Time) bool {
		f := l.Failures[failureKey(r.runID, r.jobName)]
		if f == nil || !bytes.Equal(f.Context, r.context) {
			return false
		}
		f.Context = contextJSON
		return true
	})
	if err != nil {
		log.Warn("could not record a failed scale-set job's terminal worker context", "error", err)
	}
	seen := r.snap.faults()
	added := slices.DeleteFunc(s.faultsBy(r.at), func(f string) bool { return slices.Contains(seen, f) })
	p.reportFailedJob(r.target, r.runID, r.jobName, r.cause, s, added, log)
}

// firstNonEmpty returns a, or b where a is empty.
func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// reportFailedJob logs one failed attempt with its worker's context, at Warn and with
// a Warning Event where faults names a platform fault, so a log store that keeps
// only warnings still keeps every fault.
func (p *Provisioner) reportFailedJob(target Target, runID, jobName, cause string, snap *workerSnapshot, faults []string, log *slog.Logger) {
	snapJSON := "unknown"
	if snap != nil {
		b, _ := json.Marshal(snap)
		snapJSON = string(b)
	}
	if len(faults) == 0 {
		log.Info("scale-set job failed", "cause", cause, "context", snapJSON)
		return
	}
	said := snap.withOOMReasons(faults)
	log.Warn("scale-set job failed with a platform fault", "cause", cause, "faults", said, "context", snapJSON)
	note := func(faults []string) string {
		return fmt.Sprintf("Job %q of run %s failed on worker %s, with: %s.", jobName, runID, snap.Pod, strings.Join(faults, "; "))
	}
	text := note(said)
	if len(text) > maxEventNote {
		text = note(faults)
	}
	target.RecordEvent(corev1.EventTypeWarning, "PlatformFaultOnFailedJob", "DetectFlakyJob", text)
}

// reportRerunJob counts a later attempt of a recorded failure, and reports the flaky
// verdict where it is one.
func (p *Provisioner) reportRerunJob(target Target, runID, jobName, result string, o completionPairing, log *slog.Logger) {
	key := target.Key()
	outcome := "failed"
	if result == jobResultSucceeded {
		outcome = jobResultSucceeded
	}
	if p.Metrics != nil {
		if p.Metrics.ScaleSetJobReruns != nil {
			p.Metrics.ScaleSetJobReruns.WithLabelValues(key.Namespace, key.Name, o.trigger).Inc()
		}
		if o.trigger == rerunTriggerGAG && p.Metrics.EvictionRerunOutcomes != nil {
			p.Metrics.EvictionRerunOutcomes.WithLabelValues(key.Namespace, key.Name, o.cause, rerunOutcomeEvent(o.event), outcome).Inc()
		}
		if o.flaky && p.Metrics.ScaleSetFlakyJobs != nil {
			p.Metrics.ScaleSetFlakyJobs.WithLabelValues(key.Namespace, key.Name, o.trigger).Inc()
		}
	}
	log.Info("scale-set job re-run completed", "trigger", o.trigger, "cause", o.cause, "flaky", o.flaky)
	if !o.flaky {
		return
	}
	by := "someone other than GAG"
	if o.trigger == rerunTriggerGAGCollateral {
		by = "GAG, beside a disrupted job"
	}
	ended := "failed with no disruption attributed"
	if o.timedOut {
		ended = "reached its time limit"
	}
	target.RecordEvent(corev1.EventTypeNormal, "FlakyJobDetected", "DetectFlakyJob",
		fmt.Sprintf("Job %q of run %s %s and then succeeded when re-run by %s.",
			jobName, runID, ended, by))
}

// disruptionCause returns the cause a recovery claim records for jobID's worker, ""
// where no claim covers it or the claims cannot be read: an unread claim makes a
// disrupted job's later success count as flaky, an overcount detection can afford.
func (p *Provisioner) disruptionCause(ctx context.Context, target Target, jobID string) string {
	key := target.Key()
	var cm corev1.ConfigMap
	cmKey := types.NamespacedName{Namespace: key.Namespace, Name: scaleSetRecoveryClaimsConfigMapName(key.Name)}
	if err := p.liveReader().Get(ctx, cmKey, &cm); err != nil {
		return ""
	}
	ledger, err := decodeRecoveryClaims(cm.Data[recoveryClaimDataKey])
	if err != nil {
		return ""
	}
	for _, c := range ledger.Claims {
		if c.JobID == jobID {
			return c.Cause
		}
	}
	if c, held := ledger.Claims[scaleSetPodName(key.Name, jobID)]; held && c.JobID == "" {
		return c.Cause
	}
	return ""
}
