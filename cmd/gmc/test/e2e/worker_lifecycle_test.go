//go:build e2e
// +build e2e

package e2e

import (
	"fmt"
	"net/http"
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
//   - worker pods carry a controller OwnerReference to their RunnerGroup;
//   - a completed worker pod is deleted once completedPodTTL elapses, along
//     with its job Secret;
//   - a stuck-Pending worker pod (unpullable image) is reaped once
//     pendingPodDeadline elapses, with a WorkerPodStuckPending Warning event.
//
// Serial: fakegithub session IDs carry no tenant identity, so this suite —
// which enqueues jobs onto every active session — must not run concurrently
// with other session-consuming suites (job_lifecycle). In the serial phase the
// only active sessions belong to this suite's two tenants.
var _ = Describe("E2E_AGC_WorkerPodLifecycle", Ordered, Serial, func() {
	const (
		cleanNS    = "tenant-pod-clean" // worker completes fast; short completedPodTTL
		stuckNS    = "tenant-pod-stuck" // unpullable worker image; short pendingPodDeadline
		agName     = "test-ag"
		secretName = "github-app-secret"

		// stuckImage never pulls: .invalid is a reserved TLD, so the pod stays
		// Pending (ErrImagePull/ImagePullBackOff) — the exact Q95 stuck shape.
		stuckImage = "registry.invalid/missing-runner:none"

		cleanTTL      = "15s"
		stuckDeadline = "45s"
	)

	var lifecyclePFCmd *exec.Cmd

	BeforeAll(func() {
		for _, ns := range []string{cleanNS, stuckNS} {
			utils.CreateNamespace(ns, nil)
			utils.CreateGitHubAppSecret(ns, secretName, 12345, 67890, testRSAKeyPEM)
			utils.ApplyFakegithubEgressNetworkPolicy(ns)
		}
		By("applying a tenant whose workers complete fast and are reaped on a short TTL")
		utils.ApplyActionsGatewayCRWithRunnerGroupLifecycle(cleanNS, agName, secretName, agcImage, cleanTTL, "10m")
		By("applying a tenant whose workers can never pull their image")
		utils.ApplyActionsGatewayCRWithRunnerGroupLifecycle(stuckNS, agName, secretName, stuckImage, "5m", stuckDeadline)

		By("waiting for both AGCs to be ready")
		utils.WaitForDeploymentReady(cleanNS, agcName, 4*time.Minute)
		utils.WaitForDeploymentReady(stuckNS, agcName, 4*time.Minute)

		By("starting port-forward to fakegithub control API")
		// fakegithubLocalPort is shared with job_lifecycle_test.go helpers;
		// safe to reassign here because this suite is Serial.
		fakegithubLocalPort = fmt.Sprintf("%d", 19300+GinkgoParallelProcess())
		lifecyclePFCmd = exec.Command("kubectl", "port-forward",
			"-n", infraNamespace,
			"service/"+fakegithubServiceName,
			fakegithubLocalPort+":9090",
		)
		Expect(lifecyclePFCmd.Start()).To(Succeed())
		Eventually(func() error {
			resp, err := http.Get("http://localhost:" + fakegithubLocalPort + "/control/sessions")
			if err != nil {
				return err
			}
			resp.Body.Close()
			return nil
		}, 15*time.Second, 500*time.Millisecond).Should(Succeed())

		By("enqueuing one job per active session so both tenants acquire a job")
		// Session IDs are opaque: enqueue to all of them. Each session belongs
		// to exactly one tenant's AGC, so every tenant ends up with >=1 job.
		fakegithubSvcURL := fmt.Sprintf("http://%s.%s.svc.cluster.local:%s",
			fakegithubServiceName, infraNamespace, fakegithubServicePort)
		enqueued := map[string]bool{}
		Eventually(func(g Gomega) {
			for _, id := range fakegithubActiveSessions(g) {
				if !enqueued[id] {
					fakegithubEnqueueJob(id, map[string]interface{}{
						"jobId":           "q95-" + id,
						"run_service_url": fakegithubSvcURL,
					})
					enqueued[id] = true
				}
			}
			// Pods appearing in both namespaces is the success signal — it
			// means a session of each tenant received a job.
			g.Expect(workerPodNames(g, cleanNS)).NotTo(BeEmpty(), "no worker pod in %s yet", cleanNS)
			g.Expect(workerPodNames(g, stuckNS)).NotTo(BeEmpty(), "no worker pod in %s yet", stuckNS)
		}, 4*time.Minute, 3*time.Second).Should(Succeed())
	})

	AfterAll(func() {
		if lifecyclePFCmd != nil && lifecyclePFCmd.Process != nil {
			_ = lifecyclePFCmd.Process.Kill()
		}
		for _, ns := range []string{cleanNS, stuckNS} {
			utils.DeleteActionsGatewayCR(ns, agName)
			utils.DeleteNamespace(ns)
		}
	})

	It("E2E_AGC_WorkerPodOwnerRef: worker pods carry a controller ownerRef to the RunnerGroup", func() {
		// Resolve the bootstrapped RunnerGroup's actual name rather than
		// predicting it: the GMC builds it as <ag>-<labelSafe(first label)>,
		// and labelSafe appends a 7-char hash suffix.
		cmd := exec.Command("kubectl", "get", "runnergroups",
			"-n", stuckNS,
			"-o", "jsonpath={.items[0].metadata.name}",
		)
		rgName, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())
		rgName = strings.TrimSpace(rgName)
		Expect(rgName).NotTo(BeEmpty(), "expected a bootstrapped RunnerGroup in %s", stuckNS)

		// Inspect the stuck tenant's pod: it stays Pending well past this
		// assertion (deadline 45s), so there is no race with the reaper.
		cmd = exec.Command("kubectl", "get", "pods",
			"-n", stuckNS,
			"-l", "app.kubernetes.io/managed-by=actions-gateway-controller",
			"-o", `jsonpath={range .items[*]}{.metadata.ownerReferences[0].kind}/{.metadata.ownerReferences[0].name}/{.metadata.ownerReferences[0].controller}{"\n"}{end}`,
		)
		out, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())
		lines := utils.GetNonEmptyLines(out)
		Expect(lines).NotTo(BeEmpty(), "expected at least one worker pod with ownerReferences")
		for _, line := range lines {
			Expect(line).To(Equal("RunnerGroup/"+rgName+"/true"),
				"worker pod must carry a controller ownerRef to the tenant RunnerGroup")
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

		By("verifying the WorkerPodStuckPending Warning event was emitted on the RunnerGroup")
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
