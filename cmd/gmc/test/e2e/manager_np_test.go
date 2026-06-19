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

// Manager NetworkPolicy names rendered by the Helm chart (namePrefix "gmc").
// The chart ships two ingress policies that together flip the manager pod to
// default-deny ingress and re-admit exactly the metrics scrapers and the
// apiserver webhook calls (charts/actions-gateway/templates/networkpolicy.yaml).
const (
	metricsNPName = "gmc-allow-metrics-traffic"
	webhookNPName = "gmc-allow-webhook-traffic"

	// metricsNPLabeledNS / metricsNPUnlabeledNS are throwaway client namespaces
	// for the metrics-ingress probes. The manager's allow-metrics-traffic NP keys
	// on the *namespace* label `metrics: enabled` (a namespaceSelector), so the
	// probe pod's own labels are irrelevant — only the namespace label decides.
	metricsNPLabeledNS   = "gmc-np-metrics-allowed"
	metricsNPUnlabeledNS = "gmc-np-metrics-denied"

	// webhookNPNamespace hosts the admission probes. Must not be a reserved
	// namespace (gmc-system/kube-system/etc.) or the webhook rejects the create
	// for the wrong reason.
	webhookNPNamespace = "gmc-np-webhook"
)

// E2E_GMC_ManagerMetricsNP verifies, at runtime on an enforcing CNI, that the
// default-on manager NetworkPolicy (Q34/E5) restricts the metrics endpoint
// (:8443) to namespaces labeled `metrics: enabled` while leaving the webhook
// port (:9443) open so admission keeps working (Q83).
//
// The metrics-ingress specs require a CNI that actually enforces NetworkPolicy
// (Calico/Cilium); kindnet's bundled enforcer does not reliably drop the traffic
// (see provisioning_test.go's egressEnforcingCNI comment and Q7b/Q119), so they
// self-skip on kindnet. The webhook-admission spec runs everywhere — admission
// must always succeed — but under an enforcing CNI it additionally proves the NP
// admits :9443 (a regression that source-restricted that port, combined with
// failurePolicy: Fail, would drop every admission call).
var _ = Describe("Manager NetworkPolicy", Ordered, func() {
	BeforeAll(func() {
		By("waiting for the manager metrics + webhook NetworkPolicies to be reconciled")
		Eventually(func() bool {
			return utils.ResourceExists("networkpolicy", gmcNamespace, metricsNPName) &&
				utils.ResourceExists("networkpolicy", gmcNamespace, webhookNPName)
		}, 2*time.Minute, 2*time.Second).Should(BeTrue(),
			"manager NetworkPolicies %q/%q not present — chart networkPolicy.enabled default regressed?",
			metricsNPName, webhookNPName)

		// The admission spec exercises the validating webhook, so the webhook
		// server must be fully serving before we probe it — otherwise a transient
		// "context deadline exceeded" (server still starting, endpoints not yet
		// populated, CA bundle not yet injected) is indistinguishable from the NP
		// blocking :9443. Mirror the readiness gates e2e_test.go uses for the
		// metrics-scrape spec: endpoints populated + CA bundle injected.
		By("waiting for the webhook Service endpoints to be ready")
		Eventually(func(g Gomega) {
			out, err := utils.Run(exec.Command("kubectl", "get", "endpointslices.discovery.k8s.io",
				"-n", gmcNamespace, "-l", "kubernetes.io/service-name=webhook-service",
				"-o", "jsonpath={range .items[*]}{range .endpoints[*]}{.addresses[*]}{end}{end}"))
			g.Expect(err).NotTo(HaveOccurred(), "webhook endpoints should exist")
			g.Expect(out).ShouldNot(BeEmpty(), "webhook endpoints not yet ready")
		}, 3*time.Minute, time.Second).Should(Succeed())

		By("waiting for the validating webhook CA bundle to be injected")
		Eventually(func(g Gomega) {
			out, err := utils.Run(exec.Command("kubectl", "get",
				"validatingwebhookconfigurations.admissionregistration.k8s.io",
				"gmc-validating-webhook-configuration",
				"-o", "jsonpath={.webhooks[0].clientConfig.caBundle}"))
			g.Expect(err).NotTo(HaveOccurred(), "ValidatingWebhookConfiguration should exist")
			g.Expect(out).ShouldNot(BeEmpty(), "validating webhook CA bundle not yet injected")
		}, 3*time.Minute, time.Second).Should(Succeed())
	})

	// metricsURL targets the manager metrics Service on :8443. --insecure: the
	// probe asserts TCP/TLS reachability through the NP, not metrics auth — an
	// unauthenticated request still completes the TLS handshake and gets an HTTP
	// status (401/403), which is enough to prove the NP admitted the connection.
	metricsURL := fmt.Sprintf("--insecure https://%s.%s.svc.cluster.local:8443/metrics",
		metricsServiceName, gmcNamespace)

	It("E2E_GMC_ManagerMetricsNP_DeniesUnlabeledNamespace: scrape from a namespace without `metrics: enabled` is blocked", func() {
		if !egressEnforcingCNI() {
			Skip("cluster CNI does not enforce NetworkPolicy (kindnet); recreate with `make e2e-cluster KIND_CNI=calico` (Q7b/Q83)")
		}

		By("creating a client namespace WITHOUT the `metrics: enabled` label")
		utils.CreateNamespace(metricsNPUnlabeledNS, nil)
		DeferCleanup(func() { utils.DeleteNamespace(metricsNPUnlabeledNS) })

		By("negative: a pod in the unlabeled namespace cannot reach the manager metrics endpoint :8443")
		logs := runEgressProbe(metricsNPUnlabeledNS, "metrics-denied", false, metricsURL)
		Expect(logs).To(MatchRegexp(`CURL_RC=(7|28)(\s|$)`),
			"scrape from an unlabeled namespace was NOT blocked (expected connect refused/timeout); the manager NP is not enforcing the `metrics: enabled` restriction; logs:\n%s", logs)
		Expect(logs).To(ContainSubstring("HTTP_CODE=000"),
			"scrape from an unlabeled namespace completed an HTTP exchange with :8443 — NP did not deny it; logs:\n%s", logs)
	})

	It("E2E_GMC_ManagerMetricsNP_AllowsLabeledNamespace: scrape from a namespace with `metrics: enabled` reaches :8443", func() {
		if !egressEnforcingCNI() {
			Skip("cluster CNI does not enforce NetworkPolicy (kindnet); recreate with `make e2e-cluster KIND_CNI=calico` (Q7b/Q83)")
		}

		By("creating a client namespace WITH the `metrics: enabled` label")
		utils.CreateNamespace(metricsNPLabeledNS, map[string]string{"metrics": "enabled"})
		DeferCleanup(func() { utils.DeleteNamespace(metricsNPLabeledNS) })

		// Positive control: proves the endpoint is reachable when the NP admits
		// it, so the negative above is attributable to NP enforcement and not a
		// dead endpoint. A non-000 HTTP code (even 401/403 from the metrics
		// authn filter) means the TCP/TLS connection was allowed through.
		By("positive: a pod in the labeled namespace reaches the manager metrics endpoint :8443")
		logs := runEgressProbe(metricsNPLabeledNS, "metrics-allowed", false, metricsURL)
		Expect(logs).To(MatchRegexp(`CURL_RC=0(\s|$)`),
			"scrape from a `metrics: enabled` namespace was blocked — the NP is denying an allowed source; logs:\n%s", logs)
		Expect(logs).NotTo(ContainSubstring("HTTP_CODE=000"),
			"scrape from a `metrics: enabled` namespace got no HTTP response (HTTP_CODE=000) — the NP blocked an allowed source; logs:\n%s", logs)
	})

	It("E2E_GMC_ManagerWebhookNP_AdmissionStillWorks: validating webhook on :9443 is reachable through the NP", func() {
		By("creating a non-reserved tenant namespace for the admission probes")
		utils.CreateNamespace(webhookNPNamespace, nil)
		DeferCleanup(func() { utils.DeleteNamespace(webhookNPNamespace) })

		// Negative admission: an ActionsGateway carrying a cross-namespace
		// gitHubAppRef.namespace must be REJECTED by the validating webhook's own
		// logic. This case is chosen deliberately: it passes the CRD's OpenAPI/CEL
		// schema validation (the namespace field is a plain optional string with no
		// schema rule), so the apiserver forwards the request to the :9443 webhook
		// rather than rejecting it earlier — making the rejection genuine proof the
		// webhook was reached and ran. (A non-https gitHubURL, by contrast, is
		// caught by the CRD schema's `^https://` pattern before the webhook is ever
		// called, so it proves nothing about :9443.) The error message is the proof
		// of reachability: only the webhook code emits "gitHubAppRef.namespace is
		// not supported". If the NP had blocked :9443, failurePolicy: Fail would
		// surface a webhook *call* error ("failed calling webhook"/"context deadline
		// exceeded") instead. Create the invalid CR FIRST, in an otherwise-empty
		// namespace, so the singleton check (which runs before the appRef check)
		// can't pre-empt the rejection.
		By("negative admission: an invalid ActionsGateway is rejected by the webhook (proves it ran)")
		invalidAG := fmt.Sprintf(`apiVersion: actions-gateway.github.com/v1alpha1
kind: ActionsGateway
metadata:
  name: np-webhook-invalid
  namespace: %s
spec:
  gitHubAppRef:
    name: nonexistent
    namespace: some-other-namespace
  gitHubURL: https://github.com/e2e-org
`, webhookNPNamespace)
		// Retry until the webhook returns its validation verdict. The first calls
		// after a fresh deploy can race the webhook server's TLS listener coming up
		// (a transient transport timeout), so a single attempt would flake. A
		// genuine NP block on :9443 never resolves, so the Eventually still fails —
		// surfacing the transport error in its message — which is the real finding
		// the task asks us not to paper over.
		var rejectOut string
		Eventually(func(g Gomega) {
			out, err := utils.ApplyManifestOutput(invalidAG)
			g.Expect(err).To(HaveOccurred(), "invalid ActionsGateway was admitted; webhook validation did not run:\n%s", out)
			g.Expect(out).NotTo(MatchRegexp(`(?i)failed calling webhook|context deadline exceeded|connection refused|no route to host`),
				"admission failed with a webhook-call/transport error — webhook on :9443 not (yet) reachable; output:\n%s", out)
			g.Expect(out).To(ContainSubstring("gitHubAppRef.namespace is not supported"),
				"invalid ActionsGateway was not rejected by the webhook's own validation logic; output:\n%s", out)
			rejectOut = out
		}, 2*time.Minute, 5*time.Second).Should(Succeed(),
			"the validating webhook on :9443 never returned its verdict — if this is a transport timeout, the manager NetworkPolicy is blocking admission (Q83 contradiction)")
		_, _ = fmt.Fprintf(GinkgoWriter, "webhook rejection (proves :9443 reachable through NP):\n%s\n", rejectOut)

		// Positive admission: a valid ActionsGateway must be ADMITTED. With
		// failurePolicy: Fail this can only succeed if the webhook was reached AND
		// returned allow — itself strong evidence the NP leaves :9443 open.
		By("positive admission: a valid ActionsGateway is admitted")
		validAG := fmt.Sprintf(`apiVersion: actions-gateway.github.com/v1alpha1
kind: ActionsGateway
metadata:
  name: np-webhook-valid
  namespace: %s
spec:
  gitHubAppRef:
    name: nonexistent
  gitHubURL: https://github.com/e2e-org
  proxy:
    minReplicas: 1
    maxReplicas: 3
`, webhookNPNamespace)
		out, err := utils.ApplyManifestOutput(validAG)
		Expect(err).NotTo(HaveOccurred(),
			"valid ActionsGateway was rejected; with failurePolicy: Fail this means the webhook on :9443 was unreachable (NP regression) or the CR is unexpectedly invalid; output:\n%s", out)
		Expect(strings.TrimSpace(out)).NotTo(BeEmpty(), "kubectl apply produced no output for the valid create")
	})
})
