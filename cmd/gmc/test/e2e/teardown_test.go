//go:build e2e
// +build e2e

package e2e

import (
	"os/exec"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/actions-gateway/github-actions-gateway/gmc/test/utils"
)

var _ = Describe("E2E_GMC_Teardown", Ordered, func() {
	const (
		tenantNS   = "tenant-teardown"
		agName     = "test-ag"
		secretName = "github-app-secret"
	)

	BeforeAll(func() {
		utils.CreateNamespace(tenantNS, nil)
		utils.CreateGitHubAppSecret(tenantNS, secretName, 55555, 66666, testRSAKeyPEM)
		utils.ApplyActionsGatewayCR(tenantNS, agName, secretName)

		By("waiting for provisioning to complete before testing teardown")
		utils.WaitForDeploymentReady(tenantNS, proxyName, 4*time.Minute)
	})

	AfterAll(func() {
		utils.DeleteNamespace(tenantNS)
	})

	SetDefaultEventuallyTimeout(3 * time.Minute)
	SetDefaultEventuallyPollingInterval(2 * time.Second)

	It("E2E_GMC_DeleteCRRemovesResources: deleting ActionsGateway removes managed resources", func() {
		By("deleting the ActionsGateway CR")
		utils.DeleteActionsGatewayCR(tenantNS, agName)

		By("waiting for all managed resources to be removed")
		Eventually(func(g Gomega) {
			g.Expect(utils.ResourceExists("deployment", tenantNS, proxyName)).To(BeFalse(), "proxy Deployment still exists")
			g.Expect(utils.ResourceExists("deployment", tenantNS, agcName)).To(BeFalse(), "AGC Deployment still exists")
			g.Expect(utils.ResourceExists("networkpolicy", tenantNS, proxyName)).To(BeFalse(), "proxy NetworkPolicy still exists")
			g.Expect(utils.ResourceExists("networkpolicy", tenantNS, workloadName)).To(BeFalse(), "workload NetworkPolicy still exists")
			g.Expect(utils.ResourceExists("service", tenantNS, proxyName)).To(BeFalse(), "Service still exists")
		}, 3*time.Minute, 2*time.Second).Should(Succeed())
	})

	It("E2E_GMC_FinalizerRemovedAfterCleanup: ActionsGateway CR is fully gone after deletion", func() {
		By("confirming the ActionsGateway object itself is gone")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "actionsgateway", agName,
				"-n", tenantNS, "--ignore-not-found",
			)
			out, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(BeEmpty(), "ActionsGateway still exists: %q", out)
		}, 2*time.Minute, 2*time.Second).Should(Succeed())
	})
})
