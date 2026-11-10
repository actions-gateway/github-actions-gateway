//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	v2 "github.com/actions-gateway/github-actions-gateway/api/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

const (
	egressPeerTooBroad  = "cidr must be a valid CIDR no broader than /16 for IPv4 or /48 for IPv6"
	egressPeerLinkLocal = "cidr must not be link-local"
)

// egressPeerConsumers are the two fields that carry an EgressPeer. The CRD repeats the
// peer's schema under each, so a rule on the shared type is exercised once per consumer.
var egressPeerConsumers = map[string]struct {
	// schemaPath is the peer's field path below the CRD's spec.
	schemaPath []string
	gateway    func(ns, name string, peer *v2.EgressPeer) *v2.ActionsGateway
}{
	"tracing": {
		schemaPath: []string{"tracing", "networkPolicy"},
		gateway: func(ns, name string, peer *v2.EgressPeer) *v2.ActionsGateway {
			ag := newV2GatewayWired(name, ns, "github-app", "")
			ag.Spec.Tracing = v2.TracingConfig{Endpoint: "https://collector:4317", NetworkPolicy: peer}
			return ag
		},
	},
	"vault": {
		schemaPath: []string{"credentials", "workloadIdentity", "signer", "vault", "networkPolicy"},
		gateway: func(ns, name string, peer *v2.EgressPeer) *v2.ActionsGateway {
			ag := newV2WorkloadIdentityGateway(ns, name)
			ag.Spec.Credentials.WorkloadIdentity.Signer.Vault.NetworkPolicy = peer
			return ag
		},
	},
}

// TestV2_EgressPeer_CIDRAdmission runs each CIDR through the real apiserver, under both
// consumers. The accepted ones are what an external Vault or collector needs: one address,
// a subnet, or a network up to an IPv4 /16 or an IPv6 /48. The rejected ones are a peer
// broader than that, which a link-local check alone admits (0.0.0.0/1 overlaps no
// link-local range), and a peer naming link-local space or one of the listed metadata
// addresses.
func TestV2_EgressPeer_CIDRAdmission(t *testing.T) {
	const ns = "v2-egress-peer-admission"
	createNamespace(t, ns)

	cases := []struct{ cidr, reject string }{
		{cidr: "10.0.5.7/32"},
		{cidr: "10.0.5.0/24"},
		{cidr: "10.0.5.7/24"}, // host bits set
		{cidr: "10.0.0.0/16"},
		{cidr: "169.253.0.0/16"},
		{cidr: "169.255.0.0/16"},
		{cidr: "2001:db8::1/128"},
		{cidr: "2001:db8:1::/48"},
		{cidr: "fd00:ec3::/48"},
		{cidr: "fd00:ec2::253/128"}, // beside a named address, not over it
		{cidr: "100.100.100.201/32"},
		{cidr: "100.101.0.0/16"},
		{cidr: "168.63.129.17/32"},

		{cidr: "0.0.0.0/0", reject: egressPeerTooBroad},
		{cidr: "0.0.0.0/1", reject: egressPeerTooBroad},
		{cidr: "128.0.0.0/1", reject: egressPeerTooBroad},
		{cidr: "10.0.0.0/15", reject: egressPeerTooBroad},
		{cidr: "169.0.0.0/8", reject: egressPeerTooBroad},
		{cidr: "::/0", reject: egressPeerTooBroad},
		{cidr: "2001:db8::/47", reject: egressPeerTooBroad},
		{cidr: "fe80::/10", reject: egressPeerTooBroad},
		// The pattern admits all six, and CEL's isCIDR() takes none of them.
		{cidr: "999.1.1.1/24", reject: egressPeerTooBroad},
		{cidr: "10.0.256.1/32", reject: egressPeerTooBroad},
		{cidr: "010.0.5.7/32", reject: egressPeerTooBroad},         // a leading zero, which CEL's cidr() refuses
		{cidr: "::ffff:a9fe:a9fe/128", reject: egressPeerTooBroad}, // IPv4-mapped 169.254.169.254
		{cidr: "0:0:0:0:0:ffff:a9fe:a9fe/128", reject: egressPeerTooBroad},
		{cidr: "2001:db8::/064", reject: egressPeerTooBroad}, // a leading zero in the prefix length

		{cidr: "169.254.169.254/32", reject: egressPeerLinkLocal},
		{cidr: "169.254.169.0/24", reject: egressPeerLinkLocal},
		{cidr: "169.254.0.0/16", reject: egressPeerLinkLocal},
		{cidr: "fe80::1/128", reject: egressPeerLinkLocal},
		{cidr: "febf:ffff:ffff::/48", reject: egressPeerLinkLocal},
		{cidr: "fd00:ec2::254/128", reject: egressPeerLinkLocal},
		{cidr: "fd00:ec2::/48", reject: egressPeerLinkLocal},
		{cidr: "fd20:ce::254/128", reject: egressPeerLinkLocal},
		{cidr: "fd20:ce::/64", reject: egressPeerLinkLocal},
		// Metadata and platform addresses outside link-local space.
		{cidr: "100.100.100.200/32", reject: egressPeerLinkLocal},
		{cidr: "100.100.0.0/16", reject: egressPeerLinkLocal},
		{cidr: "168.63.129.16/32", reject: egressPeerLinkLocal},
		{cidr: "168.63.0.0/16", reject: egressPeerLinkLocal},
	}
	for consumer, c := range egressPeerConsumers {
		for i, tc := range cases {
			t.Run(consumer+"/"+tc.cidr, func(t *testing.T) {
				ag := c.gateway(ns, fmt.Sprintf("%s-%d", consumer, i), &v2.EgressPeer{CIDR: tc.cidr})
				err := k8sClient.Create(ctx, ag)
				if tc.reject == "" {
					require.NoError(t, err)
					require.NoError(t, k8sClient.Delete(context.Background(), ag))
					return
				}
				require.Error(t, err)
				assert.True(t, apierrors.IsInvalid(err), "expected Invalid, got %v", err)
				assert.Contains(t, err.Error(), tc.reject)
			})
		}
		// The CIDR rules sit on the cidr field, so a selector peer never meets them.
		t.Run(consumer+"/selector", func(t *testing.T) {
			ag := c.gateway(ns, consumer+"-selector", &v2.EgressPeer{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "peer"}},
			})
			require.NoError(t, k8sClient.Create(ctx, ag))
			require.NoError(t, k8sClient.Delete(context.Background(), ag))
		})
	}
}

// TestV2_EgressPeer_StoredCIDRKeepsWorking: a gateway stored with a peer CIDR the rules now
// refuse still takes unrelated writes, the GMC's status write included, under both
// consumers. Each rule's oldSelf clause passes the value while it is unchanged; changing it
// to another refused value is rejected, and changing it to an admissible one succeeds.
func TestV2_EgressPeer_StoredCIDRKeepsWorking(t *testing.T) {
	const ns = "v2-egress-peer-ratchet"
	createNamespaceWithLabels(t, ns, map[string]string{
		v2.SecurityProfileLabel: v2.SecurityProfileRestricted,
	})
	createGitHubAppSecret(t, ns, "github-app")

	crd := &unstructured.Unstructured{}
	crd.SetGroupVersionKind(schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"})
	crdKey := types.NamespacedName{Name: "actionsgateways.actions-gateway.com"}

	for consumer, c := range egressPeerConsumers {
		t.Run(consumer, func(t *testing.T) {
			rulesPath := []string{"schema", "openAPIV3Schema", "properties", "spec"}
			for _, field := range append(append([]string{}, c.schemaPath...), "cidr") {
				rulesPath = append(rulesPath, "properties", field)
			}
			rulesPath = append(rulesPath, "x-kubernetes-validations")

			setRules := func(rules []any) {
				require.Eventually(t, func() bool {
					if err := k8sClient.Get(ctx, crdKey, crd); err != nil {
						return false
					}
					versions, _, _ := unstructured.NestedSlice(crd.Object, "spec", "versions")
					for i, v := range versions {
						version := v.(map[string]any)
						if version["name"] != "v2" {
							continue
						}
						if rules == nil {
							unstructured.RemoveNestedField(version, rulesPath...)
						} else if err := unstructured.SetNestedSlice(version, rules, rulesPath...); err != nil {
							return false
						}
						versions[i] = version
					}
					if err := unstructured.SetNestedSlice(crd.Object, versions, "spec", "versions"); err != nil {
						return false
					}
					return k8sClient.Update(ctx, crd) == nil
				}, 15*time.Second, 200*time.Millisecond, "update the CRD")
			}
			require.NoError(t, k8sClient.Get(ctx, crdKey, crd))
			versions, _, _ := unstructured.NestedSlice(crd.Object, "spec", "versions")
			var original, without []any
			for _, v := range versions {
				version := v.(map[string]any)
				if version["name"] == "v2" {
					original, _, _ = unstructured.NestedSlice(version, rulesPath...)
				}
			}
			for _, r := range original {
				rule := r.(map[string]any)
				msg, _ := rule["message"].(string)
				if strings.HasPrefix(msg, egressPeerTooBroad) || strings.HasPrefix(msg, egressPeerLinkLocal) {
					require.Equal(t, true, rule["optionalOldSelf"], "an unchanged value must pass on the status path too")
					continue
				}
				without = append(without, r)
			}
			require.Len(t, without, len(original)-2, "both rules this test exercises must exist")
			t.Cleanup(func() { setRules(original) })

			// One stored value per rule, so each oldSelf clause is the one under test.
			stored := map[string]struct{ cidr, message string }{
				consumer + "-broad":      {cidr: "0.0.0.0/0", message: egressPeerTooBroad},
				consumer + "-link-local": {cidr: "169.254.169.254/32", message: egressPeerLinkLocal},
			}

			setRules(without)
			for name, s := range stored {
				ag := c.gateway(ns, name, &v2.EgressPeer{CIDR: s.cidr})
				require.Eventually(t, func() bool { return k8sClient.Create(ctx, ag) == nil },
					15*time.Second, 200*time.Millisecond, "the CRD without the rules must admit %s", s.cidr)
				t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ag) })
			}

			setRules(original)
			for _, s := range stored {
				require.Eventually(t, func() bool {
					probe := c.gateway(ns, consumer+"-probe", &v2.EgressPeer{CIDR: s.cidr})
					err := k8sClient.Create(ctx, probe)
					if err == nil {
						_ = k8sClient.Delete(ctx, probe)
					}
					return apierrors.IsInvalid(err) && strings.Contains(err.Error(), s.message)
				}, 15*time.Second, 200*time.Millisecond, "the restored rules must reject a new object with %s", s.cidr)
			}

			peerOf := func(ag *v2.ActionsGateway) *v2.EgressPeer {
				if consumer == "vault" {
					return ag.Spec.Credentials.WorkloadIdentity.Signer.Vault.NetworkPolicy
				}
				return ag.Spec.Tracing.NetworkPolicy
			}
			for name, s := range stored {
				update := func(edit func(*v2.ActionsGateway)) error {
					var got v2.ActionsGateway
					if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &got); err != nil {
						return err
					}
					edit(&got)
					return k8sClient.Update(ctx, &got)
				}
				require.NoError(t, update(func(*v2.ActionsGateway) {}), "re-applying %s unchanged must succeed", s.cidr)
				require.NoError(t, update(func(ag *v2.ActionsGateway) { ag.Spec.LogLevel = "debug" }),
					"editing another field must succeed")
				port := int32(8443)
				require.NoError(t, update(func(ag *v2.ActionsGateway) { peerOf(ag).Port = &port }),
					"editing the peer's port must succeed")
				var got v2.ActionsGateway
				require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &got))
				got.Status.Conditions = []metav1.Condition{{Type: "Probe", Status: metav1.ConditionTrue, Reason: "Probe", LastTransitionTime: metav1.Now()}}
				require.NoError(t, k8sClient.Status().Update(ctx, &got), "a status write, as the GMC makes, must succeed")

				err := update(func(ag *v2.ActionsGateway) { peerOf(ag).CIDR = "0.0.0.0/1" })
				require.Error(t, err, "changing the CIDR to another refused one must be rejected")
				assert.True(t, apierrors.IsInvalid(err), "expected Invalid, got %v", err)
				assert.Contains(t, err.Error(), egressPeerTooBroad)
				require.NoError(t, update(func(ag *v2.ActionsGateway) { peerOf(ag).CIDR = "10.0.5.7/32" }),
					"narrowing the CIDR must succeed")
			}
		})
	}
}
