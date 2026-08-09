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

var _ = Describe("E2E_GMC_Isolation", Ordered, func() {
	const (
		nsA        = "tenant-isolation-a"
		nsB        = "tenant-isolation-b"
		agName     = "test-ag"
		secretName = "github-app-secret" //nolint:gosec // G101: the NAME of a Kubernetes Secret object, not a credential value.
	)

	BeforeAll(func() {
		for _, ns := range []string{nsA, nsB} {
			utils.CreateNamespace(ns, nil)
			utils.CreateGitHubAppSecret(ns, secretName, 11111, 22222, testRSAKeyPEM)
			utils.BaseTenant(ns, agName, secretName).ApplyWithWebhookRetry()
		}
	})

	// Dump both tenants before AfterAll deletes them: a cross-tenant block that
	// never lands is a claim about nsB's policy observed from nsA, so neither
	// side alone explains it (Q666). The enforcer state goes with them: on the
	// kindnet lane a spurious allow is as much a claim about kindnetd being alive
	// as about the policy (Q747).
	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			utils.DumpProvisioningDiagnostics(gmcNamespace, managerDeployment, nsA, nsB)
			utils.DumpCNIEnforcerState()
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
		By("getting proxy service ClusterIP in nsB")
		cmd := exec.Command("kubectl", "get", "service", proxyName,
			"-n", nsB, "-o", "jsonpath={.spec.clusterIP}")
		clusterIP, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())
		Expect(clusterIP).NotTo(BeEmpty())

		// The gate-then-assert pass runs up to crossTenantAttempts times, and a
		// retry is spent only when the CNI's NetworkPolicy enforcer restarted
		// during the attempt that saw the connection allowed.
		//
		// On the kindnet lane that enforcer is kindnetd, which runs
		// kube-network-policies with FailOpen: its nftables rules carry
		// `queue flags bypass`, so while no process is bound to the nfqueue every
		// packet is accepted and no NetworkPolicy is enforced anywhere on that
		// node. An allow observed across such a window says kindnetd was dead, not
		// that the policy is wrong, so it is discarded and re-measured. An allow
		// observed with the enforcer fingerprint unchanged is a real isolation
		// regression and fails immediately. Q747: kindnetd crash-looped on the node
		// hosting all three pods and was down for ~25 s, which is when the
		// asserting curl ran.
		const crossTenantAttempts = 2

		for attempt := 1; attempt <= crossTenantAttempts; attempt++ {
			enforcerBefore := utils.CNIEnforcerGeneration()
			logs := crossTenantProbe(nsA, clusterIP, attempt)
			if logs == "" {
				return
			}
			enforcerAfter := utils.CNIEnforcerGeneration()

			if enforcerBefore != enforcerAfter && attempt < crossTenantAttempts {
				AddReportEntry("cross-tenant allow discarded: the CNI enforcer restarted mid-probe",
					fmt.Sprintf("before: %s\nafter:  %s", enforcerBefore, enforcerAfter))
				continue
			}
			Fail(fmt.Sprintf(
				"cross-tenant connection should be blocked by NetworkPolicy, but the curl pod reached "+
					"the nsB proxy on attempt %d of %d.\ncurl logs:\n%s\n"+
					"CNI enforcer before: %s\nCNI enforcer after:  %s\n"+
					"Equal fingerprints mean no enforcer restart explains this, so read it as a real "+
					"isolation regression rather than a lane artifact (Q747).",
				attempt, crossTenantAttempts, logs, enforcerBefore, enforcerAfter))
		}
	})
})

// crossTenantProbe drives one gate-then-assert pass from ns against the other
// tenant's proxy ClusterIP. It returns the asserting curl pod's logs when the
// connection was NOT blocked, and "" when it was blocked as required; it fails
// the spec directly when the gate never observes enforcement at all.
//
// An earlier revision did `kubectl exec <proxy-pod> -- sh -c ...`, but the proxy
// image is distroless (no `sh`), so the exec always failed with an OCI error
// like `failed to start exec "<random-hex>": ... "sh": executable file not
// found`. The only assertion was `NotTo(ContainSubstring("200"))` on that error
// string, so the spec passed iff the random exec-session hex did not happen to
// contain "200" (~1.4% per run flake rate). It never probed the NetworkPolicy at
// all. Driving pods instead makes the exit code reflect the real network
// outcome.
//
// attempt suffixes the pod names from the second pass on, so a retry does not
// collide with the previous pass's pods (deleted with --wait=false).
func crossTenantProbe(ns, clusterIP string, attempt int) string {
	targetURL := fmt.Sprintf("http://%s:8080/healthz", clusterIP)
	suffix := ""
	if attempt > 1 {
		suffix = fmt.Sprintf("-retry%d", attempt)
	}

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
	//
	// Loop budget (150 iters × ~2s connected ≈ 5 min) is sized to span the outer
	// Eventually window below — kindnet's NP programming latency under a loaded CI
	// runner has exceeded the old ~2-min (60-iter) budget, ending the probe Failed
	// before enforcement landed even though calico (synchronous) passed (Q179).
	gatePodName := "cross-tenant-gate" + suffix

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
      for i in $(seq 1 150); do
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
`, gatePodName, ns, curlImage, targetURL)

	Expect(utils.ApplyManifest(probeManifest)).To(Succeed())
	DeferCleanup(func() {
		cmd := exec.Command("kubectl", "delete", "pod", gatePodName,
			"-n", ns, "--ignore-not-found", "--wait=false")
		_, _ = utils.Run(cmd)
	})

	By("waiting for the probe to confirm enforcement is live (probe pod Succeeded)")
	gatePhase := waitForPodToTerminate(ns, gatePodName, 6*time.Minute)
	probeLogs, _ := utils.Run(exec.Command("kubectl", "logs", gatePodName, "-n", ns))
	Expect(gatePhase).To(Equal("Succeeded"),
		"probe never observed the cross-tenant connection blocked, so the NetworkPolicy is not "+
			"enforced in the dataplane; got phase=%s logs:\n%s", gatePhase, probeLogs)

	curlPodName := "cross-tenant-probe" + suffix

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
`, curlPodName, ns, curlImage, clusterIP)

	Expect(utils.ApplyManifest(manifest)).To(Succeed())
	DeferCleanup(func() {
		cmd := exec.Command("kubectl", "delete", "pod", curlPodName,
			"-n", ns, "--ignore-not-found", "--wait=false")
		_, _ = utils.Run(cmd)
	})

	By("waiting for the curl pod to terminate")
	finalPhase := waitForPodToTerminate(ns, curlPodName, 90*time.Second)

	// Always dump logs so the CI artifact shows the real outcome.
	logs, _ := utils.Run(exec.Command("kubectl", "logs", curlPodName, "-n", ns))
	AddReportEntry("cross-tenant curl outcome (attempt "+fmt.Sprint(attempt)+")",
		fmt.Sprintf("phase=%s logs:\n%s", finalPhase, logs))

	// NetworkPolicy drops produce a connect timeout (curl exits 28); a missing
	// route or DNS failure produces a different non-zero code. Either way the curl
	// process must not exit 0 (Succeeded). HTTP_CODE=200 is checked separately: a
	// response body that arrives after curl's own deadline still proves the
	// connection was allowed even though the process exited non-zero.
	if finalPhase == "Succeeded" || strings.Contains(logs, "HTTP_CODE=200") {
		return fmt.Sprintf("phase=%s\n%s", finalPhase, logs)
	}
	return ""
}

// waitForPodToTerminate blocks until the pod reaches a terminal phase and
// returns it.
func waitForPodToTerminate(ns, name string, timeout time.Duration) string {
	var phase string
	Eventually(func(g Gomega) {
		cmd := exec.Command("kubectl", "get", "pod", name,
			"-n", ns, "-o", "jsonpath={.status.phase}")
		out, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(out).To(Or(Equal("Succeeded"), Equal("Failed")),
			"pod %s still in phase %q", name, out)
		phase = out
	}, timeout, 2*time.Second).Should(Succeed())
	return phase
}

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
