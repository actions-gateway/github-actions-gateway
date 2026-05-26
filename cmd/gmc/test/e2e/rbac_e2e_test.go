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

var _ = Describe("E2E_GMC_RBAC", Ordered, func() {
	const (
		tenantNS   = "tenant-rbac"
		agName     = "test-ag"
		secretName = "github-app-secret"
	)

	BeforeAll(func() {
		utils.CreateNamespace(tenantNS, nil)
		utils.CreateGitHubAppSecret(tenantNS, secretName, 11111, 99999, testRSAKeyPEM)
		utils.ApplyActionsGatewayCR(tenantNS, agName, secretName)
		utils.WaitForDeploymentReady(tenantNS, "actions-gateway-proxy", 4*time.Minute)
	})

	AfterAll(func() {
		utils.DeleteActionsGatewayCR(tenantNS, agName)
		utils.DeleteNamespace(tenantNS)
	})

	It("E2E_GMC_AGCServiceAccountHasRole: AGC ServiceAccount is bound to expected Role", func() {
		By("checking RoleBinding subjects include actions-gateway-controller ServiceAccount")
		cmd := exec.Command("kubectl", "get", "rolebinding", "actions-gateway-controller",
			"-n", tenantNS,
			"-o", "jsonpath={.subjects[0].name}",
		)
		out, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(Equal("actions-gateway-controller"),
			"RoleBinding subject should be the AGC service account")
	})

	It("E2E_GMC_AGCCanManagePods: AGC ServiceAccount can create and delete pods", func() {
		By("checking that the agc role allows pod management")
		cmd := exec.Command("kubectl", "auth", "can-i", "create", "pods",
			"--as", "system:serviceaccount:"+tenantNS+":actions-gateway-controller",
			"-n", tenantNS,
		)
		out, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(ContainSubstring("yes"), "AGC SA should be able to create pods")

		cmd = exec.Command("kubectl", "auth", "can-i", "delete", "pods",
			"--as", "system:serviceaccount:"+tenantNS+":actions-gateway-controller",
			"-n", tenantNS,
		)
		out, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(ContainSubstring("yes"), "AGC SA should be able to delete pods")
	})

	It("E2E_GMC_AGCCannotAccessOtherNamespaces: AGC SA has no access to other namespaces", func() {
		By("checking AGC SA cannot create pods in gmc-system")
		cmd := exec.Command("kubectl", "auth", "can-i", "create", "pods",
			"--as", "system:serviceaccount:"+tenantNS+":actions-gateway-controller",
			"-n", gmcNamespace,
		)
		out, _ := utils.Run(cmd)
		Expect(out).To(ContainSubstring("no"),
			"AGC SA should not have access to "+gmcNamespace)
	})
})
