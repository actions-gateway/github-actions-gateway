//go:build e2e
// +build e2e

package utils

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/gomega" //nolint:revive
)

// ApplyManifest applies a raw YAML manifest by writing it to a temp file and
// running kubectl apply -f. This avoids the stdin limitation of utils.Run which
// uses cmd.CombinedOutput() and does not honour cmd.Stdin.
func ApplyManifest(yaml string) error {
	f, err := os.CreateTemp("", "e2e-manifest-*.yaml")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(yaml); err != nil {
		return err
	}
	f.Close()
	cmd := exec.Command("kubectl", "apply", "-f", f.Name())
	_, err = Run(cmd)
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
	defer os.Remove(f.Name())
	if _, err := f.WriteString(yaml); err != nil {
		return "", err
	}
	f.Close()
	cmd := exec.Command("kubectl", "apply", "-f", f.Name())
	return Run(cmd)
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
func CreateGitHubAppSecret(ns, name string, appID, installID int64, privateKeyPEM []byte) {
	cmd := exec.Command("kubectl", "create", "secret", "generic", name,
		"-n", ns,
		fmt.Sprintf("--from-literal=appId=%d", appID),
		fmt.Sprintf("--from-literal=installationId=%d", installID),
		fmt.Sprintf("--from-literal=privateKey=%s", string(privateKeyPEM)),
		"--dry-run=client", "-o", "yaml",
	)
	yaml, err := Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "generate secret yaml")
	Expect(ApplyManifest(yaml)).To(Succeed(), "create secret %s/%s", ns, name)
}

// ApplyActionsGatewayCR applies an ActionsGateway CR to the given namespace.
func ApplyActionsGatewayCR(ns, name, secretName string) {
	yaml := fmt.Sprintf(`apiVersion: actions-gateway.github.com/v1alpha1
kind: ActionsGateway
metadata:
  name: %s
  namespace: %s
spec:
  gitHubAppRef:
    name: %s
  # Required field; the value is a placeholder. The e2e AGC talks to fakegithub
  # via the stub registrar (STUB_AUTH_URL/STUB_BROKER_URL win over GITHUB_ORG_URL),
  # so this URL is not used for registration in the default stub flow. The
  # real-GitHub suite overrides it with AGC_EXTRA_GITHUB_ORG_URL.
  gitHubURL: https://github.com/e2e-org
  proxy:
    minReplicas: 1
    maxReplicas: 3
`, name, ns, secretName)

	Expect(ApplyManifest(yaml)).To(Succeed(), "apply ActionsGateway %s/%s", ns, name)
}

// ApplyActionsGatewayCRWithRunnerGroup applies an ActionsGateway CR that includes a
// minimal RunnerGroup so that AGC has something to reconcile and can register broker
// sessions (required for job-lifecycle e2e tests).
func ApplyActionsGatewayCRWithRunnerGroup(ns, name, secretName, workerImage string) {
	yaml := fmt.Sprintf(`apiVersion: actions-gateway.github.com/v1alpha1
kind: ActionsGateway
metadata:
  name: %s
  namespace: %s
spec:
  gitHubAppRef:
    name: %s
  # Required field; the value is a placeholder. The e2e AGC talks to fakegithub
  # via the stub registrar (STUB_AUTH_URL/STUB_BROKER_URL win over GITHUB_ORG_URL),
  # so this URL is not used for registration in the default stub flow. The
  # real-GitHub suite overrides it with AGC_EXTRA_GITHUB_ORG_URL.
  gitHubURL: https://github.com/e2e-org
  # Run this tenant's AGC at debug so a session/worker-pod timeout is diagnosable.
  # The session/job specs gate a DumpAGCSessionDiagnostics dump on failure, but the
  # listener's per-session lifecycle trail — session start, job received, heal,
  # single-use recycle, idle shutdown — was demoted to Debug in the logging-audit
  # (Q87, Theme D). At the default info level a "no worker pod scheduled yet"
  # timeout (Q134/Q148) therefore dumps only AGC startup logs and gives no hint why
  # a job was never acquired. The extra volume is negligible here (maxListeners: 2,
  # a handful of jobs) and the dump is only consumed on failure (Q148).
  logLevel: debug
  proxy:
    minReplicas: 1
    maxReplicas: 3
    noProxyCIDRs:
    - svc.cluster.local
  runnerGroups:
  - runnerLabels: ["e2e"]
    maxListeners: 2
    workerImage: %s
    podTemplate:
      spec:
        containers:
        - name: runner
          image: %s
`, name, ns, secretName, workerImage, workerImage)

	Expect(ApplyManifest(yaml)).To(Succeed(), "apply ActionsGateway %s/%s with runner group", ns, name)
}

// ApplyActionsGatewayCRWithRunnerGroupLifecycle applies an ActionsGateway CR
// whose RunnerGroup sets the Q95 worker-pod lifecycle knobs (completedPodTTL,
// pendingPodDeadline) so e2e tests can prove reaping on a short clock.
func ApplyActionsGatewayCRWithRunnerGroupLifecycle(ns, name, secretName, workerImage, completedPodTTL, pendingPodDeadline string) {
	yaml := fmt.Sprintf(`apiVersion: actions-gateway.github.com/v1alpha1
kind: ActionsGateway
metadata:
  name: %s
  namespace: %s
spec:
  gitHubAppRef:
    name: %s
  # Required field; the value is a placeholder. The e2e AGC talks to fakegithub
  # via the stub registrar (STUB_AUTH_URL/STUB_BROKER_URL win over GITHUB_ORG_URL),
  # so this URL is not used for registration in the default stub flow. The
  # real-GitHub suite overrides it with AGC_EXTRA_GITHUB_ORG_URL.
  gitHubURL: https://github.com/e2e-org
  # Run this tenant's AGC at debug so a session/worker-pod timeout is diagnosable.
  # The session/job specs gate a DumpAGCSessionDiagnostics dump on failure, but the
  # listener's per-session lifecycle trail — session start, job received, heal,
  # single-use recycle, idle shutdown — was demoted to Debug in the logging-audit
  # (Q87, Theme D). At the default info level a "no worker pod scheduled yet"
  # timeout (Q134/Q148) therefore dumps only AGC startup logs and gives no hint why
  # a job was never acquired. The extra volume is negligible here (maxListeners: 2,
  # a handful of jobs) and the dump is only consumed on failure (Q148).
  logLevel: debug
  proxy:
    minReplicas: 1
    maxReplicas: 3
    noProxyCIDRs:
    - svc.cluster.local
  runnerGroups:
  - runnerLabels: ["e2e"]
    maxListeners: 2
    workerImage: %s
    completedPodTTL: %s
    pendingPodDeadline: %s
    podTemplate:
      spec:
        containers:
        - name: runner
          image: %s
`, name, ns, secretName, workerImage, completedPodTTL, pendingPodDeadline, workerImage)

	Expect(ApplyManifest(yaml)).To(Succeed(), "apply ActionsGateway %s/%s with lifecycle runner group", ns, name)
}

// ApplyActionsGatewayCRWithWorkerCeiling applies an ActionsGateway CR whose
// RunnerGroup sets maxWorkers (the worker pod-capacity ceiling the Q59 admission
// gate enforces) and pins every worker pod Pending via an unsatisfiable
// nodeSelector. A Pending pod counts toward the ceiling (activePodCount counts
// Pending) and holds its reservation — the listener's job handler blocks in
// waitForCompletion until the pod reaches a terminal phase — so the test can
// hold capacity busy deterministically and free it on demand by deleting the
// pod (the InformerPodWaiter treats deletion as completion). This is the seam
// the Q154 e2e uses to drive the gate to its full state without depending on a
// runner image actually running.
func ApplyActionsGatewayCRWithWorkerCeiling(ns, name, secretName, workerImage string, maxWorkers int) {
	yaml := fmt.Sprintf(`apiVersion: actions-gateway.github.com/v1alpha1
kind: ActionsGateway
metadata:
  name: %s
  namespace: %s
spec:
  gitHubAppRef:
    name: %s
  gitHubURL: https://github.com/e2e-org
  # Debug so a "job never acquired" / "ceiling never held" timeout is diagnosable
  # from the per-session lifecycle trail (demoted to Debug in Q87, Theme D).
  logLevel: debug
  proxy:
    minReplicas: 1
    maxReplicas: 3
    noProxyCIDRs:
    - svc.cluster.local
  runnerGroups:
  - runnerLabels: ["e2e"]
    maxListeners: 2
    maxWorkers: %d
    workerImage: %s
    podTemplate:
      spec:
        # Unschedulable: every worker pod stays Pending, holding its ceiling slot
        # and reservation until the test deletes it. The runner image is never
        # pulled, so the test does not depend on a runnable worker.
        nodeSelector:
          q154.actions-gateway/never-schedules: "true"
        containers:
        - name: runner
          image: %s
`, name, ns, secretName, maxWorkers, workerImage, workerImage)

	Expect(ApplyManifest(yaml)).To(Succeed(), "apply ActionsGateway %s/%s with worker ceiling", ns, name)
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
