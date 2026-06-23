// Package migrate implements the one-shot v1alpha1 → v2alpha1 fan-out the M5
// migration tool performs (docs/design/appendix-h-v2-api-decomposition.md §H.11).
// A conversion webhook cannot express this migration — splitting one monolithic
// v1 ActionsGateway (with an inline proxy and bootstrap runner groups) into a v2
// ActionsGateway + EgressProxy + N RunnerTemplates + N RunnerSets is a fan-out on
// create, which converts one object into several siblings (§H.11). This package is
// the pure core: FanOut maps a typed v1 object set to the typed v2 object set plus
// the tenant-namespace metadata patch, with no I/O — the CLI (cmd/gmc/migrate)
// wraps it with cluster/file reads and dry-run/apply writes.
//
// The migration preserves behavior and weakens no security property (§H.17): the
// proxy stays proxied (never silent direct egress), maxListeners keeps its v1
// concurrency ceiling, identical templates collapse to one object, and the
// securityProfile relocates onto the namespace rather than being dropped. It never
// reads Secret contents — only the githubAppRef *name* is carried across.
package migrate

import (
	"fmt"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agcv1alpha1 "github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	gmcv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/api/v1alpha1"
)

// legacyTenantMarkerLabel is the v1 tenant-namespace marker key. It is duplicated
// here (the canonical definition is unexported in the GMC controller package) so
// the migration can detect a v1-marked namespace and add the aligned v2 marker
// (§H.12). Kept in sync with controller.TenantNamespaceMarkerLabel.
const legacyTenantMarkerLabel = "actions-gateway.github.com/tenant"

// legacyTenantMarkerValue is the grandfathered boolean-looking value of
// legacyTenantMarkerLabel (Q147); v2 aligns it to the "managed" enum keyword.
const legacyTenantMarkerValue = "true"

// Input is the v1alpha1 object set for one tenant namespace: the (at most one) v1
// ActionsGateway, the standalone RunnerGroup CRs the AGC serves in that namespace,
// and the namespace's own labels/annotations (read so the tool can relabel the
// tenant marker, downgrade annotation, and privileged-profile grant onto the v2
// domain). The caller assembles this from a live cluster or a manifest bundle.
type Input struct {
	// Namespace is the tenant namespace being migrated.
	Namespace string
	// NamespaceLabels and NamespaceAnnotations are the namespace's current metadata,
	// used to compute the additive v2 relabel patch.
	NamespaceLabels      map[string]string
	NamespaceAnnotations map[string]string
	// Gateway is the v1 ActionsGateway (identity + inline proxy + inline bootstrap
	// runner groups). Required: a namespace with no gateway has nothing to migrate.
	Gateway *gmcv1alpha1.ActionsGateway
	// RunnerGroups are the standalone RunnerGroup CRs present in the namespace — the
	// authoritative runtime set the AGC serves and what the GMC materializes the
	// gateway's inline runnerGroups[] into. FanOut unions these with any inline entry
	// that has no materialized standalone CR (standalone wins on a name collision).
	RunnerGroups []agcv1alpha1.RunnerGroup
}

// NamespacePatch is the additive set of labels/annotations the migration applies to
// the tenant namespace: the aligned v2 tenant marker, the relocated securityProfile,
// the domain-migrated privileged-profile grant, and the domain/value-aligned
// downgrade opt-in. It is additive — the v1 keys are kept so v1 keeps working during
// coexistence (the VAPs dual-read both), and dropped only when v1 is removed (§H.12).
type NamespacePatch struct {
	Name        string
	Labels      map[string]string
	Annotations map[string]string
}

// Result is the emitted v2alpha1 object set plus the namespace patch and any
// operator warnings (a truncated name, a privileged profile without an eligibility
// grant). Every emitted object name satisfies the v2 52-char cap so `--apply` is
// admitted.
type Result struct {
	Gateway        *v2alpha1.ActionsGateway
	Proxy          *v2alpha1.EgressProxy
	Templates      []*v2alpha1.RunnerTemplate
	Sets           []*v2alpha1.RunnerSet
	NamespacePatch *NamespacePatch
	Warnings       []string
}

// FanOut maps a v1 tenant object set to the v2 object set + namespace patch. It is
// pure (no I/O) and deterministic: templates are content-addressed and the emitted
// slices are sorted by name, so the same input always yields byte-identical output
// (the golden-test contract). An error is returned only for a structurally
// unmigratable input (no gateway); per-field issues surface as Warnings.
func FanOut(in Input) (*Result, error) {
	if in.Gateway == nil {
		return nil, fmt.Errorf("namespace %q has no ActionsGateway to migrate", in.Namespace)
	}
	gw := in.Gateway
	ns := gw.Namespace
	res := &Result{}

	gatewayName, truncated := cap52(gw.Name)
	if truncated {
		res.Warnings = append(res.Warnings, fmt.Sprintf(
			"ActionsGateway name %q exceeds the v2 52-char cap; emitted as %q", gw.Name, gatewayName))
	}

	// 1. EgressProxy from the inline proxy. The v1 proxy is always required, so the
	// tool always emits a proxy and always wires defaultProxyRef (§H.17 invariant 1:
	// a migrated tenant stays proxied, never silent direct egress).
	proxyName, proxyTrunc := egressProxyName(gatewayName)
	if proxyTrunc {
		res.Warnings = append(res.Warnings, fmt.Sprintf(
			"EgressProxy name derived from gateway %q exceeds the v2 52-char cap; emitted as %q", gw.Name, proxyName))
	}
	res.Proxy = buildEgressProxy(ns, proxyName, gw.Spec.Proxy)

	// 2. v2 ActionsGateway (identity only). defaultProxyRef points at the emitted
	// proxy so every RunnerSet that leaves proxyRef unset inherits it.
	res.Gateway = buildGateway(ns, gatewayName, proxyName, gw.Spec)

	// 3. Authoritative runner-group set: standalone CRs, unioned with any inline
	// bootstrap entry not yet materialized to a standalone CR (standalone wins).
	groups := authoritativeGroups(gw, in.RunnerGroups)

	// 4. RunnerTemplates (reuse-deduped) + RunnerSets. The template name is a pure
	// function of (podTemplate, workerImage), so K identical templates collapse to
	// one object by construction (§H.17 invariant 2).
	seenTemplate := map[string]*v2alpha1.RunnerTemplate{}
	for i := range groups {
		rg := &groups[i]
		tmplSpec := v2alpha1.RunnerTemplateSpec{
			PodTemplate: rg.Spec.PodTemplate,
			WorkerImage: rg.Spec.WorkerImage,
		}
		tmplName, err := templateName(tmplSpec)
		if err != nil {
			return nil, fmt.Errorf("RunnerGroup %q: %w", rg.Name, err)
		}
		if _, ok := seenTemplate[tmplName]; !ok {
			seenTemplate[tmplName] = buildRunnerTemplate(ns, tmplName, tmplSpec)
		}

		setName, setTrunc := cap52(rg.Name)
		if setTrunc {
			res.Warnings = append(res.Warnings, fmt.Sprintf(
				"RunnerGroup name %q exceeds the v2 52-char cap; emitted RunnerSet as %q", rg.Name, setName))
		}
		res.Sets = append(res.Sets, buildRunnerSet(ns, setName, gatewayName, tmplName, rg.Spec))
	}
	for _, t := range seenTemplate {
		res.Templates = append(res.Templates, t)
	}
	// Deterministic ordering for stable (golden-testable) output.
	sort.Slice(res.Templates, func(i, j int) bool { return res.Templates[i].Name < res.Templates[j].Name })
	sort.Slice(res.Sets, func(i, j int) bool { return res.Sets[i].Name < res.Sets[j].Name })

	// 5. Namespace patch: securityProfile relocation + Q147/domain alignment.
	res.NamespacePatch = buildNamespacePatch(in, gw, &res.Warnings)

	return res, nil
}

// MostRestrictiveProfile returns the most-restrictive of the given securityProfile
// values, substituting the baseline default for any empty value. It is the
// most-restrictive-wins rule for the (defensive) case where a namespace holds more
// than one v1 ActionsGateway with disagreeing profiles: v2's profile is
// namespace-scoped, so the migration must pick one, and it must never weaken a
// tenant's posture — so the strictest wins. v1's one-gateway-per-namespace rule means
// this normally collapses to a single profile, but the tool handles it safely
// regardless (the task's explicit requirement). An empty input returns baseline.
func MostRestrictiveProfile(profiles ...string) string {
	best := v2alpha1.SecurityProfileBaseline
	bestRank := v2alpha1.SecurityProfileRank[best]
	for _, p := range profiles {
		eff := v2alpha1.EffectiveSecurityProfile(p)
		if r, ok := v2alpha1.SecurityProfileRank[eff]; ok && r > bestRank {
			best, bestRank = eff, r
		}
	}
	return best
}

// authoritativeGroups returns the runner groups to migrate: the standalone CRs,
// plus any inline ActionsGateway.spec.runnerGroups[] entry whose v1 derived name is
// not already present as a standalone CR. Standalone CRs win on a name collision
// because they are the live objects the AGC serves and the GMC reconciles inline
// entries into; v1 never reconciled the two representations, so the migration makes
// the merge explicit (§H.17). The returned slice is sorted by name for determinism.
func authoritativeGroups(gw *gmcv1alpha1.ActionsGateway, standalone []agcv1alpha1.RunnerGroup) []agcv1alpha1.RunnerGroup {
	byName := map[string]agcv1alpha1.RunnerGroup{}
	for _, rg := range standalone {
		byName[rg.Name] = rg
	}
	for i, spec := range gw.Spec.RunnerGroups {
		name := runnerGroupName(gw.Name, spec, i)
		if _, ok := byName[name]; ok {
			continue // already materialized as a standalone CR — standalone wins
		}
		byName[name] = agcv1alpha1.RunnerGroup{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: gw.Namespace},
			Spec:       spec,
		}
	}
	out := make([]agcv1alpha1.RunnerGroup, 0, len(byName))
	for _, rg := range byName {
		out = append(out, rg)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// runnerGroupName replicates the GMC's v1 derived name for an inline runnerGroups[]
// entry (controller.runnerGroupName): a content-derived name from the first runner
// label, or an index-based fallback. Replicated (not imported — it is unexported in
// the controller package) so the synthesized standalone name matches what the GMC
// would have materialized, making the standalone-vs-inline dedup exact.
func runnerGroupName(gatewayName string, spec agcv1alpha1.RunnerGroupSpec, i int) string {
	if len(spec.RunnerLabels) > 0 {
		return fmt.Sprintf("%s-%s", gatewayName, labelSafe(spec.RunnerLabels[0]))
	}
	return fmt.Sprintf("%s-%d", gatewayName, i)
}

// labelSafe replicates controller.labelSafe: a deterministic, RFC-1123-label-safe
// segment derived from an arbitrary runner label, suffixed with a 7-hex hash for
// uniqueness. Kept byte-for-byte identical so a synthesized inline-group name equals
// the standalone CR the GMC materialized.
func labelSafe(s string) string {
	hash := shortHash(s, 7)
	out := make([]byte, 0, len(s))
	for _, c := range []byte(s) {
		switch {
		case (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-':
			out = append(out, c)
		case c >= 'A' && c <= 'Z':
			out = append(out, c+32)
		default:
			out = append(out, '-')
		}
	}
	seg := strings.Trim(string(out), "-")
	if len(seg) > 40 {
		seg = strings.TrimRight(seg[:40], "-")
	}
	if seg == "" {
		seg = "label"
	}
	return seg + "-" + hash
}

// buildNamespacePatch computes the additive v2 namespace metadata: the relocated
// securityProfile label, the aligned tenant marker, the domain-migrated
// privileged-profile grant, and the domain/value-aligned downgrade opt-in. It never
// removes a v1 key — the v1 markers keep v1 working during coexistence and the VAPs
// dual-read both (§H.12) — so nothing is stranded mid-cutover.
func buildNamespacePatch(in Input, gw *gmcv1alpha1.ActionsGateway, warnings *[]string) *NamespacePatch {
	patch := &NamespacePatch{
		Name:        in.Namespace,
		Labels:      map[string]string{},
		Annotations: map[string]string{},
	}

	// Tenant marker alignment (Q147): add the v2 keyword marker when the namespace
	// is a managed v1 tenant. Additive — the v1 marker stays for coexistence.
	if in.NamespaceLabels[legacyTenantMarkerLabel] == legacyTenantMarkerValue {
		patch.Labels[v2alpha1.TenantNamespaceMarkerLabel] = v2alpha1.TenantNamespaceMarkerValue
	}

	// securityProfile relocation (Q175, §H.16 #7): v1 hung the profile on the
	// per-gateway spec; v2 owns it at the namespace. Most-restrictive-wins across the
	// namespace's gateways (v1's one-per-namespace rule means usually one). The label
	// is always set — including baseline — so the posture is explicit, never silently
	// dropped or downgraded to the default by omission.
	profile := v2alpha1.EffectiveSecurityProfile(gw.Spec.SecurityProfile)
	patch.Labels[v2alpha1.SecurityProfileLabel] = profile

	// Privileged eligibility: carry the platform grant forward onto the v2 domain
	// when the namespace already holds it. The migration never invents the grant —
	// it only domain-migrates an existing platform decision (the namespace label a
	// platform admin applied for v1 privileged). If the migrated profile is privileged
	// but the namespace holds no grant, warn: the v2 namespace-security-profile-guard
	// VAP will reject the profile label until a platform admin grants eligibility.
	privGranted := in.NamespaceLabels[gmcv1alpha1.PrivilegedProfileLabel] == gmcv1alpha1.PrivilegedProfileAllowed
	if privGranted {
		patch.Labels[v2alpha1.PrivilegedProfileLabel] = v2alpha1.PrivilegedProfileAllowed
	}
	if profile == v2alpha1.SecurityProfilePrivileged && !privGranted {
		*warnings = append(*warnings, fmt.Sprintf(
			"namespace %q migrates to securityProfile=privileged but holds no %s=%s grant; "+
				"a platform administrator must apply the v2 eligibility label or the profile will be rejected",
			in.Namespace, v2alpha1.PrivilegedProfileLabel, v2alpha1.PrivilegedProfileAllowed))
	}

	// Downgrade opt-in alignment (Q147): if the v1 annotation is present, add the v2
	// domain/value-aligned form. Additive — both are dual-read during coexistence.
	if in.NamespaceAnnotations[gmcv1alpha1.AllowProfileDowngradeAnnotation] == "true" {
		patch.Annotations[v2alpha1.AllowProfileDowngradeAnnotation] = v2alpha1.AllowProfileDowngradeAllowed
	}

	if len(patch.Annotations) == 0 {
		patch.Annotations = nil
	}
	return patch
}

// buildGateway assembles the v2 ActionsGateway: identity only (the inline proxy and
// runner groups are fanned out to sibling objects). defaultProxyRef wires the
// emitted EgressProxy so RunnerSets inherit it and stay proxied. The v2
// securityProfile is NOT a gateway field (it relocates to the namespace), and the
// v1 SecretReference.namespace is dropped (v2 LocalSecretReference is name-only).
func buildGateway(ns, name, proxyName string, spec gmcv1alpha1.ActionsGatewaySpec) *v2alpha1.ActionsGateway {
	return &v2alpha1.ActionsGateway{
		TypeMeta: metav1.TypeMeta{
			APIVersion: v2alpha1.GroupVersion.String(),
			Kind:       "ActionsGateway",
		},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: v2alpha1.ActionsGatewaySpec{
			GitHubAppRef:    v2alpha1.LocalSecretReference{Name: spec.GitHubAppRef.Name},
			GitHubURL:       spec.GitHubURL,
			DefaultProxyRef: &v2alpha1.LocalObjectRef{Name: proxyName},
			LogLevel:        spec.LogLevel,
			Tracing:         translateTracing(spec.Tracing),
		},
	}
}

// buildEgressProxy assembles the standalone EgressProxy from the v1 inline proxy
// config. Every tunable carries across unchanged in meaning (§H.4); the sharing
// field stays nil (same-namespace only, the v1 behavior).
func buildEgressProxy(ns, name string, p gmcv1alpha1.ProxyConfig) *v2alpha1.EgressProxy {
	return &v2alpha1.EgressProxy{
		TypeMeta: metav1.TypeMeta{
			APIVersion: v2alpha1.GroupVersion.String(),
			Kind:       "EgressProxy",
		},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: v2alpha1.EgressProxySpec{
			MinReplicas:                    p.MinReplicas,
			MaxReplicas:                    p.MaxReplicas,
			TargetCPUUtilizationPercentage: p.TargetCPUUtilizationPercentage,
			Resources:                      p.Resources,
			NoProxyCIDRs:                   p.NoProxyCIDRs,
			ManagedNetworkPolicy:           p.ManagedNetworkPolicy,
		},
	}
}

// buildRunnerTemplate wraps a RunnerTemplateSpec into a named, namespaced
// RunnerTemplate. Pure data: nothing owns it and it owns nothing (§H.8).
func buildRunnerTemplate(ns, name string, spec v2alpha1.RunnerTemplateSpec) *v2alpha1.RunnerTemplate {
	return &v2alpha1.RunnerTemplate{
		TypeMeta: metav1.TypeMeta{
			APIVersion: v2alpha1.GroupVersion.String(),
			Kind:       "RunnerTemplate",
		},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       spec,
	}
}

// buildRunnerSet assembles a v2 RunnerSet from a v1 RunnerGroup. The scheduling and
// lifecycle knobs carry across unchanged; the pod shape moves to templateRef and the
// gateway binding to gatewayRef. proxyRef is left unset so the set inherits the
// gateway's defaultProxyRef (proxied, never direct — §H.17 invariant 1). maxListeners
// is pinned to the v1 effective value (1 when v1 omitted it) so the migration
// preserves the v1 concurrency ceiling rather than inheriting v2's default of 10.
func buildRunnerSet(ns, name, gatewayName, templateName string, spec agcv1alpha1.RunnerGroupSpec) *v2alpha1.RunnerSet {
	maxListeners := spec.MaxListeners
	if maxListeners == 0 {
		// v1 unset defaults to 1; preserve that ceiling explicitly.
		maxListeners = 1
	}
	return &v2alpha1.RunnerSet{
		TypeMeta: metav1.TypeMeta{
			APIVersion: v2alpha1.GroupVersion.String(),
			Kind:       "RunnerSet",
		},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: v2alpha1.RunnerSetSpec{
			GatewayRef:         v2alpha1.ObjectRef{Name: gatewayName},
			TemplateRef:        &v2alpha1.ObjectRef{Name: templateName},
			MaxListeners:       maxListeners,
			MaxWorkers:         spec.MaxWorkers,
			RunnerLabels:       spec.RunnerLabels,
			PriorityTiers:      translatePriorityTiers(spec.PriorityTiers),
			MaxEvictionRetries: spec.MaxEvictionRetries,
			EvictionRetryDelay: spec.EvictionRetryDelay,
			MaxQuotaRetries:    spec.MaxQuotaRetries,
			QuotaRetryDelay:    spec.QuotaRetryDelay,
			CompletedPodTTL:    spec.CompletedPodTTL,
			PendingPodDeadline: spec.PendingPodDeadline,
		},
	}
}

// translatePriorityTiers maps the v1 (agc-group) PriorityTier slice onto the v2
// (neutral-module) PriorityTier slice. The two are field-identical; they differ only
// by Go package, so the migration copies field-by-field. Returns nil for an empty
// input so the emitted spec omits the field rather than carrying an empty slice.
func translatePriorityTiers(in []agcv1alpha1.PriorityTier) []v2alpha1.PriorityTier {
	if len(in) == 0 {
		return nil
	}
	out := make([]v2alpha1.PriorityTier, len(in))
	for i, t := range in {
		out[i] = v2alpha1.PriorityTier{
			PriorityClassName: t.PriorityClassName,
			Threshold:         t.Threshold,
		}
	}
	return out
}

// translateTracing maps the v1 (gmc-group) TracingConfig onto the v2 TracingConfig.
// Field-identical across the rename; copied field-by-field.
func translateTracing(in gmcv1alpha1.TracingConfig) v2alpha1.TracingConfig {
	return v2alpha1.TracingConfig{
		Endpoint:           in.Endpoint,
		Insecure:           in.Insecure,
		Sampler:            in.Sampler,
		SamplerArg:         in.SamplerArg,
		ResourceAttributes: in.ResourceAttributes,
	}
}
