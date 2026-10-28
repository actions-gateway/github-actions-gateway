package provisioner

import (
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
	"k8s.io/apimachinery/pkg/types"
)

// Flaky-job detection on the scale-set tier (Q555). A failed JobCompleted records the
// job in the failure ledger's failures map with the worker's platform context; a later
// completion of the same job (same run, same display name, a new job ID) pairs with the
// record and is attributed to whoever re-ran it, from GAG's own re-runs of the run in
// the same ledger. Detection changes no behaviour. The operator-facing account is
// docs/design/04-operational-flows.md §4.2.

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

// maxWorkerSnapshots bounds the snapshots awaiting their job's completion. A snapshot
// is consumed by its completion, so the bound only binds when completions stop arriving.
const maxWorkerSnapshots = 1024

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
	// Cause is the disruption a recovery claim names for the latest failed attempt, ""
	// where none did. A failure with a cause is never counted flaky.
	Cause       string `json:"cause,omitempty"`
	Event       string `json:"event,omitempty"`
	WorkflowRef string `json:"workflowRef,omitempty"`
	// PairedAttempts are the keys of GAG's re-runs of the run already attributed to an
	// earlier attempt of this job, so a later attempt does not claim them again.
	PairedAttempts []string `json:"pairedAttempts,omitempty"`
	// Context is the worker snapshot, kept as raw JSON so a version that knows fewer of
	// its fields rewrites it whole. Absent means unknown: no snapshot and no pod.
	Context json.RawMessage `json:"context,omitempty"`
	extra   map[string]json.RawMessage
}

// failureKey keys a job across its attempts: the job UUID changes on a re-run, the run
// ID and the display name do not.
func failureKey(runID, jobName string) string {
	return runID + "/" + jobName
}

// workerSnapshot is the platform context of a worker at its terminal transition, from
// objects the AGC already reads. Node conditions are not in it: the AGC has no nodes
// RBAC, and the pod's DisruptionTarget condition carries what a node loss shows.
type workerSnapshot struct {
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
}

type snapshotCondition struct {
	Type   string `json:"type"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// containerSnapshot is one container's last termination, init containers and native
// sidecars included.
type containerSnapshot struct {
	Name         string `json:"name"`
	Reason       string `json:"reason,omitempty"`
	ExitCode     int32  `json:"exitCode"`
	RestartCount int32  `json:"restartCount,omitempty"`
	sidecar      bool
}

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
		s.Conditions = append(s.Conditions, snapshotCondition{Type: string(c.Type), Status: string(c.Status), Reason: c.Reason})
	}
	add := func(statuses []corev1.ContainerStatus, sidecar bool) {
		for _, cs := range statuses {
			c := containerSnapshot{Name: cs.Name, RestartCount: cs.RestartCount, sidecar: sidecar}
			term := cs.State.Terminated
			if term == nil {
				term = cs.LastTerminationState.Terminated
			}
			if term != nil {
				c.Reason, c.ExitCode = term.Reason, term.ExitCode
			}
			s.Containers = append(s.Containers, c)
		}
	}
	add(pod.Status.InitContainerStatuses, true)
	add(pod.Status.ContainerStatuses, false)
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// faults lists what in the snapshot points at the platform rather than the job: a
// failed pod, a DisruptionTarget condition, any container killed for memory, or the
// runner container exiting non-zero. A sidecar's non-zero exit is not one, since
// sidecars are routinely killed when the runner finishes.
func (s *workerSnapshot) faults() []string {
	if s == nil {
		return nil
	}
	var out []string
	if s.Phase == string(corev1.PodFailed) {
		out = append(out, strings.TrimSpace("pod Failed "+s.Reason))
	}
	for _, c := range s.Conditions {
		if c.Type == string(corev1.DisruptionTarget) && c.Status == string(corev1.ConditionTrue) {
			out = append(out, "DisruptionTarget "+c.Reason)
		}
	}
	for _, c := range s.Containers {
		switch {
		case c.Reason == "OOMKilled":
			out = append(out, "container "+c.Name+" OOMKilled")
		case c.Name == runnerContainer && !c.sidecar && c.ExitCode != 0:
			out = append(out, fmt.Sprintf("container %s exited %d", c.Name, c.ExitCode))
		}
	}
	return out
}

// workerSnapshots holds each scale-set worker's snapshot from its terminal transition
// until its job's completion takes it, keyed by namespace/runner name. A restart loses
// them, and the records written after it say the context is unknown.
type workerSnapshots struct {
	mu   sync.Mutex
	byID map[string]*workerSnapshot
}

func (w *workerSnapshots) put(key string, s *workerSnapshot) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.byID == nil {
		w.byID = map[string]*workerSnapshot{}
	}
	if _, ok := w.byID[key]; !ok && len(w.byID) >= maxWorkerSnapshots {
		oldest := ""
		for k, v := range w.byID {
			if oldest == "" || v.takenAt.Before(w.byID[oldest].takenAt) {
				oldest = k
			}
		}
		delete(w.byID, oldest)
	}
	w.byID[key] = s
}

func (w *workerSnapshots) take(key string) *workerSnapshot {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.byID[key]
	delete(w.byID, key)
	return s
}

// failureIndex is the failure keys each set's ledger held at this process's last read
// or write of it, so a succeeded completion, the common case, costs no API call when
// its job has no record. A record written by another replica since is missed, and the
// pairing goes uncounted.
type failureIndex struct {
	mu    sync.Mutex
	bySet map[types.NamespacedName]map[string]bool
}

func (x *failureIndex) note(set types.NamespacedName, l *rerunLedger) {
	keys := make(map[string]bool, len(l.Failures))
	for k := range l.Failures {
		keys[k] = true
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.bySet == nil {
		x.bySet = map[types.NamespacedName]map[string]bool{}
	}
	x.bySet[set] = keys
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

// pairFailure attributes a later attempt of the job f records to whoever re-ran it,
// from GAG's re-runs of the run, r. It returns the trigger, the cause GAG re-ran the
// job for ("" unless the trigger is gag), and the re-run's attempt key ("" for other).
//
// The completion carries no run attempt, so the re-run GAG made of the attempt the job
// failed in cannot be picked out exactly: each of GAG's re-runs not already paired with
// this job is a candidate, latest first, and one that lists the job outranks one that
// re-ran it as collateral.
func pairFailure(r *rerunRecord, f *failureRecord, job string) (trigger, cause, attempt string) {
	if r == nil {
		return rerunTriggerOther, "", ""
	}
	keys := make([]string, 0, len(r.Attempts))
	for k, a := range r.Attempts {
		if a != nil && !slices.Contains(f.PairedAttempts, k) {
			keys = append(keys, k)
		}
	}
	slices.SortFunc(keys, func(a, b string) int {
		x, _ := strconv.Atoi(a)
		y, _ := strconv.Atoi(b)
		return y - x
	})
	for _, k := range keys {
		if a := r.Attempts[k]; slices.Contains(a.Jobs, job) {
			return rerunTriggerGAG, a.Cause, k
		}
	}
	for _, k := range keys {
		if r.Attempts[k].Scope == rerunScopeFailedJobs {
			return rerunTriggerGAGCollateral, "", k
		}
	}
	return rerunTriggerOther, "", ""
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
	// paired: a later attempt of a recorded failure completed.
	paired  bool
	trigger string
	cause   string
	event   string
	flaky   bool
}

// SnapshotScaleSetWorker records a scale-set worker's platform context at its
// terminal transition, for its job's completion to take: a terminal phase, or a
// deletion of a pod that never reached one. It is in-memory only and never fails.
func (p *Provisioner) SnapshotScaleSetWorker(pod *corev1.Pod) {
	if pod.Labels[LabelAcquisitionProtocol] != AcquisitionProtocolScaleSet {
		return
	}
	runner := pod.Annotations[AnnotationRunnerName]
	if runner == "" {
		return
	}
	terminal := pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed
	// A terminal pod's deletion is its reap, which says nothing the terminal transition
	// did not; snapshotting it again would hold a slot no completion takes.
	if terminal == (pod.DeletionTimestamp != nil) {
		return
	}
	p.workerSnapshots.put(pod.Namespace+"/"+runner, snapshotOf(pod, p.nowFn()))
}

// RecordScaleSetJobCompleted records a scale-set job's completion in target's failure
// ledger: a failed or disrupted completion records the job and its worker's context,
// and a later attempt of a recorded job is attributed to whoever re-ran it. A
// cancelled completion with no disruption behind it is a user's cancel and records
// nothing. It is idempotent per job.JobID.
func (p *Provisioner) RecordScaleSetJobCompleted(ctx context.Context, target Target, job ScaleSetJob, result string) error {
	key := target.Key()
	snap := p.workerSnapshots.take(key.Namespace + "/" + job.RunnerName)
	runID, jobName := job.RunID, job.JobName
	if snap != nil {
		runID, jobName = firstNonEmpty(runID, snap.runID), firstNonEmpty(jobName, snap.jobName)
	}
	if runID == "" || jobName == "" {
		p.logForKey(key).Debug("scale-set job completion names no run or job; not recorded for flaky-job detection",
			"jobID", job.JobID, "result", result)
		return nil
	}
	fkey := failureKey(runID, jobName)
	failed := result != jobResultSucceeded
	if !failed && p.failureIndex.lacks(key, fkey) {
		return nil
	}

	var cause string
	var contextJSON json.RawMessage
	if failed {
		cause = p.disruptionCause(ctx, target, job.JobID)
		if result == jobResultCanceled && cause == "" {
			return nil
		}
		if snap == nil && job.RunnerName != "" {
			if pod, err := p.scaleSetWorkerByRunner(ctx, target, job.RunnerName); err == nil && pod != nil {
				snap = snapshotOf(pod, p.nowFn())
			}
		}
		if snap != nil {
			contextJSON, _ = json.Marshal(snap)
		}
	}

	var o completionPairing
	err := p.updateRerunLedger(ctx, target, func(l *rerunLedger, now time.Time) bool {
		o = completionPairing{}
		f := l.Failures[fkey]
		if f == nil {
			if !failed {
				return false
			}
			l.Failures[fkey] = &failureRecord{
				ExpiresAt: now.Add(failureRecordTTL), FailedAt: now, JobID: job.JobID, Cause: cause,
				Event: job.EventName, WorkflowRef: job.WorkflowRef, Context: contextJSON,
			}
			o.recorded = true
			return true
		}
		if f.JobID == job.JobID {
			return false
		}
		var attempt string
		o.paired, o.event = true, f.Event
		o.trigger, o.cause, attempt = pairFailure(l.Reruns[runID], f, jobName)
		if !failed {
			o.flaky = o.trigger != rerunTriggerGAG && f.Cause == ""
			delete(l.Failures, fkey)
			return true
		}
		o.recorded = true
		f.JobID, f.Cause, f.ExpiresAt = job.JobID, cause, now.Add(failureRecordTTL)
		if attempt != "" {
			f.PairedAttempts = append(f.PairedAttempts, attempt)
		}
		return true
	})
	if err != nil {
		return err
	}
	log := p.logForKey(key).With("runID", runID, "job", jobName, "jobID", job.JobID, "result", result)
	if o.recorded {
		p.reportFailedJob(target, runID, jobName, cause, snap, log)
	}
	if o.paired {
		p.reportRerunJob(target, runID, jobName, result, o, log)
	}
	return nil
}

// firstNonEmpty returns a, or b where a is empty.
func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// reportFailedJob logs one failed attempt with its worker's context, at Warn and with
// a Warning Event where the context shows a platform fault, so a log store that keeps
// only warnings still keeps every fault.
func (p *Provisioner) reportFailedJob(target Target, runID, jobName, cause string, snap *workerSnapshot, log *slog.Logger) {
	snapJSON := "unknown"
	if snap != nil {
		b, _ := json.Marshal(snap)
		snapJSON = string(b)
	}
	faults := snap.faults()
	if len(faults) == 0 {
		log.Info("scale-set job failed", "cause", cause, "context", snapJSON)
		return
	}
	log.Warn("scale-set job failed with a platform fault", "cause", cause, "faults", faults, "context", snapJSON)
	target.RecordEvent(corev1.EventTypeWarning, "PlatformFaultOnFailedJob", "DetectFlakyJob",
		fmt.Sprintf("Job %q of run %s failed on worker %s with a platform fault: %s.",
			jobName, runID, snap.Pod, strings.Join(faults, "; ")))
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
			p.Metrics.EvictionRerunOutcomes.WithLabelValues(key.Namespace, key.Name, evictionTierScaleSet,
				o.cause, rerunOutcomeEvent(o.event), outcome).Inc()
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
	target.RecordEvent(corev1.EventTypeNormal, "FlakyJobDetected", "DetectFlakyJob",
		fmt.Sprintf("Job %q of run %s failed with no disruption attributed and then succeeded when re-run by %s.",
			jobName, runID, by))
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
