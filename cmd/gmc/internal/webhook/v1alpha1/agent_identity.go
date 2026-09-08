package v1alpha1

import (
	"context"
	"fmt"

	"github.com/actions-gateway/github-actions-gateway/api/apinames"
	gmcv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/api/v1alpha1"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/agentidentity"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/scalesetscope"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The guard lists both gateway versions, RunnerGroups, and RunnerSets to place every
// agent-identity claim in the cluster (Q1011). Every one of those reads is already in
// the GMC ClusterRole for the reconciler and the v2 webhooks; this marker records the
// dependency rather than widening the role.
// +kubebuilder:rbac:groups=actions-gateway.com,resources=runnersets;actionsgateways,verbs=get;list;watch

// validateAgentIdentityUniqueness rejects an ActionsGateway whose spec.runnerGroups
// would materialize a RunnerGroup claiming an agent-identity stem another pool
// already holds (Q1011).
//
// This is the v1 half of the guard, and it lives on the gateway rather than on a
// RunnerGroup validator because RunnerGroups are not authored: an entry carries no
// name, and the GMC derives the CR name from the gateway
// ([apinames.RunnerGroupName]). The gateway write is therefore the only moment an
// operator can still act, and v1 gains no new admission surface.
//
// The collision it exists for is the one Q466's discriminator cannot prevent: that
// prefix is not injective, so a RunnerGroup named "rs-<x>" claims exactly the stem a
// RunnerSet named "<x>" claims. Q979 stopped the loser deregistering the incumbent's
// GitHub record; nothing stopped the collision being accepted, and it costs one
// tenant its whole agent pool with no path back but renaming a CR.
//
// Rejecting here can block a rollback that re-applies a v1 gateway while the
// colliding v2 RunnerSet still exists. That is deliberate: the rollback has to delete
// that RunnerSet regardless, and the rejection names it, where admitting the write
// leaves the operator with a tenant that runs no jobs and a GitHub error that names
// neither CR.
//
// Fail-closed, like the guards beside it: a List error rejects rather than admitting
// an unverifiable claim.
func (v *ActionsGatewayCustomValidator) validateAgentIdentityUniqueness(ctx context.Context, ag *gmcv1alpha1.ActionsGateway) error {
	if v.reader == nil {
		// No reader wired (direct-construction unit-test path); the integration/e2e
		// and production paths always wire the uncached API reader.
		return nil
	}
	if len(ag.Spec.RunnerGroups) == 0 {
		return nil
	}
	pending := &agentidentity.PendingGateway{
		Key:   client.ObjectKey{Namespace: ag.Namespace, Name: ag.Name},
		Scope: scalesetscope.GitHubScope(ag.Spec.GitHubURL),
	}
	inv, err := agentidentity.Of(ctx, v.reader, pending)
	if err != nil {
		return fmt.Errorf(
			"cannot verify agent-identity uniqueness for ActionsGateway %q in namespace %q: %w",
			ag.Name, ag.Namespace, err)
	}
	for i, spec := range ag.Spec.RunnerGroups {
		name := apinames.RunnerGroupName(ag.Name, spec.RunnerLabels, i)
		self := agentidentity.RunnerGroupClaim(ag.Namespace, name, pending.Scope)
		if holder := inv.Collision(self); holder != nil {
			return agentIdentityConflictError(i, name, self, *holder)
		}
	}
	return nil
}

// agentIdentityConflictError renders a stem collision for the operator applying the
// gateway. It names the spec index and the derived RunnerGroup name, because neither
// is written in the spec — the entry has no name field, so an operator handed only
// the stem cannot tell which runnerGroups[] entry to change. A holder in another
// namespace is not named: that would disclose another tenant's namespace and object
// to anyone able to write a gateway in their own. logRejection keeps the full detail
// in the GMC log either way.
func agentIdentityConflictError(index int, name string, self, holder agentidentity.Claim) error {
	const why = "an agent pool derives its agent Secret \"agentpool-<stem>-N\" and the runner name it " +
		"registers with GitHub \"<stem>-N\" from that stem, so two owners sharing it each deregister the " +
		"other's runner and neither recovers"
	where := fmt.Sprintf(
		"spec.runnerGroups[%d] derives RunnerGroup %q, whose agent-identity stem %q",
		index, name, self.Stem)
	if holder.Namespace == self.Namespace {
		return fmt.Errorf(
			"%s is already claimed by %s %q in the same namespace; %s — rename this gateway, "+
				"change the entry's first runnerLabel, or remove the colliding CR",
			where, holder.Kind, holder.Name, why)
	}
	return fmt.Errorf(
		"%s is already claimed by another runner pool registered against GitHub scope %q; %s "+
			"— rename this gateway or change the entry's first runnerLabel "+
			"(ask your platform administrator which runner names that GitHub scope already holds)",
		where, self.Scope, why)
}
