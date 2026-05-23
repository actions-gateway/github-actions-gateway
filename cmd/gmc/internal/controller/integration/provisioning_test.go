//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	agcv1alpha1 "github.com/karlkfi/github-actions-gateway/agc/api/v1alpha1"
	gmcv1alpha1 "github.com/karlkfi/github-actions-gateway/gmc/api/v1alpha1"
	"github.com/onsi/gomega"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func newActionsGateway(name, ns, secretName string) *gmcv1alpha1.ActionsGateway {
	return &gmcv1alpha1.ActionsGateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
		},
		Spec: gmcv1alpha1.ActionsGatewaySpec{
			GitHubAppRef: gmcv1alpha1.SecretReference{Name: secretName},
		},
	}
}

func createNamespace(t *testing.T, name string) {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	err := k8sClient.Create(ctx, ns)
	if err != nil {
		// namespace might already exist from a previous test run
		require.NoError(t, client.IgnoreNotFound(err))
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), ns)
	})
}

func createGitHubAppSecret(t *testing.T, ns, name string) {
	t.Helper()
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Data: map[string][]byte{
			"appId":          []byte("12345"),
			"privateKey":     []byte("fake-key"),
			"installationId": []byte("67890"),
		},
	}
	require.NoError(t, k8sClient.Create(ctx, secret))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), secret)
	})
}

func TestGMC_TenantProvisioning_AllResourcesCreated(t *testing.T) {
	const nsName = "team-provisioning"
	createNamespace(t, nsName)
	createGitHubAppSecret(t, nsName, "github-app")

	ag := newActionsGateway("my-gateway", nsName, "github-app")
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), ag)
	})

	startGMCReconciler(t, nil)

	g := gomega.NewWithT(t)

	// ServiceAccount: actions-gateway-agc
	g.Eventually(func() error {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "actions-gateway-agc"},
			&corev1.ServiceAccount{})
	}, 15*time.Second, 25*time.Millisecond).Should(gomega.Succeed())

	// ServiceAccount: actions-gateway-worker
	g.Eventually(func() error {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "actions-gateway-worker"},
			&corev1.ServiceAccount{})
	}, 15*time.Second, 25*time.Millisecond).Should(gomega.Succeed())

	// Role: actions-gateway-agc
	g.Eventually(func() error {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "actions-gateway-agc"},
			&rbacv1.Role{})
	}, 15*time.Second, 25*time.Millisecond).Should(gomega.Succeed())

	// RoleBinding: actions-gateway-agc
	g.Eventually(func() error {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "actions-gateway-agc"},
			&rbacv1.RoleBinding{})
	}, 15*time.Second, 25*time.Millisecond).Should(gomega.Succeed())

	// NetworkPolicy: actions-gateway
	g.Eventually(func() error {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "actions-gateway"},
			&networkingv1.NetworkPolicy{})
	}, 15*time.Second, 25*time.Millisecond).Should(gomega.Succeed())

	// Deployment: actions-gateway-proxy
	g.Eventually(func() error {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "actions-gateway-proxy"},
			&appsv1.Deployment{})
	}, 15*time.Second, 25*time.Millisecond).Should(gomega.Succeed())

	// Deployment: actions-gateway-agc with proxy env vars
	g.Eventually(func() error {
		var dep appsv1.Deployment
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "actions-gateway-agc"}, &dep); err != nil {
			return err
		}
		return nil
	}, 15*time.Second, 25*time.Millisecond).Should(gomega.Succeed())

	// Verify proxy env vars are present on the AGC Deployment.
	var agcDep appsv1.Deployment
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "actions-gateway-agc"}, &agcDep))
	require.NotEmpty(t, agcDep.Spec.Template.Spec.Containers)
	envNames := make(map[string]bool)
	for _, e := range agcDep.Spec.Template.Spec.Containers[0].Env {
		envNames[e.Name] = true
	}
	require.True(t, envNames["HTTP_PROXY"], "HTTP_PROXY env var must be set on AGC Deployment")
	require.True(t, envNames["HTTPS_PROXY"], "HTTPS_PROXY env var must be set on AGC Deployment")
	require.True(t, envNames["NO_PROXY"], "NO_PROXY env var must be set on AGC Deployment")

	// Service: actions-gateway-proxy
	g.Eventually(func() error {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "actions-gateway-proxy"},
			&corev1.Service{})
	}, 15*time.Second, 25*time.Millisecond).Should(gomega.Succeed())

	// HPA: actions-gateway-proxy
	g.Eventually(func() error {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "actions-gateway-proxy"},
			&autoscalingv2.HorizontalPodAutoscaler{})
	}, 15*time.Second, 25*time.Millisecond).Should(gomega.Succeed())

	// PDB: actions-gateway-proxy
	g.Eventually(func() error {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "actions-gateway-proxy"},
			&policyv1.PodDisruptionBudget{})
	}, 15*time.Second, 25*time.Millisecond).Should(gomega.Succeed())
}

func TestGMC_TenantProvisioning_NoProxyMergesDefaults(t *testing.T) {
	const nsName = "team-noproxy"
	createNamespace(t, nsName)
	createGitHubAppSecret(t, nsName, "github-app")

	ag := newActionsGateway("noproxy-gateway", nsName, "github-app")
	ag.Spec.Proxy.NoProxyCIDRs = []string{"192.168.1.0/24"}
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), ag)
	})

	startGMCReconciler(t, nil)

	g := gomega.NewWithT(t)

	g.Eventually(func() error {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "actions-gateway-agc"},
			&appsv1.Deployment{})
	}, 15*time.Second, 25*time.Millisecond).Should(gomega.Succeed())

	var dep appsv1.Deployment
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "actions-gateway-agc"}, &dep))
	require.NotEmpty(t, dep.Spec.Template.Spec.Containers)
	var noProxy string
	for _, e := range dep.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "NO_PROXY" {
			noProxy = e.Value
			break
		}
	}
	require.Contains(t, noProxy, "192.168.1.0/24")
	require.Contains(t, noProxy, "svc.cluster.local")
}

func TestGMC_TenantProvisioning_GitHubAppRefDefaultsToOwnNamespace(t *testing.T) {
	const nsName = "team-appref-default"
	createNamespace(t, nsName)
	createGitHubAppSecret(t, nsName, "my-secret")

	ag := newActionsGateway("appref-gateway", nsName, "my-secret")
	// Namespace field intentionally omitted — should default to the CR's own namespace.
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ag) })

	startGMCReconciler(t, nil)

	g := gomega.NewWithT(t)

	// Wait for the AGC Deployment to be created.
	g.Eventually(func() error {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "actions-gateway-agc"},
			&appsv1.Deployment{})
	}, 15*time.Second, 25*time.Millisecond).Should(gomega.Succeed())

	var dep appsv1.Deployment
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "actions-gateway-agc"}, &dep))
	require.NotEmpty(t, dep.Spec.Template.Spec.Containers)

	// The secretKeyRef must reference "my-secret" (not an empty or cluster-level name).
	var foundAppID bool
	for _, e := range dep.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "GITHUB_APP_ID" {
			require.NotNil(t, e.ValueFrom)
			require.NotNil(t, e.ValueFrom.SecretKeyRef)
			require.Equal(t, "my-secret", e.ValueFrom.SecretKeyRef.Name,
				"GITHUB_APP_ID secretKeyRef must reference the CR's own secret by name")
			foundAppID = true
		}
	}
	require.True(t, foundAppID, "GITHUB_APP_ID env var must be present on the AGC Deployment")
}

func TestGMC_TenantProvisioning_CredentialRotation(t *testing.T) {
	const nsName = "team-cred-rotation"
	createNamespace(t, nsName)
	createGitHubAppSecret(t, nsName, "secret-v1")
	createGitHubAppSecret(t, nsName, "secret-v2")

	ag := newActionsGateway("rotation-gateway", nsName, "secret-v1")
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ag) })

	startGMCReconciler(t, nil)

	g := gomega.NewWithT(t)

	// Wait for initial reconcile — AGC Deployment references secret-v1.
	g.Eventually(func() error {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "actions-gateway-agc"},
			&appsv1.Deployment{})
	}, 15*time.Second, 25*time.Millisecond).Should(gomega.Succeed())

	// Update gitHubAppRef to secret-v2.
	var fetched gmcv1alpha1.ActionsGateway
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "rotation-gateway"}, &fetched))
	fetched.Spec.GitHubAppRef.Name = "secret-v2"
	require.NoError(t, k8sClient.Update(ctx, &fetched))

	// Wait for AGC Deployment to reference secret-v2.
	g.Eventually(func() bool {
		var dep appsv1.Deployment
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "actions-gateway-agc"}, &dep); err != nil {
			return false
		}
		if len(dep.Spec.Template.Spec.Containers) == 0 {
			return false
		}
		for _, e := range dep.Spec.Template.Spec.Containers[0].Env {
			if e.Name == "GITHUB_APP_ID" && e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
				return e.ValueFrom.SecretKeyRef.Name == "secret-v2"
			}
		}
		return false
	}, 15*time.Second, 25*time.Millisecond).Should(gomega.BeTrue(),
		"AGC Deployment must reference secret-v2 after credential rotation")

	// The old secret must NOT be deleted by the GMC.
	var oldSecret corev1.Secret
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "secret-v1"}, &oldSecret),
		"secret-v1 must not be deleted by the GMC during credential rotation")
}

func TestGMC_TenantProvisioning_BootstrapRunnerGroups(t *testing.T) {
	const nsName = "team-runnergroups"
	createNamespace(t, nsName)
	createGitHubAppSecret(t, nsName, "github-app")

	ag := newActionsGateway("rg-gateway", nsName, "github-app")
	minimalContainer := corev1.Container{Name: "runner", Image: "runner:test"}
	ag.Spec.RunnerGroups = []agcv1alpha1.RunnerGroupSpec{
		{
			RunnerLabels: []string{"self-hosted", "linux"},
			MaxListeners: 5,
			PodTemplate:  corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{minimalContainer}}},
		},
		{
			RunnerLabels: []string{"gpu"},
			MaxListeners: 2,
			PodTemplate:  corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{minimalContainer}}},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), ag)
	})

	startGMCReconciler(t, nil)

	g := gomega.NewWithT(t)

	g.Eventually(func() bool {
		var rgList agcv1alpha1.RunnerGroupList
		if err := k8sClient.List(ctx, &rgList, client.InNamespace(nsName)); err != nil {
			return false
		}
		return len(rgList.Items) >= 2
	}, 15*time.Second, 25*time.Millisecond).Should(gomega.BeTrue(), "expected 2 RunnerGroup CRs to be created")

	var rgList agcv1alpha1.RunnerGroupList
	require.NoError(t, k8sClient.List(ctx, &rgList, client.InNamespace(nsName)))
	require.Len(t, rgList.Items, 2)
}

