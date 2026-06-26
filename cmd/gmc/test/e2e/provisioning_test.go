//go:build e2e
// +build e2e

package e2e

import (
	"fmt"
	"os/exec"
	"regexp"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/actions-gateway/github-actions-gateway/gmc/test/utils"
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
		utils.WaitForDeploymentReady(tenantNS, proxyName, 3*time.Minute)
	})

	It("E2E_GMC_ProvisionsAGCDeployment: AGC Deployment reaches ready", func() {
		By("waiting for actions-gateway-controller Deployment to have ready replicas")
		utils.WaitForDeploymentReady(tenantNS, agcName, 3*time.Minute)
	})

	It("E2E_GMC_ReadyConditionTrue: ActionsGateway Ready condition becomes True", func() {
		By("checking ActionsGateway Ready condition")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "actionsgateways.actions-gateway.github.com", agName,
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
		Expect(utils.ResourceExists("networkpolicy", tenantNS, proxyName)).To(BeTrue())
		By("verifying workload NetworkPolicy exists")
		Expect(utils.ResourceExists("networkpolicy", tenantNS, workloadName)).To(BeTrue())
	})

	It("E2E_GMC_ServiceAccountAndRBACCreated: ServiceAccount and RoleBinding are present", func() {
		By("checking ServiceAccount actions-gateway-controller")
		Expect(utils.ResourceExists("serviceaccount", tenantNS, agcName)).To(BeTrue())
		By("checking RoleBinding actions-gateway-controller")
		Expect(utils.ResourceExists("rolebinding", tenantNS, agcName)).To(BeTrue())
	})

	It("E2E_GMC_ProxyServiceCreated: proxy Service is present", func() {
		By("verifying Service actions-gateway-proxy exists")
		Expect(utils.ResourceExists("service", tenantNS, proxyName)).To(BeTrue())
	})

	It("E2E_GMC_HPAAndPDBCreated: HPA and PDB are present", func() {
		By("checking HPA actions-gateway-proxy")
		Expect(utils.ResourceExists("horizontalpodautoscaler", tenantNS, proxyName)).To(BeTrue())
		By("checking PDB actions-gateway-proxy")
		Expect(utils.ResourceExists("poddisruptionbudget", tenantNS, proxyName)).To(BeTrue())
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
		cmd := exec.Command("kubectl", "patch", "actionsgateways.actions-gateway.github.com", agName,
			"-n", tenantNS,
			"--type=merge",
			"-p", `{"spec":{"proxy":{"minReplicas":2}}}`,
		)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("waiting for HPA minReplicas to reflect the change")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "hpa", proxyName,
				"-n", tenantNS,
				"-o", "jsonpath={.spec.minReplicas}",
			)
			out, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(Equal("2"), fmt.Sprintf("HPA minReplicas not updated yet: %q", out))
		}, 2*time.Minute, 2*time.Second).Should(Succeed())
	})

	// E2E_GMC_TenantProvisioning_ProxyConnectWorks is the keystone Tier-A test
	// (see docs/design/07-test-plan.md §7.3). It runs a workload-labeled curl pod
	// that issues an HTTPS CONNECT through the per-tenant proxy to a real
	// GitHub endpoint, exercising in one shot: kindnet workload-NP egress to
	// the proxy pods, the proxy's HTTPS+CONNECT path, the proxy egress NP's
	// IP-range allowlist (populated by the IPRangeReconciler at startup), and
	// the proxy TLS cert+SAN chain. This is the test that would have caught
	// 4 of the 5 PR #59 bugs at local-iteration speed.
	It("E2E_GMC_TenantProvisioning_ProxyConnectWorks: curl through proxy reaches GitHub", func() {
		By("waiting for proxy NetworkPolicy to be populated with GitHub IP ranges")
		// The IPRangeReconciler refreshes the cache on Manager start, which both
		// seeds the proxy NP's ipBlock allowlist at creation and patches existing
		// NPs. Until that first fetch lands the NP only permits DNS — CONNECT to
		// api.github.com would silently drop. The initial fetch is retried on a
		// capped backoff (Q61 fix) so a transient outage self-heals in seconds
		// rather than waiting the 24h refresh interval. Wait for at least one
		// ipBlock egress peer to appear.
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "networkpolicy", proxyName,
				"-n", tenantNS,
				"-o", `jsonpath={.spec.egress[*].to[*].ipBlock.cidr}`,
			)
			out, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).NotTo(BeEmpty(), "proxy NetworkPolicy has no GitHub ipBlock peers yet")
		}, 3*time.Minute, 5*time.Second).Should(Succeed())

		const curlPodName = "proxy-connect-curl"
		proxyURL := fmt.Sprintf("https://%s.%s.svc.cluster.local:8080", proxyName, tenantNS)

		By("deploying a workload-labeled curl pod that CONNECTs through the proxy")
		// Labels:
		//   actions-gateway/component=workload — matches the workload NP, so
		//     kindnet permits egress to the proxy pods on port 8080.
		// Volume:
		//   The actions-gateway-proxy-tls Secret holds the self-signed leaf cert
		//   served by the proxy. We mount only tls.crt (not tls.key) and pass it
		//   to curl via --proxy-cacert so the CONNECT handshake to the proxy is
		//   TLS-verified end-to-end.
		// 403-tolerance (Q160):
		//   We deliberately do NOT pass curl --fail-with-body. The property under
		//   test is "egress traversed the proxy and reached GitHub", not "GitHub
		//   authorized the request". api.github.com rate-limits unauthenticated
		//   requests from the shared CI runner IP with HTTP 403 (observed PR #336
		//   run 27889017317) — a transient external-reputation flake unrelated to
		//   the proxy path. Without --fail-with-body, an HTTP 403 (or 200) is a
		//   *successful* curl transfer (exit 0): the body downloaded only because
		//   the CONNECT tunnel through the proxy was established and GitHub
		//   answered. So a 403 still proves egress, and the pod ends Succeeded. A
		//   real proxy/NP/TLS regression fails *tunnel establishment* (curl exits
		//   56 tunnel/recv, 60 wrong SAN, 28 NP-drop timeout) → pod Failed → test
		//   fails; the egress assertion is preserved, not weakened. The HTTP code
		//   itself is asserted as 200-or-403 below, so a persistent non-{200,403}
		//   response (e.g. a stuck 5xx after retries) still fails the test.
		// Retry (Q139):
		//   This CONNECT can fail transiently in two distinct ways, and the retry
		//   must cover BOTH:
		//   (a) Upstream HTTP 5xx through an already-established tunnel. The tunnel
		//       terminates at the *live* api.github.com, whose edge occasionally
		//       returns a transient 504 — observed on main run 27661005022 (fast
		//       ~8s fail, HTTP_CODE=504), corroborated in the same run by the GMC
		//       IPRangeReconciler's independent direct /meta fetch also getting 504.
		//       HTTP 5xx is in curl's transient-retry set (408/429/500/502/503/504),
		//       independent of --fail, so plain --retry already retries this; the
		//       403 rate-limit is NOT transient and is intentionally not retried.
		//   (b) Upstream *dial* failure at CONNECT-establishment time. The proxy
		//       emits 502 (never 504; see cmd/proxy: TestProxy_DialFailure) only
		//       when its net.DialTimeout to api.github.com:443 fails — a transient
		//       TCP-level failure to GitHub, or a brief egress-dataplane window on
		//       the Calico lane before Felix programs the new ipBlock rules. curl
		//       then exits 56 ("CONNECT tunnel failed, response 502", HTTP_CODE=000)
		//       — observed on PR-307 run 27839987830 (e2e-calico lane).
		//   Plain --retry does NOT retry case (b): curl exit 56 (CURLE_RECV_ERROR)
		//   is not in curl's default transient-retry set, and the proxy's 502 is
		//   never surfaced as the transfer response code (it is the CONNECT code, so
		//   %{http_code} is 000), so the HTTP-5xx retry path never triggers. Verified
		//   empirically: with plain --retry curl makes exactly one CONNECT attempt
		//   against a 502-returning proxy; with --retry-all-errors it retries. We
		//   therefore add --retry-all-errors so case (b) self-heals too.
		//   Bug-catching power is preserved: a genuine *persistent* proxy/NP/TLS
		//   regression (wrong SAN → exit 60, proxy down / NP drop → exit 56/28)
		//   fails on every retry and still fails the test — --retry-all-errors only
		//   adds bounded retries, it cannot turn a deterministic failure green.
		//   --retry-max-time caps total retrying so a real persistent failure still
		//   terminates well inside the 2-minute pod-phase wait.
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
           --retry 5 --retry-delay 2 --retry-max-time 60 --retry-all-errors \
           --proxy %s \
           --proxy-cacert /etc/proxy-ca/tls.crt \
           --output /tmp/body \
           --write-out 'HTTP_CODE=%%{http_code}\n' \
           https://api.github.com/zen
      echo "BODY_BYTES=$(wc -c < /tmp/body)"
    volumeMounts:
    - name: proxy-ca
      mountPath: /etc/proxy-ca
      readOnly: true
  volumes:
  - name: proxy-ca
    secret:
      secretName: actions-gateway-proxy-tls
      items:
      - key: tls.crt
        path: tls.crt
`, curlPodName, tenantNS, curlImage, proxyURL)

		Expect(utils.ApplyManifest(manifest)).To(Succeed())
		DeferCleanup(func() {
			cmd := exec.Command("kubectl", "delete", "pod", curlPodName,
				"-n", tenantNS, "--ignore-not-found", "--wait=false")
			_, _ = utils.Run(cmd)
		})

		By("waiting for the curl pod to terminate")
		var finalPhase string
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "pod", curlPodName,
				"-n", tenantNS,
				"-o", "jsonpath={.status.phase}",
			)
			out, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(Or(Equal("Succeeded"), Equal("Failed")),
				"curl pod still in phase %q", out)
			finalPhase = out
		}, 2*time.Minute, 3*time.Second).Should(Succeed())

		// Always dump logs — even on success — so the CI artifact has the
		// HTTP_CODE/BODY_BYTES line for visual confirmation.
		logsCmd := exec.Command("kubectl", "logs", curlPodName, "-n", tenantNS)
		logs, logsErr := utils.Run(logsCmd)
		Expect(logsErr).NotTo(HaveOccurred(), "fetch curl pod logs")

		// Pod Succeeded IS the proxy-egress assertion: curl exits 0 only after the
		// CONNECT tunnel is established through the proxy (proxy up, egress NP
		// permits the GitHub ipBlock, --proxy-cacert verifies the proxy TLS SAN)
		// AND GitHub returns an HTTP response. A real proxy/NP/TLS regression fails
		// tunnel establishment, curl exits non-zero, and the pod ends Failed here.
		Expect(finalPhase).To(Equal("Succeeded"),
			"curl pod ended in phase %s; logs:\n%s", finalPhase, logs)
		// Accept 200 or a rate-limit 403 (see "403-tolerance (Q160)" above): both
		// prove the request reached GitHub through the tunnel. The HTTP status code
		// is GitHub's authorization decision, not the egress property under test.
		Expect(logs).To(MatchRegexp(`HTTP_CODE=(200|403)`),
			"expected HTTP 200 (or rate-limit 403) from api.github.com via proxy; logs:\n%s", logs)
		Expect(logs).To(MatchRegexp(`BODY_BYTES=([1-9][0-9]*)`),
			"expected non-empty response body from api.github.com via proxy; logs:\n%s", logs)
	})

	// E2E_GMC_TenantProvisioning_WorkloadNPSpec validates the *spec* of the
	// workload and AGC NetworkPolicies — the only invariant we can reliably
	// assert in kindnet CI. Two CI iterations of runtime negative-case specs
	// (`curl --noproxy '*' https://api.github.com` and the in-cluster
	// `curl http://fakegithub.e2e-infra:8080` substitute) both observed
	// successful HTTP exchanges despite the workload NP not authorising the
	// destination: kindnet's bundled `kube-network-policies` enforcer does
	// not reliably drop egress traffic in either the external-IP path or the
	// cross-namespace pod path under the policy shape the GMC emits. The
	// in-cluster runtime negatives — `WorkloadEgressBlockedToNonProxyPod` and
	// `WorkerCannotReachK8sAPI` below — therefore skip themselves unless the
	// cluster runs an egress-enforcing CNI (`make e2e-cluster KIND_CNI=calico`,
	// see Q7b).
	//
	// This spec instead asserts the NP YAML the GMC reconciles into the
	// tenant namespace matches the documented [network-architecture.md]
	// shape. Authoring regressions (PolicyTypes loses Egress; egress rule
	// drops the podSelector; an unintended `to: []` broadens scope) are
	// caught here, even though we cannot prove kindnet's enforcer honours
	// the policy at runtime in CI.
	It("E2E_GMC_TenantProvisioning_WorkloadNPSpec: workload and AGC NetworkPolicies have the expected egress shape", func() {
		// Dump each NP as YAML and assert on substrings. YAML is more robust
		// than jsonpath here: a previous iteration of this spec used
		// `{range .spec.egress[*]}…{end}` and CI returned only the first
		// egress rule's output, masking whether the proxy-podSelector rule
		// was missing or whether jsonpath was truncating. YAML sidesteps the
		// ambiguity — every rule appears verbatim.

		By("dumping the workload NetworkPolicy as YAML")
		workloadYAML, err := utils.Run(exec.Command("kubectl", "get", "networkpolicy", workloadName,
			"-n", tenantNS, "-o", "yaml"))
		Expect(err).NotTo(HaveOccurred(), "fetch workload NP YAML")

		// policyTypes must include Egress (otherwise the NP imposes no egress
		// restriction at all).
		Expect(workloadYAML).To(ContainSubstring("- Egress"),
			"workload NP missing Egress in policyTypes:\n%s", workloadYAML)

		// policyTypes must include Ingress with no ingress rules — default-deny
		// all inbound to worker pods (Q128). Worker pods run untrusted job code
		// and accept no inbound by design; without Ingress in policyTypes they
		// are default-allow, letting any pod connect to untrusted code.
		Expect(workloadYAML).To(ContainSubstring("- Ingress"),
			"workload NP missing Ingress in policyTypes (Q128: default-deny ingress to untrusted worker pods):\n%s", workloadYAML)
		Expect(workloadYAML).NotTo(MatchRegexp(`(?m)^\s*ingress:`),
			"workload NP must carry no ingress rules — an ingress: block means inbound was allowed to worker pods (Q128):\n%s", workloadYAML)

		// DNS egress rule: port 53 on both UDP and TCP. DNS is confined to
		// cluster DNS (kube-dns / CoreDNS in kube-system) plus the node-local
		// link-local block — not "any resolver" (Q105/Q136); the peer shape is
		// asserted below.
		Expect(workloadYAML).To(MatchRegexp(`(?s)port:\s*53\b.*protocol:\s*UDP`),
			"workload NP missing DNS UDP egress rule:\n%s", workloadYAML)
		Expect(workloadYAML).To(MatchRegexp(`(?s)port:\s*53\b.*protocol:\s*TCP`),
			"workload NP missing DNS TCP egress rule:\n%s", workloadYAML)

		// Proxy egress rule: port 8080 to pods matching app=actions-gateway-proxy.
		// A regression that dropped the podSelector and allowed 8080 to any
		// destination would defeat the per-tenant egress-IP guarantee; the
		// `MatchRegexp` here keeps the port and the podSelector tied together.
		Expect(workloadYAML).To(ContainSubstring("port: 8080"),
			"workload NP missing port-8080 egress rule:\n%s", workloadYAML)
		Expect(workloadYAML).To(MatchRegexp(`(?s)port:\s*8080.*podSelector:.*matchLabels:.*app:\s*actions-gateway-proxy`),
			"workload NP port-8080 egress rule missing podSelector app=actions-gateway-proxy (regression: rule broadened to any destination):\n%s", workloadYAML)

		// The workload NP must NOT contain egress to GitHub CIDRs — that is the
		// proxy NP's job. The only ipBlock permitted is the link-local block
		// 169.254.0.0/16 used for NodeLocal DNSCache DNS egress (Q136): it is
		// non-routable and node-scoped, so it cannot reach GitHub or an external
		// resolver and preserves the per-tenant egress-IP attribution (Q105).
		// Any other (routable) cidr — e.g. a GitHub CIDR leaking onto the
		// workload NP — is a regression.
		Expect(workloadYAML).To(ContainSubstring("cidr: 169.254.0.0/16"),
			"workload NP missing the node-local DNS link-local ipBlock 169.254.0.0/16 (Q136):\n%s", workloadYAML)
		for _, m := range regexp.MustCompile(`cidr:\s*(\S+)`).FindAllStringSubmatch(workloadYAML, -1) {
			Expect(m[1]).To(Equal("169.254.0.0/16"),
				"workload NP egress has an unexpected ipBlock cidr %q — only the node-local DNS link-local block is allowed; GitHub CIDRs belong on the proxy NP (Q136):\n%s", m[1], workloadYAML)
		}

		By("dumping the AGC NetworkPolicy as YAML")
		agcYAML, err := utils.Run(exec.Command("kubectl", "get", "networkpolicy", agcName,
			"-n", tenantNS, "-o", "yaml"))
		Expect(err).NotTo(HaveOccurred(), "fetch AGC NP YAML")

		// AGC NP must select app=actions-gateway-controller pods only — worker
		// pods (workload-labelled, no AGC label) must not be selected here.
		Expect(agcYAML).To(MatchRegexp(`(?s)podSelector:.*matchLabels:.*app:\s*actions-gateway-controller`),
			"AGC NP must select pods labelled app=actions-gateway-controller:\n%s", agcYAML)

		// AGC NP egress must include both 443 and 6443. See
		// docs/development/networkpolicy-port-matching.md: kind exposes the
		// apiserver on 6443 via Service port-translation, so a 443-only rule
		// silently drops k8s API access there.
		Expect(agcYAML).To(ContainSubstring("port: 443"),
			"AGC NP missing apiserver port 443:\n%s", agcYAML)
		Expect(agcYAML).To(ContainSubstring("port: 6443"),
			"AGC NP missing apiserver port 6443 (kind apiserver Service port-translates to 6443; a 443-only rule drops k8s API):\n%s", agcYAML)
	})

	// E2E_GMC_TenantProvisioning_WorkloadEgressBlockedToNonProxyPod is the
	// runtime negative for the workload NP's egress allowlist: a
	// workload-labelled pod must NOT be able to open a connection to an
	// in-cluster destination other than the proxy pods. The probe target is
	// the fakegithub Service in e2e-infra — a real, reachable pod that the
	// workload NP does not authorise (its only non-DNS egress rule is
	// port 8080 *to pods labelled app=actions-gateway-proxy in the tenant
	// namespace*). An unlabelled control pod proves the destination is up, so
	// a connect failure from the labelled pod is attributable to NP
	// enforcement, not a dead backend.
	//
	// Requires an egress-enforcing CNI (Calico/Cilium); self-skips on kindnet,
	// whose kube-network-policies enforcer demonstrably does not drop this
	// traffic (see the WorkloadNPSpec comment above and Q7b).
	It("E2E_GMC_TenantProvisioning_WorkloadEgressBlockedToNonProxyPod: workload pod cannot reach a non-proxy pod", func() {
		if !egressEnforcingCNI() {
			Skip("cluster CNI does not enforce NetworkPolicy egress (kindnet); recreate with `make e2e-cluster KIND_CNI=calico` (Q7b)")
		}

		fakegithubURL := fmt.Sprintf("http://%s.%s.svc.cluster.local:%s/",
			fakegithubServiceName, infraNamespace, fakegithubServicePort)

		// Don't rely on the earlier specs in this Ordered container having
		// run (a focused run starts at BeforeAll): without the workload NP
		// reconciled, the labelled pod's traffic would be allowed and the
		// negative would fail spuriously.
		By("waiting for the workload NetworkPolicy to exist")
		Eventually(func() bool {
			return utils.ResourceExists("networkpolicy", tenantNS, workloadName)
		}, 2*time.Minute, 2*time.Second).Should(BeTrue(), "workload NetworkPolicy not reconciled")

		By("control: an unlabelled pod in the tenant namespace can reach fakegithub")
		logs := runEgressProbe(tenantNS, "egress-control-fakegithub", false, fakegithubURL)
		Expect(logs).To(MatchRegexp(`CURL_RC=0(\s|$)`),
			"control pod could not reach fakegithub — destination down or probe broken, negative below would be meaningless; logs:\n%s", logs)

		By("negative: a workload-labelled pod cannot reach fakegithub")
		logs = runEgressProbe(tenantNS, "egress-blocked-fakegithub", true, fakegithubURL)
		Expect(logs).To(MatchRegexp(`CURL_RC=(7|28)(\s|$)`),
			"workload-labelled pod was NOT blocked from a non-proxy destination (expected connect refused/timeout); logs:\n%s", logs)
		Expect(logs).To(ContainSubstring("HTTP_CODE=000"),
			"workload-labelled pod completed an HTTP exchange with a non-proxy destination; logs:\n%s", logs)
	})

	// E2E_GMC_TenantProvisioning_WorkerCannotReachK8sAPI is the runtime
	// negative for worker→apiserver isolation: only AGC-labelled pods get the
	// additive AGC NP's 443/6443 egress, so a workload-labelled pod (the
	// label worker pods carry) must not be able to reach the kubernetes
	// Service. Note kube-proxy DNATs kubernetes.default:443 to the kind
	// node's 6443 before the CNI evaluates the packet — both ports are
	// outside the workload NP's allowlist, so the connection must drop
	// either way. Control pod as above.
	It("E2E_GMC_TenantProvisioning_WorkerCannotReachK8sAPI: workload pod cannot reach the kubernetes API", func() {
		if !egressEnforcingCNI() {
			Skip("cluster CNI does not enforce NetworkPolicy egress (kindnet); recreate with `make e2e-cluster KIND_CNI=calico` (Q7b)")
		}

		// --insecure: the probe asserts TCP/TLS reachability, not identity;
		// anonymous requests complete the handshake and get an HTTP status.
		apiURL := "--insecure https://kubernetes.default.svc.cluster.local:443/version"

		// See the equivalent wait in WorkloadEgressBlockedToNonProxyPod.
		By("waiting for the workload NetworkPolicy to exist")
		Eventually(func() bool {
			return utils.ResourceExists("networkpolicy", tenantNS, workloadName)
		}, 2*time.Minute, 2*time.Second).Should(BeTrue(), "workload NetworkPolicy not reconciled")

		By("control: an unlabelled pod in the tenant namespace can reach the kubernetes API")
		logs := runEgressProbe(tenantNS, "egress-control-k8sapi", false, apiURL)
		Expect(logs).To(MatchRegexp(`CURL_RC=0(\s|$)`),
			"control pod could not reach the kubernetes API — probe broken, negative below would be meaningless; logs:\n%s", logs)

		By("negative: a workload-labelled pod cannot reach the kubernetes API")
		logs = runEgressProbe(tenantNS, "egress-blocked-k8sapi", true, apiURL)
		Expect(logs).To(MatchRegexp(`CURL_RC=(7|28)(\s|$)`),
			"workload-labelled pod was NOT blocked from the kubernetes API (expected connect refused/timeout); logs:\n%s", logs)
		Expect(logs).To(ContainSubstring("HTTP_CODE=000"),
			"workload-labelled pod completed an HTTP exchange with the kubernetes API; logs:\n%s", logs)
	})
})

// egressEnforcingCNI reports whether the cluster runs a CNI known to enforce
// NetworkPolicy egress rules at runtime. kindnet (kind's default) bundles
// kube-network-policies, which two CI iterations showed does NOT drop egress
// for the negative cases (see the worker-egress-proxy plan); the runtime
// negative specs skip themselves unless Calico or Cilium is detected.
func egressEnforcingCNI() bool {
	return utils.ResourceExists("daemonset", "kube-system", "calico-node") || // manifest install
		utils.ResourceExists("daemonset", "calico-system", "calico-node") || // operator install
		utils.ResourceExists("daemonset", "kube-system", "cilium")
}

// runEgressProbe runs a one-shot curl pod in ns against curlArgs (the final
// curl arguments, typically just a URL) and returns its logs. The pod script
// always exits 0 and reports the outcome as CURL_RC=<n> and HTTP_CODE=<code>
// lines — callers assert on those, so a non-Succeeded phase means an
// infrastructure problem (image pull, scheduling), not a blocked connection.
// curl exit 6 (could not resolve host) is retried up to 5× to ride out
// transient CoreDNS readiness on a freshly scheduled pod; every other exit
// code (0, 7, 28) breaks immediately so the real signal is never masked.
// workloadLabeled selects whether the pod carries the
// actions-gateway/component=workload label that the tenant NetworkPolicies
// match on.
func runEgressProbe(ns, name string, workloadLabeled bool, curlArgs string) string {
	labels := "e2e-probe: control"
	if workloadLabeled {
		labels = "actions-gateway/component: workload"
	}
	manifest := fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %s
  namespace: %s
  labels:
    %s
spec:
  restartPolicy: Never
  containers:
  - name: curl
    image: %s
    imagePullPolicy: IfNotPresent
    command: ["sh", "-c"]
    args:
    - |
      set -u
      rc=0
      for attempt in 1 2 3 4 5; do
        rc=0
        curl --silent --show-error --output /dev/null \
             --connect-timeout 10 --max-time 20 \
             --write-out 'HTTP_CODE=%%{http_code}\n' \
             %s || rc=$?
        # curl 6 = could not resolve host: transient CoreDNS readiness on a
        # freshly scheduled pod. Retry only this; 0/7/28 are real signals —
        # surface them at once so assertions see the true outcome unmasked.
        if [ "$rc" -ne 6 ]; then break; fi
        sleep 2
      done
      echo "CURL_RC=${rc}"
`, name, ns, labels, curlImage, curlArgs)

	ExpectWithOffset(1, utils.ApplyManifest(manifest)).To(Succeed(), "apply egress probe pod %s/%s", ns, name)
	DeferCleanup(func() {
		cmd := exec.Command("kubectl", "delete", "pod", name,
			"-n", ns, "--ignore-not-found", "--wait=false")
		_, _ = utils.Run(cmd)
	})

	By("waiting for probe pod " + name + " to terminate")
	var finalPhase string
	EventuallyWithOffset(1, func(g Gomega) {
		cmd := exec.Command("kubectl", "get", "pod", name,
			"-n", ns,
			"-o", "jsonpath={.status.phase}",
		)
		out, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(out).To(Or(Equal("Succeeded"), Equal("Failed")),
			"probe pod still in phase %q", out)
		finalPhase = out
	}, 2*time.Minute, 3*time.Second).Should(Succeed())

	logs, logsErr := utils.Run(exec.Command("kubectl", "logs", name, "-n", ns))
	ExpectWithOffset(1, logsErr).NotTo(HaveOccurred(), "fetch probe pod logs")
	ExpectWithOffset(1, finalPhase).To(Equal("Succeeded"),
		"probe pod %s ended in phase %s (infrastructure problem — the script always exits 0); logs:\n%s", name, finalPhase, logs)
	return logs
}
