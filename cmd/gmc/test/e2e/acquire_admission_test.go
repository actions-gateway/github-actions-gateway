//go:build e2e
// +build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/actions-gateway/github-actions-gateway/gmc/test/utils"
)

// E2E_AGC_AcquireAdmissionControl validates, end-to-end on a real cluster, the
// GitHub-broker contract the Q59 pre-acquisition admission gate is built on —
// the assumption Q154 exists to confirm is true rather than merely source-read
// (the PR #59 lesson: treat ✅ source-read findings as unverified until run):
//
//   - (a) a job that is *acquired* and then held at the worker pod-capacity
//     ceiling is cancelled (owned by the runner, its unrenewed lock lapses), NOT
//     redelivered as a duplicate. This is why the gate must decide *before*
//     AcquireJob: a too-late capacity rejection cannot be undone.
//   - (b) a job the gate *skips* (declines to acquire for lack of capacity) is
//     left queued at GitHub and redelivered later, so skipping is safe — the job
//     is not lost.
//
// fakegithub models the broker. Its Q154 lease/redelivery mode (opt-in,
// owner-scoped) gives the two paths distinct, observable outcomes: an acquired
// job is consumed and never redelivered; a delivered-but-unacquired job is
// redelivered once its lease expires. The RunnerGroup pins worker pods Pending
// (unschedulable) so capacity is held deterministically and freed by deleting a
// pod — the InformerPodWaiter treats deletion as completion, releasing the slot.
var _ = Describe("E2E_AGC_AcquireAdmissionControl", Ordered, func() {
	const (
		tenantNS   = "tenant-acquire-admit"
		agName     = "acquire-admit-ag"
		secretName = "github-app-secret"
		// leaseMs is short so a skipped job is redelivered several times within a
		// tight assert window, but comfortably longer than the gap between a
		// delivery and the AcquireJob the gate issues when it admits.
		leaseMs = 1500
	)

	var (
		admitPFCmd *exec.Cmd
		rgName     string
		ownerScope = agName + "-"
	)

	fakegithubSvcURL := fmt.Sprintf("http://%s.%s.svc.cluster.local:%s",
		fakegithubServiceName, infraNamespace, fakegithubServicePort)

	// jobStats reads the Q154 lease state for a runner_request_id.
	type jobStats struct {
		Deliveries int  `json:"deliveries"`
		Leased     bool `json:"leased"`
		Acquired   bool `json:"acquired"`
	}
	getJobStats := func(g Gomega, reqID string) jobStats {
		out := fakegithubControlRequest(g, "GET", "/control/jobstats?requestId="+reqID, nil)
		var js jobStats
		g.Expect(json.Unmarshal([]byte(out), &js)).To(Succeed(), "parse jobstats: %s", out)
		return js
	}

	// enqueueJobWithReqID enqueues a job carrying an explicit runner_request_id so
	// the test can address it in /control/jobstats. The AGC sends this id back as
	// the AcquireJob jobMessageId, which is how fakegithub correlates the two.
	enqueueJobWithReqID := func(sessionID, reqID string) {
		fakegithubEnqueueJob(sessionID, map[string]interface{}{
			"jobId":             reqID,
			"runner_request_id": reqID,
			"run_service_url":   fakegithubSvcURL,
		})
	}

	liveSession := func(g Gomega) string {
		sessions := fakegithubActiveSessionsForOwner(g, ownerScope)
		g.Expect(sessions).NotTo(BeEmpty(), "no live session for this RunnerGroup")
		return sessions[0]
	}

	// otherLiveSession returns a live session other than exclude. The listener
	// that acquired the busy job is parked (blocked in its job handler on the
	// Pending worker pod) and no longer polls its session, so a job enqueued
	// there would never be delivered — the second job must target a still-polling
	// sibling session of the same owner.
	otherLiveSession := func(g Gomega, exclude string) string {
		for _, s := range fakegithubActiveSessionsForOwner(g, ownerScope) {
			if s != exclude {
				return s
			}
		}
		g.Expect(false).To(BeTrue(), "no live session other than %s yet", exclude)
		return ""
	}

	BeforeAll(func() {
		utils.CreateNamespace(tenantNS, nil)
		utils.CreateGitHubAppSecret(tenantNS, secretName, 12345, 67890, testRSAKeyPEM)
		utils.ApplyActionsGatewayCRWithWorkerCeiling(tenantNS, agName, secretName, agcImage, 1)

		By("granting workload pods egress to fakegithub in e2e-infra")
		utils.ApplyFakegithubEgressNetworkPolicy(tenantNS)

		By("waiting for AGC to be ready")
		utils.WaitForDeploymentReady(tenantNS, agcName, 4*time.Minute)

		By("waiting for the AGC to complete a full reconcile (token + agent registration)")
		utils.WaitForRunnerGroupReconciled(tenantNS, 4*time.Minute)

		By("capturing the RunnerGroup name for the ceiling label")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "runnergroup",
				"-n", tenantNS, "-o", "jsonpath={.items[0].metadata.name}")
			out, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			rgName = strings.TrimSpace(out)
			g.Expect(rgName).NotTo(BeEmpty(), "no RunnerGroup found")
		}, 2*time.Minute, 2*time.Second).Should(Succeed())

		By("starting persistent port-forward to fakegithub control API")
		fakegithubLocalPort = fmt.Sprintf("%d", 19590+GinkgoParallelProcess())
		admitPFCmd = exec.Command("kubectl", "port-forward",
			"-n", infraNamespace,
			"service/"+fakegithubServiceName,
			fakegithubLocalPort+":9090",
		)
		Expect(admitPFCmd.Start()).To(Succeed())
		Eventually(func() error {
			resp, err := http.Get("http://localhost:" + fakegithubLocalPort + "/control/sessions")
			if err != nil {
				return err
			}
			resp.Body.Close()
			return nil
		}, 15*time.Second, 500*time.Millisecond).Should(Succeed())

		By("enabling the Q154 lease/redelivery model scoped to this RunnerGroup")
		fakegithubControlRequest(Default, "POST",
			fmt.Sprintf("/control/redelivery?enabled=true&owner=%s&leaseMs=%d", ownerScope, leaseMs), nil)
	})

	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			utils.DumpAGCSessionDiagnostics(tenantNS, agcName, infraNamespace, fakegithubServiceName)
		}
		// Free capacity between specs: delete every pod carrying this group's
		// ceiling label (worker pods and the decoy), so each It starts with an
		// empty gate.
		cmd := exec.Command("kubectl", "delete", "pods",
			"-n", tenantNS,
			"-l", "actions-gateway/runner-group="+rgName,
			"--ignore-not-found", "--wait=false")
		_, _ = utils.Run(cmd)
	})

	AfterAll(func() {
		fakegithubControlRequest(nil, "POST", "/control/redelivery?enabled=false", nil)
		if admitPFCmd != nil && admitPFCmd.Process != nil {
			_ = admitPFCmd.Process.Kill()
		}
		utils.DeleteActionsGatewayCR(tenantNS, agName)
		utils.DeleteNamespace(tenantNS)
	})

	SetDefaultEventuallyTimeout(3 * time.Minute)
	SetDefaultEventuallyPollingInterval(2 * time.Second)

	It("E2E_AGC_CeilingHeldJobIsCancelledNotRedelivered: a ceiling-held acquired job is not redelivered", func() {
		// Pre-create a Pending decoy pod carrying the group's ceiling label. It
		// counts toward the maxWorkers=1 ceiling but is unknown to the gate's
		// in-memory reservation counter — exactly the gate/ceiling divergence the
		// post-acquire ceilingCheck backstop exists for (e.g. a pod from before an
		// AGC restart, or a sibling AGC). With it in place, the gate ADMITS the
		// next job (reservation 0 < 1), AcquireJob claims it, and only then does
		// the provisioner's ceilingCheck (active pods 1 >= 1) hold it — the
		// acquired-then-held failure shape, reproduced deterministically.
		By("creating a Pending decoy pod that holds the ceiling")
		decoy := fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: q154-ceiling-decoy
  namespace: %s
  labels:
    app.kubernetes.io/managed-by: actions-gateway-controller
    actions-gateway/runner-group: %s
spec:
  nodeSelector:
    q154.actions-gateway/never-schedules: "true"
  securityContext:
    runAsNonRoot: true
    runAsUser: 1001
    seccompProfile:
      type: RuntimeDefault
  containers:
  - name: decoy
    image: %s
    securityContext:
      allowPrivilegeEscalation: false
      capabilities:
        drop: ["ALL"]
`, tenantNS, rgName, agcImage)
		Expect(utils.ApplyManifest(decoy)).To(Succeed(), "create decoy pod")

		By("waiting for the decoy to be Pending")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "pod", "q154-ceiling-decoy",
				"-n", tenantNS, "-o", "jsonpath={.status.phase}")
			out, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(out)).To(Equal("Pending"))
		}, 1*time.Minute, 2*time.Second).Should(Succeed())

		// Let the AGC's pod informer cache observe the decoy before the gate
		// admits the job, so ceilingCheck counts it and genuinely holds (rather
		// than a cache-lag window letting a worker pod be created). Bounded settle
		// for cache convergence on a single-node kind cluster.
		By("letting the AGC pod cache converge on the decoy")
		time.Sleep(8 * time.Second)

		By("enqueuing a job that the gate will admit but the ceiling will hold")
		var session string
		Eventually(func(g Gomega) { session = liveSession(g) }, 4*time.Minute, 2*time.Second).Should(Succeed())
		enqueueJobWithReqID(session, "q154-held")

		By("confirming the job was acquired (it passed the pre-acquire gate)")
		Eventually(func(g Gomega) {
			g.Expect(getJobStats(g, "q154-held").Acquired).To(BeTrue(),
				"job should have been acquired before the ceiling held it")
		}, 3*time.Minute, 2*time.Second).Should(Succeed())

		By("confirming the acquired-then-held job is NOT redelivered, and no worker pod was created")
		// deliveries stays at 1 (delivered once, then acquired → terminal), and the
		// only pod with the ceiling label remains the decoy (the held job never got
		// a pod). A redelivery would push deliveries past 1 and/or yield a 2nd pod.
		Consistently(func(g Gomega) {
			js := getJobStats(g, "q154-held")
			g.Expect(js.Acquired).To(BeTrue())
			g.Expect(js.Deliveries).To(Equal(1), "acquired job must not be redelivered")
			g.Expect(workerPodNames(g, tenantNS)).To(HaveLen(1), "only the decoy pod should exist")
		}, 24*time.Second, 3*time.Second).Should(Succeed())
	})

	It("E2E_AGC_SkippedJobIsRedeliveredAfterCapacityFrees: a skipped job is redelivered once capacity frees", func() {
		By("acquiring a first job that holds the single worker slot (Pending pod)")
		var s1 string
		Eventually(func(g Gomega) { s1 = liveSession(g) }, 4*time.Minute, 2*time.Second).Should(Succeed())
		enqueueJobWithReqID(s1, "q154-busy")

		Eventually(func(g Gomega) {
			g.Expect(getJobStats(g, "q154-busy").Acquired).To(BeTrue(), "first job should be acquired")
			g.Expect(workerPodNames(g, tenantNS)).To(HaveLen(1), "first job's worker pod should exist")
		}, 3*time.Minute, 2*time.Second).Should(Succeed())

		By("enqueuing a second job onto a still-polling session while the slot is full")
		var s2 string
		Eventually(func(g Gomega) { s2 = otherLiveSession(g, s1) }, 2*time.Minute, 2*time.Second).Should(Succeed())
		enqueueJobWithReqID(s2, "q154-skipped")

		By("confirming the gate skips it (not acquired) yet GitHub keeps redelivering it")
		// While the slot is full the gate declines to acquire q154-skipped, so it
		// is delivered repeatedly (deliveries climbs) but never acquired — proving
		// it is not lost. A bug where the gate acquired it anyway, or where a
		// skipped delivery were dropped, would fail one of these.
		Eventually(func(g Gomega) {
			js := getJobStats(g, "q154-skipped")
			g.Expect(js.Acquired).To(BeFalse(), "gate must skip the job while the slot is full")
			g.Expect(js.Deliveries).To(BeNumerically(">=", 2), "skipped job should be redelivered")
		}, 90*time.Second, 2*time.Second).Should(Succeed())

		By("freeing capacity by deleting the busy worker pod")
		Expect(utils.Run(exec.Command("kubectl", "delete", "pods",
			"-n", tenantNS,
			"-l", "actions-gateway/runner-group="+rgName,
			"--wait=false"))).Error().NotTo(HaveOccurred())

		By("confirming the previously skipped job is now acquired")
		Eventually(func(g Gomega) {
			g.Expect(getJobStats(g, "q154-skipped").Acquired).To(BeTrue(),
				"redelivered job should be acquired once capacity frees")
		}, 3*time.Minute, 2*time.Second).Should(Succeed())
	})
})
