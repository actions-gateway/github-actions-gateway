package provisioner

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
)

// Classified platform retry on the scale-set tier (Q1183), and its first class: a runner
// killed for memory on a set whose sizing profile derives limits from history. A class
// is a failure shape GAG recognises from objects it owns and re-runs on the disruption
// arms' path, with their recovery claim, futility checks and ledger budget, once the
// shape's cause has cleared. This one clears by a value GAG computes: the next attempt's
// memory limit, raised from the killed pod's own limit rather than from the set's
// history, because the sampler records an out-of-memory job's peak at or below the limit
// it ran under, so the history may never move. A kill is withheld before any slot is
// spent where the next attempt could not get a higher limit, with no profile or at the
// profile's ceiling, and where it already got one raise. A re-run that holds a slot waits
// in the ledger until the run concludes (pending_rerun.go). The operator-facing account
// is docs/design/04-operational-flows.md §4.2.
//
// No arm knows whether the runner reached its own limit: a kill for memory that ran out
// above the container is expected to leave the same status, and nothing else the AGC can
// read separates the two. So every arm acts on the limit the next attempt would get, and
// no text it writes names the cause (oomKillFacts).
//
// Only a kill of the runner container is recognised. It is the one that ends the job
// on the pod's terminal transition; a sidecar killed for memory leaves the runner to
// report the job's own result, which would have to be read on its completion instead.

// Reason labels on EvictionRerunWithheld that only a class produces.
const (
	rerunWithheldReasonSameClassAgain = "same_class_again"
	rerunWithheldReasonBreakerOpen    = "breaker_open"
)

// containerReasonOOMKilled is the termination reason the kubelet reports for a
// container killed for memory.
const containerReasonOOMKilled = "OOMKilled"

// exitCodeSIGKILL is the exit code of a process killed by SIGKILL, as an out-of-memory
// kill ends one.
const exitCodeSIGKILL = 137

// memoryLimitStep rounds a raised limit up, matching the recommendation's 64Mi buckets.
const memoryLimitStep = 64 << 20

// memoryFloor is one job whose memory limit an out-of-memory retry raised, keyed by
// failureKey. Every later attempt of the job provisions at Limit at least, until it
// expires; a second kill of the job finds it and is withheld as same_class_again.
type memoryFloor struct {
	ExpiresAt time.Time `json:"expiresAt"`
	// Pod is the killed worker, so its own recovery replayed after a restart resumes
	// rather than reading as a second kill.
	Pod            string            `json:"pod"`
	Container      string            `json:"container"`
	KilledLimit    resource.Quantity `json:"killedLimit"`
	Limit          resource.Quantity `json:"limit"`
	RequestsFollow bool              `json:"requestsFollow,omitempty"`
	extra          map[string]json.RawMessage
}

// oomKilledAwaitingRecovery reports whether pod is an unclaimed worker that failed
// because its runner container was killed for memory.
func oomKilledAwaitingRecovery(pod *corev1.Pod) bool {
	if _, handled := pod.Annotations[AnnotationEvictionHandledAt]; handled {
		return false
	}
	if pod.Status.Phase != corev1.PodFailed {
		return false
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == runnerContainer && cs.State.Terminated != nil {
			return cs.State.Terminated.Reason == containerReasonOOMKilled
		}
	}
	return false
}

// containerMemory returns the named container's memory request and limit in pod's spec,
// each zero where it has none.
func containerMemory(pod *corev1.Pod, name string) (request, limit resource.Quantity) {
	for i := range pod.Spec.Containers {
		if r := &pod.Spec.Containers[i].Resources; pod.Spec.Containers[i].Name == name {
			return r.Requests[corev1.ResourceMemory], r.Limits[corev1.ResourceMemory]
		}
	}
	return resource.Quantity{}, resource.Quantity{}
}

// oomKillFacts says what is known of pod's killed runner, to follow "had its runner". The
// request and the node are what an operator has to judge the cause by: the kubelet scores
// a Burstable container's chance of a node-wide kill by its request (qos/policy.go,
// v1.35.5), and a SystemOOM Event is on the node.
func oomKillFacts(pod *corev1.Pod) string {
	request, limit := containerMemory(pod, runnerContainer)
	asked := "no memory request"
	if !request.IsZero() {
		asked = "a memory request of " + request.String()
	}
	return fmt.Sprintf("killed for memory on node %s, with %s and a limit of %s; its termination reason does not say whether it reached that limit or memory ran out above it, as on a full node",
		pod.Spec.NodeName, asked, limit.String())
}

// nextMemoryLimit is the limit a retry gives the killed container: killed ×
// HeadroomPercent/100, rounded up to memoryLimitStep, capped at the policy's ceiling or
// at twice killed where it names none. It can be at or below killed, which the caller
// withholds.
func nextMemoryLimit(killed resource.Quantity, policy MemoryRetryPolicy) resource.Quantity {
	b := killed.Value()
	next := (b*policy.HeadroomPercent/100 + memoryLimitStep - 1) / memoryLimitStep * memoryLimitStep
	ceiling := 2 * b
	if !policy.Ceiling.IsZero() {
		ceiling = policy.Ceiling.Value()
	}
	return *resource.NewQuantity(min(next, ceiling), resource.BinarySI)
}

// recoverOOMKilled is the out-of-memory class's recovery of one claimed worker: it
// withholds a kill the next attempt would repeat, records the raised limit for the
// job's next attempt, and hands the re-run to the disruption arms' path.
func (p *Provisioner) recoverOOMKilled(ctx context.Context, target Target, spec *ResolvedSpec, pod *corev1.Pod, owner, repo, runID string, log *slog.Logger) <-chan struct{} {
	job := pod.Annotations[annotationJobName]
	_, killed := containerMemory(pod, runnerContainer)
	switch {
	case killed.IsZero():
		// No limit of its own to raise, a shape no class covers yet.
		log.Info("runner was killed for memory with no memory limit of its own; no automatic re-run", "runID", runID)
		return closedChan()
	case job == "":
		log.Warn("runner was killed for memory but the worker names no job to raise the limit for; no automatic re-run",
			"runID", runID)
		return closedChan()
	}
	facts := fmt.Sprintf("worker pod %s for run %s had its runner %s", pod.Name, runID, oomKillFacts(pod))
	if spec.MemoryRetry == nil || pod.Annotations[AnnotationSizingProfile] == "" {
		p.withholdOOMRerun(target, runID, log, rerunWithheldReasonOwnLimit, facts+". No automatic re-run was requested, because the set's sizing profile did not derive that limit from history and a re-run may get the same one. If the runner reached it, raise it, or use the Binpack or Throughput sizing profile, which derives one once it has samples; then, in either case, re-run it by hand")
		return closedChan()
	}
	next := nextMemoryLimit(killed, *spec.MemoryRetry)
	if next.Cmp(killed) <= 0 {
		p.withholdOOMRerun(target, runID, log, rerunWithheldReasonOwnLimit, facts+". No automatic re-run was requested, because that limit is already the most the sizing profile allows and a re-run would get the same one. If the runner reached it, raise spec.sizing.maxRequests memory or reduce what the job uses; then, in either case, re-run it by hand")
		return closedChan()
	}
	breaker := classBreakerKey{set: target.Key(), cause: recoveryCauseOOM}
	now := p.nowFn()
	p.closeExpiredBreakers(now)
	if p.classBreakers.open(breaker, now) {
		p.withholdOOMRerun(target, runID, log, rerunWithheldReasonBreakerOpen, fmt.Sprintf("%s. No automatic re-run was requested, because out-of-memory retries on this set are paused: more than %d runs were re-run for memory within %s, which points at a shared cause rather than one job's growth. Retries resume %s after the pause began; re-run it by hand if this job needs it",
			facts, classBreakerRuns, classBreakerWindow, classBreakerCooldown))
		return closedChan()
	}

	prior, err := p.recordMemoryFloor(ctx, target, runID, job, memoryFloor{
		Pod: pod.Name, Container: runnerContainer, KilledLimit: killed, Limit: next,
		RequestsFollow: spec.MemoryRetry.RequestsFollow,
	})
	switch {
	case err != nil:
		log.Error("could not record the raised memory limit; the re-run is withheld, manual rerun may be required",
			"runID", runID, "cause", recoveryCauseOOM, "error", err)
		p.recordRerunFailure(target, runID, recoveryCauseOOM, rerunFailureReasonBudgetUnrecorded, err)
		return closedChan()
	case prior != nil:
		p.withholdOOMRerun(target, runID, log, rerunWithheldReasonSameClassAgain, fmt.Sprintf("%s. No automatic re-run was requested, because a job gets one raise and job %q had its own after an earlier kill, from %s to %s, which lasts until %s at the latest. If the runner reached its limit, look at what grew or raise it; then, in either case, re-run it by hand",
			facts, job, prior.KilledLimit.String(), prior.Limit.String(), prior.ExpiresAt.UTC().Format(time.RFC3339)))
		return closedChan()
	}
	// The breaker counts, and the Event promises, only a run that holds a slot: a
	// futile or over-budget one is reported by the recovery path instead.
	onReserved := func() {
		if p.classBreakers.record(breaker, runID, p.nowFn()) {
			p.setAutoRetryPaused(breaker, 1)
			log.Warn("out-of-memory retries paused on this set: too many runs killed in a short window",
				"runs", classBreakerRuns, "window", classBreakerWindow, "pause", classBreakerCooldown)
			target.RecordEvent(corev1.EventTypeWarning, "AutoRetryPaused", "RetryEvictedJob",
				fmt.Sprintf("out-of-memory retries are paused for %s: more than %d runs had a runner killed for memory within %s, which points at a shared cause rather than one job's growth",
					classBreakerCooldown, classBreakerRuns, classBreakerWindow))
		}
		log.Info("raising the job's memory limit for its re-run", "runID", runID, "job", job,
			"container", runnerContainer, "killedLimit", killed.String(), "limit", next.String())
		target.RecordEvent(corev1.EventTypeNormal, "MemoryLimitRaisedForRetry", "RetryEvictedJob",
			fmt.Sprintf("%s. Job %q gets a limit of %s on its next attempt, and is re-run automatically once the run concludes", facts, job, next.String()))
	}
	// Parked rather than re-run now: a job that outgrows its memory is often one of a long
	// matrix, and GitHub refuses the re-run until every sibling has concluded.
	d := disruptionOf(pod)
	d.onReserved, d.park = onReserved, true
	return p.handleEviction(ctx, target, owner, repo, runID, log, spec.MaxEvictionRetries, spec.EvictionRetryDelay,
		evictionTierScaleSet, recoveryCauseOOM, d)
}

// withholdOOMRerun logs and records one withheld out-of-memory retry.
func (p *Provisioner) withholdOOMRerun(target Target, runID string, log *slog.Logger, reason, message string) {
	log.Info("runner killed for memory; no automatic re-run", "runID", runID, "reason", reason)
	p.recordRerunWithheld(target, runID, recoveryCauseOOM, reason, message)
}

// recordMemoryFloor records f for job's next attempts under the ledger's
// compare-and-swap. It returns the floor an earlier kill of the job already recorded,
// nil where there was none or the one there is this pod's own.
func (p *Provisioner) recordMemoryFloor(ctx context.Context, target Target, runID, job string, f memoryFloor) (*memoryFloor, error) {
	key := failureKey(runID, job)
	var prior *memoryFloor
	err := p.updateRerunLedger(ctx, target, func(l *rerunLedger, now time.Time) bool {
		prior = nil
		if m := l.MemoryFloors[key]; m != nil {
			if m.Pod != f.Pod {
				prior = m
			}
			return false
		}
		f.ExpiresAt = now.Add(failureRecordTTL)
		l.MemoryFloors[key] = &f
		return true
	})
	return prior, err
}

// evictOldestMemoryFloors drops floors, the soonest to expire first, until their
// encoded size covers excess bytes, reporting whether it dropped any. Freeing a batch
// keeps one write to one re-encode per pass, where dropping a floor per pass made a
// ledger full of floors quadratic to write. The job a floor raised then provisions at
// its resolved limit again, and a second kill of it reads as a first one, which the
// run's budget still bounds.
func evictOldestMemoryFloors(l *rerunLedger, excess int) bool {
	keys := slices.SortedFunc(maps.Keys(l.MemoryFloors), func(a, b string) int {
		return l.MemoryFloors[a].ExpiresAt.Compare(l.MemoryFloors[b].ExpiresAt)
	})
	freed := 0
	for _, k := range keys {
		if freed >= excess {
			break
		}
		b, _ := json.Marshal(l.MemoryFloors[k])
		freed += len(k) + len(b) + len(`"":,`)
		delete(l.MemoryFloors, k)
	}
	return freed > 0
}

// applyMemoryFloor returns spec with job's memory limit raised to the floor an
// out-of-memory retry recorded for it, where the resolved limit is lower, and spec
// itself otherwise; it never writes through spec. The lookup is the failure
// index, so a job with no floor costs no API call once this process has read the set's
// ledger, and one read before that; a floor another replica wrote since is missed. A
// ledger that cannot be read provisions at the resolved limit, and a second kill is
// then withheld as same_class_again.
func (p *Provisioner) applyMemoryFloor(ctx context.Context, target Target, spec *ResolvedSpec, job ScaleSetJob, log *slog.Logger) *ResolvedSpec {
	if job.RunID == "" || job.JobName == "" {
		return spec
	}
	key, fkey := target.Key(), failureKey(job.RunID, job.JobName)
	f, loaded := p.failureIndex.floor(key, fkey)
	if !loaded {
		if _, _, err := p.readRerunLedger(ctx, target); err != nil {
			log.Warn("could not read the failure ledger for a raised memory limit; provisioning at the resolved limit", "error", err)
			return spec
		}
		f, _ = p.failureIndex.floor(key, fkey)
	}
	if f == nil {
		return spec
	}
	tmpl := spec.PodTemplate.DeepCopy()
	for i := range tmpl.Spec.Containers {
		c := &tmpl.Spec.Containers[i]
		if c.Name != f.Container {
			continue
		}
		resolved := c.Resources.Limits[corev1.ResourceMemory]
		if resolved.Cmp(f.Limit) >= 0 {
			return spec
		}
		if c.Resources.Limits == nil {
			c.Resources.Limits = corev1.ResourceList{}
		}
		c.Resources.Limits[corev1.ResourceMemory] = f.Limit
		if f.RequestsFollow {
			if c.Resources.Requests == nil {
				c.Resources.Requests = corev1.ResourceList{}
			}
			c.Resources.Requests[corev1.ResourceMemory] = f.Limit
		}
		raised := *spec
		raised.PodTemplate = *tmpl
		log.Info("provisioning at the memory limit an out-of-memory retry raised", "container", f.Container,
			"resolvedLimit", resolved.String(), "limit", f.Limit.String())
		return &raised
	}
	return spec
}

// applyRerunCause returns spec with its template carrying AnnotationRerunCause where
// GAG has re-run job's run, and without a tenant-set one either way; it never writes
// through spec. The lookup is the failure index applyMemoryFloor has just loaded, so it
// costs no API call.
func (p *Provisioner) applyRerunCause(target Target, spec *ResolvedSpec, job ScaleSetJob) *ResolvedSpec {
	cause := ""
	if job.RunID != "" {
		cause = p.failureIndex.rerunCause(target.Key(), job.RunID)
	}
	if _, set := spec.PodTemplate.Annotations[AnnotationRerunCause]; !set && cause == "" {
		return spec
	}
	stamped := *spec
	stamped.PodTemplate = *spec.PodTemplate.DeepCopy()
	delete(stamped.PodTemplate.Annotations, AnnotationRerunCause)
	if cause != "" {
		if stamped.PodTemplate.Annotations == nil {
			stamped.PodTemplate.Annotations = map[string]string{}
		}
		stamped.PodTemplate.Annotations[AnnotationRerunCause] = cause
	}
	return &stamped
}

// The stampede breaker. A cause firing for many runs at once is a shared outage, not
// many jobs outgrowing their limits, and re-running every one doubles the load on
// whatever failed. One breaker per set and class cause, in memory: a restart closes it,
// and the per-run budget still bounds what that costs. The disruption arms are outside
// it, because a mass Spot preemption is exactly when their recovery is wanted.
const (
	classBreakerWindow   = 10 * time.Minute
	classBreakerRuns     = 10
	classBreakerCooldown = 30 * time.Minute
)

type classBreakerKey struct {
	set   types.NamespacedName
	cause string
}

type classBreaker struct {
	// fires is each run the class fired for inside the window, by its latest fire.
	fires    map[string]time.Time
	openedAt time.Time
}

// classBreakers holds every set's breakers. Its zero value is ready to use.
type classBreakers struct {
	mu    sync.Mutex
	byKey map[classBreakerKey]*classBreaker
}

// open reports whether key's breaker is open at now. One whose cooldown has passed
// closes with an empty window.
func (c *classBreakers) open(key classBreakerKey, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	b := c.byKey[key]
	if b == nil || b.openedAt.IsZero() {
		return false
	}
	if now.Sub(b.openedAt) < classBreakerCooldown {
		return true
	}
	b.openedAt, b.fires = time.Time{}, nil
	return false
}

// closeExpired closes every breaker whose cooldown has passed at now, returning their
// keys, so the paused gauge drops when the pause ends rather than at the next fire.
func (c *classBreakers) closeExpired(now time.Time) []classBreakerKey {
	c.mu.Lock()
	defer c.mu.Unlock()
	var closed []classBreakerKey
	for key, b := range c.byKey {
		if !b.openedAt.IsZero() && now.Sub(b.openedAt) >= classBreakerCooldown {
			b.openedAt, b.fires = time.Time{}, nil
			closed = append(closed, key)
		}
	}
	return closed
}

// closeExpiredBreakers closes the breakers whose pause has ended and zeroes their
// paused gauge. The pending sweep's tick calls it, and every class fire before it reads
// its breaker at the same now, so open never closes a breaker the gauge still reports.
func (p *Provisioner) closeExpiredBreakers(now time.Time) {
	for _, key := range p.classBreakers.closeExpired(now) {
		p.setAutoRetryPaused(key, 0)
	}
}

// setAutoRetryPaused sets key's actions_gateway_auto_retry_paused series.
func (p *Provisioner) setAutoRetryPaused(key classBreakerKey, v float64) {
	if p.Metrics == nil || p.Metrics.AutoRetryPaused == nil {
		return
	}
	p.Metrics.AutoRetryPaused.WithLabelValues(key.set.Namespace, key.set.Name, key.cause).Set(v)
}

// record counts a fire for runID, reporting whether it opened the breaker: more than
// classBreakerRuns distinct runs inside classBreakerWindow.
func (c *classBreakers) record(key classBreakerKey, runID string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byKey == nil {
		c.byKey = map[classBreakerKey]*classBreaker{}
	}
	b := c.byKey[key]
	if b == nil {
		b = &classBreaker{}
		c.byKey[key] = b
	}
	if b.fires == nil {
		b.fires = map[string]time.Time{}
	}
	for run, at := range b.fires {
		if now.Sub(at) >= classBreakerWindow {
			delete(b.fires, run)
		}
	}
	b.fires[runID] = now
	if b.openedAt.IsZero() && len(b.fires) > classBreakerRuns {
		b.openedAt = now
		return true
	}
	return false
}
