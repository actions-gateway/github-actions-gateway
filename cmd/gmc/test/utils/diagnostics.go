package utils

import (
	"fmt"
	"os/exec"
	"strings"

	. "github.com/onsi/ginkgo/v2" //nolint:revive,staticcheck
)

// DumpAGCSessionDiagnostics writes best-effort cluster state to the Ginkgo
// output for debugging an AGC broker-session-registration failure — the
// "no session registered for this RunnerGroup yet" / "no worker pod scheduled
// yet" e2e timeouts (Q134/Q135).
//
// The session-registration specs poll fakegithub (the source of truth) for a
// session but capture nothing AGC-side on timeout, so a recurrence in CI gives
// no hint whether the AGC listener never started, started but failed
// createSession / the OAuth token exchange and is backing off, or hit a
// non-retriable exit that nothing revived (Q137). This dumps the signals that
// distinguish those: the RunnerGroup status (activeSessions, conditions,
// observedGeneration), the AGC pod logs (where the listener logs broker-call
// errors), pod/Deployment descriptions (scheduling + image-pull events), every
// ReplicaSet's pod template (the only surviving record of a revision a mid-run
// roll superseded — Q593), the namespace event stream, and fakegithub's own
// logs/description (to spot a contended or restarting single-replica broker
// under parallel CI load).
//
// It is best-effort: every command failure is logged and skipped, never
// propagated, so calling it from a failure-gated AfterEach cannot mask the
// original failure. Call it only when the spec has already failed.
func DumpAGCSessionDiagnostics(tenantNS, agcDeployment, infraNS, fakegithubDeployment string) {
	_, _ = fmt.Fprintf(GinkgoWriter,
		"\n===== AGC session-registration diagnostics (tenant=%s) =====\n", tenantNS)

	dumpCommand("workloads in "+tenantNS,
		"kubectl", "get", "all", "-n", tenantNS)
	dumpCommand("runnergroup status in "+tenantNS,
		"kubectl", "get", "runnergroup", "-n", tenantNS, "-o", "yaml")
	dumpCommand("pod descriptions in "+tenantNS,
		"kubectl", "describe", "pods", "-n", tenantNS)
	// A roll scales the superseded ReplicaSet to zero, so its pods — and the
	// template they ran — are gone from every pod-scoped dump above; the
	// ReplicaSet keeps the template. The table orders the revisions absolutely
	// and carries the image, the field a roll most often changes.
	dumpCommand("replicaset revisions in "+tenantNS,
		"kubectl", "get", "replicasets", "-n", tenantNS, "--sort-by=.metadata.creationTimestamp",
		"-o", "custom-columns=NAME:.metadata.name,CREATED:.metadata.creationTimestamp,"+
			"DESIRED:.spec.replicas,READY:.status.readyReplicas,IMAGES:.spec.template.spec.containers[*].image")
	// describe over `-o yaml`: it renders the template and the revision
	// annotation without the status and metadata bulk, and prints a
	// Secret-sourced env var as a reference rather than its value.
	dumpCommand("replicaset pod templates in "+tenantNS,
		"kubectl", "describe", "replicasets", "-n", tenantNS)
	// --tail is generous: the session tenants run their AGC at debug (so this dump
	// captures the listener's per-session/job/recycle trail — Q148), which is far
	// more verbose than info, and the trail must not scroll out behind it.
	dumpCommand("AGC logs in "+tenantNS,
		"kubectl", "logs", "deploy/"+agcDeployment, "-n", tenantNS, "--tail=2000", "--all-containers")
	// --previous surfaces a crash-looped AGC's prior logs; absent on first boot.
	dumpCommand("AGC previous-container logs in "+tenantNS,
		"kubectl", "logs", "deploy/"+agcDeployment, "-n", tenantNS, "--tail=300", "--all-containers", "--previous")
	dumpCommand("events in "+tenantNS,
		"kubectl", "get", "events", "-n", tenantNS, "--sort-by=.lastTimestamp")

	// fakegithub is shared and single-replica; a contended or restarting broker
	// is the leading hypothesis for slow/failed session registration under load.
	dumpCommand("fakegithub description in "+infraNS,
		"kubectl", "describe", "deploy/"+fakegithubDeployment, "-n", infraNS)
	dumpCommand("fakegithub logs in "+infraNS,
		"kubectl", "logs", "deploy/"+fakegithubDeployment, "-n", infraNS, "--tail=300")

	_, _ = fmt.Fprintf(GinkgoWriter,
		"===== end AGC session-registration diagnostics (tenant=%s) =====\n\n", tenantNS)
}

// DumpProvisioningDiagnostics writes best-effort cluster state to the Ginkgo
// output for a GMC provisioning-or-policy failure in the given namespaces — the
// multi-minute WaitForDeploymentReady, resource-existence and NetworkPolicy-
// enforcement timeouts. It is the GMC-side counterpart to
// DumpAGCSessionDiagnostics, which covers broker-session registration instead.
//
// The suites that call it delete their namespace in teardown, so a timeout used
// to leave nothing behind (Q666). Per namespace it captures the signals that
// separate the failure modes: the namespace object (the pod-security and
// selector labels several specs assert on), the workload inventory, the
// ActionsGateway status (conditions and observedGeneration separate "GMC never
// reconciled" from "reconciled and failed"), the NetworkPolicies (asserted for
// existence and for egress shape, so the rule set is what attributes a blocked
// connection to policy rather than to a dead endpoint), pod descriptions
// (scheduling, image-pull, probe and pod-security rejections), each pod's log
// tail and its previous container's, and the event stream. It then adds the
// shared manager's ingress NetworkPolicies — a regression there blocks
// admission for every tenant at once (Q83), so it presents as nothing
// provisioning anywhere — and the manager's log tail, the only record of a
// reconcile that never ran.
//
// Nothing here reads a Secret. Credentials reach a tenant pod as Secret volume
// mounts rather than env vars (E2E_GMC_AGCNoCredentialEnvVars asserts that), and
// describe renders a volume as its Secret name; the ActionsGateway spec carries
// a SecretReference, also a name.
//
// It is best-effort: every command failure is logged and skipped, never
// propagated, so calling it from a failure-gated AfterEach cannot mask the
// original failure. Call it only when the spec has already failed.
func DumpProvisioningDiagnostics(managerNS, managerDeployment string, namespaces ...string) {
	scope := strings.Join(namespaces, ",")
	_, _ = fmt.Fprintf(GinkgoWriter,
		"\n===== GMC provisioning diagnostics (namespaces=%s) =====\n", scope)

	for _, ns := range namespaces {
		// Callers pass every namespace a suite may have created; the ones a
		// focused or early-failing run never reached would otherwise emit a
		// screen of identical "unavailable" lines.
		if _, err := Run(exec.Command("kubectl", "get", "namespace", ns)); err != nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "--- namespace %s: not readable (%v), skipping ---\n", ns, err)
			continue
		}
		dumpCommand("namespace "+ns,
			"kubectl", "get", "namespace", ns, "-o", "yaml")
		dumpCommand("workloads in "+ns,
			"kubectl", "get", "all", "-n", ns)
		dumpCommand("actionsgateway status in "+ns,
			"kubectl", "get", "actionsgateways.actions-gateway.github.com", "-n", ns, "-o", "yaml")
		dumpCommand("networkpolicies in "+ns,
			"kubectl", "get", "networkpolicy", "-n", ns, "-o", "yaml")
		// describe over `-o yaml`: it renders the scheduling and probe events
		// without the status bulk, and prints a Secret-backed volume or env var
		// as a reference rather than its value.
		dumpCommand("pod descriptions in "+ns,
			"kubectl", "describe", "pods", "-n", ns)
		dumpPodLogs(ns)
		dumpCommand("events in "+ns,
			"kubectl", "get", "events", "-n", ns, "--sort-by=.lastTimestamp")
	}

	dumpCommand("manager networkpolicies in "+managerNS,
		"kubectl", "get", "networkpolicy", "-n", managerNS, "-o", "yaml")
	// No --previous counterpart: a restarted manager re-reconciles, so the
	// current tail already carries the retry.
	dumpCommand("manager logs in "+managerNS,
		"kubectl", "logs", "deploy/"+managerDeployment, "-n", managerNS, "--tail=400", "--all-containers")

	_, _ = fmt.Fprintf(GinkgoWriter,
		"===== end GMC provisioning diagnostics (namespaces=%s) =====\n\n", scope)
}

// dumpPodLogs writes a bounded log tail for every pod in ns. The probe pods the
// NetworkPolicy specs drive carry their verdict in their logs, and a crash
// looping proxy or AGC records its fatal only in the previous container.
func dumpPodLogs(ns string) {
	out, err := Run(exec.Command("kubectl", "get", "pods", "-n", ns, "-o", "name"))
	if err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "--- pod logs in %s: unavailable (%v) ---\n", ns, err)
		return
	}
	for _, pod := range strings.Fields(out) {
		dumpCommand(pod+" logs in "+ns,
			"kubectl", "logs", pod, "-n", ns, "--tail=150", "--all-containers", "--prefix")
		dumpCommand(pod+" previous-container logs in "+ns,
			"kubectl", "logs", pod, "-n", ns, "--tail=60", "--all-containers", "--prefix", "--previous")
	}
}

// dumpCommand runs a diagnostic command and writes its labeled output to the
// Ginkgo output. A non-zero exit is reported inline rather than failing the
// caller — diagnostics must never mask the real test failure.
func dumpCommand(label, name string, args ...string) {
	// G204: name/args are fixed diagnostic commands defined in this file, not
	// external input — this is e2e test scaffolding.
	out, err := Run(exec.Command(name, args...)) //nolint:gosec
	if err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "--- %s: unavailable (%v) ---\n", label, err)
		return
	}
	_, _ = fmt.Fprintf(GinkgoWriter, "--- %s ---\n%s\n", label, out)
}
