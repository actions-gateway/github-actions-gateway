//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	agcv1alpha1 "github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	"github.com/onsi/gomega"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// priorityClassGuardManifestPath is the real shipped policy — loading it (rather
// than reconstructing the objects in Go) means a CEL typo or a wrong
// matchCondition in the deployed artifact is caught here.
const priorityClassGuardManifestPath = "../../../config/admission-policy/priorityclass-allowlist-guard.yaml"

// vapParamCMName / vapParamCMNamespace are the fixed names the shipped manifest's
// binding paramRef points at.
const (
	vapParamCMName      = "gag-priorityclass-allowlist"
	vapParamCMNamespace = "gmc-system"
)

// installPriorityClassGuard applies the real VAP + binding + default (empty)
// param ConfigMap. The param ConfigMap is namespaced, so its namespace must exist
// first. Objects are installed idempotently; the ConfigMap's contents are owned
// (and mutated) by the test that calls this.
func installPriorityClassGuard(t *testing.T) {
	t.Helper()
	createNamespace(t, vapParamCMNamespace)

	f, err := os.Open(priorityClassGuardManifestPath)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()

	dec := utilyaml.NewYAMLOrJSONDecoder(f, 4096)
	for {
		u := &unstructured.Unstructured{}
		if decErr := dec.Decode(u); decErr != nil {
			if decErr == io.EOF {
				break
			}
			require.NoError(t, decErr)
		}
		if len(u.Object) == 0 {
			continue
		}
		if createErr := k8sClient.Create(ctx, u); createErr != nil && !apierrors.IsAlreadyExists(createErr) {
			require.NoError(t, createErr)
		}
	}
}

// setGuardAllowlist writes the param ConfigMap's allowedPriorityClasses value,
// creating the ConfigMap if a prior step deleted it.
func setGuardAllowlist(t *testing.T, value string) {
	t.Helper()
	var cm corev1.ConfigMap
	err := k8sClient.Get(ctx, types.NamespacedName{Namespace: vapParamCMNamespace, Name: vapParamCMName}, &cm)
	if apierrors.IsNotFound(err) {
		cm = corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: vapParamCMNamespace, Name: vapParamCMName},
			Data:       map[string]string{"allowedPriorityClasses": value},
		}
		require.NoError(t, k8sClient.Create(ctx, &cm))
		return
	}
	require.NoError(t, err)
	cm.Data = map[string]string{"allowedPriorityClasses": value}
	require.NoError(t, k8sClient.Update(ctx, &cm))
}

// guardedRG returns a RunnerGroup naming tierClass in priorityTiers (when
// non-empty) and podClass in podTemplate.spec (when non-empty) — the two routes
// the policy gates.
func guardedRG(ns, name, tierClass, podClass string) *agcv1alpha1.RunnerGroup {
	rg := &agcv1alpha1.RunnerGroup{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: agcv1alpha1.RunnerGroupSpec{
			MaxListeners: 1,
			RunnerLabels: []string{"self-hosted"},
			PodTemplate: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					PriorityClassName: podClass,
					Containers:        []corev1.Container{{Name: "runner", Image: "runner:test"}},
				},
			},
		},
	}
	if tierClass != "" {
		maxWorkers := int32(5)
		rg.Spec.MaxWorkers = &maxWorkers
		rg.Spec.PriorityTiers = []agcv1alpha1.PriorityTier{{PriorityClassName: tierClass, Threshold: 5}}
	}
	return rg
}

// TestGMC_PriorityClassGuard_GatesDirectRunnerGroupWrites verifies the Q289 VAP
// backstop (Appendix G §G.7): RunnerGroups have no validating webhook, so a
// principal with direct runnergroups RBAC bypasses the ActionsGateway webhook's
// PriorityClass allowlist — unless this policy denies the write. The policy reads
// its allowlist from a paramKind ConfigMap and must (1) deny off-allowlist
// classes on both the priorityTiers and podTemplate.spec routes, (2) admit
// allowlisted classes and class-free RunnerGroups, (3) fail closed — for every
// runnergroups write — when the param ConfigMap is missing, and (4) re-validate
// stored objects on update, which a webhook alone cannot do for the bypass path.
func TestGMC_PriorityClassGuard_GatesDirectRunnerGroupWrites(t *testing.T) {
	installPriorityClassGuard(t)

	const ns = "pcguard"
	const escalation = "system-cluster-critical"
	const allowed = "runner-standard"
	createNamespace(t, ns)

	g := gomega.NewWithT(t)

	// Gate: wait until the policy is enforced (VAP enforcement is not instantaneous
	// after the binding is created). The shipped param ConfigMap is empty, so a
	// tier naming any class must become Forbidden.
	g.Eventually(func() bool {
		err := k8sClient.Create(ctx, guardedRG(ns, "probe", escalation, ""))
		if err == nil {
			// Enforcement not active yet; remove the probe and keep polling.
			_ = k8sClient.Delete(ctx, guardedRG(ns, "probe", escalation, ""))
			return false
		}
		return apierrors.IsForbidden(err)
	}, 30*time.Second, 100*time.Millisecond).Should(gomega.BeTrue(),
		"a tier naming a class must be denied under the shipped empty allowlist")

	t.Run("empty allowlist denies both routes", func(t *testing.T) {
		err := k8sClient.Create(ctx, guardedRG(ns, "tier-escalate", escalation, ""))
		require.True(t, apierrors.IsForbidden(err), "tier route: want Forbidden, got: %v", err)
		err = k8sClient.Create(ctx, guardedRG(ns, "pod-escalate", "", escalation))
		require.True(t, apierrors.IsForbidden(err), "podTemplate route: want Forbidden, got: %v", err)
	})

	t.Run("class-free RunnerGroups are never subject to the policy", func(t *testing.T) {
		rg := guardedRG(ns, "plain", "", "")
		require.NoError(t, k8sClient.Create(ctx, rg))
		t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), rg) })
	})

	// Allowlist the class (newline-separated to exercise the same separators the
	// GMC ConfigMap watcher accepts) — both routes must admit it, live.
	setGuardAllowlist(t, allowed+"\nrunner-bursty")
	g.Eventually(func() error {
		return k8sClient.Create(ctx, guardedRG(ns, "tier-allowed", allowed, ""))
	}, 30*time.Second, 100*time.Millisecond).Should(gomega.Succeed(),
		"an allowlisted tier class must be admitted once the param ConfigMap lists it")

	t.Run("allowlisted class admitted on both routes", func(t *testing.T) {
		rg := guardedRG(ns, "pod-allowed", "", "runner-bursty")
		require.NoError(t, k8sClient.Create(ctx, rg))
	})

	t.Run("escalation stays denied regardless of the dynamic set", func(t *testing.T) {
		err := k8sClient.Create(ctx, guardedRG(ns, "tier-escalate-2", escalation, ""))
		require.True(t, apierrors.IsForbidden(err), "want Forbidden, got: %v", err)
	})

	t.Run("update re-validates stored objects", func(t *testing.T) {
		// The stored-object narrowing a webhook cannot provide on the bypass path:
		// shrink the allowlist, then touch the previously-admitted RunnerGroup — the
		// write must be denied even though the class was legal when stored.
		setGuardAllowlist(t, "runner-bursty")
		gomega.NewWithT(t).Eventually(func() bool {
			var rg agcv1alpha1.RunnerGroup
			if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "tier-allowed"}, &rg); err != nil {
				return false
			}
			rg.Spec.MaxListeners = 2
			err := k8sClient.Update(ctx, &rg)
			return apierrors.IsForbidden(err)
		}, 30*time.Second, 100*time.Millisecond).Should(gomega.BeTrue(),
			"an update to a stored RunnerGroup naming a now-off-allowlist class must be denied")
	})

	t.Run("missing param ConfigMap fails closed for every write", func(t *testing.T) {
		// parameterNotFoundAction: Deny is the never-silently-off contract: the
		// apiserver resolves binding params before any per-object filtering, so a
		// deleted param ConfigMap denies EVERY runnergroups write — including
		// class-free ones — until it is recreated. Loud and fail-closed by design;
		// the ConfigMap ships with the policy, so this state is an explicit deletion.
		var cm corev1.ConfigMap
		require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: vapParamCMNamespace, Name: vapParamCMName}, &cm))
		require.NoError(t, k8sClient.Delete(ctx, &cm))

		// A create that slips through before the apiserver observes the deletion is
		// removed and retried under a fresh name — a fixed name would turn every
		// subsequent attempt into AlreadyExists and never observe the denial. The
		// param-not-found denial does not carry StatusReasonForbidden (unlike a
		// validation-failure denial), so it is matched by the denying policy's name.
		attempt := 0
		var lastErr error
		gomega.NewWithT(t).Eventually(func() bool {
			attempt++
			rg := guardedRG(ns, fmt.Sprintf("paramless-%d", attempt), "", "")
			lastErr = k8sClient.Create(ctx, rg)
			if lastErr == nil {
				_ = k8sClient.Delete(ctx, rg)
				return false
			}
			return strings.Contains(lastErr.Error(), "gag-priorityclass-allowlist-guard") &&
				strings.Contains(lastErr.Error(), "denied request")
		}, 30*time.Second, 100*time.Millisecond).Should(gomega.BeTrue(), func() string {
			return fmt.Sprintf("parameterNotFoundAction: Deny must deny even a class-free write when the ConfigMap is gone; last create error: %v", lastErr)
		})

		// Recreating the ConfigMap restores normal admission — class-free writes
		// are admitted again without touching the policy or binding.
		setGuardAllowlist(t, "")
		recovered := 0
		gomega.NewWithT(t).Eventually(func() error {
			recovered++
			return k8sClient.Create(ctx, guardedRG(ns, fmt.Sprintf("recovered-%d", recovered), "", ""))
		}, 30*time.Second, 100*time.Millisecond).Should(gomega.Succeed(),
			"recreating the param ConfigMap must restore admission of class-free RunnerGroups")
	})
}
