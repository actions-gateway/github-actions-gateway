// Package agentidentity holds the admission-side model of who owns an agent
// identity: the per-CR claim on an agent-identity stem, the GitHub scope that stem
// is registered in, and the cluster-wide inventory the two validators read (Q1011).
//
// An agent pool derives its agent Secret name and the runner name it registers with
// GitHub from one stem ([apinames.RunnerGroupAgentStem],
// [apinames.RunnerSetAgentStem]). Two owners deriving the same stem is not a
// recoverable state: each finds its Secret held by the other and each 409s on
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
// name guard agree on what "one GitHub scope" means.
//
// # Both derivations, both directions
//
// A v1alpha1 RunnerGroup named "rs-<x>" derives exactly the stem a v2 RunnerSet named
// "<x>" derives, because Q466's discriminator is an "rs-" prefix rather than an
// injection. Both writes are guarded, at the object each is actually authored on:
//
//   - RunnerSet create/update (cmd/gmc/internal/webhook/v2alpha1) rejects a set whose
//     stem is already claimed.
//   - ActionsGateway v1alpha1 create/update (cmd/gmc/internal/webhook/v1alpha1)
//     rejects a spec.runnerGroups entry whose DERIVED RunnerGroup name would claim
//     one. RunnerGroups are not tenant-authored — the GMC materializes them from that
//     spec under a name it derives — so the gateway write is where the operator can
//     still act, and no v1alpha1 RunnerGroup validator is needed.
//
// A stem whose owner cannot be placed in a GitHub scope (its gateway is not stored,
// §H.7) still collides within its own namespace; it simply matches nothing outside
// it. An empty scope means "unknown", never "the same unknown".
//
// Errors are the caller's to interpret; both current callers fail closed, since
// admitting an unverifiable claim is the collision this prevents.
package agentidentity

import (
	"context"
	"fmt"

	agcv1alpha1 "github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	"github.com/actions-gateway/github-actions-gateway/api/apinames"
	agcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	gmcv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/api/v1alpha1"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/scalesetscope"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Owner kinds, as they appear in a rejection message.
const (
	KindRunnerGroup = "RunnerGroup"
	KindRunnerSet   = "RunnerSet"
)

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

// PendingGateway carries a v1alpha1 ActionsGateway under admission, which is not yet
// stored: the RunnerGroups it would materialize claim no stem the API can be read
// for, and its own namespace's stored RunnerGroups must not be counted against it.
type PendingGateway struct {
	Key   client.ObjectKey
	Scope string
}

// Inventory is every agent-identity claim in the cluster, plus the gateway→scope
// lookups that place a claim whose own object is not yet stored.
type Inventory struct {
	Claims []Claim

	v1Scopes map[client.ObjectKey]string
	v2Scopes map[client.ObjectKey]string
}

// V1Scope returns the GitHub scope bound by the named v1alpha1 ActionsGateway, or ""
// when no such gateway is stored or its gitHubURL yields no key.
func (inv Inventory) V1Scope(namespace, gateway string) string {
	return inv.v1Scopes[client.ObjectKey{Namespace: namespace, Name: gateway}]
}

// V2Scope returns the GitHub scope bound by the named v2 ActionsGateway, or "" when
// no such gateway is stored (§H.7).
func (inv Inventory) V2Scope(namespace, gateway string) string {
	return inv.v2Scopes[client.ObjectKey{Namespace: namespace, Name: gateway}]
}

// Of reads every gateway of both versions and every RunnerGroup and RunnerSet in the
// cluster, and returns the stem claims among them.
//
// pending, when set, replaces the named v1alpha1 gateway's stored RunnerGroups with
// the ones its incoming spec would materialize: on an update the stored set is the
// gateway's own previous generation, and counting it would reject a gateway against
// itself. Its scope is used for every RunnerGroup it owns, since the stored object
// cannot be read back yet.
//
// All reads are cluster-wide, because the GitHub half of the boundary is: a
// namespace-scoped read cannot see the collision it is looking for. Whether they are
// cached is the caller's choice — admission passes the uncached API reader, since a
// just-created sibling may not be in the informer cache yet and admitting a colliding
// claim through a stale cache is the race the guard exists to prevent.
func Of(ctx context.Context, reader client.Reader, pending *PendingGateway) (Inventory, error) {
	v1Scopes, err := v1GatewayScopes(ctx, reader)
	if err != nil {
		return Inventory{}, err
	}
	v2Scopes, err := v2GatewayScopes(ctx, reader)
	if err != nil {
		return Inventory{}, err
	}
	if pending != nil {
		v1Scopes[pending.Key] = pending.Scope
	}

	inv := Inventory{v1Scopes: v1Scopes, v2Scopes: v2Scopes}

	var groups agcv1alpha1.RunnerGroupList
	if err := reader.List(ctx, &groups); err != nil {
		return Inventory{}, fmt.Errorf("list RunnerGroups: %w", err)
	}
	for i := range groups.Items {
		rg := &groups.Items[i]
		owner := v1GatewayOf(rg, v1Scopes)
		// A pending gateway supplies its own referrers below; its stored ones are
		// the generation this write replaces.
		if pending != nil && owner == pending.Key {
			continue
		}
		inv.Claims = append(inv.Claims, Claim{
			Namespace: rg.Namespace,
			Kind:      KindRunnerGroup,
			Name:      rg.Name,
			Stem:      apinames.RunnerGroupAgentStem(rg.Name),
			Scope:     v1Scopes[owner],
		})
	}

	var sets agcv2alpha1.RunnerSetList
	if err := reader.List(ctx, &sets); err != nil {
		return Inventory{}, fmt.Errorf("list RunnerSets: %w", err)
	}
	for i := range sets.Items {
		rs := &sets.Items[i]
		inv.Claims = append(inv.Claims, Claim{
			Namespace: rs.Namespace,
			Kind:      KindRunnerSet,
			Name:      rs.Name,
			Stem:      apinames.RunnerSetAgentStem(rs.Name),
			Scope:     v2Scopes[client.ObjectKey{Namespace: rs.Namespace, Name: rs.Spec.GatewayRef.Name}],
		})
	}
	return inv, nil
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

// RunnerSetClaim is the claim a v2 RunnerSet makes, resolved against inv's view of
// its gateway.
func RunnerSetClaim(rs *agcv2alpha1.RunnerSet, scope string) Claim {
	return Claim{
		Namespace: rs.Namespace,
		Kind:      KindRunnerSet,
		Name:      rs.Name,
		Stem:      apinames.RunnerSetAgentStem(rs.Name),
		Scope:     scope,
	}
}

// RunnerGroupClaim is the claim a v1alpha1 RunnerGroup named name in namespace makes.
func RunnerGroupClaim(namespace, name, scope string) Claim {
	return Claim{
		Namespace: namespace,
		Kind:      KindRunnerGroup,
		Name:      name,
		Stem:      apinames.RunnerGroupAgentStem(name),
		Scope:     scope,
	}
}

// v1GatewayScopes maps each stored v1alpha1 ActionsGateway to the GitHub scope it
// binds. A gateway whose gitHubURL yields no key is present with an empty scope, so
// v1GatewayOf can still attribute its RunnerGroups to it.
func v1GatewayScopes(ctx context.Context, reader client.Reader) (map[client.ObjectKey]string, error) {
	var gateways gmcv1alpha1.ActionsGatewayList
	if err := reader.List(ctx, &gateways); err != nil {
		return nil, fmt.Errorf("list v1alpha1 ActionsGateways: %w", err)
	}
	scopes := make(map[client.ObjectKey]string, len(gateways.Items))
	for i := range gateways.Items {
		gw := &gateways.Items[i]
		scopes[client.ObjectKey{Namespace: gw.Namespace, Name: gw.Name}] = scalesetscope.GitHubScope(gw.Spec.GitHubURL)
	}
	return scopes, nil
}

// v2GatewayScopes maps each stored v2 ActionsGateway to the GitHub scope it binds.
func v2GatewayScopes(ctx context.Context, reader client.Reader) (map[client.ObjectKey]string, error) {
	var gateways agcv2alpha1.ActionsGatewayList
	if err := reader.List(ctx, &gateways); err != nil {
		return nil, fmt.Errorf("list v2alpha1 ActionsGateways: %w", err)
	}
	scopes := make(map[client.ObjectKey]string, len(gateways.Items))
	for i := range gateways.Items {
		gw := &gateways.Items[i]
		if s := scalesetscope.GitHubScope(gw.Spec.GitHubURL); s != "" {
			scopes[client.ObjectKey{Namespace: gw.Namespace, Name: gw.Name}] = s
		}
	}
	return scopes, nil
}

// v1GatewayOf names the v1alpha1 ActionsGateway a RunnerGroup belongs to. The GMC
// stamps a controller owner reference on every RunnerGroup it materializes, which is
// exact. A RunnerGroup written directly — a path the GMC does not use, and which
// bypasses these webhooks anyway — falls back to the namespace's sole gateway, since
// v1 admission holds one per namespace (validateSingleton); a namespace holding none
// or several yields no owner, and such a claim is then matched within its namespace
// only.
func v1GatewayOf(rg *agcv1alpha1.RunnerGroup, scopes map[client.ObjectKey]string) client.ObjectKey {
	for _, ref := range rg.OwnerReferences {
		if ref.Kind == "ActionsGateway" {
			return client.ObjectKey{Namespace: rg.Namespace, Name: ref.Name}
		}
	}
	var sole client.ObjectKey
	found := 0
	for key := range scopes {
		if key.Namespace == rg.Namespace {
			sole = key
			found++
		}
	}
	if found == 1 {
		return sole
	}
	return client.ObjectKey{}
}
