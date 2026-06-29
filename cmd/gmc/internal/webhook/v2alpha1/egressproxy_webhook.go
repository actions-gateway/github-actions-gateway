/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v2alpha1

import (
	"context"
	"fmt"
	"net"

	agcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/allowlist"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// validateEgressDestinations rejects any EgressProxy.spec.destinationFQDNs /
// destinationCIDRs entry the platform allowlist does not cover (Q242 G.1). The
// EgressProxy is a namespace-scoped, tenant-authorable CR, so opening egress beyond
// the implicit GitHub set is an admin decision: a tenant may *request* a destination
// for GitOps ergonomics, but only the platform-owned --allowed-egress-fqdns /
// --allowed-egress-cidrs allowlist (plus its watched ConfigMap) decides what is
// permitted. Both empty ⇒ deny-all-non-GitHub (secure default).
//
// The complementary host-suffix-requires-FQDN-mode coupling is enforced by the CRD's
// CEL XValidation, so it is not re-checked here.
func validateEgressDestinations(spec *agcv2alpha1.EgressProxySpec, list *allowlist.EgressDestinationAllowlist) error {
	for _, fqdn := range spec.DestinationFQDNs {
		if !list.CoversFQDN(fqdn) {
			return fmt.Errorf(
				"spec.destinationFQDNs: %q is not permitted by the platform egress allowlist; allowed FQDN suffixes: %v (set by the GMC --allowed-egress-fqdns flag / its watched ConfigMap; empty forbids all non-GitHub destinations)",
				fqdn, list.FQDNSuffixes())
		}
	}
	for _, cidr := range spec.DestinationCIDRs {
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			// The CRD CEL validates CIDR syntax; this is defense-in-depth.
			return fmt.Errorf("spec.destinationCIDRs: %q is not a valid CIDR: %w", cidr, err)
		}
		if !list.CoversCIDR(n) {
			return fmt.Errorf(
				"spec.destinationCIDRs: %q is not contained in the platform egress allowlist; allowed CIDRs: %v (set by the GMC --allowed-egress-cidrs flag / its watched ConfigMap; empty forbids all non-GitHub destinations)",
				cidr, list.CIDRStrings())
		}
	}
	return nil
}

// +kubebuilder:webhook:path=/validate-actions-gateway-com-v2alpha1-egressproxy,mutating=false,failurePolicy=fail,sideEffects=None,groups=actions-gateway.com,resources=egressproxies,verbs=create;update,versions=v2alpha1,name=vegressproxy-v2alpha1.kb.io,admissionReviewVersions=v1

// EgressProxyCustomValidator validates the namespaced, tenant-authorable EgressProxy
// data kind, gating its destinationFQDNs/destinationCIDRs against the platform-owned
// egress allowlist (Q242 G.1).
//
// +kubebuilder:object:generate=false
type EgressProxyCustomValidator struct {
	// Allowlist is the shared platform egress allowlist (static flags ∪ ConfigMap
	// dynamic set). A nil allowlist denies every non-GitHub destination.
	Allowlist *allowlist.EgressDestinationAllowlist
}

// ValidateCreate rejects an EgressProxy requesting an off-allowlist destination.
func (v *EgressProxyCustomValidator) ValidateCreate(ctx context.Context, obj *agcv2alpha1.EgressProxy) (admission.Warnings, error) {
	if err := validateEgressDestinations(&obj.Spec, v.Allowlist); err != nil {
		return nil, logRejection(ctx, "EgressProxy", "create", obj.Namespace, obj.Name, err)
	}
	return nil, nil
}

// ValidateUpdate applies the same allowlist gate on update, so widening the
// destinations on an existing EgressProxy is checked too.
func (v *EgressProxyCustomValidator) ValidateUpdate(ctx context.Context, _, newObj *agcv2alpha1.EgressProxy) (admission.Warnings, error) {
	if err := validateEgressDestinations(&newObj.Spec, v.Allowlist); err != nil {
		return nil, logRejection(ctx, "EgressProxy", "update", newObj.Namespace, newObj.Name, err)
	}
	return nil, nil
}

// ValidateDelete is a no-op.
func (v *EgressProxyCustomValidator) ValidateDelete(_ context.Context, _ *agcv2alpha1.EgressProxy) (admission.Warnings, error) {
	return nil, nil
}

// SetupEgressProxyWebhookWithManager registers the validating webhook for the
// EgressProxy data kind, wired to the shared platform egress allowlist. The
// manager's scheme must already include agcv2alpha1 (the GMC registers it at
// startup).
func SetupEgressProxyWebhookWithManager(mgr ctrl.Manager, list *allowlist.EgressDestinationAllowlist) error {
	if err := ctrl.NewWebhookManagedBy(mgr, &agcv2alpha1.EgressProxy{}).
		WithValidator(&EgressProxyCustomValidator{Allowlist: list}).
		Complete(); err != nil {
		return fmt.Errorf("register EgressProxy webhook: %w", err)
	}
	return nil
}
