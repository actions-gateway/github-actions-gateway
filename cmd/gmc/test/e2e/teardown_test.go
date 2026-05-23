//go:build e2e
// +build e2e

package e2e

import (
	"os/exec"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/karlkfi/github-actions-gateway/gmc/test/utils"
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
		utils.WaitForDeploymentReady(tenantNS, "actions-gateway-proxy", 4*time.Minute)
	})

	AfterAll(func() {
		utils.DeleteNamespace(tenantNS)
	})

	SetDefaultEventuallyTimeout(3 * time.Minute)
	SetDefaultEventuallyPollingInterval(2 * time.Second)

	It("E2E_GMC_DeleteCRRemovesResources: deleting ActionsGateway removes managed resources", func() {
		By("deleting the ActionsGateway CR")
		utils.DeleteActionsGatewayCR(tenantNS, agName)

		By("waiting for proxy Deployment to be removed")
		Eventually(func(g Gomega) {
			g.Expect(utils.ResourceExists("deployment", tenantNS, "actions-gateway-proxy")).To(BeFalse(),
				"proxy Deployment still exists after CR deletion")
		}, 3*time.Minute, 2*time.Second).Should(Succeed())

		By("waiting for AGC Deployment to be removed")
		Eventually(func(g Gomega) {
			g.Expect(utils.ResourceExists("deployment", tenantNS, "actions-gateway-agc")).To(BeFalse(),
				"AGC Deployment still exists after CR deletion")
		}, 3*time.Minute, 2*time.Second).Should(Succeed())

		By("verifying NetworkPolicy is removed")
		Eventually(func(g Gomega) {
			g.Expect(utils.ResourceExists("networkpolicy", tenantNS, "actions-gateway")).To(BeFalse())
		}, 2*time.Minute, 2*time.Second).Should(Succeed())

		By("verifying Service is removed")
		Eventually(func(g Gomega) {
			g.Expect(utils.ResourceExists("service", tenantNS, "actions-gateway-proxy")).To(BeFalse())
		}, 2*time.Minute, 2*time.Second).Should(Succeed())
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
