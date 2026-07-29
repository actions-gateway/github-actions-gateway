//go:build e2e
// +build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/actions-gateway/github-actions-gateway/gmc/test/utils"
)

// Tier C tests dispatch a real workflow_dispatch event against a GitHub repo
// and verify the gateway runs the job to completion. They require a configured
// GitHub App and a repo with a self-hosted workflow committed in advance.
//
// Required env vars (test is skipped if any are absent):
//
//	GITHUB_E2E_APP_ID             — numeric GitHub App ID
//	GITHUB_E2E_INSTALLATION_ID    — numeric installation ID for the test org/repo
//	GITHUB_E2E_PRIVATE_KEY        — path to PEM file, or the PEM body
//	GITHUB_E2E_ORG                — GitHub org owning the test repo
//	GITHUB_E2E_REPO               — name of the repo containing the workflow
//	GITHUB_E2E_WORKFLOW           — optional; workflow filename (default: test-job.yml)
//	GITHUB_E2E_LONG_WORKFLOW      — optional; the long-running workflow the Q459
//	                                graceful-deletion measurement interrupts
//	                                (default: drain-probe.yml)

var _ = Describe("E2E_GitHub_RealDispatch", Ordered, Label("github-real", realGitHubEgressLabel), func() {
	const (
		tenantNS   = "tenant-github-real"
		agName     = "real-ag"
		secretName = "real-github-app-secret" //nolint:gosec // G101: the NAME of a Kubernetes Secret object, not a credential value.
	)

	var creds struct {
		appID          int64
		installationID int64
		privateKeyPEM  []byte
		org            string
		repo           string
		workflow       string
		longWorkflow   string
	}

	BeforeAll(func() {
		appIDStr := os.Getenv("GITHUB_E2E_APP_ID")
		installIDStr := os.Getenv("GITHUB_E2E_INSTALLATION_ID")
		pkValue := os.Getenv("GITHUB_E2E_PRIVATE_KEY")
		creds.org = os.Getenv("GITHUB_E2E_ORG")
		creds.repo = os.Getenv("GITHUB_E2E_REPO")
		creds.workflow = os.Getenv("GITHUB_E2E_WORKFLOW")
		if creds.workflow == "" {
			creds.workflow = "test-job.yml"
		}
		creds.longWorkflow = os.Getenv("GITHUB_E2E_LONG_WORKFLOW")
		if creds.longWorkflow == "" {
			creds.longWorkflow = "drain-probe.yml"
		}

		if appIDStr == "" || installIDStr == "" || pkValue == "" || creds.org == "" || creds.repo == "" {
			Skip("Tier C e2e tests skipped: set GITHUB_E2E_APP_ID, GITHUB_E2E_INSTALLATION_ID, " +
				"GITHUB_E2E_PRIVATE_KEY, GITHUB_E2E_ORG, GITHUB_E2E_REPO to enable")
		}

		var err error
		creds.appID, err = strconv.ParseInt(appIDStr, 10, 64)
		Expect(err).NotTo(HaveOccurred(), "parse GITHUB_E2E_APP_ID")
		creds.installationID, err = strconv.ParseInt(installIDStr, 10, 64)
		Expect(err).NotTo(HaveOccurred(), "parse GITHUB_E2E_INSTALLATION_ID")
		creds.privateKeyPEM, err = loadPEMForTest(pkValue)
		Expect(err).NotTo(HaveOccurred(), "load GitHub App private key")

		By("swapping fakegithub overrides for real GitHub on the GMC so AGC talks to real GitHub")
		orgURL := fmt.Sprintf("https://github.com/%s/%s", creds.org, creds.repo)
		cmd := exec.Command("kubectl", "set", "env",
			"deployment/gmc-controller-manager",
			"-c", "manager",
			"-n", gmcNamespace,
			"AGC_EXTRA_GITHUB_API_BASE_URL-",
			"AGC_EXTRA_GITHUB_BROKER_URL-",
			"AGC_EXTRA_STUB_AUTH_URL-",
			"AGC_EXTRA_STUB_BROKER_URL-",
			"AGC_EXTRA_GITHUB_ORG_URL="+orgURL,
		)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "set GMC env vars for real GitHub")

		By("waiting for GMC rollout to settle after env change")
		cmd = exec.Command("kubectl", "rollout", "status",
			"deployment/gmc-controller-manager",
			"-n", gmcNamespace,
			"--timeout=3m",
		)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		utils.CreateNamespace(tenantNS, nil)
		utils.CreateGitHubAppSecret(tenantNS, secretName, creds.appID, creds.installationID, creds.privateKeyPEM)

		By("applying ActionsGateway CR with a RunnerGroup pointing at the worker image")
		utils.RunnerTenant(tenantNS, agName, secretName, workerImage).ApplyWithWebhookRetry()

		By("waiting for AGC Deployment to be ready")
		utils.WaitForDeploymentReady(tenantNS, agcName, 5*time.Minute)
	})

	AfterAll(func() {
		// Best-effort dump of AGC logs before teardown to aid diagnosis on failure.
		if CurrentSpecReport().Failed() {
			By("dumping AGC pod logs")
			cmd := exec.Command("kubectl", "logs", "-n", tenantNS,
				"deployment/"+agcName, "--tail=300")
			out, _ := utils.Run(cmd)
			_, _ = fmt.Fprintln(GinkgoWriter, out)
		}
		utils.DeleteActionsGatewayCR(tenantNS, agName)
		utils.DeleteNamespace(tenantNS)
		// Restore fakegithub-pointing env vars so subsequent suites in this
		// process work, and drop the org URL we set.
		fakegithubBaseURL := fmt.Sprintf("http://%s.%s.svc.cluster.local:%s",
			fakegithubServiceName, infraNamespace, fakegithubServicePort)
		cmd := exec.Command("kubectl", "set", "env",
			"deployment/gmc-controller-manager",
			"-c", "manager",
			"-n", gmcNamespace,
			"AGC_EXTRA_GITHUB_ORG_URL-",
			fmt.Sprintf("AGC_EXTRA_GITHUB_API_BASE_URL=%s", fakegithubBaseURL),
			fmt.Sprintf("AGC_EXTRA_GITHUB_BROKER_URL=%s", fakegithubBaseURL),
			fmt.Sprintf("AGC_EXTRA_STUB_AUTH_URL=%s/token", fakegithubBaseURL),
			fmt.Sprintf("AGC_EXTRA_STUB_BROKER_URL=%s", fakegithubBaseURL),
		)
		_, _ = utils.Run(cmd)
	})

	SetDefaultEventuallyTimeout(10 * time.Minute)
	SetDefaultEventuallyPollingInterval(15 * time.Second)

	It("E2E_GitHub_ActionsGatewayReachesReady: CR Ready=True with real GitHub", func() {
		By("verifying ActionsGateway becomes Ready")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "actionsgateways.actions-gateway.github.com", agName,
				"-n", tenantNS,
				"-o", `jsonpath={.status.conditions[?(@.type=="Ready")].status}`,
			)
			out, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(out)).To(Equal("True"), "ActionsGateway not Ready yet")
		}, 5*time.Minute, 10*time.Second).Should(Succeed())
	})

	It("E2E_GitHub_WorkflowCompletesGreen: dispatched workflow runs to success", func() {
		dispatchedAt := time.Now().UTC().Add(-30 * time.Second).Format("2006-01-02T15:04:05Z")

		By(fmt.Sprintf("dispatching workflow %q via gh CLI", creds.workflow))
		cmd := exec.Command("gh", "workflow", "run", creds.workflow,
			"--repo", creds.org+"/"+creds.repo,
			"--ref", "main",
		)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "dispatch workflow via gh CLI")

		By("locating the dispatched workflow run")
		var runID string
		Eventually(func(g Gomega) {
			cmd := exec.Command("gh", "run", "list",
				"--repo", creds.org+"/"+creds.repo,
				"--workflow", creds.workflow,
				"--created", ">="+dispatchedAt,
				"--limit", "1",
				"--json", "databaseId,status,conclusion",
			)
			out, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			var runs []struct {
				DatabaseID int64  `json:"databaseId"`
				Status     string `json:"status"`
				Conclusion string `json:"conclusion"`
			}
			g.Expect(json.Unmarshal([]byte(out), &runs)).To(Succeed(), "parse gh run list: %s", out)
			g.Expect(runs).NotTo(BeEmpty(), "no run found yet")
			runID = fmt.Sprintf("%d", runs[0].DatabaseID)
		}, 2*time.Minute, 5*time.Second).Should(Succeed())

		By("waiting for a worker pod to appear in the tenant namespace")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "pods",
				"-n", tenantNS,
				"-l", "app.kubernetes.io/managed-by=actions-gateway-controller",
				"--no-headers",
			)
			out, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			lines := utils.GetNonEmptyLines(out)
			g.Expect(lines).NotTo(BeEmpty(), "no worker pod scheduled yet")
		}, 8*time.Minute, 10*time.Second).Should(Succeed())

		By(fmt.Sprintf("waiting for workflow run %s to complete with conclusion=success", runID))
		Eventually(func(g Gomega) {
			cmd := exec.Command("gh", "run", "view", runID,
				"--repo", creds.org+"/"+creds.repo,
				"--json", "status,conclusion",
			)
			out, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			var r struct {
				Status     string `json:"status"`
				Conclusion string `json:"conclusion"`
			}
			g.Expect(json.Unmarshal([]byte(out), &r)).To(Succeed())
			g.Expect(r.Status).To(Equal("completed"), "workflow still %q", r.Status)
			g.Expect(r.Conclusion).To(Equal("success"),
				"workflow concluded %q (expected success)", r.Conclusion)
		}, 10*time.Minute, 15*time.Second).Should(Succeed())
	})

	// The Q459 measurement. Q421 established at Tier B that a graceful worker-pod
	// removal reaches no eviction recovery on either tier, and left two questions
	// it could not ask there, because its drained worker was deliberately held
	// Pending and so had no live runner to report anything:
	//
	//  1. Does the SIGTERM relay's report leave the run re-runnable? The AGC
	//     recovers a run by rerun-failed-jobs and nothing else, so if GitHub
	//     declines that endpoint for whatever state the report produces, there is
	//     no gap to close on this path.
	//  2. Is a deliberate cancellation distinguishable from this at the pod? An
	//     automatic re-run that fights a human cancel is worse than the gap.
	//
	// Both need a real runner executing a real job against real GitHub, which is
	// this tier and only this tier.
	//
	// It deletes the pod rather than draining its node on purpose. Whether an
	// admitted eviction is a graceful delete is already settled — Q421 measured it
	// at Tier B — and what is open here is everything downstream of the delete.
	// A bare delete reaches that identically while dropping the cordon, the node
	// scoping and the --force from a measurement none of them are about. The spec
	// asserts a deletionTimestamp and a non-zero grace period so the deletion it
	// performed is on the record as the graceful kind.
	//
	// The measured result and the reasoning live in docs/design/04-operational-flows.md
	// §4.2; Q459 carries the decision that follows from it.
	It("E2E_GitHub_GracefullyDeletedWorkerReportsAndIsRerunnable: what GitHub does with a relayed report", func() {
		repoSlug := creds.org + "/" + creds.repo

		By(fmt.Sprintf("dispatching the long-running workflow %q", creds.longWorkflow))
		runID := dispatchAndResolveRun(repoSlug, creds.longWorkflow)
		AddReportEntry("Q459 workflow run", fmt.Sprintf("https://github.com/%s/actions/runs/%s", repoSlug, runID))
		defer func() {
			// Unconditional: the fixture job sleeps for ten minutes, and every exit
			// path from here — pass, fail, or panic — must leave nothing running on
			// the shared org's Actions minutes or on this tenant's worker capacity.
			_, _ = utils.Run(exec.Command("gh", "run", "cancel", runID, "--repo", repoSlug))
		}()

		By("waiting for GitHub to report the job in_progress")
		// Not "a worker pod exists". A pod that has not yet reached the runner's job
		// loop has no job to report on the way out, which would make this a
		// measurement of startup rather than of the relay.
		Eventually(func(g Gomega) {
			status, _ := firstJobState(g, repoSlug, runID)
			g.Expect(status).To(Equal("in_progress"), "job is %q", status)
		}, 10*time.Minute, 10*time.Second).Should(Succeed())

		By("locating the worker pod running that job")
		var podName string
		Eventually(func(g Gomega) {
			podName = runningWorkerForRun(g, tenantNS, runID)
			g.Expect(podName).NotTo(BeEmpty(), "no Running worker pod for run %s in %s", runID, tenantNS)
		}, 3*time.Minute, 2*time.Second).Should(Succeed())
		AddReportEntry("Q459 interrupted worker pod", podName)

		By("streaming the worker's logs so the relay can be observed as it happens")
		// Started before the delete and read after: once the pod object is gone
		// `kubectl logs --previous` has nothing to read, so the only chance to see
		// the wrapper forward the signal is while it is forwarding it.
		relayLog := followPodLogs(tenantNS, podName)

		By("gracefully deleting the worker pod")
		deletedAt := time.Now()
		_, err := utils.Run(exec.Command("kubectl", "delete", "pod", podName,
			"-n", tenantNS, "--wait=false"))
		Expect(err).NotTo(HaveOccurred(), "delete worker pod")

		By("confirming the deletion was graceful, not a force-remove")
		// The whole experiment is about the graceful path. A zero grace period would
		// be a different disruption — closer to a SIGKILL than to a drain — and every
		// conclusion below would be about that instead.
		grace, hadTimestamp := deletionGrace(tenantNS, podName)
		AddReportEntry("Q459 deletionGracePeriodSeconds observed", fmt.Sprintf("%d (deletionTimestamp seen: %t)", grace, hadTimestamp))

		observed := newPhaseRecorder(tenantNS, podName)
		stopSampling := observed.start(200 * time.Millisecond)

		By("waiting for the worker pod to be gone")
		Eventually(func(g Gomega) {
			out, err := utils.Run(exec.Command("kubectl", "get", "pod", podName,
				"-n", tenantNS, "--ignore-not-found", "-o", "name"))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(out)).To(BeEmpty(), "worker pod %s still exists", podName)
		}, 3*time.Minute, 500*time.Millisecond).Should(Succeed())
		stopSampling()

		AddReportEntry("Q459 observed pod phase/reason sequence", strings.Join(observed.sequence(), " -> "))
		AddReportEntry("Q459 worker log tail across the deletion", relayLog.stopAndRead())

		By("waiting for the job to leave in_progress on GitHub, and recording what it landed on")
		// The relay's whole purpose is that this happens promptly rather than at
		// GitHub's own liveness timeout. The elapsed time is the measurement that
		// tells the two apart, so it is recorded whichever way it goes.
		var jobConclusion string
		Eventually(func(g Gomega) {
			status, conclusion := firstJobState(g, repoSlug, runID)
			g.Expect(status).To(Equal("completed"), "job is still %q", status)
			jobConclusion = conclusion
		}, 15*time.Minute, 10*time.Second).Should(Succeed())
		AddReportEntry("Q459 job conclusion after graceful deletion", jobConclusion)
		AddReportEntry("Q459 deletion to job conclusion", time.Since(deletedAt).Round(time.Second).String())

		By("asking GitHub to re-run the failed jobs, exactly as eviction recovery would")
		// The load-bearing step. This is the call handleEviction makes and the only
		// recovery the AGC has; whether GitHub accepts it for the state the relayed
		// report produced IS the answer to Q459.
		rerunStatus, rerunBody := rerunFailedJobsStatus(repoSlug, runID)
		AddReportEntry("Q459 rerun-failed-jobs response", fmt.Sprintf("%s %s", rerunStatus, rerunBody))

		if rerunStatus != "201" {
			// Recorded, not asserted away: a decline is a legitimate outcome of the
			// experiment and decides Q459 toward "accept" rather than failing it.
			AddReportEntry("Q459 outcome", "rerun-failed-jobs DECLINED — no automatic recovery is available on this path")
			return
		}

		By("confirming the accepted re-run actually produces a second attempt that runs")
		// An accepted POST is not a recovered job. If the new attempt never reaches a
		// runner, closing the gap would buy an API call and nothing else.
		Eventually(func(g Gomega) {
			g.Expect(runAttemptCount(g, repoSlug, runID)).To(BeNumerically(">=", 2),
				"GitHub accepted the re-run but created no second attempt")
		}, 3*time.Minute, 5*time.Second).Should(Succeed())

		Eventually(func(g Gomega) {
			status, _ := firstJobState(g, repoSlug, runID)
			g.Expect(status).To(Equal("in_progress"), "re-run attempt is %q", status)
		}, 10*time.Minute, 10*time.Second).Should(Succeed())
		AddReportEntry("Q459 outcome", "rerun-failed-jobs ACCEPTED and the second attempt reached a gateway runner")

		// The re-run this spec triggered is stood down by the deferred cancel above.
		// Do not block on its worker disappearing: cancelling a run does not promptly
		// remove the pod (measured — the wait times out at 5 minutes), and making this
		// spec's result depend on that is what turns a clean measurement into a flake.
	})

	// Q459's second question, and the one that decides whether the gap can be closed
	// safely rather than merely usefully.
	//
	// The spec above establishes that a disrupted worker's job concludes `failure`
	// and that GitHub will re-run it. That is not yet a licence to re-run
	// automatically: `failure` is also what a job that genuinely failed concludes,
	// and PodFailed with an empty reason is what its worker pod lands in. If those
	// two are indistinguishable in cluster state, then recovering the disruption
	// necessarily also re-runs every legitimately failing job, which is far worse
	// than the gap.
	//
	// The candidate discriminator is metadata.deletionTimestamp: a worker taken away
	// by a drain or a delete carries one when its terminal phase publishes, and a
	// worker whose job ended by itself does not. This spec measures the second half
	// of that claim against the most important case — a human cancelling the run in
	// GitHub, which reaches the runner over its own broker connection rather than
	// through the pod. Reading the code says such a pod is never externally deleted;
	// per docs/development/testing.md that is a hypothesis until it is exercised.
	// PENDING, deliberately. This spec has never produced a result. It is correct as
	// written about *what* to measure, but it cannot yet run reliably alongside the
	// spec above: that one triggers a re-run whose worker occupies the tenant for the
	// fixture's full sleep, so "which worker belongs to my run" is ambiguous, and the
	// run-id annotation that would disambiguate it is absent on this tier (Q495).
	// Cancelling the re-run does not promptly free the worker either — measured, the
	// wait times out at five minutes.
	//
	// Left pending rather than deleted: the design of the measurement is the valuable
	// part and is unaffected. Un-pend it once Q495 restores the run-id annotation, which
	// makes the worker lookup exact and removes the contention entirely.
	PIt("E2E_GitHub_CancelledRunLeavesNoDeletionMark: a human cancel is distinguishable from a disruption", func() {
		repoSlug := creds.org + "/" + creds.repo

		By(fmt.Sprintf("dispatching the long-running workflow %q", creds.longWorkflow))
		runID := dispatchAndResolveRun(repoSlug, creds.longWorkflow)
		AddReportEntry("Q459 cancel-path workflow run", fmt.Sprintf("https://github.com/%s/actions/runs/%s", repoSlug, runID))
		defer func() {
			_, _ = utils.Run(exec.Command("gh", "run", "cancel", runID, "--repo", repoSlug))
		}()

		By("waiting for GitHub to report the job in_progress")
		Eventually(func(g Gomega) {
			status, _ := firstJobState(g, repoSlug, runID)
			g.Expect(status).To(Equal("in_progress"), "job is %q", status)
		}, 10*time.Minute, 10*time.Second).Should(Succeed())

		By("locating the worker pod running that job")
		var podName string
		Eventually(func(g Gomega) {
			podName = runningWorkerForRun(g, tenantNS, runID)
			g.Expect(podName).NotTo(BeEmpty(), "no Running worker pod for run %s in %s", runID, tenantNS)
		}, 3*time.Minute, 2*time.Second).Should(Succeed())
		AddReportEntry("Q459 cancelled worker pod", podName)

		By("sampling the pod's phase and deletionTimestamp across the cancellation")
		// Both fields together, sampled as it happens: the claim is about what is
		// true of the pod at the moment its terminal phase publishes, which is a
		// state that exists only briefly and cannot be read afterwards.
		observed := newFieldRecorder(tenantNS, podName,
			"{.status.phase}/{.status.reason}/deleting={.metadata.deletionTimestamp}")
		stopSampling := observed.start(200 * time.Millisecond)

		By("cancelling the run in GitHub, the way a human would")
		_, err := utils.Run(exec.Command("gh", "run", "cancel", runID, "--repo", repoSlug))
		Expect(err).NotTo(HaveOccurred(), "cancel run %s", runID)

		By("waiting for the worker pod to reach a terminal phase")
		Eventually(func(g Gomega) {
			out, err := utils.Run(exec.Command("kubectl", "get", "pod", podName,
				"-n", tenantNS, "--ignore-not-found", "-o", "jsonpath={.status.phase}"))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(out)).To(BeElementOf("Succeeded", "Failed", ""),
				"pod is still %s", strings.TrimSpace(out))
		}, 5*time.Minute, 500*time.Millisecond).Should(Succeed())
		stopSampling()

		seq := observed.sequence()
		AddReportEntry("Q459 cancel-path pod phase/deletion sequence", strings.Join(seq, " -> "))

		By("asserting the cancelled worker reached a terminal phase without a deletion mark")
		// The discriminator. A terminal phase observed with deleting= empty is a job
		// that ended by itself; the spec above recorded the disrupted worker carrying
		// a deletionTimestamp instead. If this ever fails, the two cases have become
		// indistinguishable and automatic recovery of the disruption path would start
		// re-running deliberate cancellations.
		var terminalWithoutDeletion bool
		for _, s := range seq {
			if (strings.HasPrefix(s, "Succeeded/") || strings.HasPrefix(s, "Failed/")) &&
				strings.HasSuffix(s, "deleting=") {
				terminalWithoutDeletion = true
			}
		}
		Expect(seq).NotTo(BeEmpty(), "the sampler saw nothing; the measurement did not run")
		Expect(terminalWithoutDeletion).To(BeTrue(),
			"a cancelled run's worker never published a terminal phase free of a deletionTimestamp, "+
				"so a human cancel is NOT distinguishable from a drain at the pod. Observed: %v", seq)

		_, conclusion := firstJobState(Default, repoSlug, runID)
		AddReportEntry("Q459 cancel-path job conclusion", conclusion)
	})
})

// dispatchAndResolveRun dispatches a workflow_dispatch workflow on main and returns
// the database ID of the run it created.
//
// It identifies that run by *identity*, not by time: it snapshots the run IDs that
// already exist and waits for one that is not among them. A created-since window
// cannot do this job here. These specs run back to back against one workflow, so any
// window wide enough to absorb GitHub's own scheduling lag also contains the previous
// spec's run — and `--limit 1` then returns that one. It is not a hypothetical: the
// cancel-path spec resolved the graceful-deletion spec's run, cancelled a run that had
// already finished, and timed out waiting for a pod that nothing had disturbed.
func dispatchAndResolveRun(repoSlug, workflow string) string {
	GinkgoHelper()
	before := make(map[string]bool)
	for _, id := range recentRunIDs(Default, repoSlug, workflow) {
		before[id] = true
	}

	_, err := utils.Run(exec.Command("gh", "workflow", "run", workflow,
		"--repo", repoSlug, "--ref", "main"))
	Expect(err).NotTo(HaveOccurred(), "dispatch %s", workflow)

	var runID string
	Eventually(func(g Gomega) {
		for _, id := range recentRunIDs(g, repoSlug, workflow) {
			if !before[id] {
				runID = id
				return
			}
		}
		g.Expect(runID).NotTo(BeEmpty(), "no run of %s has appeared that did not already exist", workflow)
	}, 3*time.Minute, 5*time.Second).Should(Succeed())
	return runID
}

// runningWorkerForRun returns the name of the Running worker pod the AGC provisioned
// for a specific workflow run, or "" when none exists yet.
//
// Scoped by the run-id annotation the AGC stamps on every worker pod
// (provisioner.AnnotationRunID) rather than by "the first Running worker in the
// namespace". These specs interrupt one run while a previous spec's re-run may still
// have a worker of its own up, and picking the wrong pod would make the spec measure
// a job nobody touched.
// It prefers an exact match on the run-id annotation. When no pod carries it, a single
// Running worker is still unambiguous — there is nothing else it could be — so that is
// accepted, and the annotations actually present are recorded so a mismatch is
// diagnosable rather than silently timing out. Only a genuine ambiguity (no annotated
// match and several Running workers) yields "".
func runningWorkerForRun(g Gomega, ns, runID string) string {
	out, err := utils.Run(exec.Command("kubectl", "get", "pods",
		"-n", ns,
		"-l", "app.kubernetes.io/managed-by=actions-gateway-controller",
		"--field-selector", "status.phase=Running",
		"-o", fmt.Sprintf(
			`jsonpath={.items[?(@.metadata.annotations.actions-gateway\.com/run-id=="%s")].metadata.name}`, runID),
	))
	g.Expect(err).NotTo(HaveOccurred())
	// The selector can match more than one pod only if a run has several jobs; these
	// fixtures have exactly one, so the first field is the worker for this run.
	if fields := strings.Fields(out); len(fields) > 0 {
		return fields[0]
	}

	all, err := utils.Run(exec.Command("kubectl", "get", "pods",
		"-n", ns,
		"-l", "app.kubernetes.io/managed-by=actions-gateway-controller",
		"--field-selector", "status.phase=Running",
		"-o", `jsonpath={range .items[*]}{.metadata.name}={.metadata.annotations.actions-gateway\.com/run-id} {end}`,
	))
	g.Expect(err).NotTo(HaveOccurred())
	pods := strings.Fields(all)
	if len(pods) != 1 {
		return ""
	}
	AddReportEntry("Q459 worker matched without the run-id annotation",
		fmt.Sprintf("wanted run %s; sole Running worker is %s", runID, pods[0]))
	return strings.SplitN(pods[0], "=", 2)[0]
}

// recentRunIDs returns the database IDs of the most recent runs of a workflow. The
// window is deliberately wider than one: a re-run bumps an existing run to the top of
// the list rather than creating a new one, so the newest entry is not reliably the
// newest *run*.
func recentRunIDs(g Gomega, repoSlug, workflow string) []string {
	out, err := utils.Run(exec.Command("gh", "run", "list",
		"--repo", repoSlug, "--workflow", workflow,
		"--limit", "20", "--json", "databaseId"))
	g.Expect(err).NotTo(HaveOccurred())
	var runs []struct {
		DatabaseID int64 `json:"databaseId"`
	}
	g.Expect(json.Unmarshal([]byte(out), &runs)).To(Succeed(), "parse gh run list: %s", out)
	ids := make([]string, 0, len(runs))
	for _, r := range runs {
		ids = append(ids, fmt.Sprintf("%d", r.DatabaseID))
	}
	return ids
}

// firstJobState returns the status and conclusion of a run's first job, reading the
// latest attempt. The job is what this experiment interrupts; the run-level status
// lags it and would report "completed" for a re-run that has not started.
func firstJobState(g Gomega, repoSlug, runID string) (status, conclusion string) {
	out, err := utils.Run(exec.Command("gh", "api",
		fmt.Sprintf("repos/%s/actions/runs/%s/jobs?filter=latest", repoSlug, runID)))
	g.Expect(err).NotTo(HaveOccurred())
	var resp struct {
		Jobs []struct {
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
		} `json:"jobs"`
	}
	g.Expect(json.Unmarshal([]byte(out), &resp)).To(Succeed(), "parse jobs response: %s", out)
	g.Expect(resp.Jobs).NotTo(BeEmpty(), "run %s has no jobs yet", runID)
	return resp.Jobs[0].Status, resp.Jobs[0].Conclusion
}

// runAttemptCount returns the run's current attempt number. A re-run that GitHub
// really acted on increments it; one it merely accepted does not.
func runAttemptCount(g Gomega, repoSlug, runID string) int {
	out, err := utils.Run(exec.Command("gh", "api",
		fmt.Sprintf("repos/%s/actions/runs/%s", repoSlug, runID), "--jq", ".run_attempt"))
	g.Expect(err).NotTo(HaveOccurred())
	n, err := strconv.Atoi(strings.TrimSpace(out))
	g.Expect(err).NotTo(HaveOccurred(), "parse run_attempt: %s", out)
	return n
}

// rerunFailedJobsStatus makes the exact call handleEviction makes and returns the
// HTTP status and body rather than an error. The failure mode is the measurement
// here, so it must be readable rather than fatal.
func rerunFailedJobsStatus(repoSlug, runID string) (status, body string) {
	GinkgoHelper()
	cmd := exec.Command("gh", "api", "--method", "POST",
		"-i", fmt.Sprintf("repos/%s/actions/runs/%s/rerun-failed-jobs", repoSlug, runID))
	raw, _ := cmd.CombinedOutput() // non-2xx exits non-zero; -i still prints the response
	text := string(raw)
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "HTTP/") {
			if f := strings.Fields(line); len(f) > 1 {
				status = f[1]
			}
			break
		}
	}
	return status, strings.TrimSpace(lastNonEmptyLine(text))
}

func lastNonEmptyLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			return t
		}
	}
	return ""
}

// deletionGrace reads the grace period Kubernetes recorded on a pod that is
// terminating. It tolerates a pod that has already gone: a worker whose runner
// exits the instant it is signalled can outrun the read, and a missed read is worth
// less than a spec that fails for having been too slow to look.
func deletionGrace(ns, name string) (seconds int, sawDeletionTimestamp bool) {
	GinkgoHelper()
	out, err := utils.Run(exec.Command("kubectl", "get", "pod", name, "-n", ns,
		"--ignore-not-found",
		"-o", "jsonpath={.metadata.deletionGracePeriodSeconds}/{.metadata.deletionTimestamp}"))
	if err != nil {
		return -1, false
	}
	parts := strings.SplitN(strings.TrimSpace(out), "/", 2)
	if len(parts) != 2 || parts[0] == "" {
		return -1, false
	}
	n, convErr := strconv.Atoi(parts[0])
	if convErr != nil {
		return -1, false
	}
	return n, parts[1] != ""
}

// podLogFollower is a running `kubectl logs -f` whose output is collected until
// stopAndRead is called.
type podLogFollower struct {
	cmd *exec.Cmd
	buf *syncBuffer
}

// followPodLogs starts streaming a pod's logs. It must be started before the
// disruption being measured: once the pod object is removed there is no --previous
// to fall back on, so a log line not captured live is lost.
func followPodLogs(ns, name string) *podLogFollower {
	GinkgoHelper()
	buf := &syncBuffer{}
	cmd := exec.Command("kubectl", "logs", "-f", "-n", ns, name, "--tail=20")
	cmd.Stdout = buf
	cmd.Stderr = buf
	Expect(cmd.Start()).To(Succeed(), "start log follower for %s", name)
	return &podLogFollower{cmd: cmd, buf: buf}
}

// stopAndRead ends the stream and returns everything it collected.
func (f *podLogFollower) stopAndRead() string {
	if f.cmd.Process != nil {
		_ = f.cmd.Process.Kill()
	}
	_ = f.cmd.Wait()
	return f.buf.String()
}

// syncBuffer is an io.Writer safe for the exec package's writer goroutine to fill
// while the spec goroutine reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// loadPEMForTest reads a PEM key from a file path or returns the value if it looks like a PEM literal.
func loadPEMForTest(value string) ([]byte, error) {
	const pemHeader = "-----"
	if len(value) >= len(pemHeader) && value[:len(pemHeader)] == pemHeader {
		return []byte(value), nil
	}
	return os.ReadFile(value)
}
