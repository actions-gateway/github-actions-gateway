package utils

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2" //nolint:revive
)

// supersededRSDescription is what `kubectl describe replicasets` prints for a
// ReplicaSet a roll has scaled to zero: the pods are gone, the template is not.
const supersededRSDescription = `Name:           ssrec-agc-b8d7d6678
Namespace:      ssrec
Annotations:    deployment.kubernetes.io/revision: 1
Replicas:       0 current / 0 desired
Pod Template:
  Containers:
   agc:
    Image:  ghcr.io/example/agc:v0.9.0
    Environment:
      GITHUB_APP_PRIVATE_KEY:  <set to the key 'key' in secret 'agc-app'>
`

// fakeKubectl puts a kubectl on PATH that answers `describe replicasets` with
// fixture and echoes its arguments for every other subcommand.
func fakeKubectl(t *testing.T, fixture string) {
	t.Helper()
	dir := t.TempDir()
	fixturePath := filepath.Join(dir, "describe-rs.txt")
	if err := os.WriteFile(fixturePath, []byte(fixture), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = describe ] && [ \"$2\" = replicasets ]; then cat " + fixturePath + "; exit 0; fi\n" +
		"echo \"fake kubectl $*\"\n"
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o700); err != nil { //nolint:gosec // test fixture must be executable
		t.Fatalf("write fake kubectl: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// captureGinkgoWriter tees the diagnostics output the dump writes.
func captureGinkgoWriter(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	GinkgoWriter.TeeTo(&buf)
	t.Cleanup(GinkgoWriter.ClearTeeWriters)
	return &buf
}

// fakeKubectlForNamespaces puts a kubectl on PATH that reports every namespace
// in present as existing and every other one as not found, answers `get pods`
// with a two-pod list, and echoes its arguments otherwise.
func fakeKubectlForNamespaces(t *testing.T, present ...string) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = get ] && [ \"$2\" = namespace ]; then\n" +
		"  case \" " + strings.Join(present, " ") + " \" in *\" $3 \"*) echo \"namespace/$3\"; exit 0;; esac\n" +
		"  echo 'Error from server (NotFound)' >&2; exit 1\n" +
		"fi\n" +
		"if [ \"$1\" = get ] && [ \"$2\" = pods ]; then printf 'pod/proxy-0\\npod/agc-0\\n'; exit 0; fi\n" +
		"echo \"fake kubectl $*\"\n"
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o700); err != nil { //nolint:gosec // test fixture must be executable
		t.Fatalf("write fake kubectl: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// The dump exists to make a torn-down namespace diagnosable, so it must carry
// the GMC's verdict and the manager's log tail, not just the tenant inventory.
func TestDumpProvisioningDiagnosticsCapturesGatewayStatusAndManagerLogs(t *testing.T) {
	fakeKubectlForNamespaces(t, "tenant-a")
	out := captureGinkgoWriter(t)

	DumpProvisioningDiagnostics("gmc-system", "gmc-controller-manager", "tenant-a")

	dumped := out.String()
	for _, want := range []string{
		"--- namespace tenant-a ---",
		"--- actionsgateway status in tenant-a ---",
		"--- networkpolicies in tenant-a ---",
		"--- pod descriptions in tenant-a ---",
		"--- pod/proxy-0 logs in tenant-a ---",
		"--- pod/agc-0 previous-container logs in tenant-a ---",
		"--- events in tenant-a ---",
		"--- manager networkpolicies in gmc-system ---",
		"--- manager logs in gmc-system ---",
	} {
		if !strings.Contains(dumped, want) {
			t.Errorf("dump is missing %q:\n%s", want, dumped)
		}
	}
}

// The dump runs against live tenant namespaces, so a command that reads a
// Secret would put credential material into a CI artifact. Credentials reach a
// tenant pod as volume mounts, which describe renders as a name.
func TestDumpProvisioningDiagnosticsNeverReadsASecret(t *testing.T) {
	fakeKubectlForNamespaces(t, "tenant-a")
	out := captureGinkgoWriter(t)

	DumpProvisioningDiagnostics("gmc-system", "gmc-controller-manager", "tenant-a")

	// Run echoes every command it invokes as `running: "..."`.
	for _, line := range strings.Split(out.String(), "\n") {
		if !strings.HasPrefix(line, "running: ") {
			continue
		}
		for _, banned := range []string{" secret", " secrets"} {
			if strings.Contains(line, banned) {
				t.Errorf("dump reads a Secret, which would leak credential material into CI output: %s", line)
			}
		}
	}
}

// Callers pass every namespace their suite may create; an early failure leaves
// most of them absent, and a screen of identical "unavailable" lines per
// namespace buries the one that has the evidence.
func TestDumpProvisioningDiagnosticsSkipsAbsentNamespaces(t *testing.T) {
	fakeKubectlForNamespaces(t, "gmc-np-webhook")
	out := captureGinkgoWriter(t)

	DumpProvisioningDiagnostics("gmc-system", "gmc-controller-manager",
		"gmc-np-metrics-denied", "gmc-np-metrics-allowed", "gmc-np-webhook")

	dumped := out.String()
	if !strings.Contains(dumped, "--- namespace gmc-np-metrics-denied: not readable") {
		t.Errorf("absent namespace was not reported as skipped:\n%s", dumped)
	}
	if strings.Contains(dumped, "--- pod descriptions in gmc-np-metrics-denied ---") {
		t.Errorf("absent namespace was still dumped in full:\n%s", dumped)
	}
	if !strings.Contains(dumped, "--- pod descriptions in gmc-np-webhook ---") {
		t.Errorf("the namespace that exists was not dumped:\n%s", dumped)
	}
}

// The workload policy's GitHub meta ranges are 98.7% of its YAML, so the dump
// samples them. What survives must still answer the questions the NetworkPolicy
// specs ask: which policy, matching which pods, on which ports.
func TestSampleCIDRsKeepsPolicyShapeAndCountsTheRest(t *testing.T) {
	var doc strings.Builder
	doc.WriteString("- apiVersion: networking.k8s.io/v1\n  kind: NetworkPolicy\n" +
		"  metadata:\n    name: actions-gateway-workload\n  spec:\n" +
		"    podSelector:\n      matchLabels:\n        actions-gateway/role: workload\n" +
		"    policyTypes:\n    - Egress\n    egress:\n    - to:\n")
	for i := 0; i < 300; i++ {
		fmt.Fprintf(&doc, "      - ipBlock:\n          cidr: 10.%d.0.0/16\n", i)
	}
	doc.WriteString("      ports:\n      - port: 443\n        protocol: TCP\n")

	got := sampleCIDRs(doc.String())

	if n := strings.Count(got, "cidr:"); n != networkPolicyCIDRSample {
		t.Errorf("kept %d cidr entries, want %d", n, networkPolicyCIDRSample)
	}
	if n := strings.Count(got, "- ipBlock:"); n != networkPolicyCIDRSample {
		t.Errorf("kept %d ipBlock lines, want %d — an ipBlock must never outlive its cidr", n, networkPolicyCIDRSample)
	}
	if !strings.Contains(got, "# 295 further ipBlock cidr entries elided") {
		t.Errorf("elision is not accounted for:\n%s", got)
	}
	for _, want := range []string{
		"name: actions-gateway-workload",
		"actions-gateway/role: workload",
		"- Egress",
		"- port: 443",
		"cidr: 10.0.0.0/16",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("sampling dropped %q, which the NetworkPolicy specs assert on:\n%s", want, got)
		}
	}
}

// A policy with few ranges is reproduced whole — no count line, nothing dropped.
func TestSampleCIDRsLeavesASmallPolicyIntact(t *testing.T) {
	doc := "  spec:\n    egress:\n    - to:\n      - ipBlock:\n          cidr: 10.0.0.0/8\n"

	got := sampleCIDRs(doc)

	if got != doc {
		t.Errorf("a policy under the sample size was rewritten:\ngot:\n%s\nwant:\n%s", got, doc)
	}
}

// A superseded ReplicaSet's pod template is the only record of what a mid-run
// AGC roll changed (Q593): its pods are gone, so no pod-scoped dump holds it.
func TestDumpAGCSessionDiagnosticsCapturesSupersededReplicaSetTemplate(t *testing.T) {
	fakeKubectl(t, supersededRSDescription)
	out := captureGinkgoWriter(t)

	DumpAGCSessionDiagnostics("ssrec", "ssrec-agc", "gateway-system", "fakegithub")

	dumped := out.String()
	for _, want := range []string{
		"--- replicaset pod templates in ssrec ---",
		"deployment.kubernetes.io/revision: 1",
		"Pod Template:",
		"ghcr.io/example/agc:v0.9.0",
	} {
		if !strings.Contains(dumped, want) {
			t.Errorf("dump is missing %q from the superseded ReplicaSet's template:\n%s", want, dumped)
		}
	}
}

// The revisions table orders a roll absolutely; describe alone gives no
// creation timestamp.
func TestDumpAGCSessionDiagnosticsRequestsReplicaSetRevisionOrdering(t *testing.T) {
	fakeKubectl(t, supersededRSDescription)
	out := captureGinkgoWriter(t)

	DumpAGCSessionDiagnostics("ssrec", "ssrec-agc", "gateway-system", "fakegithub")

	dumped := out.String()
	if !strings.Contains(dumped, "--- replicaset revisions in ssrec ---") {
		t.Errorf("dump is missing the replicaset revisions section:\n%s", dumped)
	}
	if !strings.Contains(dumped, "CREATED:.metadata.creationTimestamp") {
		t.Errorf("revisions table does not request a creation timestamp:\n%s", dumped)
	}
}
