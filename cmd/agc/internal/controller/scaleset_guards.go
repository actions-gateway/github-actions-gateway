package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/scalesetlistener"
	"github.com/actions-gateway/github-actions-gateway/api/apinames"
	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// scaleSetGuardDataKey is the one data key in a guard ConfigMap: the JSON-encoded
// scalesetlistener.GuardState.
const scaleSetGuardDataKey = "guards.json"

// scaleSetGuardConfigMapName derives the name of the ConfigMap persisting a scale-set
// listener's concluded-job guards (Q606). Budgeted against the object-name ceiling —
// the name is never a label value.
func scaleSetGuardConfigMapName(runnerSetName string) string {
	return apinames.Join(apinames.MaxObjectName, "scaleset-guards", runnerSetName)
}

// scaleSetGuardStore persists a scale-set listener's GuardState in a per-RunnerSet
// ConfigMap, owner-ref'd to the RunnerSet so it is garbage-collected with it. It backs
// the listener's Config.Guards seam (Q606): Load runs once per listener start, Save on
// each poll cycle that concluded or retired something — both from the poll goroutine,
// so there is a single writer per ConfigMap.
//
// Reads go through the uncached reader: the AGC runs no ConfigMap informer (the
// EventReader rationale), and a get-per-save is the same cost class as the message
// deletes the save gates.
type scaleSetGuardStore struct {
	writer client.Client
	reader client.Reader
	cmKey  types.NamespacedName
	owner  metav1.OwnerReference
	// labels mark the ConfigMap as this RunnerSet's, so an operator can find it the
	// same way they find the set's worker pods.
	labels map[string]string
}

// scaleSetGuardStore builds the store for one RunnerSet. Falls back to the cached
// client when no APIReader is wired (tests), like the RunnerGroup probe.
func (r *RunnerSetReconciler) scaleSetGuardStore(rs *v2alpha1.RunnerSet) *scaleSetGuardStore {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	return &scaleSetGuardStore{
		writer: r.Client,
		reader: reader,
		cmKey:  types.NamespacedName{Namespace: rs.Namespace, Name: scaleSetGuardConfigMapName(rs.Name)},
		owner:  runnerSetOwnerRef(rs.Name, rs.UID),
		labels: map[string]string{provisioner.LabelRunnerSet: rs.Name},
	}
}

// Load returns the persisted guard state; a ConfigMap that does not exist yet is the
// empty state. Corrupt data is an error rather than an empty read — treating it as
// empty would silently reopen the replay window, while failing keeps the listener down,
// visibly, until the ConfigMap is deleted or repaired.
func (s *scaleSetGuardStore) Load(ctx context.Context) (scalesetlistener.GuardState, error) {
	var state scalesetlistener.GuardState
	var cm corev1.ConfigMap
	if err := s.reader.Get(ctx, s.cmKey, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			return state, nil
		}
		return state, fmt.Errorf("get guard ConfigMap %s: %w", s.cmKey.Name, err)
	}
	raw := cm.Data[scaleSetGuardDataKey]
	if raw == "" {
		return state, nil
	}
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		return state, fmt.Errorf("guard ConfigMap %s holds unparseable state (delete the ConfigMap to reset it): %w",
			s.cmKey.Name, err)
	}
	return state, nil
}

// Save replaces the persisted guard state, creating the ConfigMap on first use. Any
// error is returned as-is: the listener holds the cycle's message deletes and retries
// on the next one, so a conflict or a stale read here heals itself.
func (s *scaleSetGuardStore) Save(ctx context.Context, state scalesetlistener.GuardState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	var cm corev1.ConfigMap
	err = s.reader.Get(ctx, s.cmKey, &cm)
	switch {
	case apierrors.IsNotFound(err):
		cm = corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:       s.cmKey.Namespace,
				Name:            s.cmKey.Name,
				Labels:          s.labels,
				OwnerReferences: []metav1.OwnerReference{s.owner},
			},
			Data: map[string]string{scaleSetGuardDataKey: string(data)},
		}
		return s.writer.Create(ctx, &cm)
	case err != nil:
		return fmt.Errorf("get guard ConfigMap %s: %w", s.cmKey.Name, err)
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data[scaleSetGuardDataKey] = string(data)
	return s.writer.Update(ctx, &cm)
}

// recoverOrphanedScaleSetWorkers re-runs the workflow runs whose worker pods were
// already gone when this process started (Q844). The stored in-flight set is the record
// a preempted or drained worker cannot leave behind itself; the provisioner decides
// which entries lost their pod and runs the scan once per process.
//
// Read-only on the ConfigMap, deliberately: the listener's poll goroutine stays its
// single writer, which is what lets Save replace the whole state rather than merge it.
// Nothing here requeues — a set with no stored state is the overwhelmingly common case,
// and a read that fails is retried by the next reconcile.
//
// The returned channel closes once every recovery this call started has finished. The
// reconcile ignores it, because it must not stall on GitHub; tests block on it.
func (r *RunnerSetReconciler) recoverOrphanedScaleSetWorkers(ctx context.Context, log *slog.Logger, rs *v2alpha1.RunnerSet) <-chan struct{} {
	state, err := r.scaleSetGuardStore(rs).Load(ctx)
	if err != nil {
		log.Warn("could not read persisted in-flight jobs; workers lost while this AGC was down will not be recovered", "error", err)
		return closedChan()
	}
	if len(state.InFlight) == 0 {
		return closedChan()
	}
	orphans := make([]provisioner.OrphanedWorker, 0, len(state.InFlight))
	for _, j := range state.InFlight {
		orphans = append(orphans, provisioner.OrphanedWorker{
			JobID:      j.JobID,
			Owner:      j.Owner,
			Repository: j.Repository,
			RunID:      j.RunID,
		})
	}
	done, err := r.Provisioner.RecoverOrphanedScaleSetWorkers(ctx, r.provisionerTarget(rs), orphans)
	if err != nil {
		log.Warn("orphaned scale-set worker recovery scan failed", "error", err)
	}
	return done
}

// closedChan returns an already-closed done channel, for the exits that start no
// recovery, so a caller can always receive without a nil check.
func closedChan() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}
