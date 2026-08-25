//go:build integration

package integration_test

import (
	"testing"

	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	v2beta1 "github.com/actions-gateway/github-actions-gateway/api/v2beta1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// These tests exercise the Q74 conversion webhook end-to-end against the real
// apiserver: v2beta1 is the storage/hub version and v2alpha1 the served spoke, so a
// create/read in one version is converted through the GMC-hosted /convert endpoint
// (envtest redirects each convertible CRD's conversion to the suite webhook server).
// Only envtest can observe this — a fake client bypasses conversion entirely.

// The conversion annotations are pinned here as literals (mirroring the api-package
// unit test) so the suite also guards the on-the-wire key format a stored hub carries.
const (
	convAnnAcquisitionProtocol = "conversion.actions-gateway.com/acquisition-protocol"
	convAnnMaxListeners        = "conversion.actions-gateway.com/max-listeners"
)

// TestV2Conversion_RunnerSet_ClassicRoundTrip creates a v2alpha1 Classic, multi-label
// RunnerSet (a shape v2beta1 cannot express) and proves it survives storage as the
// ScaleSet-only hub and round-trips back unchanged: the two dropped protocol fields
// ride across as conversion annotations on the hub and are restored — never silently
// re-protocol'd — on the v2alpha1 read.
func TestV2Conversion_RunnerSet_ClassicRoundTrip(t *testing.T) {
	const ns = "v2-conv-classic"
	createNamespace(t, ns)

	orig := &v2alpha1.RunnerSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "classic-set",
			Namespace:   ns,
			Annotations: map[string]string{"user.example.com/note": "keep me"},
		},
		Spec: v2alpha1.RunnerSetSpec{
			GatewayRef:          v2alpha1.ObjectRef{Name: "gw"},
			AcquisitionProtocol: v2alpha1.AcquisitionProtocolClassic,
			RunnerLabels:        []string{"linux", "self-hosted"},
			MaxListeners:        20,
		},
	}
	require.NoError(t, k8sClient.Create(ctx, orig))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, orig) })

	// Read the stored object as the v2beta1 hub: the protocol fields are gone from the
	// schema but preserved as conversion annotations, and the user annotation survives.
	var hub v2beta1.RunnerSet
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "classic-set"}, &hub))
	assert.Equal(t, "Classic", hub.Annotations[convAnnAcquisitionProtocol], "hub must carry the acquisitionProtocol annotation")
	assert.Equal(t, "20", hub.Annotations[convAnnMaxListeners], "hub must carry the maxListeners annotation")
	assert.Equal(t, "keep me", hub.Annotations["user.example.com/note"], "hub must preserve the user annotation")
	assert.Equal(t, []string{"linux", "self-hosted"}, hub.Spec.RunnerLabels, "hub must preserve all runnerLabels")

	// Read it back as v2alpha1: the protocol fields are restored and the conversion
	// annotations are stripped from the surfaced view.
	var back v2alpha1.RunnerSet
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "classic-set"}, &back))
	assert.Equal(t, v2alpha1.AcquisitionProtocolClassic, back.Spec.AcquisitionProtocol)
	assert.Equal(t, int32(20), back.Spec.MaxListeners)
	assert.Equal(t, []string{"linux", "self-hosted"}, back.Spec.RunnerLabels)
	assert.NotContains(t, back.Annotations, convAnnAcquisitionProtocol, "conversion annotation must not leak into the v2alpha1 view")
	assert.NotContains(t, back.Annotations, convAnnMaxListeners, "conversion annotation must not leak into the v2alpha1 view")
	assert.Equal(t, "keep me", back.Annotations["user.example.com/note"])
}

// TestV2Conversion_RunnerSet_NativeHubDefaults creates a v2beta1-native RunnerSet
// (no protocol annotation) and proves the v2alpha1 view restores the ScaleSet
// defaults rather than leaving the fields zero (which would fail the field Minimum).
func TestV2Conversion_RunnerSet_NativeHubDefaults(t *testing.T) {
	const ns = "v2-conv-native"
	createNamespace(t, ns)

	native := &v2beta1.RunnerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "native-set", Namespace: ns},
		Spec: v2beta1.RunnerSetSpec{
			GatewayRef:   v2beta1.ObjectRef{Name: "gw"},
			RunnerLabels: []string{"linux"},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, native))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, native) })

	var back v2alpha1.RunnerSet
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "native-set"}, &back))
	assert.Equal(t, v2alpha1.AcquisitionProtocolScaleSet, back.Spec.AcquisitionProtocol, "a v2beta1-native set restores to ScaleSet")
	assert.Equal(t, int32(10), back.Spec.MaxListeners, "a v2beta1-native set restores to the maxListeners default")
	assert.NotContains(t, back.Annotations, convAnnAcquisitionProtocol)
	assert.NotContains(t, back.Annotations, convAnnMaxListeners)
}

// TestV2Conversion_ActionsGateway_Identity proves the identity conversion path for a
// non-RunnerSet kind: a v2alpha1 ActionsGateway is served correctly as the v2beta1
// hub with every field preserved (the discriminated credentials union included).
func TestV2Conversion_ActionsGateway_Identity(t *testing.T) {
	const ns = "v2-conv-ag"
	createNamespace(t, ns)

	ag := newV2ActionsGateway(ns, "acme")
	ag.Spec.LogLevel = "debug"
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ag) })

	var hub v2beta1.ActionsGateway
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "acme"}, &hub))
	assert.Equal(t, "https://github.com/acme", hub.Spec.GitHubURL)
	assert.Equal(t, "debug", hub.Spec.LogLevel)
	assert.Equal(t, v2beta1.CredentialTypeGitHubApp, hub.Spec.Credentials.Type)
	require.NotNil(t, hub.Spec.Credentials.GitHubApp)
	assert.Equal(t, "acme-github-app", hub.Spec.Credentials.GitHubApp.Name)

	// Mutate through the hub and confirm the change is served back on the spoke — the
	// conversion is bidirectional, not write-once.
	hub.Spec.LogLevel = "info"
	require.NoError(t, k8sClient.Update(ctx, &hub))
	var back v2alpha1.ActionsGateway
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "acme"}, &back))
	assert.Equal(t, "info", back.Spec.LogLevel)
}

// TestV2Conversion_EgressProxy_LogLevelRoundTrip proves spec.logLevel (Q327)
// survives the identity conversion through the real apiserver: a v2alpha1 create
// with logLevel=debug is served as the v2beta1 hub with the field intact, and a
// hub-side flip back to info is served on the v2alpha1 spoke.
func TestV2Conversion_EgressProxy_LogLevelRoundTrip(t *testing.T) {
	const ns = "v2-conv-ep-loglevel"
	createNamespace(t, ns)

	ep := &v2alpha1.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "proxy", Namespace: ns},
		Spec:       v2alpha1.EgressProxySpec{LogLevel: "debug"},
	}
	require.NoError(t, k8sClient.Create(ctx, ep))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ep) })

	var hub v2beta1.EgressProxy
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "proxy"}, &hub))
	assert.Equal(t, "debug", hub.Spec.LogLevel, "logLevel must survive the v2alpha1→v2beta1 conversion")

	hub.Spec.LogLevel = "info"
	require.NoError(t, k8sClient.Update(ctx, &hub))
	var back v2alpha1.EgressProxy
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "proxy"}, &back))
	assert.Equal(t, "info", back.Spec.LogLevel, "logLevel must survive the v2beta1→v2alpha1 conversion")
}

// TestV2Conversion_EgressProxy_AuditLoggingRoundTrip proves spec.auditLogging
// (Q564) survives the identity conversion through the real apiserver in both
// directions, and that the apiserver defaults it to Off. The default matters
// more than the round-trip here: it is the security property, and the CRD is
// the only place that applies it to a hand-applied CR. Mirrors the logLevel
// test above, which is the precedent for a per-pool knob on this type.
func TestV2Conversion_EgressProxy_AuditLoggingRoundTrip(t *testing.T) {
	const ns = "v2-conv-ep-auditlog"
	createNamespace(t, ns)

	// A create that omits the field must be defaulted to Off by the apiserver,
	// not left empty.
	bare := &v2alpha1.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "bare", Namespace: ns},
		Spec:       v2alpha1.EgressProxySpec{},
	}
	require.NoError(t, k8sClient.Create(ctx, bare))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, bare) })
	var defaulted v2alpha1.EgressProxy
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "bare"}, &defaulted))
	assert.Equal(t, "Off", defaulted.Spec.AuditLogging,
		"an EgressProxy that omits auditLogging must be defaulted to Off by the apiserver")

	ep := &v2alpha1.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "proxy", Namespace: ns},
		Spec:       v2alpha1.EgressProxySpec{AuditLogging: "Connections"},
	}
	require.NoError(t, k8sClient.Create(ctx, ep))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ep) })

	var hub v2beta1.EgressProxy
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "proxy"}, &hub))
	assert.Equal(t, "Connections", hub.Spec.AuditLogging,
		"auditLogging must survive the v2alpha1→v2beta1 conversion")

	hub.Spec.AuditLogging = "Off"
	require.NoError(t, k8sClient.Update(ctx, &hub))
	var back v2alpha1.EgressProxy
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "proxy"}, &back))
	assert.Equal(t, "Off", back.Spec.AuditLogging,
		"auditLogging must survive the v2beta1→v2alpha1 conversion")
}

// TestV2Conversion_EgressProxy_AuditLoggingRejectsUnknownMode: the enum is the
// whole admission surface for this field, so an unknown mode must be refused
// rather than reaching the proxy, where parseAuditMode would silently resolve it
// to Off. Both halves are wanted: the proxy fails safe, and the apiserver says so.
func TestV2Conversion_EgressProxy_AuditLoggingRejectsUnknownMode(t *testing.T) {
	const ns = "v2-conv-ep-auditlog-enum"
	createNamespace(t, ns)

	bad := &v2alpha1.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "bad", Namespace: ns},
		Spec:       v2alpha1.EgressProxySpec{AuditLogging: "Full"},
	}
	err := k8sClient.Create(ctx, bad)
	require.Error(t, err, "an auditLogging value outside the enum must be rejected at admission")
	assert.Contains(t, err.Error(), "auditLogging", "the rejection must name the offending field")
}
