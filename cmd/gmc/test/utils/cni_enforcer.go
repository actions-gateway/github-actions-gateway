package utils

import (
	"fmt"
	"os/exec"
	"sort"
	"strings"

	. "github.com/onsi/ginkgo/v2" //nolint:revive,staticcheck
)

// cniEnforcerSelectors are the kube-system label selectors for the agent that
// enforces NetworkPolicy on each e2e lane: kindnetd (which embeds
// kube-network-policies) and calico-node.
var cniEnforcerSelectors = []string{"app=kindnet", "k8s-app=calico-node"}

// CNIEnforcerGeneration returns a fingerprint of the NetworkPolicy enforcer
// pods running in kube-system: pod name, restart count and the current
// container's start time, one entry per pod, sorted.
//
// A spec that observes cross-tenant traffic being allowed compares this across
// its own probe window. On the kindnet lane the answer is only meaningful while
// kindnetd is alive: kindnetd runs kube-network-policies with FailOpen, which
// puts `queue flags bypass` on its nftables rules, so with no process bound to
// the nfqueue every packet is accepted and NetworkPolicy is not enforced at all
// (Q747 — measured: an enforcer kill produced 664 accepted cross-tenant
// connections in ~1 s, and enforcement returned when it restarted). A changed
// fingerprint therefore means the observation says nothing about the policy.
//
// Calico programs its rules into the kernel, so they survive a calico-node
// restart and the fingerprint is informational there rather than
// disqualifying.
//
// Best-effort: an unreadable selector contributes nothing, and a cluster whose
// enforcer matches no selector yields "" — callers must treat the empty string
// as "no evidence" and not as "nothing restarted".
func CNIEnforcerGeneration() string {
	const jsonPath = "jsonpath={range .items[*]}" +
		"{.metadata.name}/restarts={.status.containerStatuses[0].restartCount}" +
		"/started={.status.containerStatuses[0].state.running.startedAt}\n{end}"

	var entries []string
	for _, selector := range cniEnforcerSelectors {
		out, err := Run(exec.Command("kubectl", "get", "pods",
			"-n", "kube-system", "-l", selector, "-o", jsonPath))
		if err != nil {
			continue
		}
		entries = append(entries, strings.Fields(out)...)
	}
	sort.Strings(entries)
	return strings.Join(entries, " ")
}

// DumpCNIEnforcerState writes the NetworkPolicy enforcer's state to the Ginkgo
// output: which pods are running where, why the last container died, and the
// resource pressure that kills them.
//
// The restart evidence is what attributes a spurious allow. Q747's dump carried
// the restart count but not the termination reason, so the crash-loop was
// visible and its cause was not; `lastState.terminated` is captured here for
// exactly that reason. memory.events is the leading suspect: kind ships
// kindnetd with a 50Mi limit and the e2e cluster ran it at 47-49Mi.
//
// Best-effort, like the rest of this file: call it only from a failure path.
func DumpCNIEnforcerState() {
	_, _ = fmt.Fprintf(GinkgoWriter, "\n===== CNI NetworkPolicy enforcer state =====\n")

	for _, selector := range cniEnforcerSelectors {
		out, err := Run(exec.Command("kubectl", "get", "pods",
			"-n", "kube-system", "-l", selector, "-o", "name"))
		if err != nil || strings.TrimSpace(out) == "" {
			continue
		}
		dumpCommand("enforcer pods ("+selector+")",
			"kubectl", "get", "pods", "-n", "kube-system", "-l", selector, "-o", "wide")
		// Custom columns rather than describe: the one field that names the
		// cause is lastState.terminated.reason, and describe buries it under a
		// screen of mounts and tolerations for every pod on every node.
		dumpCommand("enforcer restart attribution ("+selector+")",
			"kubectl", "get", "pods", "-n", "kube-system", "-l", selector,
			"-o", "custom-columns=NAME:.metadata.name,NODE:.spec.nodeName,"+
				"RESTARTS:.status.containerStatuses[0].restartCount,"+
				"STARTED:.status.containerStatuses[0].state.running.startedAt,"+
				"LAST_REASON:.status.containerStatuses[0].lastState.terminated.reason,"+
				"LAST_EXIT:.status.containerStatuses[0].lastState.terminated.exitCode,"+
				"LAST_FINISHED:.status.containerStatuses[0].lastState.terminated.finishedAt")
		dumpCommand("enforcer events ("+selector+")",
			"kubectl", "get", "events", "-n", "kube-system",
			"--field-selector=involvedObject.kind=Pod", "--sort-by=.lastTimestamp")
		for _, pod := range strings.Fields(out) {
			dumpCommand(pod+" previous-container logs",
				"kubectl", "logs", pod, "-n", "kube-system", "--tail=60", "--previous")
		}
	}

	_, _ = fmt.Fprintf(GinkgoWriter, "===== end CNI NetworkPolicy enforcer state =====\n\n")
}
