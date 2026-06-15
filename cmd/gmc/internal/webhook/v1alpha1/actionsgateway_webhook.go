package v1alpha1

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	gmcv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/api/v1alpha1"
)

// defaultReservedNamespaces are namespaces in which an ActionsGateway CR is
// forbidden regardless of where the GMC is installed. `kube-system` and
// `kube-public` are universal; `gmc-system` is the default install namespace
// shipped by the project. Custom installs add their own namespace at setup
// time via the downward API (see SetupActionsGatewayWebhookWithManager).
var defaultReservedNamespaces = []string{
	"kube-system",
	"kube-public",
	"gmc-system",
}

// newReservedNamespaces returns the full set of forbidden namespaces. The
// defaults always apply; podNamespace is added when non-empty so that a
// non-default install (e.g. `actions-gateway-operator`) is also protected.
func newReservedNamespaces(podNamespace string) map[string]bool {
	s := make(map[string]bool, len(defaultReservedNamespaces)+1)
	for _, ns := range defaultReservedNamespaces {
		s[ns] = true
	}
	if podNamespace != "" {
		s[podNamespace] = true
	}
	return s
}

// SetupActionsGatewayWebhookWithManager registers the webhook for
// ActionsGateway in the manager. The GMC's own install namespace is read
// from the POD_NAMESPACE env var (which the Deployment populates via the
// downward API) and added to the reserved-namespace set so tenants cannot
// create an ActionsGateway in the operator's own namespace. allowedPriorityClasses
// is the platform allowlist of cluster-scoped PriorityClass names tenants may
// reference in priorityTiers (see ValidateCreate / validatePriorityClasses).
func SetupActionsGatewayWebhookWithManager(mgr ctrl.Manager, allowedPriorityClasses []string) error {
	return ctrl.NewWebhookManagedBy(mgr, &gmcv1alpha1.ActionsGateway{}).
		WithValidator(NewActionsGatewayCustomValidator(os.Getenv("POD_NAMESPACE"), allowedPriorityClasses)).
		Complete()
}

// NewActionsGatewayCustomValidator returns a validator whose reserved-namespace
// set includes the universal Kubernetes reserved namespaces, the GMC's default
// install namespace, and the supplied podNamespace if non-empty.
// allowedPriorityClasses is the platform allowlist of cluster-scoped
// PriorityClass names tenants may reference in priorityTiers; an empty slice
// forbids every priorityTiers PriorityClass reference (secure default). Tests
// use this to drive both behaviors without relying on the global environment.
func NewActionsGatewayCustomValidator(podNamespace string, allowedPriorityClasses []string) *ActionsGatewayCustomValidator {
	allowed := make(map[string]bool, len(allowedPriorityClasses))
	for _, name := range allowedPriorityClasses {
		if name != "" {
			allowed[name] = true
		}
	}
	return &ActionsGatewayCustomValidator{
		reservedNamespaces:       newReservedNamespaces(podNamespace),
		allowedPriorityClasses:   allowed,
		allowedPriorityClassList: allowedPriorityClassNames(allowed),
	}
}

// allowedPriorityClassNames returns the allowlist keys as a sorted slice for
// deterministic error messages.
func allowedPriorityClassNames(allowed map[string]bool) []string {
	names := make([]string, 0, len(allowed))
	for name := range allowed {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
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

	// allowedPriorityClasses is the platform allowlist of cluster-scoped
	// PriorityClass names a tenant may reference in priorityTiers. A nil/empty
	// map forbids every PriorityClass reference (secure default): a tenant
	// cannot name an arbitrary high-priority class and preempt other tenants'
	// worker pods. Populated by NewActionsGatewayCustomValidator.
	allowedPriorityClasses map[string]bool

	// allowedPriorityClassList is the sorted allowlist, cached for deterministic
	// rejection messages.
	allowedPriorityClassList []string
}

// ValidateCreate rejects CRs created in reserved namespaces, with a cross-namespace
// gitHubAppRef, or with privileged containers.
func (v *ActionsGatewayCustomValidator) ValidateCreate(_ context.Context, obj *gmcv1alpha1.ActionsGateway) (admission.Warnings, error) {
	if v.reservedNamespaces[obj.Namespace] {
		return nil, fmt.Errorf("ActionsGateway may not be created in reserved namespace %q", obj.Namespace)
	}
	if err := validateGitHubAppRef(obj); err != nil {
		return nil, err
	}
	if err := validateGitHubURL(obj); err != nil {
		return nil, err
	}
	if err := validateRunnerGroups(obj); err != nil {
		return nil, err
	}
	if err := v.validatePriorityClasses(obj); err != nil {
		return nil, err
	}
	return proxyResourceWarnings(obj), nil
}

// ValidateUpdate rejects updates that introduce a cross-namespace gitHubAppRef,
// privileged containers, or a silent securityProfile downgrade.
func (v *ActionsGatewayCustomValidator) ValidateUpdate(_ context.Context, oldObj, newObj *gmcv1alpha1.ActionsGateway) (admission.Warnings, error) {
	if err := validateGitHubAppRef(newObj); err != nil {
		return nil, err
	}
	if err := validateGitHubURL(newObj); err != nil {
		return nil, err
	}
	if err := validateRunnerGroups(newObj); err != nil {
		return nil, err
	}
	if err := v.validatePriorityClasses(newObj); err != nil {
		return nil, err
	}
	if err := validateSecurityProfileTransition(oldObj, newObj); err != nil {
		return nil, err
	}
	return proxyResourceWarnings(newObj), nil
}

// ValidateDelete is a no-op.
func (v *ActionsGatewayCustomValidator) ValidateDelete(_ context.Context, _ *gmcv1alpha1.ActionsGateway) (admission.Warnings, error) {
	return nil, nil
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
// org/enterprise/repo URL: it must parse, use the https scheme, name a host, and
// carry at least one path segment (the organization, enterprise, or owner). The
// AGC's GithubRegistrar derives its REST endpoints by string-splitting this URL
// (see cmd/agc/internal/agentpool/github_registrar.go), so a malformed value
// would silently produce broken registration calls rather than a clear failure.
// The check lives in the webhook (not a CRD CEL rule) so the error can name the
// offending component; the CRD Pattern is only a cheap https scheme guard.
func validateGitHubURL(ag *gmcv1alpha1.ActionsGateway) error {
	raw := ag.Spec.GitHubURL
	if raw == "" {
		// The CRD marks gitHubURL required (MinLength=1); a hand-built object that
		// reaches the validator directly without it is still rejected here.
		return fmt.Errorf("gitHubURL is required: set the GitHub organization, enterprise, or repository URL the runners register against")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("gitHubURL %q is not a valid URL: %w", raw, err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("gitHubURL must use the https scheme (got %q)", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("gitHubURL must include a host (got %q)", raw)
	}
	if strings.Trim(u.Path, "/") == "" {
		return fmt.Errorf("gitHubURL must include an organization, enterprise, or owner/repo path segment (got %q)", raw)
	}
	return nil
}

// validateRunnerGroups rejects privileged containers in any RunnerGroup's PodTemplate.
// This check was previously expressed as a CEL x-kubernetes-validations rule on the CRD, but
// iterating over an unbounded corev1.PodTemplateSpec.containers array exceeds the k8s 1.35
// CEL cost budget. The admission webhook is the correct place for this validation.
func validateRunnerGroups(ag *gmcv1alpha1.ActionsGateway) error {
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

// validatePriorityClasses rejects any priorityTiers entry whose
// priorityClassName is not on the platform allowlist. PriorityClass is a
// cluster-scoped resource carrying a priority value and a preemptionPolicy; an
// unvalidated tenant-chosen class lets a tenant name a high-priority, preempting
// class and have the scheduler evict OTHER tenants' running worker pods —
// breaking the cross-tenant isolation the per-tenant model promises (Q132). The
// platform pre-creates the permitted classes and lists their names via
// --allowed-priority-classes; the GMC only validates references against that
// list (it never creates the cluster-scoped classes — that stays platform-owned,
// consistent with the Q121/Q122/Q130 confinement model). An empty allowlist
// forbids every reference (secure default).
//
// This is a webhook check, not a CRD CEL rule, because the allowlist is dynamic
// platform config a spec-scoped CEL XValidation cannot read.
func (v *ActionsGatewayCustomValidator) validatePriorityClasses(ag *gmcv1alpha1.ActionsGateway) error {
	for i, rg := range ag.Spec.RunnerGroups {
		for j, tier := range rg.PriorityTiers {
			if !v.allowedPriorityClasses[tier.PriorityClassName] {
				return fmt.Errorf(
					"runnerGroups[%d].priorityTiers[%d]: priorityClassName %q is not in the platform allowlist %v; "+
						"the platform admin must pre-create the PriorityClass and add it to the GMC --allowed-priority-classes flag",
					i, j, tier.PriorityClassName, v.allowedPriorityClassList)
			}
		}
	}
	return nil
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
	if newObj.Annotations[gmcv1alpha1.AllowProfileDowngradeAnnotation] == "true" {
		return nil // explicit, deliberate downgrade
	}
	return fmt.Errorf(
		"securityProfile downgrade from %q to %q is not permitted without the %q annotation set to \"true\"; "+
			"downgrading relaxes Pod Security Admission isolation and must be deliberate",
		effectiveProfile(oldObj.Spec.SecurityProfile), effectiveProfile(newObj.Spec.SecurityProfile),
		gmcv1alpha1.AllowProfileDowngradeAnnotation)
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
