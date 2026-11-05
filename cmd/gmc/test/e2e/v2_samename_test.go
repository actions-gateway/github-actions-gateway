//go:build e2e
// +build e2e

package e2e

import (
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/actions-gateway/github-actions-gateway/gmc/test/utils"
)

// E2E_V2_SameNameAcrossNamespaces runs two tenants that hold every object name in
// common, in different namespaces, against one GitHub org. Only the scale-set name,
// which is the first runner label, differs: the Q791 guard requires that much.
var _ = Describe("E2E_V2_SameNameAcrossNamespaces", Ordered, func() {
	const (
		nsA, nsB       = "tenant-v2-samename-a", "tenant-v2-samename-b"
		labelA, labelB = "e2e-samename-a", "e2e-samename-b"
		setName        = "shared-name"
		gateway        = "gw"
		agcDeploy      = gateway + "-agc"
		workerSA       = gateway + "-worker"
		secretName     = "github-app-secret" //nolint:gosec // G101: the NAME of a Kubernetes Secret object, not a credential value.
	)

	BeforeAll(func() {
		for ns, label := range map[string]string{nsA: labelA, nsB: labelB} {
			utils.CreateNamespace(ns, map[string]string{"actions-gateway.com/tenant": "managed"})
			utils.CreateGitHubAppSecret(ns, secretName, 12345, 67890, testRSAKeyPEM)
			Expect(utils.ApplyManifestWithWebhookRetry(v2SameNameManifest(ns, secretName, agcImage, label))).To(Succeed())
			utils.ApplyFakegithubEgressNetworkPolicy(ns)
		}
		for _, ns := range []string{nsA, nsB} {
			utils.WaitForDeploymentReady(ns, agcDeploy, 4*time.Minute)
		}
		fakegithubPFCmd = startFakegithubPortForward(19190)
	})

	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			utils.DumpAGCSessionDiagnostics(nsA, agcDeploy, infraNamespace, fakegithubServiceName)
			utils.DumpAGCSessionDiagnostics(nsB, agcDeploy, infraNamespace, fakegithubServiceName)
		}
	})

	AfterAll(func() {
		if fakegithubPFCmd != nil && fakegithubPFCmd.Process != nil {
			_ = fakegithubPFCmd.Process.Kill()
		}
		for _, ns := range []string{nsA, nsB} {
			_, _ = utils.Run(exec.Command("kubectl", "delete", "clusterrolebinding",
				"agc-clusterrunnertemplate-reader."+ns+"."+gateway, "--ignore-not-found"))
			utils.DeleteNamespace(ns)
		}
	})

	It("E2E_V2_SameNameJobsStayInTheirNamespace: each job yields a worker only in its own tenant", func() {
		waitForScaleSetSession(labelA)
		waitForScaleSetSession(labelB)
		Expect(sameNameWorkers(nsA, setName)).To(BeEmpty())
		Expect(sameNameWorkers(nsB, setName)).To(BeEmpty())

		assertSameNameJobYieldsWorker(nsA, setName, labelA, workerSA)
		Consistently(func() []string { return sameNameWorkers(nsB, setName) }, 20*time.Second, 2*time.Second).
			Should(BeEmpty(), "a job on %s reached the same-named set in %s", labelA, nsB)

		assertSameNameJobYieldsWorker(nsB, setName, labelB, workerSA)
	})

	It("E2E_V2_SameNameDeletionLeavesTheOther: deleting one set leaves the same-named set serving", func() {
		_, err := utils.Run(exec.Command("kubectl", "delete", "runnerset", setName, "-n", nsA, "--timeout=2m"))
		Expect(err).NotTo(HaveOccurred())

		Eventually(func(g Gomega) {
			g.Expect(scaleSetState(g, labelA)["activeSession"]).To(BeFalse())
		}, 2*time.Minute, time.Second).Should(Succeed(), "the deleted set's session must close")

		Expect(scaleSetState(Default, labelB)["activeSession"]).To(BeTrue())
		assertSameNameJobYieldsWorker(nsB, setName, labelB, workerSA)
	})
})

// sameNameWorkers lists the worker pods of a RunnerSet in one namespace.
func sameNameWorkers(ns, runnerSet string) []string {
	GinkgoHelper()
	out, err := utils.Run(exec.Command("kubectl", "get", "pods", "-n", ns,
		"-l", "actions-gateway.com/runner-set="+runnerSet,
		"-o", `jsonpath={range .items[*]}{.metadata.name}{"\n"}{end}`))
	Expect(err).NotTo(HaveOccurred())
	return strings.Fields(out)
}

// assertSameNameJobYieldsWorker enqueues one job and waits for a pod that did not
// exist before it, so a pod left by an earlier job cannot satisfy it.
func assertSameNameJobYieldsWorker(ns, runnerSet, scaleSet, wantSA string) {
	GinkgoHelper()
	before := sameNameWorkers(ns, runnerSet)
	scaleSetEnqueue(scaleSet)
	Eventually(func(g Gomega) {
		fresh := slices.DeleteFunc(sameNameWorkers(ns, runnerSet), func(name string) bool {
			return slices.Contains(before, name)
		})
		g.Expect(fresh).NotTo(BeEmpty(), "no new worker pod in %s", ns)
		sa, err := utils.Run(exec.Command("kubectl", "get", "pod", fresh[0], "-n", ns,
			"-o", "jsonpath={.spec.serviceAccountName}"))
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(strings.TrimSpace(sa)).To(Equal(wantSA))
	}, 3*time.Minute, 2*time.Second).Should(Succeed())
}

func v2SameNameManifest(ns, secretName, workerImage, label string) string {
	return fmt.Sprintf(`apiVersion: actions-gateway.com/v2
kind: EgressProxy
metadata:
  name: shared
  namespace: %[1]s
spec:
  minReplicas: 1
  maxReplicas: 2
---
apiVersion: actions-gateway.com/v2
kind: ActionsGateway
metadata:
  name: gw
  namespace: %[1]s
spec:
  githubURL: %[4]s/example-org-samename
  credentials:
    type: GitHubApp
    githubApp:
      name: %[2]s
  defaultProxyRef:
    name: shared
  logLevel: debug
---
apiVersion: actions-gateway.com/v2
kind: RunnerTemplate
metadata:
  name: tmpl
  namespace: %[1]s
spec:
  workerImage: %[3]s
  podTemplate:
    spec:
      containers:
      - name: runner
        image: %[3]s
---
apiVersion: actions-gateway.com/v2
kind: RunnerSet
metadata:
  name: shared-name
  namespace: %[1]s
spec:
  gatewayRef:
    name: gw
  templateRef:
    name: tmpl
  runnerLabels: ["%[5]s"]
`, ns, secretName, workerImage, fakegithubHTTPSBase(), label)
}
