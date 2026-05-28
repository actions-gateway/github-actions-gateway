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

// CreateNamespace creates a namespace and applies the given labels.
func CreateNamespace(name string, labels map[string]string) {
	cmd := exec.Command("kubectl", "create", "namespace", name, "--dry-run=client", "-o", "yaml")
	yaml, err := Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "generate namespace yaml")
	Expect(ApplyManifest(yaml)).To(Succeed(), "apply namespace %s", name)

	for k, v := range labels {
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

// ApplyFakegithubEgressNetworkPolicy stamps an additive NetworkPolicy that lets
// the AGC in `ns` reach the fakegithub Service in the e2e-infra namespace on
// port 8080.
//
// Why this is needed: the per-tenant AGC NetworkPolicy created by the GMC
// restricts port-8080 egress to the proxy pods only (selected by
// `app: actions-gateway-proxy`). That is the production-correct shape —
// AGC and workers reach GitHub via the proxy, not arbitrary cluster endpoints.
// The e2e suite, however, points the AGC at the fakegithub Service running
// in `e2e-infra`, which sits on port 8080 and is reached directly
// (NO_PROXY includes `svc.cluster.local`). Without this additive policy,
// kindnet correctly drops the AGC→fakegithub connect and no broker session
// ever registers.
//
// The selector targets the AGC pod by `app: actions-gateway-controller` rather
// than `actions-gateway/component: workload`, because the AGC no longer carries
// the workload label (see the comment on labelComponent in builder.go for the
// PR #59 background).
//
// Caveat: applying this NP and the GMC-managed AGC NP to the same pod is
// exactly the two-NP-per-pod shape that the production AGC deliberately
// avoids. Tolerated here because (a) this is an e2e concern only, never
// reaches production, and (b) the production behaviour of AGC→fakegithub is
// not what we are testing. If e2e starts seeing the AGC lose its k8s API
// (443) or proxy (8080) egress after this NP is applied, the fix is to merge
// the rule below into the AGC NP itself via a controller flag rather than as
// a sidecar NP.
func ApplyFakegithubEgressNetworkPolicy(ns string) {
	manifest := fmt.Sprintf(`apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: e2e-fakegithub-egress
  namespace: %s
spec:
  podSelector:
    matchLabels:
      app: actions-gateway-controller
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
	cmd := exec.Command("kubectl", "delete", "actionsgateway", name, "-n", ns, "--ignore-not-found", "--timeout=5m")
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

