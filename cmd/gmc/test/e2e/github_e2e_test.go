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

// live-GitHub tests dispatch a real workflow_dispatch event against a GitHub repo
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

		// workerEphemeralStorageLimit is the seam the Q396 eviction measurement needs:
		// a pod-level cap the spec can deliberately overshoot to make the kubelet evict
		// that one worker. See utils.WithEphemeralStorageLimit for why this is the only
		// disruption at this tier that reaches eviction recovery at all.
		//
		// Sized from a measurement rather than a guess. The kubelet charges a pod only
		// its *writable* layer, emptyDirs and logs — image layers are read-only and do
		// not count — and a worker pod built from the real runner image was measured at
		// 28KiB against the node's stats/summary endpoint. These fixture jobs add
		// nothing to that: the `hold` job echoes and sleeps, the green-path job echoes,
		// and neither checks out a repository. 256Mi is therefore four orders of
		// magnitude of headroom, which is what makes the deliberate overshoot below the
		// only thing that can cross it.
		workerEphemeralStorageLimit = "256Mi"
		// evictionFillMiB overshoots that cap in one write. Sized to cross it
		// unambiguously without making the kubelet's own node-level disk-pressure
		// thresholds a second, uncontrolled cause of eviction.
		evictionFillMiB = 384
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
			Skip("live-GitHub e2e tests skipped: set GITHUB_E2E_APP_ID, GITHUB_E2E_INSTALLATION_ID, " +
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
		utils.RunnerTenant(tenantNS, agName, secretName, workerImage).
			WithEphemeralStorageLimit(workerEphemeralStorageLimit).
			ApplyWithWebhookRetry()

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

	// The Q396 measurement: what a real eviction costs, and whether recovery fires.
	//
	// The one eviction-latency figure the project has is unusable: the U5 probe's ~9.5
	// minutes coincided with the job's own 10-minute `timeout-minutes` boundary, so the
	// job lock's TTL, GitHub's liveness detection, and the workflow timeout are
	// indistinguishable in it. Everything downstream — the "at worst ~10 minutes" in
	// docs/design/04-operational-flows.md §4.2, the blog post, and Q418's whole premise
	// that there is latency worth short-circuiting — rests on that one confounded
	// number.
	//
	// This spec removes the confound. The fixture workflow carries no
	// `timeout-minutes` at all, so the job can only end because something interrupted
	// it or because the sleep elapsed, and those two are distinguishable.
	//
	// # Why the disruption is an ephemeral-storage overshoot
	//
	// Eviction recovery keys on exactly one shape: PodFailed with reason "Evicted",
	// the kubelet's node-pressure kill. Q421 measured that the graceful removals — a
	// drain, a delete — never produce it and reach no recovery at all, so neither can
	// be used here. Node-wide memory or disk pressure does produce it, but the kubelet
	// picks the victim by its own ranking, on a node shared with the rest of the
	// suite. A pod-level ephemeral-storage limit is the one lever that is both genuine
	// and aimed: exceed it and the kubelet evicts that pod, and only that pod, with a
	// zero grace period. Nothing about the eviction is simulated — same kubelet code
	// path, same object shape, same SIGKILL.
	//
	// The zero grace period is the point, not a side effect. It is what makes this the
	// *ungraceful* case: the runner is killed outright, the wrapper's SIGTERM relay
	// (Q385) never runs, nothing reports to GitHub, and GitHub has to notice by itself.
	// That is the case the latency question is about. The graceful counterpart, where
	// the runner does get its own report out, is Q459's and is measured directly below.
	//
	// # What is asserted versus what is recorded
	//
	// Both timestamps come from the servers that own them — the container's
	// `finishedAt` from the kubelet, the job's `completed_at` from GitHub — rather than
	// from when this spec's polling happened to notice. Poll cadence must not appear in
	// a published latency figure.
	//
	// The re-run half is asserted outright, and a refused re-run FAILS the spec
	// (Q510). Earlier revisions recorded the outcome as a report entry instead:
	// first because Q495 left this tier unable to name the run it had to re-run
	// (fixed, #967), then because the 2026-07-29 measurement found the re-run
	// firing ~9.5 minutes before GitHub concludes the run and being refused with
	// `403 This workflow is already running` (Q503). The AGC now retries a refused
	// re-run until GitHub concludes the run and accepts it, so "the re-run landed
	// and a second attempt ran" is a property this tier is required to have — and
	// a spec that records a refusal and passes can neither verify that nor catch
	// its regression (testing.md § negative assertions).
	//
	// Placed ahead of the Q459 spec below on purpose: a re-run this spec triggers
	// would hold a worker for the fixture's full ten-minute sleep, and with no run-id
	// annotation to disambiguate by, the spec below could not tell that worker from
	// its own. The Q495 fix that makes this branch reachable is the same one that
	// makes both lookups exact.
	It("E2E_GitHub_EvictedWorkerLatencyAndRerun: eviction to GitHub's conclusion, unconfounded", func() {
		repoSlug := creds.org + "/" + creds.repo

		// Snapshot the Running workers before dispatching, so the worker this run
		// produces is identified by not having been there rather than by being the only
		// one. A previous spec's worker can outlive its own run, and this spec must not
		// evict it.
		before := workerSnapshot(tenantNS)

		By(fmt.Sprintf("dispatching the long-running workflow %q (no timeout-minutes)", creds.longWorkflow))
		runID := dispatchAndResolveRun(repoSlug, creds.longWorkflow)
		AddReportEntry("Q396 workflow run", fmt.Sprintf("https://github.com/%s/actions/runs/%s", repoSlug, runID))
		defer func() {
			// Unconditional. The fixture sleeps ten minutes and a re-run would sleep ten
			// more; no exit path from here may leave either burning the shared org's
			// Actions minutes or this tenant's worker capacity.
			_, _ = utils.Run(exec.Command("gh", "run", "cancel", runID, "--repo", repoSlug))
		}()

		By("waiting for GitHub to report the job in_progress")
		// The job must be genuinely running before it is interrupted. Evicting a worker
		// that has not reached the runner's job loop would measure startup, and GitHub
		// would have no in-flight job whose loss it needs to detect — which is the
		// entire quantity under measurement.
		Eventually(func(g Gomega) {
			// An eviction this spec did not cause invalidates everything below it, and
			// from the GitHub side it is indistinguishable from a slow start. Naming it
			// here turns a ten-minute "job is still queued" timeout into an immediate,
			// correctly attributed failure. The case that actually happens is another
			// workload putting the shared node under pressure: the kubelet resolves that
			// by evicting somebody, and a worker under an explicit ephemeral-storage
			// limit is a candidate.
			g.Expect(evictedWorkerNames(tenantNS)).To(BeEmpty(),
				"a worker pod was evicted before its job ever started, so nothing below "+
					"would be measuring the eviction this spec performs. Either this node is "+
					"under pressure from another workload, or %s is too little headroom",
				workerEphemeralStorageLimit)
			status, _ := firstJobState(g, repoSlug, runID)
			g.Expect(status).To(Equal("in_progress"), "job is %q", status)
		}, 10*time.Minute, 10*time.Second).Should(Succeed())

		By("locating the worker pod running that job")
		var podName string
		Eventually(func(g Gomega) {
			var diag string
			podName, diag = runningWorkerForRun(g, tenantNS, runID, before)
			g.Expect(podName).NotTo(BeEmpty(),
				"no worker pod attributable to run %s in %s: %s", runID, tenantNS, diag)
		}, 3*time.Minute, 2*time.Second).Should(Succeed())
		AddReportEntry("Q396 evicted worker pod", podName)

		By("confirming the worker carries the ephemeral-storage cap this spec overshoots")
		// Without the cap there is nothing to exceed, the kubelet never evicts, and the
		// spec would time out on the eviction wait for a reason that has nothing to do
		// with eviction. Check the premise before acting on it.
		Expect(podEphemeralStorageLimit(tenantNS, podName)).To(Equal(workerEphemeralStorageLimit),
			"worker pod carries no ephemeral-storage limit; the eviction lever is absent")

		By("streaming the worker's logs, so anything it manages to say on the way out is captured")
		// Expected to show nothing: an ephemeral-storage eviction kills with a zero
		// grace period, so the wrapper's SIGTERM relay never runs. Captured anyway,
		// because "the runner said nothing" is the claim that makes GitHub's own
		// detection the only thing that can conclude the job.
		relayLog := followPodLogs(tenantNS, podName)

		observed := newPhaseRecorder(tenantNS, podName)
		stopSampling := observed.start(time.Second)

		By(fmt.Sprintf("overshooting the worker's ephemeral-storage limit by writing %dMiB", evictionFillMiB))
		filledAt := time.Now()
		fillOutput := overflowEphemeralStorage(tenantNS, podName, evictionFillMiB)
		AddReportEntry("Q396 fill command output", fillOutput)

		By("waiting for the kubelet to evict the worker")
		// The kubelet's eviction manager rechecks local storage on its housekeeping
		// cycle, so this is not instant — measured at roughly a minute on kind. The
		// wait is bounded well past that; blowing it means the lever did not work and
		// nothing below would be measuring an eviction.
		Eventually(func(g Gomega) {
			g.Expect(podPhaseReason(tenantNS, podName)).To(Equal("Failed/Evicted"),
				"worker pod has not been evicted")
		}, 5*time.Minute, 2*time.Second).Should(Succeed())
		stopSampling()

		evictedAt, exitCode, evictionMessage := evictionFacts(tenantNS, podName)
		AddReportEntry("Q396 observed pod phase/reason sequence", strings.Join(observed.sequence(), " -> "))
		AddReportEntry("Q396 kubelet eviction message", evictionMessage)
		AddReportEntry("Q396 runner container exit code", strconv.Itoa(exitCode))
		AddReportEntry("Q396 fill to kubelet eviction", evictedAt.Sub(filledAt).Round(time.Second).String())
		AddReportEntry("Q396 worker log tail across the eviction", relayLog.stopAndRead())

		By("confirming the runner was killed outright rather than asked to stop")
		// The Queue row's "runner genuinely killed", made an assertion. 137 is SIGKILL:
		// no grace period, no SIGTERM relay, no report to GitHub. A graceful exit code
		// here would mean the runner had a chance to say something on the way out, and
		// the latency below would then be measuring the report rather than GitHub's own
		// detection — which is Q459's experiment, not this one.
		Expect(exitCode).To(Equal(137),
			"the evicted runner exited %d, not SIGKILL; this is not the ungraceful path", exitCode)

		By("waiting for GitHub to conclude the job it can no longer hear from")
		// The measurement. Bounded at 20 minutes: the design puts the job lock's TTL at
		// ~10 minutes from the last renewal, so anything beyond this is a finding in
		// its own right rather than a wait worth extending. Pinned to attempt 1: the
		// AGC's re-run starts a second attempt as soon as GitHub concludes the run
		// (Q503), at which point the "latest" filter stops naming the job under
		// measurement.
		var jobConclusion string
		Eventually(func(g Gomega) {
			status, conclusion := firstJobStateForAttempt(g, repoSlug, runID, 1)
			g.Expect(status).To(Equal("completed"), "job is still %q", status)
			jobConclusion = conclusion
		}, 20*time.Minute, 15*time.Second).Should(Succeed())

		// GitHub's own record of when it gave up, not this spec's record of noticing.
		// The latency below is the whole deliverable, so neither end of it may carry a
		// poll interval: this is GitHub's timestamp and evictedAt is the kubelet's.
		concludedAtRaw := firstJobCompletedAtForAttempt(Default, repoSlug, runID, 1)
		concludedAt, err := time.Parse(time.RFC3339, concludedAtRaw)
		Expect(err).NotTo(HaveOccurred(), "parse job completed_at %q", concludedAtRaw)

		AddReportEntry("Q396 job conclusion after eviction", jobConclusion)
		AddReportEntry("Q396 eviction (kubelet finishedAt) -> conclusion (GitHub completed_at)",
			concludedAt.Sub(evictedAt).Round(time.Second).String())
		AddReportEntry("Q396 server timestamps",
			fmt.Sprintf("container finishedAt=%s, GitHub completed_at=%s",
				evictedAt.Format(time.RFC3339), concludedAt.Format(time.RFC3339)))

		By("reading what the AGC decided when it saw the eviction")
		agcLog := agcEvictionLog(tenantNS)
		AddReportEntry("Q396 AGC eviction log lines", agcLog)

		By("asserting the AGC observed the eviction and reached recovery")
		// "run_id unknown" was a recorded Q495 confirmation while that defect was open;
		// it shipped fixed in #967, so a worker the AGC cannot attribute to its run is
		// a regression now, not an outcome (Q510).
		Expect(agcLog).NotTo(ContainSubstring("worker pod disrupted but run_id unknown"),
			"the AGC saw the eviction but had no run identity to recover with — "+
				"the Q495 regression. Log:\n%s", agcLog)

		By("asserting the retry budget was spent exactly once")
		// The Q106 sharded-reservation invariant, at the one tier that can exercise it
		// against real GitHub. One eviction must reserve one slot: a second
		// "scheduling auto-retry" for this run would mean the budget is being refilled
		// or the eviction counted twice, which is precisely the over-budget bug that
		// invariant exists to prevent. The budget counts recoveries, not HTTP calls —
		// one recovery may retry a refused re-run several times (Q503) and still holds
		// one slot.
		scheduled := strings.Count(agcLog, "worker pod disrupted; scheduling auto-retry")
		Expect(scheduled).To(Equal(1),
			"one eviction must reserve exactly one retry slot, saw %d. Log:\n%s", scheduled, agcLog)
		// Both spellings: the AGC ships a JSON handler, but a text handler renders the
		// same attribute as attempt=1 and this assertion is about the value, not the
		// encoding.
		Expect(agcLog).To(SatisfyAny(ContainSubstring(`"attempt":1`), ContainSubstring("attempt=1")),
			"the reserved slot was not the run's first. Log:\n%s", agcLog)
		Expect(agcLog).NotTo(ContainSubstring("disruption retry budget exhausted"),
			"a single eviction exhausted the retry budget. Log:\n%s", agcLog)

		By("waiting for the AGC's re-run to be accepted by GitHub")
		// The second half of what Q396 was filed to answer, now an assertion (Q510).
		// GitHub refuses rerun-failed-jobs until the conclusion measured above, so the
		// AGC retries on its re-run interval (Q503) and logs the acceptance; a refusal
		// that outlasts the AGC's re-run window, or a terminal API error, logs a
		// terminal failure instead and must fail this spec rather than become a report
		// entry — that pass-through is exactly how Q503 went unverified.
		Eventually(func(g Gomega) {
			agcLog := agcEvictionLog(tenantNS)
			if strings.Contains(agcLog, "disruption auto-retry failed") {
				StopTrying("the AGC gave up on the re-run — refused past the re-run window, " +
					"or a terminal API error. Log:\n" + agcLog).Now()
			}
			g.Expect(agcLog).To(ContainSubstring("disruption auto-retry triggered"),
				"the re-run has not been accepted yet")
		}, 5*time.Minute, 10*time.Second).Should(Succeed())

		By("asserting GitHub actually started a second attempt")
		// Accepted is necessary but not the deliverable: the deliverable is a second
		// attempt actually running, which is what "recovery" means to the tenant.
		Eventually(func(g Gomega) {
			g.Expect(runAttemptCount(g, repoSlug, runID)).To(BeNumerically(">=", 2),
				"the AGC's re-run was accepted but GitHub created no second attempt")
		}, 5*time.Minute, 10*time.Second).Should(Succeed())

		AddReportEntry("Q396 outcome",
			"eviction detected, retry budget spent once, re-run accepted after the run concluded, "+
				"and a second attempt ran (Q503 verified)")
	})

	// The Q459 measurement. Q421 established at fake-GitHub that a graceful worker-pod
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
	// at fake-GitHub — and what is open here is everything downstream of the delete.
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
		before := workerSnapshot(tenantNS)
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
			var diag string
			podName, diag = runningWorkerForRun(g, tenantNS, runID, before)
			g.Expect(podName).NotTo(BeEmpty(), "no worker pod attributable to run %s in %s: %s", runID, tenantNS, diag)
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
	// through the pod.
	//
	// Measured 2026-07-29 and it holds: the cancelled run's worker published
	// Failed//deleting= — the same phase and empty reason as a disruption, without
	// the deletion mark. It also took 10m02s to get there, running the fixture's
	// whole 600s sleep after the cancel, because nothing relays the cancellation to
	// the pod (Q501). That is why this wait is budgeted past the sleep rather than
	// past GitHub's ~5-minute cancellation grace.
	//
	// It was pending until 2026-07-29 for a reason that has since been removed: it
	// could not tell its own worker from the one the spec above leaves behind, since
	// that spec triggers a re-run whose worker occupies the tenant for the fixture's
	// full sleep and the run-id annotation that would disambiguate it is absent on
	// this tier (Q495). Both specs now snapshot the Running workers before
	// dispatching, so each identifies its own worker by its not having been there
	// before — no annotation, and no waiting for the namespace to fall quiet.
	It("E2E_GitHub_CancelledRunLeavesNoDeletionMark: a human cancel is distinguishable from a disruption", func() {
		repoSlug := creds.org + "/" + creds.repo

		By(fmt.Sprintf("dispatching the long-running workflow %q", creds.longWorkflow))
		before := workerSnapshot(tenantNS)
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
			var diag string
			podName, diag = runningWorkerForRun(g, tenantNS, runID, before)
			g.Expect(podName).NotTo(BeEmpty(), "no worker pod attributable to run %s in %s: %s", runID, tenantNS, diag)
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
		cancelledAt := time.Now()
		_, err := utils.Run(exec.Command("gh", "run", "cancel", runID, "--repo", repoSlug))
		Expect(err).NotTo(HaveOccurred(), "cancel run %s", runID)

		By("waiting for the worker pod to reach a terminal phase")
		// Budgeted past the fixture's own 600s sleep, deliberately. A cancel does not
		// reach this worker: the AGC owns the broker session and relays nothing to the
		// pod, so the runner keeps executing and GitHub force-concludes the job at its
		// own ~5-minute cancellation grace while the container is still going. Measured
		// 2026-07-29 with a 5-minute budget, this wait expired one second before the pod
		// would have been observable at all — so anything shorter than the sleep measures
		// the timeout rather than the pod.
		Eventually(func(g Gomega) {
			out, err := utils.Run(exec.Command("kubectl", "get", "pod", podName,
				"-n", tenantNS, "--ignore-not-found", "-o", "jsonpath={.status.phase}"))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(out)).To(BeElementOf("Succeeded", "Failed", ""),
				"pod is still %s", strings.TrimSpace(out))
		}, 13*time.Minute, 500*time.Millisecond).Should(Succeed())
		stopSampling()

		seq := observed.sequence()
		AddReportEntry("Q459 cancel-path pod phase/deletion sequence", strings.Join(seq, " -> "))
		AddReportEntry("Q459 cancel to worker terminal phase",
			time.Since(cancelledAt).Round(time.Second).String())

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
		// Recorded next to the pod's terminal time above so the two can be ordered.
		// They are not the same event: GitHub concludes the job when its cancellation
		// grace lapses, the pod ends when the runner's own step finishes, and how far
		// apart they sit is the size of the work the cancel failed to stop.
		AddReportEntry("Q459 cancel-path job cancelled_at / completed_at (UTC)",
			fmt.Sprintf("%s / %s", cancelledAt.UTC().Format(time.RFC3339), firstJobCompletedAt(Default, repoSlug, runID)))
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
// It prefers an exact match on the run-id annotation. That annotation is absent on
// this tier (Q495), so the fallback is what actually resolves the worker today: the
// caller snapshots the Running workers that existed *before* it dispatched, and a
// Running worker outside that snapshot is this run's by identity rather than by
// count. That is what lets these specs run back to back — a previous spec's worker
// lingering past its own run no longer makes the lookup ambiguous, which is why
// "wait for the namespace to be quiet first" is not needed and not done.
// Only a genuine ambiguity — no annotated match and several new Running workers —
// yields "", along with a description of what it saw. That case is reachable and was
// hit on 2026-07-29: a second live-GitHub session dispatched the same fixture workflow
// against the same repo, and this tenant's AGC acquired both jobs, so two workers
// appeared that were not there before. Nothing in the cluster can separate them
// without the run-id annotation, so the spec must fail — but it must fail saying so
// rather than timing out on an empty string (Q500, Q495).
func runningWorkerForRun(g Gomega, ns, runID string, preexisting map[string]bool) (podName, diag string) {
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
		return fields[0], ""
	}

	all := runningWorkers(g, ns)
	var fresh []string
	for _, pod := range all {
		name := strings.SplitN(pod, "=", 2)[0]
		if !preexisting[name] {
			fresh = append(fresh, pod)
		}
	}
	switch len(fresh) {
	case 1:
		AddReportEntry("Q459 worker matched without the run-id annotation",
			fmt.Sprintf("wanted run %s; sole worker new since dispatch is %s", runID, fresh[0]))
		return strings.SplitN(fresh[0], "=", 2)[0], ""
	case 0:
		return "", fmt.Sprintf("no worker has appeared since dispatch; Running workers now: %v", all)
	default:
		return "", fmt.Sprintf(
			"%d workers appeared since dispatch, so none can be attributed to run %s without the "+
				"run-id annotation (Q495). Most likely another live-GitHub session dispatched the same "+
				"fixture workflow and this AGC acquired its job too (Q500). New since dispatch: %v",
			len(fresh), runID, fresh)
	}
}

// runningWorkers returns every Running worker pod in the namespace as
// "<name>=<run-id annotation>", the annotation being empty when absent (Q495).
func runningWorkers(g Gomega, ns string) []string {
	out, err := utils.Run(exec.Command("kubectl", "get", "pods",
		"-n", ns,
		"-l", "app.kubernetes.io/managed-by=actions-gateway-controller",
		"--field-selector", "status.phase=Running",
		"-o", `jsonpath={range .items[*]}{.metadata.name}={.metadata.annotations.actions-gateway\.com/run-id} {end}`,
	))
	g.Expect(err).NotTo(HaveOccurred())
	return strings.Fields(out)
}

// workerSnapshot is the set of Running worker pod names at a moment in time, taken
// immediately before a dispatch so the worker that run produces can be told from
// whatever was already there.
func workerSnapshot(ns string) map[string]bool {
	GinkgoHelper()
	before := map[string]bool{}
	for _, pod := range runningWorkers(Default, ns) {
		before[strings.SplitN(pod, "=", 2)[0]] = true
	}
	return before
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

// firstJobCompletedAt returns the RFC3339 instant GitHub recorded the run's first
// job as completed, or "" while it is still running. It is the GitHub-side half of
// the cancel-path timeline; the pod-side half is sampled in cluster.
func firstJobCompletedAt(g Gomega, repoSlug, runID string) string {
	out, err := utils.Run(exec.Command("gh", "api",
		fmt.Sprintf("repos/%s/actions/runs/%s/jobs?filter=latest", repoSlug, runID),
		"--jq", ".jobs[0].completed_at"))
	g.Expect(err).NotTo(HaveOccurred())
	return strings.TrimSpace(out)
}

// firstJobStateForAttempt is firstJobState pinned to one attempt. The eviction spec
// reads attempt 1 while the AGC's accepted re-run (Q503) may already be starting
// attempt 2, at which point the "latest" filter stops naming the job under
// measurement.
func firstJobStateForAttempt(g Gomega, repoSlug, runID string, attempt int) (status, conclusion string) {
	out, err := utils.Run(exec.Command("gh", "api",
		fmt.Sprintf("repos/%s/actions/runs/%s/attempts/%d/jobs", repoSlug, runID, attempt)))
	g.Expect(err).NotTo(HaveOccurred())
	var resp struct {
		Jobs []struct {
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
		} `json:"jobs"`
	}
	g.Expect(json.Unmarshal([]byte(out), &resp)).To(Succeed(), "parse jobs response: %s", out)
	g.Expect(resp.Jobs).NotTo(BeEmpty(), "run %s attempt %d has no jobs yet", runID, attempt)
	return resp.Jobs[0].Status, resp.Jobs[0].Conclusion
}

// firstJobCompletedAtForAttempt is firstJobCompletedAt pinned to one attempt, for the
// same reason as firstJobStateForAttempt: the published latency figure must come from
// the interrupted attempt, not from whichever attempt is latest by the time it is read.
func firstJobCompletedAtForAttempt(g Gomega, repoSlug, runID string, attempt int) string {
	out, err := utils.Run(exec.Command("gh", "api",
		fmt.Sprintf("repos/%s/actions/runs/%s/attempts/%d/jobs", repoSlug, runID, attempt),
		"--jq", ".jobs[0].completed_at"))
	g.Expect(err).NotTo(HaveOccurred())
	return strings.TrimSpace(out)
}

// podPhaseReason returns a pod's "phase/reason", the projection eviction detection
// itself keys on.
func podPhaseReason(ns, name string) string {
	GinkgoHelper()
	out, err := utils.Run(exec.Command("kubectl", "get", "pod", name, "-n", ns,
		"--ignore-not-found", "-o", "jsonpath={.status.phase}/{.status.reason}"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// evictedWorkerNames returns the tenant's worker pods that the kubelet has evicted.
// Used as a guard rather than an assertion target: an eviction nobody asked for means
// whatever the spec measures next is not the eviction it performed.
func evictedWorkerNames(ns string) []string {
	GinkgoHelper()
	out, err := utils.Run(exec.Command("kubectl", "get", "pods", "-n", ns,
		"-l", "app.kubernetes.io/managed-by=actions-gateway-controller",
		"-o", `jsonpath={range .items[?(@.status.reason=="Evicted")]}{.metadata.name} {end}`))
	if err != nil {
		return nil
	}
	return strings.Fields(out)
}

// podEphemeralStorageLimit returns the runner container's ephemeral-storage limit, or
// "" when it declares none.
func podEphemeralStorageLimit(ns, name string) string {
	GinkgoHelper()
	out, err := utils.Run(exec.Command("kubectl", "get", "pod", name, "-n", ns,
		"-o", `jsonpath={.spec.containers[?(@.name=="runner")].resources.limits.ephemeral-storage}`))
	Expect(err).NotTo(HaveOccurred())
	return strings.TrimSpace(out)
}

// overflowEphemeralStorage writes a file large enough to carry the worker past its
// ephemeral-storage limit, and returns what the write reported.
//
// It writes into /tmp rather than a mounted volume because a worker pod has none that
// the runner owns: its only volumes are read-only secret and image mounts, so the
// container's writable layer is where a real job's output lands too. Both count
// toward the same pod-level limit, which is what the kubelet enforces.
func overflowEphemeralStorage(ns, podName string, sizeMiB int) string {
	GinkgoHelper()
	out, err := utils.Run(exec.Command("kubectl", "exec", "-n", ns, podName, "-c", "runner", "--",
		"sh", "-c", fmt.Sprintf("dd if=/dev/zero of=/tmp/q396-fill bs=1M count=%d 2>&1 | tail -1", sizeMiB)))
	Expect(err).NotTo(HaveOccurred(), "fill worker %s past its ephemeral-storage limit", podName)
	return strings.TrimSpace(out)
}

// evictionFacts returns when the kubelet actually killed the worker's container, the
// exit code it died with, and the message the kubelet recorded for the eviction.
//
// The timestamp is the container's own terminated.finishedAt — the kubelet's record of
// the kill, not this spec's record of noticing it. When a pod is evicted before its
// container statuses are written the field can be absent; the Evicted Event's timestamp
// is the fallback, at second granularity, and the caller sees which one it got from the
// message that comes back with it.
//
// The exit code is what distinguishes this disruption from every graceful one: an
// ephemeral-storage eviction kills with no grace period, so the runner dies on SIGKILL
// (137) with nothing relayed and nothing reported to GitHub.
func evictionFacts(ns, name string) (killedAt time.Time, exitCode int, message string) {
	GinkgoHelper()
	exitCode = -1
	out, err := utils.Run(exec.Command("kubectl", "get", "pod", name, "-n", ns,
		"-o", "jsonpath={.status.containerStatuses[?(@.name=='runner')].state.terminated.finishedAt}"+
			"|{.status.containerStatuses[?(@.name=='runner')].state.terminated.exitCode}|{.status.message}"))
	Expect(err).NotTo(HaveOccurred())
	parts := strings.SplitN(strings.TrimSpace(out), "|", 3)
	if len(parts) == 3 {
		if code, convErr := strconv.Atoi(strings.TrimSpace(parts[1])); convErr == nil {
			exitCode = code
		}
		message = strings.TrimSpace(parts[2])
	}
	if len(parts) > 0 && parts[0] != "" {
		t, parseErr := time.Parse(time.RFC3339, parts[0])
		if parseErr == nil {
			return t, exitCode, message
		}
	}

	evt, evtErr := utils.Run(exec.Command("kubectl", "get", "events", "-n", ns,
		"--field-selector", "involvedObject.name="+name+",reason=Evicted",
		"-o", "jsonpath={.items[0].lastTimestamp}"))
	Expect(evtErr).NotTo(HaveOccurred())
	t, parseErr := time.Parse(time.RFC3339, strings.TrimSpace(evt))
	Expect(parseErr).NotTo(HaveOccurred(),
		"neither the container's finishedAt nor an Evicted Event carries a usable kill time; "+
			"there is no server-side timestamp to measure latency from")
	return t, exitCode, message + " [timestamp from the Evicted Event, not the container status]"
}

// agcEvictionLog returns the AGC's log lines about eviction handling. Scoped to the
// handful of messages handleEviction emits so the report entry is readable, rather
// than the whole controller log.
func agcEvictionLog(ns string) string {
	GinkgoHelper()
	out, err := utils.Run(exec.Command("kubectl", "logs", "-n", ns,
		"deployment/"+agcName, "--tail=-1"))
	Expect(err).NotTo(HaveOccurred(), "read AGC logs in %s", ns)
	var kept []string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "evicted") || strings.Contains(line, "eviction") ||
			strings.Contains(line, "disrupt") || strings.Contains(line, "re-run") {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
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
