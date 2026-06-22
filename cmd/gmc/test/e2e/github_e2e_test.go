//go:build e2e
// +build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/actions-gateway/github-actions-gateway/gmc/test/utils"
)

// Tier C tests dispatch a real workflow_dispatch event against a GitHub repo
// and verify the gateway runs the job to completion. They require a configured
// GitHub App and a repo with a self-hosted workflow committed in advance.
//
// Required env vars (test is skipped if any are absent):
//
//	GITHUB_E2E_APP_ID             — numeric GitHub App ID
//	GITHUB_E2E_INSTALLATION_ID    — numeric installation ID for the test org/repo
//	GITHUB_E2E_PRIVATE_KEY        — path to PEM file, or the PEM body
//	GITHUB_E2E_ORG                — GitHub org owning the test repo
//	GITHUB_E2E_REPO               — name of the repo containing the workflow
//	GITHUB_E2E_WORKFLOW           — optional; workflow filename (default: test-job.yml)

var _ = Describe("E2E_GitHub_RealDispatch", Ordered, Label("github-real"), func() {
	const (
		tenantNS   = "tenant-github-real"
		agName     = "real-ag"
		secretName = "real-github-app-secret"
	)

	var creds struct {
		appID          int64
		installationID int64
		privateKeyPEM  []byte
		org            string
		repo           string
		workflow       string
	}

	BeforeAll(func() {
		appIDStr := os.Getenv("GITHUB_E2E_APP_ID")
		installIDStr := os.Getenv("GITHUB_E2E_INSTALLATION_ID")
		pkValue := os.Getenv("GITHUB_E2E_PRIVATE_KEY")
		creds.org = os.Getenv("GITHUB_E2E_ORG")
		creds.repo = os.Getenv("GITHUB_E2E_REPO")
		creds.workflow = os.Getenv("GITHUB_E2E_WORKFLOW")
		if creds.workflow == "" {
			creds.workflow = "test-job.yml"
		}

		if appIDStr == "" || installIDStr == "" || pkValue == "" || creds.org == "" || creds.repo == "" {
			Skip("Tier C e2e tests skipped: set GITHUB_E2E_APP_ID, GITHUB_E2E_INSTALLATION_ID, " +
				"GITHUB_E2E_PRIVATE_KEY, GITHUB_E2E_ORG, GITHUB_E2E_REPO to enable")
		}

		var err error
		creds.appID, err = strconv.ParseInt(appIDStr, 10, 64)
		Expect(err).NotTo(HaveOccurred(), "parse GITHUB_E2E_APP_ID")
		creds.installationID, err = strconv.ParseInt(installIDStr, 10, 64)
		Expect(err).NotTo(HaveOccurred(), "parse GITHUB_E2E_INSTALLATION_ID")
		creds.privateKeyPEM, err = loadPEMForTest(pkValue)
		Expect(err).NotTo(HaveOccurred(), "load GitHub App private key")

		By("swapping fakegithub overrides for real GitHub on the GMC so AGC talks to real GitHub")
		orgURL := fmt.Sprintf("https://github.com/%s/%s", creds.org, creds.repo)
		cmd := exec.Command("kubectl", "set", "env",
			"deployment/gmc-controller-manager",
			"-c", "manager",
			"-n", gmcNamespace,
			"AGC_EXTRA_GITHUB_API_BASE_URL-",
			"AGC_EXTRA_GITHUB_BROKER_URL-",
			"AGC_EXTRA_STUB_AUTH_URL-",
			"AGC_EXTRA_STUB_BROKER_URL-",
			"AGC_EXTRA_GITHUB_ORG_URL="+orgURL,
		)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "set GMC env vars for real GitHub")

		By("waiting for GMC rollout to settle after env change")
		cmd = exec.Command("kubectl", "rollout", "status",
			"deployment/gmc-controller-manager",
			"-n", gmcNamespace,
			"--timeout=3m",
		)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		utils.CreateNamespace(tenantNS, nil)
		utils.CreateGitHubAppSecret(tenantNS, secretName, creds.appID, creds.installationID, creds.privateKeyPEM)

		By("applying ActionsGateway CR with a RunnerGroup pointing at the worker image")
		utils.ApplyActionsGatewayCRWithRunnerGroup(tenantNS, agName, secretName, workerImage)

		By("waiting for AGC Deployment to be ready")
		utils.WaitForDeploymentReady(tenantNS, agcName, 5*time.Minute)
	})

	AfterAll(func() {
		// Best-effort dump of AGC logs before teardown to aid diagnosis on failure.
		if CurrentSpecReport().Failed() {
			By("dumping AGC pod logs")
			cmd := exec.Command("kubectl", "logs", "-n", tenantNS,
				"deployment/"+agcName, "--tail=300")
			out, _ := utils.Run(cmd)
			_, _ = fmt.Fprintln(GinkgoWriter, out)
		}
		utils.DeleteActionsGatewayCR(tenantNS, agName)
		utils.DeleteNamespace(tenantNS)
		// Restore fakegithub-pointing env vars so subsequent suites in this
		// process work, and drop the org URL we set.
		fakegithubBaseURL := fmt.Sprintf("http://%s.%s.svc.cluster.local:%s",
			fakegithubServiceName, infraNamespace, fakegithubServicePort)
		cmd := exec.Command("kubectl", "set", "env",
			"deployment/gmc-controller-manager",
			"-c", "manager",
			"-n", gmcNamespace,
			"AGC_EXTRA_GITHUB_ORG_URL-",
			fmt.Sprintf("AGC_EXTRA_GITHUB_API_BASE_URL=%s", fakegithubBaseURL),
			fmt.Sprintf("AGC_EXTRA_GITHUB_BROKER_URL=%s", fakegithubBaseURL),
			fmt.Sprintf("AGC_EXTRA_STUB_AUTH_URL=%s/token", fakegithubBaseURL),
			fmt.Sprintf("AGC_EXTRA_STUB_BROKER_URL=%s", fakegithubBaseURL),
		)
		_, _ = utils.Run(cmd)
	})

	SetDefaultEventuallyTimeout(10 * time.Minute)
	SetDefaultEventuallyPollingInterval(15 * time.Second)

	It("E2E_GitHub_ActionsGatewayReachesReady: CR Ready=True with real GitHub", func() {
		By("verifying ActionsGateway becomes Ready")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "actionsgateways.actions-gateway.github.com", agName,
				"-n", tenantNS,
				"-o", `jsonpath={.status.conditions[?(@.type=="Ready")].status}`,
			)
			out, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(out)).To(Equal("True"), "ActionsGateway not Ready yet")
		}, 5*time.Minute, 10*time.Second).Should(Succeed())
	})

	It("E2E_GitHub_WorkflowCompletesGreen: dispatched workflow runs to success", func() {
		dispatchedAt := time.Now().UTC().Add(-30 * time.Second).Format("2006-01-02T15:04:05Z")

		By(fmt.Sprintf("dispatching workflow %q via gh CLI", creds.workflow))
		cmd := exec.Command("gh", "workflow", "run", creds.workflow,
			"--repo", creds.org+"/"+creds.repo,
			"--ref", "main",
		)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "dispatch workflow via gh CLI")

		By("locating the dispatched workflow run")
		var runID string
		Eventually(func(g Gomega) {
			cmd := exec.Command("gh", "run", "list",
				"--repo", creds.org+"/"+creds.repo,
				"--workflow", creds.workflow,
				"--created", ">="+dispatchedAt,
				"--limit", "1",
				"--json", "databaseId,status,conclusion",
			)
			out, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			var runs []struct {
				DatabaseID int64  `json:"databaseId"`
				Status     string `json:"status"`
				Conclusion string `json:"conclusion"`
			}
			g.Expect(json.Unmarshal([]byte(out), &runs)).To(Succeed(), "parse gh run list: %s", out)
			g.Expect(runs).NotTo(BeEmpty(), "no run found yet")
			runID = fmt.Sprintf("%d", runs[0].DatabaseID)
		}, 2*time.Minute, 5*time.Second).Should(Succeed())

		By("waiting for a worker pod to appear in the tenant namespace")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "pods",
				"-n", tenantNS,
				"-l", "app.kubernetes.io/managed-by=actions-gateway-controller",
				"--no-headers",
			)
			out, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			lines := utils.GetNonEmptyLines(out)
			g.Expect(lines).NotTo(BeEmpty(), "no worker pod scheduled yet")
		}, 8*time.Minute, 10*time.Second).Should(Succeed())

		By(fmt.Sprintf("waiting for workflow run %s to complete with conclusion=success", runID))
		Eventually(func(g Gomega) {
			cmd := exec.Command("gh", "run", "view", runID,
				"--repo", creds.org+"/"+creds.repo,
				"--json", "status,conclusion",
			)
			out, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			var r struct {
				Status     string `json:"status"`
				Conclusion string `json:"conclusion"`
			}
			g.Expect(json.Unmarshal([]byte(out), &r)).To(Succeed())
			g.Expect(r.Status).To(Equal("completed"), "workflow still %q", r.Status)
			g.Expect(r.Conclusion).To(Equal("success"),
				"workflow concluded %q (expected success)", r.Conclusion)
		}, 10*time.Minute, 15*time.Second).Should(Succeed())
	})
})

// loadPEMForTest reads a PEM key from a file path or returns the value if it looks like a PEM literal.
func loadPEMForTest(value string) ([]byte, error) {
	const pemHeader = "-----"
	if len(value) >= len(pemHeader) && value[:len(pemHeader)] == pemHeader {
		return []byte(value), nil
	}
	return os.ReadFile(value)
}
