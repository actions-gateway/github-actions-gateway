//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/controller"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	agcnames "github.com/actions-gateway/github-actions-gateway/agc/names"
	"github.com/actions-gateway/github-actions-gateway/api/v2"
	"github.com/actions-gateway/github-actions-gateway/scaleset/scalesettest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// createV2SetFixture creates a direct-egress gateway gw, template tmpl and rs in ns,
// and removes them afterwards.
func createV2SetFixture(t *testing.T, ns string, rs *v2.RunnerSet) {
	t.Helper()
	require.NoError(t, k8sClient.Create(ctx, newGatewayForSet("gw", ns, "")))
	require.NoError(t, k8sClient.Create(ctx, newRunnerTemplate("tmpl", ns)))
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() {
		bg := context.Background()
		_ = k8sClient.Delete(bg, rs)
		_ = k8sClient.Delete(bg, &v2.ActionsGateway{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: ns}})
		_ = k8sClient.Delete(bg, &v2.RunnerTemplate{ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: ns}})
	})
}

// TestV2_RunnerSet_WorkerPodInvariantsAdmittedUnderRestrictedPSA provisions a worker
// through the real scale-set path into a namespace enforcing PodSecurity restricted,
// and asserts the controller-enforced invariants on what the apiserver admitted: no
// API token, the worker ServiceAccount, no host namespaces, and the restricted
// security defaults. The pod existing at all is the admission half, which a fake
// client cannot give; a privileged pod in the same namespace is refused first, so the
// enforcement is shown to be live. TestV2_RunnerSet_ProvisionsWorkerPod covers the
// proxy env.
func TestV2_RunnerSet_WorkerPodInvariantsAdmittedUnderRestrictedPSA(t *testing.T) {
	const ns = "v2-rs-worker-invariants"
	require.NoError(t, k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   ns,
		Labels: map[string]string{"pod-security.kubernetes.io/enforce": "restricted"},
	}}))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
	})

	privileged := true
	err := k8sClient.Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "psa-probe", Namespace: ns},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "c", Image: "runner:test", SecurityContext: &corev1.SecurityContext{Privileged: &privileged},
		}}},
	})
	require.True(t, apierrors.IsForbidden(err), "the namespace must enforce PodSecurity restricted; got: %v", err)

	createV2SetFixture(t, ns, newRunnerSet("invariants-set", ns, "gw"))
	srv := scalesettest.New()
	t.Cleanup(srv.Close)
	startRunnerSetReconcilerOn(t, srv, func(r *controller.RunnerSetReconciler) {
		r.Provisioner.SecurityProfile = "restricted"
	})
	require.True(t, enqueueScaleSetJob(srv, "invariants-set", 15*time.Second), "a session for invariants-set should register")

	pod := waitForSetWorkerPod(t, ns, "invariants-set")
	require.NotNil(t, pod.Spec.AutomountServiceAccountToken)
	assert.False(t, *pod.Spec.AutomountServiceAccountToken, "automountServiceAccountToken must be false")
	assert.Equal(t, agcnames.WorkerSAName, pod.Spec.ServiceAccountName)
	assert.False(t, pod.Spec.HostPID || pod.Spec.HostNetwork || pod.Spec.HostIPC, "no host namespaces")

	psc := pod.Spec.SecurityContext
	require.NotNil(t, psc)
	assert.Equal(t, ptr.To(true), psc.RunAsNonRoot, "pod runAsNonRoot")
	require.NotNil(t, psc.SeccompProfile)
	assert.Equal(t, corev1.SeccompProfileTypeRuntimeDefault, psc.SeccompProfile.Type)
	for _, c := range append(pod.Spec.InitContainers, pod.Spec.Containers...) {
		require.NotNil(t, c.SecurityContext, "container %s", c.Name)
		assert.Equal(t, ptr.To(false), c.SecurityContext.AllowPrivilegeEscalation, "container %s allowPrivilegeEscalation", c.Name)
		require.NotNil(t, c.SecurityContext.Capabilities, "container %s", c.Name)
		assert.Equal(t, []corev1.Capability{"ALL"}, c.SecurityContext.Capabilities.Drop, "container %s drops ALL", c.Name)
	}
}

// TestV2_RunnerSet_ReaperTimers drives both timer arms of the reaper through the
// manager's Pod watch and RequeueAfter: a terminal worker is deleted once
// completedPodTTL passes, and one stuck Pending (envtest has no scheduler) once
// pendingPodDeadline does, while a fresh Pending worker is left alone.
// TestReaper_ReapsExpiredWorkerPods covers the arithmetic.
func TestV2_RunnerSet_ReaperTimers(t *testing.T) {
	for _, tc := range []struct {
		name, ns       string
		ttl, deadline  time.Duration
		terminal       bool
		survivorWindow time.Duration
	}{
		{"completedPodTTL", "v2-rs-reap-ttl", 2 * time.Second, time.Hour, true, 0},
		{"pendingPodDeadline", "v2-rs-reap-pending", time.Hour, 3 * time.Second, false, 2 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			createNSForAGC(t, tc.ns)
			rs := newRunnerSet("reap-set", tc.ns, "gw")
			rs.Spec.CompletedPodTTL = &metav1.Duration{Duration: tc.ttl}
			rs.Spec.PendingPodDeadline = &metav1.Duration{Duration: tc.deadline}
			createV2SetFixture(t, tc.ns, rs)
			startRunnerSetReconciler(t)
			waitForSetReadyReason(t, tc.ns, "reap-set", metav1.ConditionTrue, v2.ReasonListenerActive)

			pod := createV2WorkerPod(t, tc.ns, "reap-set", "runner-reap")
			if tc.terminal {
				pod.Status.Phase = corev1.PodSucceeded
				pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
					Name:  "runner",
					State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{FinishedAt: metav1.Now()}},
				}}
				require.NoError(t, k8sClient.Status().Update(ctx, pod))
			}
			require.Eventually(t, func() bool { return podGone(tc.ns, "runner-reap") },
				20*time.Second, 100*time.Millisecond, "the worker should be reaped once %s passes", tc.name)

			if tc.survivorWindow > 0 {
				createV2WorkerPod(t, tc.ns, "reap-set", "runner-fresh")
				require.Never(t, func() bool { return podGone(tc.ns, "runner-fresh") },
					tc.survivorWindow, 200*time.Millisecond, "a Pending worker inside the deadline must not be reaped")
			}
		})
	}
}

// TestV2_RunnerSet_WorkerPodNameAtTheDNSLabelBoundary provisions a worker for a set
// whose name puts a naive 63-character cut of "runner-<set>-<jobID>" on a hyphen of
// the fake's UUID-shaped job ID, and asserts the apiserver admitted the pod. The
// naming unit tests sweep every length against apimachinery's rules; this asserts
// against the authority that rejected the name in Q467.
func TestV2_RunnerSet_WorkerPodNameAtTheDNSLabelBoundary(t *testing.T) {
	const ns = "v2-rs-podname"
	// 23 characters: safeName adds "-" and a 7-hex digest to each segment, so the
	// naive cut lands on index 23 of the job ID, one of its hyphens.
	const set = "q1259-boundary-name-abc"
	require.Len(t, set, 23, "the boundary this test pins depends on the set name length")
	createNSForAGC(t, ns)
	createV2SetFixture(t, ns, newRunnerSet(set, ns, "gw"))
	srv := scalesettest.New()
	t.Cleanup(srv.Close)
	startRunnerSetReconcilerOn(t, srv)
	require.True(t, enqueueScaleSetJob(srv, set, 15*time.Second), "a session for %s should register", set)

	pod := waitForSetWorkerPod(t, ns, set)
	assert.LessOrEqual(t, len(pod.Name), 63)
	assert.Empty(t, validation.IsDNS1123Label(pod.Name), "worker pod name %q", pod.Name)
	assert.Contains(t, pod.Name, "q1259-boundary", "the name keeps a readable head of the set")
}

// TestV2_RunnerSet_ScaleSet_CrashedWorkerLeavesNoJobSecret is the failure half of
// TestV2_RunnerSet_ScaleSet_ReclaimsJobSecretOnCompletion: a worker that crashes
// (PodFailed, not an eviction) still has its JIT-config Secret reclaimed when GitHub
// reports the job failed, and no re-run is requested, which eviction alone earns.
func TestV2_RunnerSet_ScaleSet_CrashedWorkerLeavesNoJobSecret(t *testing.T) {
	const ns = "v2-rs-ss-crash"
	const label = "linux-crash"
	createNSForAGC(t, ns)

	var rerunCalls atomic.Int64
	fakeGitHub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "rerun-failed-jobs") {
			rerunCalls.Add(1)
		} else if r.Method == http.MethodGet {
			// The run-attempt read an eviction re-run makes first, answered as the
			// eviction test answers it, so a crash misread as an eviction does re-run.
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(runAttemptBody(rerunCalls.Load()))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(fakeGitHub.Close)

	srv := scalesettest.New()
	t.Cleanup(srv.Close)
	rs := newScaleSetRunnerSet("ss-crash", ns, "gw", label, 3)
	// The CRD floor, so a crash misread as an eviction would re-run inside the window below.
	rs.Spec.EvictionRetryDelay = &metav1.Duration{Duration: time.Second}
	rs.Spec.CompletedPodTTL = &metav1.Duration{Duration: time.Hour}
	createV2SetFixture(t, ns, rs)
	startRunnerSetReconcilerWithScaleSet(t, srv, func(p *provisioner.Provisioner) {
		p.GitHubAPIURL = fakeGitHub.URL
		p.HTTPClient = fakeGitHub.Client()
	})

	var ssID int
	require.Eventually(t, func() bool {
		id, ok := srv.ScaleSetIDByName(label)
		ssID = id
		return ok
	}, 20*time.Second, 100*time.Millisecond, "the listener must register its scale set")
	waitForSetReadyReason(t, ns, "ss-crash", metav1.ConditionTrue, v2.ReasonListenerActive)
	_, jobID := srv.EnqueueJob(ssID)

	pod := waitForSetWorkerPod(t, ns, "ss-crash")
	secretName := ""
	for _, v := range pod.Spec.Volumes {
		if v.Name == "job-payload" && v.Secret != nil {
			secretName = v.Secret.SecretName
		}
	}
	require.NotEmpty(t, secretName, "the worker must mount its job Secret")
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: secretName}, &corev1.Secret{}),
		"the job Secret must exist before the crash")

	pod.Status.Phase = corev1.PodFailed
	pod.Status.Reason = ""
	require.NoError(t, k8sClient.Status().Update(ctx, &pod))
	require.True(t, srv.CompleteAssignedJob(ssID, jobID, "failed"), "the job must be assigned server-side")

	require.Eventually(t, func() bool {
		return apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: secretName}, &corev1.Secret{}))
	}, 20*time.Second, 50*time.Millisecond, "a crashed worker's job Secret must be reclaimed")
	var secrets corev1.SecretList
	require.NoError(t, k8sClient.List(ctx, &secrets, client.InNamespace(ns), client.MatchingLabels{provisioner.LabelRunnerSet: "ss-crash"}))
	for _, s := range secrets.Items {
		assert.False(t, strings.HasPrefix(s.Name, "job-"), "job Secret %s left behind", s.Name)
	}
	assert.Never(t, func() bool { return rerunCalls.Load() > 0 }, 3*time.Second, 100*time.Millisecond,
		"a non-eviction crash must not request a re-run")
}
