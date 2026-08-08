package controller

// Cross-namespace EgressProxy sharing (M4, §H.9). Consent is provider-side: the
// proxy owner lists consumer namespaces in spec.sharing.allowedNamespaces, and a
// consumer naming the proxy from the other side never authorizes the reference.
//
// The GMC mediates every cross-namespace resolution. The AGC's cache is pinned to
// its own tenant namespace so it needs only a per-tenant Role rather than a
// ClusterRole (cmd/agc/main.go), which means it cannot read a remote EgressProxy at
// all. §H.9 already requires the GMC to distribute the proxy's CA certificate — the
// public half, never the key — into each granted namespace as a ConfigMap. That
// ConfigMap carries the connection facts alongside the trust material, so its
// presence IS the grant: the AGC reads it from its own namespace, and a revoked
// grant deletes it and fails the consumer closed.

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/actions-gateway/github-actions-gateway/api/apinames"
	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// Keys in a projected share ConfigMap. caCertKey holds the proxy's public
	// certificate; the rest are the connection facts a consumer would otherwise have
	// had to read off the EgressProxy object.
	shareCACertKey   = "ca.crt"
	shareHostKey     = "proxy-host"
	sharePortKey     = "proxy-port"
	shareNoProxyKey  = "no-proxy"
	shareProxyNSKey  = "proxy-namespace"
	shareProxyNamKey = "proxy-name"

	// Labels identifying a projected share ConfigMap. The GMC lists by
	// labelShareManaged to find projections it owns — including ones in namespaces a
	// revoked grant no longer names, which is what makes cleanup complete.
	labelShareManaged  = "actions-gateway/proxy-share"
	labelShareProxyNS  = "actions-gateway/proxy-namespace"
	labelShareProxyNam = "actions-gateway/proxy-name"
)

// proxyShareGranted reports whether ep consents to being referenced from
// consumerNS. Absent or empty sharing denies: a namespace must never gain access it
// did not previously have because a field was left unset.
//
// A same-namespace reference is not this function's business — callers short-circuit
// it before asking, since colocated use needs no grant.
func proxyShareGranted(ep *gmcv2alpha1.EgressProxy, consumerNS string) bool {
	if ep.Spec.Sharing == nil || consumerNS == "" {
		return false
	}
	return slices.Contains(ep.Spec.Sharing.AllowedNamespaces, consumerNS)
}

// proxyShareConfigMapName derives the name a projected share ConfigMap takes in a
// consumer namespace. It keys on the provider's namespace AND name: one consumer may
// hold grants from same-named proxies in two provider namespaces, and a name derived
// from the proxy name alone would collide and let one provider's CA silently
// overwrite another's.
func proxyShareConfigMapName(proxyNamespace, proxyName string) string {
	return apinames.Join(apinames.MaxLabelValue, "proxy-share", proxyNamespace, proxyName)
}

// proxyShareServiceHost is the in-cluster DNS name a consumer dials to reach the
// proxy. Same derivation the AGC uses for a colocated proxy, with the provider's
// namespace substituted.
func proxyShareServiceHost(ep *gmcv2alpha1.EgressProxy) string {
	return fmt.Sprintf("%s.%s.svc.cluster.local", proxyResourceName(ep), ep.Namespace)
}

// buildProxyShareConfigMap constructs the projection written into one granted
// consumer namespace. caPEM is the proxy's public certificate; the private key is
// never read here and never leaves the provider namespace.
//
// It carries no owner reference: cross-namespace owner references are not honoured
// by the apiserver's garbage collector (an owner must be in the same namespace, or
// cluster-scoped), so a dangling reference would make the object un-GC-able. The
// reconciler prunes these explicitly instead — see pruneProxyShares.
func buildProxyShareConfigMap(ep *gmcv2alpha1.EgressProxy, consumerNS string, caPEM []byte) *corev1.ConfigMap {
	noProxy := ""
	if cidrs := ep.Spec.NoProxyCIDRs; len(cidrs) > 0 {
		noProxy = strings.Join(cidrs, ",")
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      proxyShareConfigMapName(ep.Namespace, ep.Name),
			Namespace: consumerNS,
			Labels: map[string]string{
				labelManagedBy:     labelManagerValue,
				labelShareManaged:  "true",
				labelShareProxyNS:  ep.Namespace,
				labelShareProxyNam: ep.Name,
			},
		},
		Data: map[string]string{
			shareCACertKey:   string(caPEM),
			shareHostKey:     proxyShareServiceHost(ep),
			sharePortKey:     fmt.Sprintf("%d", proxyPort),
			shareNoProxyKey:  noProxy,
			shareProxyNSKey:  ep.Namespace,
			shareProxyNamKey: ep.Name,
		},
	}
}

// consumerNamespacesFor returns the namespaces this proxy must project into: those
// that both hold a referrer naming it cross-namespace AND appear in its
// allowedNamespaces. Sorted and deduplicated.
//
// Intersecting the two is what keeps the grant meaningful. Projecting into every
// granted namespace would publish trust material into namespaces that never asked
// for it; projecting into every referring namespace would hand out the CA on the
// consumer's say-so, which is the consent bypass this whole mechanism exists to
// prevent.
func consumerNamespacesFor(ctx context.Context, c client.Client, ep *gmcv2alpha1.EgressProxy) ([]string, error) {
	if ep.Spec.Sharing == nil || len(ep.Spec.Sharing.AllowedNamespaces) == 0 {
		return nil, nil
	}

	var gateways gmcv2alpha1.ActionsGatewayList
	if err := c.List(ctx, &gateways); err != nil {
		return nil, fmt.Errorf("list ActionsGateways: %w", err)
	}
	var runnerSets gmcv2alpha1.RunnerSetList
	if err := c.List(ctx, &runnerSets); err != nil {
		return nil, fmt.Errorf("list RunnerSets: %w", err)
	}

	var namespaces []string
	names := func(refNS, refName, referrerNS string) {
		if refName != ep.Name || refNS == "" || refNS != ep.Namespace {
			return
		}
		if referrerNS == ep.Namespace || !proxyShareGranted(ep, referrerNS) {
			return
		}
		namespaces = append(namespaces, referrerNS)
	}
	for i := range gateways.Items {
		gw := &gateways.Items[i]
		if gw.Spec.DefaultProxyRef != nil {
			names(gw.Spec.DefaultProxyRef.Namespace, gw.Spec.DefaultProxyRef.Name, gw.Namespace)
		}
	}
	for i := range runnerSets.Items {
		rs := &runnerSets.Items[i]
		if rs.Spec.ProxyRef != nil {
			names(rs.Spec.ProxyRef.Namespace, rs.Spec.ProxyRef.Name, rs.Namespace)
		}
	}

	slices.Sort(namespaces)
	return slices.Compact(namespaces), nil
}

// grantedRemoteProxyNamespaces returns the namespaces this gateway's egress policies
// must reach for cross-namespace proxies: the gateway's own defaultProxyRef plus the
// proxyRef of every RunnerSet bound to it, keeping only references the provider has
// consented to.
//
// Filtering on consent here is defence in depth rather than the enforcement point —
// the provider's own ingress policy is what actually admits the traffic, and it is
// built from the grant. Emitting an egress rule for an unconsented reference would
// open nothing, but it would misreport intent in a policy an operator reads.
//
// A read failure is returned rather than swallowed: silently returning no namespaces
// would strip working egress rules from a live policy.
func grantedRemoteProxies(ctx context.Context, c client.Client, ag *gmcv2alpha1.ActionsGateway) ([]remoteProxy, error) {
	refs := map[remoteProxy]struct{}{}
	remote := func(ref *gmcv2alpha1.ProxyObjectRef) {
		if ref != nil && ref.Namespace != "" && ref.Namespace != ag.Namespace {
			refs[remoteProxy{Namespace: ref.Namespace, Name: ref.Name}] = struct{}{}
		}
	}
	remote(ag.Spec.DefaultProxyRef)

	var runnerSets gmcv2alpha1.RunnerSetList
	if err := c.List(ctx, &runnerSets, client.InNamespace(ag.Namespace)); err != nil {
		return nil, fmt.Errorf("list RunnerSets: %w", err)
	}
	for i := range runnerSets.Items {
		rs := &runnerSets.Items[i]
		if rs.Spec.GatewayRef.Name == ag.Name {
			remote(rs.Spec.ProxyRef)
		}
	}

	var granted []remoteProxy
	for ref := range refs {
		var ep gmcv2alpha1.EgressProxy
		if err := c.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, &ep); err != nil {
			if apierrors.IsNotFound(err) {
				continue // the reference's own NotFound condition reports this
			}
			return nil, fmt.Errorf("read EgressProxy %s/%s: %w", ref.Namespace, ref.Name, err)
		}
		if proxyShareGranted(&ep, ag.Namespace) {
			granted = append(granted, ref)
		}
	}
	slices.SortFunc(granted, func(a, b remoteProxy) int {
		if a.Namespace != b.Namespace {
			return strings.Compare(a.Namespace, b.Namespace)
		}
		return strings.Compare(a.Name, b.Name)
	})
	return granted, nil
}

// remoteProxy identifies one EgressProxy pool in another namespace. Both halves are
// carried so the egress peer can select that pool by its identity label rather than
// every proxy pod in the namespace: two pools can sit in one provider namespace and
// grant different consumers.
type remoteProxy struct {
	Namespace string
	Name      string
}

// reconcileProxyShares projects the proxy's CA and connection facts into every
// namespace entitled to them, and prunes projections that are no longer entitled.
// Pruning runs even when the proxy grants nothing, so clearing allowedNamespaces
// revokes access rather than merely stopping refreshes.
func (r *EgressProxyReconciler) reconcileProxyShares(ctx context.Context, ep *gmcv2alpha1.EgressProxy) error {
	wanted, err := consumerNamespacesFor(ctx, r.Client, ep)
	if err != nil {
		return err
	}

	if len(wanted) > 0 {
		var tlsSecret corev1.Secret
		if err := r.Get(ctx, types.NamespacedName{
			Namespace: ep.Namespace, Name: egressProxyTLSSecretName(ep),
		}, &tlsSecret); err != nil {
			return fmt.Errorf("read proxy TLS Secret for share projection: %w", err)
		}
		caPEM := tlsSecret.Data[corev1.TLSCertKey]
		if len(caPEM) == 0 {
			return fmt.Errorf("proxy TLS Secret %q holds no %s", egressProxyTLSSecretName(ep), corev1.TLSCertKey)
		}
		for _, ns := range wanted {
			if err := r.applyProxyShare(ctx, buildProxyShareConfigMap(ep, ns, caPEM)); err != nil {
				return fmt.Errorf("project proxy share into %q: %w", ns, err)
			}
		}
	}

	return r.pruneProxyShares(ctx, ep, wanted)
}

// shareReader returns the uncached reader used for projected share ConfigMaps, or
// the cached client when none is wired (unit tests).
func (r *EgressProxyReconciler) shareReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// applyProxyShare creates or updates one projected share ConfigMap.
func (r *EgressProxyReconciler) applyProxyShare(ctx context.Context, desired *corev1.ConfigMap) error {
	var existing corev1.ConfigMap
	err := r.shareReader().Get(ctx, client.ObjectKeyFromObject(desired), &existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	existing.Labels = desired.Labels
	existing.Data = desired.Data
	return r.Update(ctx, &existing)
}

// pruneProxyShares deletes this proxy's projections from every namespace not in
// keep. It lists by the proxy-identifying labels rather than iterating the previous
// grant list, so a projection is reclaimed even if the grant that created it was
// removed while the controller was down.
func (r *EgressProxyReconciler) pruneProxyShares(ctx context.Context, ep *gmcv2alpha1.EgressProxy, keep []string) error {
	var existing corev1.ConfigMapList
	if err := r.shareReader().List(ctx, &existing, client.MatchingLabels{
		labelShareManaged:  "true",
		labelShareProxyNS:  ep.Namespace,
		labelShareProxyNam: ep.Name,
	}); err != nil {
		return fmt.Errorf("list projected proxy shares: %w", err)
	}
	for i := range existing.Items {
		cm := &existing.Items[i]
		if slices.Contains(keep, cm.Namespace) {
			continue
		}
		if err := r.Delete(ctx, cm); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("revoke proxy share in %q: %w", cm.Namespace, err)
		}
	}
	return nil
}
