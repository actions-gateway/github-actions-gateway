package provisioner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/actions-gateway/github-actions-gateway/api/apinames"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The per-set failure ledger and the scale-set re-run budget it holds (Q1182).
//
// The budget used to be the in-process evictionCounts map, so an AGC restart refilled
// every run's maxEvictionRetries, and two disruptions in one run each reserved a slot
// and each landed a re-run. It now lives in a ConfigMap per scale-set RunnerSet, as a
// count of GAG's own re-runs per run plus a record of what each re-run covered, so a
// second disruption of an attempt GAG already re-ran merges into that re-run instead of
// spending a slot and re-running the run again. The classic tier keeps evictionCounts.
//
// Two AGC versions share this object during every rollout and rollback, so it is built
// to be rewritten by a version that does not understand all of it: unknown fields and
// maps survive a rewrite, every map entry ages itself by its own expiresAt, and a
// writer below minWriterVersion reads but never writes. The operator-facing account is
// docs/design/04-operational-flows.md §4.2. Q555 adds the failures map (flaky_jobs.go),
// and Q1183 the pending map.

// rerunLedgerDataKey is the one data key in a failure-ledger ConfigMap.
const rerunLedgerDataKey = "failures.json"

// rerunLedgerVersion is the schema this AGC writes. rerunLedgerMinWriterVersion is the
// oldest AGC allowed to write what this one writes; it rises only for a change an older
// writer cannot make safely, and never falls.
const (
	rerunLedgerVersion          = 1
	rerunLedgerMinWriterVersion = 1
)

// rerunRecordTTL keeps a run's budget for GitHub's 30-day re-run window, measured from
// the run's latest reservation, so a budget cannot refill while the run can still be
// re-run.
const rerunRecordTTL = 30 * 24 * time.Hour

// failureRecordTTL keeps a failed job's record long enough for a re-run a day or a
// weekend later to pair with it.
const failureRecordTTL = 7 * 24 * time.Hour

// rerunLedgerMaxBytes caps the encoded ledger under the 1 MiB ConfigMap ceiling. Failure
// records are evicted oldest first to make room. A budget entry never is, because
// evicting one refills its run's budget: a write the budgets alone would push past the
// cap fails, and its re-run is withheld.
const rerunLedgerMaxBytes = 512 << 10

// Re-run scopes: what one re-run covered. rerun-failed-jobs, the only call GAG makes
// today, covers every failed job of the attempt; scopeJob is reserved for a
// single-job re-run, which covers only the jobs it lists.
const (
	rerunScopeFailedJobs = "FailedJobs"
	rerunScopeJob        = "Job"
)

var (
	// errRerunLedgerReadOnly reports a ledger whose minWriterVersion is above this AGC's
	// schema: it may read it but must not write it.
	errRerunLedgerReadOnly = errors.New("the failure ledger was written by a newer AGC that this version may not write")
	// errRerunLedgerFull reports a write that would pass rerunLedgerMaxBytes.
	errRerunLedgerFull = errors.New("the failure ledger is at its size cap")
)

// rerunLedger is the persisted document. extra holds every top-level key this version
// does not know, written back unchanged.
type rerunLedger struct {
	Version          int                     `json:"version"`
	MinWriterVersion int                     `json:"minWriterVersion"`
	Reruns           map[string]*rerunRecord `json:"reruns,omitempty"`
	// Failures holds each failed scale-set job until a later attempt of it completes,
	// keyed by failureKey.
	Failures map[string]*failureRecord `json:"failures,omitempty"`
	extra    map[string]json.RawMessage
}

// rerunRecord is one run's budget, keyed by run ID: Count is how many times GAG has
// re-run the run, Attempts what each re-run covered, keyed by the run attempt it
// re-ran from.
type rerunRecord struct {
	ExpiresAt time.Time `json:"expiresAt"`
	Count     int       `json:"count"`
	// SelfCancelledAttempt is the run attempt GAG last force-cancelled itself, so a
	// cancelled conclusion on that attempt is read as GAG's own and not as a user's.
	SelfCancelledAttempt int                      `json:"selfCancelledAttempt,omitempty"`
	Attempts             map[string]*rerunAttempt `json:"attempts,omitempty"`
	extra                map[string]json.RawMessage
}

// rerunAttempt is one re-run GAG reserved.
type rerunAttempt struct {
	Scope string `json:"scope"`
	Cause string `json:"cause"`
	// Jobs and Disruptions are what the re-run covered: the jobs, by display name, and
	// the disruptions, by worker pod name, that reserved or merged into it.
	Jobs        []string `json:"jobs,omitempty"`
	Disruptions []string `json:"disruptions,omitempty"`
	extra       map[string]json.RawMessage
}

// The alias types carry the known fields without the custom marshalers, so the
// preserving helpers below can encode and decode them with encoding/json.
type (
	rerunLedgerFields   rerunLedger
	rerunRecordFields   rerunRecord
	rerunAttemptFields  rerunAttempt
	failureRecordFields failureRecord
)

// The JSON names each type declares, which the preserving helpers keep out of extra.
var (
	rerunLedgerNames   = []string{"version", "minWriterVersion", "reruns", "failures"}
	rerunRecordNames   = []string{"expiresAt", "count", "selfCancelledAttempt", "attempts"}
	rerunAttemptNames  = []string{"scope", "cause", "jobs", "disruptions"}
	failureRecordNames = []string{"expiresAt", "failedAt", "jobID", "cause", "event", "workflowRef",
		"pairedAttempts", "context"}
)

func (l *rerunLedger) UnmarshalJSON(b []byte) error {
	return unmarshalPreserving(b, (*rerunLedgerFields)(l), rerunLedgerNames, &l.extra)
}

func (l rerunLedger) MarshalJSON() ([]byte, error) {
	return marshalPreserving(rerunLedgerFields(l), l.extra)
}

func (r *rerunRecord) UnmarshalJSON(b []byte) error {
	return unmarshalPreserving(b, (*rerunRecordFields)(r), rerunRecordNames, &r.extra)
}

func (r rerunRecord) MarshalJSON() ([]byte, error) {
	return marshalPreserving(rerunRecordFields(r), r.extra)
}

func (a *rerunAttempt) UnmarshalJSON(b []byte) error {
	return unmarshalPreserving(b, (*rerunAttemptFields)(a), rerunAttemptNames, &a.extra)
}

func (a rerunAttempt) MarshalJSON() ([]byte, error) {
	return marshalPreserving(rerunAttemptFields(a), a.extra)
}

func (f *failureRecord) UnmarshalJSON(b []byte) error {
	return unmarshalPreserving(b, (*failureRecordFields)(f), failureRecordNames, &f.extra)
}

func (f failureRecord) MarshalJSON() ([]byte, error) {
	return marshalPreserving(failureRecordFields(f), f.extra)
}

// unmarshalPreserving decodes b into known and stores every key not in names in extra.
// Go's decoder drops unknown fields, so without this an older AGC's compare-and-swap
// would erase whatever a newer one added.
func unmarshalPreserving(b []byte, known any, names []string, extra *map[string]json.RawMessage) error {
	if err := json.Unmarshal(b, known); err != nil {
		return err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(b, &all); err != nil {
		return err
	}
	for _, name := range names {
		delete(all, name)
	}
	if len(all) > 0 {
		*extra = all
	}
	return nil
}

// marshalPreserving encodes known and writes extra's keys back beside it. A known
// field wins a name clash, so a value this version writes is never shadowed by a stale
// copy it carried.
func marshalPreserving(known any, extra map[string]json.RawMessage) ([]byte, error) {
	b, err := json.Marshal(known)
	if err != nil || len(extra) == 0 {
		return b, err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(b, &all); err != nil {
		return nil, err
	}
	for k, v := range extra {
		if _, ok := all[k]; !ok {
			all[k] = v
		}
	}
	return json.Marshal(all)
}

// scaleSetRerunLedgerConfigMapName derives the name of an owner's failure-ledger
// ConfigMap, budgeted against the object-name ceiling like its siblings.
func scaleSetRerunLedgerConfigMapName(ownerName string) string {
	return apinames.Join(apinames.MaxObjectName, "scaleset-failures", ownerName)
}

// decodeRerunLedger parses the persisted ledger, returning an initialised empty one for
// an absent or empty value. Unparseable data is an error: a ledger nobody can read
// bounds nothing, and reading it as empty would refill every budget.
func decodeRerunLedger(raw string) (*rerunLedger, error) {
	l := &rerunLedger{}
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), l); err != nil {
			return nil, err
		}
	}
	if l.Reruns == nil {
		l.Reruns = map[string]*rerunRecord{}
	}
	if l.Failures == nil {
		l.Failures = map[string]*failureRecord{}
	}
	return l, nil
}

// pruneRerunLedger drops every entry whose expiresAt has passed, in the maps this
// version knows and in any it does not: every entry of every map carries its own
// expiresAt so that any version can age any map.
func pruneRerunLedger(l *rerunLedger, now time.Time) {
	for runID, r := range l.Reruns {
		if r == nil || r.ExpiresAt.Before(now) {
			delete(l.Reruns, runID)
		}
	}
	for key, f := range l.Failures {
		if f == nil || f.ExpiresAt.Before(now) {
			delete(l.Failures, key)
		}
	}
	for name, raw := range l.extra {
		l.extra[name] = pruneUnknownMap(raw, now)
	}
}

// pruneUnknownMap ages a map this version does not understand. A value that is not an
// object of objects is returned unchanged, and so is an entry with no readable
// expiresAt: dropping what cannot be dated could erase a newer version's state.
func pruneUnknownMap(raw json.RawMessage, now time.Time) json.RawMessage {
	var entries map[string]json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return raw
	}
	pruned := false
	for k, v := range entries {
		var dated struct {
			ExpiresAt *time.Time `json:"expiresAt"`
		}
		if json.Unmarshal(v, &dated) != nil || dated.ExpiresAt == nil {
			continue
		}
		if dated.ExpiresAt.Before(now) {
			delete(entries, k)
			pruned = true
		}
	}
	if !pruned {
		return raw
	}
	b, err := json.Marshal(entries)
	if err != nil {
		return raw
	}
	return b
}

// rerunDisruption names one disruption to the scale-set budget.
type rerunDisruption struct {
	// key is stable across a restart and across replicas: the worker pod's name.
	key string
	// job is the disrupted job's display name, "" where the pod did not record one.
	job string
	// failedAttempt is the run attempt the job failed in. Zero means unknown, and the
	// recovery reads the run to learn it before reserving.
	failedAttempt int
}

// Reservation outcomes, in the order reserveScaleSetRerun tests them.
type rerunReservation int

const (
	// rerunResumed: the disruption already holds a reservation, from before a restart
	// or on another replica, and re-uses it.
	rerunResumed rerunReservation = iota
	// rerunMerged: a re-run already reserved for the attempt the job failed in covers
	// it, so no slot is spent and no second re-run is made.
	rerunMerged
	// rerunRefused: the budget is spent.
	rerunRefused
	// rerunReserved: a slot was taken.
	rerunReserved
)

// reserveScaleSetRerun takes the run's budget decision for one disruption, under the
// ledger's compare-and-swap. It returns the outcome and, for a resumed or reserved one,
// the 1-based number of GAG's re-run of the run it belongs to. A non-nil error means no
// decision was recorded, and the caller withholds the re-run.
func (p *Provisioner) reserveScaleSetRerun(ctx context.Context, target Target, runID string, d rerunDisruption, cause string, maxRetries int) (rerunReservation, int, error) {
	var (
		outcome rerunReservation
		number  int
	)
	err := p.updateRerunLedger(ctx, target, func(l *rerunLedger, now time.Time) bool {
		r := l.Reruns[runID]
		if r == nil {
			r = &rerunRecord{}
			l.Reruns[runID] = r
		}
		if r.Attempts == nil {
			r.Attempts = map[string]*rerunAttempt{}
		}
		attemptKey := strconv.Itoa(d.failedAttempt)

		for _, a := range r.Attempts {
			if a != nil && slices.Contains(a.Disruptions, d.key) {
				outcome, number = rerunResumed, r.Count
				return false
			}
		}
		if a := r.Attempts[attemptKey]; a != nil &&
			(a.Scope == rerunScopeFailedJobs || (d.job != "" && slices.Contains(a.Jobs, d.job))) {
			outcome = rerunMerged
			a.Disruptions = append(a.Disruptions, d.key)
			if d.job != "" && !slices.Contains(a.Jobs, d.job) {
				a.Jobs = append(a.Jobs, d.job)
			}
			r.ExpiresAt = now.Add(rerunRecordTTL)
			return true
		}
		if r.Count >= maxRetries {
			outcome = rerunRefused
			return false
		}
		r.Count++
		a := &rerunAttempt{Scope: rerunScopeFailedJobs, Cause: cause, Disruptions: []string{d.key}}
		if d.job != "" {
			a.Jobs = []string{d.job}
		}
		r.Attempts[attemptKey] = a
		r.ExpiresAt = now.Add(rerunRecordTTL)
		outcome, number = rerunReserved, r.Count
		return true
	})
	return outcome, number, err
}

// recordSelfCancel records that GAG is about to force-cancel attempt of runID, so the
// cancelled conclusion it produces is not read as a user's cancel (Q1182, replacing an
// exemption for the abandoned arm).
func (p *Provisioner) recordSelfCancel(ctx context.Context, target Target, runID string, attempt int) error {
	return p.updateRerunLedger(ctx, target, func(l *rerunLedger, now time.Time) bool {
		r := l.Reruns[runID]
		if r == nil {
			r = &rerunRecord{}
			l.Reruns[runID] = r
		}
		r.SelfCancelledAttempt = attempt
		r.ExpiresAt = now.Add(rerunRecordTTL)
		return true
	})
}

// selfCancelledAttempt returns the run attempt GAG last force-cancelled for runID, or 0
// where it recorded none or the ledger cannot be read. Zero reads every cancel as a
// user's, which withholds the re-run: the safe direction.
func (p *Provisioner) selfCancelledAttempt(ctx context.Context, target Target, runID string) int {
	l, _, err := p.readRerunLedger(ctx, target)
	if err != nil {
		return 0
	}
	if r := l.Reruns[runID]; r != nil && !r.ExpiresAt.Before(p.nowFn()) {
		return r.SelfCancelledAttempt
	}
	return 0
}

// readRerunLedger reads target's ledger through the uncached reader: the AGC runs no
// ConfigMap informer, and a cached copy would predate a rival replica's write. A
// missing ConfigMap reads as an empty ledger and an empty object to create.
func (p *Provisioner) readRerunLedger(ctx context.Context, target Target) (*rerunLedger, *corev1.ConfigMap, error) {
	key := target.Key()
	cmKey := types.NamespacedName{Namespace: key.Namespace, Name: scaleSetRerunLedgerConfigMapName(key.Name)}
	reader := client.Reader(p.Client)
	if p.APIReader != nil {
		reader = p.APIReader
	}
	var cm corev1.ConfigMap
	err := reader.Get(ctx, cmKey, &cm)
	switch {
	case apierrors.IsNotFound(err):
		cm = corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
			Namespace:       cmKey.Namespace,
			Name:            cmKey.Name,
			Labels:          target.PodOwnerLabels(),
			OwnerReferences: []metav1.OwnerReference{target.OwnerRef()},
		}}
	case err != nil:
		return nil, nil, fmt.Errorf("provisioner: get failure-ledger ConfigMap %s: %w", cmKey.Name, err)
	}
	l, err := decodeRerunLedger(cm.Data[rerunLedgerDataKey])
	if err != nil {
		return nil, nil, fmt.Errorf("provisioner: failure-ledger ConfigMap %s holds unparseable state "+
			"(deleting it resets every run's re-run budget): %w", cmKey.Name, err)
	}
	p.failureIndex.note(key, l)
	return l, &cm, nil
}

// updateRerunLedger applies mutate to target's ledger under a compare-and-swap,
// re-reading and re-applying on a conflict. mutate reports whether it changed anything;
// one that did not costs no write. The ledger is pruned on every write, the only
// cadence available, and a write is refused rather than made when this AGC may not
// write the ledger or the result would pass the size cap.
func (p *Provisioner) updateRerunLedger(ctx context.Context, target Target, mutate func(l *rerunLedger, now time.Time) bool) error {
	if p.Client == nil {
		return errors.New("provisioner: no Kubernetes client to write the failure ledger with")
	}
	p.ledgerMu.Lock()
	defer p.ledgerMu.Unlock()

	// A conflict means another replica's write landed, so the loop retries rather than
	// counting: a replica writing a drain's worth of reservations back to back wins one
	// round per write, which outlasted a fixed bound of five with two replicas in about
	// one run in 20. Each round needs a rival write, so the loop ends when those stop;
	// ctx ends it sooner where the caller set a deadline, as the watch path's
	// disruptedWorkerRecoveryBudget does and the reconcile scan does not.
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("provisioner: write failure ledger: %w", err)
		}
		l, cm, err := p.readRerunLedger(ctx, target)
		if err != nil {
			return err
		}
		now := p.nowFn().UTC()
		pruneRerunLedger(l, now)
		if !mutate(l, now) {
			return nil
		}
		if l.MinWriterVersion > rerunLedgerVersion {
			return fmt.Errorf("provisioner: failure-ledger ConfigMap %s requires writer version %d, this AGC writes %d: %w",
				cm.Name, l.MinWriterVersion, rerunLedgerVersion, errRerunLedgerReadOnly)
		}
		l.Version = max(l.Version, rerunLedgerVersion)
		l.MinWriterVersion = max(l.MinWriterVersion, rerunLedgerMinWriterVersion)

		data, err := json.Marshal(l)
		for err == nil && len(data) > rerunLedgerMaxBytes && evictOldestFailure(l) {
			data, err = json.Marshal(l)
		}
		if err != nil {
			return fmt.Errorf("provisioner: encode failure ledger %s: %w", cm.Name, err)
		}
		if len(data) > rerunLedgerMaxBytes {
			return fmt.Errorf("provisioner: failure-ledger ConfigMap %s would hold %d bytes, past its %d-byte cap: %w",
				cm.Name, len(data), rerunLedgerMaxBytes, errRerunLedgerFull)
		}
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[rerunLedgerDataKey] = string(data)

		if cm.ResourceVersion == "" {
			err = p.Client.Create(ctx, cm)
		} else {
			// Update carries the read's resourceVersion, so a rival write in between
			// fails this one and is re-read below.
			err = p.Client.Update(ctx, cm)
		}
		switch {
		case err == nil:
			p.failureIndex.note(target.Key(), l)
			return nil
		case apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err):
		default:
			return fmt.Errorf("provisioner: write failure ledger %s: %w", cm.Name, err)
		}
	}
}
