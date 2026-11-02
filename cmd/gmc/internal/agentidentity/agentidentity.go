// Package agentidentity holds the admission-side model of who owns an agent
// identity: the per-CR claim on an agent-identity stem, the GitHub scope that stem
// is registered in, and the cluster-wide inventory the RunnerSet validator reads
// (Q1011).
//
// An agent pool derives its agent Secret name and the runner name it registers with
// GitHub from one stem ([apinames.RunnerSetAgentStem]). Two owners deriving the same
// stem is not a recoverable state: each finds its Secret held by the other and each 409s on
// register, resolved by deleting the incumbent's GitHub record, so both tenants'
// listeners run unauthorized until an operator renames one of the CRs
// (agentpool.ErrAgentNameCollision, Q979). Admission is the only place that fight can
// be prevented rather than reported.
//
// # The two halves have different boundaries
//
// The agent Secret is namespaced with its owner, so two owners in one namespace
// collide on it whatever GitHub they reach. The runner name is unique per GitHub
// org, enterprise, or repo — the scope a gateway's gitHubURL names — so two owners in
// DIFFERENT namespaces still collide on it whenever their gateways bind one scope.
// Guarding only the namespace would leave that half open, and the documented sharding
// topology (Appendix E.6) splits one org across namespaces, so it is the expected
// shape rather than an adversarial one. [Claim.CollidesWith] is therefore true on
// either condition.
//
// Scope keys are [scalesetscope.GitHubScope], so this guard and the Q791 scale-set
// name guard agree on what "one GitHub scope" means. RunnerSet create/update
// (cmd/gmc/internal/webhook/v2) rejects a set whose stem is already claimed.
//
// A stem whose owner cannot be placed in a GitHub scope (its gateway is not stored,
// §H.7) still collides within its own namespace; it simply matches nothing outside
// it. An empty scope means "unknown", never "the same unknown".
//
// Errors are the caller's to interpret; the caller fails closed, since admitting an
// unverifiable claim is the collision this prevents.
package agentidentity

import (
	"context"
	"fmt"

	"github.com/actions-gateway/github-actions-gateway/api/apinames"
	agcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/scalesetscope"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// KindRunnerSet is the owner kind, as it appears in a rejection message.
const KindRunnerSet = "RunnerSet"

// Claim is one CR's claim on an agent-identity stem: the owner's identity, the stem
// its pool derives every agent name from, and the GitHub scope its gateway binds
// ("" when that gateway is not stored).
type Claim struct {
	Namespace string
	Kind      string
	Name      string
	Stem      string
	Scope     string
}

// Is reports whether c names the same CR as o — the object under admission skipping
// its own stored copy on an update.
func (c Claim) Is(o Claim) bool {
	return c.Namespace == o.Namespace && c.Kind == o.Kind && c.Name == o.Name
}

// CollidesWith reports whether two claims would drive one agent identity. The stems
// must match, and then either boundary suffices: the same namespace, where the agent
// Secret is shared, or the same resolved GitHub scope, where the registered runner
// name is.
func (c Claim) CollidesWith(o Claim) bool {
	if c.Stem != o.Stem {
		return false
	}
	if c.Namespace == o.Namespace {
		return true
	}
	return c.Scope != "" && c.Scope == o.Scope
}

// Inventory is every agent-identity claim in the cluster, plus the gateway→scope
// lookups that place a claim whose own object is not yet stored.
type Inventory struct {
	Claims []Claim

	v2Scopes map[client.ObjectKey]string
}

// V2Scope returns the GitHub scope bound by the named v2 ActionsGateway, or "" when
// no such gateway is stored (§H.7).
func (inv Inventory) V2Scope(namespace, gateway string) string {
	return inv.v2Scopes[client.ObjectKey{Namespace: namespace, Name: gateway}]
}

// Of reads every v2 ActionsGateway and RunnerSet in the cluster, and returns the stem
// claims among them.
//
// All reads are cluster-wide, because the GitHub half of the boundary is: a
// namespace-scoped read cannot see the collision it is looking for. Whether they are
// cached is the caller's choice — admission passes the uncached API reader, since a
// just-created sibling may not be in the informer cache yet and admitting a colliding
// claim through a stale cache is the race the guard exists to prevent.
func Of(ctx context.Context, reader client.Reader) (Inventory, error) {
	var gateways agcv2alpha1.ActionsGatewayList
	if err := reader.List(ctx, &gateways); err != nil {
		return Inventory{}, fmt.Errorf("list v2alpha1 ActionsGateways: %w", err)
	}
	var sets agcv2alpha1.RunnerSetList
	if err := reader.List(ctx, &sets); err != nil {
		return Inventory{}, fmt.Errorf("list RunnerSets: %w", err)
	}
	return FromLists(gateways.Items, sets.Items), nil
}

// FromLists derives the inventory [Of] returns from lists the caller has already
// read, for a caller whose other guards read the same kinds (Q1075).
func FromLists(gateways []agcv2alpha1.ActionsGateway, sets []agcv2alpha1.RunnerSet) Inventory {
	v2Scopes := v2GatewayScopes(gateways)
	inv := Inventory{v2Scopes: v2Scopes}
	for i := range sets {
		rs := &sets[i]
		inv.Claims = append(inv.Claims, Claim{
			Namespace: rs.Namespace,
			Kind:      KindRunnerSet,
			Name:      rs.Name,
			Stem:      apinames.RunnerSetAgentStem(rs.Name),
			Scope:     v2Scopes[client.ObjectKey{Namespace: rs.Namespace, Name: rs.Spec.GatewayRef.Name}],
		})
	}
	return inv
}

// Collision returns the first stored claim self collides with, or nil when the stem
// is free. self's own stored copy is skipped, so an update does not reject the object
// against itself.
func (inv Inventory) Collision(self Claim) *Claim {
	for i := range inv.Claims {
		other := inv.Claims[i]
		if self.Is(other) {
			continue
		}
		if self.CollidesWith(other) {
			return &other
		}
	}
	return nil
}

// RunnerSetClaim is the claim a v2 RunnerSet named name in namespace makes, resolved
// against inv's view of its gateway.
func RunnerSetClaim(namespace, name, scope string) Claim {
	return Claim{
		Namespace: namespace,
		Kind:      KindRunnerSet,
		Name:      name,
		Stem:      apinames.RunnerSetAgentStem(name),
		Scope:     scope,
	}
}

// v2GatewayScopes maps each stored v2 ActionsGateway to the GitHub scope it binds.
func v2GatewayScopes(gateways []agcv2alpha1.ActionsGateway) map[client.ObjectKey]string {
	scopes := make(map[client.ObjectKey]string, len(gateways))
	for i := range gateways {
		gw := &gateways[i]
		if s := scalesetscope.GitHubScope(gw.Spec.GitHubURL); s != "" {
			scopes[client.ObjectKey{Namespace: gw.Namespace, Name: gw.Name}] = s
		}
	}
	return scopes
}
