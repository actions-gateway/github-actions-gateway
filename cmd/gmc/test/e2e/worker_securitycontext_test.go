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

// E2E_AGC_WorkerSecurityContext is the CI guard for Q115: the secure-by-default
// worker SecurityContext must produce a pod kubelet will actually admit.
//
// The provisioner stamps pod-level runAsNonRoot:true on the baseline/default
// profile. kubelet can only PROVE a container is non-root against a NUMERIC
// UID; the real worker image (built FROM ghcr.io/actions/actions-runner, which
// declares the non-numeric `USER runner`) carries only a username, so without
// the runAsUser:1001 gap-fill kubelet rejects the pod at admission with
// `CreateContainerConfigError: container has runAsNonRoot and image has
// non-numeric user`.
//
// This spec uses the REAL worker image precisely so a regression of the
// runAsUser default is caught here. The agc-image placeholder the other
// lifecycle specs use runs as a numeric UID (distroless nonroot) and would mask
// it — exactly how this bug reached live validation uncaught (milestone-4 §12).
//
// Not Serial: like job_lifecycle it owner-scopes its fakegithub session lookup
// (unique agName), so it is isolated from other tenants' sessions and can run
// concurrently with the other parallel specs.
var _ = Describe("E2E_AGC_WorkerSecurityContext", Ordered, func() {
	const (
		tenantNS   = "tenant-worker-sec"
		agName     = "sec-ag"
		secretName = "github-app-secret"
	)

	var secPFCmd *exec.Cmd

	BeforeAll(func() {
		utils.CreateNamespace(tenantNS, nil)
		utils.CreateGitHubAppSecret(tenantNS, secretName, 12345, 67890, testRSAKeyPEM)
		utils.ApplyFakegithubEgressNetworkPolicy(tenantNS)

		By("applying a tenant whose RunnerGroup uses the REAL worker image (non-numeric USER runner)")
		utils.ApplyActionsGatewayCRWithRunnerGroup(tenantNS, agName, secretName, workerImage)

		By("waiting for AGC to be ready")
		utils.WaitForDeploymentReady(tenantNS, agcName, 4*time.Minute)

		By("waiting for the AGC to complete a full reconcile (token + agent registration)")
		// Deployment-ready only means the health server is up; it is decoupled
		// from token acquisition. Gate on a reconciled RunnerGroup so the session
		// wait below starts from an operational AGC rather than absorbing the
		// AGC's whole startup budget (Q134).
		utils.WaitForRunnerGroupReconciled(tenantNS, 4*time.Minute)

		By("starting port-forward to fakegithub control API")
		// Distinct port base from job_lifecycle (19090) and worker_lifecycle
		// (19300); fakegithubLocalPort is reread by the control helpers.
		fakegithubLocalPort = fmt.Sprintf("%d", 19500+GinkgoParallelProcess())
		secPFCmd = exec.Command("kubectl", "port-forward",
			"-n", infraNamespace,
			"service/"+fakegithubServiceName,
			fakegithubLocalPort+":9090",
		)
		Expect(secPFCmd.Start()).To(Succeed())
		Eventually(func() error {
			resp, err := http.Get("http://localhost:" + fakegithubLocalPort + "/control/sessions")
			if err != nil {
				return err
			}
			resp.Body.Close()
			return nil
		}, 15*time.Second, 500*time.Millisecond).Should(Succeed())
	})

	// On failure, dump AGC + fakegithub state before AfterAll tears down the
	// namespace, so a "no session registered" / "no worker pod" timeout (Q134)
	// is debuggable from CI logs rather than a bare Eventually timeout.
	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			utils.DumpAGCSessionDiagnostics(tenantNS, agcName, infraNamespace, fakegithubServiceName)
		}
	})

	AfterAll(func() {
		if secPFCmd != nil && secPFCmd.Process != nil {
			_ = secPFCmd.Process.Kill()
		}
		utils.DeleteActionsGatewayCR(tenantNS, agName)
		utils.DeleteNamespace(tenantNS)
	})

	It("E2E_AGC_WorkerPodAdmittedWithNonNumericUserImage: the default SecurityContext lets the real runner image pass kubelet runAsNonRoot admission", func() {
		fakegithubSvcURL := fmt.Sprintf("http://%s.%s.svc.cluster.local:%s",
			fakegithubServiceName, infraNamespace, fakegithubServicePort)

		By("waiting for an owner-scoped session, then enqueuing a job to trigger a worker pod")
		var sessionID string
		Eventually(func(g Gomega) {
			sessions := fakegithubActiveSessionsForOwner(g, agName+"-")
			g.Expect(sessions).NotTo(BeEmpty(), "no session registered for this RunnerGroup yet")
			sessionID = sessions[0]
		}, 3*time.Minute, 2*time.Second).Should(Succeed())

		fakegithubEnqueueJob(sessionID, map[string]interface{}{
			"jobId":           "q115-admission",
			"run_service_url": fakegithubSvcURL,
		})

		By("locating the worker pod")
		var podName string
		Eventually(func(g Gomega) {
			names := workerPodNames(g, tenantNS)
			g.Expect(names).NotTo(BeEmpty(), "no worker pod scheduled yet")
			podName = names[0]
		}, 3*time.Minute, 2*time.Second).Should(Succeed())

		By("verifying kubelet admits the pod (no CreateContainerConfigError) and creates the container")
		// The runAsNonRoot non-numeric-user check fires at container creation,
		// AFTER the (large) runner image is pulled — hence the generous timeout.
		// Success = the container reaches a running OR terminated state (its
		// runtime fate against fakegithub is irrelevant; what matters is that it
		// got PAST admission). A Q115 regression instead pins the container in
		// state.waiting.reason=CreateContainerConfigError, which we surface
		// immediately rather than waiting out the full timeout.
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "pod", podName,
				"-n", tenantNS,
				"-o", "jsonpath={.status.containerStatuses[0].state.waiting.reason}|"+
					"{.status.containerStatuses[0].state.waiting.message}|"+
					"{.status.containerStatuses[0].state.running.startedAt}|"+
					"{.status.containerStatuses[0].state.terminated.reason}",
			)
			out, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			parts := strings.SplitN(strings.TrimSpace(out), "|", 4)
			for len(parts) < 4 {
				parts = append(parts, "")
			}
			waitingReason, waitingMsg, running, terminated := parts[0], parts[1], parts[2], parts[3]

			if waitingReason == "CreateContainerConfigError" {
				StopTrying(fmt.Sprintf(
					"Q115 regression: kubelet rejected the worker pod at admission (%s): %s — "+
						"the default worker SecurityContext is missing a numeric runAsUser",
					waitingReason, waitingMsg)).Now()
			}

			g.Expect(running != "" || terminated != "").To(BeTrue(),
				"worker container not past kubelet admission yet (waiting reason=%q)", waitingReason)
		}, 8*time.Minute, 5*time.Second).Should(Succeed())
	})
})
