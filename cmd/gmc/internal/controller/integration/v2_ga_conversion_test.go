//go:build integration

package integration_test

import (
	"testing"

	v2 "github.com/actions-gateway/github-actions-gateway/api/v2"
	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	v2beta1 "github.com/actions-gateway/github-actions-gateway/api/v2beta1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/metadata"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// egressPolicyModeAnnotation mirrors the unexported conversion annotation in
// api/v2/conversion.go that carries a stored alias on a v2 view.
const egressPolicyModeAnnotation = "conversion.actions-gateway.com/egress-policy-mode"

// These tests cover the GA v2 version served beside v2beta1 (Q413) against the real
// apiserver: v2 is a second conversion spoke of the v2beta1 hub, v2 is the storage
// version from 1.10 (Q1086), and v2 omits the deprecated CiliumFQDN/CalicoFQDN aliases.

// TestV2GA_ServedBesideStorage reads the served and storage flags off the installed
// CustomResourceDefinitions rather than off the kubebuilder markers: every
// actions-gateway.com kind serves v2 and v2beta1 and stores v2.
func TestV2GA_ServedBesideStorage(t *testing.T) {
	for _, plural := range []string{
		"actionsgateways", "egressproxies", "runnersets", "runnertemplates",
		"clusterrunnertemplates", "priorityclassallowlists",
	} {
		t.Run(plural, func(t *testing.T) {
			crd := &unstructured.Unstructured{}
			crd.SetGroupVersionKind(schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"})
			require.NoError(t, k8sClient.Get(ctx, client.ObjectKey{Name: plural + ".actions-gateway.com"}, crd))
			versions, found, err := unstructured.NestedSlice(crd.Object, "spec", "versions")
			require.NoError(t, err)
			require.True(t, found)

			served := map[string]bool{}
			var storage []string
			for _, raw := range versions {
				v := raw.(map[string]any)
				name := v["name"].(string)
				served[name] = v["served"].(bool)
				if v["storage"].(bool) {
					storage = append(storage, name)
				}
			}
			assert.True(t, served["v2"], "v2 must be served")
			assert.True(t, served["v2beta1"], "v2beta1 must stay served beside v2")
			assert.Equal(t, []string{"v2"}, storage, "v2 must be the only storage version from 1.10")
		})
	}
}

// TestV2GA_PreferredVersionIsV2 pins what an unpinned `kubectl get egressproxies`
// resolves to once v2 is served: the group's preferred version, which the apiserver
// ranks GA above beta. The operator docs' advice to pin the alias check to v2beta1
// rests on this.
func TestV2GA_PreferredVersionIsV2(t *testing.T) {
	dc, err := discovery.NewDiscoveryClientForConfig(testEnv.Config)
	require.NoError(t, err)
	groups, err := dc.ServerGroups()
	require.NoError(t, err)
	for _, g := range groups.Groups {
		if g.Name == "actions-gateway.com" {
			assert.Equal(t, "v2", g.PreferredVersion.Version)
			return
		}
	}
	t.Fatal("actions-gateway.com is not served")
}

// TestV2GA_ConversionRoundTrip creates each converted kind at v2 and reads it back at
// v2beta1 and v2alpha1, then updates it through v2beta1 and reads the change at v2.
func TestV2GA_ConversionRoundTrip(t *testing.T) {
	const ns = "v2-ga-conv"
	createNamespace(t, ns)
	key := func(name string) client.ObjectKey { return client.ObjectKey{Namespace: ns, Name: name} }

	t.Run("ActionsGateway", func(t *testing.T) {
		ag := &v2.ActionsGateway{
			ObjectMeta: metav1.ObjectMeta{Name: "acme", Namespace: ns},
			Spec: v2.ActionsGatewaySpec{
				Credentials: v2.GitHubCredentials{
					Type:      v2.CredentialTypeGitHubApp,
					GitHubApp: &v2.LocalSecretReference{Name: "acme-github-app"},
				},
				GitHubURL: "https://github.com/acme",
				LogLevel:  "debug",
			},
		}
		require.NoError(t, k8sClient.Create(ctx, ag))
		t.Cleanup(func() { _ = k8sClient.Delete(ctx, ag) })

		var alpha v2alpha1.ActionsGateway
		require.NoError(t, k8sClient.Get(ctx, key("acme"), &alpha))
		assert.Equal(t, "debug", alpha.Spec.LogLevel)
		require.NotNil(t, alpha.Spec.Credentials.GitHubApp)
		assert.Equal(t, "acme-github-app", alpha.Spec.Credentials.GitHubApp.Name)

		var hub v2beta1.ActionsGateway
		require.NoError(t, k8sClient.Get(ctx, key("acme"), &hub))
		hub.Spec.LogLevel = "info"
		require.NoError(t, k8sClient.Update(ctx, &hub))
		var back v2.ActionsGateway
		require.NoError(t, k8sClient.Get(ctx, key("acme"), &back))
		assert.Equal(t, "info", back.Spec.LogLevel)
	})

	t.Run("EgressProxy", func(t *testing.T) {
		ep := &v2.EgressProxy{
			ObjectMeta: metav1.ObjectMeta{Name: "proxy", Namespace: ns},
			Spec: v2.EgressProxySpec{
				EgressPolicyMode: v2.EgressPolicyModeFQDN,
				DestinationFQDNs: []string{"proxy.golang.org"},
				AuditLogging:     "Connections",
			},
		}
		require.NoError(t, k8sClient.Create(ctx, ep))
		t.Cleanup(func() { _ = k8sClient.Delete(ctx, ep) })

		var alpha v2alpha1.EgressProxy
		require.NoError(t, k8sClient.Get(ctx, key("proxy"), &alpha))
		assert.Equal(t, v2alpha1.EgressPolicyModeFQDN, alpha.Spec.EgressPolicyMode)
		assert.Equal(t, []string{"proxy.golang.org"}, alpha.Spec.DestinationFQDNs)
		assert.Equal(t, "Connections", alpha.Spec.AuditLogging)
	})

	t.Run("RunnerTemplate", func(t *testing.T) {
		rt := &v2.RunnerTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: ns},
			Spec: v2.RunnerTemplateSpec{
				PodTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "runner", Image: "ghcr.io/example/runner:latest"}},
				}},
			},
		}
		require.NoError(t, k8sClient.Create(ctx, rt))
		t.Cleanup(func() { _ = k8sClient.Delete(ctx, rt) })

		var hub v2beta1.RunnerTemplate
		require.NoError(t, k8sClient.Get(ctx, key("tmpl"), &hub))
		require.Len(t, hub.Spec.PodTemplate.Spec.Containers, 1)
		assert.Equal(t, "ghcr.io/example/runner:latest", hub.Spec.PodTemplate.Spec.Containers[0].Image)
	})

	t.Run("ClusterRunnerTemplate", func(t *testing.T) {
		crt := &v2.ClusterRunnerTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "v2-ga-golden"},
			Spec: v2.RunnerTemplateSpec{
				PodTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "runner", Image: "ghcr.io/example/dind:latest"}},
				}},
			},
		}
		require.NoError(t, k8sClient.Create(ctx, crt))
		t.Cleanup(func() { _ = k8sClient.Delete(ctx, crt) })

		var alpha v2alpha1.ClusterRunnerTemplate
		require.NoError(t, k8sClient.Get(ctx, client.ObjectKey{Name: "v2-ga-golden"}, &alpha))
		require.Len(t, alpha.Spec.PodTemplate.Spec.Containers, 1)
		assert.Equal(t, "ghcr.io/example/dind:latest", alpha.Spec.PodTemplate.Spec.Containers[0].Image)
	})
}

// TestV2GA_RunnerSet_ClassicSurvivesV2Update creates a Classic RunnerSet at
// v2alpha1, updates it through v2, and reads it back at v2alpha1. v2 has no field for
// the protocol, so it survives only as the conversion annotation the v2 view keeps;
// losing it would re-protocol the set to ScaleSet on the next v2 write.
func TestV2GA_RunnerSet_ClassicSurvivesV2Update(t *testing.T) {
	const ns = "v2-ga-conv-classic"
	createNamespace(t, ns)

	orig := &v2alpha1.RunnerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "classic-set", Namespace: ns},
		Spec: v2alpha1.RunnerSetSpec{
			GatewayRef:          v2alpha1.ObjectRef{Name: "gw"},
			AcquisitionProtocol: v2alpha1.AcquisitionProtocolClassic,
			RunnerLabels:        []string{"linux", "self-hosted"},
			MaxListeners:        20,
		},
	}
	require.NoError(t, k8sClient.Create(ctx, orig))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, orig) })

	var ga v2.RunnerSet
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(orig), &ga))
	ga.Spec.RunnerLabels = []string{"linux", "self-hosted", "gpu"}
	require.NoError(t, k8sClient.Update(ctx, &ga))

	var back v2alpha1.RunnerSet
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(orig), &back))
	assert.Equal(t, v2alpha1.AcquisitionProtocolClassic, back.Spec.AcquisitionProtocol, "a v2 update must not re-protocol a Classic set")
	assert.Equal(t, int32(20), back.Spec.MaxListeners)
	assert.Equal(t, []string{"linux", "self-hosted", "gpu"}, back.Spec.RunnerLabels)
}

// TestV2GA_WriteReachesValidatingWebhook proves a v2 write is validated by the
// v2alpha1-registered EgressProxy webhook, which reaches it only through
// matchPolicy Equivalent converting the request. The off-allowlist FQDN is valid
// schema, so only the webhook can refuse it.
func TestV2GA_WriteReachesValidatingWebhook(t *testing.T) {
	const ns = "v2-ga-webhook"
	createNamespace(t, ns)

	ep := &v2.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "offlist", Namespace: ns},
		Spec: v2.EgressProxySpec{
			EgressPolicyMode: v2.EgressPolicyModeFQDN,
			DestinationFQDNs: []string{"evil.example.com"},
		},
	}
	err := k8sClient.Create(ctx, ep)
	require.Error(t, err, "a v2 write naming an off-allowlist FQDN must be refused by the webhook")
	assert.Contains(t, err.Error(), "platform egress allowlist")
}

// TestV2GA_EgressProxy_AliasRejectedBySchema: v2's enum is CIDR;FQDN, so a v2 write
// naming an alias is refused by the CRD schema before any webhook runs.
func TestV2GA_EgressProxy_AliasRejectedBySchema(t *testing.T) {
	const ns = "v2-ga-alias-schema"
	createNamespace(t, ns)

	ep := &v2.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "alias", Namespace: ns},
		Spec:       v2.EgressProxySpec{EgressPolicyMode: "CiliumFQDN"},
	}
	err := k8sClient.Create(ctx, ep)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Unsupported value")
}

// TestV2GA_StoredAliasReadsAtV2 plants a stored CalicoFQDN EgressProxy and reads and
// writes it at v2, which does not define the alias. The metadata-client LIST and
// DELETECOLLECTION at v2 are the calls the namespace deleter makes, and the garbage
// collector lists at the same preferred version, so a v2 read failing here would
// leave a namespace Terminating and every EgressProxy untracked by the collector.
func TestV2GA_StoredAliasReadsAtV2(t *testing.T) {
	const ns = "v2-ga-stored-alias"
	createNamespace(t, ns)

	stored := &v2alpha1.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "legacy", Namespace: ns},
		Spec:       v2alpha1.EgressProxySpec{EgressPolicyMode: v2alpha1.EgressPolicyModeCalicoFQDN},
	}
	createStoredAliasProxy(t, stored)
	key := client.ObjectKeyFromObject(stored)
	gvr := schema.GroupVersionResource{Group: "actions-gateway.com", Version: "v2", Resource: "egressproxies"}
	meta, err := metadata.NewForConfig(testEnv.Config)
	require.NoError(t, err)

	storedMode := func() v2beta1.EgressPolicyMode {
		t.Helper()
		var hub v2beta1.EgressProxy
		require.NoError(t, k8sClient.Get(ctx, key, &hub))
		assert.NotContains(t, hub.Annotations, egressPolicyModeAnnotation, "the conversion annotation must never be stored")
		return hub.Spec.EgressPolicyMode
	}

	var ga v2.EgressProxy
	require.NoError(t, k8sClient.Get(ctx, key, &ga), "a stored alias must read at v2")
	assert.Equal(t, v2.EgressPolicyModeFQDN, ga.Spec.EgressPolicyMode)
	assert.Equal(t, "CalicoFQDN", ga.Annotations[egressPolicyModeAnnotation])

	var list v2.EgressProxyList
	require.NoError(t, k8sClient.List(ctx, &list, client.InNamespace(ns)))
	_, err = meta.Resource(gvr).Namespace(ns).List(ctx, metav1.ListOptions{})
	require.NoError(t, err, "the namespace deleter's metadata LIST at v2")

	var alpha v2alpha1.EgressProxy
	require.NoError(t, k8sClient.Get(ctx, key, &alpha))
	assert.Equal(t, v2alpha1.EgressPolicyModeCalicoFQDN, alpha.Spec.EgressPolicyMode, "v2alpha1 still shows the alias itself")

	t.Run("v2 update leaves the stored alias", func(t *testing.T) {
		require.NoError(t, k8sClient.Get(ctx, key, &ga))
		ga.Labels = map[string]string{"edited-at": "v2"}
		require.NoError(t, k8sClient.Update(ctx, &ga))
		assert.Equal(t, v2beta1.EgressPolicyModeCalicoFQDN, storedMode())
	})

	t.Run("v2 create cannot introduce an alias through the annotation", func(t *testing.T) {
		smuggled := &v2.EgressProxy{
			ObjectMeta: metav1.ObjectMeta{
				Name: "smuggled", Namespace: ns,
				Annotations: map[string]string{egressPolicyModeAnnotation: "CiliumFQDN"},
			},
			Spec: v2.EgressProxySpec{EgressPolicyMode: v2.EgressPolicyModeFQDN},
		}
		err := k8sClient.Create(ctx, smuggled)
		require.Error(t, err, "the Q1085 guard must reject a create that stores an alias")
		assert.Contains(t, err.Error(), "may no longer be introduced")
	})

	t.Run("deleting the annotation at v2 migrates the pool", func(t *testing.T) {
		require.NoError(t, k8sClient.Get(ctx, key, &ga))
		delete(ga.Annotations, egressPolicyModeAnnotation)
		require.NoError(t, k8sClient.Update(ctx, &ga))
		assert.Equal(t, v2beta1.EgressPolicyModeFQDN, storedMode())
	})

	t.Run("metadata DELETECOLLECTION at v2", func(t *testing.T) {
		second := &v2alpha1.EgressProxy{
			ObjectMeta: metav1.ObjectMeta{Name: "legacy-2", Namespace: ns},
			Spec:       v2alpha1.EgressProxySpec{EgressPolicyMode: v2alpha1.EgressPolicyModeCiliumFQDN},
		}
		createStoredAliasProxy(t, second)
		require.NoError(t, meta.Resource(gvr).Namespace(ns).DeleteCollection(ctx, metav1.DeleteOptions{}, metav1.ListOptions{}),
			"the namespace deleter's DELETECOLLECTION at v2 must get past a stored alias")
	})
}

// TestV2GA_PriorityClassAllowlist_ServedAtV2 creates a PriorityClassAllowlist at
// v2beta1 and reads it at v2. The kind has no conversion webhook, so this
// is the apiserver's None strategy, which is lossless only while the schemas match.
func TestV2GA_PriorityClassAllowlist_ServedAtV2(t *testing.T) {
	pca := &v2beta1.PriorityClassAllowlist{
		ObjectMeta: metav1.ObjectMeta{Name: "v2-ga-pca"},
		Spec: v2beta1.PriorityClassAllowlistSpec{
			AllowedPriorityClasses:      []string{"tenant-high"},
			AllowedInfraPriorityClasses: []string{"infra-critical"},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, pca))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, pca) })

	var ga v2.PriorityClassAllowlist
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKey{Name: "v2-ga-pca"}, &ga))
	assert.Equal(t, []string{"tenant-high"}, ga.Spec.AllowedPriorityClasses)
	assert.Equal(t, []string{"infra-critical"}, ga.Spec.AllowedInfraPriorityClasses)
}
