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
		secretName = "github-app-secret" //nolint:gosec // G101: the NAME of a Kubernetes Secret object, not a credential value.
	)

	tenant := utils.BaseTenant(tenantNS, agName, secretName)
	agcName, proxyName, workloadName := tenant.AGCName(), tenant.ProxyName(), tenant.WorkloadPolicyName()

	BeforeAll(func() {
		utils.CreateNamespace(tenantNS, nil)
		utils.CreateGitHubAppSecret(tenantNS, secretName, 55555, 66666, testRSAKeyPEM)
		tenant.ApplyWithWebhookRetry()

		By("waiting for provisioning to complete before testing teardown")
		utils.WaitForDeploymentReady(tenantNS, proxyName, 4*time.Minute)
		// The AGC is the gateway's, provisioned in a pass unordered against the
		// proxy (Q1277); without it the removal assertions pass on an AGC that never existed.
		utils.WaitForDeploymentReady(tenantNS, agcName, 4*time.Minute)
	})

	// Dump before AfterAll deletes the namespace; a managed resource that never
	// disappears is only visible in the inventory (Q666).
	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			utils.DumpProvisioningDiagnostics(gmcNamespace, managerDeployment, tenantNS)
		}
	})

	AfterAll(func() {
		utils.DeleteNamespace(tenantNS)
	})

	SetDefaultEventuallyTimeout(3 * time.Minute)
	SetDefaultEventuallyPollingInterval(2 * time.Second)

	// v2 splits the tenant's control plane between two CRs: the ActionsGateway owns
	// the AGC and the workload NetworkPolicy, and the EgressProxy owns the proxy pool,
	// which outlives any one gateway because several may share it. Tearing the tenant
	// down is deleting both.
	It("E2E_GMC_DeleteCRRemovesResources: deleting ActionsGateway and EgressProxy removes managed resources", func() {
		By("deleting the ActionsGateway CR")
		utils.DeleteActionsGatewayCR(tenantNS, agName)

		By("waiting for the gateway's managed resources to be removed")
		Eventually(func(g Gomega) {
			g.Expect(utils.ResourceExists("deployment", tenantNS, agcName)).To(BeFalse(), "AGC Deployment still exists")
			g.Expect(utils.ResourceExists("networkpolicy", tenantNS, workloadName)).To(BeFalse(), "workload NetworkPolicy still exists")
		}, 3*time.Minute, 2*time.Second).Should(Succeed())

		By("deleting the EgressProxy CR")
		_, err := utils.Run(exec.Command("kubectl", "delete", "egressproxies.actions-gateway.com", tenant.EgressProxyName(),
			"-n", tenantNS, "--ignore-not-found", "--timeout=5m"))
		Expect(err).NotTo(HaveOccurred())

		By("waiting for the proxy pool's managed resources to be removed")
		Eventually(func(g Gomega) {
			g.Expect(utils.ResourceExists("deployment", tenantNS, proxyName)).To(BeFalse(), "proxy Deployment still exists")
			g.Expect(utils.ResourceExists("networkpolicy", tenantNS, proxyName)).To(BeFalse(), "proxy NetworkPolicy still exists")
			g.Expect(utils.ResourceExists("service", tenantNS, proxyName)).To(BeFalse(), "Service still exists")
		}, 3*time.Minute, 2*time.Second).Should(Succeed())
	})

	It("E2E_GMC_FinalizerRemovedAfterCleanup: ActionsGateway CR is fully gone after deletion", func() {
		By("confirming the ActionsGateway object itself is gone")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "actionsgateways.actions-gateway.com", agName,
				"-n", tenantNS, "--ignore-not-found",
			)
			out, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(BeEmpty(), "ActionsGateway still exists: %q", out)
		}, 2*time.Minute, 2*time.Second).Should(Succeed())
	})
})
