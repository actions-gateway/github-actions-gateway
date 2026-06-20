//go:build e2e
// +build e2e

package e2e

import (
	"fmt"
	"os/exec"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/actions-gateway/github-actions-gateway/gmc/test/utils"
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
		Eventually(func(g Gomega) {
			for _, ns := range []string{nsA, nsB} {
				cmd := exec.Command("kubectl", "get", "deployment", proxyName,
					"-n", ns, "-o", "jsonpath={.status.readyReplicas}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).NotTo(BeEmpty(), "%s: readyReplicas not yet set", ns)
				g.Expect(out).NotTo(Equal("0"), "%s: no ready replicas yet", ns)
			}
		}, 4*time.Minute, 2*time.Second).Should(Succeed())
	})

	It("E2E_GMC_NetworkPolicyScopedToNamespace: NetworkPolicies exist in each namespace", func() {
		Expect(utils.ResourceExists("networkpolicy", nsA, proxyName)).To(BeTrue())
		Expect(utils.ResourceExists("networkpolicy", nsA, workloadName)).To(BeTrue())
		Expect(utils.ResourceExists("networkpolicy", nsA, agcName)).To(BeTrue())
		Expect(utils.ResourceExists("networkpolicy", nsB, proxyName)).To(BeTrue())
		Expect(utils.ResourceExists("networkpolicy", nsB, workloadName)).To(BeTrue())
		Expect(utils.ResourceExists("networkpolicy", nsB, agcName)).To(BeTrue())
	})

	It("E2E_GMC_CrossTenantNetworkBlocked: pod in nsA cannot reach proxy in nsB", func() {
		// Earlier revisions of this spec did `kubectl exec <proxy-pod-in-nsA> -- sh -c ...`,
		// but the proxy image is distroless (no `sh`), so the exec always failed with an
		// OCI error like `failed to start exec "<random-hex>": ... "sh": executable file
		// not found`. The only assertion was `NotTo(ContainSubstring("200"))` on that
		// error string, so the spec passed iff the random exec-session hex didn't happen
		// to contain "200" (~1.4% per run flake rate). It never actually probed the
		// NetworkPolicy. Drive a one-shot curl pod instead so the exit code reflects the
		// real network outcome.
		By("getting proxy service ClusterIP in nsB")
		cmd := exec.Command("kubectl", "get", "service", proxyName,
			"-n", nsB, "-o", "jsonpath={.spec.clusterIP}")
		clusterIP, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())
		Expect(clusterIP).NotTo(BeEmpty())

		targetURL := fmt.Sprintf("http://%s:8080/healthz", clusterIP)

		// Gate on dataplane enforcement before the single asserting curl below.
		// nsB's proxy-ingress NetworkPolicy is programmed asynchronously by the CNI.
		// calico programs it synchronously enough that the first connection is already
		// blocked, but kindnet (the default lane) has programming latency — a lone
		// connection can race ahead of enforcement and succeed, which used to flake the
		// asserting curl. So first drive a probe pod that loops curl against the nsB
		// proxy and exits 0 only after it observes the connection blocked on several
		// consecutive attempts: a deterministic "policy is enforced now" signal. If
		// enforcement never appears within the loop budget the probe exits non-zero and
		// the pod ends Failed, surfacing a real isolation regression rather than a flake.
		const probePodName = "cross-tenant-gate"

		By("deploying a probe pod in nsA that polls until the nsB proxy is blocked in the dataplane")
		probeManifest := fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %s
  namespace: %s
spec:
  restartPolicy: Never
  containers:
  - name: probe
    image: %s
    imagePullPolicy: IfNotPresent
    command: ["/bin/sh", "-c"]
    args:
    - |
      set -u
      blocks=0
      for i in $(seq 1 60); do
        if curl --silent --show-error --max-time 5 --connect-timeout 5 --output /dev/null "%s"; then
          blocks=0
          echo "attempt $i: connected — NetworkPolicy not yet enforced"
        else
          blocks=$((blocks + 1))
          echo "attempt $i: blocked ($blocks consecutive)"
          if [ "$blocks" -ge 3 ]; then
            echo "ENFORCED: cross-tenant connection blocked on $blocks consecutive attempts"
            exit 0
          fi
        fi
        sleep 2
      done
      echo "TIMEOUT: never observed a sustained cross-tenant block"
      exit 1
`, probePodName, nsA, curlImage, targetURL)

		Expect(utils.ApplyManifest(probeManifest)).To(Succeed())
		DeferCleanup(func() {
			cmd := exec.Command("kubectl", "delete", "pod", probePodName,
				"-n", nsA, "--ignore-not-found", "--wait=false")
			_, _ = utils.Run(cmd)
		})

		By("waiting for the probe to confirm enforcement is live (probe pod Succeeded)")
		var probePhase string
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "pod", probePodName,
				"-n", nsA, "-o", "jsonpath={.status.phase}")
			out, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(Or(Equal("Succeeded"), Equal("Failed")),
				"probe pod still in phase %q", out)
			probePhase = out
		}, 5*time.Minute, 2*time.Second).Should(Succeed())

		probeLogs, _ := utils.Run(exec.Command("kubectl", "logs", probePodName, "-n", nsA))
		Expect(probePhase).To(Equal("Succeeded"),
			"probe never observed the cross-tenant connection blocked, so the NetworkPolicy is not "+
				"enforced in the dataplane; got phase=%s logs:\n%s", probePhase, probeLogs)

		const curlPodName = "cross-tenant-probe"

		By("deploying a one-shot curl pod in nsA that targets the nsB proxy service")
		// Unlabeled (no actions-gateway/* label) so it is not selected by any source-side
		// NetworkPolicy in nsA — the only thing that can block the connection is nsB's
		// proxy-ingress NP, which is what this spec is meant to verify.
		manifest := fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %s
  namespace: %s
spec:
  restartPolicy: Never
  containers:
  - name: curl
    image: %s
    imagePullPolicy: IfNotPresent
    command: ["curl"]
    args:
    - "--silent"
    - "--show-error"
    - "--max-time"
    - "5"
    - "--connect-timeout"
    - "5"
    - "--output"
    - "/dev/null"
    - "--write-out"
    - "HTTP_CODE=%%{http_code}\n"
    - "http://%s:8080/healthz"
`, curlPodName, nsA, curlImage, clusterIP)

		Expect(utils.ApplyManifest(manifest)).To(Succeed())
		DeferCleanup(func() {
			cmd := exec.Command("kubectl", "delete", "pod", curlPodName,
				"-n", nsA, "--ignore-not-found", "--wait=false")
			_, _ = utils.Run(cmd)
		})

		By("waiting for the curl pod to terminate")
		var finalPhase string
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "pod", curlPodName,
				"-n", nsA, "-o", "jsonpath={.status.phase}")
			out, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(Or(Equal("Succeeded"), Equal("Failed")),
				"curl pod still in phase %q", out)
			finalPhase = out
		}, 90*time.Second, 2*time.Second).Should(Succeed())

		// Always dump logs so the CI artifact shows the real outcome.
		logsCmd := exec.Command("kubectl", "logs", curlPodName, "-n", nsA)
		logs, _ := utils.Run(logsCmd)

		// NetworkPolicy drops produce a connect timeout (curl exits 28); a missing
		// route or DNS failure produces a different non-zero code. Either way, the
		// curl process must NOT exit 0 (Succeeded) and must NOT print HTTP_CODE=200.
		Expect(finalPhase).To(Equal("Failed"),
			"cross-tenant connection should be blocked by NetworkPolicy; got phase=%s logs:\n%s", finalPhase, logs)
		Expect(logs).NotTo(ContainSubstring("HTTP_CODE=200"),
			"cross-tenant connection should be blocked; logs:\n%s", logs)
	})
})

// getPodName returns the name of the first running pod matching the label selector.
// Used by hpa_pdb_test.go and resilience_test.go.
func getPodName(ns, selector string) string {
	cmd := exec.Command("kubectl", "get", "pods",
		"-n", ns,
		"-l", selector,
		"-o", "jsonpath={.items[0].metadata.name}",
	)
	name, _ := utils.Run(cmd)
	return name
}
