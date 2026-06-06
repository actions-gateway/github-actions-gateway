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

var _ = Describe("E2E_GMC_SecurityProfile", Ordered, func() {
	const (
		ns         = "tenant-security-profile"
		agName     = "test-ag"
		secretName = "github-app-secret"
	)

	BeforeAll(func() {
		utils.CreateNamespace(ns, nil)
		utils.CreateGitHubAppSecret(ns, secretName, 55555, 66666, testRSAKeyPEM)
		utils.ApplyActionsGatewayCR(ns, agName, secretName)
	})

	AfterAll(func() {
		utils.DeleteActionsGatewayCR(ns, agName)
		utils.DeleteNamespace(ns)
	})

	SetDefaultEventuallyTimeout(3 * time.Minute)
	SetDefaultEventuallyPollingInterval(2 * time.Second)

	It("E2E_GMC_PSALabelBaseline: GMC stamps pod-security enforce=baseline on tenant namespace by default", func() {
		By("waiting for GMC to reconcile PSA labels onto the namespace")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "namespace", ns,
				"-o", `jsonpath={.metadata.labels.pod-security\.kubernetes\.io/enforce}`,
			)
			out, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(Equal("baseline"),
				"namespace must have pod-security.kubernetes.io/enforce=baseline after GMC reconcile")
		}, 2*time.Minute, 2*time.Second).Should(Succeed())
	})

	It("E2E_GMC_PSALabelWarnAndAudit: namespace also gets warn and audit PSA labels", func() {
		By("checking warn label")
		cmd := exec.Command("kubectl", "get", "namespace", ns,
			"-o", `jsonpath={.metadata.labels.pod-security\.kubernetes\.io/warn}`,
		)
		out, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(Equal("baseline"), "namespace must have pod-security.kubernetes.io/warn=baseline")

		By("checking audit label")
		cmd = exec.Command("kubectl", "get", "namespace", ns,
			"-o", `jsonpath={.metadata.labels.pod-security\.kubernetes\.io/audit}`,
		)
		out, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(Equal("baseline"), "namespace must have pod-security.kubernetes.io/audit=baseline")
	})

	It("E2E_GMC_PSALabelPrivileged: privileged securityProfile stamps pod-security enforce=privileged", func() {
		// baseline -> privileged is a downgrade (privileged is the least
		// restrictive profile), so the GMC validating webhook requires the
		// explicit allow-profile-downgrade annotation. Set it in the same merge
		// patch; this also exercises the opt-in path end-to-end.
		By("patching ActionsGateway to privileged securityProfile with the downgrade opt-in annotation")
		cmd := exec.Command("kubectl", "patch", "actionsgateway", agName,
			"-n", ns,
			"--type=merge",
			"-p", `{"metadata":{"annotations":{"actions-gateway.github.com/allow-profile-downgrade":"true"}},"spec":{"securityProfile":"privileged"}}`,
		)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("waiting for namespace enforce label to update to privileged")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "namespace", ns,
				"-o", `jsonpath={.metadata.labels.pod-security\.kubernetes\.io/enforce}`,
			)
			out, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(Equal("privileged"),
				"namespace enforce label must update to 'privileged' after spec change")
		}, 2*time.Minute, 2*time.Second).Should(Succeed())
	})
})
