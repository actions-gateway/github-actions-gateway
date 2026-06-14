//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	agcv1alpha1 "github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	gmcv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/api/v1alpha1"
	"github.com/onsi/gomega"
	"github.com/stretchr/testify/assert"
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
			GitHubURL:    "https://github.com/example-org",
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

	// ServiceAccount: actions-gateway-controller
	g.Eventually(func() error {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: agcName},
			&corev1.ServiceAccount{})
	}, 15*time.Second, 25*time.Millisecond).Should(gomega.Succeed())

	// ServiceAccount: actions-gateway-worker
	g.Eventually(func() error {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: workerSAName},
			&corev1.ServiceAccount{})
	}, 15*time.Second, 25*time.Millisecond).Should(gomega.Succeed())

	// RoleBinding: actions-gateway-controller — binds the AGC SA to the shipped
	// agc-tenant-role ClusterRole. No per-tenant Role is created (see §B B1 in
	// docs/plan/k8s-best-practices.md).
	g.Eventually(func() error {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: agcName},
			&rbacv1.RoleBinding{})
	}, 15*time.Second, 25*time.Millisecond).Should(gomega.Succeed())

	// NetworkPolicy: actions-gateway-proxy (proxy pod egress to GitHub CIDRs)
	g.Eventually(func() error {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: proxyName},
			&networkingv1.NetworkPolicy{})
	}, 15*time.Second, 25*time.Millisecond).Should(gomega.Succeed())

	// NetworkPolicy: actions-gateway-workload (AGC and worker egress to proxy only)
	g.Eventually(func() error {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: workloadName},
			&networkingv1.NetworkPolicy{})
	}, 15*time.Second, 25*time.Millisecond).Should(gomega.Succeed())

	// Deployment: actions-gateway-proxy
	g.Eventually(func() error {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: proxyName},
			&appsv1.Deployment{})
	}, 15*time.Second, 25*time.Millisecond).Should(gomega.Succeed())

	// Deployment: actions-gateway-controller with proxy env vars
	g.Eventually(func() error {
		var dep appsv1.Deployment
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: agcName}, &dep); err != nil {
			return err
		}
		return nil
	}, 15*time.Second, 25*time.Millisecond).Should(gomega.Succeed())

	// Verify proxy env vars are present on the AGC Deployment.
	var agcDep appsv1.Deployment
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: agcName}, &agcDep))
	require.NotEmpty(t, agcDep.Spec.Template.Spec.Containers)
	envNames := make(map[string]bool)
	envValues := make(map[string]string)
	for _, e := range agcDep.Spec.Template.Spec.Containers[0].Env {
		envNames[e.Name] = true
		envValues[e.Name] = e.Value
	}
	require.True(t, envNames["HTTP_PROXY"], "HTTP_PROXY env var must be set on AGC Deployment")
	require.True(t, envNames["HTTPS_PROXY"], "HTTPS_PROXY env var must be set on AGC Deployment")
	require.True(t, envNames["NO_PROXY"], "NO_PROXY env var must be set on AGC Deployment")
	// spec.gitHubURL is threaded to the AGC as GITHUB_ORG_URL — the first-class
	// production path that replaces the testing-only --allow-agc-extra-env workaround.
	require.True(t, envNames["GITHUB_ORG_URL"], "GITHUB_ORG_URL env var must be set on AGC Deployment")
	require.Equal(t, "https://github.com/example-org", envValues["GITHUB_ORG_URL"],
		"GITHUB_ORG_URL must carry the spec.gitHubURL value")

	// Service: actions-gateway-proxy
	g.Eventually(func() error {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: proxyName},
			&corev1.Service{})
	}, 15*time.Second, 25*time.Millisecond).Should(gomega.Succeed())

	// HPA: actions-gateway-proxy
	g.Eventually(func() error {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: proxyName},
			&autoscalingv2.HorizontalPodAutoscaler{})
	}, 15*time.Second, 25*time.Millisecond).Should(gomega.Succeed())

	// PDB: actions-gateway-proxy
	g.Eventually(func() error {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: proxyName},
			&policyv1.PodDisruptionBudget{})
	}, 15*time.Second, 25*time.Millisecond).Should(gomega.Succeed())

	// Metrics mTLS Secrets (Q69). Names are the literal on-cluster names from
	// metrics_cert.go (unexported there). The server bundle is mounted into the
	// AGC + proxy; the client bundle is published for the scraper.
	g.Eventually(func() error {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "actions-gateway-metrics-tls"},
			&corev1.Secret{})
	}, 15*time.Second, 25*time.Millisecond).Should(gomega.Succeed())

	var metricsServerSec corev1.Secret
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "actions-gateway-metrics-tls"}, &metricsServerSec))
	assert.Equal(t, corev1.SecretTypeTLS, metricsServerSec.Type)
	for _, k := range []string{corev1.TLSCertKey, corev1.TLSPrivateKeyKey, "ca.crt"} {
		assert.NotEmpty(t, metricsServerSec.Data[k], "metrics server Secret must carry %s", k)
	}

	g.Eventually(func() error {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "actions-gateway-metrics-client"},
			&corev1.Secret{})
	}, 15*time.Second, 25*time.Millisecond).Should(gomega.Succeed())

	var metricsClientSec corev1.Secret
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "actions-gateway-metrics-client"}, &metricsClientSec))
	assert.Equal(t, corev1.SecretTypeTLS, metricsClientSec.Type)
	for _, k := range []string{corev1.TLSCertKey, corev1.TLSPrivateKeyKey, "ca.crt"} {
		assert.NotEmpty(t, metricsClientSec.Data[k], "metrics client Secret must carry %s", k)
	}
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
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: agcName},
			&appsv1.Deployment{})
	}, 15*time.Second, 25*time.Millisecond).Should(gomega.Succeed())

	var dep appsv1.Deployment
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: agcName}, &dep))
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
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: agcName},
			&appsv1.Deployment{})
	}, 15*time.Second, 25*time.Millisecond).Should(gomega.Succeed())

	var dep appsv1.Deployment
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: agcName}, &dep))
	require.NotEmpty(t, dep.Spec.Template.Spec.Volumes)

	// Credentials are mounted from a Secret volume (not env vars). The volume
	// must reference "my-secret" (not an empty or cluster-level name).
	var foundCredVol bool
	for _, v := range dep.Spec.Template.Spec.Volumes {
		if v.Secret != nil && v.Secret.SecretName == "my-secret" {
			foundCredVol = true
		}
	}
	require.True(t, foundCredVol, "AGC Deployment must mount 'my-secret' as a Secret volume")
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
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: agcName},
			&appsv1.Deployment{})
	}, 15*time.Second, 25*time.Millisecond).Should(gomega.Succeed())

	// Update gitHubAppRef to secret-v2 (retry on conflict).
	require.Eventually(t, func() bool {
		var fetched gmcv1alpha1.ActionsGateway
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "rotation-gateway"}, &fetched); err != nil {
			return false
		}
		fetched.Spec.GitHubAppRef.Name = "secret-v2"
		return k8sClient.Update(ctx, &fetched) == nil
	}, 5*time.Second, 25*time.Millisecond, "update ActionsGateway gitHubAppRef to secret-v2")

	// Wait for AGC Deployment to reference secret-v2 via the credential volume.
	g.Eventually(func() bool {
		var dep appsv1.Deployment
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: agcName}, &dep); err != nil {
			return false
		}
		for _, v := range dep.Spec.Template.Spec.Volumes {
			if v.Secret != nil && v.Secret.SecretName == "secret-v2" {
				return true
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

func TestGMC_TenantProvisioning_PSALabelsStamped(t *testing.T) {
	const nsName = "team-psa-baseline"
	createNamespace(t, nsName)
	createGitHubAppSecret(t, nsName, "github-app")

	ag := newActionsGateway("psa-gateway", nsName, "github-app")
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ag) })

	startGMCReconciler(t, nil)

	g := gomega.NewWithT(t)

	// Wait for the proxy Deployment to be created (reconcile completed).
	g.Eventually(func() error {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: proxyName},
			&appsv1.Deployment{})
	}, 15*time.Second, 25*time.Millisecond).Should(gomega.Succeed())

	// The tenant namespace must have PSA enforce=baseline labels.
	var ns corev1.Namespace
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: nsName}, &ns))
	require.Equal(t, "baseline", ns.Labels["pod-security.kubernetes.io/enforce"],
		"enforce label must be set to baseline by default")
	require.Equal(t, "latest", ns.Labels["pod-security.kubernetes.io/enforce-version"])
	require.Equal(t, "baseline", ns.Labels["pod-security.kubernetes.io/warn"])
	require.Equal(t, "baseline", ns.Labels["pod-security.kubernetes.io/audit"])
}

func TestGMC_TenantProvisioning_PSALabels_CustomProfile(t *testing.T) {
	const nsName = "team-psa-privileged"
	createNamespace(t, nsName)
	createGitHubAppSecret(t, nsName, "github-app")

	ag := newActionsGateway("priv-gateway", nsName, "github-app")
	ag.Spec.SecurityProfile = "privileged"
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ag) })

	startGMCReconciler(t, nil)

	g := gomega.NewWithT(t)

	g.Eventually(func() error {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: proxyName},
			&appsv1.Deployment{})
	}, 15*time.Second, 25*time.Millisecond).Should(gomega.Succeed())

	var ns corev1.Namespace
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: nsName}, &ns))
	require.Equal(t, "privileged", ns.Labels["pod-security.kubernetes.io/enforce"],
		"enforce label must reflect the custom SecurityProfile")
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
