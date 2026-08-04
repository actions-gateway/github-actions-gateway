//go:build e2e
// +build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
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

		// The Q422 sibling gateway: a second AGC on the same GitHub runner group, in
		// its own namespace because a ResourceQuota is namespaced and the experiment
		// needs one tenant with headroom and one without.
		//
		// The name is agName plus a suffix, and both halves of that matter. It must
		// DIFFER from agName because agentpool derives its runner names from the
		// gateway name, and two gateways registering one name take the 409 conflict
		// path where each deregisters the other (Q511). It must EXTEND agName because
		// the runner names this suite is accountable for are identified by that
		// prefix — a sibling named independently would be invisible to a
		// stranded-runner sweep and register runners nothing knows to clean up.
		siblingNS     = "tenant-github-sibling"
		siblingAGName = agName + "-sib"
		// quotaFullName is the ResourceQuota this spec fills the primary tenant's
		// namespace with.
		quotaFullName = "q422-full"
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

		By("verifying no other live-GitHub run holds the fixture repo")
		preflightFixtureRepoIdle(creds.org+"/"+creds.repo, agName)

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

			// The scale-set tenant runs its own AGC in its own namespace, and dumping
			// only the classic one is why a scale-set failure on 2026-08-03 could not be
			// diagnosed after the cluster was gone: the log that would have said whether
			// the runner reported was never captured. Best-effort — the tenant does not
			// exist unless its spec got far enough to create it.
			cmd = exec.Command("kubectl", "logs", "-n", scaleSetTenantNS,
				"deployment/"+scaleSetAGCDeploy, "--tail=300")
			if out, err := utils.Run(cmd); err == nil {
				_, _ = fmt.Fprintln(GinkgoWriter, "--- scale-set tenant AGC ---")
				_, _ = fmt.Fprintln(GinkgoWriter, out)
			}
		}
		// The scale-set tenant is created inside its own spec rather than here, so this
		// has to cope with it never having existed — a spec that failed earlier in the
		// container leaves nothing to delete. It goes first because it registers runners
		// on the shared fixture repo, and its AGC must still be up to deregister them:
		// stranded registrations keep accepting assignments and block the next run's
		// preflight.
		deleteScaleSetTenant()
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
	// (Q510): the AGC retries a refused re-run until GitHub concludes the run and
	// accepts it (Q503), so "the re-run landed and a second attempt ran" is a
	// property this tier is required to have — a spec that records a refusal and
	// passes can neither verify that nor catch its regression (testing.md §
	// negative assertions).
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

		By("confirming the worker carries the run identity recovery needs")
		// Q544. The run-id half is proven by the lookup above having resolved at all;
		// this is the other key handleEviction reads. The re-run assertions at the
		// bottom of this spec cannot pass without both, so checking here attributes the
		// cause rather than leaving it to be inferred from a silent recovery.
		assertWorkerCarriesRepository(tenantNS, podName)

		By("confirming the worker carries the ephemeral-storage cap this spec overshoots")
		// Without the cap there is nothing to exceed, the kubelet never evicts, and the
		// spec would time out on the eviction wait for a reason that has nothing to do
		// with eviction. Check the premise before acting on it.
		Expect(podEphemeralStorageLimit(tenantNS, podName)).To(Equal(workerEphemeralStorageLimit),
			"worker pod carries no ephemeral-storage limit; the eviction lever is absent")

		By("streaming the worker's logs, so anything it manages to say on the way out is captured")
		// Whether the runner says anything is the outcome under measurement, not a
		// foregone conclusion: an ephemeral-storage eviction leaves a window measured at
		// about two seconds, which the runner sometimes clears (Q657). "The runner said
		// nothing" is what makes GitHub's own detection the only thing that can conclude
		// the job, so it has to be observed rather than assumed. The stream loses the last
		// instant before the kill; evictedWorkerLog re-reads the log whole afterwards.
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
		// Two captures that fail in different ways, so the verdict can tell "the relay did
		// not run" from "nothing was readable" (Q657). The follower is the one that reaches
		// the last instant before the kill; the post-hoc read corroborates it when the
		// kubelet has not already reclaimed the container.
		followerLog := relayLog.stopAndRead()
		AddReportEntry("Q396 worker log tail across the eviction", followerLog)
		podLog, podLogOK := evictedWorkerLog("Q396", tenantNS, podName)
		workerLogs := []workerLogSource{
			{name: "follower", log: followerLog, ok: strings.TrimSpace(followerLog) != ""},
			{name: "pod", log: podLog, ok: podLogOK},
		}

		By("confirming the kubelet killed the runner rather than asking it to stop")
		// The Queue row's "runner genuinely killed", made an assertion. 137 is SIGKILL,
		// which is what says the kubelet took the container rather than the job ending on
		// its own — a graceful exit code here would mean this spec measured something
		// other than the eviction it performed.
		//
		// It does NOT say the runner was silent. A runner that reports and then overruns
		// its grace is SIGKILLed too, and both 2026-08-03 scale-set runs exited 137 while
		// splitting 9m38s against 17s to conclusion. What escaped is recorded below, from
		// the runner's own log and GitHub's step records, rather than inferred from here.
		Expect(exitCode).To(Equal(137),
			"the evicted runner exited %d, not SIGKILL; the kubelet did not take this container", exitCode)

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
		recordEvictionOutcome("Q396", concludedAt.Sub(evictedAt), exitCode, workerLogs,
			firstJobStepRecords(repoSlug, runID, 1))

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

		By("asserting the accepted re-run is the one the retry loop landed, not the first call")
		// The distinction Q503 is actually about. An accepted re-run alone does not
		// separate "the loop outlasted GitHub's refusal" from "GitHub accepted
		// immediately", and only the first is the behaviour that shipped. The check is
		// conditional on the latency measured above, because whether there was a refusal
		// to absorb is decided by whether the runner reported — see assertRerunLanded.
		assertRerunLanded(agcEvictionLog(tenantNS), concludedAt.Sub(evictedAt), "Q503")

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
			// Unconditional — same reasoning as the eviction spec's deferred cancel.
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
		assertWorkerCarriesRepository(tenantNS, podName)

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
	// # The second measurement it now carries (Q501 candidate A)
	//
	// The cheapest candidate trigger for relaying a cancellation is the renew loop the
	// AGC already runs every 60s: if the run service stops honouring renewjob once
	// GitHub concludes a cancelled job, Q254's teardown fires on its own and Q501's
	// actuator reclaims the worker, and no new mechanism is needed. The 2026-07-29 run
	// cannot answer that — the actuator did not exist yet, so a renew-loop teardown at
	// ~5 minutes and no teardown at all looked identical from outside. This spec now
	// records the renew loop's own lines across the cancel window, and the pod carries
	// the corroborating half: a reclaim stamps the worker job_abandoned before deleting
	// it, so the answer is positive either way rather than an absent log line.
	//
	// That is also why the deletion assertion is about an *unaccounted* mark rather
	// than about no mark at all. Once the actuator can delete this worker, "terminal
	// with no deletionTimestamp" stops being the only safe shape, and demanding it
	// would fail the spec for the gateway working.
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
		assertWorkerCarriesRepository(tenantNS, podName)

		By("sampling the pod's phase, deletionTimestamp and deletion-reason across the cancellation")
		// All four fields together, sampled as it happens: the claim is about what is
		// true of the pod at the moment its terminal phase publishes, which is a
		// state that exists only briefly and cannot be read afterwards. The
		// deletion-reason stamp joins the other three because a deletion mark alone no
		// longer says "disruption" — the AGC issues its own deletes on this path since
		// Q501's actuator, and only the stamp tells the two apart.
		observed := newFieldRecorder(tenantNS, podName,
			"{.status.phase}/{.status.reason}"+
				"/deleting={.metadata.deletionTimestamp}"+
				"/by={.metadata.annotations['"+annotationDeletionReason+"']}")
		stopSampling := observed.start(200 * time.Millisecond)

		By("baselining the renew loop's failure lines before the cancel")
		// Q501 candidate A. The AGC's log is cumulative across every spec in this
		// container, so the window this spec is accountable for is the suffix past
		// these counts. Renewal SUCCESS is silent, so there is no pre-cancel control
		// line to read — what makes the outcome positive either way is the pod, below.
		renewWarnBase := len(agcLogLines(tenantNS, renewNonFatalLine))
		renewTeardownBase := len(agcLogLines(tenantNS, renewTeardownLine))

		By("cancelling the run in GitHub, the way a human would")
		cancelledAt := time.Now()
		_, err := utils.Run(exec.Command("gh", "run", "cancel", runID, "--repo", repoSlug))
		Expect(err).NotTo(HaveOccurred(), "cancel run %s", runID)

		By("waiting for the worker pod to reach a terminal phase or be reclaimed")
		// Budgeted past the fixture's own 600s sleep, deliberately. On the measured
		// outcome a cancel does not reach this worker: the AGC owns the broker session
		// and relays nothing to the pod, so the runner keeps executing and GitHub
		// force-concludes the job at its own ~5-minute cancellation grace while the
		// container is still going. Measured 2026-07-29 with a 5-minute budget, this
		// wait expired one second before the pod would have been observable at all — so
		// anything shorter than the sleep measures the timeout rather than the pod.
		//
		// The empty phase is the other outcome, and the one Q501 candidate A predicts:
		// if renewjob starts failing definitively once GitHub concludes the cancelled
		// job, the renew loop abandons it and the actuator deletes the worker, so the
		// pod is gone rather than terminal. Both end this wait; which one happened is
		// read off the sampled sequence below.
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

		By("recording what the renew loop did across the cancel window (Q501 candidate A)")
		renewWarns := sinceBaseline(agcLogLines(tenantNS, renewNonFatalLine), renewWarnBase)
		renewTeardowns := sinceBaseline(agcLogLines(tenantNS, renewTeardownLine), renewTeardownBase)
		AddReportEntry("Q501 renewjob failures in the cancel window", strings.Join(renewWarns, "\n"))
		AddReportEntry("Q501 renewjob definitive losses in the cancel window", strings.Join(renewTeardowns, "\n"))

		By("asserting the cancelled worker published no deletion mark the gateway cannot account for")
		// The discriminator, and the property that must hold whichever way the
		// measurement above goes. Q502 re-runs a worker it finds terminal carrying a
		// deletionTimestamp that no AGC delete stamped; a cancelled run whose worker
		// published that shape would be re-run against an operator's explicit stop.
		//
		// Two shapes are safe, and both are real outcomes on this path. A terminal
		// phase with no deletion mark is the 2026-07-29 result — nothing reached the
		// worker and the runner ran the fixture's sleep out. A mark stamped
		// job_abandoned is the renew loop giving up on the job (Q254) and Q501's
		// actuator reclaiming its pod, which is candidate A answering yes; the pod may
		// then be gone before any terminal phase publishes, so that shape counts
		// wherever in the sequence it appears.
		var unstampedTerminal []string
		var endedByItself, reclaimed bool
		for _, s := range seq {
			terminal, deletedAt, by := deletionMark(s)
			if by == reapReasonJobAbandoned {
				reclaimed = true
			}
			if !terminal {
				continue
			}
			switch {
			case deletedAt == "":
				endedByItself = true
			case by == "":
				unstampedTerminal = append(unstampedTerminal, s)
			}
		}
		Expect(seq).NotTo(BeEmpty(), "the sampler saw nothing; the measurement did not run")
		Expect(unstampedTerminal).To(BeEmpty(),
			"a cancelled run's worker published a terminal phase carrying a deletionTimestamp that no AGC "+
				"delete stamped — the exact shape Q502 recovers — so a human cancel is NOT distinguishable "+
				"from a drain at the pod. Observed: %v", seq)
		Expect(endedByItself || reclaimed).To(BeTrue(),
			"the sampler recorded neither a terminal phase nor a gateway reclaim, so nothing here says how "+
				"the cancelled worker ended. Observed: %v", seq)

		candidateA := "renewjob did NOT report the loss — the worker ran on and ended by itself, " +
			"so candidate B (a REST run-status poll) is the remaining trigger"
		switch {
		case reclaimed:
			candidateA = "renewjob DID report the loss — the renew loop abandoned the job and the actuator " +
				"reclaimed the worker, so no new trigger is needed"
		case len(renewTeardowns) > 0:
			candidateA = "renewjob reported the loss, but not before the worker was already gone — " +
				"read the teardown lines against the terminal time above"
		}
		AddReportEntry("Q501 candidate A outcome", candidateA)

		_, conclusion := firstJobState(Default, repoSlug, runID)
		AddReportEntry("Q459 cancel-path job conclusion", conclusion)
		// Recorded next to the pod's terminal time above so the two can be ordered.
		// They are not the same event: GitHub concludes the job when its cancellation
		// grace lapses, the pod ends when the runner's own step finishes, and how far
		// apart they sit is the size of the work the cancel failed to stop.
		AddReportEntry("Q459 cancel-path job cancelled_at / completed_at (UTC)",
			fmt.Sprintf("%s / %s", cancelledAt.UTC().Format(time.RFC3339), firstJobCompletedAt(Default, repoSlug, runID)))
	})

	// Experiment 4 half B (Q422): the premise the pre-claim quota rung rests on, at
	// the only tier that can be asked about it.
	//
	// The rung (#784) refuses to CLAIM a job the namespace ResourceQuota cannot house,
	// on the premise that GitHub then redelivers it to a sibling session with room.
	// Half A (cmd/agc/internal/controller/integration/q422_quota_admission_test.go)
	// proved the refusal against a real apiserver — acquirejob is never called,
	// nothing is staged, the ceiling budget is untouched. It cannot prove the premise:
	// a fake broker redelivers because it was written to, so asserting redelivery
	// there restates the fake rather than GitHub. Only live GitHub answers whether an
	// unclaimed delivery comes back, and whether a sibling gateway then runs it.
	//
	// # Why the sibling arrives late
	//
	// Both gateways register into the org's Default runner group under the same `e2e`
	// label, so GitHub may offer the job to either. With both up at dispatch the
	// sibling could take it on the first offer and the blocked gateway would never see
	// it — the spec would pass without the decline that is its whole subject. Standing
	// the sibling up only after the decline is observed makes the ordering a property
	// of this spec rather than of GitHub's routing.
	//
	// # What proves the job was never claimed
	//
	// Not "no worker pod appeared". A claimed job whose pod the quota then rejects
	// also leaves no pod, and that claim-and-stall is exactly what the rung exists to
	// prevent — the trap half A names for the same reason. The discriminator is the
	// backstop one layer down: createPodWithQuotaRetry logs "pod creation blocked by
	// namespace quota" at Info on every quota-rejected create, so its ABSENCE, beside
	// the gate's own decline line, says the job was left at GitHub instead of claimed
	// and abandoned.
	//
	// # Why the quota is on `pods`
	//
	// It is the one quota key that constrains a worker without also constraining how
	// the tenant's control plane is admitted: a `requests.cpu` quota rejects any pod
	// that declares no CPU request, which is what makes a LimitRange a prerequisite
	// for quota'd tenants (Q262). Filling `pods` to the namespace's current occupancy
	// models a busy namespace the way half A's `hard − used` arithmetic does, rather
	// than declaring a ceiling too small to ever fit a worker.
	It("E2E_GitHub_QuotaBlockedJobRunsOnSibling: a declined job is redelivered to a sibling gateway", func() {
		repoSlug := creds.org + "/" + creds.repo

		By("waiting until no worker pod in the tenant still counts against its quota")
		// The quota below is sized from live occupancy, so it must be taken when
		// occupancy is at its floor. A worker left over from an earlier spec — the
		// re-runs above outlive their own specs, since cancelling a run does not stop
		// its runner (Q501) — would be counted in, and its later exit would hand the
		// gate exactly the one pod of headroom this spec needs it not to have.
		Eventually(func(g Gomega) {
			g.Expect(liveWorkerPods(g, tenantNS)).To(BeEmpty(), "worker pods from an earlier spec are still running")
		}, 12*time.Minute, 10*time.Second).Should(Succeed())

		By("filling the tenant's namespace quota so it has room for exactly zero more pods")
		occupied := nonTerminalPodCount(Default, tenantNS)
		Expect(occupied).To(BeNumerically(">", 0), "the tenant namespace runs no pods, so its AGC is not up")
		fillPodQuota(tenantNS, quotaFullName, occupied)
		DeferCleanup(func() {
			_, _ = utils.Run(exec.Command("kubectl", "delete", "resourcequota", quotaFullName,
				"-n", tenantNS, "--ignore-not-found"))
		})
		AddReportEntry("Q422 quota filled at", fmt.Sprintf("pods=%d, all occupied", occupied))

		By("waiting for the RunnerGroup to report WorkerQuotaExceeded=True")
		// The gate reads the quota through the manager's informer cache, and this
		// condition is computed from that same read against the same one-worker
		// footprint. It flipping True is what says the gate now sees zero headroom —
		// dispatching before it does would race the cache.
		Eventually(func(g Gomega) {
			g.Expect(runnerGroupCondition(g, tenantNS, "WorkerQuotaExceeded")).To(Equal("True"))
		}, 3*time.Minute, 5*time.Second).Should(Succeed())

		By("confirming the namespace stays full rather than settling back into headroom")
		// The ceiling was sized from live occupancy, so anything that vacates a pod —
		// the proxy HPA scaling back to its floor, most plausibly — hands the gate the
		// one pod of room this spec needs it not to have. Held here, up front, so that
		// churn fails as itself instead of surfacing later as "the blocked gateway
		// provisioned a worker".
		Consistently(func(g Gomega) {
			g.Expect(runnerGroupCondition(g, tenantNS, "WorkerQuotaExceeded")).To(Equal("True"))
		}, 30*time.Second, 5*time.Second).Should(Succeed())

		// Baselines, all taken before the dispatch: the AGC serves every spec in this
		// container, so its log is cumulative and the namespace already holds the
		// terminal worker pods those specs left behind. Only the deltas are this
		// spec's.
		declinesBefore := countAGCLogLines(tenantNS, admissionDeclinedLine)
		quotaRetriesBefore := countAGCLogLines(tenantNS, quotaRetryLine)
		blockedWorkersBefore := allWorkerPods(Default, tenantNS)

		By(fmt.Sprintf("dispatching %q with the only gateway on this runner group out of quota", creds.workflow))
		dispatchedAt := time.Now()
		runID := dispatchAndResolveRun(repoSlug, creds.workflow)
		AddReportEntry("Q422 workflow run", fmt.Sprintf("https://github.com/%s/actions/runs/%s", repoSlug, runID))
		defer func() {
			// Unconditional. A run this spec abandons stays queued at GitHub for hours
			// and is the state the next live-GitHub run's preflight has to clear.
			_, _ = utils.Run(exec.Command("gh", "run", "cancel", runID, "--repo", repoSlug))
		}()

		By("waiting for the blocked gateway to decline a delivery for quota")
		Eventually(func(g Gomega) {
			g.Expect(countAGCLogLines(tenantNS, admissionDeclinedLine)-declinesBefore).
				To(BeNumerically(">=", 1), "the gateway has not declined a delivery yet")
		}, 8*time.Minute, 10*time.Second).Should(Succeed())

		admissionLog := agcAdmissionLog(tenantNS)
		AddReportEntry("Q422 blocked gateway admission log", admissionLog)

		By("asserting the decline named quota, not the configured ceiling")
		// The group declares no maxWorkers, so a `ceiling` decline here would mean the
		// gate refused for a reason this experiment did not arrange. Both encodings:
		// the AGC ships a JSON handler, but a text handler renders the same attribute
		// as reason=quota and the assertion is about the value.
		Expect(admissionLog).To(SatisfyAny(ContainSubstring(`"reason":"quota"`), ContainSubstring("reason=quota")),
			"the gateway declined, but not for quota. Log:\n%s", admissionLog)
		Expect(admissionLog).To(ContainSubstring("no namespace ResourceQuota headroom for another worker pod"),
			"the quota rung did not report what it read. Log:\n%s", admissionLog)

		By("asserting the job was left at GitHub rather than claimed and abandoned")
		// See the spec's header: this, not the absence of a pod, is what separates the
		// pre-claim rung from the post-claim backstop it exists to make unnecessary.
		Expect(countAGCLogLines(tenantNS, quotaRetryLine)).To(Equal(quotaRetriesBefore),
			"the gateway claimed the job and then hit the quota on pod creation — the pre-claim rung "+
				"did not fire, and the job is claim-and-stalled")

		By("confirming GitHub still holds the job as queued")
		queuedStatus, _ := firstJobState(Default, repoSlug, runID)
		Expect(queuedStatus).To(Equal("queued"),
			"the declined job is %q rather than queued, so nothing is left for a sibling to pick up", queuedStatus)

		By("bringing up a sibling gateway on the same runner group, with quota headroom")
		utils.CreateNamespace(siblingNS, nil)
		utils.CreateGitHubAppSecret(siblingNS, secretName, creds.appID, creds.installationID, creds.privateKeyPEM)
		utils.RunnerTenant(siblingNS, siblingAGName, secretName, workerImage).ApplyWithWebhookRetry()
		DeferCleanup(func() {
			// The CR first and the namespace second, while the AGC is still up: the
			// agentpool-cleanup finalizer is what deregisters this gateway's runners,
			// and only its own AGC can clear it. Deleting the namespace first strands
			// registrations that go on taking job assignments (Q511).
			if CurrentSpecReport().Failed() {
				out, _ := utils.Run(exec.Command("kubectl", "logs", "-n", siblingNS,
					"deployment/"+agcName, "--tail=300"))
				_, _ = fmt.Fprintln(GinkgoWriter, out)
			}
			utils.DeleteActionsGatewayCR(siblingNS, siblingAGName)
			utils.DeleteNamespace(siblingNS)
		})
		utils.WaitForDeploymentReady(siblingNS, agcName, 5*time.Minute)
		// Deployment readiness is only the health server binding. The sibling cannot be
		// offered anything until its token fetch, agent registration and listener
		// multiplexer are all up, which is what observedGeneration reports (Q134) — and
		// without this gate that whole startup would be charged to the worker wait below
		// and surface as "the sibling never picked the job up".
		utils.WaitForRunnerGroupReconciled(siblingNS, 5*time.Minute)

		By("waiting for the sibling to provision a worker for the job the blocked gateway declined")
		// Any phase, not Running: the fixture job is an echo, so its worker can reach
		// Succeeded between two polls. Its namespace is new, so any worker pod in it is
		// this run's.
		var siblingWorker string
		Eventually(func(g Gomega) {
			pods := allWorkerPods(g, siblingNS)
			g.Expect(pods).NotTo(BeEmpty(), "the sibling has not provisioned a worker")
			siblingWorker = pods[0]
		}, 10*time.Minute, 10*time.Second).Should(Succeed())
		AddReportEntry("Q422 sibling worker pod", siblingWorker)
		AddReportEntry("Q422 dispatch -> sibling worker", time.Since(dispatchedAt).Round(time.Second).String())

		By("waiting for the run to complete with conclusion=success")
		// The deliverable. Redelivery that reaches a worker but not a green job would
		// leave the rung's premise half-proven.
		Eventually(func(g Gomega) {
			status, conclusion := firstJobState(g, repoSlug, runID)
			g.Expect(status).To(Equal("completed"), "job is still %q", status)
			g.Expect(conclusion).To(Equal("success"), "job concluded %q", conclusion)
		}, 10*time.Minute, 10*time.Second).Should(Succeed())

		By("asserting the blocked gateway provisioned nothing throughout")
		// A delta, not an absolute: the specs above leave terminal worker pods in this
		// namespace, and none of them is a claim this spec's dispatch produced.
		Expect(allWorkerPods(Default, tenantNS)).To(ConsistOf(blockedWorkersBefore),
			"the out-of-quota gateway provisioned a worker for a job it had declined")

		AddReportEntry("Q422 outcome",
			"the out-of-quota gateway declined the delivery without claiming it, GitHub held the job queued, "+
				"and a sibling gateway on the same runner group ran it to success")
	})

	// The scale-set half of Q396's experiment 1. The classic half is measured above; this
	// runs the same disruption against the other acquisition tier and asks whether the
	// two findings that came out of it — GitHub's ~10-minute conclusion latency after an
	// ungraceful kill, and the re-run refusal that latency implies — reproduce there.
	//
	// Q417 plumbed recovery onto scale-set from the owning reconciler rather than from a
	// job goroutine, so the detection path is genuinely different code. The 403 is
	// tier-independent by construction — it is a property of the API and of the delay,
	// not of how the AGC noticed — but that is reasoning, and this is the tier where it
	// stops being reasoning.
	//
	// # Placed last, deliberately
	//
	// This is the only spec in the container that stands up a tenant on a path no
	// live-GitHub run has ever exercised: the AGC's scale-set listener has never
	// bootstrapped against real GitHub from inside a cluster. cmd/probe's Investigation E
	// proves the wire protocol from a standalone binary, which is a different question.
	// Ordered containers run specs in declaration order, so putting it here means a
	// failure to register a scale set costs this measurement and nothing above it.
	//
	// # Why it carries its own label
	//
	// Being last in an Ordered container means everything ahead of it is a gate: Ginkgo
	// skips the remainder of the container after a failure, so one unrelated spec failing
	// costs this measurement the whole run. That happened twice on 2026-08-03 — once to
	// the suite budget elapsing, once to the cancel spec resolving the wrong worker — and
	// both times the ~55 minutes ahead of it were spent for nothing. The scaleset-live
	// label lets it be run on its own (SUITE=live-github-scaleset) once the container
	// above is known green, without re-paying for six specs to prove it.
	//
	// # Why the bootstrap is asserted separately
	//
	// Without it, a listener that never opened its session would surface as "the job
	// stayed queued for ten minutes" — indistinguishable from GitHub being slow, from a
	// label mismatch, and from the runner group's public-repository rule. The Degraded
	// condition the listener publishes on a successful Start says which of those it was,
	// so it is checked before anything is dispatched.
	It("E2E_GitHub_ScaleSetEvictedWorkerLatencyAndRerun: the same measurement on the scale-set tier", Label(scaleSetLiveLabel), func() {
		repoSlug := creds.org + "/" + creds.repo
		orgURL := fmt.Sprintf("https://github.com/%s/%s", creds.org, creds.repo)

		// Every runner this scale set registers is named "<scaleSetLabel>-<jobID>", and
		// the suite recognizes its own registrations by the agName prefix alone. A label
		// that stopped extending agName would strand runners on the shared fixture repo
		// that neither the preflight nor `make e2e-github-cleanup` can see — so the
		// relation is asserted here rather than left to the const's comment.
		Expect(scaleSetLabel).To(HavePrefix(agName+"-"),
			"the scale-set label must extend %q or its runner registrations become invisible "+
				"to suiteRunnerPrefixes (Q511)", agName)

		By("standing up the v2 scale-set tenant against real GitHub")
		utils.CreateNamespace(scaleSetTenantNS, map[string]string{
			// The GMC's dual-reading ValidatingAdmissionPolicies only admit v2
			// provisioning in a namespace marked as a managed tenant.
			"actions-gateway.com/tenant": "managed",
		})
		utils.CreateGitHubAppSecret(scaleSetTenantNS, scaleSetSecretName,
			creds.appID, creds.installationID, creds.privateKeyPEM)
		Expect(utils.ApplyManifestWithWebhookRetry(
			scaleSetLiveManifest(scaleSetTenantNS, orgURL, scaleSetSecretName,
				workerImage, workerEphemeralStorageLimit))).To(Succeed())

		By("waiting for the per-gateway AGC Deployment to become ready")
		utils.WaitForDeploymentReady(scaleSetTenantNS, scaleSetAGCDeploy, 5*time.Minute)

		By("asserting the listener registered a scale set and opened a session at real GitHub")
		// The risky step, asserted as itself. Listener.Start ensures the scale set and
		// opens the session synchronously and only then publishes Degraded=False with
		// this reason, so the condition means both halves of the bootstrap succeeded
		// against dotcom — not merely that the AGC process is up.
		Eventually(func(g Gomega) {
			status, reason := runnerSetCondition(g, scaleSetTenantNS, scaleSetName, "Degraded")
			g.Expect(status).To(Equal("False"), "Degraded=%s/%s", status, reason)
			g.Expect(reason).To(Equal("SessionAuthorized"), "Degraded=False for %q, not an open session", reason)
		}, 5*time.Minute, 10*time.Second).Should(Succeed(), func() string {
			// A closure, not a string: Gomega evaluates a plain description eagerly, so
			// the dump would be taken before the wait began and would always read "no
			// conditions published" — a message that describes the start of the wait
			// rather than the failure at the end of it.
			return "the scale-set listener never opened a session against real GitHub, so nothing " +
				"below could measure an eviction. RunnerSet conditions:\n" +
				runnerSetConditionDump(scaleSetTenantNS, scaleSetName)
		})

		By(fmt.Sprintf("dispatching %q on the scale set's own label %q", creds.longWorkflow, scaleSetLabel))
		// The scale set's name IS its runs-on label, so this dispatch cannot reuse the
		// classic tenant's: the fixture takes the label as an input for exactly this.
		runID := dispatchAndResolveRun(repoSlug, creds.longWorkflow, "runner="+scaleSetLabel)
		AddReportEntry("Q396 scale-set workflow run", fmt.Sprintf("https://github.com/%s/actions/runs/%s", repoSlug, runID))
		defer func() {
			// Unconditional, same reasoning as the classic spec: the fixture sleeps ten
			// minutes and the re-run this spec provokes sleeps ten more.
			_, _ = utils.Run(exec.Command("gh", "run", "cancel", runID, "--repo", repoSlug))
		}()

		By("waiting for GitHub to report the job in_progress")
		// A real runner has to be executing a real job before it is interrupted. On this
		// tier reaching in_progress also proves the acquisition half end to end — the
		// listener polled its queue, took the assignment, minted a JIT config and the
		// provisioner built a worker that registered and picked the job up.
		Eventually(func(g Gomega) {
			// An eviction this spec did not cause is indistinguishable from a slow start
			// when read from GitHub, and invalidates everything below it. Naming it here
			// turns a ten-minute "still queued" timeout into an immediate, correctly
			// attributed failure.
			g.Expect(evictedWorkerNames(scaleSetTenantNS)).To(BeEmpty(),
				"a scale-set worker was evicted before its job started, so nothing below would "+
					"be measuring the eviction this spec performs. Either the node is under "+
					"pressure from another workload, or %s is too little headroom",
				workerEphemeralStorageLimit)
			status, _ := firstJobState(g, repoSlug, runID)
			g.Expect(status).To(Equal("in_progress"), "job is %q", status)
		}, 10*time.Minute, 10*time.Second).Should(Succeed())

		By("locating the scale-set worker running that job")
		var podName string
		Eventually(func(g Gomega) {
			var diag string
			podName, diag = scaleSetWorkerForRun(g, scaleSetTenantNS, runID)
			g.Expect(podName).NotTo(BeEmpty(), "%s", diag)
		}, 3*time.Minute, 2*time.Second).Should(Succeed())
		AddReportEntry("Q396 scale-set evicted worker pod", podName)

		By("confirming the worker really is a scale-set worker carrying its run identity")
		// The protocol label separates this from a classic worker that happened to be in
		// the namespace, and the repository annotation is the other half of what
		// handleEviction needs. On this tier run identity arrives with the assignment
		// message and nowhere else, so its absence would make recovery inert no matter
		// what the rest of the spec observed.
		Expect(podLabel(scaleSetTenantNS, podName, "actions-gateway.com/acquisition-protocol")).
			To(Equal("ScaleSet"), "worker %s was not provisioned by the scale-set path", podName)
		Expect(podAnnotation(scaleSetTenantNS, podName, "actions-gateway.com/repository")).
			NotTo(BeEmpty(), "scale-set worker %s carries no repository annotation (Q417)", podName)
		Expect(podEphemeralStorageLimit(scaleSetTenantNS, podName)).To(Equal(workerEphemeralStorageLimit),
			"worker pod carries no ephemeral-storage limit; the eviction lever is absent")

		By("confirming this worker is the one GitHub says is executing the job")
		// Measured 2026-08-03: the run identity is NOT sufficient to name the worker. A
		// listener that has provisioned more than one worker attributed to the run leaves
		// several candidates, and the lookup above returns whichever the API listed first.
		// On that run it returned an idle worker while the job executed on a sibling, and
		// everything downstream still looked healthy — the AGC detected the eviction and
		// attributed it to the right run — while the latency being measured was the cost
		// of killing a runner that had no job to lose.
		//
		// GitHub's own runner_name is the tie-break, and it is the only source that knows
		// which runner is executing: the scale-set runner is named "<scaleSetName>-<jobID>"
		// and the worker pod embeds that same jobID, so the two can be reconciled.
		Eventually(func(g Gomega) {
			g.Expect(scaleSetJobRunnerName(g, repoSlug, runID)).NotTo(BeEmpty(),
				"GitHub reports the job in_progress but names no runner yet")
		}, 2*time.Minute, 5*time.Second).Should(Succeed())
		assertWorkerRunsTheJob(scaleSetTenantNS, podName,
			scaleSetJobRunnerName(Default, repoSlug, runID))

		relayLog := followPodLogs(scaleSetTenantNS, podName)
		observed := newPhaseRecorder(scaleSetTenantNS, podName)
		stopSampling := observed.start(time.Second)

		By(fmt.Sprintf("overshooting the worker's ephemeral-storage limit by writing %dMiB", evictionFillMiB))
		filledAt := time.Now()
		fillOutput := overflowEphemeralStorage(scaleSetTenantNS, podName, evictionFillMiB)
		AddReportEntry("Q396 scale-set fill command output", fillOutput)

		By("waiting for the kubelet to evict the worker")
		Eventually(func(g Gomega) {
			g.Expect(podPhaseReason(scaleSetTenantNS, podName)).To(Equal("Failed/Evicted"),
				"worker pod has not been evicted")
		}, 5*time.Minute, 2*time.Second).Should(Succeed())
		stopSampling()

		evictedAt, exitCode, evictionMessage := evictionFacts(scaleSetTenantNS, podName)
		AddReportEntry("Q396 scale-set observed pod phase/reason sequence", strings.Join(observed.sequence(), " -> "))
		AddReportEntry("Q396 scale-set kubelet eviction message", evictionMessage)
		AddReportEntry("Q396 scale-set runner container exit code", strconv.Itoa(exitCode))
		AddReportEntry("Q396 scale-set fill to kubelet eviction", evictedAt.Sub(filledAt).Round(time.Second).String())
		// Two captures, read together — see the classic spec above (Q657).
		followerLog := relayLog.stopAndRead()
		AddReportEntry("Q396 scale-set worker log tail across the eviction", followerLog)
		podLog, podLogOK := evictedWorkerLog("Q396 scale-set", scaleSetTenantNS, podName)
		workerLogs := []workerLogSource{
			{name: "follower", log: followerLog, ok: strings.TrimSpace(followerLog) != ""},
			{name: "pod", log: podLog, ok: podLogOK},
		}

		By("confirming the kubelet killed the runner rather than asking it to stop")
		// 137 says the kubelet took the container, and nothing more — see the classic
		// spec's reasoning. This is the tier on which a 137 exit was measured on both
		// sides of the outcome split.
		Expect(exitCode).To(Equal(137),
			"the evicted runner exited %d, not SIGKILL; the kubelet did not take this container", exitCode)

		By("waiting for GitHub to conclude the job it can no longer hear from")
		// The measurement, and the number the classic half puts at 9m36s. Pinned to
		// attempt 1 for the same reason: the AGC's re-run starts a second attempt as
		// soon as GitHub concludes the run, at which point the "latest" filter stops
		// naming the job under measurement.
		var jobConclusion string
		Eventually(func(g Gomega) {
			status, conclusion := firstJobStateForAttempt(g, repoSlug, runID, 1)
			g.Expect(status).To(Equal("completed"), "job is still %q", status)
			jobConclusion = conclusion
		}, 20*time.Minute, 15*time.Second).Should(Succeed())

		concludedAtRaw := firstJobCompletedAtForAttempt(Default, repoSlug, runID, 1)
		concludedAt, err := time.Parse(time.RFC3339, concludedAtRaw)
		Expect(err).NotTo(HaveOccurred(), "parse job completed_at %q", concludedAtRaw)

		AddReportEntry("Q396 scale-set job conclusion after eviction", jobConclusion)
		AddReportEntry("Q396 scale-set eviction (kubelet finishedAt) -> conclusion (GitHub completed_at)",
			concludedAt.Sub(evictedAt).Round(time.Second).String())
		AddReportEntry("Q396 scale-set server timestamps",
			fmt.Sprintf("container finishedAt=%s, GitHub completed_at=%s",
				evictedAt.Format(time.RFC3339), concludedAt.Format(time.RFC3339)))
		recordEvictionOutcome("Q396 scale-set", concludedAt.Sub(evictedAt), exitCode, workerLogs,
			firstJobStepRecords(repoSlug, runID, 1))
		// Scale-set only, and a fourth independent read of when GitHub gave up: the
		// listener stamps this the moment its queue delivers the terminal JobCompleted
		// for the job (markJobCompleted), so it dates the conclusion from inside the
		// cluster rather than from GitHub's own record. Empty once the reaper has been.
		AddReportEntry("Q396 scale-set job-completed-at stamped on the worker",
			podAnnotationBestEffort(scaleSetTenantNS, podName, "actions-gateway.com/job-completed-at"))

		By("asserting the owning reconciler detected the disruption and reached recovery")
		agcLog := agcEvictionLogFor(scaleSetTenantNS, scaleSetAGCDeploy)
		AddReportEntry("Q396 scale-set AGC eviction log lines", agcLog)
		// The filtered view above is what the assertions below read, and it drops
		// everything about the job's own lifecycle — which is why the 2026-08-03 runs left
		// no way to see what the listener made of the disruption. The unfiltered tail is
		// for diagnosis only; it is bounded because this tenant runs at logLevel: debug.
		AddReportEntry("Q396 scale-set AGC log tail, unfiltered",
			agcDeploymentLogTail(scaleSetTenantNS, scaleSetAGCDeploy, 1000))
		Expect(agcLog).NotTo(ContainSubstring("worker pod disrupted but run_id unknown"),
			"the AGC saw the eviction but had no run identity to recover with. Log:\n%s", agcLog)

		By("asserting the retry budget was spent exactly once, on the scale-set tier")
		// The Q106 sharded-reservation invariant again, and additionally the tier label:
		// the budget is keyed by run ID alone and shared across tiers, so a recovery
		// attributed to the wrong tier would still spend the slot while telling an
		// operator the wrong thing about which path detected it.
		scheduled := strings.Count(agcLog, "worker pod disrupted; scheduling auto-retry")
		Expect(scheduled).To(Equal(1),
			"one eviction must reserve exactly one retry slot, saw %d. Log:\n%s", scheduled, agcLog)
		Expect(agcLog).To(SatisfyAny(
			ContainSubstring(`"tier":"scaleset"`), ContainSubstring("tier=scaleset")),
			"the recovery was not attributed to the scale-set tier. Log:\n%s", agcLog)
		Expect(agcLog).NotTo(ContainSubstring("disruption retry budget exhausted"),
			"a single eviction exhausted the retry budget. Log:\n%s", agcLog)

		By("waiting for the AGC's re-run to be accepted by GitHub")
		Eventually(func(g Gomega) {
			agcLog := agcEvictionLogFor(scaleSetTenantNS, scaleSetAGCDeploy)
			if strings.Contains(agcLog, "disruption auto-retry failed") {
				StopTrying("the AGC gave up on the re-run — refused past the re-run window, " +
					"or a terminal API error. Log:\n" + agcLog).Now()
			}
			g.Expect(agcLog).To(ContainSubstring("disruption auto-retry triggered"),
				"the re-run has not been accepted yet")
		}, 5*time.Minute, 10*time.Second).Should(Succeed())

		By("asserting the accepted re-run is the one the retry loop landed, not the first call")
		assertRerunLanded(agcEvictionLogFor(scaleSetTenantNS, scaleSetAGCDeploy),
			concludedAt.Sub(evictedAt), "Q396 scale-set")

		By("asserting GitHub actually started a second attempt")
		Eventually(func(g Gomega) {
			g.Expect(runAttemptCount(g, repoSlug, runID)).To(BeNumerically(">=", 2),
				"the AGC's re-run was accepted but GitHub created no second attempt")
		}, 5*time.Minute, 10*time.Second).Should(Succeed())

		AddReportEntry("Q396 scale-set outcome",
			"scale set registered against real GitHub, job acquired and run, eviction detected by the "+
				"owning reconciler, retry budget spent once at tier=scaleset, re-run accepted after the "+
				"run concluded, and a second attempt ran")
	})
})

// suiteRunnerPrefixes are the runner-name prefixes this suite's tenant registers with
// GitHub, given its gateway name. agentpool names an agent "<runnerGroup>-<index>", or
// "rs-<runnerSet>-<index>" under the v2 scheme, and the group name is derived from the
// gateway name — so every runner this suite owns carries one of these prefixes, and no
// other tenant's does.
//
// scripts/e2e/e2e-github-cleanup.sh applies the same rule, and has to: the preflight below
// blocks on exactly what that script clears, so a narrower filter there wedges the next
// run behind a runner the cleanup reports as handled.
//
// "this suite's tenant" is plural: siblingAGName extends agName precisely so a second
// gateway's runners fall under the same prefix. A gateway named outside it registers
// runners neither the preflight nor the cleanup can see.
func suiteRunnerPrefixes(gateway string) []string {
	return []string{gateway + "-", "rs-" + gateway + "-"}
}

// fixtureRepoRunners returns this suite's runners currently registered against the
// fixture repo, each rendered as "<name> (<status>, busy=<bool>)".
func fixtureRepoRunners(repoSlug, gateway string) []string {
	GinkgoHelper()
	out, err := utils.Run(exec.Command("gh", "api",
		fmt.Sprintf("repos/%s/actions/runners", repoSlug),
		"--jq", `.runners[] | "\(.name) (\(.status), busy=\(.busy))"`))
	Expect(err).NotTo(HaveOccurred(), "list self-hosted runners on %s", repoSlug)

	prefixes := suiteRunnerPrefixes(gateway)
	var mine []string
	for _, line := range nonEmptyLines(out) {
		for _, p := range prefixes {
			if strings.HasPrefix(line, p) {
				mine = append(mine, line)
				break
			}
		}
	}
	return mine
}

// fixtureRepoActiveRuns returns the fixture repo's workflow runs that have not reached
// "completed", each rendered as "<id> <workflow> (<status>)". The repo exists only to
// serve this suite, so any run still in flight there belongs to a peer session.
func fixtureRepoActiveRuns(repoSlug string) []string {
	GinkgoHelper()
	out, err := utils.Run(exec.Command("gh", "api",
		fmt.Sprintf("repos/%s/actions/runs?per_page=50", repoSlug),
		"--jq", `.workflow_runs[] | select(.status != "completed") | "\(.id) \(.path) (\(.status))"`))
	Expect(err).NotTo(HaveOccurred(), "list workflow runs on %s", repoSlug)
	return nonEmptyLines(out)
}

// nonEmptyLines splits jq's line-per-record output, dropping the trailing blank.
func nonEmptyLines(out string) []string {
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		if l := strings.TrimSpace(line); l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

// preflightFixtureRepoIdle fails the suite unless the fixture repo carries no state
// from another live-GitHub run. Call it before the first mutation of anything shared —
// ahead of the GMC env swap, which is itself cluster-wide.
//
// Concurrency here is not merely noisy, it is silent. Runner names are unique per
// registration scope and the fixture repo is one scope, so a second run registering the
// same name takes agentpool's 409 path: it resolves the conflicting record, deletes it,
// and registers its own. Both runs then hold a listener the other has deregistered
// underneath it, and each acquires jobs the other dispatched. Nothing errors on either
// side — the conflict path is the intended recovery for an AGC restart, where deleting
// the incumbent is correct. Diagnosing it from inside one run cost ~2.5 h (Q511).
//
// The check cannot tell a live peer from wreckage left by a killed run, so it reports
// what it saw and names both remedies rather than guessing.
func preflightFixtureRepoIdle(repoSlug, gateway string) {
	GinkgoHelper()
	runners := fixtureRepoRunners(repoSlug, gateway)
	runs := fixtureRepoActiveRuns(repoSlug)
	if len(runners) == 0 && len(runs) == 0 {
		return
	}

	var b strings.Builder
	fmt.Fprintf(&b, "live-GitHub preflight: %s is not idle, so this run would collide with another (Q511).\n", repoSlug)
	if len(runners) > 0 {
		fmt.Fprintf(&b, "  registered runners owned by this suite: %s\n", strings.Join(runners, ", "))
	}
	if len(runs) > 0 {
		fmt.Fprintf(&b, "  workflow runs not yet completed:         %s\n", strings.Join(runs, ", "))
	}
	b.WriteString("\nlive-GitHub is a singleton: one run at a time across every worktree and cluster.\n")
	b.WriteString("Either a peer run is in flight — wait for it — or one was killed with `kill -9`,\n")
	b.WriteString("stranding this state. Once you have confirmed no peer is running, clear it with:\n")
	b.WriteString("    make e2e-github-cleanup\n")
	Fail(b.String())
}

// admissionDeclinedLine is the listener's per-delivery decline (listener/job.go), and
// quotaRetryLine is the post-claim backstop's (provisioner/capacity.go). The Q422
// spec reads them as a pair: the first must appear and the second must not, which
// together say the rung refused BEFORE acquirejob rather than after.
const (
	admissionDeclinedLine = "job admission rejected: no worker capacity"
	quotaRetryLine        = "pod creation blocked by namespace quota"
)

// The renew loop's two failure lines (listener/renew.go). The cancel-path spec reads
// them as Q501's candidate-A measurement: whether the run service stops honouring
// renewjob once GitHub has concluded a cancelled job. renewNonFatalLine is any single
// failure; renewTeardownLine is the definitive loss that cancels the job context and,
// since Q501's actuator, reclaims the worker pod.
const (
	renewNonFatalLine = "RenewJob error (non-fatal)"
	renewTeardownLine = "RenewJob: job lock definitively lost"
)

// annotationDeletionReason is provisioner.AnnotationDeletionReason with its dots
// escaped for a jsonpath key selector, restated as a literal because this module cannot
// import the AGC's internal packages. The annotation is stamped before every AGC-issued
// worker delete and is what separates the gateway's own reclaim from a disruption.
const annotationDeletionReason = `actions-gateway\.com/deletion-reason`

// reapReasonJobAbandoned is the stamp value the AGC writes before deleting the worker
// of a job its listener gave up on (Q501).
const reapReasonJobAbandoned = "job_abandoned"

// deletionMark splits a `<phase>/<reason>/deleting=<ts>/by=<stamp>` sample from the
// cancel-path recorder into the two deletion fields, and reports whether the phase is
// terminal. A sample the recorder produced always carries both markers; one that does
// not yields empty fields rather than a parse error, since a dropped sample must not
// read as a mark that was never there.
func deletionMark(sample string) (terminal bool, deletedAt, stamp string) {
	terminal = strings.HasPrefix(sample, "Succeeded/") || strings.HasPrefix(sample, "Failed/")
	_, rest, ok := strings.Cut(sample, "/deleting=")
	if !ok {
		return terminal, "", ""
	}
	deletedAt, stamp, _ = strings.Cut(rest, "/by=")
	return terminal, deletedAt, stamp
}

// fillPodQuota puts a `pods` ResourceQuota on a namespace whose ceiling is its
// current occupancy, leaving headroom for exactly zero more pods.
func fillPodQuota(ns, name string, hard int) {
	GinkgoHelper()
	_, err := utils.Run(exec.Command("kubectl", "create", "quota", name,
		"-n", ns, fmt.Sprintf("--hard=pods=%d", hard)))
	Expect(err).NotTo(HaveOccurred(), "create ResourceQuota %s/%s", ns, name)
}

// nonTerminalPodCount counts the pods a `pods` ResourceQuota would charge the
// namespace for: Succeeded and Failed pods are not counted by the pod evaluator, so
// a namespace full of reaped workers is not full.
func nonTerminalPodCount(g Gomega, ns string) int {
	out, err := utils.Run(exec.Command("kubectl", "get", "pods", "-n", ns,
		"--field-selector", "status.phase!=Succeeded,status.phase!=Failed", "-o", "name"))
	g.Expect(err).NotTo(HaveOccurred())
	return len(utils.GetNonEmptyLines(out))
}

// liveWorkerPods returns the namespace's worker pods that have not reached a terminal
// phase — the ones still holding quota.
func liveWorkerPods(g Gomega, ns string) []string {
	out, err := utils.Run(exec.Command("kubectl", "get", "pods", "-n", ns,
		"-l", "app.kubernetes.io/managed-by=actions-gateway-controller",
		"--field-selector", "status.phase!=Succeeded,status.phase!=Failed",
		"-o", `jsonpath={range .items[*]}{.metadata.name} {end}`))
	g.Expect(err).NotTo(HaveOccurred())
	return strings.Fields(out)
}

// allWorkerPods returns every worker pod in a namespace regardless of phase, which is
// what a "did this gateway provision anything" check needs: a worker that ran and
// exited is still evidence that a job was claimed.
func allWorkerPods(g Gomega, ns string) []string {
	out, err := utils.Run(exec.Command("kubectl", "get", "pods", "-n", ns,
		"-l", "app.kubernetes.io/managed-by=actions-gateway-controller",
		"-o", `jsonpath={range .items[*]}{.metadata.name} {end}`))
	g.Expect(err).NotTo(HaveOccurred())
	return strings.Fields(out)
}

// runnerGroupCondition returns the status of one condition on the namespace's sole
// RunnerGroup, or "" when the condition is absent. Each fixture tenant declares one
// group, so indexing the list is unambiguous.
func runnerGroupCondition(g Gomega, ns, condType string) string {
	out, err := utils.Run(exec.Command("kubectl", "get", "runnergroups.actions-gateway.github.com",
		"-n", ns, "-o", fmt.Sprintf(
			`jsonpath={.items[0].status.conditions[?(@.type=="%s")].status}`, condType)))
	g.Expect(err).NotTo(HaveOccurred())
	return strings.TrimSpace(out)
}

// agcAdmissionLog returns the AGC's log lines about admission decisions, scoped so the
// report entry is readable rather than the whole controller log.
func agcAdmissionLog(ns string) string {
	GinkgoHelper()
	var kept []string
	for _, line := range strings.Split(agcLog(ns), "\n") {
		if strings.Contains(line, "admission") || strings.Contains(line, "quota") {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

// countAGCLogLines counts the AGC log lines containing substr. Counting rather than
// matching, because these assertions are deltas against a baseline read before the
// work starts: the AGC serves every spec in this container and its log is cumulative.
func countAGCLogLines(ns, substr string) int {
	GinkgoHelper()
	return strings.Count(agcLog(ns), substr)
}

// agcLogLines returns the AGC log lines containing substr, in order — one entry per
// matching line, where countAGCLogLines counts occurrences and so counts a line twice
// if substr appears in it twice. Like that helper it reads a cumulative log, so a
// caller measuring one window takes the suffix past a baseline count.
func agcLogLines(ns, substr string) []string {
	GinkgoHelper()
	var out []string
	for _, line := range strings.Split(agcLog(ns), "\n") {
		if strings.Contains(line, substr) {
			out = append(out, line)
		}
	}
	return out
}

// sinceBaseline returns the entries lines gained past a baseline length, tolerating a
// shrunk log: the AGC's container can restart or its log rotate mid-window, and a
// negative delta means the baseline no longer indexes into this log at all.
func sinceBaseline(lines []string, baseline int) []string {
	if baseline > len(lines) {
		return lines
	}
	return lines[baseline:]
}

// agcLog reads the tenant AGC's full log. The v1 tenant's AGC Deployment carries the
// fixed controller name; a v2 gateway's is named after the gateway, so that tier reads
// its log through agcDeploymentLog.
func agcLog(ns string) string {
	GinkgoHelper()
	return agcDeploymentLog(ns, agcName)
}

// agcDeploymentLog reads the full log of a named AGC Deployment.
func agcDeploymentLog(ns, deployment string) string {
	GinkgoHelper()
	out, err := utils.Run(exec.Command("kubectl", "logs", "-n", ns,
		"deployment/"+deployment, "--tail=-1"))
	Expect(err).NotTo(HaveOccurred(), "read AGC logs from %s/%s", ns, deployment)
	return out
}

// agcDeploymentLogTail reads the last lines of a named AGC Deployment's log, for a
// diagnostic report entry rather than an assertion. Bounded because a tenant running at
// logLevel: debug produces more than a report should carry, and tolerant of a failed read
// because losing a diagnostic must not fail a measurement that otherwise completed.
func agcDeploymentLogTail(ns, deployment string, lines int) string {
	out, err := utils.Run(exec.Command("kubectl", "logs", "-n", ns,
		"deployment/"+deployment, fmt.Sprintf("--tail=%d", lines)))
	if err != nil {
		return fmt.Sprintf("(could not read AGC log from %s/%s: %v)", ns, deployment, err)
	}
	return out
}

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
//
// inputs are "key=value" pairs forwarded as `gh workflow run -f`. The fixture declares
// a default for every input it takes, so a caller that passes none dispatches what
// callers dispatched before any input existed.
func dispatchAndResolveRun(repoSlug, workflow string, inputs ...string) string {
	GinkgoHelper()
	before := make(map[string]bool)
	for _, id := range recentRunIDs(Default, repoSlug, workflow) {
		before[id] = true
	}

	args := []string{"workflow", "run", workflow, "--repo", repoSlug, "--ref", "main"}
	for _, in := range inputs {
		args = append(args, "-f", in)
	}
	_, err := utils.Run(exec.Command("gh", args...))
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
// for a specific workflow run, or "" and a diagnostic when none exists yet.
//
// Matching is on the run-id annotation the AGC stamps on every worker pod
// (provisioner.AnnotationRunID) and on nothing else. There used to be a fallback —
// "the sole Running worker that was not there before this spec dispatched" — from when
// Q495 left the annotation absent on this tier and identity-by-freshness was the only
// thing available. On a build that stamps the annotation the fallback is not a safety
// net but a way to resolve the WRONG pod, so it is gone.
//
// It failed live on 2026-08-03. The cancel-path spec dispatched run 30856065695 and was
// handed a worker annotated 30856024324: an earlier spec's re-run had reached its second
// attempt and provisioned a worker AFTER this spec took its snapshot, which made someone
// else's pod "fresh". The spec would have cancel-tested a pod belonging to another run.
// Freshness stopped being an identity signal the moment these specs started triggering
// re-runs, and every spec here now does.
//
// preexisting is kept for the diagnostic alone: which workers appeared since the
// dispatch is useless for choosing one but sharpens the message when none matches.
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

	// No match yet. Separate the two reasons, because they need opposite responses: an
	// unannotated worker is a Q495 regression and waiting will not fix it, whereas
	// workers annotated for other runs mean this run's worker simply has not been
	// provisioned yet and the caller should keep polling.
	var unannotated, otherRuns []string
	for _, pod := range runningWorkers(g, ns) {
		name, annotated, _ := strings.Cut(pod, "=")
		if annotated == "" {
			unannotated = append(unannotated, name+" (no run-id)")
			continue
		}
		mark := ""
		if !preexisting[name] {
			mark = ", appeared since dispatch"
		}
		otherRuns = append(otherRuns, fmt.Sprintf("%s (run %s%s)", name, annotated, mark))
	}

	switch {
	case len(unannotated) > 0:
		return "", fmt.Sprintf(
			"no worker carries run-id=%s, and %d Running worker(s) carry NO run-id at all: %s. "+
				"That is the Q495 regression — the AGC provisioned a worker it cannot attribute "+
				"to a run, so eviction recovery is inert on it (Q544)",
			runID, len(unannotated), strings.Join(unannotated, ", "))
	case len(otherRuns) > 0:
		return "", fmt.Sprintf(
			"no worker carries run-id=%s yet; Running workers belong to other runs: %s. "+
				"Workers marked \"appeared since dispatch\" are most likely an earlier spec's "+
				"re-run reaching its second attempt, which is why freshness cannot identify a "+
				"worker here",
			runID, strings.Join(otherRuns, ", "))
	default:
		return "", fmt.Sprintf("no Running worker in %s yet", ns)
	}
}

// assertWorkerCarriesRepository fails unless the worker also carries the repository
// annotation.
//
// The run-id half needs no assertion: runningWorkerForRun matches on it and nothing
// else, so a worker that resolved at all has one. Repository is the other key, read from
// the same acquire-payload github context — Q495 was the two arriving together or not at
// all — and rerun-failed-jobs needs owner/repo as well as the run id, so a worker with
// one and not the other is one recovery still cannot act on (Q544).
func assertWorkerCarriesRepository(ns, podName string) {
	GinkgoHelper()
	Expect(podAnnotation(ns, podName, "actions-gateway.com/repository")).NotTo(BeEmpty(),
		"worker %s resolved by its run-id annotation but carries no repository annotation; "+
			"recovery is still inert on this pod (Q495)", podName)
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
// The exit code says the kubelet killed rather than asked: 137 is SIGKILL. It does not
// say the runner was silent — a runner that gets its report out and then overruns its
// grace is SIGKILLed too, and both 2026-08-03 scale-set runs exited 137 while concluding
// 9m38s and 17s after the kill. What the runner managed to say is read from its log and
// from GitHub's step records instead (recordEvictionOutcome).
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

// relayRanMarker is what the worker wrapper logs when it receives a termination signal
// and passes it to the runner (terminationRelay, cmd/worker/main.go). Its presence in an
// evicted worker's log is direct evidence that the kill left a window to report in.
const relayRanMarker = "forwarding termination signal to child"

// evictedWorkerLog reads an evicted worker's container log whole, off the pod object, and
// reports it before returning it.
//
// It is a second, independent read alongside the follower, and the weaker of the two:
// measured 2026-08-03, the kubelet had already removed the evicted container and kubectl
// answered `unable to retrieve container logs for containerd://…`. The follower is what
// captured the last instant on that run, streaming to within a second of the container's
// finishedAt. So this read is corroboration, never the sole source — and `ok` is what
// keeps a failed read from being mistaken for a silent runner.
//
// It reports at the point of capture rather than leaving that to the caller's verdict,
// because a capture is worth most on the runs that fail: the 2026-08-03 re-measurement
// took this log and then lost it to an assertion that fired before the verdict was
// written. The one thing a diagnostic must not do is depend on the spec getting further.
func evictedWorkerLog(prefix, ns, name string) (log string, ok bool) {
	out, err := utils.Run(exec.Command("kubectl", "logs", "-n", ns, name, "-c", "runner"))
	// kubectl reports a container the kubelet has already reclaimed on stdout and still
	// exits 0, so the exit status alone does not say whether a log came back.
	ok = err == nil && !strings.Contains(out, "unable to retrieve container logs")
	if !ok {
		out = fmt.Sprintf("(no log readable off the evicted pod: err=%v, output=%s)", err, out)
	}
	AddReportEntry(prefix+" evicted worker log, read whole off the pod", out)
	return out, ok
}

// scaleSetJobRunnerName returns the runner GitHub records as executing the run's first
// job, "" while it names none. This is the only source that knows which of a tenant's
// runners actually took the job; the cluster side can only say which pods exist.
func scaleSetJobRunnerName(g Gomega, repoSlug, runID string) string {
	out, err := utils.Run(exec.Command("gh", "api",
		fmt.Sprintf("repos/%s/actions/runs/%s/jobs?filter=latest", repoSlug, runID),
		"--jq", ".jobs[0].runner_name"))
	g.Expect(err).NotTo(HaveOccurred())
	name := strings.TrimSpace(out)
	if name == "null" {
		return ""
	}
	return name
}

// assertWorkerRunsTheJob requires the worker about to be disrupted to be the one GitHub
// says is executing the job.
//
// The scale-set runner is named "<scaleSetName>-<jobID>" (listener.runnerName) and the
// worker pod's name embeds the same jobID, truncated to fit the name budget. Comparing
// the jobID's first segment reconciles them: it is 8 hex characters, which separates a
// tenant's handful of concurrent workers without depending on how far the pod name was
// truncated.
//
// This exists because the run-id annotation alone put the wrong pod under the eviction
// lever on 2026-08-03 (Q657). The diagnostic lists every worker in the namespace, because
// "the annotation matched a different pod than GitHub named" is only actionable next to
// the set it was chosen from.
func assertWorkerRunsTheJob(ns, podName, runnerName string) {
	GinkgoHelper()
	jobID := strings.TrimPrefix(runnerName, scaleSetLabel+"-")
	Expect(jobID).NotTo(Equal(runnerName),
		"GitHub named runner %q, which does not carry the %q prefix this scale set registers under",
		runnerName, scaleSetLabel)

	segment, _, _ := strings.Cut(jobID, "-")
	Expect(segment).NotTo(BeEmpty(), "runner %q carries no job ID", runnerName)
	Expect(podName).To(ContainSubstring(segment),
		"the worker about to be evicted is %s, but GitHub says the job is executing on runner %q "+
			"(job ID %s) — evicting this pod would measure the loss of a runner that has no job. "+
			"Workers in %s: %s",
		podName, runnerName, jobID, ns, strings.Join(allWorkerPods(Default, ns), ", "))
}

// podAnnotationBestEffort is podAnnotation for a pod that may legitimately be gone.
//
// The eviction specs read annotations that are only written late — after GitHub has
// concluded the run, by which time the AGC's reaper may have taken the object. That read
// is a diagnostic, so a missing pod has to come back as an empty string rather than fail
// a measurement that already completed.
func podAnnotationBestEffort(ns, name, key string) string {
	out, err := utils.Run(exec.Command("kubectl", "get", "pod", name, "-n", ns,
		"--ignore-not-found",
		"-o", fmt.Sprintf("jsonpath={.metadata.annotations['%s']}", strings.ReplaceAll(key, ".", `\.`))))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// firstJobStepRecords renders the per-step records GitHub holds for one attempt's first
// job, as "<number>. <name>=<status>/<conclusion>" lines.
//
// This is the read of "did the runner report?" that does not depend on capturing a log at
// all, and it is the one evidence source that survived every capture failure: the runner
// posts step results as it executes them, and a job GitHub concludes by itself — because a
// lock stopped being renewed — has no such post behind it.
//
// The lock-lapsed shape is measured (2026-08-03, 9m45s to conclusion): the job reads
// `completed/failure` while the step it was interrupted in is frozen at
// `in_progress/null`. The shape a reported loss leaves has not been captured yet, so this
// stays evidence rather than an assertion.
func firstJobStepRecords(repoSlug, runID string, attempt int) string {
	out, err := utils.Run(exec.Command("gh", "api",
		fmt.Sprintf("repos/%s/actions/runs/%s/attempts/%d/jobs", repoSlug, runID, attempt),
		"--jq", `.jobs[0].steps[] | "\(.number). \(.name)=\(.status)/\(.conclusion)"`))
	if err != nil {
		return fmt.Sprintf("(could not read step records: %v)", err)
	}
	if strings.TrimSpace(out) == "" {
		return "(no step records)"
	}
	return strings.TrimSpace(out)
}

// recordEvictionOutcome writes one line classifying a measured eviction, followed by the
// evidence the classification rests on.
//
// A kubelet ephemeral-storage eviction produces one of two outcomes and the spec cannot
// choose which: measured 2026-08-03, two runs of one spec on one build split 9m38s to
// conclusion against 17s. Latency is what separates them (see reportedItsOwnLoss), but
// latency alone cannot say *why*, and neither of those runs captured enough to tell —
// which is the whole of Q657. So every run states its verdict and its evidence in one
// place, and a later run's observation can be pooled with this one's instead of read
// alone.
//
// It is called as soon as the conclusion has been timed, ahead of the recovery
// assertions, because an eviction that recovery then mishandles is still an observation
// of the split.
//
// relayRan is three-valued on purpose. The marker's absence only means the relay did not
// run if a log was actually read, and on 2026-08-03 the post-hoc read came back empty
// because the kubelet had reclaimed the container — which an unguarded Contains would
// have rendered as a measured `false`.
func recordEvictionOutcome(prefix string, evictionToConclusion time.Duration, exitCode int,
	logs []workerLogSource, stepRecords string,
) {
	verdict := "lock-lapsed"
	if evictionToConclusion < reportedItsOwnLoss {
		verdict = "report-escaped"
	}

	relayRan, read := "unknown", []string{}
	for _, src := range logs {
		if !src.ok {
			continue
		}
		read = append(read, src.name)
		if strings.Contains(src.log, relayRanMarker) {
			relayRan = "true"
		} else if relayRan == "unknown" {
			relayRan = "false"
		}
	}
	if len(read) == 0 {
		read = append(read, "none")
	}

	AddReportEntry(prefix+" eviction outcome", fmt.Sprintf(
		"verdict=%s latency=%s exitCode=%d relayRan=%s logsRead=%s",
		verdict, evictionToConclusion.Round(time.Second), exitCode,
		relayRan, strings.Join(read, "+")))
	AddReportEntry(prefix+" GitHub step records for the interrupted attempt", stepRecords)
}

// workerLogSource is one capture of a disrupted worker's output, carrying whether the
// capture actually produced anything. The two sources fail in different ways — the
// follower dies with the container mid-write, the post-hoc read finds the container
// already reclaimed — so the verdict reads both and says which ones answered.
type workerLogSource struct {
	name string
	log  string
	ok   bool
}

// agcEvictionLog returns the AGC's log lines about eviction handling. Scoped to the
// handful of messages handleEviction emits so the report entry is readable, rather
// than the whole controller log.
func agcEvictionLog(ns string) string {
	GinkgoHelper()
	return agcEvictionLogFor(ns, agcName)
}

// agcEvictionLogFor is agcEvictionLog against a named AGC Deployment, for the v2
// scale-set tenant whose Deployment is named after its gateway.
func agcEvictionLogFor(ns, deployment string) string {
	GinkgoHelper()
	var kept []string
	for _, line := range strings.Split(agcDeploymentLog(ns, deployment), "\n") {
		if strings.Contains(line, "evicted") || strings.Contains(line, "eviction") ||
			strings.Contains(line, "disrupt") || strings.Contains(line, "re-run") {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

// reportedItsOwnLoss is the eviction→conclusion latency below which the runner must have
// got its own report out: GitHub concluded far too quickly to have been waiting out the
// job lock.
//
// The two populations sit either side of a wide gap — 15–26s when a disrupted runner
// does report (Q459), 9m36–9m38s when it does not (Q396) — so any threshold between them
// separates the cases, and 60s is comfortably inside it.
const reportedItsOwnLoss = 60 * time.Second

// assertRerunLanded checks the re-run the AGC's recovery made, and requires it to have
// absorbed at least one refusal only when the eviction was actually ungraceful.
//
// The refusal is not a property of eviction, it is a property of GitHub not yet knowing
// the job is over. A kubelet ephemeral-storage eviction is not reliably ungraceful:
// measured 2026-08-03, two runs of one spec on one build saw 9m38s-to-conclusion with 20
// paced calls, and 17s-to-conclusion with the re-run accepted on the first call. Both are
// correct behaviour — in the second the runner reported, so there was never a refusal to
// out-wait — and an unconditional "at least two calls" fails the healthy case.
//
// What must hold either way is that the re-run landed at all. The Q503 negative assertion
// is kept exactly where it means something: on the ungraceful path, where a fire-once
// recovery would have been refused and given up.
func assertRerunLanded(agcLog string, evictionToConclusion time.Duration, entryPrefix string) {
	GinkgoHelper()
	calls := rerunCallsOnAcceptance(agcLog)
	AddReportEntry(entryPrefix+" rerun-failed-jobs calls before GitHub accepted", strconv.Itoa(calls))
	Expect(calls).To(BeNumerically(">=", 1),
		"the AGC logged an accepted re-run with no call count; the acceptance line has changed shape")

	if evictionToConclusion < reportedItsOwnLoss {
		AddReportEntry(entryPrefix+" eviction was graceful enough for the runner to report",
			fmt.Sprintf("GitHub concluded %s after the kill — far inside the job lock's ~10-minute TTL — so "+
				"the runner got its own report out and the re-run faced no refusal to absorb. Q503's retry "+
				"loop is untested by this run, and that is a property of the disruption, not a fault",
				evictionToConclusion.Round(time.Second)))
		return
	}

	Expect(calls).To(BeNumerically(">=", 2),
		"GitHub took %s to conclude — the ungraceful path, where it refuses re-runs until it does — "+
			"yet the re-run was accepted on call %d. Either the recovery no longer waits out the refusal, "+
			"or the refusal window has changed shape (Q503)",
		evictionToConclusion.Round(time.Second), calls)
}

// rerunCallsAttr matches the rerunCalls attribute in either encoding the AGC may ship:
// `"rerunCalls":19` from the JSON handler, `rerunCalls=19` from the text one.
var rerunCallsAttr = regexp.MustCompile(`rerunCalls"?[:=](\d+)`)

// rerunCallsOnAcceptance returns how many rerun-failed-jobs calls the recovery made
// before GitHub accepted one, read off the acceptance line. Returns 0 when that line is
// absent — the caller has already asserted it is there.
//
// Scoped to the acceptance line specifically. Both terminal-failure lines carry the
// same attribute, so a scan of the whole log would report the call count of a recovery
// that never landed as though it were one that did.
func rerunCallsOnAcceptance(agcLog string) int {
	GinkgoHelper()
	for _, line := range strings.Split(agcLog, "\n") {
		if !strings.Contains(line, "disruption auto-retry triggered") {
			continue
		}
		m := rerunCallsAttr.FindStringSubmatch(line)
		Expect(m).NotTo(BeNil(), "no rerunCalls attribute on the acceptance line: %s", line)
		n, err := strconv.Atoi(m[1])
		Expect(err).NotTo(HaveOccurred(), "parse rerunCalls from %q", line)
		return n
	}
	return 0
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
