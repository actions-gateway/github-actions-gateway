package controller

import (
	"context"
	"net"
	"testing"
	"time"

	gmcv2 "github.com/actions-gateway/github-actions-gateway/api/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// directEgressFixture is a provisioned-able direct-egress gateway on a fake client.
func directEgressFixture(t *testing.T, cache *IPRangeCache) (*ActionsGatewayV2Reconciler, client.Client, *gmcv2.ActionsGateway) {
	t.Helper()
	scheme := actionsGatewayV2TestScheme(t)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "github-app", Namespace: "team-a"}}
	ag := v2Gateway("gw", "team-a", "github-app", "")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ns, secret, ag).WithStatusSubresource(ag).Build()
	r := &ActionsGatewayV2Reconciler{Client: c, Scheme: scheme, AGCImage: "agc:test", IPCache: cache,
		EgressStaleThreshold: 49 * time.Hour}
	return r, c, ag
}

func filledCache(t *testing.T, refreshed time.Time) *IPRangeCache {
	t.Helper()
	_, cidr, err := net.ParseCIDR("140.82.112.0/20")
	require.NoError(t, err)
	cache := &IPRangeCache{}
	cache.Set([]net.IPNet{*cidr})
	cache.MarkRefreshed(refreshed)
	return cache
}

func egressRulesStale(t *testing.T, c client.Client, ag *gmcv2.ActionsGateway) *metav1.Condition {
	t.Helper()
	var got gmcv2.ActionsGateway
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(ag), &got))
	cond := meta.FindStatusCondition(got.Status.Conditions, gmcv2.ConditionEgressRulesStale)
	require.NotNil(t, cond, "a direct-egress gateway carries EgressRulesStale")
	return cond
}

// A cache refreshed within the window reports current.
func TestDirectEgressRulesStale_Current(t *testing.T) {
	r, c, ag := directEgressFixture(t, filledCache(t, time.Now().Add(-2*time.Hour)))
	reconcileV2Gateway(t, r, "team-a", "gw")

	cond := egressRulesStale(t, c, ag)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, gmcv2.ReasonRefreshCurrent, cond.Reason)
}

// Never filled, first provision: the policies are created with no GitHub rule, so
// GitHub egress is denied for both the AGC and workers.
func TestDirectEgressRulesStale_NeverFilledFirstProvisionDenies(t *testing.T) {
	r, c, ag := directEgressFixture(t, &IPRangeCache{})
	reconcileV2Gateway(t, r, "team-a", "gw")

	cond := egressRulesStale(t, c, ag)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, gmcv2.ReasonRefreshNeverSucceeded, cond.Reason)
	assert.Contains(t, cond.Message, `"gw-agc" carries no GitHub rule, so GitHub egress from the AGC is denied`)
	assert.Contains(t, cond.Message, `"gw-workload" carries no GitHub rule, so GitHub egress from workers is denied`)
}

// Never filled, policies already programmed (a GMC restart with no persisted ranges):
// preserveExistingEgress keeps their GitHub rule, so the condition is True but must
// not tell the operator egress is denied.
func TestDirectEgressRulesStale_NeverFilledPreservedRulesServe(t *testing.T) {
	r, c, ag := directEgressFixture(t, filledCache(t, time.Now()))
	reconcileV2Gateway(t, r, "team-a", "gw")
	r.IPCache = &IPRangeCache{}
	reconcileV2Gateway(t, r, "team-a", "gw")

	var wnp networkingv1.NetworkPolicy
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "team-a", Name: "gw-workload"}, &wnp))
	require.True(t, hasGitHubCIDREgress(&wnp, "140.82.112.0/20"), "precondition: the programmed rule was preserved")

	cond := egressRulesStale(t, c, ag)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, gmcv2.ReasonRefreshNeverSucceeded, cond.Reason)
	assert.Contains(t, cond.Message, "keep the GitHub ranges they were last programmed with")
	assert.NotContains(t, cond.Message, "denied")
}

// A policy deleted while the cache is empty is recreated with no GitHub rule; only it
// is named as denying.
func TestDirectEgressRulesStale_NeverFilledRecreatedPolicyNamed(t *testing.T) {
	r, c, ag := directEgressFixture(t, filledCache(t, time.Now()))
	reconcileV2Gateway(t, r, "team-a", "gw")
	r.IPCache = &IPRangeCache{}
	require.NoError(t, c.Delete(context.Background(), &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "gw-workload"}}))
	reconcileV2Gateway(t, r, "team-a", "gw")

	cond := egressRulesStale(t, c, ag)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Contains(t, cond.Message, `"gw-workload" carries no GitHub rule`)
	assert.NotContains(t, cond.Message, `"gw-agc"`)
}

// Stalled with ranges still served: the policies carry the last fetched set, which
// may have drifted.
func TestDirectEgressRulesStale_StalledServingRanges(t *testing.T) {
	r, c, ag := directEgressFixture(t, filledCache(t, time.Now().Add(-72*time.Hour)))
	reconcileV2Gateway(t, r, "team-a", "gw")

	cond := egressRulesStale(t, c, ag)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, gmcv2.ReasonRefreshStalled, cond.Reason)
	assert.Contains(t, cond.Message, "silently dropped")
}

// Stalled on an expired seed: the cache serves nothing, but the programmed policies
// keep their rule, so the shared stalled message's "egress is denied" would be wrong.
func TestDirectEgressRulesStale_StalledExpiredSeedPreservedRulesServe(t *testing.T) {
	r, c, ag := directEgressFixture(t, filledCache(t, time.Now()))
	reconcileV2Gateway(t, r, "team-a", "gw")
	now := time.Now()
	expired := &IPRangeCache{now: func() time.Time { return now }}
	expired.Seed(nil, now.Add(-50*time.Hour), now.Add(-time.Hour))
	r.IPCache = expired
	reconcileV2Gateway(t, r, "team-a", "gw")

	cond := egressRulesStale(t, c, ag)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, gmcv2.ReasonRefreshStalled, cond.Reason)
	assert.Contains(t, cond.Message, "keep the GitHub ranges they were last programmed with")
	assert.NotContains(t, cond.Message, "denied")
}

// A proxied gateway's policies carry no GitHub rule; its pool reports the allowlist.
func TestDirectEgressRulesStale_ProxiedIsPending(t *testing.T) {
	r := &ActionsGatewayV2Reconciler{IPCache: &IPRangeCache{}}
	ep := &gmcv2.EgressProxy{ObjectMeta: metav1.ObjectMeta{Name: "pool"}}
	es := r.evalDirectEgressRulesStale(context.Background(), v2Gateway("gw", "team-a", "s", "pool"), ep, time.Now())
	assert.False(t, es.stale, "an empty cache is the pool's to report, not a proxied gateway's")
	assert.Equal(t, gmcv2.ReasonRefreshPending, es.reason)
}

func TestDirectEgressRulesStale_NilCacheIsPending(t *testing.T) {
	r := &ActionsGatewayV2Reconciler{}
	es := r.evalDirectEgressRulesStale(context.Background(), v2Gateway("gw", "team-a", "s", ""), nil, time.Now())
	assert.False(t, es.stale)
	assert.Equal(t, gmcv2.ReasonRefreshPending, es.reason)
}

// The refresh loop never wakes this reconciler, so a direct gateway requeues on a
// fraction of the window, and every minute until the first fetch lands.
func TestDirectEgressRecheckRequeue(t *testing.T) {
	r, c, ag := directEgressFixture(t, filledCache(t, time.Now()))
	reconcileV2Gateway(t, r, "team-a", "gw")
	// A ready AGC drops the not-ready requeue, leaving the recheck as the only one.
	var dep appsv1.Deployment
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "team-a", Name: agcNameV2(ag)}, &dep))
	dep.Status.ReadyReplicas = 1
	require.NoError(t, c.Status().Update(context.Background(), &dep))
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ag)})
	require.NoError(t, err)
	assert.Equal(t, 49*time.Hour/8, res.RequeueAfter)

	assert.Equal(t, time.Minute, (&ActionsGatewayV2Reconciler{IPCache: &IPRangeCache{}}).directEgressRecheckRequeue(nil))
	assert.Zero(t, (&ActionsGatewayV2Reconciler{IPCache: &IPRangeCache{}}).directEgressRecheckRequeue(&gmcv2.EgressProxy{}))
	assert.Zero(t, (&ActionsGatewayV2Reconciler{}).directEgressRecheckRequeue(nil))
}

// A Vault rule on 443 to a CIDR has the GitHub rule's shape and must not read as one.
func TestHasGitHubCIDREgressRule_IgnoresVaultLookalike(t *testing.T) {
	ag := v2WorkloadIdentityGateway("gw", "team-a", "")
	ag.Spec.Credentials.WorkloadIdentity.Signer.Vault.Address = "https://vault.example.com"
	ag.Spec.Credentials.WorkloadIdentity.Signer.Vault.NetworkPolicy = &gmcv2.EgressPeer{CIDR: "10.0.0.0/24"}
	vault, ok := vaultEgressRule(ag)
	require.True(t, ok)
	require.Equal(t, 443, vault.Ports[0].Port.IntValue(), "precondition: the lookalike is on 443")

	assert.False(t, hasGitHubCIDREgressRule([]networkingv1.NetworkPolicyEgressRule{vault}, []networkingv1.NetworkPolicyEgressRule{vault}))
	gh, _ := githubCIDREgressRule([]net.IPNet{{IP: net.IPv4(140, 82, 112, 0), Mask: net.CIDRMask(20, 32)}})
	assert.True(t, hasGitHubCIDREgressRule([]networkingv1.NetworkPolicyEgressRule{vault, gh}, []networkingv1.NetworkPolicyEgressRule{vault}))
	apiserver := agcAPIServerEgressRule([]string{"10.1.0.1/32"})
	assert.False(t, hasGitHubCIDREgressRule([]networkingv1.NetworkPolicyEgressRule{apiserver}, []networkingv1.NetworkPolicyEgressRule{vault}))
}
