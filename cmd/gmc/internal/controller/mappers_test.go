package controller

import (
	"context"
	"testing"

	agcv1alpha1 "github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	gmcv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// mapperTestAG returns an ActionsGateway referencing the given GitHubApp Secret
// name, for use as fake-client seed data in the mapper tests below.
func mapperTestAG(name, ns, secretName string) *gmcv1alpha1.ActionsGateway {
	return &gmcv1alpha1.ActionsGateway{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: gmcv1alpha1.ActionsGatewaySpec{
			GitHubAppRef: gmcv1alpha1.SecretReference{Name: secretName},
		},
	}
}

// TestSecretToActionsGateway_MatchesByNameAndNamespace verifies only the
// ActionsGateway(s) in the Secret's namespace that reference it by name are
// enqueued; a same-name Secret in another namespace and an unrelated Secret name
// in the same namespace must not match.
func TestSecretToActionsGateway_MatchesByNameAndNamespace(t *testing.T) {
	scheme := applyTestScheme(t)
	matching := mapperTestAG("tenant-a", "ns1", "gh-app-creds")
	otherSecret := mapperTestAG("tenant-b", "ns1", "other-creds")
	otherNS := mapperTestAG("tenant-c", "ns2", "gh-app-creds")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(matching, otherSecret, otherNS).Build()
	r := &ActionsGatewayReconciler{Client: c}

	secret := &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Name: "gh-app-creds", Namespace: "ns1"}}
	reqs := r.secretToActionsGateway(context.Background(), secret)

	require.Len(t, reqs, 1)
	assert.Equal(t, types.NamespacedName{Namespace: "ns1", Name: "tenant-a"}, reqs[0].NamespacedName)
}

// TestSecretToActionsGateway_NoMatchReturnsNil verifies a Secret referenced by no
// ActionsGateway in its namespace yields no requests.
func TestSecretToActionsGateway_NoMatchReturnsNil(t *testing.T) {
	scheme := applyTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mapperTestAG("tenant-a", "ns1", "other-creds")).Build()
	r := &ActionsGatewayReconciler{Client: c}

	secret := &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Name: "gh-app-creds", Namespace: "ns1"}}
	assert.Empty(t, r.secretToActionsGateway(context.Background(), secret))
}

// TestQuotaToActionsGateways_EnqueuesEveryGatewayInNamespace verifies a
// ResourceQuota event fans out to every ActionsGateway in the quota's namespace,
// regardless of what each gateway references, and ignores gateways elsewhere.
func TestQuotaToActionsGateways_EnqueuesEveryGatewayInNamespace(t *testing.T) {
	scheme := applyTestScheme(t)
	a := mapperTestAG("tenant-a", "ns1", "secret-a")
	b := mapperTestAG("tenant-b", "ns1", "secret-b")
	other := mapperTestAG("tenant-c", "ns2", "secret-c")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(a, b, other).Build()
	r := &ActionsGatewayReconciler{Client: c}

	quota := &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Name: "compute-quota", Namespace: "ns1"}}
	reqs := r.quotaToActionsGateways(context.Background(), quota)

	require.Len(t, reqs, 2)
	names := []string{reqs[0].Name, reqs[1].Name}
	assert.ElementsMatch(t, []string{"tenant-a", "tenant-b"}, names)
}

// TestQuotaToActionsGateways_EmptyNamespaceReturnsEmpty verifies a namespace with
// no ActionsGateways yields an empty (non-nil) slice rather than an error.
func TestQuotaToActionsGateways_EmptyNamespaceReturnsEmpty(t *testing.T) {
	scheme := applyTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &ActionsGatewayReconciler{Client: c}

	quota := &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Name: "compute-quota", Namespace: "empty-ns"}}
	assert.Empty(t, r.quotaToActionsGateways(context.Background(), quota))
}

// TestRunnerGroupToActionsGateway_MapsViaOwnerLabels verifies a RunnerGroup
// carrying both owner labels maps to exactly that ActionsGateway's request.
func TestRunnerGroupToActionsGateway_MapsViaOwnerLabels(t *testing.T) {
	r := &ActionsGatewayReconciler{}
	rg := &agcv1alpha1.RunnerGroup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tenant-a-0",
			Namespace: "ns1",
			Labels:    map[string]string{"actions-gateway/owner-name": "tenant-a", "actions-gateway/owner-ns": "ns1"},
		},
	}

	reqs := r.runnerGroupToActionsGateway(context.Background(), rg)

	require.Len(t, reqs, 1)
	assert.Equal(t, types.NamespacedName{Namespace: "ns1", Name: "tenant-a"}, reqs[0].NamespacedName)
}

// TestRunnerGroupToActionsGateway_MissingOwnerLabelsReturnsNil verifies a
// RunnerGroup missing either owner label maps to no request rather than panicking
// on an empty NamespacedName.
func TestRunnerGroupToActionsGateway_MissingOwnerLabelsReturnsNil(t *testing.T) {
	r := &ActionsGatewayReconciler{}

	noLabels := &agcv1alpha1.RunnerGroup{ObjectMeta: metav1.ObjectMeta{Name: "orphan", Namespace: "ns1"}}
	assert.Empty(t, r.runnerGroupToActionsGateway(context.Background(), noLabels))

	onlyOwnerName := &agcv1alpha1.RunnerGroup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "partial",
			Namespace: "ns1",
			Labels:    map[string]string{"actions-gateway/owner-name": "tenant-a"},
		},
	}
	assert.Empty(t, r.runnerGroupToActionsGateway(context.Background(), onlyOwnerName))
}

// TestRunnerGroupName_LabeledIsContentDerivedAndStable verifies a spec with a
// runner label produces a name derived from that label (not the index), and the
// same label always yields the same name.
func TestRunnerGroupName_LabeledIsContentDerivedAndStable(t *testing.T) {
	ag := &gmcv1alpha1.ActionsGateway{ObjectMeta: metav1.ObjectMeta{Name: "tenant"}}
	spec := agcv1alpha1.RunnerGroupSpec{RunnerLabels: []string{"gpu-a100"}}

	name0 := runnerGroupName(ag, spec, 0)
	name5 := runnerGroupName(ag, spec, 5)

	assert.Equal(t, name0, name5, "the name must be derived from the label, not the index")
	assert.Contains(t, name0, "tenant-")
	assert.Contains(t, name0, "gpu-a100")
}

// TestRunnerGroupName_UnlabeledFallsBackToIndex verifies a spec with no runner
// labels falls back to an index-based name, so reordering entries changes the
// generated name (pruneRunnerGroups relies on owner labels, not this name, to
// stay correct across reorders).
func TestRunnerGroupName_UnlabeledFallsBackToIndex(t *testing.T) {
	ag := &gmcv1alpha1.ActionsGateway{ObjectMeta: metav1.ObjectMeta{Name: "tenant"}}
	spec := agcv1alpha1.RunnerGroupSpec{}

	assert.Equal(t, "tenant-0", runnerGroupName(ag, spec, 0))
	assert.Equal(t, "tenant-3", runnerGroupName(ag, spec, 3))
}

// TestProvisioningError_UnwrapReturnsUnderlyingCause verifies Unwrap exposes the
// wrapped cause so errors.Is/As still see through the step annotation (Q156).
func TestProvisioningError_UnwrapReturnsUnderlyingCause(t *testing.T) {
	underlying := assertableErr{"boom"}
	pe := &provisioningError{step: "applyDeployment", err: underlying}

	assert.Equal(t, underlying, pe.Unwrap())
	assert.Equal(t, "applyDeployment: boom", pe.Error())
}

// assertableErr is a trivial comparable error type for exercising Unwrap.
type assertableErr struct{ msg string }

func (e assertableErr) Error() string { return e.msg }

// TestPruneRunnerGroups_DeletesOnlyUndesiredOwnedGroups verifies pruning deletes
// only this gateway's RunnerGroups absent from the desired set, leaving desired
// groups and another owner's groups untouched.
func TestPruneRunnerGroups_DeletesOnlyUndesiredOwnedGroups(t *testing.T) {
	scheme := applyTestScheme(t)
	ag := applyTestAG()
	keep := ownedRunnerGroup("tenant-keep", ag.Name, ag.Namespace)
	stale := ownedRunnerGroup("tenant-stale", ag.Name, ag.Namespace)
	otherOwner := ownedRunnerGroup("other-keep", "different-gateway", ag.Namespace)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(keep, stale, otherOwner).Build()
	r := &ActionsGatewayReconciler{Client: c}

	desired := map[string]struct{}{"tenant-keep": {}}
	require.NoError(t, r.pruneRunnerGroups(context.Background(), ag, desired))

	var rgList agcv1alpha1.RunnerGroupList
	require.NoError(t, c.List(context.Background(), &rgList))
	var remaining []string
	for _, rg := range rgList.Items {
		remaining = append(remaining, rg.Name)
	}
	assert.ElementsMatch(t, []string{"tenant-keep", "other-keep"}, remaining)
}

// TestPruneRunnerGroups_NoOwnedGroupsIsNoOp verifies pruning against an empty
// owned set (no RunnerGroups at all) succeeds without error.
func TestPruneRunnerGroups_NoOwnedGroupsIsNoOp(t *testing.T) {
	scheme := applyTestScheme(t)
	ag := applyTestAG()
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &ActionsGatewayReconciler{Client: c}

	require.NoError(t, r.pruneRunnerGroups(context.Background(), ag, map[string]struct{}{}))
}
