//go:build e2e
// +build e2e

package e2e

import (
	"os"
	"os/exec"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/actions-gateway/github-actions-gateway/gmc/test/utils"
)

// Tier C tests require real GitHub App credentials.
// Set the following env vars to opt in:
//
//	GITHUB_E2E_APP_ID             — numeric GitHub App ID
//	GITHUB_E2E_INSTALLATION_ID    — installation ID
//	GITHUB_E2E_PRIVATE_KEY        — path to PEM file or PEM literal
//	GITHUB_E2E_ORG                — GitHub org that owns the App
//	GITHUB_E2E_REPO               — repository to dispatch a workflow in
//
// All Tier C tests are skipped when any of the above are absent.

var _ = Describe("E2E_GitHub_RealDispatch", Ordered, func() {
	const (
		tenantNS   = "tenant-github-real"
		agName     = "real-ag"
		secretName = "real-github-app-secret"
	)

	var githubCreds struct {
		appID          string
		installationID string
		privateKey     string
		org            string
		repo           string
	}

	BeforeAll(func() {
		githubCreds.appID = os.Getenv("GITHUB_E2E_APP_ID")
		githubCreds.installationID = os.Getenv("GITHUB_E2E_INSTALLATION_ID")
		githubCreds.privateKey = os.Getenv("GITHUB_E2E_PRIVATE_KEY")
		githubCreds.org = os.Getenv("GITHUB_E2E_ORG")
		githubCreds.repo = os.Getenv("GITHUB_E2E_REPO")

		if githubCreds.appID == "" || githubCreds.installationID == "" ||
			githubCreds.privateKey == "" || githubCreds.org == "" || githubCreds.repo == "" {
			Skip("Tier C e2e tests skipped: set GITHUB_E2E_APP_ID, GITHUB_E2E_INSTALLATION_ID, " +
				"GITHUB_E2E_PRIVATE_KEY, GITHUB_E2E_ORG, GITHUB_E2E_REPO to enable")
		}

		utils.CreateNamespace(tenantNS, nil)

		By("creating real GitHub App secret")
		pemBytes, err := loadPEMForTest(githubCreds.privateKey)
		Expect(err).NotTo(HaveOccurred(), "load GitHub App private key")

		cmd := exec.Command("kubectl", "create", "secret", "generic", secretName,
			"-n", tenantNS,
			"--from-literal=appId="+githubCreds.appID,
			"--from-literal=installationId="+githubCreds.installationID,
			"--from-literal=privateKey="+string(pemBytes),
			"--dry-run=client", "-o", "yaml",
		)
		yaml, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		applyCmd := exec.Command("kubectl", "apply", "-f", "-")
		applyCmd.Stdin = stringReader(yaml)
		_, err = utils.Run(applyCmd)
		Expect(err).NotTo(HaveOccurred())

		By("applying ActionsGateway CR with real credentials (no fake broker override)")
		// Remove AGC_EXTRA env vars that point to fakegithub so the real GitHub API is used.
		cmd = exec.Command("kubectl", "set", "env",
			"deployment/gmc-controller-manager",
			"-c", "manager",
			"-n", gmcNamespace,
			"AGC_EXTRA_GITHUB_API_BASE_URL-",
			"AGC_EXTRA_GITHUB_BROKER_URL-",
			"AGC_EXTRA_STUB_AUTH_URL-",
			"AGC_EXTRA_STUB_BROKER_URL-",
		)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		utils.ApplyActionsGatewayCR(tenantNS, agName, secretName)
		utils.WaitForDeploymentReady(tenantNS, agcName, 5*time.Minute)
	})

	AfterAll(func() {
		utils.DeleteActionsGatewayCR(tenantNS, agName)
		utils.DeleteNamespace(tenantNS)
		// Restore AGC_EXTRA env vars for any subsequent tests.
	})

	SetDefaultEventuallyTimeout(10 * time.Minute)
	SetDefaultEventuallyPollingInterval(15 * time.Second)

	It("E2E_GitHub_AGCConnectsToGitHub: AGC successfully authenticates with real GitHub", func() {
		By("verifying ActionsGateway becomes Ready")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "actionsgateway", agName,
				"-n", tenantNS,
				"-o", `jsonpath={.status.conditions[?(@.type=="Ready")].status}`,
			)
			out, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(Equal("True"))
		}, 5*time.Minute, 15*time.Second).Should(Succeed())
	})

	It("E2E_GitHub_WorkflowDispatchCreatesWorker: dispatching a workflow creates a worker pod", func() {
		By("dispatching a workflow via GitHub API")
		// Dispatch a workflow_dispatch event using the GitHub CLI.
		cmd := exec.Command("gh", "workflow", "run", "e2e-runner-test.yml",
			"--repo", githubCreds.org+"/"+githubCreds.repo,
		)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "dispatch workflow via gh CLI")

		By("waiting for a worker pod to appear")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "pods",
				"-n", tenantNS,
				"-l", "app=actions-gateway-worker",
				"--no-headers",
			)
			out, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			lines := utils.GetNonEmptyLines(out)
			g.Expect(lines).NotTo(BeEmpty(), "no worker pod created yet")
		}, 10*time.Minute, 30*time.Second).Should(Succeed())
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
