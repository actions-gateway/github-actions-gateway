//go:build integration

package integration_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/actions-gateway/github-actions-gateway/broker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestV2_RunnerSet_DedicatedProxyPool: two RunnerSets under one gateway egress
// through different pools when one names its own proxyRef (Q567, appendix-g §G.5).
// The light set inherits the gateway's defaultProxyRef; the heavy set's proxyRef
// overrides it, so its worker carries the dedicated pool's address and CA and none
// of the shared pool's. Each EgressProxy gets its own Deployment/Service/HPA from the
// GMC, so this is what isolates a bandwidth-heavy set from its co-tenants.
func TestV2_RunnerSet_DedicatedProxyPool(t *testing.T) {
	const ns = "v2-rs-dedicated-proxy"
	createNSForAGC(t, ns)

	gw := newGatewayForSet("gw", ns, "shared")
	require.NoError(t, k8sClient.Create(ctx, gw))
	require.NoError(t, k8sClient.Create(ctx, newRunnerTemplate("tmpl", ns)))
	shared := &v2alpha1.EgressProxy{ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: ns}}
	heavy := &v2alpha1.EgressProxy{ObjectMeta: metav1.ObjectMeta{Name: "heavy", Namespace: ns}}
	require.NoError(t, k8sClient.Create(ctx, shared))
	require.NoError(t, k8sClient.Create(ctx, heavy))

	lightSet := newRunnerSet("dedicated-light", ns, "gw")
	heavySet := newRunnerSet("dedicated-heavy", ns, "gw")
	heavySet.Spec.ProxyRef = &v2alpha1.ProxyObjectRef{Name: "heavy"}
	require.NoError(t, k8sClient.Create(ctx, lightSet))
	require.NoError(t, k8sClient.Create(ctx, heavySet))
	t.Cleanup(func() {
		bg := context.Background()
		_ = k8sClient.Delete(bg, lightSet)
		_ = k8sClient.Delete(bg, heavySet)
		_ = k8sClient.Delete(bg, gw)
		_ = k8sClient.Delete(bg, &v2alpha1.RunnerTemplate{ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: ns}})
		_ = k8sClient.Delete(bg, shared)
		_ = k8sClient.Delete(bg, heavy)
	})

	startRunnerSetReconciler(t)

	for _, tc := range []struct {
		set, pool, otherPool string
	}{
		{set: "dedicated-light", pool: "shared", otherPool: "heavy"},
		{set: "dedicated-heavy", pool: "heavy", otherPool: "shared"},
	} {
		id := enqueueJobOnSetSession(15*time.Second, tc.set, nil, broker.RunnerJobRequestBody{})
		require.NotEmpty(t, id, "a session for %s should register", tc.set)

		pod := waitForSetWorkerPod(t, ns, tc.set)
		env := runnerEnv(t, pod)
		wantProxy := "https://" + tc.pool + "-proxy." + ns + ".svc.cluster.local:8080"
		assert.Equal(t, wantProxy, env["HTTPS_PROXY"], "%s egresses through %s", tc.set, tc.pool)
		assert.Equal(t, wantProxy, env["HTTP_PROXY"], "%s egresses through %s", tc.set, tc.pool)

		var secrets []string
		for _, v := range pod.Spec.Volumes {
			if v.Secret != nil {
				secrets = append(secrets, v.Secret.SecretName)
			}
		}
		assert.Contains(t, secrets, tc.pool+"-proxy-tls", "%s trusts its own pool's CA", tc.set)
		assert.NotContains(t, secrets, tc.otherPool+"-proxy-tls", "%s mounts nothing of the other pool", tc.set)
	}
}

// waitForSetWorkerPod returns the first worker pod the provisioner creates for set.
func waitForSetWorkerPod(t *testing.T, ns, set string) corev1.Pod {
	t.Helper()
	var pod corev1.Pod
	require.Eventually(t, func() bool {
		var pods corev1.PodList
		if err := k8sClient.List(ctx, &pods, client.InNamespace(ns),
			client.MatchingLabels{provisioner.LabelRunnerSet: set}); err != nil {
			return false
		}
		for _, p := range pods.Items {
			if strings.HasPrefix(p.Name, "runner-") {
				pod = p
				return true
			}
		}
		return false
	}, 20*time.Second, 50*time.Millisecond, "worker Pod should be created for %s", set)
	return pod
}

// runnerEnv maps the runner container's env by name.
func runnerEnv(t *testing.T, pod corev1.Pod) map[string]string {
	t.Helper()
	for _, c := range pod.Spec.Containers {
		if c.Name != "runner" {
			continue
		}
		env := map[string]string{}
		for _, e := range c.Env {
			env[e.Name] = e.Value
		}
		return env
	}
	require.FailNow(t, "worker pod has no runner container", pod.Name)
	return nil
}
