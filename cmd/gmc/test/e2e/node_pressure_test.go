//go:build e2e
// +build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/actions-gateway/github-actions-gateway/gmc/test/utils"
)

// The node-pressure eviction lever (Q1177). The live eviction specs need a genuine
// kubelet node-pressure kill of one chosen worker: the ungraceful SIGKILL whose
// conclusion latency they measure, and the shape the eviction arm re-runs. The pod-level
// ephemeral-storage overshoot they used before is an own-limit eviction, which recovery
// now withholds.
//
// The lever raises the kubelet's memory.available hard threshold on the worker's node to
// just under what that node has free, so the spec's memory fill is what crosses it. It
// is computed from the node's own stats/summary at the moment of use, because a fixed
// threshold would be right on one host and wrong on the next: a kind node's capacity is
// the Docker host's memory, while its working set is only its own containers'. Measured
// on kind v1.35.5 with a 1GiB fill: the kubelet evicted the filling pod 17s after the
// fill began, exit 137, DisruptionTarget=True/TerminationByKubelet.
//
// The kubelet ranks pods using more than they requested first, by how far over, so the
// worker's low request (utils.NodePressureWorkerResources) is what makes it the victim.
// kind sets evictionPressureTransitionPeriod to 0s, so MemoryPressure clears as soon as
// the fill is gone.
const (
	// nodePressureFillMiB is what the worker is made to hold.
	nodePressureFillMiB = 1024
	// nodePressureMarginMiB is how far under the node's free memory the threshold sits:
	// half the fill, so drift between measuring and filling cannot cross it early, and
	// the fill crosses it by the same margin.
	nodePressureMarginMiB = nodePressureFillMiB / 2

	kubeletConfigPath       = "/var/lib/kubelet/config.yaml"
	kubeletConfigBackupPath = kubeletConfigPath + ".q1177-orig"
)

// nodePressureEvictionPrefix opens the message the kubelet writes for a memory-pressure
// eviction (nodeLowMessageFmt, pkg/kubelet/eviction/helpers.go at v1.35.5).
const nodePressureEvictionPrefix = "The node was low on resource: memory."

// armNodeMemoryPressure raises the memory threshold on the node running the worker and
// returns its idempotent restore, which is also registered with DeferCleanup. Call the
// restore as soon as the eviction is observed, so the node is back to normal before the
// re-run's worker needs scheduling.
func armNodeMemoryPressure(prefix, ns, podName string) (restore func()) {
	GinkgoHelper()
	node := podNodeName(ns, podName)
	threshold, restore := applyNodeMemoryPressureThreshold(node)
	AddReportEntry(prefix+" node-pressure lever",
		fmt.Sprintf("node %s, memory.available hard threshold %dMi, fill %dMi", node, threshold, nodePressureFillMiB))
	return restore
}

// podNodeName returns the node a pod is bound to.
func podNodeName(ns, name string) string {
	GinkgoHelper()
	out, err := utils.Run(exec.Command("kubectl", "get", "pod", name, "-n", ns,
		"-o", "jsonpath={.spec.nodeName}"))
	Expect(err).NotTo(HaveOccurred())
	node := strings.TrimSpace(out)
	Expect(node).NotTo(BeEmpty(), "pod %s/%s is not bound to a node", ns, name)
	return node
}

// nodeMemoryAvailableMiB is the kubelet's own memory.available reading for node, the
// value its hard threshold is compared against.
func nodeMemoryAvailableMiB(node string) int64 {
	GinkgoHelper()
	out, err := utils.Run(exec.Command("kubectl", "get", "--raw",
		fmt.Sprintf("/api/v1/nodes/%s/proxy/stats/summary", node)))
	Expect(err).NotTo(HaveOccurred())
	var summary struct {
		Node struct {
			Memory struct {
				AvailableBytes *int64 `json:"availableBytes"`
			} `json:"memory"`
		} `json:"node"`
	}
	Expect(json.Unmarshal([]byte(out), &summary)).To(Succeed())
	Expect(summary.Node.Memory.AvailableBytes).NotTo(BeNil(), "node %s reports no memory.availableBytes", node)
	return *summary.Node.Memory.AvailableBytes / (1 << 20)
}

// kindNodeExec runs script in the kind node container named node. A kind node's
// container is named for the node, which the premise check in
// applyNodeMemoryPressureThreshold confirms before anything is changed.
func kindNodeExec(node, script string) (string, error) {
	return utils.Run(exec.Command("docker", "exec", node, "sh", "-c", script)) //nolint:gosec // G204: node is the API server's node name and script is built from this file's constants.
}

// applyNodeMemoryPressureThreshold sets node's kubelet memory.available hard threshold
// just under its free memory and restarts the kubelet, returning the threshold set and
// an idempotent restore that puts the original config back. The restore is registered
// with DeferCleanup before the config is touched, so a failure partway through the patch
// still restores it; a run killed outright leaves the backup on the node, and the next
// call restores it first.
func applyNodeMemoryPressureThreshold(node string) (thresholdMiB int64, restore func()) {
	GinkgoHelper()
	restoreScript := fmt.Sprintf(
		"if [ -f %[2]s ]; then cp %[2]s %[1]s && rm %[2]s && systemctl restart kubelet; fi", kubeletConfigPath, kubeletConfigBackupPath)

	out, err := utils.Run(exec.Command("docker", "inspect", "-f", "{{.State.Running}}", node)) //nolint:gosec // G204: node is the API server's node name.
	Expect(err).NotTo(HaveOccurred(),
		"node %s has no container of that name; the node-pressure lever needs a kind cluster", node)
	Expect(strings.TrimSpace(out)).To(Equal("true"), "kind node container %s is not running", node)

	_, err = kindNodeExec(node, restoreScript)
	Expect(err).NotTo(HaveOccurred(), "restore a kubelet config a previous run left patched on %s", node)
	waitForKubeletHealthy(node)

	thresholdMiB = nodeMemoryAvailableMiB(node) - nodePressureMarginMiB
	Expect(thresholdMiB).To(BeNumerically(">", nodePressureFillMiB),
		"node %s has too little free memory for a %dMiB fill to be the pressure", node, nodePressureFillMiB)

	// kind's kubelet config carries an evictionHard block (its nodefs/imagefs entries),
	// so the threshold is inserted as its first entry; the grep is what proves it landed.
	entry := fmt.Sprintf("memory.available: %dMi", thresholdMiB)
	restore = sync.OnceFunc(func() {
		_, err := kindNodeExec(node, restoreScript)
		Expect(err).NotTo(HaveOccurred(), "restore node %s's kubelet config", node)
		waitForKubeletHealthy(node)
	})
	DeferCleanup(restore)
	_, err = kindNodeExec(node, fmt.Sprintf(
		`cp %[1]s %[2]s && sed -i 's/^evictionHard:$/evictionHard:\n  %[3]s/' %[1]s && grep -q '^  %[3]s$' %[1]s && systemctl restart kubelet`,
		kubeletConfigPath, kubeletConfigBackupPath, entry))
	Expect(err).NotTo(HaveOccurred(), "set %s on node %s", entry, node)
	waitForKubeletHealthy(node)

	return thresholdMiB, restore
}

// waitForKubeletHealthy waits for a restarted kubelet to answer healthz through the
// API server's node proxy, and for the API to see the node Ready. Ready alone is not
// enough: it is the last heartbeat's, so it reads True for a kubelet that is down.
func waitForKubeletHealthy(node string) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		out, err := utils.Run(exec.Command("kubectl", "get", "--raw",
			fmt.Sprintf("/api/v1/nodes/%s/proxy/healthz", node)))
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(strings.TrimSpace(out)).To(Equal("ok"))
		ready, err := utils.Run(exec.Command("kubectl", "get", "node", node,
			"-o", `jsonpath={.status.conditions[?(@.type=="Ready")].status}`))
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(strings.TrimSpace(ready)).To(Equal("True"))
	}, 2*time.Minute, 2*time.Second).Should(Succeed(), "kubelet on %s did not come back healthy", node)
}

// fillWorkerMemory makes the worker's runner container hold sizeMiB of memory in a
// detached shell, and returns what the exec reported. The fill is a shell variable
// rather than a file because nothing in a worker pod is memory-backed: its writable
// layer is disk, and charging disk would be the own-limit lever again.
func fillWorkerMemory(ns, podName string, sizeMiB int) string {
	GinkgoHelper()
	script := fmt.Sprintf(`nohup bash -c 'x=$(head -c %dm /dev/zero | tr "\0" a); sleep 3600' >/dev/null 2>&1 & echo started`, sizeMiB)
	out, err := utils.Run(exec.Command("kubectl", "exec", "-n", ns, podName, "-c", "runner", "--",
		"bash", "-c", script))
	Expect(err).NotTo(HaveOccurred(), "start a %dMiB memory fill in worker %s", sizeMiB, podName)
	return strings.TrimSpace(out)
}

// podMemoryLimit returns the runner container's memory limit, or "" when it declares
// none.
func podMemoryLimit(ns, name string) string {
	GinkgoHelper()
	out, err := utils.Run(exec.Command("kubectl", "get", "pod", name, "-n", ns,
		"-o", `jsonpath={.spec.containers[?(@.name=="runner")].resources.limits.memory}`))
	Expect(err).NotTo(HaveOccurred())
	return strings.TrimSpace(out)
}
