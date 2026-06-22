package controller

import (
	"context"
	"fmt"
	"strings"

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

	proxyName := refs.proxy.Name
	noProxy := defaultNoProxy
	if cidrs := refs.proxy.Spec.NoProxyCIDRs; len(cidrs) > 0 {
		noProxy = strings.Join(cidrs, ",") + "," + defaultNoProxy
	}
	proxyAddr := fmt.Sprintf("https://%s.%s.svc.cluster.local:%d", egressProxyServiceName(proxyName), t.key.Namespace, proxyPort)

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
		HTTPProxy:          proxyAddr,
		HTTPSProxy:         proxyAddr,
		NoProxy:            noProxy,
		ProxyTLSSecretName: egressProxyTLSSecretName(proxyName),
		SecurityProfile:    t.prov.SecurityProfile,
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
// ProxyNotFound) rather than an error, so the reconciler sets the condition and
// waits for the referent→referrer watch to re-enqueue when it appears — no apply
// ordering required (§H.7). Proxy is required in M3a (single-gateway parity): a
// RunnerSet whose proxyRef and gateway.defaultProxyRef are both unset reports
// ProxyNotFound, never silent direct egress (direct egress is deferred, §H.10).
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

	// templateRef → RunnerTemplate (namespaced) or ClusterRunnerTemplate (cluster).
	tmplSpec, res := resolveTemplate(ctx, c, ns, rs.Spec.TemplateRef)
	if !res.resolved() {
		return nil, res
	}
	refs.template = tmplSpec

	// proxyRef → EgressProxy, else gateway.defaultProxyRef. Proxy required (M3a).
	proxyName := ""
	if rs.Spec.ProxyRef != nil {
		proxyName = rs.Spec.ProxyRef.Name
	} else if gw.Spec.DefaultProxyRef != nil {
		proxyName = gw.Spec.DefaultProxyRef.Name
	}
	if proxyName == "" {
		return nil, refResolution{reason: v2alpha1.ReasonProxyNotFound,
			message: fmt.Sprintf("no proxy: RunnerSet %q sets no proxyRef and gateway %q has no defaultProxyRef", rs.Name, gw.Name)}
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

// resolveTemplate resolves a templateRef to a worker pod shape. kind selects the
// cluster-scoped ClusterRunnerTemplate; the default (empty/RunnerTemplate) is the
// namespaced RunnerTemplate.
func resolveTemplate(ctx context.Context, c client.Client, ns string, ref v2alpha1.ObjectRef) (*v2alpha1.RunnerTemplateSpec, refResolution) {
	if ref.Kind == "ClusterRunnerTemplate" {
		// Cluster-scoped ClusterRunnerTemplate reads need a ClusterRoleBinding (not
		// the per-tenant RoleBinding) and the corresponding cluster-scoped watch;
		// both land in M3b. In M3a the AGC has neither, so we deliberately do NOT
		// read or watch the kind here — attempting a cached Get would force an
		// informer the AGC's RBAC forbids, breaking manager cache sync. Fail closed
		// with a clear condition instead; namespaced RunnerTemplate gives v1 parity.
		return nil, refResolution{reason: v2alpha1.ReasonTemplateNotFound,
			message: fmt.Sprintf("templateRef.kind=ClusterRunnerTemplate (%q) is not supported in this milestone; cluster-scoped template access lands in M3b — use a namespaced RunnerTemplate", ref.Name)}
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
