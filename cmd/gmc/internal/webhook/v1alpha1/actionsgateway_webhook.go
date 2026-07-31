package v1alpha1

import (
	"context"
	"fmt"
	"net/url"
	"os"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	gmcv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/api/v1alpha1"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/allowlist"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/webhook/noproxy"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/webhook/validation"
)

// SetupActionsGatewayWebhookWithManager registers the webhook for
// ActionsGateway in the manager. The GMC's own install namespace is read
// from the POD_NAMESPACE env var (which the Deployment populates via the
// downward API) and added to the reserved-namespace set so tenants cannot
// create an ActionsGateway in the operator's own namespace. priorityClasses
// is the platform allowlist of cluster-scoped PriorityClass names tenants may
// reference in priorityTiers (see ValidateCreate / validatePriorityClasses); it
// is the shared, live allowlist whose dynamic half the GMC's ConfigMap watch
// updates (Q188). A nil pointer is treated as an empty allowlist (the secure
// default — no PriorityClass reference permitted).
func SetupActionsGatewayWebhookWithManager(mgr ctrl.Manager, priorityClasses *allowlist.PriorityClassAllowlist) error {
	v := NewActionsGatewayCustomValidatorWithAllowlist(os.Getenv("POD_NAMESPACE"), priorityClasses)
	// The per-namespace singleton guard lists existing ActionsGateways. Use the
	// uncached API reader, not the manager's cache-backed client: a just-created
	// CR may not be in the informer cache yet, and admitting a second CR through
	// a stale cache is exactly the race the guard exists to prevent.
	v.reader = mgr.GetAPIReader()
	return ctrl.NewWebhookManagedBy(mgr, &gmcv1alpha1.ActionsGateway{}).
		WithValidator(v).
		Complete()
}

// NewActionsGatewayCustomValidator returns a validator whose reserved-namespace
// set includes the universal Kubernetes reserved namespaces, the GMC's default
// install namespace, and the supplied podNamespace if non-empty.
// allowedPriorityClasses is the static platform allowlist of cluster-scoped
// PriorityClass names tenants may reference in priorityTiers; an empty slice
// forbids every priorityTiers PriorityClass reference (secure default). Tests
// use this to drive both behaviors without relying on the global environment.
// The resulting validator carries a fixed (static-only) allowlist; production
// wires the shared, ConfigMap-backed allowlist via
// NewActionsGatewayCustomValidatorWithAllowlist.
func NewActionsGatewayCustomValidator(podNamespace string, allowedPriorityClasses []string) *ActionsGatewayCustomValidator {
	return NewActionsGatewayCustomValidatorWithAllowlist(podNamespace, allowlist.New(allowedPriorityClasses))
}

// NewActionsGatewayCustomValidatorWithAllowlist is NewActionsGatewayCustomValidator
// with the PriorityClass allowlist supplied as a live, shared
// *allowlist.PriorityClassAllowlist rather than a static slice, so the GMC's
// ConfigMap watch can widen it without restarting the validator (Q188). A nil
// allowlist is treated as empty (the secure default — no PriorityClass reference
// permitted).
func NewActionsGatewayCustomValidatorWithAllowlist(podNamespace string, priorityClasses *allowlist.PriorityClassAllowlist) *ActionsGatewayCustomValidator {
	if priorityClasses == nil {
		priorityClasses = allowlist.New(nil)
	}
	return &ActionsGatewayCustomValidator{
		reservedNamespaces: validation.ReservedNamespaces(podNamespace),
		priorityClasses:    priorityClasses,
	}
}

// +kubebuilder:webhook:path=/validate-actions-gateway-github-com-v1alpha1-actionsgateway,mutating=false,failurePolicy=fail,sideEffects=None,groups=actions-gateway.github.com,resources=actionsgateways,verbs=create;update,versions=v1alpha1,name=vactionsgateway-v1alpha1.kb.io,admissionReviewVersions=v1

// ActionsGatewayCustomValidator validates ActionsGateway resources.
//
// +kubebuilder:object:generate=false
type ActionsGatewayCustomValidator struct {
	// reservedNamespaces is the set of namespaces where ActionsGateway CRs
	// are forbidden. Populated by NewActionsGatewayCustomValidator. If nil
	// (e.g. a test that constructs the struct directly), the reservation
	// check is a no-op — those tests are responsible for not relying on it.
	// Production paths go through the constructor.
	reservedNamespaces map[string]bool

	// priorityClasses is the platform allowlist of cluster-scoped PriorityClass
	// names a tenant may reference in priorityTiers. An empty allowlist forbids
	// every PriorityClass reference (secure default): a tenant cannot name an
	// arbitrary high-priority class and preempt other tenants' worker pods. It is
	// the shared, live allowlist (static flag ∪ ConfigMap-sourced dynamic set,
	// Q188), read fresh on every admission so a ConfigMap update takes effect
	// without restarting the webhook. Populated by the constructors; never nil
	// after construction.
	priorityClasses *allowlist.PriorityClassAllowlist

	// reader lists existing ActionsGateways for the per-namespace singleton
	// guard (validateSingleton). It is the manager's uncached API reader in
	// production (wired by SetupActionsGatewayWebhookWithManager). A nil reader
	// disables the singleton check — unit tests that construct the validator
	// directly are not exercising it; the integration/e2e and production paths
	// always wire a reader.
	reader client.Reader
}

// logRejection records a server-side audit line whenever an admission request is
// denied. The webhook returns rich rejection messages to the API client, but
// without this the GMC keeps no trail of who attempted a privileged-container or
// reserved-namespace create — exactly the events an operator needs after the
// fact. It is logged at Info (not Debug): admission denials are rare and
// security-relevant, so the audit trail must be visible by default. The error
// text is a validation message (namespace, URL, container, or PriorityClass
// names) and never carries Secret contents or credentials.
func logRejection(ctx context.Context, op string, ag *gmcv1alpha1.ActionsGateway, err error) error {
	logf.FromContext(ctx).Info("ActionsGateway admission denied",
		"operation", op,
		"namespace", ag.Namespace,
		"name", ag.Name,
		"reason", err.Error())
	return err
}

// ValidateCreate rejects CRs created in reserved namespaces, with a cross-namespace
// gitHubAppRef, with privileged containers, or requesting securityProfile:
// privileged in a namespace the platform has not labelled eligible.
func (v *ActionsGatewayCustomValidator) ValidateCreate(ctx context.Context, obj *gmcv1alpha1.ActionsGateway) (admission.Warnings, error) {
	if v.reservedNamespaces[obj.Namespace] {
		return nil, logRejection(ctx, "create", obj, fmt.Errorf("ActionsGateway may not be created in reserved namespace %q", obj.Namespace))
	}
	if err := v.validateSingleton(ctx, obj); err != nil {
		return nil, logRejection(ctx, "create", obj, err)
	}
	if err := v.validatePrivilegedEligibility(ctx, obj); err != nil {
		return nil, logRejection(ctx, "create", obj, err)
	}
	if err := validateGitHubAppRef(obj); err != nil {
		return nil, logRejection(ctx, "create", obj, err)
	}
	if err := validateGitHubURL(obj); err != nil {
		return nil, logRejection(ctx, "create", obj, err)
	}
	if err := validateRunnerGroups(obj); err != nil {
		return nil, logRejection(ctx, "create", obj, err)
	}
	if err := v.validatePriorityClasses(obj); err != nil {
		return nil, logRejection(ctx, "create", obj, err)
	}
	if err := validateNoProxyCIDRs(obj); err != nil {
		return nil, logRejection(ctx, "create", obj, err)
	}
	return proxyResourceWarnings(obj), nil
}

// ValidateUpdate rejects updates that introduce a cross-namespace gitHubAppRef,
// privileged containers, a silent securityProfile downgrade, or securityProfile:
// privileged in a namespace the platform has not labelled eligible.
func (v *ActionsGatewayCustomValidator) ValidateUpdate(ctx context.Context, oldObj, newObj *gmcv1alpha1.ActionsGateway) (admission.Warnings, error) {
	if err := validateGitHubAppRef(newObj); err != nil {
		return nil, logRejection(ctx, "update", newObj, err)
	}
	if err := validateGitHubURL(newObj); err != nil {
		return nil, logRejection(ctx, "update", newObj, err)
	}
	if err := validateRunnerGroups(newObj); err != nil {
		return nil, logRejection(ctx, "update", newObj, err)
	}
	if err := v.validatePriorityClasses(newObj); err != nil {
		return nil, logRejection(ctx, "update", newObj, err)
	}
	if err := validateSecurityProfileTransition(oldObj, newObj); err != nil {
		return nil, logRejection(ctx, "update", newObj, err)
	}
	if err := v.validatePrivilegedEligibility(ctx, newObj); err != nil {
		return nil, logRejection(ctx, "update", newObj, err)
	}
	if err := validateNoProxyCIDRs(newObj); err != nil {
		return nil, logRejection(ctx, "update", newObj, err)
	}
	return proxyResourceWarnings(newObj), nil
}

// ValidateDelete is a no-op.
func (v *ActionsGatewayCustomValidator) ValidateDelete(_ context.Context, _ *gmcv1alpha1.ActionsGateway) (admission.Warnings, error) {
	return nil, nil
}

// validateSingleton rejects creating a second ActionsGateway in a namespace that
// already has one (Q127). Only one ActionsGateway per namespace is supported:
// every per-tenant resource (the AGC Deployment, proxy Deployment, Services,
// NetworkPolicies, RoleBindings) has a fixed, namespace-scoped name, so two CRs
// fight over the same objects — and because each CR's securityProfile drives the
// namespace's Pod Security Admission labels, two CRs with different profiles make
// the GMC flap those labels, intermittently admitting privileged pods. Deleting
// either CR then tears down the survivor's infrastructure. Rejecting the second
// create at admission is the clean guard.
//
// The check is fail-closed: if the singleton invariant cannot be verified (the
// List errors), the create is rejected rather than admitted on faith — admitting
// a possible second CR is the failure mode this guards against. It runs only on
// CREATE; an update never adds a CR, and the per-namespace name uniqueness the
// apiserver already enforces means an update cannot turn one CR into two.
func (v *ActionsGatewayCustomValidator) validateSingleton(ctx context.Context, ag *gmcv1alpha1.ActionsGateway) error {
	if v.reader == nil {
		// No reader wired (unit-test path); the integration/e2e and production
		// paths always wire one. Skipping here keeps direct-construction unit
		// tests focused on the validation they exercise.
		return nil
	}
	var existing gmcv1alpha1.ActionsGatewayList
	if err := v.reader.List(ctx, &existing, client.InNamespace(ag.Namespace)); err != nil {
		return fmt.Errorf("cannot verify the one-ActionsGateway-per-namespace invariant for namespace %q: %w", ag.Namespace, err)
	}
	for i := range existing.Items {
		// On CREATE the new object is not yet persisted, so any returned item is
		// pre-existing. Skip a name match defensively (a re-create observed
		// through an unexpected path must not self-trip).
		if existing.Items[i].Name == ag.Name {
			continue
		}
		return fmt.Errorf(
			"an ActionsGateway (%q) already exists in namespace %q; only one ActionsGateway per namespace is supported — "+
				"a second CR contends over fixed-name per-tenant resources and would flap the namespace's Pod Security Admission labels",
			existing.Items[i].Name, ag.Namespace)
	}
	return nil
}

// validatePrivilegedEligibility rejects an ActionsGateway requesting
// securityProfile: privileged unless its namespace carries
// PrivilegedProfileLabel set to PrivilegedProfileAllowed ("true") — at
// create OR update (Q133). It closes a self-granted-escalation gap: a tenant
// owns the ActionsGateway CR and may freely set securityProfile: privileged at
// create (only *downgrades* are otherwise gated, by
// validateSecurityProfileTransition), and that profile makes the GMC stamp the
// namespace PSA to `privileged`. So without this gate a tenant could grant
// themselves the cluster's least-restrictive pod-security posture. Eligibility to
// run privileged is instead a platform decision: a platform administrator opts a
// namespace in by labelling it (the same trust model as the
// actions-gateway.github.com/tenant marker), and the tenant cannot self-grant it
// because they do not control namespace labels.
//
// The check is fail-closed in every direction. Non-privileged profiles never
// consult the label. For privileged, the namespace must be readable AND carry the
// exact label/value; a read error, a missing label, or any other value rejects
// the request. The eligibility decision is made on the namespace's CURRENT label
// (read via the uncached API reader) — a tenant cannot smuggle the label in
// through the ActionsGateway CR, which carries no namespace labels.
//
// This is a webhook check, not a CRD CEL rule, because the decision depends on a
// label of a *different* object (the namespace) that a spec-scoped CEL
// XValidation cannot read.
func (v *ActionsGatewayCustomValidator) validatePrivilegedEligibility(ctx context.Context, ag *gmcv1alpha1.ActionsGateway) error {
	if effectiveProfile(ag.Spec.SecurityProfile) != "privileged" {
		return nil
	}
	if v.reader == nil {
		// No reader wired (direct-construction unit-test path). The
		// integration/e2e and production paths always wire the uncached API
		// reader (SetupActionsGatewayWebhookWithManager), and exercise the label
		// gate end to end; skipping here keeps direct-construction unit tests
		// focused on the validation they exercise, mirroring validateSingleton.
		return nil
	}
	var ns corev1.Namespace
	if err := v.reader.Get(ctx, client.ObjectKey{Name: ag.Namespace}, &ns); err != nil {
		// Fail closed: if eligibility cannot be confirmed, privileged is denied.
		return fmt.Errorf(
			"cannot verify privileged eligibility for namespace %q: %w; securityProfile: privileged requires the "+
				"namespace label %s=%s applied by a platform administrator",
			ag.Namespace, err, gmcv1alpha1.PrivilegedProfileLabel, gmcv1alpha1.PrivilegedProfileAllowed)
	}
	// Dual-read the eligibility grant across both label domains for the v1/v2
	// coexistence window (§H.12) — see validation.PrivilegedGrantPresent, the single
	// definition this and `gag-migrate` share so the tool and admission can never
	// disagree about whether a namespace is granted (Q463).
	if !validation.PrivilegedGrantPresent(ns.Labels) {
		return fmt.Errorf(
			"securityProfile: privileged is not eligible in namespace %q: it requires the namespace label %s=%s "+
				"(or the aligned %s=%s), which only a platform administrator may apply — privileged eligibility is a "+
				"platform decision and is deliberately not tenant-settable",
			ag.Namespace, gmcv1alpha1.PrivilegedProfileLabel, gmcv1alpha1.PrivilegedProfileAllowed,
			v2alpha1.PrivilegedProfileLabel, v2alpha1.PrivilegedProfileAllowed)
	}
	return nil
}

// validateGitHubAppRef rejects a non-empty gitHubAppRef.namespace. The field is
// ignored by the Secret lookup (which always uses the CR's own namespace), but it
// looks like a cross-namespace reference to users — a confused-deputy footgun.
// A CEL XValidation rule is not used here because k8s ≤ 1.30 CEL cannot apply
// has() to optional non-pointer string fields; the webhook is version-agnostic.
func validateGitHubAppRef(ag *gmcv1alpha1.ActionsGateway) error {
	if ag.Spec.GitHubAppRef.Namespace != "" {
		return fmt.Errorf("gitHubAppRef.namespace is not supported; the Secret must reside in the ActionsGateway's own namespace (got %q)", ag.Spec.GitHubAppRef.Namespace)
	}
	return nil
}

// validateGitHubURL rejects a spec.gitHubURL that is not a well-formed GitHub
// org/enterprise/repo URL. The check itself lives in the shared validation
// package so the v1 and v2 webhooks enforce a single definition (Q323).
func validateGitHubURL(ag *gmcv1alpha1.ActionsGateway) error {
	return validation.GitHubURL(ag.Spec.GitHubURL)
}

// validateRunnerGroups rejects privileged containers in any RunnerGroup's PodTemplate,
// EXCEPT when the ActionsGateway has explicitly opted into securityProfile: privileged.
// This check was previously expressed as a CEL x-kubernetes-validations rule on the CRD, but
// iterating over an unbounded corev1.PodTemplateSpec.containers array exceeds the k8s 1.35
// CEL cost budget. The admission webhook is the correct place for this validation.
//
// Profile-awareness (Q127) resolves an incoherence: securityProfile: privileged is a
// documented, supported opt-in for Kata/DinD worker patterns (05-security.md), and it makes
// the GMC stamp the namespace PSA to `privileged` so Pod Security Admission admits privileged
// pods — yet this webhook still rejected privileged containers unconditionally, making the
// documented pattern impossible to actually apply. Gating the rejection on the profile keeps
// the behaviour secure by default (baseline/restricted, including the empty default, still
// reject privileged) while honouring the explicit privileged opt-in. The GMC-managed path is
// the only one that flows through this webhook; a directly-applied RunnerGroup CR bypasses it
// entirely, so Pod Security Admission (stamped per the namespace's profile) is the real
// enforcement backstop for both paths — see 05-security.md.
func validateRunnerGroups(ag *gmcv1alpha1.ActionsGateway) error {
	if effectiveProfile(ag.Spec.SecurityProfile) == "privileged" {
		// The tenant has explicitly opted into the privileged profile; the
		// namespace PSA is stamped `privileged` to match, so privileged worker
		// containers are a coherent, admitted configuration here.
		return nil
	}
	for i, rg := range ag.Spec.RunnerGroups {
		for _, c := range rg.PodTemplate.Spec.Containers {
			if isPrivileged(c.SecurityContext) {
				return fmt.Errorf("runnerGroups[%d]: privileged containers are not permitted in worker pods (container %q)", i, c.Name)
			}
		}
		for _, c := range rg.PodTemplate.Spec.InitContainers {
			if isPrivileged(c.SecurityContext) {
				return fmt.Errorf("runnerGroups[%d]: privileged init containers are not permitted in worker pods (container %q)", i, c.Name)
			}
		}
	}
	return nil
}

// validatePriorityClasses rejects any tenant-authored reference to a cluster-scoped
// PriorityClass that is not on the platform allowlist. PriorityClass carries a
// priority value and a preemptionPolicy; an unvalidated tenant-chosen class lets a
// tenant name a high-priority, preempting class and have the scheduler evict OTHER
// tenants' running worker pods — breaking the cross-tenant isolation the per-tenant
// model promises (Q132). The platform pre-creates the permitted classes and lists
// their names via --allowed-priority-classes (or the watched allowlist ConfigMap,
// Q188); the GMC only validates references against that list (it never creates the
// cluster-scoped classes — that stays platform-owned, consistent with the
// Q121/Q122/Q130 confinement model). An empty allowlist forbids every reference
// (secure default).
//
// A v1 ActionsGateway reaches a worker pod's priorityClassName by TWO routes, and
// both are gated here (Q289):
//
//   - runnerGroups[].priorityTiers[].priorityClassName — the tier the AGC stamps once
//     a concurrency threshold is crossed. Required, so an empty name is itself invalid.
//   - runnerGroups[].podTemplate.spec.priorityClassName — the full PodTemplateSpec the
//     GMC copies into the bootstrapped RunnerGroup and the AGC then copies verbatim
//     into the worker pod, overriding it only when a tier matches. Optional, so an
//     empty name means "no PriorityClass" and is always permitted.
//
// Gating only the first left the second wide open: Kubernetes ships
// system-cluster-critical (value 2000000000, preemptionPolicy PreemptLowerPriority)
// in every cluster and does not restrict it to kube-system, so a tenant could preempt
// every other tenant's workers and egress proxies with no setup at all.
//
// This is a webhook check, not a CRD CEL rule, because the allowlist is dynamic
// platform config a spec-scoped CEL XValidation cannot read.
func (v *ActionsGatewayCustomValidator) validatePriorityClasses(ag *gmcv1alpha1.ActionsGateway) error {
	const remedy = "the platform admin must pre-create the PriorityClass and add it to the GMC --allowed-priority-classes flag " +
		"or the watched PriorityClass allowlist ConfigMap"
	for i, rg := range ag.Spec.RunnerGroups {
		for j, tier := range rg.PriorityTiers {
			if !v.priorityClasses.Allowed(tier.PriorityClassName) {
				return fmt.Errorf(
					"runnerGroups[%d].priorityTiers[%d]: priorityClassName %q is not in the platform allowlist %v; %s",
					i, j, tier.PriorityClassName, v.priorityClasses.Names(), remedy)
			}
		}
		if pc := rg.PodTemplate.Spec.PriorityClassName; !v.priorityClasses.AllowedPodPriorityClass(pc) {
			return fmt.Errorf(
				"runnerGroups[%d].podTemplate.spec.priorityClassName: %q is not in the platform allowlist %v; "+
					"a PriorityClass sets the scheduler's preemption order across the whole cluster, so %s",
				i, pc, v.priorityClasses.Names(), remedy)
		}
	}
	return nil
}

// validateNoProxyCIDRs rejects any spec.proxy.noProxyCIDRs entry that would route
// the tenant's GitHub-bound traffic around the per-tenant egress proxy, defeating
// the egress-IP attribution that isolates tenants. The mechanism (NO_PROXY
// domain-suffix semantics, the surgical hostname-vs-CIDR split, and the accepted
// IP-range residual) lives in the shared noproxy package; this wrapper adds the
// tenant's own gitHubURL host (including a GitHub Enterprise Server host) to the
// protected set — the reason this is a webhook check, not a CRD CEL rule
// (mirroring validateGitHubAppRef).
func validateNoProxyCIDRs(ag *gmcv1alpha1.ActionsGateway) error {
	protected := append([]string{}, noproxy.GitHubHosts...)
	if u, err := url.Parse(ag.Spec.GitHubURL); err == nil && u.Hostname() != "" {
		protected = append(protected, u.Hostname())
	}
	return noproxy.ValidateEntries("proxy.noProxyCIDRs", ag.Spec.Proxy.NoProxyCIDRs, protected)
}

// securityProfileRank orders the Pod Security Admission profiles from least to
// most restrictive. A downgrade is any update that lowers the rank. An empty
// value maps to the baseline default (see effectiveProfile).
var securityProfileRank = map[string]int{
	"privileged": 0,
	"baseline":   1,
	"restricted": 2,
}

// effectiveProfile returns the securityProfile, substituting the baseline
// default for an empty value so an old/new comparison matches what the GMC
// actually stamps on the namespace.
func effectiveProfile(profile string) string {
	if profile == "" {
		return "baseline"
	}
	return profile
}

// validateSecurityProfileTransition rejects an update that lowers
// spec.securityProfile to a less-restrictive level (e.g. restricted -> baseline)
// unless the new object carries AllowProfileDowngradeAnnotation set to "true".
// Upgrades and no-op changes are always allowed. Gating relaxation on an
// explicit annotation means a stray re-apply — or a manifest that drops the
// field and lets it re-default — cannot silently weaken a tenant's isolation,
// while a deliberate rollback (e.g. after a failed hardening attempt) needs only
// a two-field edit rather than recreating the whole ActionsGateway.
func validateSecurityProfileTransition(oldObj, newObj *gmcv1alpha1.ActionsGateway) error {
	oldRank, oldOK := securityProfileRank[effectiveProfile(oldObj.Spec.SecurityProfile)]
	newRank, newOK := securityProfileRank[effectiveProfile(newObj.Spec.SecurityProfile)]
	if !oldOK || !newOK {
		// Unknown values are rejected by the CRD enum; nothing to compare here.
		return nil
	}
	if newRank >= oldRank {
		return nil // upgrade or no change
	}
	// Dual-read the downgrade opt-in across both domains AND both value keywords for
	// the v1/v2 coexistence window (§H.12, Q147): the legacy
	// actions-gateway.github.com/allow-profile-downgrade="true" and the aligned
	// actions-gateway.com/allow-profile-downgrade="allowed" are both honored
	// across the v1/v2 coexistence window — only the accepted *spelling* of the
	// explicit opt-in is widened (cf. validation.PrivilegedGrantPresent).
	if newObj.Annotations[gmcv1alpha1.AllowProfileDowngradeAnnotation] == "true" ||
		newObj.Annotations[v2alpha1.AllowProfileDowngradeAnnotation] == v2alpha1.AllowProfileDowngradeAllowed {
		return nil // explicit, deliberate downgrade
	}
	return fmt.Errorf(
		"securityProfile downgrade from %q to %q is not permitted without the %q annotation set to \"true\" "+
			"(or the aligned %q set to %q); downgrading relaxes Pod Security Admission isolation and must be deliberate",
		effectiveProfile(oldObj.Spec.SecurityProfile), effectiveProfile(newObj.Spec.SecurityProfile),
		gmcv1alpha1.AllowProfileDowngradeAnnotation,
		v2alpha1.AllowProfileDowngradeAnnotation, v2alpha1.AllowProfileDowngradeAllowed)
}

// proxyResourceWarnings returns a warning when proxy.resources.requests is set
// without a cpu key. The builder merges user values over defaults, so the
// default cpu request is preserved in the Deployment, but callers who expect
// their explicit requests map to be the authoritative source will be surprised.
// A warning surfaces the issue at apply time without blocking the operation.
func proxyResourceWarnings(ag *gmcv1alpha1.ActionsGateway) admission.Warnings {
	if ag.Spec.Proxy.Resources.Requests != nil {
		if _, hasCPU := ag.Spec.Proxy.Resources.Requests[corev1.ResourceCPU]; !hasCPU {
			return admission.Warnings{"proxy.resources.requests does not include cpu; " +
				"HPA requires a cpu request to compute utilization — autoscaling will not function if the default is later removed"}
		}
	}
	return nil
}

// isPrivileged returns true when the SecurityContext explicitly sets privileged: true.
func isPrivileged(sc *corev1.SecurityContext) bool {
	return sc != nil && sc.Privileged != nil && *sc.Privileged
}
