package utils

import (
	"fmt"
	"os/exec"

	. "github.com/onsi/ginkgo/v2" // nolint:revive,staticcheck
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
// errors), pod/Deployment descriptions (scheduling + image-pull events), the
// namespace event stream, and fakegithub's own logs/description (to spot a
// contended or restarting single-replica broker under parallel CI load).
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
