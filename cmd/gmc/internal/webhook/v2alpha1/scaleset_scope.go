package v2alpha1

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	agcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// This file holds the GitHub-scope half of the ScaleSet label-uniqueness guard
// (Q791). A scale set's name is unique per GitHub config URL — the org, enterprise,
// or repo a gateway's githubURL names — because the AGC adopts one BY NAME against
// the Actions service that URL reaches (scaleset.Client.GetRunnerScaleSetByName).
// Namespaces do not enter into it: two gateways in different namespaces bound to one
// org share one scale-set namespace at GitHub, so two ScaleSet RunnerSets under them
// claiming one first runnerLabel drive a single scale set and each tenant's AGC
// acquires the other's jobs. That is the documented sharding topology (Appendix E.6
// splits one org across namespaces and recommends consistent labels), not only an
// adversarial one.
//
// Like the Q322 GitHub-bypass guard, the pair is assembled from two objects — the
// label lives on the RunnerSet, the scope on its gateway — so both admission paths
// consult the other side through the uncached API reader:
//
//   - RunnerSet create/update resolves its own gateway's scope and rejects a first
//     label already claimed anywhere in that scope.
//   - ActionsGateway create/update resolves the scope its RunnerSet referrers would
//     land in, closing the create-order gap: a RunnerSet applied before its gateway
//     admits unchecked (§H.7), leaving the arriving gateway the first object that can
//     see the conflict.
//
// A RunnerSet whose gateway is not stored contributes no scope (§H.7); List errors
// fail closed, since admitting an unverifiable claim is the collision this prevents.

// gitHubScope returns the scale-set namespace a gateway's githubURL reaches, as a
// comparable key: lowercased host and path, no trailing slash. GitHub owner and repo
// names are case-insensitive, so ".../Acme" and ".../acme" are one scope. The port is
// dropped (Hostname), which merges two GHES endpoints sharing a host — that errs
// toward rejecting, the safe direction for a guard against sharing. Returns "" when
// the URL does not parse or names no host or path; validation.GitHubURL rejects those
// at the gateway's own admission, so a stored gateway always yields a key.
func gitHubScope(raw string) string {
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

// scaleSetClaim is one ScaleSet RunnerSet's claim on a scale-set name: the set's
// identity, the gateway it names, its first runnerLabel, and the GitHub scope that
// gateway binds ("" when the gateway is not stored).
type scaleSetClaim struct {
	namespace  string
	name       string
	gatewayRef string
	label      string
	scope      string
}

// collidesWith reports whether two claims would drive one scale set at GitHub. The
// first labels must match, and then either claim shape suffices: the same gateway
// object — one scope whatever it resolves to, so this holds even before the gateway
// is applied — or the same resolved scope, which is what catches two gateways in
// different namespaces bound to one org.
func (c scaleSetClaim) collidesWith(o scaleSetClaim) bool {
	if c.label != o.label {
		return false
	}
	if c.namespace == o.namespace && c.gatewayRef == o.gatewayRef {
		return true
	}
	return c.scope != "" && c.scope == o.scope
}

// pendingGateway carries the scope of a gateway under admission, which is not yet
// stored and so cannot be read back from the API.
type pendingGateway struct {
	key   client.ObjectKey
	scope string
}

// scaleSetInventory is every ScaleSet claim in the cluster plus the gateway→scope
// lookup that places a claim whose own object is not yet stored.
type scaleSetInventory struct {
	claims []scaleSetClaim
	scopes map[client.ObjectKey]string
}

// scopeOf returns the GitHub scope bound by the named gateway in namespace, or ""
// when no such gateway is stored (§H.7).
func (inv scaleSetInventory) scopeOf(namespace, gateway string) string {
	return inv.scopes[client.ObjectKey{Namespace: namespace, Name: gateway}]
}

// scaleSetInventoryOf reads every ActionsGateway and RunnerSet in the cluster and
// returns the ScaleSet claims among them. pending, when set, attributes a scope to a
// gateway being admitted so its referrers can be placed during its own admission.
//
// Both reads are cluster-wide and uncached. Cluster-wide because the scale-set
// namespace at GitHub is, so a namespace-scoped read cannot see the collision it is
// looking for; uncached for the reason the namespace-scoped guard gave — a
// just-created sibling may not be in the informer cache yet, and admitting a colliding
// claim through a stale cache is exactly the race the guard exists to prevent. The
// cost lands only on ScaleSet RunnerSet and ActionsGateway writes, which are tenant
// configuration events rather than a hot path.
func scaleSetInventoryOf(ctx context.Context, reader client.Reader, pending *pendingGateway) (scaleSetInventory, error) {
	var gateways agcv2alpha1.ActionsGatewayList
	if err := reader.List(ctx, &gateways); err != nil {
		return scaleSetInventory{}, fmt.Errorf("list ActionsGateways: %w", err)
	}
	scopes := make(map[client.ObjectKey]string, len(gateways.Items))
	for i := range gateways.Items {
		gw := &gateways.Items[i]
		if s := gitHubScope(gw.Spec.GitHubURL); s != "" {
			scopes[client.ObjectKey{Namespace: gw.Namespace, Name: gw.Name}] = s
		}
	}
	if pending != nil && pending.scope != "" {
		scopes[pending.key] = pending.scope
	}

	var sets agcv2alpha1.RunnerSetList
	if err := reader.List(ctx, &sets); err != nil {
		return scaleSetInventory{}, fmt.Errorf("list RunnerSets: %w", err)
	}
	inv := scaleSetInventory{scopes: scopes}
	for i := range sets.Items {
		rs := &sets.Items[i]
		// Classic sets register no scale-set object, so they claim no name (Q726).
		if rs.Spec.AcquisitionProtocol != agcv2alpha1.AcquisitionProtocolScaleSet || len(rs.Spec.RunnerLabels) == 0 {
			continue
		}
		inv.claims = append(inv.claims, scaleSetClaim{
			namespace:  rs.Namespace,
			name:       rs.Name,
			gatewayRef: rs.Spec.GatewayRef.Name,
			label:      rs.Spec.RunnerLabels[0],
			scope:      scopes[client.ObjectKey{Namespace: rs.Namespace, Name: rs.Spec.GatewayRef.Name}],
		})
	}
	return inv, nil
}

// scaleSetConflictError renders a claim collision for the tenant applying self. A
// holder in the applying object's own namespace is named — the tenant owns both
// objects. A holder in another namespace is not: the RunnerSet is tenant-authored, so
// naming it would disclose another tenant's namespace and object to anyone able to
// create a RunnerSet in their own. logRejection writes the full detail to the GMC log
// either way, so the platform admin keeps what the tenant is not told.
func scaleSetConflictError(self, holder scaleSetClaim) error {
	where := fmt.Sprintf("under gateway %q in namespace %q", self.gatewayRef, self.namespace)
	if self.scope != "" {
		where = fmt.Sprintf("against GitHub scope %q", self.scope)
	}
	if holder.namespace == self.namespace {
		return fmt.Errorf(
			"ScaleSet runnerLabels[0] %q is already used by RunnerSet %q registered %s; "+
				"a ScaleSet set's FIRST runnerLabel is its scale-set name at GitHub, so two sets sharing it "+
				"would drive one scale set — pick a distinct first label (later labels may overlap freely)",
			self.label, holder.name, where)
	}
	return fmt.Errorf(
		"ScaleSet runnerLabels[0] %q is already claimed by another RunnerSet registered %s; "+
			"a ScaleSet set's FIRST runnerLabel is its scale-set name at GitHub, so two sets sharing it "+
			"would drive one scale set, each acquiring the other's jobs — pick a distinct first label "+
			"(ask your platform administrator which scale-set names that GitHub scope already holds)",
		self.label, where)
}
