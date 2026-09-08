package v2alpha1

import (
	"context"
	"fmt"

	agcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/agentidentity"
)

// validateAgentIdentityUniqueness rejects a RunnerSet whose agent-identity stem is
// already claimed — by a v1alpha1 RunnerGroup named "rs-<this set>" in the same
// namespace, or by any owner deriving the same stem under the same GitHub scope
// (Q1011). The agentidentity package doc carries why the two halves have different
// boundaries and why the reverse direction is guarded on the v1 gateway instead.
//
// The check is fail-closed, like the Q791 label guard: admitting an unverifiable
// claim is the collision this prevents. An admitted collision costs one of the two
// tenants its entire agent pool and does not self-heal — the AGC reports
// `agent identity is owned by another pool` on every reconcile until an operator
// renames one of the CRs (Q979).
func (v *RunnerSetCustomValidator) validateAgentIdentityUniqueness(ctx context.Context, rs *agcv2alpha1.RunnerSet) error {
	if v.reader == nil {
		// No reader wired (direct-construction unit-test path); the integration/e2e
		// and production paths always wire the uncached API reader.
		return nil
	}
	inv, err := agentidentity.Of(ctx, v.reader, nil)
	if err != nil {
		return fmt.Errorf(
			"cannot verify agent-identity uniqueness for RunnerSet %q in namespace %q: %w",
			rs.Name, rs.Namespace, err)
	}
	self := agentidentity.RunnerSetClaim(rs, inv.V2Scope(rs.Namespace, rs.Spec.GatewayRef.Name))
	if holder := inv.Collision(self); holder != nil {
		return agentIdentityConflictError(self, *holder)
	}
	return nil
}

// agentIdentityConflictError renders a stem collision for whoever is applying self. A
// holder in the applying object's own namespace is named — the tenant owns both
// objects and has to rename one. A holder in another namespace is not: both CRs are
// tenant-authored surfaces, so naming it would disclose another tenant's namespace and
// object. logRejection writes the full detail to the GMC log either way, so the
// platform admin keeps what the tenant is not told. Mirrors scaleSetConflictError.
func agentIdentityConflictError(self, holder agentidentity.Claim) error {
	const why = "an agent pool derives its agent Secret \"agentpool-<stem>-N\" and the runner name it " +
		"registers with GitHub \"<stem>-N\" from that stem, so two owners sharing it each deregister the " +
		"other's runner and neither recovers"
	if holder.Namespace == self.Namespace {
		return fmt.Errorf(
			"agent-identity stem %q is already claimed by %s %q in namespace %q; %s — rename one of the two CRs",
			self.Stem, holder.Kind, holder.Name, holder.Namespace, why)
	}
	return fmt.Errorf(
		"agent-identity stem %q is already claimed by another runner pool registered against GitHub scope %q; %s "+
			"— pick a distinct name for this %s (ask your platform administrator which runner names that GitHub scope already holds)",
		self.Stem, self.Scope, why, self.Kind)
}
