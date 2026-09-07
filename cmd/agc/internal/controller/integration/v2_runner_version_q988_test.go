//go:build integration

package integration_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/controller"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/runnerimage"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/runnerimage/runnerimagetest"
	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Q988 end-to-end against the real apiserver. The unit tests drive the resolver and
// the merge separately; only these prove the asynchronous half of the path — that a
// reading which lands after the reconcile that asked for it reaches status at all.
// The reconciler publishes Unknown first, the inspection completes on another
// goroutine, its wake re-enqueues the set, and the next reconcile carries the
// verdict through the status write.

// withRegistry wires a real resolver, reading pull secrets from the apiserver the
// way main.go does (uncached: the suite disables the Secret cache). main.go scopes
// the read to POD_NAMESPACE; here it is the test's namespace.
func withRegistry(reg *runnerimagetest.Registry, ns string) func(*controller.RunnerSetReconciler) {
	return func(r *controller.RunnerSetReconciler) {
		r.ImageResolver = &runnerimage.Resolver{
			HTTP: reg.Client(),
			ReadSecret: func(ctx context.Context, name string) ([]byte, error) {
				var s corev1.Secret
				if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &s); err != nil {
					return nil, err
				}
				return s.Data[corev1.DockerConfigJsonKey], nil
			},
		}
	}
}

func setupRegistrySet(t *testing.T, ns, image string, pullSecret string, tweak func(*controller.RunnerSetReconciler)) {
	t.Helper()
	startRunnerSetReconciler(t, tweak)

	tmpl := templateWithWorkerImage("tmpl", ns, image)
	if pullSecret != "" {
		tmpl.Spec.PodTemplate.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: pullSecret}}
	}
	require.NoError(t, k8sClient.Create(ctx, newGatewayForSet("gw", ns, "")))
	require.NoError(t, k8sClient.Create(ctx, tmpl))
	rs := newRunnerSet("set", ns, "gw")
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), rs)
		_ = k8sClient.Delete(context.Background(), &v2alpha1.ActionsGateway{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: ns}})
		_ = k8sClient.Delete(context.Background(), &v2alpha1.RunnerTemplate{ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: ns}})
	})
}

func runnerVersionMessage(t *testing.T, ns, name string) string {
	t.Helper()
	var rs v2alpha1.RunnerSet
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &rs))
	c := meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionRunnerVersionTooOld)
	require.NotNil(t, c)
	return c.Message
}

// TestV2_RegistryRead_DigestOnlyImageGetsVerdict is the row's headline: a reference
// the tag reading can only call Unknown becomes a verdict once the registry answers.
//
// The registry holds the layer until the set has gone quiet on Unknown, and the
// verdict is then required inside a window nothing but the resolver's wake explains:
// the status-write echo has already settled, and the resync is hours away. Without
// the hold, the inspection lands inside the echo and the test is green with the wake
// deleted (measured 2026-09-07).
func TestV2_RegistryRead_DigestOnlyImageGetsVerdict(t *testing.T) {
	const ns = "v2-rs-registry-digest"
	reg := runnerimagetest.New(t)
	reg.HoldBlobs = make(chan struct{})
	digest := reg.RunnerImage("acme/runner", "unused", "2.320.0")
	createNSForAGC(t, ns)
	setupRegistrySet(t, ns, reg.Image("acme/runner", "@"+digest), "", withRegistry(reg, ns))

	waitForRunnerVersionCondition(t, ns, "set", metav1.ConditionUnknown, v2alpha1.ReasonWorkerImageVersionUnknown)
	assert.Contains(t, runnerVersionMessage(t, ns, "set"), "in progress")
	time.Sleep(3 * time.Second) // let the echo reconciles settle while the layer is held

	close(reg.HoldBlobs)
	require.Eventually(t, func() bool {
		var rs v2alpha1.RunnerSet
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "set"}, &rs); err != nil {
			return false
		}
		c := meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionRunnerVersionTooOld)
		return c != nil && c.Status == metav1.ConditionTrue && c.Reason == v2alpha1.ReasonWorkerImageBelowMinimum
	}, 3*time.Second, 50*time.Millisecond, "the reading must reach status through the wake, not a later resync")
	msg := runnerVersionMessage(t, ns, "set")
	assert.Contains(t, msg, "read from the registry at "+digest)
	assert.Contains(t, msg, "2.320.0")
}

// TestV2_RegistryRead_PullSecretFromTemplate proves the credential path the AGC
// actually has: the pod template's imagePullSecret, read from the apiserver and
// presented to the registry.
func TestV2_RegistryRead_PullSecretFromTemplate(t *testing.T) {
	const ns = "v2-rs-registry-secret"
	reg := runnerimagetest.New(t)
	reg.Auth = runnerimagetest.AuthBasic
	reg.Login = runnerimagetest.Credential{Username: "bot", Password: "s3cret"}
	reg.RunnerImage("acme/runner", "v3-cuda", "2.335.1")
	createNSForAGC(t, ns)
	require.NoError(t, k8sClient.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "regcred", Namespace: ns},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: runnerimagetest.DockerConfigJSON(reg.Host(), reg.Login)},
	}))
	setupRegistrySet(t, ns, reg.Image("acme/runner", ":v3-cuda"), "regcred", withRegistry(reg, ns))

	waitForRunnerVersionCondition(t, ns, "set", metav1.ConditionFalse, v2alpha1.ReasonWorkerImageCurrent)
	assert.Contains(t, runnerVersionMessage(t, ns, "set"), "read from the registry")
}

// TestV2_RegistryRead_UnreachableKeepsTagVerdict pins the fallback an FQDN-mode
// policy or a tenant's own registry produces: the read fails, and the verdict the
// tag already supported stands with the failure in the message rather than
// regressing to Unknown.
func TestV2_RegistryRead_UnreachableKeepsTagVerdict(t *testing.T) {
	const ns = "v2-rs-registry-down"
	reg := runnerimagetest.New(t)
	hc := reg.Client()
	reg.Close()
	createNSForAGC(t, ns)
	setupRegistrySet(t, ns, reg.Image("acme/runner", ":2.335.1"), "", func(r *controller.RunnerSetReconciler) {
		r.ImageResolver = &runnerimage.Resolver{HTTP: hc}
	})

	waitForRunnerVersionCondition(t, ns, "set", metav1.ConditionFalse, v2alpha1.ReasonWorkerImageCurrent)
	require.Eventually(t, func() bool {
		var rs v2alpha1.RunnerSet
		if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "set"}, &rs); err != nil {
			return false
		}
		c := meta.FindStatusCondition(rs.Status.Conditions, v2alpha1.ConditionRunnerVersionTooOld)
		return c != nil && c.Status == metav1.ConditionFalse &&
			strings.Contains(c.Message, "registry read of the image failed")
	}, 20*time.Second, 100*time.Millisecond, "the failed read is reported on the standing tag verdict")
}
