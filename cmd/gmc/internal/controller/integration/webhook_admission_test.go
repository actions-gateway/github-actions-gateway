//go:build integration

package integration_test

import (
	"context"
	"testing"

	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The tests in this file drive the GMC validating webhook through the real
// apiserver (client.Create / client.Update) rather than calling the validator
// function directly. The webhook is registered with the envtest apiserver in
// TestMain (failurePolicy=Fail), so a rejected request proves the admission
// path — apiserver -> webhook -> CA -> validator -> denial -> error to client —
// works end to end, not just the validation logic in isolation (Q75).

// TestWebhookAdmission_ScaleSetRunnerLabelUniqueness verifies the apiserver admits
// the first ScaleSet-protocol RunnerSet claiming a runnerLabel under a gateway,
// rejects a second ScaleSet set claiming the SAME label under the same gateway (a
// scale-set name collision at GitHub the CRD CEL cannot see), and still admits one
// claiming a DISTINCT label (Q264 P3).
func TestWebhookAdmission_ScaleSetRunnerLabelUniqueness(t *testing.T) {
	const nsName = "team-webhook-scaleset-label"
	createNamespace(t, nsName)

	first := newScaleSetRunnerSet("first-set", nsName, "gw", "linux")
	require.NoError(t, k8sClient.Create(ctx, first),
		"the first ScaleSet set claiming a label must be admitted")
	t.Cleanup(func() { _ = client.IgnoreNotFound(k8sClient.Delete(context.Background(), first)) })

	dup := newScaleSetRunnerSet("dup-set", nsName, "gw", "linux")
	err := k8sClient.Create(ctx, dup)
	t.Cleanup(func() { _ = client.IgnoreNotFound(k8sClient.Delete(context.Background(), dup)) })
	require.Error(t, err, "a second ScaleSet set claiming the same label under the same gateway must be rejected")
	assert.Contains(t, err.Error(), "already used by RunnerSet",
		"rejection must come from the GMC RunnerSet webhook")

	distinct := newScaleSetRunnerSet("distinct-set", nsName, "gw", "windows")
	require.NoError(t, k8sClient.Create(ctx, distinct),
		"a ScaleSet set claiming a distinct label must be admitted")
	t.Cleanup(func() { _ = client.IgnoreNotFound(k8sClient.Delete(context.Background(), distinct)) })
}

// TestWebhookAdmission_ClassicSetMayShareAScaleSetName covers the Classic half of the
// label guard through the real apiserver. A Classic set has no scale-set object, so it
// may reuse a first label a ScaleSet set already claims. The webhook rule names v2,
// which has no acquisitionProtocol field: the v2alpha1 write reaches the validator
// converted through the v2beta1 hub, carrying Classic only in the
// conversion.actions-gateway.com/acquisition-protocol annotation (Q1150). A validator
// that ignored it would read the set as ScaleSet and reject it as a collision.
func TestWebhookAdmission_ClassicSetMayShareAScaleSetName(t *testing.T) {
	const nsName = "team-webhook-classic-shares"
	createNamespace(t, nsName)

	scaleSet := newScaleSetRunnerSet("scaleset-set", nsName, "gw", "linux")
	require.NoError(t, k8sClient.Create(ctx, scaleSet), "the ScaleSet set claiming the name must be admitted")
	t.Cleanup(func() { _ = client.IgnoreNotFound(k8sClient.Delete(context.Background(), scaleSet)) })

	classic := newScaleSetRunnerSet("classic-set", nsName, "gw", "linux", "amd64")
	classic.Spec.AcquisitionProtocol = v2alpha1.AcquisitionProtocolClassic
	require.NoError(t, k8sClient.Create(ctx, classic),
		"a Classic set sharing a ScaleSet set's first label must be admitted: it registers no scale set")
	t.Cleanup(func() { _ = client.IgnoreNotFound(k8sClient.Delete(context.Background(), classic)) })

	// The control: the same first label on a second ScaleSet set is still refused, so
	// the admit above is the protocol's doing and not a guard that never ran.
	dup := newScaleSetRunnerSet("dup-set", nsName, "gw", "linux")
	err := k8sClient.Create(ctx, dup)
	t.Cleanup(func() { _ = client.IgnoreNotFound(k8sClient.Delete(context.Background(), dup)) })
	require.Error(t, err, "a second ScaleSet set claiming the same label must still be rejected")
	assert.Contains(t, err.Error(), "already used by RunnerSet")
}

// TestWebhookAdmission_ScaleSetUniquenessIsOnTheFirstLabelOnly covers what Q726 moved.
// Only the FIRST runnerLabel names the scale set at GitHub, so only it has to be
// unique. Labels after the first are ordinary match targets — "linux" across a dozen
// sets is the shape multi-label exists to serve — and which set an ambiguous runs-on
// reaches is GitHub's decision, not an admission-time collision.
func TestWebhookAdmission_ScaleSetUniquenessIsOnTheFirstLabelOnly(t *testing.T) {
	const nsName = "team-webhook-scaleset-multilabel"
	createNamespace(t, nsName)

	gpu := newScaleSetRunnerSet("gpu-set", nsName, "gw", "gpu", "linux", "cuda")
	require.NoError(t, k8sClient.Create(ctx, gpu), "a multi-label ScaleSet set must be admitted")
	t.Cleanup(func() { _ = client.IgnoreNotFound(k8sClient.Delete(context.Background(), gpu)) })

	// A distinct first label, sharing every later one.
	arm := newScaleSetRunnerSet("arm-set", nsName, "gw", "arm64", "linux", "cuda")
	require.NoError(t, k8sClient.Create(ctx, arm),
		"sets sharing only non-first labels must both be admitted")
	t.Cleanup(func() { _ = client.IgnoreNotFound(k8sClient.Delete(context.Background(), arm)) })

	// The same first label is still a scale-set name collision, whatever follows it.
	dup := newScaleSetRunnerSet("dup-first", nsName, "gw", "gpu", "arm64")
	err := k8sClient.Create(ctx, dup)
	t.Cleanup(func() { _ = client.IgnoreNotFound(k8sClient.Delete(context.Background(), dup)) })
	require.Error(t, err, "a second set whose FIRST label is already claimed must be rejected")
	assert.Contains(t, err.Error(), "already used by RunnerSet",
		"rejection must come from the GMC RunnerSet webhook")

	// And a first label that merely appears LATER in another set's list is free: that
	// set does not own the scale-set name.
	claimsLinux := newScaleSetRunnerSet("linux-set", nsName, "gw", "linux")
	require.NoError(t, k8sClient.Create(ctx, claimsLinux),
		"a label another set carries in a non-first position must remain claimable as a name")
	t.Cleanup(func() { _ = client.IgnoreNotFound(k8sClient.Delete(context.Background(), claimsLinux)) })
}

// TestWebhookAdmission_ScaleSetLabelIsUniquePerGitHubScope drives the Q791 guard
// through the real apiserver, where the webhook's reader is the manager's uncached
// API reader and the cluster-wide List actually has to be permitted by the GMC's
// ClusterRole — neither of which a fake reader can observe.
//
// A scale set is adopted BY NAME against the Actions service its gateway's githubURL
// reaches, so two tenants in different namespaces under ONE org that claim one first
// runnerLabel drive a single scale set, each AGC acquiring the other's jobs. Under two
// different orgs the same label is free.
func TestWebhookAdmission_ScaleSetLabelIsUniquePerGitHubScope(t *testing.T) {
	const (
		nsA   = "team-scaleset-scope-a"
		nsB   = "team-scaleset-scope-b"
		nsC   = "team-scaleset-scope-c"
		acme  = "https://github.com/acme-scope"
		other = "https://github.com/other-scope"
	)
	for _, ns := range []string{nsA, nsB, nsC} {
		createNamespace(t, ns)
	}

	create := func(t *testing.T, obj client.Object) error {
		t.Helper()
		err := k8sClient.Create(ctx, obj)
		t.Cleanup(func() { _ = client.IgnoreNotFound(k8sClient.Delete(context.Background(), obj)) })
		return err
	}

	// Two tenants on one org, one tenant on a different org.
	require.NoError(t, create(t, ghesAdmissionGateway("gw", nsA, acme, "")))
	require.NoError(t, create(t, ghesAdmissionGateway("gw", nsB, acme, "")))
	require.NoError(t, create(t, ghesAdmissionGateway("gw", nsC, other, "")))

	require.NoError(t, create(t, newScaleSetRunnerSet("first-set", nsA, "gw", "linux")),
		"the first set claiming a scale-set name must be admitted")

	err := create(t, newScaleSetRunnerSet("stealer", nsB, "gw", "linux"))
	require.Error(t, err, "a second tenant claiming that name under the SAME GitHub org must be rejected")
	assert.Contains(t, err.Error(), "already claimed by another RunnerSet")
	assert.NotContains(t, err.Error(), "first-set", "the other tenant's set must not be disclosed")
	assert.NotContains(t, err.Error(), nsA, "the other tenant's namespace must not be disclosed")

	require.NoError(t, create(t, newScaleSetRunnerSet("free-set", nsC, "gw", "linux")),
		"the same name under a DIFFERENT GitHub org registers into a different scale-set namespace")

	require.NoError(t, create(t, newScaleSetRunnerSet("distinct", nsB, "gw", "windows")),
		"a distinct name in the same org must stay admissible")
}

// TestWebhookAdmission_ScaleSetScopeClosesApplyOrderGap is the gateway half of Q791
// through the apiserver. A RunnerSet applied before its gateway has no resolvable
// GitHub scope and admits unchecked (§H.7), so the arriving gateway is the first
// object that can see the conflict — without that half the guard is bypassable by
// simply applying the RunnerSet first.
func TestWebhookAdmission_ScaleSetScopeClosesApplyOrderGap(t *testing.T) {
	const (
		nsA  = "team-scaleset-order-a"
		nsB  = "team-scaleset-order-b"
		acme = "https://github.com/acme-order"
	)
	createNamespace(t, nsA)
	createNamespace(t, nsB)

	create := func(t *testing.T, obj client.Object) error {
		t.Helper()
		err := k8sClient.Create(ctx, obj)
		t.Cleanup(func() { _ = client.IgnoreNotFound(k8sClient.Delete(context.Background(), obj)) })
		return err
	}

	require.NoError(t, create(t, ghesAdmissionGateway("gw", nsA, acme, "")))
	require.NoError(t, create(t, newScaleSetRunnerSet("held", nsA, "gw", "linux")))

	// Applied before its gateway exists: no scope to compare, so §H.7 admits it.
	require.NoError(t, create(t, newScaleSetRunnerSet("sneaky", nsB, "gw", "linux")),
		"a set whose gateway is not yet applied must be admitted (§H.7)")

	// The gateway that would put it in the same scope is what gets rejected.
	err := create(t, ghesAdmissionGateway("gw", nsB, acme, ""))
	require.Error(t, err, "the gateway binding the colliding scope must be rejected")
	assert.Contains(t, err.Error(), "spec.githubURL")
	assert.Contains(t, err.Error(), "sneaky", "its own namespace's referrer is named")
}

// newScaleSetRunnerSet builds a ScaleSet-protocol RunnerSet bound to the named gateway,
// whose first runnerLabel names its scale set (references are resolved at runtime, so
// no template need exist for admission).
func newScaleSetRunnerSet(name, ns, gateway, label string, more ...string) *v2alpha1.RunnerSet {
	return &v2alpha1.RunnerSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: v2alpha1.RunnerSetSpec{
			GatewayRef:          v2alpha1.ObjectRef{Name: gateway},
			AcquisitionProtocol: v2alpha1.AcquisitionProtocolScaleSet,
			RunnerLabels:        append([]string{label}, more...),
		},
	}
}
