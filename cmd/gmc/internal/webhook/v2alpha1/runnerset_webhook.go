package v2alpha1

import (
	"context"
	"fmt"

	agcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/allowlist"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/webhook/validation"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// +kubebuilder:webhook:path=/validate-actions-gateway-com-v2alpha1-runnerset,mutating=false,failurePolicy=fail,sideEffects=None,groups=actions-gateway.com,resources=runnersets,verbs=create;update,versions=v2alpha1,name=vrunnerset-v2alpha1.kb.io,admissionReviewVersions=v1

// The validator lists RunnerSets cluster-wide to enforce ScaleSet label uniqueness
// within a GitHub scope (Q791), so the GMC ServiceAccount needs read access to them
// (the AGC — not the GMC — owns their lifecycle, hence read-only here). The GMC's
// role is already a ClusterRole, so the existing rule covers the wider read. Since a
// ScaleSet set fails closed if this List is denied, the permission is mandatory once
// ScaleSet is the default (Q264 P5): without it every ScaleSet RunnerSet is rejected
// `cannot list resource "runnersets"`.
// +kubebuilder:rbac:groups=actions-gateway.com,resources=runnersets,verbs=get;list;watch

// RunnerSetCustomValidator enforces the RunnerSet invariants a spec-scoped CRD CEL
// rule cannot express:
//
//   - No two ScaleSet-protocol RunnerSets under one gateway may claim the same single
//     runnerLabel (Q264 §5a-U7). The scale set's name IS that label, so two such sets
//     would register the same scale-set name at GitHub and collide. Everything else
//     about acquisitionProtocol — the enum, the default, the immutability, and the
//     ScaleSet⇒exactly-one-label rule — is enforced by CRD CEL on the RunnerSet type.
//   - Every priorityTiers[].priorityClassName must be on the platform PriorityClass
//     allowlist (Q132/Q289): the allowlist is dynamic platform config CEL cannot read.
//
// +kubebuilder:object:generate=false
type RunnerSetCustomValidator struct {
	// PriorityClasses is the platform allowlist of cluster-scoped PriorityClass names
	// a tenant may reference in priorityTiers. A nil allowlist forbids every named
	// class (the secure default), matching the v1 ActionsGateway validator.
	PriorityClasses *allowlist.PriorityClassAllowlist

	// reader lists RunnerSets and the ActionsGateways that give them a GitHub scope
	// for the label-uniqueness guard (Q791), and resolves the gatewayRef/proxyRef pair
	// for the noProxyCIDRs GitHub-bypass guard (Q322, validateProxyGitHubBypass). It is
	// the manager's uncached API reader in production (wired by
	// SetupRunnerSetWebhookWithManager): a just-created sibling may not be in the
	// informer cache yet, and admitting a colliding scale-set label through a stale
	// cache is exactly the race the guard exists to prevent — mirroring the v1
	// ActionsGateway singleton guard. A nil reader disables the check (direct-
	// construction unit tests that are not exercising it); the integration/e2e and
	// production paths always wire a reader.
	reader client.Reader
}

// ValidateCreate rejects a RunnerSet naming an off-allowlist PriorityClass in
// priorityTiers, a ScaleSet RunnerSet whose single runnerLabel collides with an
// existing ScaleSet sibling under the same gateway, or a proxyRef naming an
// EgressProxy whose noProxyCIDRs would route the gateway's GitHub host around it.
func (v *RunnerSetCustomValidator) ValidateCreate(ctx context.Context, obj *agcv2alpha1.RunnerSet) (admission.Warnings, error) {
	if err := v.validatePriorityTiers(obj); err != nil {
		return nil, logRejection(ctx, "RunnerSet", "create", obj.Namespace, obj.Name, err)
	}
	if err := v.validateScaleSetLabelUniqueness(ctx, obj); err != nil {
		return nil, logRejection(ctx, "RunnerSet", "create", obj.Namespace, obj.Name, err)
	}
	if err := v.validateProxyGitHubBypass(ctx, obj); err != nil {
		return nil, logRejection(ctx, "RunnerSet", "create", obj.Namespace, obj.Name, err)
	}
	return nil, nil
}

// ValidateUpdate applies the same checks on update: an existing RunnerSet cannot be
// edited to smuggle in an off-allowlist PriorityClass, and — acquisitionProtocol
// itself being immutable (CRD CEL) — runnerLabels, gatewayRef, and proxyRef can
// still change, so an update can still move a ScaleSet set onto a colliding label
// or bind the gateway's GitHub host to a proxy that excludes it. Deletion-only
// updates — deletionTimestamp set, spec unchanged — are admitted without
// re-validation (Q518; see validation.DeletionOnlyUpdate).
func (v *RunnerSetCustomValidator) ValidateUpdate(ctx context.Context, oldObj, newObj *agcv2alpha1.RunnerSet) (admission.Warnings, error) {
	if validation.DeletionOnlyUpdate(newObj, oldObj.Spec, newObj.Spec) {
		return nil, nil
	}
	if err := v.validatePriorityTiers(newObj); err != nil {
		return nil, logRejection(ctx, "RunnerSet", "update", newObj.Namespace, newObj.Name, err)
	}
	if err := v.validateScaleSetLabelUniqueness(ctx, newObj); err != nil {
		return nil, logRejection(ctx, "RunnerSet", "update", newObj.Namespace, newObj.Name, err)
	}
	if err := v.validateProxyGitHubBypass(ctx, newObj); err != nil {
		return nil, logRejection(ctx, "RunnerSet", "update", newObj.Namespace, newObj.Name, err)
	}
	return nil, nil
}

// validatePriorityTiers rejects any priorityTiers[].priorityClassName not on the
// platform PriorityClass allowlist (Q132/Q289). The RunnerSet is tenant-authored —
// it is the v2 front door — and the AGC stamps the matched tier's class verbatim
// onto worker pods (provisioner.buildPod), so an ungated tier is a direct route to
// the escalation the allowlist exists to stop: Kubernetes ships
// system-cluster-critical (value 2000000000, preemptionPolicy PreemptLowerPriority)
// in every cluster with nothing restricting it to kube-system, so a tenant naming it
// would have the scheduler evict OTHER tenants' running workers — and their egress
// proxies — to place its own. Mirrors the v1 ActionsGateway webhook's
// validatePriorityClasses; the tier name is required, so an empty string is itself
// off-allowlist rather than "unset".
func (v *RunnerSetCustomValidator) validatePriorityTiers(rs *agcv2alpha1.RunnerSet) error {
	for i, tier := range rs.Spec.PriorityTiers {
		if !v.PriorityClasses.Allowed(tier.PriorityClassName) {
			return fmt.Errorf(
				"priorityTiers[%d]: priorityClassName %q is not in the platform allowlist %v; "+
					"a PriorityClass sets the scheduler's preemption order across the whole cluster, so the platform admin must "+
					"pre-create it and add it to the GMC --allowed-priority-classes flag or the watched PriorityClass allowlist ConfigMap",
				i, tier.PriorityClassName, v.PriorityClasses.Names())
		}
	}
	return nil
}

// ValidateDelete is a no-op.
func (v *RunnerSetCustomValidator) ValidateDelete(_ context.Context, _ *agcv2alpha1.RunnerSet) (admission.Warnings, error) {
	return nil, nil
}

// validateScaleSetLabelUniqueness rejects a ScaleSet-protocol RunnerSet whose FIRST
// runnerLabel is already claimed by another ScaleSet-protocol RunnerSet in the same
// GitHub scope — the org, enterprise, or repo its gateway's githubURL names. The first
// label is the scale set's name at GitHub, so two sets sharing it are two controllers
// driving one scale-set object. It is a no-op for Classic sets, which have no
// scale-set object.
//
// The scope, NOT the namespace, is the uniqueness boundary (Q791): the AGC adopts a
// scale set by name against the Actions service the gateway's githubURL reaches, so
// two tenants in different namespaces under one org collide. A same-gateway claim also
// counts even when that gateway is not yet applied — two sets naming one gateway share
// its scope whatever it resolves to. See scaleset_scope.go for both halves of the
// guard and the create-order gap the gateway webhook closes.
//
// Labels after the first are deliberately NOT checked. They are ordinary match
// targets, so "linux" on a dozen sets is the expected shape, and which set an
// ambiguous runs-on reaches is GitHub's decision rather than an admission-time
// collision (Q726). For a single-label set this is the same comparison it has
// always been.
//
// The check is fail-closed: if the List errors, the request is rejected rather than
// admitted on faith — admitting a possible collision is the failure mode this guards
// against.
func (v *RunnerSetCustomValidator) validateScaleSetLabelUniqueness(ctx context.Context, rs *agcv2alpha1.RunnerSet) error {
	if rs.Spec.AcquisitionProtocol != agcv2alpha1.AcquisitionProtocolScaleSet {
		return nil
	}
	if len(rs.Spec.RunnerLabels) == 0 {
		// MinItems=1 rejects this before the webhook runs; nothing to compare.
		return nil
	}
	if v.reader == nil {
		// No reader wired (direct-construction unit-test path); the integration/e2e
		// and production paths always wire the uncached API reader.
		return nil
	}
	inv, err := scaleSetInventoryOf(ctx, v.reader, nil)
	if err != nil {
		return fmt.Errorf(
			"cannot verify ScaleSet runnerLabel uniqueness for %q in namespace %q: %w",
			rs.Name, rs.Namespace, err)
	}
	self := scaleSetClaim{
		namespace:  rs.Namespace,
		name:       rs.Name,
		gatewayRef: rs.Spec.GatewayRef.Name,
		label:      rs.Spec.RunnerLabels[0],
		scope:      inv.scopeOf(rs.Namespace, rs.Spec.GatewayRef.Name),
	}
	for _, other := range inv.claims {
		// On CREATE the new object is not yet persisted; on UPDATE it appears in the
		// inventory. Either way, skip the object being admitted.
		if other.namespace == self.namespace && other.name == self.name {
			continue
		}
		if self.collidesWith(other) {
			return scaleSetConflictError(self, other)
		}
	}
	return nil
}

// validateProxyGitHubBypass rejects a RunnerSet whose proxyRef names an EgressProxy
// whose noProxyCIDRs would route the set's GitHub host — its gateway's gitHubURL
// host, a GitHub Enterprise Server host in particular — around that proxy (Q322,
// noproxy_referrers.go). A set with no proxyRef inherits the gateway's
// defaultProxyRef, a pair the gateway's own admission already validates, so only an
// explicit proxyRef is checked here. A missing gateway or proxy admits the write
// (§H.7): the pair is re-checked from the arriving object's side when it is created.
// Read errors other than NotFound fail closed, like the label-uniqueness guard.
func (v *RunnerSetCustomValidator) validateProxyGitHubBypass(ctx context.Context, rs *agcv2alpha1.RunnerSet) error {
	if v.reader == nil || rs.Spec.ProxyRef == nil {
		return nil
	}
	var gw agcv2alpha1.ActionsGateway
	if err := v.reader.Get(ctx, client.ObjectKey{Namespace: rs.Namespace, Name: rs.Spec.GatewayRef.Name}, &gw); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("cannot verify proxyRef %q against gateway %q's GitHub host: %w",
			rs.Spec.ProxyRef.Name, rs.Spec.GatewayRef.Name, err)
	}
	return validateGitHubHostAgainstProxy(ctx, v.reader, rs.Namespace, rs.Spec.ProxyRef.Name,
		"spec.proxyRef", gitHubURLHost(gw.Spec.GitHubURL))
}

// SetupRunnerSetWebhookWithManager registers the validating webhook for the
// namespaced RunnerSet. The manager's scheme must already include agcv2alpha1 (the
// GMC registers it at startup). The uncached API reader backs the sibling-uniqueness
// guard, matching the v1 ActionsGateway singleton webhook. priorityClasses is the
// shared platform PriorityClass allowlist priorityTiers are gated against
// (Q132/Q289); nil forbids every named class, the secure default.
func SetupRunnerSetWebhookWithManager(mgr ctrl.Manager, priorityClasses *allowlist.PriorityClassAllowlist) error {
	v := &RunnerSetCustomValidator{reader: mgr.GetAPIReader(), PriorityClasses: priorityClasses}
	if err := ctrl.NewWebhookManagedBy(mgr, &agcv2alpha1.RunnerSet{}).
		WithValidator(v).
		Complete(); err != nil {
		return fmt.Errorf("register RunnerSet webhook: %w", err)
	}
	return nil
}
