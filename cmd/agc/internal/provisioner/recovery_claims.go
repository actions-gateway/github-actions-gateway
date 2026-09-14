package provisioner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/actions-gateway/github-actions-gateway/api/apinames"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The durable recovery claim on the scale-set tier (Q1108).
//
// # Why the claim left the pod
//
// Every recovery arm reads its evidence off a pod, and two of the causes DELETE their
// victim. The evidence — the cause and the run identity — is in hand by the time
// recovery acts on it, read from the informer; the at-most-once claim was not, because
// it was written back onto that same pod. So a pod removed in between took the lock
// with it and its run was reported unrecoverable (Q809 measured about two seconds on a
// drain). The evidence stays the pod's; the claim lives here instead, where the
// deletion does not reach it.
//
// The alternatives, and why a separate object rather than a field in the listener's
// guard ConfigMap (Q606): docs/design/04-operational-flows.md §4.2, "Detecting a
// disruption is not the same as claiming it". The short form is that guards.json is
// re-serialised whole from the listener's memory on every save, so nothing another
// writer puts inside it survives.

// recoveryClaimDataKey is the one data key in a recovery-claim ConfigMap: the
// JSON-encoded recoveryClaimLedger.
const recoveryClaimDataKey = "recovery-claims.json"

// recoveryClaimTTL bounds how long a claim is kept. It has to outlive every rival
// claimant for the same pod: a stale informer copy (seconds), the watch handler's own
// disruptedWorkerRecoveryBudget, and the once-per-process orphaned-worker scan a
// restart runs on its first reconcile. An hour clears all three with room, and is far
// short of the in-flight record's own 24-hour bound, so the ledger does not become a
// second history of the set's jobs.
const recoveryClaimTTL = time.Hour

// maxRecoveryClaims caps the ledger independently of the TTL, so a RunnerSet under
// sustained preemption cannot grow the object toward the 1 MiB ConfigMap ceiling. The
// oldest claims are dropped first; dropping one can cost at-most-once for that pod,
// which is a strictly better failure than the manual re-run this whole mechanism
// replaces.
const maxRecoveryClaims = 256

// recoveryClaimConflictRetries bounds the re-read retries a claim makes against a
// conflicting write. Nothing but another claim writes this object, and claimMu leaves
// only the other AGC replicas contending, so the bound is the replica count with
// headroom rather than the number of workers a drain disrupts at once.
const recoveryClaimConflictRetries = 5

// errRecoveryClaimHeld reports that the pod's recovery is already someone else's — the
// other detection path, another reconcile, or another replica. The mechanism working,
// not an error.
var errRecoveryClaimHeld = errors.New("provisioner: the disruption's recovery is already claimed")

// recoveryClaim is one disrupted worker's claim. Cause is recorded for the operator
// reading the ConfigMap, never read back by the claim itself.
type recoveryClaim struct {
	ClaimedAt time.Time `json:"claimedAt"`
	Cause     string    `json:"cause"`
}

// recoveryClaimLedger is the persisted set, keyed by worker pod name — the one
// identifier both detection paths and the orphaned-worker scan derive the same way
// (scaleSetPodName), and the one that outlives the pod itself.
type recoveryClaimLedger struct {
	Claims map[string]recoveryClaim `json:"claims,omitempty"`
}

// scaleSetRecoveryClaimsConfigMapName derives the name of the ConfigMap holding an
// owner's recovery claims. Budgeted against the object-name ceiling, like the guard
// ConfigMap it sits beside; the name is never a label value.
func scaleSetRecoveryClaimsConfigMapName(ownerName string) string {
	return apinames.Join(apinames.MaxObjectName, "scaleset-recovery-claims", ownerName)
}

// claimDisruptionRecovery enters podName in target's recovery-claim ledger under an
// optimistic lock, so exactly one caller ever proceeds to re-run the run behind that
// pod. It returns errRecoveryClaimHeld when the claim is already there, and any other
// error when no claim could be recorded at all — which is the one remaining way a
// detected disruption goes unrecovered.
//
// The ConfigMap is created on first use and owner-ref'd to target, so the garbage
// collector reaps it with the owner. Reads go through the uncached reader for the same
// reason the guard store's do: the AGC runs no ConfigMap informer, and a cached read
// would answer "unclaimed" from a copy that predates the rival's write.
func (p *Provisioner) claimDisruptionRecovery(ctx context.Context, target Target, podName, cause string) error {
	// Serialise this process's own claims, so the compare-and-swap below arbitrates
	// between REPLICAS rather than between the goroutines of one AGC. A node drain
	// disrupts every worker it holds at once and both detection paths see each of them,
	// so the uncontended case is the one worth engineering for: one mutex for all
	// owners is enough because a claim happens only on a disruption, and the work it
	// serialises — one round trip — is what the per-pod claim it replaced cost anyway.
	p.claimMu.Lock()
	defer p.claimMu.Unlock()

	key := target.Key()
	cmKey := types.NamespacedName{Namespace: key.Namespace, Name: scaleSetRecoveryClaimsConfigMapName(key.Name)}
	reader := client.Reader(p.Client)
	if p.APIReader != nil {
		reader = p.APIReader
	}

	for attempt := 0; ; attempt++ {
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
			return fmt.Errorf("provisioner: get recovery-claim ConfigMap %s: %w", cmKey.Name, err)
		}

		ledger, err := decodeRecoveryClaims(cm.Data[recoveryClaimDataKey])
		if err != nil {
			// A ledger nobody can parse arbitrates nothing, and treating it as empty
			// would silently re-run every disruption twice. Refusing leaves the run
			// reported rather than doubly re-run, and names the object to delete.
			return fmt.Errorf("provisioner: recovery-claim ConfigMap %s holds unparseable state "+
				"(delete the ConfigMap to reset it): %w", cmKey.Name, err)
		}
		if _, held := ledger.Claims[podName]; held {
			return errRecoveryClaimHeld
		}
		ledger.Claims[podName] = recoveryClaim{ClaimedAt: p.nowFn().UTC(), Cause: cause}
		pruneRecoveryClaims(ledger, p.nowFn().UTC())

		data, err := json.Marshal(ledger)
		if err != nil {
			return fmt.Errorf("provisioner: encode recovery claims for %s: %w", cmKey.Name, err)
		}
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[recoveryClaimDataKey] = string(data)

		if cm.ResourceVersion == "" {
			err = p.Client.Create(ctx, &cm)
		} else {
			// Update carries the resourceVersion the read returned, so the apiserver
			// compares and swaps: a rival that claimed in between loses this write and
			// wins the re-read below.
			err = p.Client.Update(ctx, &cm)
		}
		switch {
		case err == nil:
			return nil
		case apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err):
			if attempt == recoveryClaimConflictRetries {
				return fmt.Errorf("provisioner: claim recovery of %s: %w", podName, err)
			}
		default:
			return fmt.Errorf("provisioner: claim recovery of %s: %w", podName, err)
		}
	}
}

// recoveryAlreadyClaimed reports whether podName's recovery has already been claimed —
// the read half of the ledger, for a caller that has no evidence of its own to act on.
// An unreadable ledger answers false: the orphaned-worker scan is the only caller, and
// leaving a lost worker un-re-run is the worse of the two failures there.
func (p *Provisioner) recoveryAlreadyClaimed(ctx context.Context, target Target, podNames map[string]bool) map[string]bool {
	key := target.Key()
	reader := client.Reader(p.Client)
	if p.APIReader != nil {
		reader = p.APIReader
	}
	var cm corev1.ConfigMap
	cmKey := types.NamespacedName{Namespace: key.Namespace, Name: scaleSetRecoveryClaimsConfigMapName(key.Name)}
	if err := reader.Get(ctx, cmKey, &cm); err != nil {
		return nil
	}
	ledger, err := decodeRecoveryClaims(cm.Data[recoveryClaimDataKey])
	if err != nil {
		return nil
	}
	claimed := make(map[string]bool, len(podNames))
	for name := range podNames {
		if _, held := ledger.Claims[name]; held {
			claimed[name] = true
		}
	}
	return claimed
}

// decodeRecoveryClaims parses the persisted ledger, returning an initialised empty one
// for an absent or empty value. Unparseable data is an error rather than an empty read
// — see the caller for why.
func decodeRecoveryClaims(raw string) (*recoveryClaimLedger, error) {
	ledger := &recoveryClaimLedger{Claims: map[string]recoveryClaim{}}
	if raw == "" {
		return ledger, nil
	}
	if err := json.Unmarshal([]byte(raw), ledger); err != nil {
		return nil, err
	}
	if ledger.Claims == nil {
		ledger.Claims = map[string]recoveryClaim{}
	}
	return ledger, nil
}

// pruneRecoveryClaims drops claims past recoveryClaimTTL and then, if the set is still
// over maxRecoveryClaims, the oldest of what is left. Called on every write, which is
// the only cadence available: nothing else reads this object often enough to sweep it,
// and a claim is entered exactly when a sweep is cheap.
func pruneRecoveryClaims(ledger *recoveryClaimLedger, now time.Time) {
	cutoff := now.Add(-recoveryClaimTTL)
	for name, c := range ledger.Claims {
		if c.ClaimedAt.Before(cutoff) {
			delete(ledger.Claims, name)
		}
	}
	if len(ledger.Claims) <= maxRecoveryClaims {
		return
	}
	names := make([]string, 0, len(ledger.Claims))
	for name := range ledger.Claims {
		names = append(names, name)
	}
	// Oldest first, with the name breaking a tie so the prune is deterministic.
	sort.Slice(names, func(i, j int) bool {
		a, b := ledger.Claims[names[i]].ClaimedAt, ledger.Claims[names[j]].ClaimedAt
		if a.Equal(b) {
			return names[i] < names[j]
		}
		return a.Before(b)
	})
	for _, name := range names[:len(ledger.Claims)-maxRecoveryClaims] {
		delete(ledger.Claims, name)
	}
}
