//go:build e2e
// +build e2e

package e2e

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/actions-gateway/github-actions-gateway/gmc/test/utils"
)

var (
	fakegithubPFCmd     *exec.Cmd
	fakegithubLocalPort string
)

var _ = Describe("E2E_AGC_JobLifecycle", Ordered, func() {
	const (
		tenantNS   = "tenant-job-lifecycle"
		agName     = "test-ag"
		secretName = "github-app-secret" //nolint:gosec // G101: the NAME of a Kubernetes Secret object, not a credential value.
	)

	tenant := utils.RunnerTenant(tenantNS, agName, secretName, agcImage)
	agcName, rsName, scaleSet := tenant.AGCName(), tenant.RunnerSetName(0), tenant.ScaleSetName(0)

	BeforeAll(func() {
		utils.CreateNamespace(tenantNS, nil)
		utils.CreateGitHubAppSecret(tenantNS, secretName, 12345, 67890, testRSAKeyPEM)
		// The worker image is the agc image (already loaded into the cluster); it is a
		// placeholder, since this spec asserts pod creation, not that a runner runs.
		tenant.ApplyWithWebhookRetry()

		By("granting workload pods egress to fakegithub in e2e-infra")
		// The GMC-built workload NP restricts 8080 egress to proxy pods only
		// (production-correct shape). fakegithub lives in another namespace on 8080 and
		// is reached directly (NO_PROXY matches svc.cluster.local), so stamp an
		// additive NP that lets the AGC open its scale-set session there.
		utils.ApplyFakegithubEgressNetworkPolicy(tenantNS)

		By("waiting for AGC to be ready")
		utils.WaitForDeploymentReady(tenantNS, agcName, 4*time.Minute)

		By("waiting for the AGC to complete a full reconcile")
		// Deployment-ready only means the health server is up; gate on a reconciled
		// RunnerSet so the session wait starts from an operational AGC (Q134).
		utils.WaitForRunnerSetReconciled(tenantNS, 4*time.Minute)

		fakegithubPFCmd = startFakegithubPortForward(19090)
		waitForScaleSetSession(scaleSet)
	})

	// On failure, dump AGC + fakegithub state before AfterAll tears down the
	// namespace, so a "no worker pod" timeout is debuggable from CI logs rather than
	// a bare Eventually timeout.
	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			utils.DumpAGCSessionDiagnostics(tenantNS, agcName, infraNamespace, fakegithubServiceName)
		}
	})

	AfterAll(func() {
		if fakegithubPFCmd != nil && fakegithubPFCmd.Process != nil {
			_ = fakegithubPFCmd.Process.Kill()
		}
		utils.DeleteActionsGatewayCR(tenantNS, agName)
		utils.DeleteNamespace(tenantNS)
	})

	It("E2E_AGC_MultipleJobsQueued: each queued job gets its own worker pod", func() {
		// The session and the first job's pod are E2E_AGC_ScaleSetAcquisition's; what
		// this adds is two jobs on one scale set, each provisioned on its own. Each
		// assignment carries its own workflow run, so a pod per run is a pod per job.
		jobs := []scaleSetJob{scaleSetEnqueue(scaleSet), scaleSetEnqueue(scaleSet)}
		for _, job := range jobs {
			runID := fmt.Sprintf("%d", job.WorkflowRunID)
			By("waiting for the worker pod of run " + runID)
			Eventually(func(g Gomega) {
				g.Expect(scaleSetWorkerPodsWithRunID(g, tenantNS, rsName, runID)).To(HaveLen(1),
					"run %s must be provisioned exactly one worker pod", runID)
			}, 3*time.Minute, 2*time.Second).Should(Succeed())
		}
	})
})

// fakegithubHTTPSBase is the fakegithub Service's address as a gateway githubURL
// names it: https, at the port the Service serves plaintext on. The AGC re-points
// only a gateway naming this host:port at the stub, so a spec manifest that wants its
// listener up appends an org path to this.
func fakegithubHTTPSBase() string {
	return fmt.Sprintf("https://%s.%s.svc.cluster.local:%s",
		fakegithubServiceName, infraNamespace, fakegithubServicePort)
}

// startFakegithubPortForward opens a port-forward to the fakegithub control API on a
// per-process local port derived from base, records it in fakegithubLocalPort, and
// waits until the port answers. Any response proves the tunnel, so the probe asks for
// a path that needs no scale set to exist yet.
func startFakegithubPortForward(base int) *exec.Cmd {
	GinkgoHelper()
	By("starting port-forward to the fakegithub control API")
	fakegithubLocalPort = fmt.Sprintf("%d", base+GinkgoParallelProcess())
	cmd := exec.Command("kubectl", "port-forward",
		"-n", infraNamespace,
		"service/"+fakegithubServiceName,
		fakegithubLocalPort+":9090",
	)
	Expect(cmd.Start()).To(Succeed())
	Eventually(func() error {
		resp, err := http.Get("http://localhost:" + fakegithubLocalPort + "/control/scaleset/state")
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
		return nil
	}, 15*time.Second, 500*time.Millisecond).Should(Succeed())
	return cmd
}

// fakegithubControlRequest executes an HTTP request against the fakegithub control API
// using the persistent port-forward established in BeforeAll.
// It retries once after 100 ms to handle transient port-forward interruptions.
func fakegithubControlRequest(g interface {
	Expect(interface{}, ...interface{}) Assertion
}, method, path string, body []byte) string {
	url := "http://localhost:" + fakegithubLocalPort + path

	var (
		resp *http.Response
		err  error
	)
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			time.Sleep(100 * time.Millisecond)
		}
		var bodyReader io.Reader
		if body != nil {
			bodyReader = bytes.NewReader(body)
		}
		var req *http.Request
		req, err = http.NewRequest(method, url, bodyReader)
		if err != nil {
			continue
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err = http.DefaultClient.Do(req)
		if err == nil {
			break
		}
	}

	if g != nil {
		Expect(err).NotTo(HaveOccurred(), "HTTP %s %s", method, path)
		defer func() { _ = resp.Body.Close() }()
		Expect(resp.StatusCode).To(BeNumerically("<", 300),
			"unexpected status %d for %s %s", resp.StatusCode, method, path)
	} else if err != nil {
		return ""
	} else {
		defer func() { _ = resp.Body.Close() }()
	}
	if resp == nil {
		return ""
	}
	data, _ := io.ReadAll(resp.Body)
	return strings.TrimSpace(string(data))
}
