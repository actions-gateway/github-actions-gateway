package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"

	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/scalesetscope"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// scaleSetCollisionState is one gateway's ScaleSetNameCollision verdict: whether any
// ScaleSet RunnerSet bound to it shares a scale-set name in its GitHub scope, and the
// tenant-facing reason/message. observed is false when the inventory could not be
// read, which is neither verdict — see evalScaleSetNameCollisions.
type scaleSetCollisionState struct {
	observed bool
	collided bool
	reason   string
	message  string
}

// evalScaleSetNameCollisions reports a scale-set name this gateway's RunnerSets share
// with another ScaleSet RunnerSet in the same GitHub scope (Q849).
//
// Admission already rejects a write that would create such a pair from either side
// (Q791), so nothing here can arrive through a validated write. What it catches is the
// pair admission never saw: one that predates the guard — the shape an upgrade from a
// release before Q791 carries in, since a webhook fires on write and no write follows
// — or one applied during a window with the validating webhook uninstalled. Both AGCs
// drive one scale set and each acquires the other tenant's jobs until someone
// re-applies an object, which is a security boundary failing silently.
//
// It is a report, not an enforcement: the condition does not gate Ready and
// provisioning proceeds. GAG cannot pick which of two tenants loses the name, and
// refusing to run the AGC would take down both tenants rather than the one that is
// misconfigured — so the operator gets the signal and makes the call. §5.2 of the
// security design carries that reasoning.
//
// It runs from updateStatus, so a gateway held at Ready=False before provisioning
// (CredentialUnavailable, ProxyNotFound) does not re-evaluate and keeps whatever
// verdict it last recorded. That is the same "last verdict stands" the read-failure
// path takes, and for the same reason: nothing here has measured the scope.
//
// The read is the reconciler's cached, cluster-wide client: it already watches both
// kinds, and this reports a persisted state rather than arbitrating a write, so the
// admission path's uncached-read race does not apply. A read failure yields
// observed=false — the caller leaves the last verdict standing rather than reporting a
// clean scope it did not measure, the one direction that would be a lie about a
// security boundary.
func (r *ActionsGatewayV2Reconciler) evalScaleSetNameCollisions(ctx context.Context, ag *gmcv2alpha1.ActionsGateway) scaleSetCollisionState {
	inv, err := scalesetscope.Of(ctx, r.Client, nil)
	if err != nil {
		logf.FromContext(ctx).Error(err, "could not read the cluster's ScaleSet claims; leaving the last ScaleSetNameCollision verdict in place")
		return scaleSetCollisionState{}
	}

	scope := scalesetscope.GitHubScope(ag.Spec.GitHubURL)
	// own maps each of this gateway's colliding RunnerSets to the scale-set name it
	// shares; holders records the other side for the GMC log.
	own := map[string]string{}
	var holders []string
	for i, mine := range inv.Claims {
		if mine.Namespace != ag.Namespace || mine.GatewayRef != ag.Name {
			continue
		}
		for j, other := range inv.Claims {
			if i == j || !mine.CollidesWith(other) {
				continue
			}
			own[mine.Name] = mine.Label
			holders = append(holders, fmt.Sprintf("%q claims %q in namespace %q", other.Name, other.Label, other.Namespace))
		}
	}
	if len(own) == 0 {
		return scaleSetCollisionState{
			observed: true,
			reason:   gmcv2alpha1.ReasonScaleSetNamesUnique,
			message:  scaleSetNamesUniqueMessage(scope),
		}
	}

	// The full pair — including a holder in another tenant's namespace, which the
	// condition and the Event withhold — goes to the GMC log, which is the platform
	// admin's. Written on every reconcile that observes the collision rather than only
	// on the transition into it: the condition persists across a GMC restart while a
	// transition-only line does not, and this is a security boundary that is currently
	// failing, not an event that happened once.
	logf.FromContext(ctx).Info("ScaleSet name collision: RunnerSets bound to this gateway share a scale-set name with another RunnerSet in the same GitHub scope",
		"githubScope", scope, "colliding", ownDetail(own), "holders", dedupeSorted(holders))

	return scaleSetCollisionState{
		observed: true,
		collided: true,
		reason:   gmcv2alpha1.ReasonScaleSetNameShared,
		message:  scaleSetCollisionMessage(scope, own),
	}
}

// ownDetail renders this gateway's colliding sets as a stable, sorted list for the log.
func ownDetail(own map[string]string) []string {
	out := make([]string, 0, len(own))
	for set, label := range own {
		out = append(out, fmt.Sprintf("%s claims %s", set, label))
	}
	sort.Strings(out)
	return out
}

// scaleSetNamesUniqueMessage states what was checked, not just that nothing was found:
// a bare "no collisions" reads the same whether the scope held one gateway or thirty.
func scaleSetNamesUniqueMessage(scope string) string {
	if scope == "" {
		return "no ScaleSet RunnerSet bound to this gateway shares its scale-set name"
	}
	return fmt.Sprintf("no ScaleSet RunnerSet bound to this gateway shares its scale-set name in GitHub scope %q", scope)
}

// scaleSetCollisionMessage names this gateway's own RunnerSets and the names they
// share, and stops there. The holder on the other side is withheld for the reason the
// admission error withholds it (scaleset_scope.go): a RunnerSet is tenant-authored, so
// naming a cross-namespace holder in a status message anyone able to read this gateway
// can see would disclose another tenant's namespace and label usage. The full pair
// goes to the GMC log, which is the platform admin's.
func scaleSetCollisionMessage(scope string, own map[string]string) string {
	pairs := make([]string, 0, len(own))
	for set, label := range own {
		pairs = append(pairs, fmt.Sprintf("%q claims %q", set, label))
	}
	sort.Strings(pairs)
	where := "in this gateway's GitHub scope"
	if scope != "" {
		where = fmt.Sprintf("in GitHub scope %q", scope)
	}
	return fmt.Sprintf(
		"%d RunnerSet(s) bound to this gateway claim a scale-set name another RunnerSet already claims %s: %s. "+
			"A ScaleSet set's FIRST runnerLabel is its scale-set name at GitHub, so both AGCs drive one scale set and each "+
			"acquires the other's jobs. Give one side a distinct first runnerLabel; the GMC log names the other holder",
		len(own), where, strings.Join(pairs, ", "))
}

// dedupeSorted returns the distinct entries in sorted order, so the logged holder list
// is stable across reconciles and a set colliding on two counts is named once.
func dedupeSorted(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
