//go:build e2e
// +build e2e

package e2e

import (
	"fmt"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/actions-gateway/github-actions-gateway/gmc/test/utils"
)

// E2E_AGC_WorkerPodLifecycle proves the Q95 worker-pod cleanup contract on a
// real cluster — the tier where kubelet-set phases, image-pull failures, and
// the live Kubernetes GC controller exist:
//
//   - worker pods carry a controller OwnerReference to their RunnerSet;
//   - a completed worker pod is deleted once completedPodTTL elapses, along
//     with its job Secret;
//   - a stuck-Pending worker pod (unpullable image) is reaped once
//     pendingPodDeadline elapses, with a WorkerPodStuckPending Warning event.
//
// NOT Serial: each tenant's scale set is named after its own RunnerSet, and the
// RunnerSet after its ActionsGateway, so a job enqueued by name reaches exactly this
// suite's tenant and the specs can run in the parallel phase. That matters: Ginkgo
// runs Serial specs after every parallel spec completes. The two tenants therefore
// need ActionsGateway names distinct from each other AND from every other suite's.
var _ = Describe("E2E_AGC_WorkerPodLifecycle", Ordered, func() {
	const (
		cleanNS    = "tenant-pod-clean" // worker completes fast; short completedPodTTL
		stuckNS    = "tenant-pod-stuck" // unpullable worker image; short pendingPodDeadline
		cleanAG    = "podclean-ag"
		stuckAG    = "podstuck-ag"
		secretName = "github-app-secret" //nolint:gosec // G101: the NAME of a Kubernetes Secret object, not a credential value.

		// stuckImage never pulls: .invalid is a reserved TLD, so the pod stays
		// Pending (ErrImagePull/ImagePullBackOff) — the exact Q95 stuck shape.
		stuckImage = "registry.invalid/missing-runner:none"

		cleanTTL      = "15s"
		stuckDeadline = "45s"
	)

	var lifecyclePFCmd *exec.Cmd

	cleanTenant := utils.RunnerTenant(cleanNS, cleanAG, secretName, agcImage).WithLifecycle(cleanTTL, "10m")
	stuckTenant := utils.RunnerTenant(stuckNS, stuckAG, secretName, stuckImage).WithLifecycle("5m", stuckDeadline)

	BeforeAll(func() {
		for _, ns := range []string{cleanNS, stuckNS} {
			utils.CreateNamespace(ns, nil)
			utils.CreateGitHubAppSecret(ns, secretName, 12345, 67890, testRSAKeyPEM)
			utils.ApplyFakegithubEgressNetworkPolicy(ns)
		}
		By("applying a tenant whose workers complete fast and are reaped on a short TTL")
		cleanTenant.ApplyWithWebhookRetry()
		By("applying a tenant whose workers can never pull their image")
		stuckTenant.ApplyWithWebhookRetry()

		By("waiting for both AGCs to be ready")
		utils.WaitForDeploymentReady(cleanNS, cleanTenant.AGCName(), 4*time.Minute)
		utils.WaitForDeploymentReady(stuckNS, stuckTenant.AGCName(), 4*time.Minute)

		// fakegithubLocalPort is the package-level var the control helpers read. Every
		// suite assigns it from its own base port in its own BeforeAll; that is safe
		// without Serial because Ginkgo runs an Ordered container's specs contiguously
		// within one process, so no other container can reassign it mid-suite.
		lifecyclePFCmd = startFakegithubPortForward(19300)

		By("enqueuing one job on each of this suite's two scale sets")
		for _, t := range []utils.TenantFixture{cleanTenant, stuckTenant} {
			waitForScaleSetSession(t.ScaleSetName(0))
			scaleSetEnqueue(t.ScaleSetName(0))
		}
		// Each tenant counts once its pod has been seen: the clean tenant's one pod can
		// complete and be reaped on its 15s TTL before the stuck tenant's appears.
		seen := map[string]bool{}
		Eventually(func(g Gomega) {
			for _, ns := range []string{cleanNS, stuckNS} {
				seen[ns] = seen[ns] || len(workerPodNames(g, ns)) > 0
			}
			g.Expect(seen[cleanNS]).To(BeTrue(), "no worker pod in %s yet", cleanNS)
			g.Expect(seen[stuckNS]).To(BeTrue(), "no worker pod in %s yet", stuckNS)
		}, 4*time.Minute, 3*time.Second).Should(Succeed())
	})

	// On failure, dump both tenants' state before AfterAll deletes their
	// namespaces. Q664's stuck-Pending reap timeout left no evidence at all: the
	// suite dumped nothing and the teardown below removed the namespace seconds
	// later, so the CI log carried the timeout and nothing that could explain it.
	// Both tenants are dumped because each spec reads a different one and the
	// AfterEach cannot tell which failed.
	AfterEach(func() {
		if !CurrentSpecReport().Failed() {
			return
		}
		for _, t := range []utils.TenantFixture{stuckTenant, cleanTenant} {
			dumpWorkerPodReapState(t.Namespace)
			utils.DumpAGCSessionDiagnostics(t.Namespace, t.AGCName(), infraNamespace, fakegithubServiceName)
		}
	})

	AfterAll(func() {
		if lifecyclePFCmd != nil && lifecyclePFCmd.Process != nil {
			_ = lifecyclePFCmd.Process.Kill()
		}
		for _, tenant := range []struct{ ns, ag string }{{cleanNS, cleanAG}, {stuckNS, stuckAG}} {
			utils.DeleteActionsGatewayCR(tenant.ns, tenant.ag)
			utils.DeleteNamespace(tenant.ns)
		}
	})

	It("E2E_AGC_WorkerPodOwnerRef: worker pods carry a controller ownerRef to the RunnerSet", func() {
		rsName := stuckTenant.RunnerSetName(0)

		// Inspect the stuck tenant's pod: it stays Pending well past this
		// assertion (deadline 45s), so there is no race with the reaper.
		cmd := exec.Command("kubectl", "get", "pods",
			"-n", stuckNS,
			"-l", "app.kubernetes.io/managed-by=actions-gateway-controller",
			"-o", `jsonpath={range .items[*]}{.metadata.ownerReferences[0].kind}/{.metadata.ownerReferences[0].name}/{.metadata.ownerReferences[0].controller}{"\n"}{end}`,
		)
		out, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())
		lines := utils.GetNonEmptyLines(out)
		Expect(lines).NotTo(BeEmpty(), "expected at least one worker pod with ownerReferences")
		for _, line := range lines {
			Expect(line).To(Equal("RunnerSet/"+rsName+"/true"),
				"worker pod must carry a controller ownerRef to the tenant RunnerSet")
		}
	})

	It("E2E_AGC_CompletedPodReaped: a completed worker pod and its job Secret are deleted after completedPodTTL", func() {
		By("waiting for the clean tenant's worker pods to disappear")
		// The placeholder worker (agc image) exits within seconds; the pod goes
		// terminal and the reaper deletes it once the 15s TTL elapses.
		Eventually(func(g Gomega) {
			g.Expect(workerPodNames(g, cleanNS)).To(BeEmpty(), "completed worker pods should be reaped")
		}, 3*time.Minute, 3*time.Second).Should(Succeed())

		By("verifying the job Secret was cleaned up too")
		cmd := exec.Command("kubectl", "get", "secrets",
			"-n", cleanNS,
			"-o", "jsonpath={.items[*].metadata.name}",
		)
		out, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())
		for _, name := range strings.Fields(out) {
			Expect(name).NotTo(HavePrefix("job-"), "job Secrets must be deleted after completion")
		}
	})

	It("E2E_AGC_StuckPendingPodReaped: a Pending worker pod is reaped after pendingPodDeadline with a Warning event", func() {
		By("waiting for the stuck tenant's Pending worker pod to be reaped")
		Eventually(func(g Gomega) {
			g.Expect(workerPodNames(g, stuckNS)).To(BeEmpty(), "stuck-Pending worker pods should be reaped")
		}, 3*time.Minute, 3*time.Second).Should(Succeed())

		By("verifying the WorkerPodStuckPending Warning event was emitted on the RunnerSet")
		// Avoid --field-selector: the events.k8s.io v1 recorder's reason field
		// mapping into the core events view varies by server version.
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "events",
				"-n", stuckNS,
				"-o", `jsonpath={range .items[*]}{.type} {.reason}{"\n"}{end}`,
			)
			out, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(ContainSubstring("Warning WorkerPodStuckPending"),
				"reaping a stuck-Pending pod must emit a WorkerPodStuckPending Warning event")
		}, 30*time.Second, 2*time.Second).Should(Succeed())
	})
})

// dumpWorkerPodReapState writes the fields the reaper's decision is made from,
// for every worker pod in ns. DumpAGCSessionDiagnostics runs `kubectl describe
// pods`, which reports status.startTime but never metadata.creationTimestamp —
// the field every pendingPodDeadline is measured from — so a reap timeout is
// unreadable without this. Overdue pods say no reconcile reached the reaper; a
// pod younger than the deadline says it was re-provisioned under the assertion;
// and the deletion-reason stamp separates the third case, a reaper that marked
// the pod and then failed to delete it (the stamp-then-delete order in
// reapTarget.delete).
func dumpWorkerPodReapState(ns string) {
	cmd := exec.Command("kubectl", "get", "pods",
		"-n", ns,
		"-l", "app.kubernetes.io/managed-by=actions-gateway-controller",
		"-o", `jsonpath={range .items[*]}{.metadata.name} phase={.status.phase}`+
			` created={.metadata.creationTimestamp}`+
			` deleting={.metadata.deletionTimestamp}`+
			` reapReason={.metadata.annotations['actions-gateway\.com/deletion-reason']}`+
			` jobCompletedAt={.metadata.annotations['actions-gateway\.com/job-completed-at']}`+
			`{"\n"}{end}`,
	)
	out, err := utils.Run(cmd)
	if err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "--- worker pod reap state in %s: unavailable (%v) ---\n", ns, err)
		return
	}
	_, _ = fmt.Fprintf(GinkgoWriter, "--- worker pod reap state in %s (dumped at %s) ---\n%s\n",
		ns, time.Now().UTC().Format(time.RFC3339), out)
}

// workerPodNames lists the worker pods (provisioner-managed) in ns.
func workerPodNames(g Gomega, ns string) []string {
	cmd := exec.Command("kubectl", "get", "pods",
		"-n", ns,
		"-l", "app.kubernetes.io/managed-by=actions-gateway-controller",
		"-o", "jsonpath={.items[*].metadata.name}",
	)
	out, err := utils.Run(cmd)
	g.Expect(err).NotTo(HaveOccurred())
	return strings.Fields(out)
}
