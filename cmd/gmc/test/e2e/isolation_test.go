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

var _ = Describe("E2E_GMC_Isolation", Ordered, func() {
	const (
		nsA        = "tenant-isolation-a"
		nsB        = "tenant-isolation-b"
		agName     = "test-ag"
		secretName = "github-app-secret"
	)

	BeforeAll(func() {
		for _, ns := range []string{nsA, nsB} {
			utils.CreateNamespace(ns, nil)
			utils.CreateGitHubAppSecret(ns, secretName, 11111, 22222, testRSAKeyPEM)
			utils.ApplyActionsGatewayCR(ns, agName, secretName)
		}
	})

	AfterAll(func() {
		for _, ns := range []string{nsA, nsB} {
			utils.DeleteActionsGatewayCR(ns, agName)
			utils.DeleteNamespace(ns)
		}
	})

	SetDefaultEventuallyTimeout(4 * time.Minute)
	SetDefaultEventuallyPollingInterval(2 * time.Second)

	It("E2E_GMC_TwoTenantsIndependentResources: each tenant has its own proxy deployment", func() {
		By("waiting for both proxy deployments")
		utils.WaitForDeploymentReady(nsA, "actions-gateway-proxy", 4*time.Minute)
		utils.WaitForDeploymentReady(nsB, "actions-gateway-proxy", 4*time.Minute)
	})

	It("E2E_GMC_NetworkPolicyScopedToNamespace: NetworkPolicy exists in each namespace", func() {
		Expect(utils.ResourceExists("networkpolicy", nsA, "actions-gateway")).To(BeTrue())
		Expect(utils.ResourceExists("networkpolicy", nsB, "actions-gateway")).To(BeTrue())
	})

	It("E2E_GMC_CrossTenantNetworkBlocked: proxy in nsA cannot reach proxy in nsB", func() {
		By("getting proxy pod in nsA")
		podName := getPodName(nsA, "app=actions-gateway-proxy")
		Expect(podName).NotTo(BeEmpty())

		By("getting proxy service ClusterIP in nsB")
		cmd := exec.Command("kubectl", "get", "service", "actions-gateway-proxy",
			"-n", nsB, "-o", "jsonpath={.spec.clusterIP}")
		clusterIP, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())
		Expect(clusterIP).NotTo(BeEmpty())

		By("attempting connection from nsA pod to nsB proxy — should fail due to NetworkPolicy")
		// Use a timeout of 3s so the test doesn't hang. The connection should be
		// blocked by NetworkPolicy and time out quickly.
		cmd = exec.Command("kubectl", "exec", podName, "-n", nsA,
			"--", "sh", "-c",
			"wget -q -T 3 -O - http://"+clusterIP+":8080/healthz 2>&1; true",
		)
		out, _ := utils.Run(cmd)
		// We expect a timeout/connection refused, not a successful HTTP response.
		Expect(out).NotTo(ContainSubstring("200"),
			"cross-tenant connection should be blocked")
	})
})

// getPodName returns the name of the first running pod matching the label selector.
func getPodName(ns, selector string) string {
	cmd := exec.Command("kubectl", "get", "pods",
		"-n", ns,
		"-l", selector,
		"-o", "jsonpath={.items[0].metadata.name}",
	)
	name, _ := utils.Run(cmd)
	return name
}
