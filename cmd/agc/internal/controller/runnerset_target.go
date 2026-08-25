package controller

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/runnercore"
	"github.com/actions-gateway/github-actions-gateway/api/apinames"
	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
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

	// Keys the GMC writes into a projected cross-namespace proxy share (§H.9), and
	// the prefix its name is built from. Same convention-not-API reasoning as the
	// suffixes above: the authority is
	// cmd/gmc/internal/controller/egressproxy_sharing.go, and the two modules must
	// agree because the ConfigMap is the entire interface between them.
	proxyShareNamePrefix = "proxy-share"
	proxyShareHostKey    = "proxy-host"
	proxySharePortKey    = "proxy-port"
	proxyShareNoProxyKey = "no-proxy"

	// defaultNoProxy excludes cluster-internal traffic from the egress proxy so the
	// proxy is only used for external (GitHub) traffic. The GMC sets the AGC's own
	// NO_PROXY from the static half of the same list (its buildNoProxy in
	// cmd/gmc/internal/controller/shared_agc_deployment.go); the AGC sets each
	// worker's NO_PROXY here from the resolved EgressProxy.
	//
	// Worker pods reach in-cluster Services by DNS name only, and never talk to the
	// API server — they run with automountServiceAccountToken: false and hold no
	// kubeconfig — so this list is DNS plus loopback and nothing else. It is
	// deliberately narrower than the AGC's, which additionally exempts the API
	// server's ClusterIP because client-go dials it by IP (Q465). It no longer
	// carries the kubeadm Service CIDR 10.96.0.0/12: that range is not the Service
	// CIDR on any managed distribution, and on a cluster whose pod or node
	// addresses fall inside it, it exempted arbitrary traffic from the tenant's
	// egress attribution for no benefit. An operator who does need a ClusterIP
	// range exempted names it in the EgressProxy's spec.noProxyCIDRs.
	defaultNoProxy = "svc.cluster.local,localhost,127.0.0.1"
)

// egressProxyServiceName / egressProxyTLSSecretName derive an EgressProxy's child
// Service and TLS-Secret names. See egressProxyResourceSuffix.
func egressProxyServiceName(name string) string   { return name + egressProxyResourceSuffix }
func egressProxyTLSSecretName(name string) string { return name + egressProxyTLSSuffix }

// proxyShareConfigMapName derives the name of the ConfigMap the GMC projects into
// this namespace for a granted cross-namespace proxy. It keys on the provider's
// namespace AND name so grants from same-named proxies in two provider namespaces
// cannot collide. Must match the GMC's derivation exactly.
func proxyShareConfigMapName(proxyNamespace, proxyName string) string {
	return apinames.Join(apinames.MaxLabelValue, proxyShareNamePrefix, proxyNamespace, proxyName)
}

// runnerSetTarget adapts a v2alpha1.RunnerSet to the provisioner Target seam. It
// owns worker pods via an OwnerReference to the real RunnerSet (a synthesized
// in-memory RunnerGroup would have a dangling owner-ref the apiserver GCs), and
// resolves the RunnerSet's RunnerTemplate (pod shape) and EgressProxy (worker
// egress) on every acquired job so reference edits take effect without a restart
// (Q117) and a reference that stops resolving fails the job fail-closed (§H.7).
type runnerSetTarget struct {
	client client.Client
	// reader is the manager's uncached reader, used only for the projected
	// proxy-share ConfigMap (see resolveRunnerSetRefs). Nil falls back to client.
	reader client.Reader
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
	events runnercore.EventRecorder
}

// Key returns the RunnerSet's namespace/name.
func (t *runnerSetTarget) Key() client.ObjectKey { return t.key }

// OwnerRef returns a controller OwnerReference to the RunnerSet so deleting it (or
// its namespace) cascade-GCs the worker pods and job Secrets. BlockOwnerDeletion
// is left unset, matching the v1 RunnerGroup owner-ref.
func (t *runnerSetTarget) OwnerRef() metav1.OwnerReference {
	return runnerSetOwnerRef(t.key.Name, t.uid)
}

// runnerSetOwnerRef returns the controller OwnerReference to a RunnerSet that every
// object the AGC derives from it carries — worker pods and job Secrets via the Target
// above, agent-pool Secrets via the RunnerSet reconciler — so the two paths cannot
// drift into two spellings of the same reference.
func runnerSetOwnerRef(name string, uid types.UID) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: v2alpha1.GroupVersion.String(),
		Kind:       "RunnerSet",
		Name:       name,
		UID:        uid,
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

// QuotaExhausted checks live namespace-ResourceQuota headroom against one more
// worker pod of this set's current shape. It resolves only the template chain (not
// the proxy) — the pod shape is all the footprint needs — and applies the sizing
// profile, so the gate sizes the same pod Resolve would build.
//
// Fail-open at every step: an unreadable set or gateway, a template chain that does
// not resolve, or an unreadable quota all yield false. A set whose references are
// broken must fail closed in Resolve (§H.7), not be silently starved by the gate.
func (t *runnerSetTarget) QuotaExhausted(ctx context.Context) (bool, string) {
	spec, ok := t.workerPodSpec(ctx)
	if !ok {
		return false, ""
	}
	return provisioner.WorkerQuotaExhausted(ctx, t.client, t.key.Namespace, spec)
}

// QuotaCapacity is the integer form of the same rung, for the scale-set tier's per-poll
// capacity advertisement (Q443): the total worker pods this set may have in flight
// given live namespace-ResourceQuota headroom, capped at max. It sizes the pod exactly
// as QuotaExhausted does, and converts the observed headroom delta to a total with the
// set's own non-terminal worker pods — the ones already counted in the quota's `used`.
//
// Fail-open at every step, for the same reason: a set whose references are broken must
// fail closed in Resolve (§H.7), never be silently starved of assignments by the gate.
func (t *runnerSetTarget) QuotaCapacity(ctx context.Context, max int32) (int32, bool) {
	spec, ok := t.workerPodSpec(ctx)
	if !ok {
		return 0, false
	}
	active := countActiveWorkerPodsByLabel(ctx, t.client, t.key.Namespace, provisioner.LabelRunnerSet, t.key.Name)
	return provisioner.WorkerQuotaCapacity(ctx, t.client, t.key.Namespace, spec, active, max)
}

// CapacityDeclined is the placeability rung of the admission ladder (Q405): true when
// this set opted into a capacity gate AND that gate is currently declining, which the
// reconciler publishes as the WorkerCapacityDeclined condition.
//
// Reading the published condition rather than re-deriving the verdict here is what
// keeps the gate and the operator's view of it from ever disagreeing — the condition
// IS the decision, and `kubectl describe runnerset` explains a stall completely. It
// also keeps this a cheap per-delivery cache read, like Ceiling and QuotaExhausted,
// instead of a pod list on the acquisition hot path — except in the latched state
// (Q512), where one pod list decides whether this delivery is the probe: a latched
// gate declines only while a probe pod is outstanding, so exactly one job per
// deadline window gets through to re-test the cluster. Without that admission the
// latch could never resolve on the classic tier — no probe pod would ever exist.
//
// Fail-open at every step: an unreadable set, a set that never opted in, and a set
// whose condition has not been computed yet all yield false. The mode is re-checked
// here even though the reconciler only publishes the condition for an opted-in set,
// so that opting out takes effect on the very next delivered job rather than waiting
// for a reconcile to retract the condition.
func (t *runnerSetTarget) CapacityDeclined(ctx context.Context) (bool, string) {
	c := t.capacityGateCondition(ctx)
	if c == nil {
		return false, ""
	}
	if c.Reason == v2alpha1.ReasonAwaitingProbe {
		if _, probeOutstanding := t.activeWorkerPodsWithProbe(ctx, c.LastTransitionTime.Time); !probeOutstanding {
			return false, ""
		}
	}
	return true, c.Message
}

// DeclinedCapacity is the integer form of the same rung, for the scale-set tier's
// per-poll advertisement (Q443's invariant — a rung expressed in only one form ships
// to only one tier). A live decline means "no room for another worker pod", so the
// bound is this set's own non-terminal worker pods: GitHub keeps whatever it has
// already assigned and is offered nothing more. In the latched state (Q512) the
// bound adds one probe slot whenever no probe pod is outstanding, because this tier
// has no per-job decision point: without the slot the advertisement would snap back
// to the full ceiling when the reaper cleared the gate's evidence — measured as a
// burst of N wasted claims per deadline window — and with it the tier trickles at
// one, the same rate the classic tier's per-delivery form allows.
//
// Bias-low by construction, like QuotaCapacity: the count is of pods the AGC can see,
// which lags an assignment it has not provisioned yet. Under-advertising only delays
// jobs; over-advertising would reproduce the claim-and-stall this rung exists to stop.
func (t *runnerSetTarget) DeclinedCapacity(ctx context.Context, max int32) (int32, bool) {
	c := t.capacityGateCondition(ctx)
	if c == nil {
		return 0, false
	}
	active, probeOutstanding := t.activeWorkerPodsWithProbe(ctx, c.LastTransitionTime.Time)
	limit := active
	if c.Reason == v2alpha1.ReasonAwaitingProbe && !probeOutstanding {
		limit++
	}
	if limit > max {
		limit = max
	}
	return limit, true
}

// ScaleUpLimit reads the rate limit from the fresh RunnerSet spec, so the rate rung
// of the admission ladder honours a spec.scaleUp edit on the next delivered job or
// long-poll without an AGC restart (Q117). Fail-open like Ceiling: an unreadable set
// yields nil, which leaves the rung unbound rather than starving the set.
func (t *runnerSetTarget) ScaleUpLimit(ctx context.Context) *provisioner.ScaleUpConfig {
	rs := &v2alpha1.RunnerSet{}
	if err := t.client.Get(ctx, t.key, rs); err != nil {
		return nil
	}
	return scaleUpConfigFromV2(rs.Spec.ScaleUp)
}

// capacityGateCondition returns the set's WorkerCapacityDeclined condition when the
// gate is enabled and currently True, else nil. The shared read for both forms of
// the placeability rung, folding in every fail-open step they have in common: an
// unreadable set, mode Off (re-checked live so opting out takes effect on the next
// delivered job), and a condition absent, False, or not yet computed.
func (t *runnerSetTarget) capacityGateCondition(ctx context.Context) *metav1.Condition {
	rs := &v2alpha1.RunnerSet{}
	if err := t.client.Get(ctx, t.key, rs); err != nil {
		return nil
	}
	if runnerSetCapacityGateMode(rs) == v2alpha1.CapacityGateModeOff {
		return nil
	}
	c := meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionWorkerCapacityDeclined)
	if c == nil || c.Status != metav1.ConditionTrue {
		return nil
	}
	return c
}

// activeWorkerPodsWithProbe counts this set's ceiling-relevant worker pods exactly
// as countActiveWorkerPodsByLabel does — non-terminal and not being deleted — and
// reports whether any of them was created after since, the latched gate's
// outstanding-probe test (Q512). One list serves both answers so they cannot come
// from different snapshots. A list failure reads as no pods and no probe, which for
// the latch means "admit the probe" — the fail-open direction.
func (t *runnerSetTarget) activeWorkerPodsWithProbe(ctx context.Context, since time.Time) (active int32, probeOutstanding bool) {
	var pods corev1.PodList
	if err := t.client.List(ctx, &pods,
		client.InNamespace(t.key.Namespace),
		client.MatchingLabels{provisioner.LabelRunnerSet: t.key.Name},
	); err != nil {
		return 0, false
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if !p.DeletionTimestamp.IsZero() {
			continue
		}
		switch p.Status.Phase {
		case corev1.PodSucceeded, corev1.PodFailed, corev1.PodUnknown:
			continue
		}
		active++
		if p.CreationTimestamp.Time.After(since) {
			probeOutstanding = true
		}
	}
	return active, probeOutstanding
}

// runnerSetCapacityGateMode returns the set's effective capacity-gate mode, applying
// the Off default for an unset spec.capacityGate (an older object stored before the
// field existed, or one that simply omits it). Off is the only value that means "no
// rung", so defaulting to it is the fail-open direction.
func runnerSetCapacityGateMode(rs *v2alpha1.RunnerSet) string {
	if rs.Spec.CapacityGate == nil || rs.Spec.CapacityGate.Mode == "" {
		return v2alpha1.CapacityGateModeOff
	}
	return rs.Spec.CapacityGate.Mode
}

// workerPodSpec resolves the pod spec of the worker this set would provision right
// now — the template chain only (not the proxy; the pod shape is all a quota
// footprint needs), with the sizing profile applied. ok=false means something did not
// resolve, and every caller must then fail open.
func (t *runnerSetTarget) workerPodSpec(ctx context.Context) (*corev1.PodSpec, bool) {
	rs := &v2alpha1.RunnerSet{}
	if err := t.client.Get(ctx, t.key, rs); err != nil {
		return nil, false
	}
	gw := &v2alpha1.ActionsGateway{}
	if err := t.client.Get(ctx, types.NamespacedName{Namespace: t.key.Namespace, Name: rs.Spec.GatewayRef.Name}, gw); err != nil {
		return nil, false
	}
	tmpl, _, _, res := resolveTemplateChain(ctx, t.client, t.key.Namespace, rs, gw)
	if !res.resolved() {
		return nil, false
	}
	return runnerSetWorkerPodSpec(rs, tmpl), true
}

// Resolve re-reads the RunnerSet and resolves its references into a provisioning
// spec. A missing RunnerSet/Gateway/Template/Proxy yields an error so the job is
// failed without creating a worker pod (fail-closed, §H.7).
func (t *runnerSetTarget) Resolve(ctx context.Context) (*provisioner.ResolvedSpec, error) {
	rs := &v2alpha1.RunnerSet{}
	if err := t.client.Get(ctx, t.key, rs); err != nil {
		return nil, fmt.Errorf("read RunnerSet: %w", err)
	}
	refs, res := resolveRunnerSetRefs(ctx, t.client, t.reader, rs)
	if res.err != nil {
		return nil, res.err
	}
	if !res.resolved() {
		return nil, fmt.Errorf("%s: %s", res.reason, res.message)
	}

	spec := &provisioner.ResolvedSpec{
		// The opt-in sizing profile (Q359 Phase 3) derives worker cpu/memory from
		// the persisted usage history (status.sizingRecommendation) at pod-build
		// time; Static/no profile passes the template through untouched. Re-read
		// per job like every other input, so a spec edit or newly-confident
		// history takes effect on the next job without a restart.
		PodTemplate:        applySizingProfile(refs.template.PodTemplate, rs.Spec.Sizing, refs.template, rs.Status.SizingRecommendation),
		WorkerImage:        refs.template.WorkerImage,
		MaxWorkers:         rs.Spec.MaxWorkers,
		PriorityTiers:      runnerSetTierThresholds(rs.Spec.PriorityTiers),
		MaxEvictionRetries: t.prov.MaxEvictionRetries,
		EvictionRetryDelay: t.prov.EvictionRetryDelay,
		MaxQuotaRetries:    t.prov.MaxQuotaRetries,
		QuotaRetryDelay:    t.prov.QuotaRetryDelay,
		CompletedPodTTL:    provisioner.CompletedPodTTLOrDefault(rs.Spec.CompletedPodTTL),
		MaxWorkerLifetime:  provisioner.MaxWorkerLifetimeOrDefault(rs.Spec.MaxWorkerLifetime),
		SecurityProfile:    t.prov.SecurityProfile,
		// Per-gateway, not per-proxy: the appliance's CA is the same whether the
		// worker egresses through the proxy or directly, so it is set outside the
		// proxied branch below.
		GitHubCAConfigMapName: t.prov.GitHubCAConfigMapName,
	}
	// Proxied: wire the worker's egress through the resolved EgressProxy. Direct
	// (refs.proxy == nil, §H.10): leave the proxy fields empty so the worker gets no
	// HTTP(S)_PROXY env and no proxy-CA mount and reaches GitHub directly — still
	// restricted by the GMC's direct-egress workload NetworkPolicy to DNS + GitHub.
	if refs.proxy != nil {
		noProxy := defaultNoProxy
		if cidrs := refs.proxy.noProxyCIDRs; len(cidrs) > 0 {
			noProxy = strings.Join(cidrs, ",") + "," + defaultNoProxy
		}
		proxyAddr := fmt.Sprintf("https://%s:%d", refs.proxy.host, refs.proxy.port)
		spec.HTTPProxy = proxyAddr
		spec.HTTPSProxy = proxyAddr
		spec.NoProxy = noProxy
		spec.ProxyTLSSecretName = refs.proxy.tlsSecretName
		spec.ProxyCAConfigMapName = refs.proxy.caConfigMapName
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
	spec.ScaleUp = scaleUpConfigFromV2(rs.Spec.ScaleUp)
	return spec, nil
}

// scaleUpConfigFromV2 converts the v2 ScaleUpRateLimit into the neutral
// provisioner.ScaleUpConfig, defaulting Burst to MaxPerSecond when the spec omits
// it. Nil in ⇒ nil out (no rate limit), mirroring the v1 adapter.
func scaleUpConfigFromV2(s *v2alpha1.ScaleUpRateLimit) *provisioner.ScaleUpConfig {
	if s == nil {
		return nil
	}
	burst := s.MaxPerSecond
	if s.Burst != nil {
		burst = *s.Burst
	}
	return &provisioner.ScaleUpConfig{MaxPerSecond: s.MaxPerSecond, Burst: burst}
}

// resolvedRefs holds a RunnerSet's resolved references: the gateway it binds to,
// the worker pod shape from its template, and the egress proxy its workers use.
type resolvedRefs struct {
	gateway  *v2alpha1.ActionsGateway
	template *v2alpha1.RunnerTemplateSpec
	proxy    *resolvedProxy
	// templateSource is which rung of the optional-templateRef chain supplied the
	// template (Q172): one of v2alpha1.TemplateSource{Ref,GatewayDefault,ClusterDefault}.
	// Set only on full resolution; surfaced in RunnerSet status.templateSource.
	templateSource string
	// templateAnnotations are the resolved template object's metadata annotations,
	// carried so the reconciler can read the self-exiting-sidecars opt-out (Q249)
	// without re-reading the template. Set only on full resolution.
	templateAnnotations map[string]string
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
// reader is the uncached reader used for the one read that must not establish an
// informer: the projected proxy-share ConfigMap. The AGC Role grants get on
// ConfigMaps but not list/watch, so a cached read would try to start an informer it
// has no permission to run. Nil falls back to the cached client, as elsewhere.
func resolveRunnerSetRefs(ctx context.Context, c client.Client, reader client.Reader, rs *v2alpha1.RunnerSet) (*resolvedRefs, refResolution) {
	if reader == nil {
		reader = c
	}
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
	tmplSpec, tmplAnnotations, tmplSource, res := resolveTemplateChain(ctx, c, ns, rs, gw)
	if !res.resolved() {
		return nil, res
	}
	refs.template = tmplSpec
	refs.templateAnnotations = tmplAnnotations
	refs.templateSource = tmplSource

	// proxyRef → EgressProxy, else gateway.defaultProxyRef. Both unset ⇒ direct
	// egress (§H.10): refs.proxy stays nil, the worker reaches GitHub directly
	// (still NetworkPolicy-restricted), and the set is Ready with proxyMode Direct —
	// no longer a fail-closed ProxyNotFound. A proxyRef/defaultProxyRef that names a
	// *missing* proxy is still fail-closed ProxyNotFound: an explicit reference to a
	// not-yet-applied proxy must not silently fall back to direct egress.
	proxyName, proxyNS := "", ""
	if rs.Spec.ProxyRef != nil {
		proxyName, proxyNS = rs.Spec.ProxyRef.Name, rs.Spec.ProxyRef.Namespace
	} else if gw.Spec.DefaultProxyRef != nil {
		proxyName, proxyNS = gw.Spec.DefaultProxyRef.Name, gw.Spec.DefaultProxyRef.Namespace
	}
	if proxyName == "" {
		return refs, refResolution{} // direct egress: refs.proxy == nil
	}
	if proxyNS != "" && proxyNS != ns {
		proxy, res := resolveSharedProxy(ctx, reader, ns, proxyNS, proxyName)
		if !res.resolved() {
			return nil, res
		}
		refs.proxy = proxy
		return refs, refResolution{}
	}
	proxy := &v2alpha1.EgressProxy{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: proxyName}, proxy); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, refResolution{reason: v2alpha1.ReasonProxyNotFound,
				message: fmt.Sprintf("EgressProxy %q not found in namespace %q", proxyName, ns)}
		}
		return nil, refResolution{err: fmt.Errorf("read EgressProxy: %w", err)}
	}
	refs.proxy = &resolvedProxy{
		host:          fmt.Sprintf("%s.%s.svc.cluster.local", egressProxyServiceName(proxyName), ns),
		port:          proxyPort,
		noProxyCIDRs:  proxy.Spec.NoProxyCIDRs,
		tlsSecretName: egressProxyTLSSecretName(proxyName),
	}

	return refs, refResolution{}
}

// resolvedProxy is the egress wiring a worker needs, flattened off whichever source
// could supply it. A colocated proxy is read from the EgressProxy object directly; a
// proxy shared from another namespace is unreadable from here (the AGC's cache and
// Role are scoped to its own namespace), so its facts come from the ConfigMap the
// GMC projects in on the grant's behalf.
type resolvedProxy struct {
	host         string
	port         int
	noProxyCIDRs []string
	// Exactly one of these carries the proxy's public certificate: a same-namespace
	// TLS Secret for a colocated proxy, the projected ConfigMap for a shared one.
	tlsSecretName   string
	caConfigMapName string
}

// resolveSharedProxy resolves a cross-namespace proxyRef through the projection the
// GMC writes into the consumer's own namespace once the provider consents (§H.9).
//
// The projection's presence IS the grant. The AGC never evaluates
// allowedNamespaces itself and never reads the remote EgressProxy — it cannot, and
// that is the point: consent is decided by the GMC, which watches both sides, and
// revoking a grant deletes this ConfigMap so the reference fails closed here.
func resolveSharedProxy(ctx context.Context, reader client.Reader, consumerNS, proxyNS, proxyName string) (*resolvedProxy, refResolution) {
	name := proxyShareConfigMapName(proxyNS, proxyName)
	var cm corev1.ConfigMap
	if err := reader.Get(ctx, types.NamespacedName{Namespace: consumerNS, Name: name}, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, refResolution{reason: v2alpha1.ReasonProxyShareNotGranted,
				message: fmt.Sprintf("EgressProxy %q in namespace %q does not list namespace %q in spec.sharing.allowedNamespaces",
					proxyName, proxyNS, consumerNS)}
		}
		return nil, refResolution{err: fmt.Errorf("read projected proxy share: %w", err)}
	}

	host := cm.Data[proxyShareHostKey]
	if host == "" {
		return nil, refResolution{err: fmt.Errorf("projected proxy share %q carries no %s", name, proxyShareHostKey)}
	}
	port := proxyPort
	if raw := cm.Data[proxySharePortKey]; raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			return nil, refResolution{err: fmt.Errorf("projected proxy share %q carries an unparseable %s: %w", name, proxySharePortKey, err)}
		}
		port = parsed
	}
	var noProxy []string
	if raw := cm.Data[proxyShareNoProxyKey]; raw != "" {
		noProxy = strings.Split(raw, ",")
	}
	return &resolvedProxy{
		host:            host,
		port:            port,
		noProxyCIDRs:    noProxy,
		caConfigMapName: name,
	}, refResolution{}
}

// vanishedReferentReason upgrades a *NotFound resolution outcome to the matching
// *Deleted reason when the referent stopped resolving out from under a
// previously-resolved RunnerSet (degrade-not-block, §H.8): deleting a shared
// referent is allowed and degrades referrers rather than blocking, and the
// distinct reason tells the operator the referent existed and vanished rather
// than never having been applied. Prior resolution is read from the set's own
// status markers (templateSource; proxyMode Proxied), trusted only while the spec
// generation is unchanged — after a spec edit the markers may describe the
// pre-edit references, so the plain NotFound reason stands and the caller clears
// the stale marker (clearStaleResolutionMarkers) so later reconciles do not
// upgrade from it. Best-effort: a referent-side edit to a dangling name (e.g. the
// gateway's defaultTemplateRef renamed to a missing template) is indistinguishable
// from deletion without tracking resolved UIDs; the message names the missing
// referent either way.
func vanishedReferentReason(rs *v2alpha1.RunnerSet, res refResolution) (string, string) {
	if rs.Generation != rs.Status.ObservedGeneration {
		return res.reason, res.message
	}
	switch res.reason {
	case v2alpha1.ReasonTemplateNotFound:
		if rs.Status.TemplateSource != "" {
			return v2alpha1.ReasonTemplateDeleted,
				res.message + "; the previously-resolved template was deleted — no new worker pods until it is restored"
		}
	case v2alpha1.ReasonProxyNotFound:
		if rs.Status.ProxyMode == v2alpha1.ProxyModeProxied {
			return v2alpha1.ReasonProxyDeleted,
				res.message + "; the previously-resolved proxy was deleted — no new worker pods until it is restored"
		}
	}
	return res.reason, res.message
}

// clearStaleResolutionMarkers drops the status markers a plain *NotFound outcome
// invalidates: a set reporting TemplateNotFound resolved no template, so
// status.templateSource must not keep naming a rung; likewise ProxyNotFound
// invalidates proxyMode. The *Deleted reasons deliberately keep their marker —
// it is the evidence of the prior resolution that distinguishes them, and keeping
// it makes the reason stable across reconciles.
func clearStaleResolutionMarkers(rs *v2alpha1.RunnerSet, reason string) {
	switch reason {
	case v2alpha1.ReasonTemplateNotFound:
		rs.Status.TemplateSource = ""
	case v2alpha1.ReasonProxyNotFound, v2alpha1.ReasonProxyShareNotGranted:
		// A withdrawn grant invalidates proxyMode exactly as a deleted proxy does:
		// the set resolved no proxy, so it must not keep reporting Proxied.
		rs.Status.ProxyMode = ""
	}
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
func resolveTemplateChain(ctx context.Context, c client.Client, ns string, rs *v2alpha1.RunnerSet, gw *v2alpha1.ActionsGateway) (*v2alpha1.RunnerTemplateSpec, map[string]string, string, refResolution) {
	// Rung 1: the set's own explicit templateRef.
	if rs.Spec.TemplateRef != nil {
		spec, annotations, res := resolveTemplate(ctx, c, ns, *rs.Spec.TemplateRef)
		return spec, annotations, v2alpha1.TemplateSourceRef, res
	}
	// Rung 2: the gateway's defaultTemplateRef, inherited because templateRef is unset.
	if gw.Spec.DefaultTemplateRef != nil {
		spec, annotations, res := resolveTemplate(ctx, c, ns, *gw.Spec.DefaultTemplateRef)
		return spec, annotations, v2alpha1.TemplateSourceGatewayDefault, res
	}
	// Rung 3: the single cluster-default ClusterRunnerTemplate.
	spec, annotations, res := resolveClusterDefaultTemplate(ctx, c)
	return spec, annotations, v2alpha1.TemplateSourceClusterDefault, res
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
func resolveClusterDefaultTemplate(ctx context.Context, c client.Client) (*v2alpha1.RunnerTemplateSpec, map[string]string, refResolution) {
	var list v2alpha1.ClusterRunnerTemplateList
	if err := c.List(ctx, &list); err != nil {
		return nil, nil, refResolution{err: fmt.Errorf("list ClusterRunnerTemplates: %w", err)}
	}
	var defaults []*v2alpha1.ClusterRunnerTemplate
	for i := range list.Items {
		if list.Items[i].Annotations[v2alpha1.IsDefaultTemplateAnnotation] == v2alpha1.IsDefaultTemplateValue {
			defaults = append(defaults, &list.Items[i])
		}
	}
	switch len(defaults) {
	case 0:
		return nil, nil, refResolution{reason: v2alpha1.ReasonTemplateNotFound,
			message: fmt.Sprintf("no templateRef, no gateway defaultTemplateRef, and no ClusterRunnerTemplate marked %s=%s",
				v2alpha1.IsDefaultTemplateAnnotation, v2alpha1.IsDefaultTemplateValue)}
	case 1:
		return &defaults[0].Spec, defaults[0].Annotations, refResolution{}
	default:
		names := make([]string, len(defaults))
		for i, d := range defaults {
			names[i] = d.Name
		}
		sort.Strings(names)
		return nil, nil, refResolution{reason: v2alpha1.ReasonAmbiguousDefault,
			message: fmt.Sprintf("%d ClusterRunnerTemplates are marked the cluster default (%s); exactly one must be: %s",
				len(defaults), v2alpha1.IsDefaultTemplateAnnotation, strings.Join(names, ", "))}
	}
}

// resolveTemplate resolves a templateRef to a worker pod shape. kind selects the
// cluster-scoped ClusterRunnerTemplate; the default (empty/RunnerTemplate) is the
// namespaced RunnerTemplate. Both fail closed with TemplateNotFound when the
// referent is absent, so the set waits for the referent→referrer watch (§H.7).
func resolveTemplate(ctx context.Context, c client.Client, ns string, ref v2alpha1.ObjectRef) (*v2alpha1.RunnerTemplateSpec, map[string]string, refResolution) {
	if ref.Kind == "ClusterRunnerTemplate" {
		// Cluster-scoped read, authorized by the per-gateway ClusterRoleBinding to
		// agc-clusterrunnertemplate-reader the GMC creates (M3b). The kind is
		// platform-authored and holds golden (incl. privileged) templates (§H.7).
		crt := &v2alpha1.ClusterRunnerTemplate{}
		if err := c.Get(ctx, types.NamespacedName{Name: ref.Name}, crt); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, nil, refResolution{reason: v2alpha1.ReasonTemplateNotFound,
					message: fmt.Sprintf("ClusterRunnerTemplate %q not found", ref.Name)}
			}
			return nil, nil, refResolution{err: fmt.Errorf("read ClusterRunnerTemplate: %w", err)}
		}
		return &crt.Spec, crt.Annotations, refResolution{}
	}
	rt := &v2alpha1.RunnerTemplate{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: ref.Name}, rt); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil, refResolution{reason: v2alpha1.ReasonTemplateNotFound,
				message: fmt.Sprintf("RunnerTemplate %q not found in namespace %q", ref.Name, ns)}
		}
		return nil, nil, refResolution{err: fmt.Errorf("read RunnerTemplate: %w", err)}
	}
	return &rt.Spec, rt.Annotations, refResolution{}
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
