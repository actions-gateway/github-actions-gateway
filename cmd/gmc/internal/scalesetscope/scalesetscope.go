// Package scalesetscope holds the GitHub-scope half of the ScaleSet name-uniqueness
// guard (Q791): the scope key, the per-RunnerSet claim on a scale-set name, and the
// cluster-wide inventory both consumers read.
//
// A scale set's name is unique per GitHub config URL — the org, enterprise, or repo a
// gateway's githubURL names — because the AGC adopts one BY NAME against the Actions
// service that URL reaches (scaleset.Client.GetRunnerScaleSetByName). Namespaces do not
// enter into it: two gateways in different namespaces bound to one org share one
// scale-set namespace at GitHub, so two ScaleSet RunnerSets under them claiming one
// first runnerLabel drive a single scale set and each tenant's AGC acquires the other's
// jobs. That is the documented sharding topology (Appendix E.6 splits one org across
// namespaces and recommends consistent labels), not only an adversarial one.
//
// Two consumers read the inventory, at the two moments a collision can be seen:
//
//   - Admission (cmd/gmc/internal/webhook/v2alpha1) rejects a write that would create
//     one. Like the Q322 GitHub-bypass guard, the pair is assembled from two objects —
//     the label lives on the RunnerSet, the scope on its gateway — so both paths
//     consult the other side. RunnerSet create/update resolves its own gateway's scope
//     and rejects a first label already claimed anywhere in that scope; ActionsGateway
//     create/update resolves the scope its RunnerSet referrers would land in, closing
//     the create-order gap (a RunnerSet applied before its gateway admits unchecked,
//     §H.7, leaving the arriving gateway the first object that can see the conflict).
//   - Reconcile (cmd/gmc/internal/controller) reports one that already exists (Q849).
//     Admission fires on write, so a pair that predates the guard — an upgrade from a
//     release before Q791, or a window with the webhook uninstalled — is never
//     re-validated and runs on silently.
//
// A RunnerSet whose gateway is not stored contributes no scope (§H.7). Errors are the
// caller's to interpret: admission fails closed, since admitting an unverifiable claim
// is the collision this prevents, while reconcile leaves the last observed verdict
// standing rather than reporting a clean cluster it did not measure.
package scalesetscope

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	agcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// GitHubScope returns the scale-set namespace a gateway's githubURL reaches, as a
// comparable key: lowercased host and path, no trailing slash. GitHub owner and repo
// names are case-insensitive, so ".../Acme" and ".../acme" are one scope. The port is
// dropped (Hostname), which merges two GHES endpoints sharing a host — that errs
// toward rejecting, the safe direction for a guard against sharing. Returns "" when
// the URL does not parse or names no host or path; validation.GitHubURL rejects those
// at the gateway's own admission, so a stored gateway always yields a key.
func GitHubScope(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	path := strings.Trim(u.Path, "/")
	if host == "" || path == "" {
		return ""
	}
	return host + "/" + strings.ToLower(path)
}

// Claim is one ScaleSet RunnerSet's claim on a scale-set name: the set's identity, the
// gateway it names, its first runnerLabel, and the GitHub scope that gateway binds ("" when
// the gateway is not stored).
type Claim struct {
	Namespace  string
	Name       string
	GatewayRef string
	Label      string
	Scope      string
}

// CollidesWith reports whether two claims would drive one scale set at GitHub. The
// first labels must match, and then either claim shape suffices: the same gateway
// object — one scope whatever it resolves to, so this holds even before the gateway
// is applied — or the same resolved scope, which is what catches two gateways in
// different namespaces bound to one org.
func (c Claim) CollidesWith(o Claim) bool {
	if c.Label != o.Label {
		return false
	}
	if c.Namespace == o.Namespace && c.GatewayRef == o.GatewayRef {
		return true
	}
	return c.Scope != "" && c.Scope == o.Scope
}

// PendingGateway carries the scope of a gateway under admission, which is not yet
// stored and so cannot be read back from the API.
type PendingGateway struct {
	Key   client.ObjectKey
	Scope string
}

// Inventory is every ScaleSet claim in the cluster plus the gateway→scope lookup that
// places a claim whose own object is not yet stored.
type Inventory struct {
	Claims []Claim
	Scopes map[client.ObjectKey]string
}

// ScopeOf returns the GitHub scope bound by the named gateway in namespace, or ""
// when no such gateway is stored (§H.7).
func (inv Inventory) ScopeOf(namespace, gateway string) string {
	return inv.Scopes[client.ObjectKey{Namespace: namespace, Name: gateway}]
}

// Of reads every ActionsGateway and RunnerSet in the cluster and returns the ScaleSet
// claims among them. pending, when set, attributes a scope to a gateway being admitted
// so its referrers can be placed during its own admission.
//
// Both reads are cluster-wide, because the scale-set namespace at GitHub is: a
// namespace-scoped read cannot see the collision it is looking for. Whether they are
// cached is the caller's choice — admission passes the uncached API reader, since a
// just-created sibling may not be in the informer cache yet and admitting a colliding
// claim through a stale cache is exactly the race the guard exists to prevent, while
// the reconciler passes its cached client, which already watches both kinds and is
// reporting a persisted state rather than arbitrating a write.
func Of(ctx context.Context, reader client.Reader, pending *PendingGateway) (Inventory, error) {
	var gateways agcv2alpha1.ActionsGatewayList
	if err := reader.List(ctx, &gateways); err != nil {
		return Inventory{}, fmt.Errorf("list ActionsGateways: %w", err)
	}
	scopes := make(map[client.ObjectKey]string, len(gateways.Items))
	for i := range gateways.Items {
		gw := &gateways.Items[i]
		if s := GitHubScope(gw.Spec.GitHubURL); s != "" {
			scopes[client.ObjectKey{Namespace: gw.Namespace, Name: gw.Name}] = s
		}
	}
	if pending != nil && pending.Scope != "" {
		scopes[pending.Key] = pending.Scope
	}

	var sets agcv2alpha1.RunnerSetList
	if err := reader.List(ctx, &sets); err != nil {
		return Inventory{}, fmt.Errorf("list RunnerSets: %w", err)
	}
	inv := Inventory{Scopes: scopes}
	for i := range sets.Items {
		rs := &sets.Items[i]
		// Classic sets register no scale-set object, so they claim no name (Q726).
		if rs.Spec.AcquisitionProtocol != agcv2alpha1.AcquisitionProtocolScaleSet || len(rs.Spec.RunnerLabels) == 0 {
			continue
		}
		inv.Claims = append(inv.Claims, Claim{
			Namespace:  rs.Namespace,
			Name:       rs.Name,
			GatewayRef: rs.Spec.GatewayRef.Name,
			Label:      rs.Spec.RunnerLabels[0],
			Scope:      scopes[client.ObjectKey{Namespace: rs.Namespace, Name: rs.Spec.GatewayRef.Name}],
		})
	}
	return inv, nil
}
