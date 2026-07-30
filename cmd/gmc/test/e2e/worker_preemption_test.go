//go:build e2e
// +build e2e

package e2e

import (
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/actions-gateway/github-actions-gateway/gmc/test/utils"
)

// E2E_AGC_WorkerPreemption backs the oversubscription claim end to end: a worker
// displaced by a higher-priority `priorityTiers` tier gets its run re-run automatically,
// with no human in the loop.
//
// `priorityTiers` maps a PriorityClass onto a RunnerGroup's worker pods so low-priority
// CI can be packed into capacity reserved for higher-priority work, and the claim is
// that this is safe *because* a displaced job recovers automatically. The packing half
// was already covered elsewhere; this spec is the recovery half.
//
// # It started as a measurement, and the measurement said no
//
// Q423 opened with a prediction — "preemption is kubelet-initiated, so unlike a drain it
// does produce PodFailed/Evicted and does exercise handleEviction on classic". That
// conflates two different mechanisms that both get called eviction:
//
//   - The **kubelet's node-pressure eviction** publishes PodFailed with
//     Status.Reason "Evicted". That was the ONLY shape either tier's recovery acted on.
//   - **kube-scheduler preemption**, which is what a PriorityClass actually drives,
//     removes the victim by DELETING it — the same graceful removal Q421 measured for
//     `kubectl drain` and Q459 measured for `kubectl delete pod`.
//
// Run on 2026-07-29, this spec measured the second, and therefore measured NO recovery:
// the displaced run needed a manual re-run, and the published safety claim was wrong.
// Q497 closed that by keying recovery on the DisruptionTarget condition with reason
// PreemptionByScheduler — which only kube-scheduler writes, and so carries none of the
// human-cancel ambiguity that keeps Q459's drain slice open. The spec kept its whole
// apparatus and flipped its rerun assertion from "never" to "exactly once"; the
// never-Evicted assertion stays, because recovery here must be reached by the marker
// rather than by the pod taking the kubelet shape.
//
// # How the preemption is forced
//
// Node CPU and memory are the obvious contended resources and the wrong ones to use:
// how much of a kind node is free depends on everything else the suite has running, so
// a preemption forced that way is a race with the rest of the cluster. Instead the spec
// advertises a custom EXTENDED RESOURCE — exactly one unit, on exactly one node — and
// has both the worker and the displacing pod request it. Extended resources are
// integers the kubelet does not manage, so the arithmetic is exact and unaffected by
// anything else running: one slot, two claimants, the higher priority wins. The
// preemption is then the scheduler's only way to place the second pod.
//
// The displacing pod is a plain high-priority pod rather than a second tenant's worker.
// The mechanism under test is the scheduler's, and it is identical either way; a second
// gateway would add a second AGC, a second proxy pool and a second session to the parts
// of the run that could go wrong, none of which the measurement is about.
//
// # Why the victim is deliberately held Pending
//
// Same reason as E2E_AGC_WorkerNodeDrain, and the same cost. The worker's image cannot
// be pulled, so the pod is *scheduled* — it holds the slot, and it is a preemption
// victim like any other assigned pod — but never starts a container. A worker running
// the real runner image against fakegithub exits by itself within seconds, so a
// preemption aimed at one would land on a pod that had already ended, and the result
// would be measuring the runner's exit rather than the preemption.
//
// What that costs, stated plainly: the first spec does not exercise the wrapper's
// SIGTERM relay, because there is no live container to signal, and cannot show what
// phase the kubelet publishes on the way out. The second spec in this file
// (E2E_AGC_PreemptedRunningPodPhaseFollowsItsExitCode) covers that half on a RUNNING
// victim; between them the two answer both questions. What a Pending victim
// additionally shows is the worse case — a job displaced before its runner ever
// connected has no report of its own to fall back on.
//
// The measured result and the design reasoning live in
// docs/design/04-operational-flows.md §4.2; the operator-facing behaviour is
// docs/operations/troubleshooting.md, "Draining a Worker Does Not Auto-Re-Run the Jobs
// It Interrupts" — which now covers the drain slice only.
//
// Serial and multi-node: it advertises a resource on a named node, creates
// cluster-scoped PriorityClasses, edits the cluster-scoped PriorityClassAllowlist, and
// pins fakegithub's global AcquireJob response.
var _ = Describe("E2E_AGC_WorkerPreemption", Ordered, Serial, Label("multi-node"), func() {
	const (
		tenantNS   = "tenant-preempt"
		agName     = "preemptprobe-ag"
		secretName = "github-app-secret" //nolint:gosec // G101: the NAME of a Kubernetes Secret object, not a credential value.

		// preemptionConditionReason is the DisruptionTarget reason kube-scheduler writes
		// on a preemption victim, and the discriminator the AGC's recovery keys on
		// (Q497). Spelled out here rather than imported so the spec asserts the literal
		// Kubernetes contract rather than whatever constant the AGC happens to compare.
		preemptionConditionReason = "PreemptionByScheduler"

		// runID scopes the rerun assertion to this spec: /control/reruns is
		// process-wide, and another spec's rerun must not be readable as ours.
		runID     = "4230423"
		repoOwner = "preemptorg"
		repoName  = "preemptrepo"

		// unpullableImage holds the worker pod scheduled-but-Pending (see the note
		// above). `.invalid` is a reserved TLD, so the pull can never succeed.
		unpullableImage = "registry.invalid/preempt-probe-runner:none"
		// pendingDeadline must outlast the spec: the Q95 reaper deletes a stuck-Pending
		// worker once it elapses, and a reaped pod would remove the preemption's subject
		// before the preemption ran.
		pendingDeadline = "30m"

		// tenantPriorityClass is the tier the RunnerGroup declares — an opportunistic,
		// low-value class of exactly the kind the oversubscription story puts CI in. It
		// is created preemptionPolicy: Never, which is what values.yaml tells platform
		// admins to do for a tenant-nameable class: a tenant's own workers must not be
		// able to displace anyone else's.
		tenantPriorityClass = "gag-e2e-opportunistic"
		// platformPriorityClass is the displacing workload's class. It is deliberately
		// NOT added to the tenant allowlist — a tenant that could name it could preempt
		// across tenants, which is the whole reason the allowlist exists.
		platformPriorityClass = "gag-e2e-preemptor"

		// slotResource is the extended resource that makes the contention exact. One
		// unit is advertised on one node and both pods ask for it.
		slotResource = "actions-gateway.e2e/preempt-slot"

		preemptorNS      = "e2e-preempt-driver"
		preemptorPodName = "preemptor"
	)

	var (
		preemptPFCmd *exec.Cmd
		// slotNode is the node carrying the single preempt slot, resolved from the
		// cluster rather than named: which workers exist depends on the kind config.
		slotNode string
		// rgName is resolved from the cluster, not composed from agName — the GMC
		// derives it as "<gateway>-<first label>-<hash>", and the hash is what the
		// worker-pod label selector keys on.
		rgName string
		// allowlistName is the PriorityClassAllowlist CR the GMC watches, read off its
		// own flag so the spec augments the object the GMC is actually reading.
		allowlistName string
	)

	BeforeAll(func() {
		By("resolving the PriorityClassAllowlist the GMC watches")
		allowlistName = gmcFlagValue("--priority-class-allowlist-name")
		Expect(allowlistName).NotTo(BeEmpty(),
			"the GMC must run with --priority-class-allowlist-name, or priorityTiers cannot be allowlisted for this spec")

		By("creating the two PriorityClasses")
		// Values are far apart and well below system-cluster-critical, so nothing this
		// spec creates can outrank cluster infrastructure.
		Expect(utils.ApplyManifest(priorityClassManifest(tenantPriorityClass, 100, "Never"))).To(Succeed())
		Expect(utils.ApplyManifest(priorityClassManifest(platformPriorityClass, 1_000_000, "PreemptLowerPriority"))).To(Succeed())

		By("allowlisting the tenant's class so the gateway is admitted")
		// The CR augments --allowed-priority-classes and is watched (Q188), so this
		// takes effect without a GMC restart. It is also the paramKind of the
		// priorityclass-allowlist-guard policy, so one edit satisfies both gates.
		setAllowedPriorityClasses(allowlistName, []string{tenantPriorityClass})

		By("advertising a single preempt slot on one worker node")
		slotNode = firstSchedulableWorkerNode()
		advertiseExtendedResource(slotNode, slotResource, 1)

		utils.CreateNamespace(tenantNS, nil)
		utils.CreateNamespace(preemptorNS, nil)
		utils.CreateGitHubAppSecret(tenantNS, secretName, 12345, 67890, testRSAKeyPEM)
		utils.ApplyFakegithubEgressNetworkPolicy(tenantNS)

		By("applying a tenant whose workers run in the opportunistic tier")
		utils.RunnerTenant(tenantNS, agName, secretName, unpullableImage).
			WithLifecycle("30m", pendingDeadline).
			WithPriorityTier(tenantPriorityClass, 1, corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:                resource.MustParse("50m"),
					corev1.ResourceMemory:             resource.MustParse("64Mi"),
					corev1.ResourceName(slotResource): resource.MustParse("1"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:                resource.MustParse("50m"),
					corev1.ResourceMemory:             resource.MustParse("64Mi"),
					corev1.ResourceName(slotResource): resource.MustParse("1"),
				},
			}).
			ApplyWithWebhookRetry()

		By("waiting for the AGC to be ready and fully reconciled")
		utils.WaitForDeploymentReady(tenantNS, agcName, 4*time.Minute)
		utils.WaitForRunnerGroupReconciled(tenantNS, 4*time.Minute)

		By("resolving the RunnerGroup name the GMC actually created")
		out, err := utils.Run(exec.Command("kubectl", "get", "runnergroup",
			"-n", tenantNS, "-o", "jsonpath={.items[0].metadata.name}"))
		Expect(err).NotTo(HaveOccurred())
		rgName = strings.TrimSpace(out)
		Expect(rgName).NotTo(BeEmpty(), "no RunnerGroup in %s to provision workers from", tenantNS)

		By("starting port-forward to the fakegithub control API")
		fakegithubLocalPort = fmt.Sprintf("%d", 19990+GinkgoParallelProcess())
		preemptPFCmd = exec.Command("kubectl", "port-forward",
			"-n", infraNamespace,
			"service/"+fakegithubServiceName,
			fakegithubLocalPort+":9090",
		)
		Expect(preemptPFCmd.Start()).To(Succeed())
		Eventually(func() error {
			resp, err := http.Get("http://localhost:" + fakegithubLocalPort + "/control/sessions")
			if err != nil {
				return err
			}
			_ = resp.Body.Close()
			return nil
		}, 15*time.Second, 500*time.Millisecond).Should(Succeed())
	})

	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			utils.DumpAGCSessionDiagnostics(tenantNS, agcName, infraNamespace, fakegithubServiceName)
		}
	})

	AfterAll(func() {
		// ORDER IS LOAD-BEARING: the tenant must be fully gone before the allowlist is
		// narrowed again.
		//
		// The priorityclass-allowlist-guard policy re-validates STORED objects on
		// update, which is the point of it (a webhook only sees writes). Tearing the
		// tenant down is a sequence of updates: the GMC clears its gmc-cleanup finalizer
		// from the ActionsGateway and the AGC clears agentpool-cleanup from the
		// RunnerGroup. Both objects still name tenantPriorityClass. Narrow the allowlist
		// first and every one of those updates is denied — the finalizers can never be
		// removed, and the namespace wedges in Terminating with no controller able to
		// free it. Observed exactly that way while building this spec; recovering needs
		// a human to re-widen the allowlist and strip the finalizer by hand.
		utils.DeleteActionsGatewayCR(tenantNS, agName)
		utils.DeleteNamespace(tenantNS)
		utils.DeleteNamespace(preemptorNS)
		waitForNamespaceGone(tenantNS, 3*time.Minute)

		// Only now is it safe to restore the fail-closed default. Leaving the allowlist
		// widened would let a later spec's tenant name a class the platform did not
		// intend, and silently pass a gate that should have denied it.
		if allowlistName != "" {
			setAllowedPriorityClasses(allowlistName, nil)
		}
		// The slot is harmless to leave (nothing else requests it) but is withdrawn
		// anyway so the node's advertised capacity matches reality for later specs.
		if slotNode != "" {
			withdrawExtendedResource(slotNode, slotResource)
		}
		if preemptPFCmd != nil && preemptPFCmd.Process != nil {
			_ = preemptPFCmd.Process.Kill()
		}
		for _, pc := range []string{tenantPriorityClass, platformPriorityClass} {
			_, _ = utils.Run(exec.Command("kubectl", "delete", "priorityclass", pc, "--ignore-not-found"))
		}
	})

	It("E2E_AGC_PreemptedWorkerIsRecovered: a preempted worker pod is deleted, never Evicted, and its run is re-run automatically", func() {
		By("confirming a rerun is observable")
		// The AGC builds the rerun-failed-jobs URL from GITHUB_API_BASE_URL; if that did
		// not address fakegithub the call would leave for the real api.github.com and
		// fakegithub would report zero whatever the preemption did. Checked first so a
		// failure here reads as "the measurement is broken", not "recovery regressed".
		Expect(agcEnvValue(tenantNS, "GITHUB_API_BASE_URL")).To(ContainSubstring(fakegithubServiceName),
			"the AGC must address fakegithub for REST calls, or the rerun assertion proves nothing")

		By("recording the pre-existing rerun count for this run")
		Expect(rerunCountForRun(runID)).To(Equal(0),
			"no rerun may exist for this spec's run before the spec has done anything")

		By("pinning the next AcquireJob response to a payload carrying a complete run identity")
		// Without owner/repo/run_id, handleEviction returns early and no rerun could
		// fire regardless of the preemption — a pass would be vacuous and a failure
		// would be about the payload rather than about recovery.
		fakegithubSvcURL := fmt.Sprintf("http://%s.%s.svc.cluster.local:%s",
			fakegithubServiceName, infraNamespace, fakegithubServicePort)
		setAcquireJobResponse(map[string]interface{}{
			"plan": map[string]string{"planId": "preempt-plan-" + runID},
			"variables": map[string]interface{}{
				"system.github.repository": map[string]string{"value": repoOwner + "/" + repoName},
				"system.github.run_id":     map[string]string{"value": runID},
			},
			"resources": map[string]interface{}{
				"endpoints": []map[string]interface{}{{
					"name": "SystemVssConnection",
					"url":  fakegithubSvcURL,
					"authorization": map[string]interface{}{
						"scheme": "OAuth",
						//nolint:gosec // G101: a literal handed to fakegithub in-cluster, not a credential.
						"parameters": map[string]string{"AccessToken": "preempt-job-token"},
					},
				}},
			},
		})
		defer setAcquireJobResponse(nil)

		By("enqueuing a job onto this tenant's own session")
		var sessionID string
		Eventually(func(g Gomega) {
			sessions := fakegithubActiveSessionsForOwner(g, agName+"-")
			g.Expect(sessions).NotTo(BeEmpty(), "no live session for this RunnerGroup")
			sessionID = sessions[0]
		}, 3*time.Minute, 2*time.Second).Should(Succeed())
		fakegithubEnqueueJob(sessionID, map[string]interface{}{
			"jobId":           "preempt-job-1",
			"jobName":         "preempt probe",
			"run_service_url": fakegithubSvcURL,
		})

		By("waiting for the worker pod to be scheduled onto the slot node")
		var podName string
		var podNode string
		Eventually(func(g Gomega) {
			podName, podNode = workerPodAndNode(g, tenantNS, rgName)
			g.Expect(podName).NotTo(BeEmpty(), "no worker pod yet")
			g.Expect(podNode).NotTo(BeEmpty(), "worker pod %s is not scheduled yet", podName)
		}, 4*time.Minute, 1*time.Second).Should(Succeed())
		AddReportEntry("Q423 worker pod", podName+" on node "+podNode)
		Expect(podNode).To(Equal(slotNode),
			"the worker must hold the only preempt slot, or the displacing pod would simply schedule elsewhere")

		// The payload has been read; restore the shared fakegithub default so nothing
		// after this spec inherits our run identity.
		setAcquireJobResponse(nil)

		By("confirming priorityTiers actually put the worker in the opportunistic class")
		// The load-bearing precondition for the whole oversubscription claim. If the
		// tier never reached the pod there is no priority relationship to preempt on,
		// and everything below would be measuring an ordinary eviction.
		Expect(podPriorityClass(tenantNS, podName)).To(Equal(tenantPriorityClass),
			"the worker pod must carry the tier's PriorityClass, or this spec is not testing oversubscription")

		By("confirming the preemption's subject is a live worker, not one that already ended")
		Expect(podPhase(tenantNS, podName)).To(Equal("Pending"),
			"the worker pod must still be live when the preemption starts")

		By("recording the node-disruption-safety annotations the AGC stamped")
		// The AGC marks every worker pod safe-to-evict=false / do-not-disrupt so
		// autoscalers and deschedulers leave a mid-job worker alone. The scheduler
		// honours none of them — they are advisory to those controllers only — so
		// preemption, like a drain, needs its own answer rather than inheriting theirs.
		safeToEvict := podAnnotation(tenantNS, podName, "cluster-autoscaler.kubernetes.io/safe-to-evict")
		AddReportEntry("Q423 safe-to-evict annotation on the preempted worker", safeToEvict)
		Expect(safeToEvict).To(Equal("false"),
			"worker pods must carry the disruption-safety marker, or this spec is not preempting a normally-protected pod")

		By("sampling the worker pod's phase, reason, deletion mark and disruption condition")
		// The measurement. Each sample carries four fields at once, because what decides
		// recovery is not any one of them but their combination at the moment the pod
		// leaves: the phase/reason pair is the kubelet-eviction shape, which this path
		// must never take, and the DisruptionTarget condition is the discriminator Q497
		// keys recovery on. deletionTimestamp is sampled alongside because it is the
		// candidate Q459 is still weighing for the rest of the graceful-removal path.
		observed := newFieldRecorder(tenantNS, podName,
			`{.status.phase}/{.status.reason}/{.metadata.deletionTimestamp}/`+
				`{.status.conditions[?(@.type=="DisruptionTarget")].reason}`)
		stopSampling := observed.start(200 * time.Millisecond)

		By("creating a higher-priority pod that can only run by displacing the worker")
		Expect(utils.ApplyManifest(preemptorPodManifest(
			preemptorNS, preemptorPodName, platformPriorityClass, slotNode, slotResource,
		))).To(Succeed())

		By("waiting for the scheduler to place the displacing pod")
		// The displacing pod reaching a node is the proof that a preemption actually
		// happened. Without it, an absent worker pod could just as well mean the reaper
		// or a crash took it, and the whole measurement would be about the wrong event.
		Eventually(func(g Gomega) {
			out, err := utils.Run(exec.Command("kubectl", "get", "pod", preemptorPodName,
				"-n", preemptorNS, "-o", "jsonpath={.spec.nodeName}"))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(out)).To(Equal(slotNode),
				"the displacing pod is still unscheduled; no preemption has occurred yet")
		}, 3*time.Minute, 1*time.Second).Should(Succeed())

		By("waiting for the worker pod to be gone")
		Eventually(func(g Gomega) {
			out, err := utils.Run(exec.Command("kubectl", "get", "pod", podName,
				"-n", tenantNS, "--ignore-not-found", "-o", "name"))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(out)).To(BeEmpty(), "worker pod %s still exists", podName)
		}, 2*time.Minute, 1*time.Second).Should(Succeed())

		stopSampling()
		seq := observed.sequence()
		AddReportEntry("Q423 observed worker phase/reason/deletionTimestamp/disruptionReason", strings.Join(seq, " -> "))
		AddReportEntry("Q423 scheduler events on the worker pod", podEvents(tenantNS, podName))

		By("asserting the preempted pod never took the kubelet-eviction shape")
		// Recovery must be reached by the scheduler's marker, not by the pod turning up
		// Evicted. If this ever fails, the mechanism under test is not the one this spec
		// believes it is measuring, and the rerun assertion below would pass for the
		// wrong reason.
		Expect(seq).NotTo(BeEmpty(), "the sampler saw nothing; the measurement did not run")
		Expect(seq).NotTo(ContainElement(HavePrefix("Failed/Evicted")),
			"a preempted worker reached PodFailed/Evicted — the kubelet-eviction shape. That would mean "+
				"scheduler preemption DOES reach eviction recovery, contradicting Q421's and Q459's "+
				"measurements of the graceful-removal path: re-read the experiment before trusting either")

		By("asserting the preemption marker the recovery keys on was actually published")
		// The premise of Q497: PreemptionByScheduler is written only by kube-scheduler,
		// so it is safe to recover on where deletionTimestamp is not. If the marker never
		// appeared, a passing rerun assertion would mean the AGC re-ran for some other
		// reason entirely.
		Expect(seq).To(ContainElement(HaveSuffix("/"+preemptionConditionReason)),
			"the victim never carried DisruptionTarget/%s; that condition is the whole basis "+
				"for recovering this path, so recovery cannot be attributed to it", preemptionConditionReason)

		By("asserting the displaced run was re-run automatically")
		// The behaviour Q497 added, and the half of the oversubscription claim that was
		// unbacked before it: displaced work comes back without a human. handleEviction
		// waits out evictionRetryDelay before calling GitHub, so the rerun needs room to
		// appear — Eventually, not an immediate read.
		Eventually(func(g Gomega) {
			g.Expect(rerunCountForRun(runID)).To(BeNumerically(">=", 1),
				"a preempted worker's run was never re-run; the safety half of the oversubscription "+
					"claim is unbacked again — check the AGC logs for 'worker pod disrupted; scheduling auto-retry'")
		}, 90*time.Second, 2*time.Second).Should(Succeed())

		By("asserting the run was re-run exactly once")
		// At-most-once is what keeps one preemption from spending the run's whole retry
		// budget. The victim is readable for its entire termination grace period and the
		// worker-pod watch fires more than once inside it, so the claim annotation is
		// doing real work here rather than guarding a hypothetical.
		Consistently(func(g Gomega) {
			g.Expect(rerunCountForRun(runID)).To(Equal(1),
				"one preemption produced more than one rerun; the at-most-once claim on the "+
					"disrupted pod is not holding, and a run's retry budget can be spent by a single event")
		}, 30*time.Second, 3*time.Second).Should(Succeed())
	})

	// The spec above cannot answer what phase a preempted worker publishes on its way
	// out, because its victim is held Pending and so has no container to terminate. That
	// question decides whether recovery could ever key on the phase, and Q459 answered a
	// neighbouring version of it: a gracefully deleted RUNNING worker lands in PodFailed
	// with an empty reason — the same shape a genuinely failing job produces, which is
	// why that plan reaches for `deletionTimestamp` instead.
	//
	// This spec shows the phase is weaker still. Its victim is worker-shaped — the same
	// disruption-safety annotations, no PodDisruptionBudget — and running a process that
	// exits 0 on SIGTERM. Preempted, it lands in `Succeeded`. So the terminal phase on
	// this path is not merely ambiguous, it is not stable: Pending, Succeeded and Failed
	// all occur, decided by what the interrupted process was doing and what it exited
	// with. No phase/reason pair can discriminate a disruption from an ordinary outcome.
	//
	// It is deliberately NOT a gateway worker. A worker's command is the injected
	// wrapper, so its exit code is the runner's, and a fake-GitHub worker cannot be made to
	// exit 0 on demand. What is under test here is the kubelet's behaviour on a
	// preemption, which is worker-independent — so the pod is built to isolate it.
	It("E2E_AGC_PreemptedRunningPodPhaseFollowsItsExitCode: a preempted running pod can end Succeeded, never Evicted", func() {
		By("freeing the preempt slot the previous spec's displacing pod holds")
		_, _ = utils.Run(exec.Command("kubectl", "delete", "pod", preemptorPodName,
			"-n", preemptorNS, "--ignore-not-found", "--wait=true"))

		const victimPod = "graceful-victim"
		By("creating a worker-shaped victim that exits 0 on SIGTERM")
		Expect(utils.ApplyManifest(gracefulVictimManifest(
			preemptorNS, victimPod, tenantPriorityClass, slotNode, slotResource, curlImage,
		))).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(podPhase(preemptorNS, victimPod)).To(Equal("Running"))
		}, 3*time.Minute, time.Second).Should(Succeed(),
			"the victim must be running, or there is no terminal phase to observe")

		observed := newFieldRecorder(preemptorNS, victimPod,
			`{.status.phase}/{.status.reason}/{.metadata.deletionTimestamp}/`+
				`{.status.conditions[?(@.type=="DisruptionTarget")].reason}`)
		stopSampling := observed.start(200 * time.Millisecond)

		By("displacing it with the higher-priority pod")
		Expect(utils.ApplyManifest(preemptorPodManifest(
			preemptorNS, preemptorPodName, platformPriorityClass, slotNode, slotResource,
		))).To(Succeed())

		By("waiting for the victim to be gone")
		Eventually(func(g Gomega) {
			out, err := utils.Run(exec.Command("kubectl", "get", "pod", victimPod,
				"-n", preemptorNS, "--ignore-not-found", "-o", "name"))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(out)).To(BeEmpty(), "victim pod %s still exists", victimPod)
		}, 3*time.Minute, time.Second).Should(Succeed())

		stopSampling()
		seq := observed.sequence()
		AddReportEntry("Q423 running victim phase/reason/deletionTimestamp/disruptionReason", strings.Join(seq, " -> "))

		Expect(seq).NotTo(BeEmpty(), "the sampler saw nothing; the measurement did not run")
		Expect(seq).NotTo(ContainElement(HavePrefix("Failed/Evicted")),
			"a preempted running pod reached PodFailed/Evicted — the kubelet-eviction shape, which "+
				"scheduler preemption is not supposed to produce")
		Expect(seq).To(ContainElement(HavePrefix("Succeeded/")),
			"a preempted pod whose process exits 0 must end Succeeded — that is the finding: the "+
				"terminal phase follows the exit code, so it cannot tell a disruption from an outcome")
		Expect(seq).To(ContainElement(ContainSubstring("PreemptionByScheduler")),
			"the scheduler must mark its victim with DisruptionTarget/PreemptionByScheduler — "+
				"the discriminator Q497 would key on")
	})
})

// gracefulVictimManifest renders a preemption victim that terminates cleanly: it traps
// SIGTERM and exits 0, so the phase the kubelet publishes is `Succeeded`. The trap is
// the whole point — with a bare `sleep` the shell would die on the signal and the pod
// would land in `Failed`, which is the outcome this spec exists to distinguish from.
func gracefulVictimManifest(ns, name, priorityClassName, node, slotResource, image string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %s
  namespace: %s
  annotations:
    cluster-autoscaler.kubernetes.io/safe-to-evict: "false"
    karpenter.sh/do-not-disrupt: "true"
spec:
  priorityClassName: %s
  nodeSelector:
    kubernetes.io/hostname: %s
  restartPolicy: Never
  terminationGracePeriodSeconds: 30
  containers:
  - name: sleeper
    image: %s
    command: ["sh", "-c", "trap 'exit 0' TERM; sleep 3600 & wait"]
    resources:
      requests:
        cpu: 50m
        memory: 64Mi
        %s: "1"
      limits:
        cpu: 50m
        memory: 64Mi
        %s: "1"
`, name, ns, priorityClassName, node, image, slotResource, slotResource)
}

// gmcFlagValue returns the value of one --flag=value argument on the GMC manager
// container, or "" when the flag is absent.
func gmcFlagValue(flag string) string {
	GinkgoHelper()
	out, err := utils.Run(exec.Command("kubectl", "get", "deployment", "gmc-controller-manager",
		"-n", "gmc-system", "-o", "jsonpath={.spec.template.spec.containers[0].args}"))
	Expect(err).NotTo(HaveOccurred())
	for _, arg := range strings.FieldsFunc(out, func(r rune) bool {
		return r == ',' || r == '[' || r == ']' || r == '"'
	}) {
		if value, ok := strings.CutPrefix(strings.TrimSpace(arg), flag+"="); ok {
			return value
		}
	}
	return ""
}

// setAllowedPriorityClasses rewrites the platform PriorityClassAllowlist's
// spec.allowedPriorityClasses. A nil list restores the empty default, which is the
// fail-closed posture every other spec relies on.
func setAllowedPriorityClasses(name string, classes []string) {
	GinkgoHelper()
	list := "[]"
	if len(classes) > 0 {
		list = `["` + strings.Join(classes, `","`) + `"]`
	}
	out, err := utils.Run(exec.Command("kubectl", "patch", "priorityclassallowlist", name,
		"--type=merge", "-p", `{"spec":{"allowedPriorityClasses":`+list+`}}`))
	Expect(err).NotTo(HaveOccurred(), "patch PriorityClassAllowlist %s: %s", name, out)
}

// waitForNamespaceGone blocks until the namespace is absent, failing the spec if it is
// still Terminating when the timeout elapses. A namespace that will not drain is worth
// a loud failure rather than a silent leak: it holds the tenant's objects, and any spec
// that later reuses the name is blocked by it.
func waitForNamespaceGone(ns string, timeout time.Duration) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		out, err := utils.Run(exec.Command("kubectl", "get", "namespace", ns,
			"--ignore-not-found", "-o", "name"))
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(strings.TrimSpace(out)).To(BeEmpty(), "namespace %s has not finished terminating", ns)
	}, timeout, 2*time.Second).Should(Succeed())
}

// firstSchedulableWorkerNode returns a node that is Ready, uncordoned, and not the
// control plane. The suite's drain spec cordons a node, so schedulability is checked
// rather than assumed even though the two specs are both Serial.
func firstSchedulableWorkerNode() string {
	GinkgoHelper()
	// Cordoned nodes are filtered in Go rather than by a jsonpath predicate:
	// `?(@.spec.unschedulable!=true)` matches nothing at all on an uncordoned node,
	// because the field is absent rather than false and kubectl's jsonpath does not
	// treat a missing field as unequal to a value.
	out, err := utils.Run(exec.Command("kubectl", "get", "nodes",
		"-l", "!node-role.kubernetes.io/control-plane",
		"-o", `jsonpath={range .items[*]}{.metadata.name} {.spec.unschedulable}{"\n"}{end}`))
	Expect(err).NotTo(HaveOccurred())
	for _, line := range utils.GetNonEmptyLines(out) {
		fields := strings.Fields(line)
		if len(fields) == 1 || fields[1] != "true" {
			return fields[0]
		}
	}
	Fail("no schedulable worker node to advertise the preempt slot on")
	return ""
}

// advertiseExtendedResource adds count units of an integer extended resource to a
// node's status.capacity, the documented way to give a node a countable, exclusive
// resource. The kubelet does not manage names outside the kubernetes.io domain, so the
// value survives its node-status updates and mirrors into allocatable.
func advertiseExtendedResource(node, name string, count int) {
	GinkgoHelper()
	patch := fmt.Sprintf(`[{"op":"add","path":"/status/capacity/%s","value":"%d"}]`,
		strings.ReplaceAll(name, "/", "~1"), count)
	out, err := utils.Run(exec.Command("kubectl", "patch", "node", node,
		"--subresource=status", "--type=json", "-p", patch))
	Expect(err).NotTo(HaveOccurred(), "advertise %s on %s: %s", name, node, out)

	// Assert it reached allocatable, which is what the scheduler actually reads.
	// Capacity alone would leave the slot invisible and every pod requesting it
	// permanently unschedulable — a failure that would otherwise surface much later as
	// "the worker never got a node".
	Eventually(func(g Gomega) {
		got, err := utils.Run(exec.Command("kubectl", "get", "node", node,
			"-o", fmt.Sprintf("jsonpath={.status.allocatable['%s']}", strings.ReplaceAll(name, ".", `\.`))))
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(strings.TrimSpace(got)).To(Equal(fmt.Sprintf("%d", count)))
	}, time.Minute, time.Second).Should(Succeed(), "%s never became allocatable on %s", name, node)
}

// withdrawExtendedResource removes an extended resource from a node's capacity.
func withdrawExtendedResource(node, name string) {
	GinkgoHelper()
	patch := fmt.Sprintf(`[{"op":"remove","path":"/status/capacity/%s"}]`,
		strings.ReplaceAll(name, "/", "~1"))
	if out, err := utils.Run(exec.Command("kubectl", "patch", "node", node,
		"--subresource=status", "--type=json", "-p", patch)); err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "withdraw %s from %s failed: %v\n%s\n", name, node, err, out)
	}
}

// priorityClassManifest renders a cluster-scoped PriorityClass.
func priorityClassManifest(name string, value int, preemptionPolicy string) string {
	return fmt.Sprintf(`apiVersion: scheduling.k8s.io/v1
kind: PriorityClass
metadata:
  name: %s
value: %d
preemptionPolicy: %s
globalDefault: false
description: "Q423 oversubscription experiment; created and deleted by the e2e suite."
`, name, value, preemptionPolicy)
}

// preemptorPodManifest renders the pod whose scheduling can only succeed by displacing
// the worker. It uses an unpullable image on purpose: being SCHEDULED is the whole
// signal, and a pod that never starts a container costs the kind node nothing.
func preemptorPodManifest(ns, name, priorityClassName, node, slotResource string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %s
  namespace: %s
spec:
  priorityClassName: %s
  nodeSelector:
    kubernetes.io/hostname: %s
  restartPolicy: Never
  containers:
  - name: hog
    image: registry.invalid/preemptor:none
    resources:
      requests:
        cpu: 50m
        memory: 64Mi
        %s: "1"
      limits:
        cpu: 50m
        memory: 64Mi
        %s: "1"
`, name, ns, priorityClassName, node, slotResource, slotResource)
}

// podPriorityClass returns a pod's resolved .spec.priorityClassName.
func podPriorityClass(ns, name string) string {
	GinkgoHelper()
	out, err := utils.Run(exec.Command("kubectl", "get", "pod", name,
		"-n", ns, "-o", "jsonpath={.spec.priorityClassName}"))
	Expect(err).NotTo(HaveOccurred())
	return strings.TrimSpace(out)
}

// podEvents returns the events recorded against one pod, newest last. The scheduler
// records its preemption decision here ("Preempted" on the victim), which is the
// cluster's own account of what removed the pod — worth capturing alongside the
// sampled phases even though nothing asserts on it.
func podEvents(ns, name string) string {
	out, err := utils.Run(exec.Command("kubectl", "get", "events",
		"-n", ns, "--field-selector", "involvedObject.name="+name,
		"-o", `jsonpath={range .items[*]}{.reason}: {.message}{"\n"}{end}`))
	if err != nil {
		return "events unavailable: " + err.Error()
	}
	return strings.TrimSpace(out)
}
