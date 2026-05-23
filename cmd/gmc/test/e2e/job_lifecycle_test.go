//go:build e2e
// +build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/karlkfi/github-actions-gateway/gmc/test/utils"
)

var _ = Describe("E2E_AGC_JobLifecycle", Ordered, func() {
	const (
		tenantNS   = "tenant-job-lifecycle"
		agName     = "test-ag"
		secretName = "github-app-secret"
	)

	BeforeAll(func() {
		utils.CreateNamespace(tenantNS, nil)
		utils.CreateGitHubAppSecret(tenantNS, secretName, 12345, 67890, testRSAKeyPEM)
		// Use a RunnerGroup so AGC has something to reconcile and can register
		// broker sessions with fakegithub. The worker image is the agc image
		// (already loaded into the cluster); it acts as a placeholder since
		// job-lifecycle tests only verify session registration and pod creation,
		// not that runner pods complete successfully.
		utils.ApplyActionsGatewayCRWithRunnerGroup(tenantNS, agName, secretName, agcImage)

		By("waiting for AGC to be ready")
		utils.WaitForDeploymentReady(tenantNS, "actions-gateway-agc", 4*time.Minute)
	})

	AfterAll(func() {
		utils.DeleteActionsGatewayCR(tenantNS, agName)
		utils.DeleteNamespace(tenantNS)
	})

	SetDefaultEventuallyTimeout(3 * time.Minute)
	SetDefaultEventuallyPollingInterval(5 * time.Second)

	It("E2E_AGC_SessionRegistered: AGC creates broker sessions after startup", func() {
		By("waiting for at least one active session to appear in fakegithub")
		Eventually(func(g Gomega) {
			sessions := fakegithubActiveSessions(g)
			g.Expect(sessions).NotTo(BeEmpty(), "no sessions registered yet")
		}, 3*time.Minute, 5*time.Second).Should(Succeed())
	})

	It("E2E_AGC_JobDelivered: enqueuing a job triggers worker pod creation", func() {
		By("getting the first active session ID")
		var sessionID string
		Eventually(func(g Gomega) {
			sessions := fakegithubActiveSessions(g)
			g.Expect(sessions).NotTo(BeEmpty())
			sessionID = sessions[0]
		}, 2*time.Minute, 5*time.Second).Should(Succeed())

		// run_service_url causes the listener to call /acquirejob on fakegithub,
		// which returns a unique planId ("plan-N") so each job gets a distinct pod name.
		fakegithubSvcURL := fmt.Sprintf("http://%s.%s.svc.cluster.local:%s",
			fakegithubServiceName, infraNamespace, fakegithubServicePort)

		By("enqueuing a job for that session")
		fakegithubEnqueueJob(sessionID, map[string]interface{}{
			"jobId":          "e2e-job-1",
			"jobName":        "e2e test job",
			"run_service_url": fakegithubSvcURL,
		})

		By("waiting for a worker pod to appear in the tenant namespace")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "pods",
				"-n", tenantNS,
				"-l", "app.kubernetes.io/managed-by=actions-gateway-agc",
				"-o", "jsonpath={.items[*].metadata.name}",
			)
			out, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(out)).NotTo(BeEmpty(), "no worker pod scheduled yet")
		}, 3*time.Minute, 5*time.Second).Should(Succeed())
	})

	It("E2E_AGC_MultipleJobsQueued: multiple jobs result in multiple worker pods", func() {
		By("getting active sessions")
		var sessions []string
		Eventually(func(g Gomega) {
			sessions = fakegithubActiveSessions(g)
			g.Expect(len(sessions)).To(BeNumerically(">=", 1))
		}, 2*time.Minute, 5*time.Second).Should(Succeed())

		// run_service_url causes /acquirejob to be called, yielding a unique planId
		// per job so each gets a distinct pod name even when queued to the same session.
		fakegithubSvcURL := fmt.Sprintf("http://%s.%s.svc.cluster.local:%s",
			fakegithubServiceName, infraNamespace, fakegithubServicePort)

		By("enqueuing two jobs")
		fakegithubEnqueueJob(sessions[0], map[string]interface{}{"jobId": "e2e-job-2a", "run_service_url": fakegithubSvcURL})
		if len(sessions) > 1 {
			fakegithubEnqueueJob(sessions[1], map[string]interface{}{"jobId": "e2e-job-2b", "run_service_url": fakegithubSvcURL})
		} else {
			fakegithubEnqueueJob(sessions[0], map[string]interface{}{"jobId": "e2e-job-2b", "run_service_url": fakegithubSvcURL})
		}

		By("waiting for at least 2 worker pods")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "pods",
				"-n", tenantNS,
				"-l", "app.kubernetes.io/managed-by=actions-gateway-agc",
				"--no-headers",
			)
			out, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			lines := utils.GetNonEmptyLines(out)
			g.Expect(len(lines)).To(BeNumerically(">=", 2),
				"expected ≥2 worker pods, got %d: %s", len(lines), out)
		}, 4*time.Minute, 5*time.Second).Should(Succeed())
	})
})

// fakegithubActiveSessions queries the fakegithub control API for active sessions.
// It forwards the HTTP request via kubectl port-forward so it works from outside the cluster.
func fakegithubActiveSessions(g Gomega) []string {
	out := fakegithubControlRequest(g, "GET", "/control/sessions", nil)
	var sessions []string
	err := json.Unmarshal([]byte(out), &sessions)
	g.Expect(err).NotTo(HaveOccurred(), "parse sessions response: %s", out)
	if sessions == nil {
		sessions = []string{}
	}
	return sessions
}

// fakegithubEnqueueJob enqueues a job for the given session via the fakegithub control API.
func fakegithubEnqueueJob(sessionID string, payload map[string]interface{}) {
	GinkgoHelper()
	body, _ := json.Marshal(payload)
	fakegithubControlRequest(nil, "POST",
		fmt.Sprintf("/control/enqueue?sessionId=%s", sessionID),
		bytes.NewReader(body),
	)
}

// fakegithubControlRequest executes an HTTP request against the fakegithub control API
// by using kubectl port-forward to reach the control port (9090).
func fakegithubControlRequest(g interface{ Expect(interface{}, ...interface{}) Assertion }, method, path string, body io.Reader) string {
	// Port-forward fakegithub control port to localhost.
	const localPort = "19090"
	pfCmd := exec.Command("kubectl", "port-forward",
		"-n", infraNamespace,
		"service/"+fakegithubServiceName,
		localPort+":9090",
	)
	err := pfCmd.Start()
	if g != nil {
		Expect(err).NotTo(HaveOccurred(), "start port-forward")
	}
	defer func() { _ = pfCmd.Process.Kill() }()

	// Give port-forward a moment to establish.
	time.Sleep(500 * time.Millisecond)

	url := "http://localhost:" + localPort + path
	req, err := http.NewRequest(method, url, body)
	if g != nil {
		Expect(err).NotTo(HaveOccurred())
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := http.DefaultClient.Do(req)
	if g != nil {
		Expect(err).NotTo(HaveOccurred(), "HTTP %s %s", method, path)
		defer resp.Body.Close()
		Expect(resp.StatusCode).To(BeNumerically("<", 300),
			"unexpected status %d for %s %s", resp.StatusCode, method, path)
	} else if err != nil {
		return ""
	} else {
		defer resp.Body.Close()
	}
	if resp == nil {
		return ""
	}
	data, _ := io.ReadAll(resp.Body)
	return strings.TrimSpace(string(data))
}
