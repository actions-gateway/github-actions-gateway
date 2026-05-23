//go:build e2e
// +build e2e

package e2e

import (
	"fmt"
	"os/exec"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/karlkfi/github-actions-gateway/gmc/test/utils"
)

var _ = Describe("E2E_GMC_Provisioning", Ordered, func() {
	const (
		tenantNS   = "tenant-provisioning"
		agName     = "test-ag"
		secretName = "github-app-secret"
	)

	BeforeAll(func() {
		utils.CreateNamespace(tenantNS, nil)
		utils.CreateGitHubAppSecret(tenantNS, secretName, 12345, 67890, testRSAKeyPEM)
		utils.ApplyActionsGatewayCR(tenantNS, agName, secretName)
	})

	AfterAll(func() {
		utils.DeleteActionsGatewayCR(tenantNS, agName)
		utils.DeleteNamespace(tenantNS)
	})

	SetDefaultEventuallyTimeout(3 * time.Minute)
	SetDefaultEventuallyPollingInterval(2 * time.Second)

	It("E2E_GMC_ProvisionsProxyDeployment: proxy Deployment reaches ready", func() {
		By("waiting for actions-gateway-proxy Deployment to have ready replicas")
		utils.WaitForDeploymentReady(tenantNS, "actions-gateway-proxy", 3*time.Minute)
	})

	It("E2E_GMC_ProvisionsAGCDeployment: AGC Deployment reaches ready", func() {
		By("waiting for actions-gateway-agc Deployment to have ready replicas")
		utils.WaitForDeploymentReady(tenantNS, "actions-gateway-agc", 3*time.Minute)
	})

	It("E2E_GMC_ReadyConditionTrue: ActionsGateway Ready condition becomes True", func() {
		By("checking ActionsGateway Ready condition")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "actionsgateway", agName,
				"-n", tenantNS,
				"-o", `jsonpath={.status.conditions[?(@.type=="Ready")].status}`,
			)
			out, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(Equal("True"), "Ready condition not True yet: %q", out)
		}, 4*time.Minute, 2*time.Second).Should(Succeed())
	})

	It("E2E_GMC_NetworkPoliciesCreated: NetworkPolicy is present in tenant namespace", func() {
		By("verifying NetworkPolicy exists")
		Expect(utils.ResourceExists("networkpolicy", tenantNS, "actions-gateway")).To(BeTrue())
	})

	It("E2E_GMC_ServiceAccountAndRBACCreated: ServiceAccount and RoleBinding are present", func() {
		By("checking ServiceAccount actions-gateway-agc")
		Expect(utils.ResourceExists("serviceaccount", tenantNS, "actions-gateway-agc")).To(BeTrue())
		By("checking RoleBinding actions-gateway-agc")
		Expect(utils.ResourceExists("rolebinding", tenantNS, "actions-gateway-agc")).To(BeTrue())
	})

	It("E2E_GMC_ProxyServiceCreated: proxy Service is present", func() {
		By("verifying Service actions-gateway-proxy exists")
		Expect(utils.ResourceExists("service", tenantNS, "actions-gateway-proxy")).To(BeTrue())
	})

	It("E2E_GMC_HPAAndPDBCreated: HPA and PDB are present", func() {
		By("checking HPA actions-gateway-proxy")
		Expect(utils.ResourceExists("horizontalpodautoscaler", tenantNS, "actions-gateway-proxy")).To(BeTrue())
		By("checking PDB actions-gateway-proxy")
		Expect(utils.ResourceExists("poddisruptionbudget", tenantNS, "actions-gateway-proxy")).To(BeTrue())
	})

	It("E2E_GMC_ProxyPodScheduledOnWorker: proxy pod runs on a worker node", Label("local-only"), func() {
		By("finding proxy pod node")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "pods",
				"-n", tenantNS,
				"-l", "app=actions-gateway-proxy",
				"-o", `jsonpath={.items[0].spec.nodeName}`,
			)
			out, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).NotTo(BeEmpty(), "proxy pod not yet scheduled")
			g.Expect(out).NotTo(ContainSubstring("control-plane"),
				"proxy pod scheduled on control-plane node")
		}, 3*time.Minute, 2*time.Second).Should(Succeed())
	})

	It("E2E_GMC_ReconcileAfterUpdate: changing spec triggers reconcile", func() {
		By("patching ActionsGateway proxy.minReplicas to 2")
		cmd := exec.Command("kubectl", "patch", "actionsgateway", agName,
			"-n", tenantNS,
			"--type=merge",
			"-p", `{"spec":{"proxy":{"minReplicas":2}}}`,
		)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("waiting for HPA minReplicas to reflect the change")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "hpa", "actions-gateway-proxy",
				"-n", tenantNS,
				"-o", "jsonpath={.spec.minReplicas}",
			)
			out, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(Equal("2"), fmt.Sprintf("HPA minReplicas not updated yet: %q", out))
		}, 2*time.Minute, 2*time.Second).Should(Succeed())
	})
})
