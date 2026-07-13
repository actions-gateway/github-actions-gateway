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
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/controller"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// validateFQDNBackend rejects an EgressProxy whose egressPolicyMode requests the FQDN
// intent on a cluster that declares no FQDN egress backend (--fqdn-policy-backend=none,
// the secure default). This is the fail-closed-and-loud contract of the intent/backend
// split (Q245): the operator learns at apply time, not from a stranded proxy pool that
// silently goes Degraded. The deprecated CiliumFQDN/CalicoFQDN intents pin their own
// backend and so are never gated here.
func validateFQDNBackend(spec *agcv2alpha1.EgressProxySpec, backend controller.FQDNBackend) error {
	// Only an explicitly-configured cilium/calico/gke backend admits FQDN intent; none,
	// the empty zero value, and any other value all fail closed (main.go normalizes the
	// flag via ParseFQDNBackend, so only valid values reach production).
	backendConfigured := backend == controller.FQDNBackendCilium ||
		backend == controller.FQDNBackendCalico ||
		backend == controller.FQDNBackendGKE
	if spec.EgressPolicyMode == agcv2alpha1.EgressPolicyModeFQDN && !backendConfigured {
		return fmt.Errorf(
			"spec.egressPolicyMode: FQDN requires the platform operator to configure an FQDN egress backend (GMC --fqdn-policy-backend=cilium|calico|gke); this cluster has none. Use egressPolicyMode: CIDR, or ask the platform operator to enable an FQDN backend")
	}
	return nil
}

// deprecatedModeWarning returns a non-blocking admission warning when an EgressProxy
// still names a deprecated CNI-specific egressPolicyMode. The value is accepted and
// keeps working (it pins its namesake backend), but the operator is nudged toward the
// FQDN intent + --fqdn-policy-backend split (Q245). An empty string means no warning.
func deprecatedModeWarning(mode agcv2alpha1.EgressPolicyMode) string {
	switch mode {
	case agcv2alpha1.EgressPolicyModeCiliumFQDN:
		return "spec.egressPolicyMode CiliumFQDN is deprecated: use FQDN and have the platform operator set GMC --fqdn-policy-backend=cilium. CiliumFQDN still works but will be removed in a future release."
	case agcv2alpha1.EgressPolicyModeCalicoFQDN:
		return "spec.egressPolicyMode CalicoFQDN is deprecated: use FQDN and have the platform operator set GMC --fqdn-policy-backend=calico. CalicoFQDN still works but will be removed in a future release."
	default:
		return ""
	}
}

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
// egress allowlist (Q242 G.1) and its spec.scheduling.priorityClassName against the
// infra-only PriorityClass allowlist (Q284).
//
// +kubebuilder:object:generate=false
type EgressProxyCustomValidator struct {
	// Allowlist is the shared platform egress allowlist (static flags ∪ ConfigMap
	// dynamic set). A nil allowlist denies every non-GitHub destination.
	Allowlist *allowlist.EgressDestinationAllowlist
	// FQDNBackend is the cluster's operator-selected FQDN egress backend (the GMC
	// --fqdn-policy-backend flag). The secure default (none) rejects FQDN intent at
	// admission (Q245).
	FQDNBackend controller.FQDNBackend
	// InfraPriorityClasses is the infra-only PriorityClass allowlist
	// (--allowed-infra-priority-classes, Q284), disjoint from the worker allowlist. A
	// nil allowlist forbids every named class (the secure default), so only an
	// empty/unset spec.scheduling.priorityClassName passes.
	InfraPriorityClasses *allowlist.PriorityClassAllowlist
}

// validate runs the shared admission checks for both create and update: it reject an
// FQDN intent with no operator backend, gates any extra destinations against the
// platform allowlist, and attaches a non-blocking deprecation warning for the legacy
// CiliumFQDN/CalicoFQDN modes.
func (v *EgressProxyCustomValidator) validate(ctx context.Context, verb string, obj *agcv2alpha1.EgressProxy) (admission.Warnings, error) {
	var warnings admission.Warnings
	if w := deprecatedModeWarning(obj.Spec.EgressPolicyMode); w != "" {
		warnings = append(warnings, w)
	}
	if err := validateFQDNBackend(&obj.Spec, v.FQDNBackend); err != nil {
		return warnings, logRejection(ctx, "EgressProxy", verb, obj.Namespace, obj.Name, err)
	}
	if err := validateEgressDestinations(&obj.Spec, v.Allowlist); err != nil {
		return warnings, logRejection(ctx, "EgressProxy", verb, obj.Namespace, obj.Name, err)
	}
	if err := validateSchedulingPriorityClass(obj.Spec.Scheduling, v.InfraPriorityClasses); err != nil {
		return warnings, logRejection(ctx, "EgressProxy", verb, obj.Namespace, obj.Name, err)
	}
	return warnings, nil
}

// ValidateCreate rejects an EgressProxy requesting an off-allowlist destination or an
// FQDN intent with no operator backend, warning on a deprecated CNI-specific mode.
func (v *EgressProxyCustomValidator) ValidateCreate(ctx context.Context, obj *agcv2alpha1.EgressProxy) (admission.Warnings, error) {
	return v.validate(ctx, "create", obj)
}

// ValidateUpdate applies the same gates on update, so widening the destinations or
// switching mode on an existing EgressProxy is checked too.
func (v *EgressProxyCustomValidator) ValidateUpdate(ctx context.Context, _, newObj *agcv2alpha1.EgressProxy) (admission.Warnings, error) {
	return v.validate(ctx, "update", newObj)
}

// ValidateDelete is a no-op.
func (v *EgressProxyCustomValidator) ValidateDelete(_ context.Context, _ *agcv2alpha1.EgressProxy) (admission.Warnings, error) {
	return nil, nil
}

// SetupEgressProxyWebhookWithManager registers the validating webhook for the
// EgressProxy data kind, wired to the shared platform egress allowlist, the cluster's
// FQDN egress backend, and the infra-only PriorityClass allowlist (Q284). The manager's
// scheme must already include agcv2alpha1 (the GMC registers it at startup).
func SetupEgressProxyWebhookWithManager(mgr ctrl.Manager, list *allowlist.EgressDestinationAllowlist, backend controller.FQDNBackend, infraPriorityClasses *allowlist.PriorityClassAllowlist) error {
	if err := ctrl.NewWebhookManagedBy(mgr, &agcv2alpha1.EgressProxy{}).
		WithValidator(&EgressProxyCustomValidator{Allowlist: list, FQDNBackend: backend, InfraPriorityClasses: infraPriorityClasses}).
		Complete(); err != nil {
		return fmt.Errorf("register EgressProxy webhook: %w", err)
	}
	return nil
}
