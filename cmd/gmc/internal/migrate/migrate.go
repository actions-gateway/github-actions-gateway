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
	"strconv"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agcv1alpha1 "github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	"github.com/actions-gateway/github-actions-gateway/api/apinames"
	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	gmcv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/api/v1alpha1"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/webhook/validation"
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
	Gateway   *v2alpha1.ActionsGateway
	Proxy     *v2alpha1.EgressProxy
	Templates []*v2alpha1.RunnerTemplate
	// ClusterTemplates are the cluster-scoped templates emitted for privileged
	// (DinD/sysbox) worker shapes, which the namespaced kind's admission webhook
	// refuses (§H.6). Empty for every tenant without a privileged podTemplate — the
	// ordinary case — so a non-privileged migration is unchanged.
	ClusterTemplates []*v2alpha1.ClusterRunnerTemplate
	Sets             []*v2alpha1.RunnerSet
	NamespacePatch   *NamespacePatch
	Warnings         []string
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
	res.Proxy = buildEgressProxy(ns, proxyName, gw.Spec.Proxy, gw.Spec.LogLevel)

	// 2. v2 ActionsGateway (identity only). defaultProxyRef points at the emitted
	// proxy so every RunnerSet that leaves proxyRef unset inherits it.
	res.Gateway = buildGateway(ns, gatewayName, proxyName, gw.Spec)

	// 3. Authoritative runner-group set: standalone CRs, unioned with any inline
	// bootstrap entry not yet materialized to a standalone CR (standalone wins).
	groups := authoritativeGroups(gw, in.RunnerGroups)

	// 4. Templates (reuse-deduped) + RunnerSets. The template name is a pure function
	// of (podTemplate, workerImage), so K identical templates collapse to one object
	// by construction (§H.17 invariant 2). A privileged worker shape lands on the
	// cluster-scoped kind instead — see templateRefFor.
	seenTemplate := map[string]*v2alpha1.RunnerTemplate{}
	seenClusterTemplate := map[string]*v2alpha1.ClusterRunnerTemplate{}
	for i := range groups {
		rg := &groups[i]
		tmplSpec := v2alpha1.RunnerTemplateSpec{
			PodTemplate: rg.Spec.PodTemplate,
			WorkerImage: rg.Spec.WorkerImage,
		}
		ref, err := templateRefFor(ns, rg, tmplSpec, seenTemplate, seenClusterTemplate, &res.Warnings)
		if err != nil {
			return nil, err
		}

		setName, setTrunc := cap52(rg.Name)
		if setTrunc {
			res.Warnings = append(res.Warnings, fmt.Sprintf(
				"RunnerGroup name %q exceeds the v2 52-char cap; emitted RunnerSet as %q", rg.Name, setName))
		}
		res.Sets = append(res.Sets, buildRunnerSet(ns, setName, gatewayName, ref, rg.Spec))
	}
	for _, t := range seenTemplate {
		res.Templates = append(res.Templates, t)
	}
	for _, t := range seenClusterTemplate {
		res.ClusterTemplates = append(res.ClusterTemplates, t)
	}
	// Deterministic ordering for stable (golden-testable) output.
	sort.Slice(res.Templates, func(i, j int) bool { return res.Templates[i].Name < res.Templates[j].Name })
	sort.Slice(res.ClusterTemplates, func(i, j int) bool { return res.ClusterTemplates[i].Name < res.ClusterTemplates[j].Name })
	sort.Slice(res.Sets, func(i, j int) bool { return res.Sets[i].Name < res.Sets[j].Name })

	// 5. Namespace patch: securityProfile relocation + Q147/domain alignment.
	res.NamespacePatch = buildNamespacePatch(in, gw, &res.Warnings)

	return res, nil
}

// templateRefFor emits the template object for one runner group — into the caller's
// dedup maps — and returns the RunnerSet templateRef that points at it.
//
// The kind is chosen by the pod shape, not by the operator. A v1 podTemplate carrying
// a privileged container cannot become a namespaced RunnerTemplate: that kind's
// admission webhook rejects privileged containers outright, because a TENANT must not
// self-author a privileged worker shape. The cluster-scoped ClusterRunnerTemplate is
// the kind that exists to hold exactly these golden DinD/sysbox shapes (§H.4/§H.6),
// and it is the right destination here because `gag-migrate` is run by a platform
// administrator — the same role that authors a ClusterRunnerTemplate by hand — not by
// a tenant. Without this split the tool emits a template the apiserver refuses,
// failing `--apply` after the EgressProxy is already created and leaving the namespace
// half-migrated (Q414).
//
// This weakens nothing. Pod Security Admission, stamped from the namespace's
// securityProfile, is the runtime backstop for BOTH template kinds (§H.4), and the
// privileged profile it enforces is gated by the platform's privileged-profile grant —
// which buildNamespacePatch carries forward from v1 and never invents. A privileged
// tenant therefore migrates under exactly the grant it already held.
//
// Every emitted cluster template produces a warning: a namespace-scoped migration
// creating a cluster-scoped object is a blast-radius change the operator must see in
// the dry-run before approving --apply, and namespace deletion will not clean it up.
func templateRefFor(
	ns string,
	rg *agcv1alpha1.RunnerGroup,
	spec v2alpha1.RunnerTemplateSpec,
	seen map[string]*v2alpha1.RunnerTemplate,
	seenCluster map[string]*v2alpha1.ClusterRunnerTemplate,
	warnings *[]string,
) (*v2alpha1.ObjectRef, error) {
	if !hasPrivilegedContainer(&spec) {
		name, err := templateName(spec)
		if err != nil {
			return nil, fmt.Errorf("RunnerGroup %q: %w", rg.Name, err)
		}
		if _, ok := seen[name]; !ok {
			seen[name] = buildRunnerTemplate(ns, name, spec)
		}
		return &v2alpha1.ObjectRef{Name: name}, nil
	}

	name, truncated, err := clusterTemplateName(ns, spec)
	if err != nil {
		return nil, fmt.Errorf("RunnerGroup %q: %w", rg.Name, err)
	}
	if truncated {
		*warnings = append(*warnings, fmt.Sprintf(
			"ClusterRunnerTemplate name derived from namespace %q exceeds the v2 52-char cap; emitted as %q", ns, name))
	}
	if _, ok := seenCluster[name]; !ok {
		seenCluster[name] = buildClusterRunnerTemplate(ns, name, spec)
		*warnings = append(*warnings, fmt.Sprintf(
			"RunnerGroup %q has a privileged worker pod shape, so it migrates to the CLUSTER-SCOPED "+
				"ClusterRunnerTemplate %q rather than a namespaced RunnerTemplate (a namespaced template may not "+
				"declare privileged containers). Applying it requires cluster-scoped create permission, and deleting "+
				"namespace %q will NOT remove it — tear it down explicitly, or find it again with "+
				"`kubectl get clusterrunnertemplates -l %s=%s`",
			rg.Name, name, ns, v2alpha1.MigratedFromNamespaceLabel, ns))
	}
	return &v2alpha1.ObjectRef{Name: name, Kind: "ClusterRunnerTemplate"}, nil
}

// hasPrivilegedContainer reports whether any container or init container in the
// worker pod shape explicitly requests privileged. It mirrors the predicate the v2
// RunnerTemplate webhook rejects on (webhook/v2alpha1.validateReservedPodFields), so
// the migration's kind choice and admission's verdict cannot disagree. Init containers
// count: a DinD sidecar is declared as a native sidecar — a restartPolicy: Always init
// container — and that is where the privileged flag sits.
func hasPrivilegedContainer(spec *v2alpha1.RunnerTemplateSpec) bool {
	for _, list := range [][]corev1.Container{
		spec.PodTemplate.Spec.Containers,
		spec.PodTemplate.Spec.InitContainers,
	} {
		for i := range list {
			sc := list[i].SecurityContext
			if sc != nil && sc.Privileged != nil && *sc.Privileged {
				return true
			}
		}
	}
	return false
}

// MostRestrictiveProfile returns the most-restrictive of the given securityProfile
// values, substituting the baseline default for any empty value. It is the
// most-restrictive-wins rule for the (defensive) case where a namespace holds more
// than one v1 ActionsGateway with disagreeing profiles: v2's profile is
// namespace-scoped, so the migration must pick one, and it must never weaken a
// tenant's posture — so the strictest wins. v1's one-gateway-per-namespace rule means
// this normally collapses to a single profile, but the tool handles it safely
// regardless (the task's explicit requirement). An empty input returns baseline.
//
// The seed is the FIRST profile, not an unconditional baseline (Q414):
// `privileged` ranks BELOW `baseline`, so a baseline seed could never be beaten
// by a lone privileged tenant, silently migrating it to a profile under which
// Pod Security Admission refuses its own DinD workers. Seeding from the input
// leaves every genuine multi-gateway comparison unchanged — the strictest still
// wins — while a single gateway migrates to exactly its own profile.
func MostRestrictiveProfile(profiles ...string) string {
	best := v2alpha1.SecurityProfileBaseline
	bestRank, seeded := v2alpha1.SecurityProfileRank[best], false
	for _, p := range profiles {
		eff := v2alpha1.EffectiveSecurityProfile(p)
		r, ok := v2alpha1.SecurityProfileRank[eff]
		if !ok {
			continue
		}
		if !seeded || r > bestRank {
			best, bestRank, seeded = eff, r, true
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
// label, or an index-based fallback, bounded to the 63-char label-value budget.
// Replicated (not imported — it is unexported in the controller package) so the
// synthesized standalone name matches what the GMC would have materialized, making
// the standalone-vs-inline dedup exact. Both sides now derive through apinames, so
// that equality is enforced by the shared helper rather than by keeping two copies
// in step by hand.
func runnerGroupName(gatewayName string, spec agcv1alpha1.RunnerGroupSpec, i int) string {
	if len(spec.RunnerLabels) > 0 {
		return apinames.Join(apinames.MaxLabelValue, gatewayName, labelSafe(spec.RunnerLabels[0]))
	}
	return apinames.Join(apinames.MaxLabelValue, gatewayName, strconv.Itoa(i))
}

// labelSafe is controller.labelSafe: a deterministic, RFC-1123-label-safe segment
// derived from an arbitrary runner label, suffixed with a 7-hex hash for uniqueness.
// The two must agree exactly or a synthesized inline-group name would not equal the
// standalone CR the GMC materialized, so both call the one shared implementation.
func labelSafe(s string) string { return apinames.Segment(s, "label") }

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
	//
	// The grant is read across BOTH label domains, exactly as the v1 webhook admits it
	// (§H.12, Q463): a namespace granted on the v2 domain alone is legal and admitted
	// privileged, so reading only v1 would report a live grant as missing and prescribe
	// a label the operator already holds — an unactionable warning at the worst moment.
	privGranted := validation.PrivilegedGrantPresent(in.NamespaceLabels)
	if privGranted {
		patch.Labels[v2alpha1.PrivilegedProfileLabel] = v2alpha1.PrivilegedProfileAllowed
	}
	if profile == v2alpha1.SecurityProfilePrivileged && !privGranted {
		*warnings = append(*warnings, fmt.Sprintf(
			"namespace %q migrates to securityProfile=privileged but holds no privileged-eligibility grant on "+
				"either label domain (neither %s=%s nor %s=%s); a platform administrator must apply %s=%s "+
				"or the profile will be rejected",
			in.Namespace,
			gmcv1alpha1.PrivilegedProfileLabel, gmcv1alpha1.PrivilegedProfileAllowed,
			v2alpha1.PrivilegedProfileLabel, v2alpha1.PrivilegedProfileAllowed,
			v2alpha1.PrivilegedProfileLabel, v2alpha1.PrivilegedProfileAllowed))
	}

	// Downgrade opt-in alignment (Q147): if the v1 annotation is present, add the v2
	// domain/value-aligned form. Additive — both are dual-read during coexistence.
	if in.NamespaceAnnotations[gmcv1alpha1.AllowProfileDowngradeAnnotation] == "true" {
		patch.Annotations[v2alpha1.AllowProfileDowngradeAnnotation] = v2alpha1.AllowProfileDowngradeAllowed
	}

	warnIfDowngradeGuardWillReject(in, profile, patch, warnings)

	if len(patch.Annotations) == 0 {
		patch.Annotations = nil
	}
	return patch
}

// warnIfDowngradeGuardWillReject warns when the namespace-security-profile-guard
// ValidatingAdmissionPolicy will reject the relocation patch as a profile downgrade.
//
// This is not hypothetical, and it hits the DinD case every time (Q414). The guard
// compares the incoming securityProfile label against the namespace's current one, and
// an ABSENT label reads as the baseline default. A tenant migrating from v1 has never
// carried the v2 label, so relocating `privileged` — which ranks BELOW baseline, being
// the least restrictive level — always presents as baseline→privileged, i.e. a
// downgrade, and is denied without the opt-in annotation.
//
// The apparent downgrade is an artifact of the label being new, not a real weakening:
// the namespace's Pod Security Admission enforcement was ALREADY privileged under v1,
// stamped there by the GMC from the gateway's spec. But the guard cannot see that, and
// it must not: it is the control that stops a stray re-apply from silently relaxing a
// tenant's isolation.
//
// So the tool warns rather than self-granting. Writing the opt-in annotation here would
// be the tool inventing a security decision on the operator's behalf — the same thing
// the privileged-eligibility grant is deliberately never invented for. The operator adds
// the annotation, migrates, and removes it; the runbook spells out that sequence.
func warnIfDowngradeGuardWillReject(in Input, profile string, patch *NamespacePatch, warnings *[]string) {
	current := v2alpha1.EffectiveSecurityProfile(in.NamespaceLabels[v2alpha1.SecurityProfileLabel])
	if v2alpha1.SecurityProfileRank[profile] >= v2alpha1.SecurityProfileRank[current] {
		return // not a downgrade — the guard has nothing to object to
	}
	// The opt-in may already be on the namespace, or be arriving in this same patch
	// (carried forward from the v1 annotation). Either satisfies the guard, because the
	// patch sets labels and annotations in ONE write that the policy evaluates whole.
	if patch.Annotations[v2alpha1.AllowProfileDowngradeAnnotation] == v2alpha1.AllowProfileDowngradeAllowed ||
		in.NamespaceAnnotations[v2alpha1.AllowProfileDowngradeAnnotation] == v2alpha1.AllowProfileDowngradeAllowed {
		return
	}
	*warnings = append(*warnings, fmt.Sprintf(
		"relocating securityProfile %q onto namespace %q reads as a downgrade from %q (an absent %s label "+
			"defaults to baseline, and %q is less restrictive), so the namespace-security-profile-guard policy "+
			"will REJECT the namespace patch. This does not weaken the tenant — its Pod Security Admission level "+
			"is already %q under v1 — but the opt-in is the operator's to give, not this tool's. Before --apply, run: "+
			"kubectl annotate namespace %s %s=%s   (remove it once the migration is verified)",
		profile, in.Namespace, current, v2alpha1.SecurityProfileLabel, profile, profile,
		in.Namespace, v2alpha1.AllowProfileDowngradeAnnotation, v2alpha1.AllowProfileDowngradeAllowed))
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
			Credentials: v2alpha1.GitHubCredentials{
				Type:      v2alpha1.CredentialTypeGitHubApp,
				GitHubApp: &v2alpha1.LocalSecretReference{Name: spec.GitHubAppRef.Name},
			},
			GitHubURL:       spec.GitHubURL,
			DefaultProxyRef: &v2alpha1.ProxyObjectRef{Name: proxyName},
			LogLevel:        spec.LogLevel,
			Tracing:         translateTracing(spec.Tracing),
		},
	}
}

// buildEgressProxy assembles the standalone EgressProxy from the v1 inline proxy
// config. Every tunable carries across unchanged in meaning (§H.4); the sharing
// field stays nil (same-namespace only, the v1 behavior). logLevel is the v1
// gateway-level knob: in v1 it governed both the AGC and the inline proxy, so the
// emitted pool inherits it (Q327) — otherwise a tenant migrated mid-repro would
// silently drop from debug back to info on the proxy side.
func buildEgressProxy(ns, name string, p gmcv1alpha1.ProxyConfig, logLevel string) *v2alpha1.EgressProxy {
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
			LogLevel:                       logLevel,
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

// buildClusterRunnerTemplate wraps a RunnerTemplateSpec into a named, cluster-scoped
// ClusterRunnerTemplate — the destination for a privileged (DinD/sysbox) v1 worker
// shape (see templateRefFor). Identical to buildRunnerTemplate but for the kind, the
// absent namespace (the kind is cluster-scoped), and the provenance label recording
// which tenant namespace the shape came from: this is the one migration output that
// namespace deletion does not garbage-collect, so an operator needs a way to find it.
func buildClusterRunnerTemplate(sourceNS, name string, spec v2alpha1.RunnerTemplateSpec) *v2alpha1.ClusterRunnerTemplate {
	return &v2alpha1.ClusterRunnerTemplate{
		TypeMeta: metav1.TypeMeta{
			APIVersion: v2alpha1.GroupVersion.String(),
			Kind:       "ClusterRunnerTemplate",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{v2alpha1.MigratedFromNamespaceLabel: sourceNS},
		},
		Spec: spec,
	}
}

// buildRunnerSet assembles a v2 RunnerSet from a v1 RunnerGroup. The scheduling and
// lifecycle knobs carry across unchanged; the pod shape moves to templateRef and the
// gateway binding to gatewayRef. proxyRef is left unset so the set inherits the
// gateway's defaultProxyRef (proxied, never direct — §H.17 invariant 1). maxListeners
// is pinned to the v1 effective value (1 when v1 omitted it) so the migration
// preserves the v1 concurrency ceiling rather than inheriting v2's default of 10.
//
// templateRef is supplied by templateRefFor, which also picks its kind: a namespaced
// RunnerTemplate for an ordinary shape, or a cluster-scoped ClusterRunnerTemplate for
// a privileged one.
func buildRunnerSet(ns, name, gatewayName string, templateRef *v2alpha1.ObjectRef, spec agcv1alpha1.RunnerGroupSpec) *v2alpha1.RunnerSet {
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
			GatewayRef:  v2alpha1.ObjectRef{Name: gatewayName},
			TemplateRef: templateRef,
			// Pin Classic explicitly: the v2alpha1 default is ScaleSet (Q264 P5), but a
			// migrated v1 group carries classic-protocol semantics and may declare
			// multiple runnerLabels — which ScaleSet forbids (one label == the scale-set
			// name). Emitting Classic keeps the migrated set byte-for-byte equivalent to
			// the v1 group and lets the tenant opt into ScaleSet later on a fresh set
			// (§5a-U7). Without this, a multi-label group would default to ScaleSet and be
			// rejected by admission on apply.
			AcquisitionProtocol: v2alpha1.AcquisitionProtocolClassic,
			MaxListeners:        maxListeners,
			MaxWorkers:          spec.MaxWorkers,
			RunnerLabels:        spec.RunnerLabels,
			PriorityTiers:       translatePriorityTiers(spec.PriorityTiers),
			MaxEvictionRetries:  spec.MaxEvictionRetries,
			EvictionRetryDelay:  spec.EvictionRetryDelay,
			MaxQuotaRetries:     spec.MaxQuotaRetries,
			QuotaRetryDelay:     spec.QuotaRetryDelay,
			CompletedPodTTL:     spec.CompletedPodTTL,
			PendingPodDeadline:  spec.PendingPodDeadline,
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
