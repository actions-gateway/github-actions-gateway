package controller

import (
	"context"
	"errors"
	"testing"

	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// scopedGateway builds a v2 ActionsGateway bound to the given GitHub URL — the field
// that decides which scale-set namespace at GitHub its RunnerSets claim names in.
func scopedGateway(name, ns, gitHubURL string) *gmcv2alpha1.ActionsGateway {
	return &gmcv2alpha1.ActionsGateway{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       gmcv2alpha1.ActionsGatewaySpec{GitHubURL: gitHubURL},
	}
}

// claimingRunnerSet builds a ScaleSet RunnerSet whose first runnerLabel is the
// scale-set name it claims at GitHub.
func claimingRunnerSet(name, ns, gateway string, labels ...string) *gmcv2alpha1.RunnerSet {
	return &gmcv2alpha1.RunnerSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: gmcv2alpha1.RunnerSetSpec{
			GatewayRef:          gmcv2alpha1.ObjectRef{Name: gateway},
			AcquisitionProtocol: gmcv2alpha1.AcquisitionProtocolScaleSet,
			RunnerLabels:        labels,
		},
	}
}

func collisionReconciler(t *testing.T, objs ...client.Object) *ActionsGatewayV2Reconciler {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(actionsGatewayV2TestScheme(t)).WithObjects(objs...).Build()
	return &ActionsGatewayV2Reconciler{Client: c}
}

// TestEvalScaleSetNameCollisions_Clean: distinct first labels in one scope, and one
// label reused in a different scope, both leave the condition False with a reason that
// names the scope actually checked.
func TestEvalScaleSetNameCollisions_Clean(t *testing.T) {
	ag := scopedGateway("gw-a", "tenant-a", "https://github.com/acme")
	r := collisionReconciler(t,
		ag,
		scopedGateway("gw-b", "tenant-b", "https://github.com/other"),
		claimingRunnerSet("rs-a", "tenant-a", "gw-a", "acme-linux"),
		claimingRunnerSet("rs-b", "tenant-a", "gw-a", "acme-windows"),
		// Same label, different GitHub org: a different scale-set namespace entirely.
		claimingRunnerSet("rs-c", "tenant-b", "gw-b", "acme-linux"),
	)

	got := r.evalScaleSetNameCollisions(context.Background(), ag)
	assert.True(t, got.observed)
	assert.False(t, got.collided)
	assert.Equal(t, gmcv2alpha1.ReasonScaleSetNamesUnique, got.reason)
	assert.Contains(t, got.message, "github.com/acme")
}

// TestEvalScaleSetNameCollisions_ClassicIsUnaffected: a Classic set registers no
// scale-set object, so it claims no name and cannot collide (Q726).
func TestEvalScaleSetNameCollisions_ClassicIsUnaffected(t *testing.T) {
	ag := scopedGateway("gw-a", "tenant-a", "https://github.com/acme")
	classic := claimingRunnerSet("rs-classic", "tenant-b", "gw-b", "acme-linux")
	classic.Spec.AcquisitionProtocol = gmcv2alpha1.AcquisitionProtocolClassic

	r := collisionReconciler(t,
		ag,
		scopedGateway("gw-b", "tenant-b", "https://github.com/acme"),
		claimingRunnerSet("rs-a", "tenant-a", "gw-a", "acme-linux"),
		classic,
	)

	got := r.evalScaleSetNameCollisions(context.Background(), ag)
	assert.True(t, got.observed)
	assert.False(t, got.collided)
}

// TestEvalScaleSetNameCollisions_CrossTenant is the case admission never re-validates:
// two gateways in different namespaces bound to one GitHub org, each with a ScaleSet
// RunnerSet claiming the same first label. Both drive one scale set at GitHub.
func TestEvalScaleSetNameCollisions_CrossTenant(t *testing.T) {
	ag := scopedGateway("gw-a", "tenant-a", "https://github.com/acme")
	r := collisionReconciler(t,
		ag,
		// Case differs, which GitHub resolves to one org — the scope key is lowercased
		// for exactly this reason, so the collision must still be seen.
		scopedGateway("gw-b", "tenant-b", "https://github.com/Acme"),
		claimingRunnerSet("rs-a", "tenant-a", "gw-a", "shared-name", "linux"),
		claimingRunnerSet("rs-b", "tenant-b", "gw-b", "shared-name", "linux"),
	)

	got := r.evalScaleSetNameCollisions(context.Background(), ag)
	assert.True(t, got.observed)
	assert.True(t, got.collided)
	assert.Equal(t, gmcv2alpha1.ReasonScaleSetNameShared, got.reason)
	assert.Contains(t, got.message, `"rs-a" claims "shared-name"`)
	assert.Contains(t, got.message, "github.com/acme")
}

// TestEvalScaleSetNameCollisions_DoesNotNameACrossTenantHolder pins the
// non-enumeration property the admission error already holds (Q791): the gateway's
// status is readable by the tenant, so a message naming the other namespace or its
// RunnerSet would let anyone able to read one gateway probe another tenant's label
// usage. The holder belongs in the GMC log, which is the platform admin's.
func TestEvalScaleSetNameCollisions_DoesNotNameACrossTenantHolder(t *testing.T) {
	ag := scopedGateway("gw-a", "tenant-a", "https://github.com/acme")
	r := collisionReconciler(t,
		ag,
		scopedGateway("victim-gateway", "victim-namespace", "https://github.com/acme"),
		claimingRunnerSet("rs-a", "tenant-a", "gw-a", "shared-name"),
		claimingRunnerSet("victim-runnerset", "victim-namespace", "victim-gateway", "shared-name"),
	)

	got := r.evalScaleSetNameCollisions(context.Background(), ag)
	require.True(t, got.collided)
	assert.NotContains(t, got.message, "victim-namespace")
	assert.NotContains(t, got.message, "victim-runnerset")
	assert.NotContains(t, got.message, "victim-gateway")
}

// TestEvalScaleSetNameCollisions_SameNamespaceHolderIsNamed: the tenant owns both
// objects, so naming its own colliding set discloses nothing and is what makes the
// message actionable.
func TestEvalScaleSetNameCollisions_SameNamespaceHolderIsNamed(t *testing.T) {
	ag := scopedGateway("gw-a", "tenant-a", "https://github.com/acme")
	r := collisionReconciler(t,
		ag,
		claimingRunnerSet("rs-one", "tenant-a", "gw-a", "shared-name"),
		claimingRunnerSet("rs-two", "tenant-a", "gw-a", "shared-name"),
	)

	got := r.evalScaleSetNameCollisions(context.Background(), ag)
	require.True(t, got.collided)
	assert.Contains(t, got.message, `"rs-one" claims "shared-name"`)
	assert.Contains(t, got.message, `"rs-two" claims "shared-name"`)
	assert.Contains(t, got.message, "2 RunnerSet(s)")
}

// TestEvalScaleSetNameCollisions_OtherGatewaysCollisionIsNotOurs: a pair that does not
// involve this gateway's own RunnerSets leaves its condition False. The two gateways
// holding the pair report it themselves.
func TestEvalScaleSetNameCollisions_OtherGatewaysCollisionIsNotOurs(t *testing.T) {
	ag := scopedGateway("gw-a", "tenant-a", "https://github.com/acme")
	r := collisionReconciler(t,
		ag,
		scopedGateway("gw-b", "tenant-b", "https://github.com/acme"),
		scopedGateway("gw-c", "tenant-c", "https://github.com/acme"),
		claimingRunnerSet("rs-a", "tenant-a", "gw-a", "mine-alone"),
		claimingRunnerSet("rs-b", "tenant-b", "gw-b", "theirs"),
		claimingRunnerSet("rs-c", "tenant-c", "gw-c", "theirs"),
	)

	got := r.evalScaleSetNameCollisions(context.Background(), ag)
	assert.True(t, got.observed)
	assert.False(t, got.collided)
}

// TestEvalScaleSetNameCollisions_ReadFailureIsNotAVerdict: an unreadable inventory
// must not report a clean scope. observed=false tells updateStatus to leave the last
// condition standing rather than write False from a read that did not happen.
func TestEvalScaleSetNameCollisions_ReadFailureIsNotAVerdict(t *testing.T) {
	ag := scopedGateway("gw-a", "tenant-a", "https://github.com/acme")
	c := fake.NewClientBuilder().
		WithScheme(actionsGatewayV2TestScheme(t)).
		WithObjects(ag).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				return errors.New("apiserver unavailable")
			},
		}).Build()
	r := &ActionsGatewayV2Reconciler{Client: c}

	got := r.evalScaleSetNameCollisions(context.Background(), ag)
	assert.False(t, got.observed)
	assert.False(t, got.collided)
	assert.Empty(t, got.reason)
}
