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
		secretName = "github-app-secret" //nolint:gosec // G101: the NAME of a Kubernetes Secret object, not a credential value.
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
		// Retry rather than one-shot: the ActionsGateway/RunnerTemplate/RunnerSet
		// validating-webhook POSTs can transiently time out under the parallel suite
		// even with the GMC 1/1 Running, and failurePolicy: Fail turns that blip into
		// a hard BeforeAll failure (Q391). A genuine denial still fails fast.
		Expect(utils.ApplyManifestWithWebhookRetry(directEgressManifest(tenantNS, secretName, agcImage))).To(Succeed())

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

	// NonGitHubBlocked is the fifth egress negative, so this container needs the
	// enforcer read on failure too (Q747, #1417).
	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			utils.DumpAGCSessionDiagnostics(tenantNS, agcDeploy, infraNamespace, fakegithubServiceName)
			utils.DumpCNIEnforcerState()
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

	It("E2E_V2_DirectEgress_Provisions: proxy-less gateway wires direct mode and a GitHub-CIDR workload NetworkPolicy", Label(realGitHubEgressLabel), func() {
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

	It("E2E_V2_DirectEgress_ReachesGitHub: a direct-egress workload pod reaches GitHub without a proxy", Label(realGitHubEgressLabel), func() {
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
		// The retry budget was widened (90s→150s, ceiling 3m→4m) after this spec
		// red-flaked alongside the two proxy-connect egress specs on e2e-calico
		// (Q291): three 30s attempts exactly exhausted the old 90s budget before
		// Felix finished programming the ipBlock.
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
           --retry 8 --retry-delay 2 --retry-max-time 150 --retry-all-errors \
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
		}, 4*time.Minute, 3*time.Second).Should(Succeed())

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

	// Q716. The runtime half of TestBuildNetworkPolicy_DeniesCloudMetadataServer
	// (cmd/gmc/internal/controller/metadata_egress_test.go), which pins the same
	// property at the authoring level on every PR.
	//
	// Nothing denies the metadata address by name — NetworkPolicy is allowlist-only.
	// What keeps it unreachable is that the sole rule admitting its address, the
	// link-local ipBlock 169.254.0.0/16 that NodeLocal DNSCache needs (Q136), is
	// scoped to port 53. This spec proves a policy-enforcing CNI actually honours
	// that port scoping on an ipBlock peer, which the authoring test cannot say.
	//
	// Kata is not the control here and asserting Kata isolation would test the wrong
	// layer: it bounds the kernel, not the pod network. Q226 measured the address
	// still reachable from inside a Kata micro-VM on GKE with the token endpoint
	// returning HTTP 200 (docs/operations/kata-dind-workloads.md).
	It("E2E_V2_DirectEgress_MetadataServerBlocked: a workload pod cannot reach the cloud metadata server", func() {
		if !egressEnforcingCNI() {
			Skip("cluster CNI does not enforce NetworkPolicy egress (kindnet); the metadata-server block " +
				"can only be proven on Calico — recreate with `make e2e-cluster KIND_CNI=calico` (Q7b/Q119)")
		}

		deployMetadataStandin()

		// Both URLs are the SAME address; only the port differs. That is the whole
		// point — the assertion is about the port scoping of one ipBlock peer, so
		// varying anything else would weaken it.
		const metadataHTTP = "http://169.254.169.254:80/"
		const metadataDNSPort = "http://169.254.169.254:53/"

		By("control: an unlabelled pod reaches the metadata stand-in on :80 (destination is up)")
		logs := runEgressProbe(tenantNS, "metadata-control", false, metadataHTTP)
		Expect(logs).To(MatchRegexp(`CURL_RC=0(\s|$)`),
			"control pod could not reach the metadata stand-in on :80 — the stand-in DaemonSet is not serving, "+
				"so the negative below would pass whatever the NetworkPolicy does; logs:\n%s", logs)

		// The anti-vacuity control, and the reason this spec cannot pass because
		// nothing ran. A blocked :80 means the port scoping held ONLY if link-local
		// is reachable from this same pod's network context at all; if the whole
		// block were unroutable, or the probe pod were broken, this leg fails too
		// and the negative is correctly disqualified rather than silently trusted.
		By("anti-vacuity: a workload-labelled pod DOES reach the same link-local address on :53")
		logs = runEgressProbe(tenantNS, "metadata-linklocal-dns", true, metadataDNSPort)
		Expect(logs).To(MatchRegexp(`CURL_RC=0(\s|$)`),
			"workload pod could not reach link-local on :53 — the NodeLocal DNSCache allowance (Q136) is not in "+
				"effect, so a blocked :80 below would prove nothing about port scoping; logs:\n%s", logs)

		By("negative: a workload-labelled pod cannot reach the metadata server on :80")
		logs = runEgressProbe(tenantNS, "metadata-blocked", true, metadataHTTP)
		Expect(logs).To(MatchRegexp(`CURL_RC=(7|28)(\s|$)`),
			"a worker pod reached the cloud metadata server at 169.254.169.254:80 — it can read the node's cloud "+
				"credentials. The link-local DNS allowance (Q136) has leaked beyond port 53; logs:\n%s", logs)
		Expect(logs).To(ContainSubstring("HTTP_CODE=000"),
			"workload pod completed an HTTP exchange with the cloud metadata server — NP did not deny it; logs:\n%s", logs)
	})
})

// deployMetadataStandin stands a cloud metadata server up at 169.254.169.254 on
// every node, so the negative above has a live destination to be blocked FROM.
//
// Without it the spec is vacuous: a kind cluster has no metadata server, so a
// probe of that address fails for every pod whatever the NetworkPolicy says.
//
// hostNetwork because the address has to exist in the NODE's network namespace,
// which is where a pod's link-local traffic lands — the same shape NodeLocal
// DNSCache uses for 169.254.20.10. A DaemonSet rather than a Deployment because
// link-local is node-scoped: a probe pod reaches only its OWN node's copy.
//
// The listener is fakegithub, whose ADDR/CONTROL_ADDR env vars bind it wherever
// we ask. It is used here purely as an HTTP server that is already built and
// side-loaded onto the kind nodes; nothing in this spec touches its GitHub
// behaviour, and any status it answers with proves the point equally, because
// the assertions read curl's exit code rather than the HTTP status.
//
// An earlier revision served with busybox httpd from the curl image and
// crash-looped every pod: Alpine ships httpd in busybox-extras, not in the base
// busybox that curlimages/curl carries. Serving from an image this repo builds
// removes that class of guess. The one applet still needed is `ip`, in the init
// container, and its failure is now attributable to a single container rather
// than being one of two FATAL paths in a combined script.
func deployMetadataStandin() {
	const dsName = "metadata-standin"

	By("deploying the cloud metadata stand-in DaemonSet in " + infraNamespace)
	manifest := fmt.Sprintf(`apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  selector:
    matchLabels:
      app: %[1]s
  template:
    metadata:
      labels:
        app: %[1]s
    spec:
      hostNetwork: true
      # Land on control-plane nodes too: the probe pods are not confined to
      # workers, and a node without the stand-in yields an unreachable control.
      tolerations:
      - operator: Exists
      initContainers:
      # Sole job is the address, so a failure here is unambiguous. Idempotent
      # because the e2e cluster is reused across runs: an address left by a
      # previous run must not fail the pod.
      - name: add-metadata-address
        image: %[3]s
        imagePullPolicy: IfNotPresent
        securityContext:
          privileged: true
          runAsUser: 0
        command: ["sh", "-c"]
        args:
        - |
          set -eu
          if ip addr show dev lo | grep -q '169[.]254[.]169[.]254'; then
            echo "169.254.169.254 already present on lo"
          else
            ip addr add 169.254.169.254/32 dev lo
          fi
          ip addr show dev lo
      containers:
      # Two listeners, same address, different ports. :80 is the real metadata
      # port. :53 is the port the link-local DNS rule admits, and serving it is
      # what lets the spec prove the block is the PORT scoping rather than the
      # address being unreachable. runAsUser 0 because both are privileged
      # ports; CONTROL_ADDR differs so the two containers do not collide in the
      # shared host network namespace.
      - name: meta-http
        image: %[4]s
        imagePullPolicy: IfNotPresent
        securityContext:
          runAsUser: 0
        env:
        - name: ADDR
          value: "169.254.169.254:80"
        - name: CONTROL_ADDR
          value: "169.254.169.254:9090"
        # A tcpSocket probe is executed by the kubelet, so readiness needs no
        # shell or curl inside this distroless image. Readiness here means the
        # port is actually accepting, which is what the caller's wait needs it
        # to mean — the pod's containers would otherwise report Ready before
        # either listener bound, and the control probe would race them.
        readinessProbe:
          tcpSocket:
            host: 169.254.169.254
            port: 80
          initialDelaySeconds: 2
          periodSeconds: 3
          failureThreshold: 30
      - name: meta-dns-port
        image: %[4]s
        imagePullPolicy: IfNotPresent
        securityContext:
          runAsUser: 0
        env:
        - name: ADDR
          value: "169.254.169.254:53"
        - name: CONTROL_ADDR
          value: "169.254.169.254:9091"
        readinessProbe:
          tcpSocket:
            host: 169.254.169.254
            port: 53
          initialDelaySeconds: 2
          periodSeconds: 3
          failureThreshold: 30
`, dsName, infraNamespace, curlImage, fakegithubImage)

	Expect(utils.ApplyManifest(manifest)).To(Succeed(), "apply metadata stand-in DaemonSet")
	DeferCleanup(func() {
		if CurrentSpecReport().Failed() {
			dumpMetadataStandin(dsName)
		}
		_, _ = utils.Run(exec.Command("kubectl", "delete", "daemonset", dsName,
			"-n", infraNamespace, "--ignore-not-found", "--wait=false"))
	})

	By("waiting for the metadata stand-in to be ready on every node")
	Eventually(func(g Gomega) {
		out, err := utils.Run(exec.Command("kubectl", "get", "daemonset", dsName,
			"-n", infraNamespace,
			"-o", "jsonpath={.status.desiredNumberScheduled}/{.status.numberReady}"))
		g.Expect(err).NotTo(HaveOccurred())
		parts := strings.SplitN(out, "/", 2)
		g.Expect(parts).To(HaveLen(2), "unexpected DaemonSet status %q", out)
		g.Expect(parts[0]).NotTo(Equal("0"), "metadata stand-in DaemonSet scheduled onto no nodes")
		g.Expect(parts[1]).To(Equal(parts[0]), "metadata stand-in not ready on every node (%s)", out)
	}, 3*time.Minute, 3*time.Second).Should(Succeed())
}

// dumpMetadataStandin writes the stand-in's state to the Ginkgo output on
// failure. Added because the first CI run of this spec reported only
// "not ready on every node (3/0)": the pods were crash-looping and nothing
// captured why, which cost a full 15-minute lane run to diagnose from events
// alone. Best-effort, like DumpCNIEnforcerState — call it only on a failure path.
func dumpMetadataStandin(dsName string) {
	_, _ = fmt.Fprintf(GinkgoWriter, "\n===== metadata stand-in state =====\n")
	for _, args := range [][]string{
		{"get", "pods", "-n", infraNamespace, "-l", "app=" + dsName, "-o", "wide"},
		{"describe", "daemonset", dsName, "-n", infraNamespace},
		{"describe", "pods", "-n", infraNamespace, "-l", "app=" + dsName},
	} {
		out, err := utils.Run(exec.Command("kubectl", args...))
		_, _ = fmt.Fprintf(GinkgoWriter, "--- kubectl %s ---\n%s\n(err: %v)\n",
			strings.Join(args, " "), out, err)
	}
	// Per-container logs, current and previous: a crash-looped container's fatal
	// line survives only in --previous.
	pods, err := utils.Run(exec.Command("kubectl", "get", "pods", "-n", infraNamespace,
		"-l", "app="+dsName, "-o", "name"))
	if err != nil {
		return
	}
	for _, pod := range strings.Fields(pods) {
		for _, container := range []string{"add-metadata-address", "meta-http", "meta-dns-port"} {
			for _, prev := range []bool{false, true} {
				args := []string{"logs", pod, "-n", infraNamespace, "-c", container, "--tail=40"}
				if prev {
					args = append(args, "--previous")
				}
				out, logErr := utils.Run(exec.Command("kubectl", args...))
				_, _ = fmt.Fprintf(GinkgoWriter, "--- %s/%s (previous=%v) ---\n%s\n(err: %v)\n",
					pod, container, prev, out, logErr)
			}
		}
	}
	_, _ = fmt.Fprintf(GinkgoWriter, "===== end metadata stand-in state =====\n\n")
}

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
  credentials:
    type: GitHubApp
    githubApp:
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
  acquisitionProtocol: Classic   # pinned to the classic tier; the default is ScaleSet (Q264 P5)
  runnerLabels: ["e2e-direct"]
`, ns, secretName, workerImage)
}
