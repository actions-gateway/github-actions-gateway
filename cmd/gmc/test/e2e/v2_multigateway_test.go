//go:build e2e
// +build e2e

package e2e

import (
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/actions-gateway/github-actions-gateway/gmc/test/utils"
)

// E2E_V2_MultiGateway proves the M3b headline (Q167) live on kind: two v2
// ActionsGateways in ONE namespace each provision their own AGC control plane
// under disjoint per-gateway names, and each AGC reconciles only its own gateway's
// RunnerSet — the field-selector scoping isolation boundary. It also proves v2
// parity: a queued job yields a worker pod (the job → pod link), and a
// workload-labeled pod reaches GitHub through the v2 EgressProxy (the pod → proxy
// → GitHub link).
//
// The decisive isolation assertion is the worker pod's ServiceAccount: gateway
// "alpha"'s AGC carries WORKER_SERVICE_ACCOUNT=alpha-worker and is field-scoped to
// RunnerSets targeting "alpha", so a worker pod for set-alpha running as
// alpha-worker proves alpha's AGC (not beta's) handled set-alpha's job — N AGCs in
// one namespace do not fight over the same RunnerSets.
var _ = Describe("E2E_V2_MultiGateway", Ordered, func() {
	const (
		tenantNS   = "tenant-v2-multigw"
		secretName = "github-app-secret"
		proxyRef   = "shared"
		// Per-gateway derived names (§H.16 #1).
		alphaAGC = "alpha-agc"
		betaAGC  = "beta-agc"
		// EgressProxy "shared" derives a "shared-proxy" Deployment/Service.
		proxyDeploy = proxyRef + "-proxy"
	)

	BeforeAll(func() {
		utils.CreateNamespace(tenantNS, map[string]string{
			// Mark the namespace a v2-managed tenant so the GMC's dual-reading VAPs
			// admit its provisioning; the v1 marker CreateNamespace already stamps is
			// also honored, but v2 tenants carry the v2 marker.
			"actions-gateway.com/tenant": "managed",
		})
		utils.CreateGitHubAppSecret(tenantNS, secretName, 12345, 67890, testRSAKeyPEM)

		By("applying the v2 object set: one EgressProxy, two ActionsGateways, one template, two RunnerSets")
		Expect(utils.ApplyManifest(v2MultiGatewayManifest(tenantNS, secretName, agcImage))).To(Succeed())

		By("granting workload pods egress to fakegithub in e2e-infra (both gateways' AGCs)")
		utils.ApplyFakegithubEgressNetworkPolicy(tenantNS)

		By("waiting for both per-gateway AGC Deployments to become ready")
		utils.WaitForDeploymentReady(tenantNS, alphaAGC, 4*time.Minute)
		utils.WaitForDeploymentReady(tenantNS, betaAGC, 4*time.Minute)

		By("waiting for the shared EgressProxy pool to become ready")
		utils.WaitForDeploymentReady(tenantNS, proxyDeploy, 4*time.Minute)

		By("starting persistent port-forward to fakegithub control API")
		fakegithubLocalPort = fmt.Sprintf("%d", 19090+GinkgoParallelProcess())
		fakegithubPFCmd = exec.Command("kubectl", "port-forward",
			"-n", infraNamespace,
			"service/"+fakegithubServiceName,
			fakegithubLocalPort+":9090",
		)
		Expect(fakegithubPFCmd.Start()).To(Succeed())
		Eventually(func() error {
			resp, err := http.Get("http://localhost:" + fakegithubLocalPort + "/control/sessions")
			if err != nil {
				return err
			}
			resp.Body.Close()
			return nil
		}, 15*time.Second, 500*time.Millisecond).Should(Succeed())
	})

	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			// Both AGCs share the fakegithub/infra; dump each gateway's AGC state.
			utils.DumpAGCSessionDiagnostics(tenantNS, alphaAGC, infraNamespace, fakegithubServiceName)
			utils.DumpAGCSessionDiagnostics(tenantNS, betaAGC, infraNamespace, fakegithubServiceName)
		}
	})

	AfterAll(func() {
		if fakegithubPFCmd != nil && fakegithubPFCmd.Process != nil {
			_ = fakegithubPFCmd.Process.Kill()
		}
		// Cluster-scoped ClusterRoleBindings are not namespace-GC'd; delete them so
		// reruns on a persisted cluster start clean. The reconciler also deletes them
		// on gateway deletion, but the namespace teardown races that.
		for _, gw := range []string{"alpha", "beta"} {
			_, _ = utils.Run(exec.Command("kubectl", "delete", "clusterrolebinding",
				"agc-clusterrunnertemplate-reader."+tenantNS+"."+gw, "--ignore-not-found"))
		}
		utils.DeleteNamespace(tenantNS)
	})

	SetDefaultEventuallyTimeout(3 * time.Minute)
	SetDefaultEventuallyPollingInterval(2 * time.Second)

	It("E2E_V2_TwoGatewaysCoexist: two gateways provision disjoint control planes in one namespace", func() {
		By("verifying both per-gateway AGC Deployments exist with distinct names")
		Expect(utils.ResourceExists("deployment", tenantNS, alphaAGC)).To(BeTrue())
		Expect(utils.ResourceExists("deployment", tenantNS, betaAGC)).To(BeTrue())

		By("verifying each AGC is scoped to its own gateway via GATEWAY_NAME")
		Expect(agcGatewayNameEnv(tenantNS, alphaAGC)).To(Equal("alpha"))
		Expect(agcGatewayNameEnv(tenantNS, betaAGC)).To(Equal("beta"))

		By("verifying each gateway has its own ClusterRunnerTemplate ClusterRoleBinding")
		for _, gw := range []string{"alpha", "beta"} {
			Expect(utils.ResourceExists("clusterrolebinding", "", "agc-clusterrunnertemplate-reader."+tenantNS+"."+gw)).
				To(BeTrue(), "per-gateway ClusterRoleBinding for %s", gw)
		}
	})

	It("E2E_V2_PerGatewayJobIsolation: each gateway's AGC services only its own RunnerSet's jobs", func() {
		// alpha's job → a worker pod for set-alpha that runs as alpha-worker (the
		// per-gateway worker SA only alpha's AGC carries). That the pod exists AND
		// runs as alpha-worker proves alpha's scoped AGC handled it.
		assertGatewayJobYieldsScopedWorker(tenantNS, "set-alpha", "alpha-worker")
		assertGatewayJobYieldsScopedWorker(tenantNS, "set-beta", "beta-worker")
	})

	It("E2E_V2_ProxyConnectWorks: a workload pod reaches GitHub through the v2 EgressProxy", func() {
		By("waiting for the v2 EgressProxy NetworkPolicy to gain GitHub ipBlock peers")
		Eventually(func(g Gomega) {
			out, err := utils.Run(exec.Command("kubectl", "get", "networkpolicy", proxyDeploy,
				"-n", tenantNS, "-o", `jsonpath={.spec.egress[*].to[*].ipBlock.cidr}`))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).NotTo(BeEmpty(), "EgressProxy NetworkPolicy has no GitHub ipBlock peers yet")
		}, 3*time.Minute, 5*time.Second).Should(Succeed())

		const curlPod = "v2-proxy-connect-curl"
		proxyURL := fmt.Sprintf("https://%s.%s.svc.cluster.local:8080", proxyDeploy, tenantNS)

		By("deploying a workload-labeled curl pod that CONNECTs through the v2 proxy")
		// Mirrors the v1 keystone proxy-connect spec (provisioning_test.go): the
		// workload label admits egress to the proxy; --proxy-cacert pins the proxy's
		// self-signed leaf (the EgressProxy's <ep>-proxy-tls Secret). A 200 or
		// rate-limit 403 both prove the CONNECT tunnel reached GitHub; tunnel/TLS/NP
		// regressions fail establishment (curl 56/60/28) → pod Failed.
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
      secretName: %s-proxy-tls
      items:
      - key: tls.crt
        path: tls.crt
`, curlPod, tenantNS, curlImage, proxyURL, proxyRef)

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
		}, 2*time.Minute, 3*time.Second).Should(Succeed())

		logs, logsErr := utils.Run(exec.Command("kubectl", "logs", curlPod, "-n", tenantNS))
		Expect(logsErr).NotTo(HaveOccurred(), "fetch curl pod logs")
		Expect(finalPhase).To(Equal("Succeeded"),
			"curl through the v2 EgressProxy did not reach GitHub; logs:\n%s", logs)
		Expect(logs).To(MatchRegexp(`HTTP_CODE=(200|403)`),
			"expected HTTP 200 (or rate-limit 403) from api.github.com via the v2 proxy; logs:\n%s", logs)
	})
})

// agcGatewayNameEnv reads the GATEWAY_NAME env on a per-gateway AGC Deployment.
func agcGatewayNameEnv(ns, deploy string) string {
	out, err := utils.Run(exec.Command("kubectl", "get", "deployment", deploy,
		"-n", ns,
		"-o", `jsonpath={range .spec.template.spec.containers[?(@.name=="agc")].env[?(@.name=="GATEWAY_NAME")]}{.value}{end}`))
	Expect(err).NotTo(HaveOccurred(), "read GATEWAY_NAME on %s", deploy)
	return strings.TrimSpace(out)
}

// assertGatewayJobYieldsScopedWorker enqueues a job onto a live session owned by
// runnerSet and asserts a worker pod for that set appears running as wantSA — the
// per-gateway worker ServiceAccount that only the gateway's scoped AGC carries.
func assertGatewayJobYieldsScopedWorker(ns, runnerSet, wantSA string) {
	GinkgoHelper()
	fakegithubSvcURL := fmt.Sprintf("http://%s.%s.svc.cluster.local:%s",
		fakegithubServiceName, infraNamespace, fakegithubServicePort)

	By(fmt.Sprintf("picking a live session owned by %s", runnerSet))
	var session string
	Eventually(func(g Gomega) {
		sessions := fakegithubActiveSessionsForOwner(g, runnerSet+"-")
		g.Expect(sessions).NotTo(BeEmpty(), "no live session for RunnerSet %s", runnerSet)
		session = sessions[0]
	}, 3*time.Minute, 2*time.Second).Should(Succeed())

	By(fmt.Sprintf("enqueuing a job onto %s", session))
	fakegithubEnqueueJob(session, map[string]interface{}{
		"jobId":           "e2e-" + runnerSet,
		"run_service_url": fakegithubSvcURL,
	})

	By(fmt.Sprintf("waiting for a worker pod for %s running as %s", runnerSet, wantSA))
	Eventually(func(g Gomega) {
		out, err := utils.Run(exec.Command("kubectl", "get", "pods",
			"-n", ns,
			"-l", "actions-gateway.com/runner-set="+runnerSet,
			"-o", `jsonpath={range .items[*]}{.spec.serviceAccountName}{"\n"}{end}`))
		g.Expect(err).NotTo(HaveOccurred())
		sas := strings.Fields(out)
		g.Expect(sas).NotTo(BeEmpty(), "no worker pod for %s yet", runnerSet)
		for _, sa := range sas {
			g.Expect(sa).To(Equal(wantSA),
				"worker pod for %s ran as %q, want %q — wrong gateway's AGC handled it (scoping breach)", runnerSet, sa, wantSA)
		}
	}, 3*time.Minute, 2*time.Second).Should(Succeed())
}

// v2MultiGatewayManifest renders the v2 object set: a shared EgressProxy, two
// ActionsGateways (alpha, beta) each pointing at it, one RunnerTemplate, and two
// RunnerSets each bound to a different gateway. workerImage is a placeholder (the
// job-isolation spec asserts pod creation + ServiceAccount, not job completion).
func v2MultiGatewayManifest(ns, secretName, workerImage string) string {
	return fmt.Sprintf(`apiVersion: actions-gateway.com/v2alpha1
kind: EgressProxy
metadata:
  name: shared
  namespace: %[1]s
spec:
  minReplicas: 1
  maxReplicas: 2
---
apiVersion: actions-gateway.com/v2alpha1
kind: ActionsGateway
metadata:
  name: alpha
  namespace: %[1]s
spec:
  githubURL: https://github.com/example-org-alpha
  githubAppRef:
    name: %[2]s
  defaultProxyRef:
    name: shared
  logLevel: debug
---
apiVersion: actions-gateway.com/v2alpha1
kind: ActionsGateway
metadata:
  name: beta
  namespace: %[1]s
spec:
  githubURL: https://github.com/example-org-beta
  githubAppRef:
    name: %[2]s
  defaultProxyRef:
    name: shared
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
  name: set-alpha
  namespace: %[1]s
spec:
  gatewayRef:
    name: alpha
  templateRef:
    name: tmpl
  maxListeners: 2
  runnerLabels: ["e2e-alpha"]
---
apiVersion: actions-gateway.com/v2alpha1
kind: RunnerSet
metadata:
  name: set-beta
  namespace: %[1]s
spec:
  gatewayRef:
    name: beta
  templateRef:
    name: tmpl
  maxListeners: 2
  runnerLabels: ["e2e-beta"]
`, ns, secretName, workerImage)
}
