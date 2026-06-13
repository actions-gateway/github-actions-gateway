//go:build e2e
// +build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/actions-gateway/github-actions-gateway/gmc/test/utils"
)

// E2E_AGC_SingleUseSelfHeal proves the Q114 self-heal loop on a real cluster:
// with fakegithub simulating GitHub's single-use JIT runners (the acquired
// job consumes the runner record and kills its session), the AGC must
// re-register the agent after the job and keep serving jobs. The pre-fix AGC
// would leave the consumed session polling 401/EOF forever and the tenant's
// throughput would decay to zero after ~maxListeners jobs (M4 §12 bug 2).
var _ = Describe("E2E_AGC_SingleUseSelfHeal", Ordered, func() {
	const (
		tenantNS   = "tenant-selfheal"
		agName     = "selfheal-ag"
		secretName = "github-app-secret"
	)

	var selfhealPFCmd *exec.Cmd

	BeforeAll(func() {
		utils.CreateNamespace(tenantNS, nil)
		utils.CreateGitHubAppSecret(tenantNS, secretName, 12345, 67890, testRSAKeyPEM)
		utils.ApplyActionsGatewayCRWithRunnerGroup(tenantNS, agName, secretName, agcImage)

		By("granting workload pods egress to fakegithub in e2e-infra")
		utils.ApplyFakegithubEgressNetworkPolicy(tenantNS)

		By("waiting for AGC to be ready")
		utils.WaitForDeploymentReady(tenantNS, agcName, 4*time.Minute)

		By("starting persistent port-forward to fakegithub control API")
		fakegithubLocalPort = fmt.Sprintf("%d", 19390+GinkgoParallelProcess())
		selfhealPFCmd = exec.Command("kubectl", "port-forward",
			"-n", infraNamespace,
			"service/"+fakegithubServiceName,
			fakegithubLocalPort+":9090",
		)
		Expect(selfhealPFCmd.Start()).To(Succeed())
		Eventually(func() error {
			resp, err := http.Get("http://localhost:" + fakegithubLocalPort + "/control/sessions")
			if err != nil {
				return err
			}
			resp.Body.Close()
			return nil
		}, 15*time.Second, 500*time.Millisecond).Should(Succeed())

		// Scope the single-use simulation to this spec's RunnerGroup only
		// (session ownerName is "<group>-<index>" and the group name is
		// "<agName>-<first runner label>"), so specs running in parallel
		// against the shared fakegithub are unaffected.
		By("enabling single-use JIT simulation for this RunnerGroup")
		fakegithubControlRequest(Default, "POST",
			"/control/singleuse?enabled=true&owner="+agName+"-", nil)
	})

	AfterAll(func() {
		// Drop the simulation scope before tearing down.
		fakegithubControlRequest(nil, "POST", "/control/singleuse?enabled=false", nil)
		if selfhealPFCmd != nil && selfhealPFCmd.Process != nil {
			_ = selfhealPFCmd.Process.Kill()
		}
		utils.DeleteActionsGatewayCR(tenantNS, agName)
		utils.DeleteNamespace(tenantNS)
	})

	SetDefaultEventuallyTimeout(3 * time.Minute)
	SetDefaultEventuallyPollingInterval(2 * time.Second)

	It("re-registers single-use agents and outlives maxListeners jobs", func() {
		fakegithubSvcURL := fmt.Sprintf("http://%s.%s.svc.cluster.local:%s",
			fakegithubServiceName, infraNamespace, fakegithubServicePort)

		// All session queries are owner-filtered: fakegithub is shared across
		// parallel specs and tenants, so an unfiltered sessions[0] could belong
		// to another spec's RunnerGroup.
		ownSessions := func(g Gomega) []string {
			out := fakegithubControlRequest(g, "GET", "/control/sessions?owner="+agName+"-", nil)
			var sessions []string
			g.Expect(json.Unmarshal([]byte(out), &sessions)).To(Succeed(), "parse sessions response: %s", out)
			return sessions
		}
		workerPodCount := func(g Gomega) int {
			cmd := exec.Command("kubectl", "get", "pods",
				"-n", tenantNS,
				"-l", "app.kubernetes.io/managed-by=actions-gateway-controller",
				"--no-headers",
			)
			out, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			return len(utils.GetNonEmptyLines(out))
		}

		// Run one more job than maxListeners (2). Each acquisition consumes its
		// agent's runner record and kills its session; without the post-job
		// re-registration the pool is exhausted after 2 jobs and job 3 is never
		// delivered (the pre-Q114 tenant-death mode). With the fix, recycled
		// agents keep replacing consumed sessions indefinitely.
		for i := 1; i <= 3; i++ {
			var session string
			By(fmt.Sprintf("job %d: picking a live session", i))
			Eventually(func(g Gomega) {
				sessions := ownSessions(g)
				g.Expect(sessions).NotTo(BeEmpty(), "no live session for this RunnerGroup")
				session = sessions[0]
			}, 4*time.Minute, 2*time.Second).Should(Succeed())

			By(fmt.Sprintf("job %d: enqueuing onto %s", i, session))
			fakegithubEnqueueJob(session, map[string]interface{}{
				"jobId":           fmt.Sprintf("selfheal-job-%d", i),
				"run_service_url": fakegithubSvcURL,
			})

			By(fmt.Sprintf("job %d: waiting for its worker pod", i))
			Eventually(func(g Gomega) {
				g.Expect(workerPodCount(g)).To(BeNumerically(">=", i),
					"worker pod for job %d not scheduled yet", i)
			}, 4*time.Minute, 2*time.Second).Should(Succeed())

			By(fmt.Sprintf("job %d: waiting for the consumed session %s to be torn down", i, session))
			Eventually(func(g Gomega) {
				g.Expect(ownSessions(g)).NotTo(ContainElement(session),
					"the consumed session must be torn down, not polled forever")
			}, 4*time.Minute, 2*time.Second).Should(Succeed())
		}
	})
})
