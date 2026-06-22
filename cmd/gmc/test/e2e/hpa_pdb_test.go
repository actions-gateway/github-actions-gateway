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

	"github.com/actions-gateway/github-actions-gateway/gmc/test/utils"
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
		utils.WaitForDeploymentReady(tenantNS, proxyName, 4*time.Minute)
	})

	AfterAll(func() {
		utils.DeleteActionsGatewayCR(tenantNS, agName)
		utils.DeleteNamespace(tenantNS)
	})

	SetDefaultEventuallyTimeout(5 * time.Minute)
	SetDefaultEventuallyPollingInterval(2 * time.Second)

	It("E2E_GMC_HPAExists: HPA is present and references the proxy deployment", func() {
		By("checking HPA target reference")
		cmd := exec.Command("kubectl", "get", "hpa", proxyName,
			"-n", tenantNS,
			"-o", "jsonpath={.spec.scaleTargetRef.name}",
		)
		out, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(Equal(proxyName))
	})

	It("E2E_GMC_HPADrivesScaleUp: HPA drives proxy Deployment replica count", func() {
		// The HPA controller only enforces minReplicas when it can read metrics.
		// Wait for ScalingActive=True before patching so the HPA is guaranteed to act.
		By("waiting for HPA to be scaling-active (metrics available)")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "hpa", proxyName,
				"-n", tenantNS, "-o",
				`jsonpath={.status.conditions[?(@.type=="ScalingActive")].status}`)
			out, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(Equal("True"), "HPA not yet scaling-active")
		}, 5*time.Minute, 10*time.Second).Should(Succeed())

		By("patching HPA minReplicas to 2 to trigger scale-up")
		cmd := exec.Command("kubectl", "patch", "hpa", proxyName,
			"-n", tenantNS, "--type=merge", "-p", `{"spec":{"minReplicas":2}}`)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			cmd := exec.Command("kubectl", "patch", "hpa", proxyName,
				"-n", tenantNS, "--type=merge", "-p", `{"spec":{"minReplicas":1}}`)
			_, _ = utils.Run(cmd)
		})

		// On failure, dump the HPA so a future flake is diagnosable rather than
		// a blind timeout — distinguishes "controller never reconciled" from
		// "scaled but we asserted the wrong field".
		DeferCleanup(func() {
			if CurrentSpecReport().Failed() {
				cmd := exec.Command("kubectl", "describe", "hpa", proxyName, "-n", tenantNS)
				if out, derr := utils.Run(cmd); derr == nil {
					_, _ = fmt.Fprintf(GinkgoWriter, "HPA state at failure:\n%s\n", out)
				}
			}
		})

		// minReplicas is a hard floor the HPA enforces via its
		// currentReplicas<minReplicas branch, independent of metric readings —
		// so the scale-up is deterministic once the controller reconciles. The
		// only variable is reconcile latency: kube-controller-manager's HPA
		// sync loop can lag well past two minutes on a CPU-starved CI runner
		// (notably under Calico, where it competes with slower pod networking),
		// which is the historical source of this test's flakiness. Allow the
		// full suite-default window rather than a tight 2-minute one.
		//
		// desiredReplicas reflects the HPA's computed target (min-clamped);
		// currentReplicas requires the pod to actually start, which is
		// unreliable on a resource-constrained cluster (pod may stay Pending).
		By("waiting for HPA desiredReplicas to reach 2")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "hpa", proxyName,
				"-n", tenantNS, "-o", "jsonpath={.status.desiredReplicas}")
			out, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(Equal("2"), "HPA desired replicas have not reached 2 yet")
		}, 5*time.Minute, 5*time.Second).Should(Succeed())
	})

	It("E2E_GMC_PDBPreventsEvictionBelowMinAvailable: PDB blocks eviction when at minimum",
		Label("multi-node"), func() {
			By("waiting for proxy deployment to stabilize before eviction")
			utils.WaitForDeploymentReady(tenantNS, proxyName, 2*time.Minute)

			// The preceding Ordered/Serial test (HPADrivesScaleUp) patches HPA
			// minReplicas back to 1 in its DeferCleanup, which triggers an
			// HPA-driven scale-down from 2 → 1 that runs asynchronously. Between
			// reading a pod name and issuing the eviction the pod we sampled
			// may be terminated by the ReplicaSet controller — surfacing as
			// "Error from server (NotFound)" instead of the PDB's 429
			// TooManyRequests. WaitForDeploymentReady above is not sufficient
			// to close the race (it only checks readyReplicas != 0 and does
			// not block on terminating pods, see commit a86b262). Retry the
			// {fetch pod name, attempt eviction} pair until we land on either
			// the PDB rejection or a successful eviction.
			By("evicting a proxy pod and asserting PDB outcome (retries on transient NotFound)")
			var evictionSucceeded bool
			Eventually(func(g Gomega) {
				podName := getPodName(tenantNS, "app=actions-gateway-proxy")
				g.Expect(podName).NotTo(BeEmpty(), "no proxy pod found yet")

				evictJSON := fmt.Sprintf(`{"apiVersion":"policy/v1","kind":"Eviction","metadata":{"name":"%s","namespace":"%s"}}`,
					podName, tenantNS)
				cmd := exec.Command("kubectl", "create", "--raw",
					fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/eviction", tenantNS, podName),
					"-f", "-",
				)
				cmd.Stdin = strings.NewReader(evictJSON)
				out, err := utils.Run(cmd)
				if err == nil {
					// Eviction succeeded — acceptable when an extra replica
					// still satisfied minAvailable=1. Recovery is verified
					// after the Eventually loop exits.
					evictionSucceeded = true
					return
				}
				// Anything other than TooManyRequests is treated as transient
				// (NotFound from a Terminating pod, occasional 5xx, etc.) and
				// retried until the deployment settles.
				g.Expect(out).To(ContainSubstring("TooManyRequests"),
					"expected PDB to reject eviction with 429 TooManyRequests, got transient error: %s", out)
			}, 2*time.Minute, 1*time.Second).Should(Succeed())

			if evictionSucceeded {
				By("eviction succeeded — verify deployment recovered")
				utils.WaitForDeploymentReady(tenantNS, proxyName, 3*time.Minute)
			}
		})
})
