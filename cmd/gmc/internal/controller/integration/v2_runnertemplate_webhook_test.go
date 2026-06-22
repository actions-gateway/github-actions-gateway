//go:build integration

package integration_test

import (
	"testing"

	agcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// These tests exercise the v2 reserved-pod-field validating webhook (Q163, M2)
// through the real apiserver. The webhook implements the per-container checks the
// M1 CRD CEL rules cannot afford (an unbounded containers-array walk): rejecting the
// AGC-injected proxy env vars on every template, and rejecting privileged containers
// on the namespaced RunnerTemplate while allowing them on the platform-authored
// ClusterRunnerTemplate. The scalar pod-level reserved fields stay on the M1 CEL
// rules and are not re-tested here.

func runnerTemplateWithContainer(ns, name string, c corev1.Container) *agcv2alpha1.RunnerTemplate {
	return &agcv2alpha1.RunnerTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: agcv2alpha1.RunnerTemplateSpec{
			PodTemplate: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{c}},
			},
		},
	}
}

func clusterRunnerTemplateWithContainer(name string, c corev1.Container) *agcv2alpha1.ClusterRunnerTemplate {
	return &agcv2alpha1.ClusterRunnerTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: agcv2alpha1.RunnerTemplateSpec{
			PodTemplate: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{c}},
			},
		},
	}
}

func TestV2_RunnerTemplate_RejectsReservedProxyEnv(t *testing.T) {
	const ns = "v2-rt-proxyenv"
	createNamespace(t, ns)

	rt := runnerTemplateWithContainer(ns, "bad", corev1.Container{
		Name: "runner",
		Env:  []corev1.EnvVar{{Name: "HTTP_PROXY", Value: "http://evil:3128"}},
	})
	err := k8sClient.Create(ctx, rt)
	require.Error(t, err, "a RunnerTemplate setting a reserved proxy env var must be rejected")
	assert.Contains(t, err.Error(), "is reserved")
}

func TestV2_RunnerTemplate_RejectsReservedProxyEnvCaseInsensitive(t *testing.T) {
	const ns = "v2-rt-proxyenv-lower"
	createNamespace(t, ns)

	rt := runnerTemplateWithContainer(ns, "bad-lower", corev1.Container{
		Name: "runner",
		Env:  []corev1.EnvVar{{Name: "https_proxy", Value: "http://evil:3128"}},
	})
	err := k8sClient.Create(ctx, rt)
	require.Error(t, err, "the reserved proxy env check is case-insensitive")
	assert.Contains(t, err.Error(), "is reserved")
}

func TestV2_RunnerTemplate_RejectsPrivilegedContainer(t *testing.T) {
	const ns = "v2-rt-privileged"
	createNamespace(t, ns)

	priv := true
	rt := runnerTemplateWithContainer(ns, "priv", corev1.Container{
		Name:            "runner",
		SecurityContext: &corev1.SecurityContext{Privileged: &priv},
	})
	err := k8sClient.Create(ctx, rt)
	require.Error(t, err, "a privileged container in a namespaced RunnerTemplate must be rejected")
	assert.Contains(t, err.Error(), "privileged containers are not permitted")
}

func TestV2_RunnerTemplate_AdmitsCleanTemplate(t *testing.T) {
	const ns = "v2-rt-clean"
	createNamespace(t, ns)

	rt := runnerTemplateWithContainer(ns, "clean", corev1.Container{
		Name:      "runner",
		Resources: corev1.ResourceRequirements{},
	})
	require.NoError(t, k8sClient.Create(ctx, rt), "a clean RunnerTemplate must be admitted")
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, rt) })
}

func TestV2_ClusterRunnerTemplate_AllowsPrivilegedContainer(t *testing.T) {
	priv := true
	crt := clusterRunnerTemplateWithContainer("golden-dind", corev1.Container{
		Name:            "runner",
		SecurityContext: &corev1.SecurityContext{Privileged: &priv},
	})
	require.NoError(t, k8sClient.Create(ctx, crt),
		"a privileged container is allowed on the platform-authored cluster-scoped template")
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, crt) })
}

func TestV2_ClusterRunnerTemplate_RejectsReservedProxyEnv(t *testing.T) {
	crt := clusterRunnerTemplateWithContainer("bad-cluster", corev1.Container{
		Name: "runner",
		Env:  []corev1.EnvVar{{Name: "NO_PROXY", Value: "example.com"}},
	})
	err := k8sClient.Create(ctx, crt)
	require.Error(t, err, "a reserved proxy env var is rejected even on the cluster-scoped template")
	assert.Contains(t, err.Error(), "is reserved")
}
