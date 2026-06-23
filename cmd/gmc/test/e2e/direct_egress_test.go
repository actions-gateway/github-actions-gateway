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

// E2E_V2_DirectEgress is the live-enforcement counterpart to the Q168 envtest
// coverage (cmd/gmc/internal/controller/integration/v2_direct_egress_test.go).
// Envtest proves the *shape* of the direct-egress NetworkPolicies but has no CNI,
// so it cannot prove the default-deny egress lockdown is actually enforced at
// runtime. This suite stands up a proxy-less v2 ActionsGateway (no
// defaultProxyRef) with a proxy-less RunnerSet (no proxyRef) on a real kind
// cluster and proves both halves of the §H.10 secure-by-default contract:
//
//   - Positive (runs on BOTH CNI legs): a workload-labelled pod in the
//     direct-egress tenant reaches GitHub directly — no proxy in the path. This
//     confirms the workload NetworkPolicy's GitHub-CIDR allowance lets proxy-less
//     workers egress to GitHub.
//   - Negative (Calico leg ONLY): from the same workload network context, a
//     connection to a non-GitHub destination is dropped by the default-deny
//     egress NetworkPolicy. This is the defence-in-depth point — dropping the
//     proxy drops egress *identity*, never egress *restriction*.
//
// The negative MUST gate on an egress-enforcing CNI: kindnet accepts
// NetworkPolicy objects but its bundled kube-network-policies enforcer does not
// drop egress (Q7b/Q119), so on kindnet the "non-GitHub blocked" assertion would
// falsely pass (catching nothing) or fail for the wrong reason. It self-skips
// there via egressEnforcingCNI() — the Calico lane (e2e-calico.yml) exercises it.
// The positive runs everywhere: on kindnet it proves the path works under a
// permissive enforcer; on Calico it additionally proves the GitHub-CIDR allow
// rule is programmed and admits the traffic.
var _ = Describe("E2E_V2_DirectEgress", Ordered, func() {
	const (
		tenantNS   = "tenant-v2-direct"
		secretName = "github-app-secret"
		gwName     = "direct"
		// Per-gateway derived names (§H.16 #1): "<gw>-agc" Deployment, "<gw>-workload"
		// workload NetworkPolicy.
		agcDeploy  = gwName + "-agc"
		workloadNP = gwName + "-workload"
	)

	BeforeAll(func() {
		utils.CreateNamespace(tenantNS, map[string]string{
			// Mark the namespace a v2-managed tenant so the GMC's dual-reading VAPs
			// admit its provisioning.
			"actions-gateway.com/tenant": "managed",
		})
		utils.CreateGitHubAppSecret(tenantNS, secretName, 13579, 24680, testRSAKeyPEM)

		By("applying the proxy-less v2 object set: one ActionsGateway (no defaultProxyRef), one template, one RunnerSet (no proxyRef)")
		Expect(utils.ApplyManifest(directEgressManifest(tenantNS, secretName, agcImage))).To(Succeed())

		// Deployment readiness is decoupled from GitHub reachability (the AGC binds
		// its health server early — see WaitForRunnerGroupReconciled's comment), so
		// this succeeds even though the e2e AGC is redirected to fakegithub, which
		// the direct-egress AGC NetworkPolicy does NOT allow (fakegithub is an
		// in-cluster pod, not a GitHub CIDR). That is intentional: this suite probes
		// the workload NetworkPolicy via dedicated probe pods, not via a live broker
		// session, so it deliberately does NOT stamp an e2e fakegithub-egress carve
		// out (doing so would punch a hole that defeats the negative assertion).
		By("waiting for the proxy-less AGC Deployment to become ready (proves the gateway provisioned)")
		utils.WaitForDeploymentReady(tenantNS, agcDeploy, 4*time.Minute)
	})

	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			utils.DumpAGCSessionDiagnostics(tenantNS, agcDeploy, infraNamespace, fakegithubServiceName)
		}
	})

	AfterAll(func() {
		// Cluster-scoped ClusterRoleBindings are not namespace-GC'd; delete so reruns
		// on a persisted cluster start clean (mirrors v2_multigateway_test.go).
		_, _ = utils.Run(exec.Command("kubectl", "delete", "clusterrolebinding",
			"agc-clusterrunnertemplate-reader."+tenantNS+"."+gwName, "--ignore-not-found"))
		utils.DeleteNamespace(tenantNS)
	})

	SetDefaultEventuallyTimeout(3 * time.Minute)
	SetDefaultEventuallyPollingInterval(2 * time.Second)

	It("E2E_V2_DirectEgress_Provisions: proxy-less gateway wires direct mode and a GitHub-CIDR workload NetworkPolicy", func() {
		By("verifying the AGC Deployment carries NO proxy env (direct mode: control-plane HTTP goes direct)")
		// In direct mode the GMC omits HTTP(S)_PROXY/PROXY_TLS_SECRET_NAME entirely
		// (actionsgateway_v2_builder.go buildAGCDeploymentV2). Their presence would
		// mean the gateway was reconciled as proxied — the opposite of what this
		// suite exercises.
		envNames, err := utils.Run(exec.Command("kubectl", "get", "deployment", agcDeploy,
			"-n", tenantNS,
			"-o", `jsonpath={range .spec.template.spec.containers[?(@.name=="agc")].env[*]}{.name}{"\n"}{end}`))
		Expect(err).NotTo(HaveOccurred(), "read AGC env names on %s", agcDeploy)
		Expect(envNames).NotTo(ContainSubstring("HTTP_PROXY"),
			"AGC has HTTP_PROXY env — gateway was reconciled as proxied, not direct:\n%s", envNames)
		Expect(envNames).NotTo(ContainSubstring("HTTPS_PROXY"),
			"AGC has HTTPS_PROXY env — gateway was reconciled as proxied, not direct:\n%s", envNames)
		Expect(envNames).NotTo(ContainSubstring("PROXY_TLS_SECRET_NAME"),
			"AGC has PROXY_TLS_SECRET_NAME env — gateway was reconciled as proxied, not direct:\n%s", envNames)

		By("waiting for the workload NetworkPolicy to gain GitHub ipBlock peers (direct-egress allowlist)")
		// The GitHub CIDRs come from the shared IP-range cache; the workload NP is
		// patched once the first fetch lands. Their presence is the runtime signal
		// that the direct-egress reconcile authored the GitHub allowance the worker
		// needs (and that the positive probe below depends on).
		Eventually(func(g Gomega) {
			out, err := utils.Run(exec.Command("kubectl", "get", "networkpolicy", workloadNP,
				"-n", tenantNS, "-o", `jsonpath={.spec.egress[*].to[*].ipBlock.cidr}`))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).NotTo(BeEmpty(), "direct-egress workload NetworkPolicy has no GitHub ipBlock peers yet")
		}, 3*time.Minute, 5*time.Second).Should(Succeed())
	})

	It("E2E_V2_DirectEgress_ReachesGitHub: a direct-egress workload pod reaches GitHub without a proxy", func() {
		By("waiting for the workload NetworkPolicy GitHub ipBlock peers to be present")
		Eventually(func(g Gomega) {
			out, err := utils.Run(exec.Command("kubectl", "get", "networkpolicy", workloadNP,
				"-n", tenantNS, "-o", `jsonpath={.spec.egress[*].to[*].ipBlock.cidr}`))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).NotTo(BeEmpty(), "direct-egress workload NetworkPolicy has no GitHub ipBlock peers yet")
		}, 3*time.Minute, 5*time.Second).Should(Succeed())

		const curlPod = "direct-egress-github-curl"

		By("deploying a workload-labelled curl pod that reaches api.github.com DIRECTLY (no proxy)")
		// Carries the actions-gateway/component=workload label that the tenant
		// workload NetworkPolicy selects — the same label worker pods carry — so this
		// pod stands in for a proxy-less worker's network context. NO --proxy: egress
		// is direct, governed solely by the workload NP's DNS + GitHub-CIDR allowance.
		// --retry rides the CNI's NetworkPolicy programming latency (the GitHub-CIDR
		// allow rule can still be propagating on Calico just after the YAML appears).
		// A 200 or rate-limit 403 both prove the request reached GitHub; a real NP/DNS
		// regression fails the connection (curl 6/7/28) → pod Failed.
		manifest := fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %s
  namespace: %s
  labels:
    actions-gateway/component: workload
spec:
  restartPolicy: Never
  containers:
  - name: curl
    image: %s
    imagePullPolicy: IfNotPresent
    command: ["sh", "-c"]
    args:
    - |
      set -eu
      curl --silent --show-error \
           --max-time 30 \
           --retry 5 --retry-delay 2 --retry-max-time 90 --retry-all-errors \
           --output /tmp/body \
           --write-out 'HTTP_CODE=%%{http_code}\n' \
           https://api.github.com/zen
      echo "BODY_BYTES=$(wc -c < /tmp/body)"
`, curlPod, tenantNS, curlImage)

		Expect(utils.ApplyManifest(manifest)).To(Succeed())
		DeferCleanup(func() {
			_, _ = utils.Run(exec.Command("kubectl", "delete", "pod", curlPod,
				"-n", tenantNS, "--ignore-not-found", "--wait=false"))
		})

		By("waiting for the curl pod to terminate")
		var finalPhase string
		Eventually(func(g Gomega) {
			out, err := utils.Run(exec.Command("kubectl", "get", "pod", curlPod,
				"-n", tenantNS, "-o", "jsonpath={.status.phase}"))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(Or(Equal("Succeeded"), Equal("Failed")), "curl pod still in phase %q", out)
			finalPhase = out
		}, 3*time.Minute, 3*time.Second).Should(Succeed())

		logs, logsErr := utils.Run(exec.Command("kubectl", "logs", curlPod, "-n", tenantNS))
		Expect(logsErr).NotTo(HaveOccurred(), "fetch curl pod logs")
		Expect(finalPhase).To(Equal("Succeeded"),
			"direct (proxy-less) egress to api.github.com did not succeed; logs:\n%s", logs)
		Expect(logs).To(MatchRegexp(`HTTP_CODE=(200|403)`),
			"expected HTTP 200 (or rate-limit 403) from api.github.com via direct egress; logs:\n%s", logs)
		Expect(logs).To(MatchRegexp(`BODY_BYTES=([1-9][0-9]*)`),
			"expected a non-empty response body from api.github.com via direct egress; logs:\n%s", logs)
	})

	It("E2E_V2_DirectEgress_NonGitHubBlocked: a direct-egress workload pod cannot reach a non-GitHub destination", func() {
		if !egressEnforcingCNI() {
			Skip("cluster CNI does not enforce NetworkPolicy egress (kindnet); the non-GitHub block " +
				"can only be proven on Calico — recreate with `make e2e-cluster KIND_CNI=calico` (Q7b/Q119)")
		}

		// The probe target is the fakegithub Service in e2e-infra: a real, reachable
		// in-cluster pod that is NOT a GitHub CIDR and NOT DNS, so the direct-egress
		// workload NetworkPolicy (DNS + GitHub CIDRs + proxy:8080 only) does not
		// authorise it. An unlabelled control pod proves the destination is up, so a
		// connect failure from the labelled pod is attributable to NP enforcement,
		// not a dead backend.
		fakegithubURL := fmt.Sprintf("http://%s.%s.svc.cluster.local:%s/",
			fakegithubServiceName, infraNamespace, fakegithubServicePort)

		By("control: an unlabelled pod in the tenant namespace can reach fakegithub (destination is up)")
		logs := runEgressProbe(tenantNS, "direct-egress-control", false, fakegithubURL)
		Expect(logs).To(MatchRegexp(`CURL_RC=0(\s|$)`),
			"control pod could not reach fakegithub — destination down or probe broken, negative below would be meaningless; logs:\n%s", logs)

		By("negative: a workload-labelled (direct-egress) pod cannot reach the non-GitHub fakegithub destination")
		logs = runEgressProbe(tenantNS, "direct-egress-blocked", true, fakegithubURL)
		Expect(logs).To(MatchRegexp(`CURL_RC=(7|28)(\s|$)`),
			"direct-egress workload pod was NOT blocked from a non-GitHub destination (expected connect refused/timeout); "+
				"the default-deny egress NetworkPolicy is not enforcing the GitHub-only allowlist; logs:\n%s", logs)
		Expect(logs).To(ContainSubstring("HTTP_CODE=000"),
			"direct-egress workload pod completed an HTTP exchange with a non-GitHub destination — NP did not deny it; logs:\n%s", logs)
	})
})

// directEgressManifest renders the proxy-less v2 object set: ONE ActionsGateway
// with no defaultProxyRef (⇒ direct egress, §H.10), one RunnerTemplate, and one
// RunnerSet with no proxyRef (⇒ inherits the gateway's direct mode). workerImage
// is a placeholder — the suite probes the workload NetworkPolicy via dedicated
// curl pods, not a runnable worker.
func directEgressManifest(ns, secretName, workerImage string) string {
	return fmt.Sprintf(`apiVersion: actions-gateway.com/v2alpha1
kind: ActionsGateway
metadata:
  name: direct
  namespace: %[1]s
spec:
  githubURL: https://github.com/example-org-direct
  githubAppRef:
    name: %[2]s
  logLevel: debug
---
apiVersion: actions-gateway.com/v2alpha1
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
apiVersion: actions-gateway.com/v2alpha1
kind: RunnerSet
metadata:
  name: set-direct
  namespace: %[1]s
spec:
  gatewayRef:
    name: direct
  templateRef:
    name: tmpl
  maxListeners: 2
  runnerLabels: ["e2e-direct"]
`, ns, secretName, workerImage)
}
