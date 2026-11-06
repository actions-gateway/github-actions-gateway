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

// E2E_AGC_ScaleSetSweep is the Q1290 gate: a listener's start-up sweep collects its own
// stale record and leaves the record of a set whose first label extends its own, across
// namespaces under one org. Admission admits the pair, and the extending set's worker
// sits Pending on a nodeSelector no node carries, so its record stays offline and is
// unclaimed from the other set's side: the state a sweep must not act on.
//
// The seeded record is what stops a surviving record proving nothing: it shows the sweep
// ran and could delete. Fake GitHub cannot show what the worker or github.com then does.
var _ = Describe("E2E_AGC_ScaleSetSweep", Ordered, func() {
	const (
		shortNS, extendingNS       = "tenant-sweep-short", "tenant-sweep-extending"
		shortLabel, extendingLabel = "e2e-sweep", "e2e-sweep-linux"
		setName                    = "set"
		gwName                     = "gw"
		agcDeploy                  = gwName + "-agc"
		secretName                 = "github-app-secret" //nolint:gosec // G101: the NAME of a Kubernetes Secret object, not a credential value.
		staleRecord                = shortLabel + "-11111111-2222-5333-8444-555555555555"
	)

	var pf *exec.Cmd

	applyTenant := func(ns, label, nodeSelector string) {
		GinkgoHelper()
		utils.CreateNamespace(ns, map[string]string{"actions-gateway.com/tenant": "managed"})
		utils.CreateGitHubAppSecret(ns, secretName, 12345, 67890, testRSAKeyPEM)
		utils.ApplyFakegithubEgressNetworkPolicy(ns)
		Expect(utils.ApplyManifestWithWebhookRetry(sweepManifest(ns, secretName, workerImage, label, nodeSelector))).To(Succeed())
		utils.WaitForDeploymentReady(ns, agcDeploy, 4*time.Minute)
	}

	BeforeAll(func() {
		pf = startFakegithubPortForward(20490)
	})

	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			utils.DumpAGCSessionDiagnostics(shortNS, agcDeploy, infraNamespace, fakegithubServiceName)
			utils.DumpAGCSessionDiagnostics(extendingNS, agcDeploy, infraNamespace, fakegithubServiceName)
		}
	})

	AfterAll(func() {
		if pf != nil && pf.Process != nil {
			_ = pf.Process.Kill()
		}
		for _, ns := range []string{shortNS, extendingNS} {
			_, _ = utils.Run(exec.Command("kubectl", "delete", "runnerset", setName,
				"-n", ns, "--ignore-not-found", "--timeout=2m"))
			_, _ = utils.Run(exec.Command("kubectl", "delete", "actionsgateways.actions-gateway.com", gwName,
				"-n", ns, "--ignore-not-found", "--timeout=2m"))
			_, _ = utils.Run(exec.Command("kubectl", "delete", "clusterrolebinding",
				"agc-clusterrunnertemplate-reader."+ns+"."+gwName, "--ignore-not-found"))
			utils.DeleteNamespace(ns)
		}
	})

	It("E2E_AGC_ScaleSetSweepLeavesAnExtendingSetsRecord: the sweep collects its own stale record and no other set's", func() {
		By("holding a worker of the extending set Pending, with an offline record")
		applyTenant(extendingNS, extendingLabel, "actions-gateway.com/e2e-no-such-node")
		waitForScaleSetSession(extendingLabel)
		job := scaleSetEnqueue(extendingLabel)
		record := extendingLabel + "-" + job.JobID
		Eventually(func(g Gomega) {
			g.Expect(sweepRegistered(g, extendingLabel)).To(ContainElement(record))
			pod := firstScaleSetWorkerPod(g, extendingNS, setName)
			g.Expect(pod).NotTo(BeEmpty())
			g.Expect(sweepPodPhase(extendingNS, pod)).To(Equal("Pending"))
		}, 3*time.Minute, 2*time.Second).Should(Succeed())

		By("seeding a stale record the shorter set must collect")
		_, err := scaleSetControl("POST", "/control/scaleset/seedrunner?name="+staleRecord)
		Expect(err).NotTo(HaveOccurred())
		Expect(sweepRegistered(Default, extendingLabel)).To(ContainElement(staleRecord))

		By("starting the shorter set's listener, which admission must allow beside the other")
		applyTenant(shortNS, shortLabel, "")
		waitForScaleSetSession(shortLabel)

		Eventually(func(g Gomega) {
			g.Expect(sweepRegistered(g, extendingLabel)).NotTo(ContainElement(staleRecord))
		}, time.Minute, time.Second).Should(Succeed(),
			"the sweep never collected its own stale record, so a survivor below would prove nothing")
		Consistently(func(g Gomega) {
			g.Expect(sweepRegistered(g, extendingLabel)).To(ContainElement(record))
		}, 15*time.Second, time.Second).Should(Succeed(),
			"the shorter set's sweep deleted the extending set's record")
	})
})

// sweepRegistered reads the runner records fakegithub's scale-set registry holds.
func sweepRegistered(g Gomega, scaleSet string) []string {
	raw, ok := scaleSetState(g, scaleSet)["registered"].([]any)
	g.Expect(ok).To(BeTrue(), "fakegithub's state must report the registered runners")
	names := make([]string, 0, len(raw))
	for _, name := range raw {
		names = append(names, fmt.Sprint(name))
	}
	return names
}

func sweepPodPhase(ns, pod string) string {
	GinkgoHelper()
	out, err := utils.Run(exec.Command("kubectl", "get", "pod", pod, "-n", ns, "-o", "jsonpath={.status.phase}"))
	Expect(err).NotTo(HaveOccurred())
	return strings.TrimSpace(out)
}

func sweepManifest(ns, secretName, workerImage, label, nodeSelector string) string {
	selector := ""
	if nodeSelector != "" {
		selector = fmt.Sprintf("\n      nodeSelector:\n        %s: \"true\"", nodeSelector)
	}
	return fmt.Sprintf(`apiVersion: actions-gateway.com/v2
kind: ActionsGateway
metadata:
  name: gw
  namespace: %[1]s
spec:
  githubURL: %[4]s/example-org-sweep
  credentials:
    type: GitHubApp
    githubApp:
      name: %[2]s
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
    spec:%[6]s
      containers:
      - name: runner
        image: %[3]s
---
apiVersion: actions-gateway.com/v2
kind: RunnerSet
metadata:
  name: set
  namespace: %[1]s
spec:
  gatewayRef:
    name: gw
  templateRef:
    name: tmpl
  runnerLabels: ["%[5]s"]
`, ns, secretName, workerImage, fakegithubHTTPSBase(), label, selector)
}
