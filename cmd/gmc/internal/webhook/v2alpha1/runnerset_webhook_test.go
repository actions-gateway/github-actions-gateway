package v2alpha1

import (
	"context"
	"testing"

	agcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/allowlist"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// runnerSetValidatorWith returns a validator whose reader is a fake client preloaded
// with the given sibling RunnerSets, so the ScaleSet label-uniqueness guard can be
// exercised without a live apiserver. Production wires mgr.GetAPIReader().
func runnerSetValidatorWith(t *testing.T, existing ...*agcv2alpha1.RunnerSet) *RunnerSetCustomValidator {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, agcv2alpha1.AddToScheme(scheme))
	objs := make([]client.Object, 0, len(existing))
	for _, rs := range existing {
		objs = append(objs, rs)
	}
	return &RunnerSetCustomValidator{
		reader: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build(),
	}
}

// scaleSetRS builds a ScaleSet-protocol RunnerSet with a single runnerLabel bound to
// the named gateway.
func scaleSetRS(name, namespace, gateway, label string) *agcv2alpha1.RunnerSet {
	return &agcv2alpha1.RunnerSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agcv2alpha1.RunnerSetSpec{
			GatewayRef:          agcv2alpha1.ObjectRef{Name: gateway},
			AcquisitionProtocol: agcv2alpha1.AcquisitionProtocolScaleSet,
			RunnerLabels:        []string{label},
		},
	}
}

// classicRS builds a Classic-protocol RunnerSet (default) with the given labels.
func classicRS(name, namespace, gateway string, labels ...string) *agcv2alpha1.RunnerSet {
	return &agcv2alpha1.RunnerSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agcv2alpha1.RunnerSetSpec{
			GatewayRef:          agcv2alpha1.ObjectRef{Name: gateway},
			AcquisitionProtocol: agcv2alpha1.AcquisitionProtocolClassic,
			RunnerLabels:        labels,
		},
	}
}

// TestRunnerSetWebhook_ProxyGitHubBypass covers the RunnerSet corner of the Q322
// guard: a proxyRef naming an EgressProxy whose noProxyCIDRs exclude the gateway's
// GitHub host (a GHES host in particular) is rejected; missing referents admit
// (§H.7 — the arriving object's own admission closes the pair); a set with no
// proxyRef is never checked (the inherited defaultProxyRef pair is the gateway's own
// admission's job); and read errors fail closed.
func TestRunnerSetWebhook_ProxyGitHubBypass(t *testing.T) {
	ctx := context.Background()
	newRS := func(proxy string) *agcv2alpha1.RunnerSet {
		rs := classicRS("rs", "team-a", "gw", "linux")
		if proxy != "" {
			rs.Spec.ProxyRef = &agcv2alpha1.ProxyObjectRef{Name: proxy}
		}
		return rs
	}

	t.Run("excluded gateway host rejected", func(t *testing.T) {
		gw := v2Gateway("team-a", "gw", "https://ghes.corp.example/my-org", "")
		ep := proxyWithNoProxy("team-a", "ep", "ghes.corp.example")
		v := &RunnerSetCustomValidator{reader: fakeReader(t, gw, ep)}
		_, err := v.ValidateCreate(ctx, newRS("ep"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "spec.proxyRef")
		assert.Contains(t, err.Error(), "ghes.corp.example")
		assert.Contains(t, err.Error(), "around the per-tenant egress proxy")

		_, err = v.ValidateUpdate(ctx, newRS(""), newRS("ep"))
		require.Error(t, err, "adding the proxyRef on update assembles the same pair")
	})

	t.Run("internal-only proxy admits", func(t *testing.T) {
		gw := v2Gateway("team-a", "gw", "https://ghes.corp.example/my-org", "")
		ep := proxyWithNoProxy("team-a", "ep", "10.0.0.0/8", "svc.cluster.local")
		v := &RunnerSetCustomValidator{reader: fakeReader(t, gw, ep)}
		_, err := v.ValidateCreate(ctx, newRS("ep"))
		require.NoError(t, err)
	})

	t.Run("missing gateway admits", func(t *testing.T) {
		ep := proxyWithNoProxy("team-a", "ep", "ghes.corp.example")
		v := &RunnerSetCustomValidator{reader: fakeReader(t, ep)}
		_, err := v.ValidateCreate(ctx, newRS("ep"))
		require.NoError(t, err)
	})

	t.Run("missing proxy admits", func(t *testing.T) {
		gw := v2Gateway("team-a", "gw", "https://ghes.corp.example/my-org", "")
		v := &RunnerSetCustomValidator{reader: fakeReader(t, gw)}
		_, err := v.ValidateCreate(ctx, newRS("ep"))
		require.NoError(t, err)
	})

	t.Run("no proxyRef is never checked", func(t *testing.T) {
		gw := v2Gateway("team-a", "gw", "https://ghes.corp.example/my-org", "ep")
		ep := proxyWithNoProxy("team-a", "ep", "ghes.corp.example")
		v := &RunnerSetCustomValidator{reader: fakeReader(t, gw, ep)}
		_, err := v.ValidateCreate(ctx, newRS(""))
		require.NoError(t, err, "the inherited defaultProxyRef pair is validated on the gateway, not the set")
	})

	t.Run("read error fails closed", func(t *testing.T) {
		v := &RunnerSetCustomValidator{reader: failingReader{}}
		_, err := v.ValidateCreate(ctx, newRS("ep"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cannot verify")
	})
}

func TestRunnerSetWebhook_ClassicIsNeverChecked(t *testing.T) {
	// Two Classic sets sharing a label under one gateway are fine: no scale-set
	// object exists, so there is no name collision at GitHub. Even with a colliding
	// ScaleSet sibling preloaded, a Classic create is admitted.
	v := runnerSetValidatorWith(t, scaleSetRS("existing", "tenant", "gw", "linux"))
	_, err := v.ValidateCreate(context.Background(), classicRS("newset", "tenant", "gw", "linux", "amd64"))
	require.NoError(t, err)
}

func TestRunnerSetWebhook_RejectsDuplicateScaleSetLabel(t *testing.T) {
	v := runnerSetValidatorWith(t, scaleSetRS("existing", "tenant", "gw", "linux"))
	_, err := v.ValidateCreate(context.Background(), scaleSetRS("newset", "tenant", "gw", "linux"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "linux")
	assert.Contains(t, err.Error(), "existing")
}

func TestRunnerSetWebhook_AllowsDistinctScaleSetLabels(t *testing.T) {
	v := runnerSetValidatorWith(t, scaleSetRS("existing", "tenant", "gw", "linux"))
	_, err := v.ValidateCreate(context.Background(), scaleSetRS("newset", "tenant", "gw", "windows"))
	require.NoError(t, err)
}

func TestRunnerSetWebhook_SameLabelDifferentGatewayIsAllowed(t *testing.T) {
	// Two gateways register their scale sets against different GitHub bindings, so
	// the same label under different gateways cannot collide.
	v := runnerSetValidatorWith(t, scaleSetRS("existing", "tenant", "gw-a", "linux"))
	_, err := v.ValidateCreate(context.Background(), scaleSetRS("newset", "tenant", "gw-b", "linux"))
	require.NoError(t, err)
}

func TestRunnerSetWebhook_SameLabelDifferentNamespaceIsAllowed(t *testing.T) {
	v := runnerSetValidatorWith(t, scaleSetRS("existing", "tenant-a", "gw", "linux"))
	_, err := v.ValidateCreate(context.Background(), scaleSetRS("newset", "tenant-b", "gw", "linux"))
	require.NoError(t, err)
}

func TestRunnerSetWebhook_DuplicateAgainstClassicSiblingIsAllowed(t *testing.T) {
	// A Classic sibling with the same label does not register a scale set, so it
	// cannot collide with a new ScaleSet set claiming that label.
	v := runnerSetValidatorWith(t, classicRS("existing", "tenant", "gw", "linux"))
	_, err := v.ValidateCreate(context.Background(), scaleSetRS("newset", "tenant", "gw", "linux"))
	require.NoError(t, err)
}

func TestRunnerSetWebhook_UpdateOntoCollidingLabelIsRejected(t *testing.T) {
	// The set already exists under a distinct label; an update that moves it onto a
	// sibling's label is rejected (acquisitionProtocol is immutable, but labels are not).
	sibling := scaleSetRS("sibling", "tenant", "gw", "linux")
	self := scaleSetRS("self", "tenant", "gw", "windows")
	v := runnerSetValidatorWith(t, sibling, self)

	moved := scaleSetRS("self", "tenant", "gw", "linux")
	_, err := v.ValidateUpdate(context.Background(), self, moved)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sibling")
}

func TestRunnerSetWebhook_UpdateSelfNoOpIsAllowed(t *testing.T) {
	// An update that does not change the label must not self-collide with the set's
	// own persisted copy in the list.
	self := scaleSetRS("self", "tenant", "gw", "linux")
	v := runnerSetValidatorWith(t, self)
	_, err := v.ValidateUpdate(context.Background(), self, scaleSetRS("self", "tenant", "gw", "linux"))
	require.NoError(t, err)
}

func TestRunnerSetWebhook_NilReaderSkips(t *testing.T) {
	// The direct-construction path (no reader) is a no-op — production always wires one.
	v := &RunnerSetCustomValidator{}
	_, err := v.ValidateCreate(context.Background(), scaleSetRS("newset", "tenant", "gw", "linux"))
	require.NoError(t, err)
}

// tieredRS builds a Classic RunnerSet whose priorityTiers name the given classes.
func tieredRS(name string, classes ...string) *agcv2alpha1.RunnerSet {
	rs := classicRS(name, "tenant", "gw", "linux")
	for i, c := range classes {
		rs.Spec.PriorityTiers = append(rs.Spec.PriorityTiers, agcv2alpha1.PriorityTier{
			PriorityClassName: c,
			Threshold:         int32(10 * (i + 1)),
		})
	}
	return rs
}

func TestRunnerSetWebhook_NilAllowlistRejectsAnyTierClass(t *testing.T) {
	// The secure default: no allowlist wired forbids every named class — including
	// the zero-setup escalation Kubernetes ships in every cluster.
	v := &RunnerSetCustomValidator{}
	_, err := v.ValidateCreate(context.Background(), tieredRS("newset", "system-cluster-critical"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "priorityTiers[0]")
	assert.Contains(t, err.Error(), "system-cluster-critical")
}

func TestRunnerSetWebhook_AllowsAllowlistedTierClasses(t *testing.T) {
	v := &RunnerSetCustomValidator{PriorityClasses: allowlist.New([]string{"runner-standard", "runner-bursty"})}
	_, err := v.ValidateCreate(context.Background(), tieredRS("newset", "runner-standard", "runner-bursty"))
	require.NoError(t, err)
}

func TestRunnerSetWebhook_RejectsOffAllowlistTierClass(t *testing.T) {
	// The second tier is off-allowlist; the rejection names the offending index.
	v := &RunnerSetCustomValidator{PriorityClasses: allowlist.New([]string{"runner-standard"})}
	_, err := v.ValidateCreate(context.Background(), tieredRS("newset", "runner-standard", "system-cluster-critical"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "priorityTiers[1]")
	assert.Contains(t, err.Error(), "system-cluster-critical")
}

func TestRunnerSetWebhook_RejectsEmptyTierClassName(t *testing.T) {
	// A tier's priorityClassName is required — unlike podTemplate.spec, the empty
	// string is a misconfiguration, not "unset", so it is off-allowlist too.
	v := &RunnerSetCustomValidator{PriorityClasses: allowlist.New([]string{"runner-standard"})}
	_, err := v.ValidateCreate(context.Background(), tieredRS("newset", ""))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "priorityTiers[0]")
}

func TestRunnerSetWebhook_UpdateCannotSmuggleTierClass(t *testing.T) {
	// An existing (possibly pre-gate) RunnerSet cannot be edited onto an
	// off-allowlist class.
	v := &RunnerSetCustomValidator{PriorityClasses: allowlist.New([]string{"runner-standard"})}
	old := tieredRS("self", "runner-standard")
	_, err := v.ValidateUpdate(context.Background(), old, tieredRS("self", "system-cluster-critical"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "priorityTiers[0]")
}

// TestRunnerSetWebhook_DeletionOnlyUpdateExemption covers the Q518 exemption: the
// AGC's finalizer-removal write on a deleting RunnerSet naming a since-removed
// class must be admitted, or teardown wedges (Q499). Only deletion-only writes
// are exempt — live objects and spec changes on deleting ones stay denied.
func TestRunnerSetWebhook_DeletionOnlyUpdateExemption(t *testing.T) {
	v := &RunnerSetCustomValidator{PriorityClasses: allowlist.New([]string{"runner-standard"})}
	now := metav1.Now()

	deleting := func(finalizers ...string) *agcv2alpha1.RunnerSet {
		rs := tieredRS("self", "removed-class")
		rs.DeletionTimestamp = &now
		rs.Finalizers = finalizers
		return rs
	}

	t.Run("finalizer removal on a deleting set is admitted", func(t *testing.T) {
		_, err := v.ValidateUpdate(context.Background(),
			deleting("actions-gateway.com/agentpool-cleanup"), deleting())
		require.NoError(t, err)
	})

	t.Run("the same write on a live set is still denied", func(t *testing.T) {
		old := tieredRS("self", "removed-class")
		old.Finalizers = []string{"actions-gateway.com/agentpool-cleanup"}
		_, err := v.ValidateUpdate(context.Background(), old, tieredRS("self", "removed-class"))
		require.Error(t, err, "live objects keep the stored-object re-validation")
	})

	t.Run("a spec change on a deleting set is still denied", func(t *testing.T) {
		changed := deleting()
		changed.Spec.RunnerLabels = append(changed.Spec.RunnerLabels, "extra")
		_, err := v.ValidateUpdate(context.Background(), deleting("actions-gateway.com/agentpool-cleanup"), changed)
		require.Error(t, err, "the exemption must not admit spec changes on a deleting object")
	})
}

func TestRunnerSetWebhook_NoTiersNeedsNoAllowlist(t *testing.T) {
	// A RunnerSet with no priorityTiers is admitted under the secure default — the
	// gate must not forbid ordinary, unprioritized runner sets.
	v := &RunnerSetCustomValidator{}
	_, err := v.ValidateCreate(context.Background(), classicRS("newset", "tenant", "gw", "linux"))
	require.NoError(t, err)
}
