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
		By("waiting for actions-gateway-controller Deployment to have ready replicas")
		utils.WaitForDeploymentReady(tenantNS, "actions-gateway-controller", 3*time.Minute)
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

	It("E2E_GMC_NetworkPoliciesCreated: NetworkPolicies are present in tenant namespace", func() {
		By("verifying proxy NetworkPolicy exists")
		Expect(utils.ResourceExists("networkpolicy", tenantNS, "actions-gateway-proxy")).To(BeTrue())
		By("verifying workload NetworkPolicy exists")
		Expect(utils.ResourceExists("networkpolicy", tenantNS, "actions-gateway-workload")).To(BeTrue())
	})

	It("E2E_GMC_ServiceAccountAndRBACCreated: ServiceAccount and RoleBinding are present", func() {
		By("checking ServiceAccount actions-gateway-controller")
		Expect(utils.ResourceExists("serviceaccount", tenantNS, "actions-gateway-controller")).To(BeTrue())
		By("checking RoleBinding actions-gateway-controller")
		Expect(utils.ResourceExists("rolebinding", tenantNS, "actions-gateway-controller")).To(BeTrue())
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

	It("E2E_GMC_ProxyPodScheduledOnWorker: proxy pod runs on a worker node", Label("multi-node"), func() {
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

	It("E2E_GMC_AGCNoCredentialEnvVars: AGC pod does not expose GitHub App credentials as env vars", func() {
		By("finding a ready AGC pod")
		var agcPodName string
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "pods",
				"-n", tenantNS,
				"-l", "app=actions-gateway-controller",
				"--field-selector=status.phase=Running",
				"-o", "jsonpath={.items[0].metadata.name}",
			)
			out, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).NotTo(BeEmpty(), "AGC pod not yet running")
			agcPodName = out
		}, 3*time.Minute, 2*time.Second).Should(Succeed())

		// The AGC container is distroless (no shell), so we inspect the pod spec
		// rather than using kubectl exec.

		By("checking no GITHUB_APP_* env vars are present in the AGC container spec")
		cmd := exec.Command("kubectl", "get", "pod", agcPodName,
			"-n", tenantNS,
			"-o", `jsonpath={range .spec.containers[?(@.name=="agc")].env[*]}{.name}{"\n"}{end}`,
		)
		out, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())
		Expect(out).NotTo(ContainSubstring("GITHUB_APP"),
			"AGC container spec must not include GITHUB_APP_* env vars; credentials must be file-mounted only")

		By("verifying credential volume is mounted at the expected path in the AGC container spec")
		cmd = exec.Command("kubectl", "get", "pod", agcPodName,
			"-n", tenantNS,
			"-o", `jsonpath={range .spec.containers[?(@.name=="agc")].volumeMounts[*]}{.mountPath}{"\n"}{end}`,
		)
		out, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(ContainSubstring("/etc/actions-gateway/github-app"),
			"AGC container must have credential volume mounted at /etc/actions-gateway/github-app")

		By("verifying GitHub App secret keys are present (they become filenames when mounted)")
		// Secret data keys become filenames in the pod's volume mount — checking the
		// secret itself is sufficient since the Secret volume mounts all keys by default.
		cmd = exec.Command("kubectl", "get", "secret", secretName,
			"-n", tenantNS,
			"-o", "jsonpath={.data}",
		)
		out, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(ContainSubstring("appId"))
		Expect(out).To(ContainSubstring("installationId"))
		Expect(out).To(ContainSubstring("privateKey"))
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
