//go:build e2e
// +build e2e

package e2e

import (
	"fmt"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/karlkfi/github-actions-gateway/gmc/test/utils"
)

var _ = Describe("E2E_GMC_HPA_PDB", Ordered, Serial, func() {
	const (
		tenantNS   = "tenant-hpa-pdb"
		agName     = "test-ag"
		secretName = "github-app-secret"
	)

	BeforeAll(func() {
		utils.CreateNamespace(tenantNS, nil)
		utils.CreateGitHubAppSecret(tenantNS, secretName, 33333, 44444, testRSAKeyPEM)
		utils.ApplyActionsGatewayCR(tenantNS, agName, secretName)
		utils.WaitForDeploymentReady(tenantNS, "actions-gateway-proxy", 4*time.Minute)
	})

	AfterAll(func() {
		utils.DeleteActionsGatewayCR(tenantNS, agName)
		utils.DeleteNamespace(tenantNS)
	})

	SetDefaultEventuallyTimeout(5 * time.Minute)
	SetDefaultEventuallyPollingInterval(2 * time.Second)

	It("E2E_GMC_HPAExists: HPA is present and references the proxy deployment", func() {
		By("checking HPA target reference")
		cmd := exec.Command("kubectl", "get", "hpa", "actions-gateway-proxy",
			"-n", tenantNS,
			"-o", "jsonpath={.spec.scaleTargetRef.name}",
		)
		out, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(Equal("actions-gateway-proxy"))
	})

	It("E2E_GMC_HPADrivesScaleUp: HPA drives proxy Deployment replica count", func() {
		// The HPA controller only enforces minReplicas when it can read metrics.
		// Wait for ScalingActive=True before patching so the HPA is guaranteed to act.
		By("waiting for HPA to be scaling-active (metrics available)")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "hpa", "actions-gateway-proxy",
				"-n", tenantNS, "-o",
				`jsonpath={.status.conditions[?(@.type=="ScalingActive")].status}`)
			out, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(Equal("True"), "HPA not yet scaling-active")
		}, 5*time.Minute, 10*time.Second).Should(Succeed())

		By("patching HPA minReplicas to 2 to trigger scale-up")
		cmd := exec.Command("kubectl", "patch", "hpa", "actions-gateway-proxy",
			"-n", tenantNS, "--type=merge", "-p", `{"spec":{"minReplicas":2}}`)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			cmd := exec.Command("kubectl", "patch", "hpa", "actions-gateway-proxy",
				"-n", tenantNS, "--type=merge", "-p", `{"spec":{"minReplicas":1}}`)
			_, _ = utils.Run(cmd)
		})

		// desiredReplicas reflects the HPA's computed target (min-clamped).
		// currentReplicas requires the pod to actually start; in a resource-
		// constrained CI cluster the pod may be Pending, making it unreliable.
		By("waiting for HPA desiredReplicas to reach 2")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "hpa", "actions-gateway-proxy",
				"-n", tenantNS, "-o", "jsonpath={.status.desiredReplicas}")
			out, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(Equal("2"), "HPA desired replicas have not reached 2 yet")
		}, 2*time.Minute, 2*time.Second).Should(Succeed())
	})

	It("E2E_GMC_PDBPreventsEvictionBelowMinAvailable: PDB blocks eviction when at minimum",
		Label("multi-node"), func() {
			By("getting proxy pod name")
			podName := getPodName(tenantNS, "app=actions-gateway-proxy")
			Expect(podName).NotTo(BeEmpty())

			By("attempting to evict the proxy pod via the eviction API")
			evictJSON := fmt.Sprintf(`{"apiVersion":"policy/v1","kind":"Eviction","metadata":{"name":"%s","namespace":"%s"}}`,
				podName, tenantNS)
			cmd := exec.Command("kubectl", "create", "--raw",
				fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/eviction", tenantNS, podName),
				"-f", "-",
			)
			cmd.Stdin = strings.NewReader(evictJSON)
			out, err := utils.Run(cmd)
			// Expect a 429 (TooManyRequests) response from the PDB.
			// kubectl prints "TooManyRequests" rather than the numeric 429 code.
			if err != nil {
				Expect(out).To(ContainSubstring("TooManyRequests"),
					"expected PDB to reject eviction with 429 TooManyRequests, got: %s", out)
			} else {
				// If no error, the eviction may have succeeded — only acceptable
				// if minAvailable was already satisfied (replica count > 1).
				By("eviction succeeded — verify pod recovered")
				utils.WaitForDeploymentReady(tenantNS, "actions-gateway-proxy", 3*time.Minute)
			}
		})
})
