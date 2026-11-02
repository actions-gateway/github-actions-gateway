//go:build e2e
// +build e2e

package e2e

import (
	"fmt"
	"os/exec"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/actions-gateway/github-actions-gateway/gmc/test/utils"
)

var _ = Describe("E2E_GMC_SecurityProfile", Ordered, func() {
	const (
		ns         = "tenant-security-profile"
		agName     = "test-ag"
		secretName = "github-app-secret" //nolint:gosec // G101: the NAME of a Kubernetes Secret object, not a credential value.
	)

	BeforeAll(func() {
		// The privileged-profile label is the platform-applied eligibility gate for
		// the privileged security profile (Q133); the privileged sub-test below
		// relabels the namespace to privileged, which the
		// namespace-security-profile-guard policy rejects without it.
		utils.CreateNamespace(ns, map[string]string{
			v2alpha1.PrivilegedProfileLabel: v2alpha1.PrivilegedProfileAllowed,
		})
		utils.CreateGitHubAppSecret(ns, secretName, 55555, 66666, testRSAKeyPEM)
		utils.BaseTenant(ns, agName, secretName).ApplyWithWebhookRetry()
	})

	// Dump before AfterAll deletes the namespace; the dump's namespace object
	// carries the pod-security labels these specs assert on (Q666).
	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			utils.DumpProvisioningDiagnostics(gmcNamespace, managerDeployment, ns)
		}
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

	It("E2E_GMC_PSALabelPrivileged: privileged security profile stamps pod-security enforce=privileged", func() {
		// v2 keeps the security profile on the namespace, not the gateway. baseline
		// -> privileged is a downgrade (privileged is the least restrictive
		// profile), so the namespace-security-profile-guard policy requires the
		// explicit allow-profile-downgrade annotation. Set it in the same merge
		// patch; this also exercises the opt-in path end-to-end.
		By("relabelling the namespace to the privileged security profile with the downgrade opt-in annotation")
		patch := fmt.Sprintf(`{"metadata":{"annotations":{%q:%q},"labels":{%q:%q}}}`,
			v2alpha1.AllowProfileDowngradeAnnotation, v2alpha1.AllowProfileDowngradeAllowed,
			v2alpha1.SecurityProfileLabel, v2alpha1.SecurityProfilePrivileged)
		cmd := exec.Command("kubectl", "patch", "namespace", ns, "--type=merge", "-p", patch)
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
				"namespace enforce label must update to 'privileged' after the profile change")
		}, 2*time.Minute, 2*time.Second).Should(Succeed())
	})
})
