//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	v2 "github.com/actions-gateway/github-actions-gateway/api/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// credSecretAnnotation records the GitHub App Secret an AGC pod template was rendered
// from, so a rotation shows in `kubectl rollout history`.
const credSecretAnnotation = "actions-gateway/github-app-secret"

// createDirectV2Gateway creates a direct-egress v2 gateway named gw in ns reading the
// named GitHub App Secret, and removes it and its ClusterRoleBinding afterwards.
func createDirectV2Gateway(t *testing.T, ns, secretName string, mutate func(*v2.ActionsGateway)) {
	t.Helper()
	ag := newV2GatewayWired("gw", ns, secretName, "")
	if mutate != nil {
		mutate(ag)
	}
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), ag)
		_ = k8sClient.Delete(context.Background(), &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "agc-clusterrunnertemplate-reader." + ns + ".gw"}})
	})
}

// updateV2Gateway applies mutate to the live gateway gw in ns, retrying on conflict
// with the reconciler's own finalizer and status writes.
func updateV2Gateway(t *testing.T, ns string, mutate func(*v2.ActionsGateway)) {
	t.Helper()
	require.Eventually(t, func() bool {
		var ag v2.ActionsGateway
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "gw"}, &ag); err != nil {
			return false
		}
		mutate(&ag)
		return k8sClient.Update(ctx, &ag) == nil
	}, 5*time.Second, 50*time.Millisecond, "update ActionsGateway %s/gw", ns)
}

// agcDeployment reads the gateway's AGC Deployment, or reports false while it is absent.
func agcDeployment(ns string) (appsv1.Deployment, bool) {
	var dep appsv1.Deployment
	err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "gw-agc"}, &dep)
	return dep, err == nil
}

// TestV2_CredentialRotation_SecretRefChangeRollsTheAGC pins the rotation path
// docs/getting-started.md documents: pointing the gateway at a new Secret changes the
// AGC pod template, which is what rolls the AGC onto the new credential.
func TestV2_CredentialRotation_SecretRefChangeRollsTheAGC(t *testing.T) {
	const ns = "v2-cred-ref-change"
	createNamespace(t, ns)
	createGitHubAppSecret(t, ns, "secret-v1")
	createGitHubAppSecret(t, ns, "secret-v2")
	createDirectV2Gateway(t, ns, "secret-v1", nil)
	startCountingV2Reconcilers(t, nil, 2*time.Second)

	require.Eventually(t, func() bool {
		dep, ok := agcDeployment(ns)
		return ok && dep.Spec.Template.Annotations[credSecretAnnotation] == "secret-v1"
	}, 15*time.Second, 100*time.Millisecond, "the AGC pod template should record secret-v1")

	updateV2Gateway(t, ns, func(ag *v2.ActionsGateway) { ag.Spec.Credentials.GitHubApp.Name = "secret-v2" })

	require.Eventually(t, func() bool {
		dep, ok := agcDeployment(ns)
		if !ok || dep.Spec.Template.Annotations[credSecretAnnotation] != "secret-v2" {
			return false
		}
		for _, v := range dep.Spec.Template.Spec.Volumes {
			if v.Secret != nil && v.Secret.SecretName == "secret-v2" {
				return true
			}
		}
		return false
	}, 15*time.Second, 100*time.Millisecond, "the AGC pod template should mount and record secret-v2")
}

// TestV2_CredentialRotation_InPlaceSecretEditDoesNotRoll pins the other half: editing
// the referenced Secret's contents under the same name leaves the AGC Deployment
// unwritten, because the template records only the Secret's name and the AGC reads
// the mounted files, which the kubelet refreshes in place.
func TestV2_CredentialRotation_InPlaceSecretEditDoesNotRoll(t *testing.T) {
	const ns = "v2-cred-in-place"
	createNamespace(t, ns)
	createGitHubAppSecret(t, ns, "github-app")
	createDirectV2Gateway(t, ns, "github-app", nil)
	const syncPeriod = 2 * time.Second
	agCount, _ := startCountingV2Reconcilers(t, nil, syncPeriod)

	require.Eventually(t, func() bool {
		dep, ok := agcDeployment(ns)
		return ok && dep.Spec.Template.Annotations[credSecretAnnotation] == "github-app"
	}, 15*time.Second, 100*time.Millisecond, "the AGC pod template should record github-app")
	time.Sleep(2 * syncPeriod)
	before, _ := agcDeployment(ns)

	require.Eventually(t, func() bool {
		var s corev1.Secret
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "github-app"}, &s); err != nil {
			return false
		}
		s.Data["privateKey"] = []byte("rotated-in-place")
		return k8sClient.Update(ctx, &s) == nil
	}, 5*time.Second, 50*time.Millisecond, "edit the Secret in place")
	agBefore := agCount.startedIn(ns)

	time.Sleep(3 * syncPeriod)
	after, _ := agcDeployment(ns)
	assert.Equal(t, before.ResourceVersion, after.ResourceVersion, "an in-place Secret edit must not rewrite the AGC Deployment")
	assert.Equal(t, "github-app", after.Spec.Template.Annotations[credSecretAnnotation])
	assert.GreaterOrEqual(t, agCount.startedIn(ns)-agBefore, 2, "ActionsGateway reconciles after the edit")
}

// TestV2_ActionsGateway_LogLevelChangeRollsTheAGC is the AGC half of the per-tenant
// verbosity knob; TestV2_EgressProxy_LogLevelChange_RollsProxy is the proxy half.
// LOG_LEVEL is an env on the pod template, so a change rolls the AGC.
func TestV2_ActionsGateway_LogLevelChangeRollsTheAGC(t *testing.T) {
	const ns = "v2-ag-loglevel"
	createNamespace(t, ns)
	createGitHubAppSecret(t, ns, "github-app")
	createDirectV2Gateway(t, ns, "github-app", nil)
	startCountingV2Reconcilers(t, nil, 2*time.Second)

	require.Eventually(t, func() bool {
		dep, ok := agcDeployment(ns)
		return ok && envValue(&dep, "LOG_LEVEL") == "info"
	}, 15*time.Second, 100*time.Millisecond, "the AGC should start at LOG_LEVEL=info")

	updateV2Gateway(t, ns, func(ag *v2.ActionsGateway) { ag.Spec.LogLevel = "debug" })

	require.Eventually(t, func() bool {
		dep, ok := agcDeployment(ns)
		return ok && envValue(&dep, "LOG_LEVEL") == "debug"
	}, 15*time.Second, 100*time.Millisecond, "the AGC should roll to LOG_LEVEL=debug")
}

// TestV2_ActionsGateway_DegradedOnProvisioningError forces a mid-reconcile apply
// failure the apiserver rejects (an existing AGC Deployment whose immutable selector
// differs from the builder's) and asserts the gateway reports Degraded=True naming the
// failing step, then clears it once the obstruction is gone. The unit tier fakes the
// error; here the apiserver produces it.
func TestV2_ActionsGateway_DegradedOnProvisioningError(t *testing.T) {
	const ns = "v2-ag-degraded"
	createNamespace(t, ns)
	createGitHubAppSecret(t, ns, "github-app")

	conflicting := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "gw-agc", Namespace: ns},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"q1259": "conflict"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"q1259": "conflict"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "busybox"}}},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, conflicting))
	createDirectV2Gateway(t, ns, "github-app", nil)
	startCountingV2Reconcilers(t, nil, 2*time.Second)

	degraded := func() *metav1.Condition {
		var ag v2.ActionsGateway
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "gw"}, &ag); err != nil {
			return nil
		}
		return meta.FindStatusCondition(ag.Status.Conditions, v2.ConditionDegraded)
	}
	require.Eventually(t, func() bool {
		c := degraded()
		return c != nil && c.Status == metav1.ConditionTrue && c.Reason == v2.ReasonProvisioningFailed
	}, 20*time.Second, 100*time.Millisecond, "a rejected apply must surface Degraded=True")
	assert.Contains(t, degraded().Message, "AGC Deployment", "Degraded must name the failing step")

	require.NoError(t, k8sClient.Delete(ctx, conflicting))
	require.Eventually(t, func() bool {
		c := degraded()
		return c != nil && c.Status == metav1.ConditionFalse && c.Reason == v2.ReasonReconcileSucceeded
	}, 20*time.Second, 100*time.Millisecond, "Degraded must clear once the obstruction is removed")
}
