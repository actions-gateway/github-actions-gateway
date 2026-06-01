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
			cmd := exec.Command("kubectl", "get", "actionsgateway", agName,
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
		cmd := exec.Command("kubectl", "patch", "actionsgateway", agName,
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
		// The IPRangeReconciler refreshes the cache on Manager start, then the
		// periodic reconciler patches the proxy NP with GitHub CIDRs. Until that
		// completes the NP only permits DNS — CONNECT to api.github.com would
		// silently drop. Wait for at least one ipBlock egress peer to appear.
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
    image: curlimages/curl:8.10.1
    imagePullPolicy: IfNotPresent
    command: ["sh", "-c"]
    args:
    - |
      set -eu
      curl --silent --show-error --fail-with-body \
           --max-time 30 \
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
`, curlPodName, tenantNS, proxyURL)

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

		Expect(finalPhase).To(Equal("Succeeded"),
			"curl pod ended in phase %s; logs:\n%s", finalPhase, logs)
		Expect(logs).To(ContainSubstring("HTTP_CODE=200"),
			"expected HTTP 200 from api.github.com via proxy; logs:\n%s", logs)
		Expect(logs).To(MatchRegexp(`BODY_BYTES=([1-9][0-9]*)`),
			"expected non-empty response body from api.github.com/zen; logs:\n%s", logs)
	})

	// E2E_GMC_TenantProvisioning_WorkloadDirectEgressBlocked is the negative
	// counterpart to E2E_GMC_TenantProvisioning_ProxyConnectWorks. It confirms
	// that the workload NetworkPolicy blocks direct egress to GitHub from a
	// workload-labelled pod when the proxy is bypassed (--noproxy '*'). Without
	// this assertion, a regression that broadens workload egress to GitHub CIDRs
	// (defeating the per-tenant egress-IP guarantee) would still pass the
	// positive ProxyConnectWorks test and ship silently.
	It("E2E_GMC_TenantProvisioning_WorkloadDirectEgressBlocked: workload pod cannot reach GitHub directly", func() {
		const curlPodName = "workload-direct-egress-probe"

		By("deploying a workload-labelled curl pod that bypasses the proxy")
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
    image: curlimages/curl:8.10.1
    imagePullPolicy: IfNotPresent
    command: ["curl"]
    args:
    - "--silent"
    - "--show-error"
    - "--noproxy"
    - "*"
    - "--max-time"
    - "5"
    - "--connect-timeout"
    - "5"
    - "--output"
    - "/dev/null"
    - "--write-out"
    - "HTTP_CODE=%%{http_code}\n"
    - "https://api.github.com/zen"
`, curlPodName, tenantNS)

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
				"-n", tenantNS, "-o", "jsonpath={.status.phase}")
			out, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(Or(Equal("Succeeded"), Equal("Failed")),
				"curl pod still in phase %q", out)
			finalPhase = out
		}, 90*time.Second, 2*time.Second).Should(Succeed())

		logsCmd := exec.Command("kubectl", "logs", curlPodName, "-n", tenantNS)
		logs, _ := utils.Run(logsCmd)

		// NetworkPolicy drops produce a connect timeout (curl exits 28, pod
		// Failed). The crucial invariant: the workload pod must NOT have
		// completed an HTTP exchange with api.github.com.
		Expect(finalPhase).To(Equal("Failed"),
			"workload pod direct egress to GitHub should be blocked by NetworkPolicy; got phase=%s logs:\n%s", finalPhase, logs)
		Expect(logs).NotTo(ContainSubstring("HTTP_CODE=200"),
			"workload pod direct egress to GitHub should be blocked; got HTTP 200, logs:\n%s", logs)
	})

	// E2E_GMC_TenantProvisioning_WorkerCannotReachK8sAPI verifies the M-12 split:
	// worker pods (workload-labelled, NOT carrying the AGC app label) are
	// selected only by the workload NetworkPolicy and so have no egress to the
	// Kubernetes API server. The AGC NetworkPolicy adds 443/6443-to-anywhere
	// only for pods labelled app=actions-gateway-controller, so a workload-only
	// pod (a stand-in for a worker) must time out when reaching kubernetes.default.
	It("E2E_GMC_TenantProvisioning_WorkerCannotReachK8sAPI: worker-labelled pod cannot reach the K8s API server", func() {
		const curlPodName = "worker-k8s-api-probe"

		By("deploying a workload-only-labelled curl pod (simulating a worker)")
		manifest := fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %s
  namespace: %s
  labels:
    actions-gateway/component: workload
spec:
  restartPolicy: Never
  automountServiceAccountToken: false
  containers:
  - name: curl
    image: curlimages/curl:8.10.1
    imagePullPolicy: IfNotPresent
    command: ["curl"]
    args:
    - "--silent"
    - "--show-error"
    - "--noproxy"
    - "*"
    - "--insecure"
    - "--max-time"
    - "5"
    - "--connect-timeout"
    - "5"
    - "--output"
    - "/dev/null"
    - "--write-out"
    - "HTTP_CODE=%%{http_code}\n"
    - "https://kubernetes.default.svc:443/version"
`, curlPodName, tenantNS)

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
				"-n", tenantNS, "-o", "jsonpath={.status.phase}")
			out, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(Or(Equal("Succeeded"), Equal("Failed")),
				"curl pod still in phase %q", out)
			finalPhase = out
		}, 90*time.Second, 2*time.Second).Should(Succeed())

		logsCmd := exec.Command("kubectl", "logs", curlPodName, "-n", tenantNS)
		logs, _ := utils.Run(logsCmd)

		// kubernetes.default.svc resolves via DNS (allowed by the workload NP),
		// but the TCP connection on 443 must be dropped because the workload NP
		// only permits egress to proxy pods on proxyPort. A successful HTTP
		// exchange would indicate the M-12 split has regressed.
		Expect(finalPhase).To(Equal("Failed"),
			"worker-labelled pod must not reach the K8s API server; got phase=%s logs:\n%s", finalPhase, logs)
		Expect(logs).NotTo(ContainSubstring("HTTP_CODE=200"),
			"worker-labelled pod must not reach the K8s API server; got HTTP 200, logs:\n%s", logs)
		Expect(logs).NotTo(ContainSubstring("HTTP_CODE=401"),
			"worker-labelled pod must not reach the K8s API server; got HTTP 401 (unauthenticated reply means TCP succeeded), logs:\n%s", logs)
		Expect(logs).NotTo(ContainSubstring("HTTP_CODE=403"),
			"worker-labelled pod must not reach the K8s API server; got HTTP 403 (forbidden reply means TCP succeeded), logs:\n%s", logs)
	})
})
