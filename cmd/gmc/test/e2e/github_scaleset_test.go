//go:build e2e
// +build e2e

package e2e

import (
	"fmt"
	"os/exec"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/actions-gateway/github-actions-gateway/gmc/test/utils"
)

// Fixtures for the scale-set half of the Q396 eviction measurement. The spec itself
// lives in the E2E_GitHub_RealDispatch Ordered container in github_e2e_test.go, because
// every live-GitHub spec must: the suite runs --procs 6, so a second top-level container
// would run concurrently with that one and put two live gateways on one fixture repo,
// which is the Q511 collision inside a single process.
const (
	// scaleSetTenantNS holds the whole v2 object set. Separate from the classic tenant's
	// namespace so the two gateways cannot see each other's workers, Secrets, or quota.
	scaleSetTenantNS = "tenant-github-scaleset"

	// scaleSetLabel is the RunnerSet's single runs-on label, which on this protocol IS
	// the scale set's name, which in turn is the prefix of every runner it registers
	// (listener.runnerName: "<scaleSetName>-<jobID>").
	//
	// It therefore has to extend the classic gateway's name. suiteRunnerPrefixes
	// identifies everything this suite registered by the "<agName>-" prefix, and the
	// preflight and `make e2e-github-cleanup` both work off that: a label chosen
	// independently would register runners on the shared fixture repo that nothing knows
	// to sweep. Same reasoning as the Q422 sibling gateway's name.
	//
	// agName is scoped to the Describe closure and cannot be referenced here, so the spec
	// asserts the prefix relation rather than this const expressing it.
	scaleSetLabel = "real-ag-ss"

	scaleSetGatewayName = scaleSetLabel
	scaleSetAGCDeploy   = scaleSetGatewayName + "-agc"
	scaleSetName        = "set-ss"
	scaleSetTemplate    = "tmpl-ss"
	scaleSetSecretName  = "scaleset-github-app-secret" //nolint:gosec // G101: the NAME of a Kubernetes Secret object, not a credential value.
)

// scaleSetLiveManifest is the v2 object set for the scale-set tenant, pointed at real
// GitHub.
//
// githubURL names the fixture REPO rather than the org, which is what scopes the scale
// set to it. That is not a stylistic choice: this repo is public and the org's Default
// runner group sets allows_public_repositories: false, so an org-scoped scale set never
// receives the job (the same constraint the cmd/probe scale-set investigations run
// under).
//
// No defaultProxyRef and no proxyRef — the tenant runs on direct egress. Worker traffic
// still leaves under the GitHub-CIDR NetworkPolicy the GMC programs from its live /meta
// fetch; E2E_V2_DirectEgress proves that path reaches real GitHub on this cluster. A
// proxy pool would add an HPA, an anti-affinity-pinned node, and a second failure mode
// to a measurement that is about eviction recovery.
//
// The ephemeral-storage limit is the eviction lever, sized and justified exactly as the
// classic tenant's is (utils.WithEphemeralStorageLimit). CPU and memory are declared
// alongside it because the provisioner's resource defaulting is gap-fill and
// all-or-nothing: a container naming any resource keeps its values verbatim and gets no
// defaults, so naming only ephemeral storage would ship an unbounded worker.
func scaleSetLiveManifest(ns, orgURL, secretName, workerImage, storageLimit string) string {
	return fmt.Sprintf(`apiVersion: actions-gateway.com/v2alpha1
kind: ActionsGateway
metadata:
  name: %[6]s
  namespace: %[1]s
spec:
  githubURL: %[4]s
  credentials:
    type: GitHubApp
    githubApp:
      name: %[2]s
  logLevel: debug
---
apiVersion: actions-gateway.com/v2alpha1
kind: RunnerTemplate
metadata:
  name: %[8]s
  namespace: %[1]s
spec:
  workerImage: %[3]s
  podTemplate:
    spec:
      containers:
      - name: runner
        image: %[3]s
        resources:
          requests:
            cpu: 500m
            memory: 1Gi
            ephemeral-storage: %[5]s
          limits:
            cpu: 500m
            memory: 1Gi
            ephemeral-storage: %[5]s
---
apiVersion: actions-gateway.com/v2alpha1
kind: RunnerSet
metadata:
  name: %[7]s
  namespace: %[1]s
spec:
  gatewayRef:
    name: %[6]s
  templateRef:
    name: %[8]s
  acquisitionProtocol: ScaleSet
  runnerLabels: ["%[9]s"]
`, ns, secretName, workerImage, orgURL, storageLimit,
		scaleSetGatewayName, scaleSetName, scaleSetTemplate, scaleSetLabel)
}

// runnerSetCondition returns one condition's status and reason off a RunnerSet, both ""
// when the condition is absent.
func runnerSetCondition(g Gomega, ns, name, condType string) (status, reason string) {
	out, err := utils.Run(exec.Command("kubectl", "get", "runnerset", name,
		"-n", ns, "-o", fmt.Sprintf(
			`jsonpath={.status.conditions[?(@.type=="%s")].status} {.status.conditions[?(@.type=="%s")].reason}`,
			condType, condType)))
	g.Expect(err).NotTo(HaveOccurred(), "read %s condition on runnerset %s/%s", condType, ns, name)
	fields := strings.Fields(out)
	if len(fields) < 2 {
		return "", ""
	}
	return fields[0], fields[1]
}

// runnerSetConditionDump renders every condition on a RunnerSet, for a failure message
// that has to say why a listener never came up rather than just that it did not.
func runnerSetConditionDump(ns, name string) string {
	GinkgoHelper()
	out, err := utils.Run(exec.Command("kubectl", "get", "runnerset", name,
		"-n", ns, "-o", `jsonpath={range .status.conditions[*]}{.type}={.status}/{.reason}: {.message}{"\n"}{end}`))
	if err != nil {
		return fmt.Sprintf("(could not read conditions: %v)", err)
	}
	if strings.TrimSpace(out) == "" {
		return "(no conditions published)"
	}
	return out
}

// scaleSetWorkerForRun returns the Running scale-set worker pod provisioned for a run.
//
// Unlike the classic path there is no snapshot fallback and none is wanted: the
// assignment message is the only place this tier ever learns the run identity, so a
// worker without the annotation is a worker eviction recovery cannot act on at all
// (Q417). Resolving it by any other means would hide exactly the defect that matters.
func scaleSetWorkerForRun(g Gomega, ns, runID string) (podName, diag string) {
	out, err := utils.Run(exec.Command("kubectl", "get", "pods",
		"-n", ns,
		"-l", "app.kubernetes.io/managed-by=actions-gateway-controller",
		"--field-selector", "status.phase=Running",
		"-o", fmt.Sprintf(
			`jsonpath={.items[?(@.metadata.annotations.actions-gateway\.com/run-id=="%s")].metadata.name}`, runID),
	))
	g.Expect(err).NotTo(HaveOccurred())
	if fields := strings.Fields(out); len(fields) > 0 {
		return fields[0], ""
	}

	all, aerr := utils.Run(exec.Command("kubectl", "get", "pods", "-n", ns,
		"-l", "app.kubernetes.io/managed-by=actions-gateway-controller",
		"-o", `jsonpath={range .items[*]}{.metadata.name}={.status.phase}/run-id={.metadata.annotations.actions-gateway\.com/run-id} {end}`))
	g.Expect(aerr).NotTo(HaveOccurred())
	return "", fmt.Sprintf("no Running worker carries run-id=%s; workers in %s: %s", runID, ns, strings.TrimSpace(all))
}

// deleteScaleSetTenant tears the v2 object set down in dependency order.
//
// The RunnerSet goes first and is waited on: its agentpool-cleanup finalizer is cleared
// by the AGC, which lives in this namespace, so a bare namespace delete races the AGC
// pod's own termination and a lost race wedges the namespace in Terminating on a
// finalizer whose controller is already gone. Deregistration also needs a live token, so
// the gateway — and with it the AGC's NetworkPolicies — must outlive the set.
//
// Best-effort throughout: this runs in an AfterAll that must also cope with a spec that
// failed before creating any of it.
func deleteScaleSetTenant() {
	_, _ = utils.Run(exec.Command("kubectl", "delete", "runnerset", scaleSetName,
		"-n", scaleSetTenantNS, "--ignore-not-found", "--timeout=2m"))
	_, _ = utils.Run(exec.Command("kubectl", "delete", "actionsgateways.actions-gateway.com",
		scaleSetGatewayName, "-n", scaleSetTenantNS, "--ignore-not-found", "--timeout=2m"))
	_, _ = utils.Run(exec.Command("kubectl", "delete", "clusterrolebinding",
		"agc-clusterrunnertemplate-reader."+scaleSetTenantNS+"."+scaleSetGatewayName, "--ignore-not-found"))
	_, _ = utils.Run(exec.Command("kubectl", "delete", "namespace", scaleSetTenantNS,
		"--ignore-not-found", "--timeout=3m"))
}
