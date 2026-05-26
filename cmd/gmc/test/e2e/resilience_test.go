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

var _ = Describe("E2E_GMC_Resilience", Ordered, Serial, func() {
	const (
		tenantNS   = "tenant-resilience"
		agName     = "test-ag"
		secretName = "github-app-secret"
	)

	BeforeAll(func() {
		utils.CreateNamespace(tenantNS, nil)
		utils.CreateGitHubAppSecret(tenantNS, secretName, 77777, 88888, testRSAKeyPEM)
		utils.ApplyActionsGatewayCR(tenantNS, agName, secretName)

		By("waiting for initial provisioning")
		utils.WaitForDeploymentReady(tenantNS, proxyName, 4*time.Minute)
	})

	AfterAll(func() {
		utils.DeleteActionsGatewayCR(tenantNS, agName)
		utils.DeleteNamespace(tenantNS)
	})

	SetDefaultEventuallyTimeout(4 * time.Minute)
	SetDefaultEventuallyPollingInterval(2 * time.Second)

	It("E2E_GMC_ProxyRecoversAfterPodDelete: proxy pod recovers after manual deletion", func() {
		By("getting the current proxy pod name")
		podName := getPodName(tenantNS, "app=actions-gateway-proxy")
		Expect(podName).NotTo(BeEmpty(), "no proxy pod found")

		By("deleting the proxy pod")
		cmd := exec.Command("kubectl", "delete", "pod", podName,
			"-n", tenantNS,
			"--grace-period=0",
		)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("waiting for the Deployment to restore a ready replica")
		utils.WaitForDeploymentReady(tenantNS, proxyName, 3*time.Minute)
	})

	It("E2E_GMC_GMCRestartPreservesState: GMC restart does not re-provision existing resources", Label("multi-node"), func() {
		By("restarting the GMC controller pod")
		cmd := exec.Command("kubectl", "rollout", "restart",
			"deployment/gmc-controller-manager",
			"-n", gmcNamespace,
		)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("waiting for GMC to be ready again")
		cmd = exec.Command("kubectl", "rollout", "status",
			"deployment/gmc-controller-manager",
			"-n", gmcNamespace,
			"--timeout=3m",
		)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("verifying proxy and AGC deployments still exist")
		Expect(utils.ResourceExists("deployment", tenantNS, proxyName)).To(BeTrue())
		Expect(utils.ResourceExists("deployment", tenantNS, agcName)).To(BeTrue())

		By("verifying ActionsGateway Ready condition is still True")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "actionsgateway", agName,
				"-n", tenantNS,
				"-o", `jsonpath={.status.conditions[?(@.type=="Ready")].status}`,
			)
			out, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(Equal("True"))
		}, 3*time.Minute, 2*time.Second).Should(Succeed())
	})
})
