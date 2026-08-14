package v2alpha1

import (
	"fmt"

	"github.com/actions-gateway/github-actions-gateway/gmc/internal/scalesetscope"
)

// This file holds the admission half of the ScaleSet label-uniqueness guard (Q791).
// The scope key, the claim, and the cluster-wide inventory live in scalesetscope,
// which the reconciler also reads to report a pair admission never saw (Q849); that
// package's doc comment carries why the scale-set name is scoped to the GitHub
// binding rather than the namespace, and why both admission paths consult the other
// side of the pair.
//
// List errors fail closed here, since admitting an unverifiable claim is the
// collision this prevents.

// scaleSetConflictError renders a claim collision for the tenant applying self. A
// holder in the applying object's own namespace is named — the tenant owns both
// objects. A holder in another namespace is not: the RunnerSet is tenant-authored, so
// naming it would disclose another tenant's namespace and object to anyone able to
// create a RunnerSet in their own. logRejection writes the full detail to the GMC log
// either way, so the platform admin keeps what the tenant is not told.
func scaleSetConflictError(self, holder scalesetscope.Claim) error {
	where := fmt.Sprintf("under gateway %q in namespace %q", self.GatewayRef, self.Namespace)
	if self.Scope != "" {
		where = fmt.Sprintf("against GitHub scope %q", self.Scope)
	}
	if holder.Namespace == self.Namespace {
		return fmt.Errorf(
			"ScaleSet runnerLabels[0] %q is already used by RunnerSet %q registered %s; "+
				"a ScaleSet set's FIRST runnerLabel is its scale-set name at GitHub, so two sets sharing it "+
				"would drive one scale set — pick a distinct first label (later labels may overlap freely)",
			self.Label, holder.Name, where)
	}
	return fmt.Errorf(
		"ScaleSet runnerLabels[0] %q is already claimed by another RunnerSet registered %s; "+
			"a ScaleSet set's FIRST runnerLabel is its scale-set name at GitHub, so two sets sharing it "+
			"would drive one scale set, each acquiring the other's jobs — pick a distinct first label "+
			"(ask your platform administrator which scale-set names that GitHub scope already holds)",
		self.Label, where)
}
