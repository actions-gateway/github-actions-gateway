//go:build e2e
// +build e2e

package utils

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	// ST1001: the gomega dot-import is the sanctioned ginkgo/gomega idiom — the
	// matcher DSL (Expect/Succeed/HaveOccurred) is designed to read unqualified,
	// and every other e2e file in this tree imports it the same way. Accepted
	// here rather than repo-wide so a dot-import of anything else still fails.
	. "github.com/onsi/gomega" //nolint:revive,staticcheck
)

// ApplyManifest applies a raw YAML manifest by writing it to a temp file and
// running kubectl apply -f. This avoids the stdin limitation of utils.Run which
// uses cmd.CombinedOutput() and does not honour cmd.Stdin.
func ApplyManifest(yaml string) error {
	_, err := ApplyManifestOutput(yaml)
	return err
}

// ApplyManifestOutput applies a raw YAML manifest like ApplyManifest but returns
// kubectl's combined output (stdout+stderr) alongside the error, so callers can
// assert on an admission-webhook rejection message — e.g. to prove the webhook
// ran (its validation text appears) rather than being unreachable (a transport
// error appears). On apply failure Run wraps the output into the error, but the
// returned string carries the raw output verbatim for substring assertions.
func ApplyManifestOutput(yaml string) (string, error) {
	f, err := os.CreateTemp("", "e2e-manifest-*.yaml")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err := f.WriteString(yaml); err != nil {
		return "", err
	}
	// Close before apply, and CHECK it: a failed Close means the manifest was
	// never fully flushed, so kubectl would apply a truncated document and the
	// spec would fail somewhere far from the real cause.
	if err := f.Close(); err != nil {
		return "", err
	}
	cmd := exec.Command("kubectl", "apply", "-f", f.Name())
	return Run(cmd)
}

// webhookTransportErrorRe matches the apiserver's error text when it could not
// COMPLETE a call to an admission webhook — a transport-level failure (the
// webhook Service had no ready endpoint for the picked pod, its TLS listener was
// mid-restart, or the POST outran its per-call deadline) — as opposed to the
// webhook running and DENYING the request. The apiserver emits these phrases
// only for the unreachable case; a genuine rejection reads
// `admission webhook "…" denied the request: …` and matches none of them. The
// signatures mirror manager_np_test.go's admission-reachability probe.
var webhookTransportErrorRe = regexp.MustCompile(
	`(?i)failed calling webhook|failed to call webhook|context deadline exceeded|` +
		`connection refused|no route to host|no endpoints available`)

// ApplyManifestWithWebhookRetry applies a manifest that triggers the GMC
// admission webhooks, retrying ONLY while the apiserver reports it could not
// reach the webhook. Under the parallel e2e suite (`--procs 6`) a single
// webhook POST can transiently hit `context deadline exceeded` even though the
// GMC pod is `1/1 Running` with programmed endpoints; with `failurePolicy: Fail`
// and no apiserver-side retry, that one stall hard-fails a plain one-shot apply
// (Q391). Retrying rides out the blip.
//
// It is NOT a blind retry: a clean apply returns nil immediately, and a genuine
// webhook DENIAL (or any non-transport error) is returned on the FIRST attempt
// so real failures fail fast instead of burning the budget. kubectl apply is
// idempotent, so re-applying objects an earlier attempt already created is safe.
// A persistent webhook outage still surfaces the last transport error once the
// bounded budget elapses — this waits for the webhook to actually respond, it
// does not paper a real outage over.
func ApplyManifestWithWebhookRetry(yaml string) error {
	const (
		budget = 90 * time.Second
		gap    = 3 * time.Second
	)
	deadline := time.Now().Add(budget)
	for {
		err := ApplyManifest(yaml)
		if err == nil || !webhookTransportErrorRe.MatchString(err.Error()) {
			return err
		}
		if !time.Now().Before(deadline) {
			return err
		}
		time.Sleep(gap)
	}
}

// CreateNamespace creates a namespace and applies the given labels.
//
// Every namespace created here is a GMC-managed tenant namespace, so it is
// stamped with the actions-gateway.github.com/tenant=true marker label that the
// GMC admission policies require: namespace-psa-guard before the GMC may patch
// Pod Security Admission labels on it, and tenant-resource-guard before the GMC
// may create any tenant resource (Deployments, Secrets, RoleBindings, …) in it
// (see cmd/gmc/config/admission-policy/). Without the marker the GMC reconcile is
// denied at the PSA-stamping step and never provisions tenant resources. A caller
// may override the marker by passing it in labels.
func CreateNamespace(name string, labels map[string]string) {
	cmd := exec.Command("kubectl", "create", "namespace", name, "--dry-run=client", "-o", "yaml")
	yaml, err := Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "generate namespace yaml")
	Expect(ApplyManifest(yaml)).To(Succeed(), "apply namespace %s", name)

	merged := map[string]string{"actions-gateway.github.com/tenant": "true"}
	for k, v := range labels {
		merged[k] = v
	}
	for k, v := range merged {
		cmd = exec.Command("kubectl", "label", "--overwrite", "namespace", name, fmt.Sprintf("%s=%s", k, v))
		_, err = Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "label namespace %s", name)
	}
}

// DeleteNamespace deletes a namespace, ignoring not-found errors.
func DeleteNamespace(name string) {
	cmd := exec.Command("kubectl", "delete", "namespace", name, "--ignore-not-found", "--wait=false")
	_, _ = Run(cmd)
}

// CreateGitHubAppSecret creates a Kubernetes Secret with GitHub App credentials
// in the given namespace. The privateKeyPEM must be a valid RSA PEM block.
//
// The key goes through a temp file and --from-file, never --from-literal: Run
// echoes every command's argv to the GinkgoWriter and folds it into the error
// on failure, so a literal PEM would reach the run log, the JUnit report, and
// any `ps` snapshot taken while kubectl is running. live-GitHub passes a live GitHub
// App key here (other tiers use a throwaway), so that exposure is real. appId
// and installationId are not secret and stay inline. This matches how
// scripts/dogfood/{setup,e2e-setup}.sh already stamp the same Secret.
func CreateGitHubAppSecret(ns, name string, appID, installID int64, privateKeyPEM []byte) {
	// CreateTemp opens at 0600, so the PEM is never world-readable on disk.
	keyFile, err := os.CreateTemp("", "e2e-github-app-key-*.pem")
	Expect(err).NotTo(HaveOccurred(), "create temp file for GitHub App private key")
	defer func() { _ = os.Remove(keyFile.Name()) }()
	_, err = keyFile.Write(privateKeyPEM)
	Expect(err).NotTo(HaveOccurred(), "write GitHub App private key")
	// Closed before kubectl reads it, and CHECKED for the same reason
	// ApplyManifest checks: an unflushed write would hand kubectl a truncated
	// PEM, and the failure would surface far from the real cause.
	Expect(keyFile.Close()).To(Succeed(), "close GitHub App private key file")

	cmd := exec.Command("kubectl", "create", "secret", "generic", name,
		"-n", ns,
		fmt.Sprintf("--from-literal=appId=%d", appID),
		fmt.Sprintf("--from-literal=installationId=%d", installID),
		"--from-file=privateKey="+keyFile.Name(),
		"--dry-run=client", "-o", "yaml",
	)
	yaml, err := Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "generate secret yaml")
	Expect(ApplyManifest(yaml)).To(Succeed(), "create secret %s/%s", ns, name)
}

// ApplyFakegithubEgressNetworkPolicy stamps an additive NetworkPolicy that lets
// workload-labeled pods in `ns` reach the fakegithub Service in the e2e-infra
// namespace on port 8080.
//
// Why this is needed: the per-tenant workload NetworkPolicy created by the GMC
// restricts port-8080 egress to the proxy pods only (selected by
// `app: actions-gateway-proxy`). That is the production-correct shape — workers
// must not reach arbitrary cluster endpoints. The e2e suite, however, points
// the AGC at the fakegithub Service running in `e2e-infra`, which sits on
// port 8080 and is reached directly (NO_PROXY includes `svc.cluster.local`).
// Without this additive policy, kindnet correctly drops the AGC→fakegithub
// connect and no broker session ever registers.
//
// NetworkPolicies are additive: this policy adds an allowed egress path
// without weakening the workload NP's deny-by-default for everything else.
func ApplyFakegithubEgressNetworkPolicy(ns string) {
	manifest := fmt.Sprintf(`apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: e2e-fakegithub-egress
  namespace: %s
spec:
  podSelector:
    matchLabels:
      actions-gateway/component: workload
  policyTypes: [Egress]
  egress:
  - to:
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: e2e-infra
      podSelector:
        matchLabels:
          app: fakegithub
    ports:
    - port: 8080
      protocol: TCP
`, ns)
	Expect(ApplyManifest(manifest)).To(Succeed(), "apply fakegithub egress NP in %s", ns)
}

// DeleteActionsGatewayCR deletes an ActionsGateway CR and waits for the finalizer to clear.
// A 5-minute timeout prevents hangs if the controller is unavailable.
func DeleteActionsGatewayCR(ns, name string) {
	cmd := exec.Command("kubectl", "delete", "actionsgateways.actions-gateway.github.com", name, "-n", ns, "--ignore-not-found", "--timeout=5m")
	_, _ = Run(cmd)
}

// WaitForDeploymentReady waits until a Deployment reaches the desired ready replica count.
func WaitForDeploymentReady(ns, name string, timeout time.Duration) {
	EventuallyWithOffset(1, func(g Gomega) {
		cmd := exec.Command("kubectl", "get", "deployment", name,
			"-n", ns,
			"-o", "jsonpath={.status.readyReplicas}",
		)
		out, err := Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(out).NotTo(BeEmpty(), "readyReplicas not yet set")
		g.Expect(out).NotTo(Equal("0"), "no ready replicas yet")
	}, timeout, 2*time.Second).Should(Succeed(), "deployment %s/%s not ready", ns, name)
}

// WaitForRunnerGroupReconciled waits until at least one RunnerGroup in ns has a
// populated .status.observedGeneration. The AGC sets observedGeneration only at
// the end of a full reconcile — after the installation-token fetch, agent-pool
// registration (EnsureAgents), and listener-multiplexer start have all
// succeeded — so this is the signal that the AGC is operationally past startup
// and a broker session is imminent.
//
// Deployment readiness (readyReplicas>=1, WaitForDeploymentReady) is a far
// weaker signal and must not be mistaken for it: the AGC's health server binds
// within a few seconds of pod start and is deliberately decoupled from token
// acquisition (see cmd/agc/main.go — readiness is bound early so rollout success
// does not hinge on GitHub reachability). The initial token fetch alone has a
// budget of up to ~2 minutes there. Gating a session-registration wait on
// Deployment readiness therefore folds the AGC's entire startup (token +
// registration + first session, all round-trips to the shared single-replica
// fakegithub) into the session budget; under parallel CI load those round-trips
// slow and the budget is exhausted before any session appears, surfacing as a
// misleading "no session registered" timeout (Q134). Waiting for this stronger
// signal first separates "AGC still starting up" from "session failed to
// register" and keeps each phase within its own budget.
func WaitForRunnerGroupReconciled(ns string, timeout time.Duration) {
	EventuallyWithOffset(1, func(g Gomega) {
		cmd := exec.Command("kubectl", "get", "runnergroup",
			"-n", ns,
			"-o", `jsonpath={range .items[*]}{.status.observedGeneration}{"\n"}{end}`,
		)
		out, err := Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())
		reconciled := false
		for _, line := range GetNonEmptyLines(out) {
			if strings.TrimSpace(line) != "0" {
				reconciled = true
				break
			}
		}
		g.Expect(reconciled).To(BeTrue(), "no RunnerGroup in %s has a reconciled status yet", ns)
	}, timeout, 2*time.Second).Should(Succeed(), "no RunnerGroup in %s reached a reconciled status", ns)
}

// WaitForCondition waits until the given jsonpath expression on a resource equals expectedValue.
func WaitForCondition(resource, ns, name, jsonpath, expectedValue string, timeout time.Duration) {
	EventuallyWithOffset(1, func(g Gomega) {
		cmd := exec.Command("kubectl", "get", resource, name,
			"-n", ns,
			"-o", fmt.Sprintf("jsonpath={%s}", jsonpath),
		)
		out, err := Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(out).To(Equal(expectedValue),
			"%s/%s %s jsonpath %s: got %q want %q", resource, name, ns, jsonpath, out, expectedValue)
	}, timeout, 3*time.Second).Should(Succeed())
}

// ResourceExists returns true if the named resource exists in ns.
func ResourceExists(resource, ns, name string) bool {
	cmd := exec.Command("kubectl", "get", resource, name, "-n", ns, "--ignore-not-found")
	out, err := Run(cmd)
	if err != nil {
		return false
	}
	return strings.TrimSpace(out) != ""
}
