package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/listener"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// egressProxyResourceSuffix / egressProxyTLSSuffix derive an EgressProxy's
	// child Service and TLS-Secret names from its object name. They mirror the GMC
	// EgressProxy reconciler's derivation (cmd/gmc/internal/controller/egressproxy_builder.go):
	// the AGC wires worker egress to "<ep>-proxy.<ns>.svc.cluster.local:8080" and
	// projects "<ep>-proxy-tls"'s public cert into worker pods, so the two modules
	// must agree on these stable name conventions (§H.8). Kept as local constants
	// rather than a shared import because they are a naming convention, not API.
	egressProxyResourceSuffix = "-proxy"
	egressProxyTLSSuffix      = "-proxy-tls"

	// proxyPort is the EgressProxy CONNECT/data port (matches the GMC's proxyPort).
	proxyPort = 8080

	// defaultNoProxy excludes cluster-internal traffic from the egress proxy so the
	// proxy is only used for external (GitHub) traffic. Mirrors the GMC builder's
	// defaultNoProxy; the GMC sets the AGC's own NO_PROXY from the same list, and
	// the AGC sets each worker's NO_PROXY here from the resolved EgressProxy.
	defaultNoProxy = "svc.cluster.local,localhost,127.0.0.1,10.96.0.0/12"
)

// egressProxyServiceName / egressProxyTLSSecretName derive an EgressProxy's child
// Service and TLS-Secret names. See egressProxyResourceSuffix.
func egressProxyServiceName(name string) string   { return name + egressProxyResourceSuffix }
func egressProxyTLSSecretName(name string) string { return name + egressProxyTLSSuffix }

// runnerSetTarget adapts a v2alpha1.RunnerSet to the provisioner Target seam. It
// owns worker pods via an OwnerReference to the real RunnerSet (a synthesized
// in-memory RunnerGroup would have a dangling owner-ref the apiserver GCs), and
// resolves the RunnerSet's RunnerTemplate (pod shape) and EgressProxy (worker
// egress) on every acquired job so reference edits take effect without a restart
// (Q117) and a reference that stops resolving fails the job fail-closed (§H.7).
type runnerSetTarget struct {
	client client.Client
	// prov supplies the AGC-wide provisioning defaults (eviction/quota tunables)
	// and the namespace's effective PSA profile (set process-wide from the
	// SECURITY_PROFILE env the GMC stamps from the namespace security-profile
	// label — PSA is namespace-scoped in v2, so every set shares one profile).
	prov *provisioner.Provisioner
	key  client.ObjectKey
	uid  types.UID
	// events routes owner-scoped provisioning Events (quota/eviction-retry exhaustion)
	// to the RunnerSet reconciler's event channel. Distinct from the v1 path's
	// Provisioner.Events because one Provisioner is shared across both owners. Nil
	// disables event recording.
	events listener.EventRecorder
}

// Key returns the RunnerSet's namespace/name.
func (t *runnerSetTarget) Key() client.ObjectKey { return t.key }

// OwnerRef returns a controller OwnerReference to the RunnerSet so deleting it (or
// its namespace) cascade-GCs the worker pods and job Secrets. BlockOwnerDeletion
// is left unset, matching the v1 RunnerGroup owner-ref.
func (t *runnerSetTarget) OwnerRef() metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: v2alpha1.GroupVersion.String(),
		Kind:       "RunnerSet",
		Name:       t.key.Name,
		UID:        t.uid,
		Controller: ptr.To(true),
	}
}

// PodOwnerLabels stamps the v2 runner-set identity label so the RunnerSet
// controller's Pod watch and reaper select only this set's worker pods, never a
// v1 RunnerGroup's.
func (t *runnerSetTarget) PodOwnerLabels() map[string]string {
	return map[string]string{provisioner.LabelRunnerSet: t.key.Name}
}

// RecordEvent routes an owner-scoped provisioning Event to the RunnerSet reconciler,
// which records it on the live RunnerSet. A no-op when no recorder is wired.
func (t *runnerSetTarget) RecordEvent(eventtype, reason, action, note string) {
	if t.events == nil {
		return
	}
	t.events.Event(t.key.Namespace, t.key.Name, eventtype, reason, action, note)
}

// Ceiling reads the worker ceiling from the fresh RunnerSet spec.
func (t *runnerSetTarget) Ceiling(ctx context.Context) (int32, bool) {
	rs := &v2alpha1.RunnerSet{}
	if err := t.client.Get(ctx, t.key, rs); err != nil {
		// Cannot read the set; admit conservatively as unbounded (the post-acquire
		// ceilingCheck remains the authoritative backstop, matching the v1 fallback).
		return 0, false
	}
	return provisioner.WorkerCeilingFromTiers(runnerSetTierThresholds(rs.Spec.PriorityTiers), rs.Spec.MaxWorkers)
}

// Resolve re-reads the RunnerSet and resolves its references into a provisioning
// spec. A missing RunnerSet/Gateway/Template/Proxy yields an error so the job is
// failed without creating a worker pod (fail-closed, §H.7).
func (t *runnerSetTarget) Resolve(ctx context.Context) (*provisioner.ResolvedSpec, error) {
	rs := &v2alpha1.RunnerSet{}
	if err := t.client.Get(ctx, t.key, rs); err != nil {
		return nil, fmt.Errorf("read RunnerSet: %w", err)
	}
	refs, res := resolveRunnerSetRefs(ctx, t.client, rs)
	if res.err != nil {
		return nil, res.err
	}
	if !res.resolved() {
		return nil, fmt.Errorf("%s: %s", res.reason, res.message)
	}

	spec := &provisioner.ResolvedSpec{
		PodTemplate:        refs.template.PodTemplate,
		WorkerImage:        refs.template.WorkerImage,
		MaxWorkers:         rs.Spec.MaxWorkers,
		PriorityTiers:      runnerSetTierThresholds(rs.Spec.PriorityTiers),
		MaxEvictionRetries: t.prov.MaxEvictionRetries,
		EvictionRetryDelay: t.prov.EvictionRetryDelay,
		MaxQuotaRetries:    t.prov.MaxQuotaRetries,
		QuotaRetryDelay:    t.prov.QuotaRetryDelay,
		CompletedPodTTL:    provisioner.CompletedPodTTLOrDefault(rs.Spec.CompletedPodTTL),
		SecurityProfile:    t.prov.SecurityProfile,
	}
	// Proxied: wire the worker's egress through the resolved EgressProxy. Direct
	// (refs.proxy == nil, §H.10): leave the proxy fields empty so the worker gets no
	// HTTP(S)_PROXY env and no proxy-CA mount and reaches GitHub directly — still
	// restricted by the GMC's direct-egress workload NetworkPolicy to DNS + GitHub.
	if refs.proxy != nil {
		proxyName := refs.proxy.Name
		noProxy := defaultNoProxy
		if cidrs := refs.proxy.Spec.NoProxyCIDRs; len(cidrs) > 0 {
			noProxy = strings.Join(cidrs, ",") + "," + defaultNoProxy
		}
		proxyAddr := fmt.Sprintf("https://%s.%s.svc.cluster.local:%d", egressProxyServiceName(proxyName), t.key.Namespace, proxyPort)
		spec.HTTPProxy = proxyAddr
		spec.HTTPSProxy = proxyAddr
		spec.NoProxy = noProxy
		spec.ProxyTLSSecretName = egressProxyTLSSecretName(proxyName)
	}
	if rs.Spec.MaxEvictionRetries != nil {
		spec.MaxEvictionRetries = int(*rs.Spec.MaxEvictionRetries)
	}
	if rs.Spec.EvictionRetryDelay != nil && rs.Spec.EvictionRetryDelay.Duration > 0 {
		spec.EvictionRetryDelay = rs.Spec.EvictionRetryDelay.Duration
	}
	if rs.Spec.MaxQuotaRetries != nil {
		spec.MaxQuotaRetries = int(*rs.Spec.MaxQuotaRetries)
	}
	if rs.Spec.QuotaRetryDelay != nil && rs.Spec.QuotaRetryDelay.Duration > 0 {
		spec.QuotaRetryDelay = rs.Spec.QuotaRetryDelay.Duration
	}
	return spec, nil
}

// resolvedRefs holds a RunnerSet's resolved references: the gateway it binds to,
// the worker pod shape from its template, and the egress proxy its workers use.
type resolvedRefs struct {
	gateway  *v2alpha1.ActionsGateway
	template *v2alpha1.RunnerTemplateSpec
	proxy    *v2alpha1.EgressProxy
	// templateSource is which rung of the optional-templateRef chain supplied the
	// template (Q172): one of v2alpha1.TemplateSource{Ref,GatewayDefault,ClusterDefault}.
	// Set only on full resolution; surfaced in RunnerSet status.templateSource.
	templateSource string
}

// refResolution is the outcome of resolving a RunnerSet's references: either a
// non-nil err (an unexpected API error to retry with backoff) or a reason/message
// naming the missing referent (a fail-closed runtime condition, §H.7), or — when
// reason is empty and err is nil — full resolution.
type refResolution struct {
	reason  string
	message string
	err     error
}

func (r refResolution) resolved() bool { return r.reason == "" && r.err == nil }

// resolveRunnerSetRefs resolves a RunnerSet's gatewayRef, templateRef, and
// proxyRef (or the gateway's defaultProxyRef) in the set's own namespace. Missing
// referents surface as a reason/message (GatewayNotFound / TemplateNotFound /
// AmbiguousDefault / ProxyNotFound) rather than an error, so the reconciler sets the
// condition and waits for the referent→referrer watch to re-enqueue when it appears —
// no apply ordering required (§H.7). The template is optional (Q172, §H.4): an unset
// templateRef resolves through the gateway's defaultTemplateRef, then the single
// cluster-default ClusterRunnerTemplate, before failing closed TemplateNotFound — never
// a phantom pod shape. The proxy is optional (Q168, §H.10): a RunnerSet whose proxyRef
// and gateway.defaultProxyRef are both unset resolves with refs.proxy == nil (direct
// egress, still NetworkPolicy-restricted), not ProxyNotFound. A reference to a *named
// but missing* proxy still fails closed with ProxyNotFound.
func resolveRunnerSetRefs(ctx context.Context, c client.Client, rs *v2alpha1.RunnerSet) (*resolvedRefs, refResolution) {
	ns := rs.Namespace
	refs := &resolvedRefs{}

	// gatewayRef → ActionsGateway (same namespace).
	gw := &v2alpha1.ActionsGateway{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: rs.Spec.GatewayRef.Name}, gw); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, refResolution{reason: v2alpha1.ReasonGatewayNotFound,
				message: fmt.Sprintf("ActionsGateway %q not found in namespace %q", rs.Spec.GatewayRef.Name, ns)}
		}
		return nil, refResolution{err: fmt.Errorf("read ActionsGateway: %w", err)}
	}
	refs.gateway = gw

	// templateRef → RunnerTemplate/ClusterRunnerTemplate via the optional-templateRef
	// chain (Q172, §H.4): rs.templateRef → gateway.defaultTemplateRef → the single
	// cluster-default ClusterRunnerTemplate → fail-closed TemplateNotFound. Fail-closed
	// throughout — never a phantom pod shape.
	tmplSpec, tmplSource, res := resolveTemplateChain(ctx, c, ns, rs, gw)
	if !res.resolved() {
		return nil, res
	}
	refs.template = tmplSpec
	refs.templateSource = tmplSource

	// proxyRef → EgressProxy, else gateway.defaultProxyRef. Both unset ⇒ direct
	// egress (§H.10): refs.proxy stays nil, the worker reaches GitHub directly
	// (still NetworkPolicy-restricted), and the set is Ready with proxyMode Direct —
	// no longer a fail-closed ProxyNotFound. A proxyRef/defaultProxyRef that names a
	// *missing* proxy is still fail-closed ProxyNotFound: an explicit reference to a
	// not-yet-applied proxy must not silently fall back to direct egress.
	proxyName := ""
	if rs.Spec.ProxyRef != nil {
		proxyName = rs.Spec.ProxyRef.Name
	} else if gw.Spec.DefaultProxyRef != nil {
		proxyName = gw.Spec.DefaultProxyRef.Name
	}
	if proxyName == "" {
		return refs, refResolution{} // direct egress: refs.proxy == nil
	}
	proxy := &v2alpha1.EgressProxy{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: proxyName}, proxy); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, refResolution{reason: v2alpha1.ReasonProxyNotFound,
				message: fmt.Sprintf("EgressProxy %q not found in namespace %q", proxyName, ns)}
		}
		return nil, refResolution{err: fmt.Errorf("read EgressProxy: %w", err)}
	}
	refs.proxy = proxy

	return refs, refResolution{}
}

// resolveTemplateChain resolves a RunnerSet's worker pod shape through the optional-
// templateRef fallback chain (Q172, §H.4): rs.spec.templateRef → gateway.spec.
// defaultTemplateRef → the single cluster-default ClusterRunnerTemplate → fail-closed
// TemplateNotFound. It returns the resolved spec, which rung supplied it
// (status.templateSource), and the resolution outcome. Fail-closed throughout: a
// named-but-missing template yields TemplateNotFound, two cluster-defaults yield
// AmbiguousDefault, and an exhausted chain yields TemplateNotFound — the AGC never
// synthesizes a pod shape. A set with an explicit templateRef behaves exactly as
// before the relaxation (rung 1 only).
func resolveTemplateChain(ctx context.Context, c client.Client, ns string, rs *v2alpha1.RunnerSet, gw *v2alpha1.ActionsGateway) (*v2alpha1.RunnerTemplateSpec, string, refResolution) {
	// Rung 1: the set's own explicit templateRef.
	if rs.Spec.TemplateRef != nil {
		spec, res := resolveTemplate(ctx, c, ns, *rs.Spec.TemplateRef)
		return spec, v2alpha1.TemplateSourceRef, res
	}
	// Rung 2: the gateway's defaultTemplateRef, inherited because templateRef is unset.
	if gw.Spec.DefaultTemplateRef != nil {
		spec, res := resolveTemplate(ctx, c, ns, *gw.Spec.DefaultTemplateRef)
		return spec, v2alpha1.TemplateSourceGatewayDefault, res
	}
	// Rung 3: the single cluster-default ClusterRunnerTemplate.
	spec, res := resolveClusterDefaultTemplate(ctx, c)
	return spec, v2alpha1.TemplateSourceClusterDefault, res
}

// resolveClusterDefaultTemplate resolves the single cluster-default ClusterRunnerTemplate
// — the one carrying IsDefaultTemplateAnnotation=IsDefaultTemplateValue — the last rung of
// the template chain, reached only when neither templateRef nor the gateway's
// defaultTemplateRef is set (Q172, §H.4). At-most-one is enforced here, at runtime: zero
// marked ⇒ TemplateNotFound, exactly one ⇒ resolved, two or more ⇒ AmbiguousDefault
// (fail-closed, never silently picks one — stricter than upstream StorageClass). Enforced
// at resolution rather than admission because the invariant is cross-object (single-object
// CEL cannot express it) and admission-time rejection would break GitOps apply-ordering
// (§H.7). The cluster-scoped List is authorized by the per-gateway
// agc-clusterrunnertemplate-reader ClusterRoleBinding the GMC creates (M3b).
func resolveClusterDefaultTemplate(ctx context.Context, c client.Client) (*v2alpha1.RunnerTemplateSpec, refResolution) {
	var list v2alpha1.ClusterRunnerTemplateList
	if err := c.List(ctx, &list); err != nil {
		return nil, refResolution{err: fmt.Errorf("list ClusterRunnerTemplates: %w", err)}
	}
	var defaults []*v2alpha1.ClusterRunnerTemplate
	for i := range list.Items {
		if list.Items[i].Annotations[v2alpha1.IsDefaultTemplateAnnotation] == v2alpha1.IsDefaultTemplateValue {
			defaults = append(defaults, &list.Items[i])
		}
	}
	switch len(defaults) {
	case 0:
		return nil, refResolution{reason: v2alpha1.ReasonTemplateNotFound,
			message: fmt.Sprintf("no templateRef, no gateway defaultTemplateRef, and no ClusterRunnerTemplate marked %s=%s",
				v2alpha1.IsDefaultTemplateAnnotation, v2alpha1.IsDefaultTemplateValue)}
	case 1:
		return &defaults[0].Spec, refResolution{}
	default:
		names := make([]string, len(defaults))
		for i, d := range defaults {
			names[i] = d.Name
		}
		sort.Strings(names)
		return nil, refResolution{reason: v2alpha1.ReasonAmbiguousDefault,
			message: fmt.Sprintf("%d ClusterRunnerTemplates are marked the cluster default (%s); exactly one must be: %s",
				len(defaults), v2alpha1.IsDefaultTemplateAnnotation, strings.Join(names, ", "))}
	}
}

// resolveTemplate resolves a templateRef to a worker pod shape. kind selects the
// cluster-scoped ClusterRunnerTemplate; the default (empty/RunnerTemplate) is the
// namespaced RunnerTemplate. Both fail closed with TemplateNotFound when the
// referent is absent, so the set waits for the referent→referrer watch (§H.7).
func resolveTemplate(ctx context.Context, c client.Client, ns string, ref v2alpha1.ObjectRef) (*v2alpha1.RunnerTemplateSpec, refResolution) {
	if ref.Kind == "ClusterRunnerTemplate" {
		// Cluster-scoped read, authorized by the per-gateway ClusterRoleBinding to
		// agc-clusterrunnertemplate-reader the GMC creates (M3b). The kind is
		// platform-authored and holds golden (incl. privileged) templates (§H.7).
		crt := &v2alpha1.ClusterRunnerTemplate{}
		if err := c.Get(ctx, types.NamespacedName{Name: ref.Name}, crt); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, refResolution{reason: v2alpha1.ReasonTemplateNotFound,
					message: fmt.Sprintf("ClusterRunnerTemplate %q not found", ref.Name)}
			}
			return nil, refResolution{err: fmt.Errorf("read ClusterRunnerTemplate: %w", err)}
		}
		return &crt.Spec, refResolution{}
	}
	rt := &v2alpha1.RunnerTemplate{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: ref.Name}, rt); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, refResolution{reason: v2alpha1.ReasonTemplateNotFound,
				message: fmt.Sprintf("RunnerTemplate %q not found in namespace %q", ref.Name, ns)}
		}
		return nil, refResolution{err: fmt.Errorf("read RunnerTemplate: %w", err)}
	}
	return &rt.Spec, refResolution{}
}

// runnerSetTierThresholds converts v2 priority tiers to the neutral TierThreshold
// shape the provisioner's shared ceiling logic consumes.
func runnerSetTierThresholds(tiers []v2alpha1.PriorityTier) []provisioner.TierThreshold {
	if len(tiers) == 0 {
		return nil
	}
	out := make([]provisioner.TierThreshold, len(tiers))
	for i, t := range tiers {
		out[i] = provisioner.TierThreshold{PriorityClassName: t.PriorityClassName, Threshold: t.Threshold}
	}
	return out
}
