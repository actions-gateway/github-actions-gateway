package v1alpha1

import (
	"context"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
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
	v := NewActionsGatewayCustomValidator(os.Getenv("POD_NAMESPACE"), allowedPriorityClasses)
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
// AllowPrivilegedProfileLabel set to AllowPrivilegedProfileValue ("true") — at
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
			ag.Namespace, err, gmcv1alpha1.AllowPrivilegedProfileLabel, gmcv1alpha1.AllowPrivilegedProfileValue)
	}
	if ns.Labels[gmcv1alpha1.AllowPrivilegedProfileLabel] != gmcv1alpha1.AllowPrivilegedProfileValue {
		return fmt.Errorf(
			"securityProfile: privileged is not eligible in namespace %q: it requires the namespace label %s=%s, "+
				"which only a platform administrator may apply — privileged eligibility is a platform decision and is "+
				"deliberately not tenant-settable",
			ag.Namespace, gmcv1alpha1.AllowPrivilegedProfileLabel, gmcv1alpha1.AllowPrivilegedProfileValue)
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

// githubNoProxyHosts is the set of public GitHub hostnames that the AGC and
// worker pods must always reach *through* the per-tenant egress proxy for the
// egress-IP attribution to hold. A NO_PROXY entry that matches any of these
// would route that tenant's GitHub traffic around the proxy. The tenant's own
// gitHubURL host (including a GitHub Enterprise Server host) is added
// dynamically in validateNoProxyCIDRs.
var githubNoProxyHosts = []string{
	"github.com",
	"api.github.com",
	"codeload.github.com",
	"objects.githubusercontent.com",
	"raw.githubusercontent.com",
	"pkg-containers.githubusercontent.com",
	"ghcr.io",
}

// validateNoProxyCIDRs rejects any spec.proxy.noProxyCIDRs entry that would route
// the tenant's GitHub-bound traffic around the per-tenant egress proxy, defeating
// the egress-IP attribution that isolates tenants. Each entry is threaded
// verbatim into the AGC/worker NO_PROXY env var (builder.go buildNoProxy), where
// Go's httpproxy matches a hostname entry as a domain suffix. So an entry like
// "github.com" (or ".github.com", or an over-broad ".com") would silently
// exclude GitHub from the proxy — the documented footgun.
//
// The check is surgical, not a blanket "CIDRs only" rule: NO_PROXY legitimately
// takes domain-suffix entries for cluster-internal destinations (the GMC's own
// default appends svc.cluster.local/localhost, and tenants reach in-cluster
// services that way), so forbidding all hostnames would break a supported,
// load-bearing pattern. Only entries that NO_PROXY-match the GitHub hosts the
// tenant registers against are rejected. A CIDR/IP entry is allowed through here
// even if it happens to cover GitHub's published ranges — those ranges rotate
// and an in-tree IP blocklist would rot into a false sense of safety; that
// residual is the operator's responsibility (see 05-security.md §5.2).
//
// This is a webhook check, not a CRD CEL rule, because it depends on the
// gitHubURL host parse and is version-agnostic (mirroring validateGitHubAppRef).
func validateNoProxyCIDRs(ag *gmcv1alpha1.ActionsGateway) error {
	protected := append([]string{}, githubNoProxyHosts...)
	if u, err := url.Parse(ag.Spec.GitHubURL); err == nil && u.Hostname() != "" {
		protected = append(protected, u.Hostname())
	}
	for i, entry := range ag.Spec.Proxy.NoProxyCIDRs {
		// CIDR / bare-IP entries cannot be a hostname-suffix bypass; the
		// IP-range residual is accepted and documented.
		if _, err := netip.ParsePrefix(entry); err == nil {
			continue
		}
		if _, err := netip.ParseAddr(entry); err == nil {
			continue
		}
		for _, host := range protected {
			if noProxyHostnameMatches(entry, host) {
				return fmt.Errorf(
					"proxy.noProxyCIDRs[%d]: %q would route GitHub traffic (%s) around the per-tenant egress proxy, "+
						"defeating egress-IP attribution; remove it — GitHub must always traverse the proxy. "+
						"noProxyCIDRs may exclude internal destinations (CIDRs or domain suffixes), never GitHub",
					i, entry, host)
			}
		}
	}
	return nil
}

// noProxyHostnameMatches reports whether a NO_PROXY hostname entry would match
// the given host under Go's httpproxy domain-suffix semantics: an entry matches a
// host that equals it or is a sub-domain of it. A leading dot on the entry is
// insignificant for this purpose (".github.com" and "github.com" both match
// "api.github.com"); the comparison is case-insensitive.
func noProxyHostnameMatches(entry, host string) bool {
	e := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(entry), "."))
	h := strings.ToLower(host)
	if e == "" {
		return false
	}
	return h == e || strings.HasSuffix(h, "."+e)
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
