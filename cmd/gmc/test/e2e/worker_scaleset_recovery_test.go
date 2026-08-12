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

	"github.com/actions-gateway/github-actions-gateway/gmc/test/utils"
)

// E2E_AGC_ScaleSetRecovery is the Q519 gate: scale-set disruption recovery run
// end-to-end under the chart's REAL agc-tenant-role, on a cluster that enforces
// RBAC. The envtest pair (TestAGC_Drain_ScaleSetWorkerTerminalWithMark_Recovers and
// its preemption twin) proves the same mechanism against a real apiserver — but
// envtest's client runs as admin, which is exactly how Q502's claim and
// completed-at pod patches shipped without their `pods` `patch` grant and silently
// 403'd on every real install. This spec exists so that failure class fails a CI
// gate: if a future controller change needs an RBAC verb the chart role does not
// grant, the claim patch is refused, no rerun ever fires, and the rerun assertion
// below goes red.
//
// # What is and is not exercised, stated plainly
//
// The recovery half of the scale-set tier runs for real: the deployed AGC's
// RunnerSet reconciler (under the chart role), its worker-pod watch, the
// Failed-with-deletion-mark discriminator (Q502), the optimistic-lock claim patch,
// and the rerun-failed-jobs call to fakegithub. The ACQUISITION half does not: this
// spec's gateway points at a host that does not resolve (see below), so the
// listener's session never opens here and the worker pod is staged by the
// spec rather than provisioned from an assignment. That is now a property of this
// spec rather than of the venue — fakegithub serves the scale-set protocol as of
// Q528, and E2E_AGC_ScaleSetAcquisition drives the acquisition half through it. The
// deliberately-failing bootstrap is retained here because this spec's subject is the
// recovery scan running on a set whose listener is NOT up (see below), which is the
// harder case. The pod restates the recovery-relevant shape
// ProvisionScaleSetWorker stamps — the runner-set owner label, the
// acquisition-protocol label the recovery scan filters on, and the run-identity
// annotations it re-runs from (cmd/agc/internal/provisioner/target.go,
// payload.go); the envtest pair covers that those stamps really come from the
// provisioning path. Recovery itself does not care who created the pod: it lists
// by labels and reads annotations, which is what makes a staged pod a faithful
// subject for the RBAC question this spec asks.
//
// The disruption is the graceful-deletion arm (a bare `kubectl delete pod`, the
// drain shape), on a RUNNING worker with a real kubelet: the deletion mark must
// land before the container's recorded exit, and the claim must land inside the
// real teardown window between terminal publish and the kubelet removing the
// object — the window envtest can only simulate with a finalizer. Design boundary:
// docs/design/04-operational-flows.md §4.2; operator-facing behaviour:
// docs/operations/troubleshooting.md, "Draining a Worker Auto-Re-Runs the Jobs It
// Interrupts".
//
// # Why the RunnerSet's listener failing to start is fine — and load-bearing
//
// The reconciler runs the recovery scan every reconcile BEFORE routing to the
// acquisition tier, so a ScaleSet-protocol set whose listener cannot bootstrap
// still recovers disrupted workers on every worker-pod event. The gateway's
// githubURL names a host that does not resolve, so each bootstrap attempt dies on
// NXDOMAIN in milliseconds — a fast, clean failure rather than a dial timeout that
// would stall reconciles through the claim window. The spec waits for that failure
// to surface on the RunnerSet's Ready condition before staging anything: it proves
// the reconciler is live on this set and past the recovery step's position in the
// loop.
//
// Do not "fix" that URL to something reachable. The condition wait below is the
// spec's precondition, not a formality: point it at the stub and the listener comes
// up, the wait times out after four minutes, and the spec fails having tested
// nothing (which is how Q528 first broke it).
var _ = Describe("E2E_AGC_ScaleSetRecovery", Ordered, func() {
	const (
		tenantNS   = "tenant-ss-recovery"
		gwName     = "ssrec"
		agcDeploy  = gwName + "-agc"
		setName    = "set-ssrec"
		secretName = "github-app-secret" //nolint:gosec // G101: the NAME of a Kubernetes Secret object, not a credential value.

		// runIDBase scopes the rerun assertion to this spec: /control/reruns is
		// process-wide, and another spec's rerun must not be readable as ours. Each
		// attempt appends its number, so a re-staged attempt (see below) reads a run
		// the abandoned one never touched.
		runIDBase = "5190519"
		repoOwner = "ssrecorg"
		repoName  = "ssrecrepo"

		probePodBase = "ssrec-drain-probe"

		// A disruption whose claim was won by an AGC that then went away is not
		// recoverable by any later AGC, so the spec re-stages instead of asserting on
		// it. Bounded: two control-plane replacements in two consecutive claim windows
		// is not churn, it is a defect worth failing on.
		maxAttempts = 3

		// The recovery-relevant shape ProvisionScaleSetWorker stamps, restated as
		// literals because this module cannot import the AGC's internal provisioner
		// package. If a constant moves in cmd/agc/internal/provisioner/target.go or
		// payload.go, the staged pod stops matching the scan and this spec fails
		// loudly on the rerun assertion — the right direction for drift.
		labelRunnerSet           = "actions-gateway.com/runner-set"
		labelAcquisitionProtocol = "actions-gateway.com/acquisition-protocol"
		annotationRunID          = "actions-gateway.com/run-id"
		annotationRepository     = "actions-gateway.com/repository"
		annotationClaim          = "actions-gateway.com/eviction-handled-at"
	)

	var ssrecPFCmd *exec.Cmd

	BeforeAll(func() {
		utils.CreateNamespace(tenantNS, map[string]string{
			"actions-gateway.com/tenant": "managed",
		})
		utils.CreateGitHubAppSecret(tenantNS, secretName, 12345, 67890, testRSAKeyPEM)
		utils.ApplyFakegithubEgressNetworkPolicy(tenantNS)

		By("applying the v2 object set: one ActionsGateway, one template, one ScaleSet-protocol RunnerSet")
		Expect(utils.ApplyManifestWithWebhookRetry(scaleSetRecoveryManifest(tenantNS, secretName, agcImage))).To(Succeed())

		By("waiting for the per-gateway AGC Deployment to become ready")
		utils.WaitForDeploymentReady(tenantNS, agcDeploy, 4*time.Minute)

		By("starting port-forward to the fakegithub control API")
		fakegithubLocalPort = fmt.Sprintf("%d", 20190+GinkgoParallelProcess())
		ssrecPFCmd = exec.Command("kubectl", "port-forward",
			"-n", infraNamespace,
			"service/"+fakegithubServiceName,
			fakegithubLocalPort+":9090",
		)
		Expect(ssrecPFCmd.Start()).To(Succeed())
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
			utils.DumpAGCSessionDiagnostics(tenantNS, agcDeploy, infraNamespace, fakegithubServiceName)
		}
	})

	AfterAll(func() {
		if ssrecPFCmd != nil && ssrecPFCmd.Process != nil {
			_ = ssrecPFCmd.Process.Kill()
		}
		// Delete the tenant CRs in dependency order, WAITING on each, before the
		// namespace: the RunnerSet's agentpool-cleanup finalizer is cleared by the
		// AGC, which lives in this namespace — a bare namespace delete races the AGC
		// pod's own termination and a lost race wedges the namespace in Terminating
		// forever (observed on this spec's first run). The gateway goes second so the
		// GMC tears the AGC control plane down while it can still reconcile.
		_, _ = utils.Run(exec.Command("kubectl", "delete", "runnerset", setName,
			"-n", tenantNS, "--ignore-not-found", "--timeout=2m"))
		_, _ = utils.Run(exec.Command("kubectl", "delete", "actionsgateways.actions-gateway.com", gwName,
			"-n", tenantNS, "--ignore-not-found", "--timeout=2m"))
		// The per-gateway ClusterRoleBinding is cluster-scoped and not namespace-GC'd.
		_, _ = utils.Run(exec.Command("kubectl", "delete", "clusterrolebinding",
			"agc-clusterrunnertemplate-reader."+tenantNS+"."+gwName, "--ignore-not-found"))
		utils.DeleteNamespace(tenantNS)
	})

	It("E2E_AGC_ScaleSetDrainedWorkerClaimAndRerunLandUnderChartRBAC: a deleted scale-set worker is claimed and its run re-run by the AGC under the chart role", func() {
		By("confirming a rerun would be observable if one fired")
		// The AGC builds the rerun-failed-jobs URL from GITHUB_API_BASE_URL; if that
		// did not address fakegithub the call would leave for the real api.github.com
		// and fakegithub would report zero whatever the recovery did.
		Expect(agcEnvValue(tenantNS, agcDeploy, "GITHUB_API_BASE_URL")).To(ContainSubstring(fakegithubServiceName),
			"the AGC must address fakegithub for REST calls, or the rerun assertion proves nothing")

		By("waiting for the RunnerSet reconciler to be live on this set")
		// The listener bootstrap fails by design (see the package comment); the Ready
		// condition carrying that failure proves references resolved and the reconcile
		// loop — recovery scan included — is running for this set. Staging the
		// disruption before this point could race the reconciler's startup and read as
		// a recovery failure.
		Eventually(func(g Gomega) {
			out, err := utils.Run(exec.Command("kubectl", "get", "runnerset", setName,
				"-n", tenantNS,
				"-o", `jsonpath={.status.conditions[?(@.type=="Ready")].reason}/{.status.conditions[?(@.type=="Ready")].message}`))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(ContainSubstring("scale-set listener failed to start"),
				"RunnerSet Ready condition is %q; the reconciler has not yet attempted (and failed) the listener bootstrap", out)
		}, 4*time.Minute, 2*time.Second).Should(Succeed())

		// One AGC process has to both claim the disruption and re-run it, and the two
		// halves are not equally durable: claimEvictionRecovery stamps
		// AnnotationEvictionHandledAt BEFORE handleEviction's delayed GitHub call, and
		// disruptionAwaitingRecovery skips an already-stamped pod forever after. An AGC
		// replaced inside that window takes the recovery with it, and no later AGC can
		// produce the re-run — the deletion arm is deliberately not restart-safe
		// (provisioner/eviction_scaleset.go). The window is not the spec's to control:
		// the GMC owns the AGC Deployment and rolls it when its rendered pod template
		// changes (Q549, measured on run 30658951388). So each attempt pins the control
		// plane it is testing, and a broken pin means the attempt measured nothing.
		// Reasoning: testing.md § Pin the process when the signal comes out of its memory.
		var runID string
		for attempt := 1; attempt <= maxAttempts; attempt++ {
			probePod := fmt.Sprintf("%s-%d", probePodBase, attempt)
			runID = fmt.Sprintf("%s%d", runIDBase, attempt)

			By("waiting for the AGC control plane to settle, and pinning it")
			_, err := utils.Run(exec.Command("kubectl", "rollout", "status",
				"deployment/"+agcDeploy, "-n", tenantNS, "--timeout=4m"))
			Expect(err).NotTo(HaveOccurred())
			pinnedAGC := agcPodIdentity(tenantNS, agcDeploy)
			Expect(pinnedAGC).NotTo(BeEmpty(), "no AGC pod to run the disruption against")

			By("recording the pre-existing rerun count for this run")
			Expect(rerunCountForRun(runID)).To(Equal(0),
				"no rerun may exist for this attempt's run before the attempt has done anything")

			By("staging a running scale-set worker carrying the run identity")
			Expect(utils.ApplyManifest(scaleSetWorkerProbeManifest(
				tenantNS, probePod, setName, runID, repoOwner+"/"+repoName, curlImage,
			))).To(Succeed())
			DeferCleanup(func() {
				_, _ = utils.Run(exec.Command("kubectl", "delete", "pod", probePod,
					"-n", tenantNS, "--ignore-not-found", "--wait=false"))
			})
			Eventually(func(g Gomega) {
				g.Expect(podPhase(tenantNS, probePod)).To(Equal("Running"))
			}, 3*time.Minute, time.Second).Should(Succeed(),
				"the worker must be Running before the delete, or the deletion arm's "+
					"mark-before-exit ordering cannot be produced")

			By("sampling the pod's phase, deletion mark, and claim annotation across the teardown")
			// Diagnostics, not evidence: a sampler bounds what was observed, never what
			// happened (testing.md § negative assertions), so nothing below asserts on
			// this sequence beyond it being non-empty. The claim's proof is the rerun —
			// the reconciler calls GitHub only after the claim patch succeeds, so a rerun
			// landing IS the claim landing.
			observed := newFieldRecorder(tenantNS, probePod,
				`{.status.phase}/{.metadata.deletionTimestamp}/`+
					`{.metadata.annotations['actions-gateway\.com/eviction-handled-at']}`)
			stopSampling := observed.start(100 * time.Millisecond)

			By("deleting the worker pod gracefully, as a drain would")
			_, err = utils.Run(exec.Command("kubectl", "delete", "pod", probePod,
				"-n", tenantNS, "--wait=false"))
			Expect(err).NotTo(HaveOccurred())

			By("waiting for the worker pod to be gone")
			Eventually(func(g Gomega) {
				out, err := utils.Run(exec.Command("kubectl", "get", "pod", probePod,
					"-n", tenantNS, "--ignore-not-found", "-o", "name"))
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(out)).To(BeEmpty(), "worker pod %s still exists", probePod)
			}, 2*time.Minute, time.Second).Should(Succeed())

			stopSampling()
			seq := observed.sequence()
			AddReportEntry(fmt.Sprintf("Q519 attempt %d observed phase/deletionTimestamp/%s", attempt, annotationClaim),
				strings.Join(seq, " -> "))
			Expect(seq).NotTo(BeEmpty(), "the sampler saw nothing; the pod was never observed")

			By("waiting for the disrupted run to be re-run automatically")
			// The whole point. If the chart role were missing a verb the recovery path
			// needs — the pods patch behind the claim above all — the claim is refused
			// with a Forbidden and no rerun ever fires, which is exactly the 403-broken
			// shipping mode Q502 found and this spec exists to catch. handleEviction
			// waits out evictionRetryDelay (5s here) before calling GitHub, so the rerun
			// needs room to appear.
			if waitForRerun(runID, 90*time.Second, 2*time.Second) {
				break
			}

			// No re-run. Whether that is the defect this spec exists to catch depends
			// entirely on whether the attempt got a window in which the recovery could
			// have run at all. Two ways it does not, both re-staged rather than failed.

			// The kubelet removed the pod between the AGC's cached List and its claim
			// patch, so the disruption's only record was gone before anything could be
			// claimed (Q809). No AGC can recover that, under any role — the attempt says
			// nothing about the RBAC question, exactly like a replaced control plane
			// below. Read from the AGC rather than inferred: an unclaimed pod looks
			// identical whether the claim was refused with a Forbidden (the defect) or
			// lost to the deletion (not the defect), and only the AGC knows which.
			if evictionRecoveryEvidenceLost(tenantNS, agcDeploy, probePod) {
				AddReportEntry("Q809 re-staging", fmt.Sprintf(
					"attempt %d: the worker pod was deleted before the AGC could claim its recovery, so no "+
						"re-run was possible and the chart role was never exercised", attempt))
				Expect(attempt).To(BeNumerically("<", maxAttempts),
					"the disrupted pod was deleted before the claim could land on every one of %d attempts; "+
						"the drain-recovery window is not reachable on this cluster at all", maxAttempts)
				continue
			}

			if agcPodIdentity(tenantNS, agcDeploy) == pinnedAGC {
				Fail("a deleted scale-set worker's run was never re-run under the chart role, and the AGC " +
					"that observed the disruption is still running; either the role lost a verb the recovery " +
					"path needs (check the AGC logs for 'could not claim scale-set worker disruption') or the " +
					"deletion-mark discriminator regressed")
			}
			AddReportEntry("Q549 re-staging", fmt.Sprintf(
				"attempt %d: the AGC control plane was replaced inside the claim window (pinned %q, now %q); "+
					"the claim is durable and the re-run is not, so this disruption is unrecoverable by any "+
					"later AGC and proves nothing either way",
				attempt, pinnedAGC, agcPodIdentity(tenantNS, agcDeploy)))
			Expect(attempt).To(BeNumerically("<", maxAttempts),
				"the AGC control plane was replaced inside the claim window on every one of %d attempts; "+
					"the spec never got an undisturbed window to test in", maxAttempts)
		}

		By("asserting the run was re-run exactly once")
		// The claim annotation is what makes recovery at-most-once per disrupted pod,
		// across however many reconciles observe the terminating pod.
		Consistently(func(g Gomega) {
			g.Expect(rerunCountForRun(runID)).To(Equal(1),
				"one deletion produced more than one rerun; the at-most-once claim on the "+
					"disrupted pod is not holding, and a run's retry budget can be spent by a single event")
		}, 30*time.Second, 3*time.Second).Should(Succeed())
	})
})

// evictionRecoveryEvidenceLost reports whether the AGC found probePod's disruption and
// then lost it because the pod was deleted before the claim patch could land — the Q809
// race, measured on three e2e-calico runs on 2026-08-12. The kubelet removes a drained
// worker's object seconds after its container exits, and the recovery scan lists from
// the informer cache and patches through the live client, so the window is real and
// narrow.
//
// The AGC is the only witness. The pod is gone either way, and an unclaimed pod looks
// the same whether the claim was refused (the RBAC defect this spec exists to catch) or
// never got to be made. The AGC logs the second case at Warn with the pod's name, which
// is what makes the two separable at all.
func evictionRecoveryEvidenceLost(ns, deploy, probePod string) bool {
	GinkgoHelper()
	out, err := utils.Run(exec.Command("kubectl", "logs",
		"-n", ns, "-l", "app="+deploy, "--tail=-1", "--prefix"))
	if err != nil {
		return false // no logs to read is not evidence of a lost claim
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "disruption was lost before it could be claimed") &&
			strings.Contains(line, probePod) {
			return true
		}
	}
	return false
}

// scaleSetRecoveryManifest renders the tenant: one gateway, one template, one
// ScaleSet-protocol RunnerSet. The gateway's githubURL deliberately names a host
// that does not resolve, so the scale-set client's bootstrap fails in milliseconds
// on NXDOMAIN — never a dial timeout that would stall the reconcile loop this spec
// depends on (see the package comment).
//
// It used to name the fakegithub service over https, failing on the TLS handshake
// against the plaintext port. That stopped working when Q528 taught the AGC to swap
// exactly that scheme for exactly that host: the bootstrap started succeeding and
// this spec's precondition evaporated. An unresolvable host is outside the rewrite's
// scope (cmd/agc/internal/controller.scaleSetStubURLs) and cannot be re-pointed by
// anything, which is the property this spec actually needs.
//
// workerImage is a placeholder: no job is ever assigned, so no worker is ever
// provisioned from the template.
func scaleSetRecoveryManifest(ns, secretName, workerImage string) string {
	githubURL := "https://ghes.invalid/ssrecorg"
	return fmt.Sprintf(`apiVersion: actions-gateway.com/v2alpha1
kind: ActionsGateway
metadata:
  name: ssrec
  namespace: %[1]s
spec:
  githubURL: %[4]s
  credentials:
    type: GitHubApp
    githubApp:
      name: %[2]s
  logLevel: debug
---
apiVersion: actions-gateway.com/v2alpha1
kind: RunnerTemplate
metadata:
  name: tmpl
  namespace: %[1]s
spec:
  workerImage: %[3]s
  podTemplate:
    spec:
      containers:
      - name: runner
        image: %[3]s
---
apiVersion: actions-gateway.com/v2alpha1
kind: RunnerSet
metadata:
  name: set-ssrec
  namespace: %[1]s
spec:
  gatewayRef:
    name: ssrec
  templateRef:
    name: tmpl
  acquisitionProtocol: ScaleSet
  runnerLabels: ["e2e-ssrec"]
  # Keep the spec's rerun wait short; the CRD floor is 1s, the shipped default 5s.
  evictionRetryDelay: 5s
`, ns, secretName, workerImage, githubURL)
}

// scaleSetWorkerProbeManifest renders the staged scale-set worker: the owner and
// acquisition-protocol labels the recovery scan selects on, the run-identity
// annotations it re-runs from, and a container that exits 1 on SIGTERM. The exit
// code is load-bearing twice over: a non-zero exit is what lands the pod in
// PodFailed (the only phase the deletion arm recovers), and the recorded
// finishedAt it produces is what the scan orders the deletion mark against — a
// worker that never ran has no exit record and must not be recovered (Q502).
func scaleSetWorkerProbeManifest(ns, name, runnerSet, runID, repository, image string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %s
  namespace: %s
  labels:
    actions-gateway.com/runner-set: %s
    actions-gateway.com/acquisition-protocol: ScaleSet
  annotations:
    actions-gateway.com/run-id: "%s"
    actions-gateway.com/repository: %s
spec:
  restartPolicy: Never
  terminationGracePeriodSeconds: 30
  containers:
  - name: runner
    image: %s
    command: ["sh", "-c", "trap 'exit 1' TERM; sleep 3600 & wait"]
    resources:
      requests:
        cpu: 50m
        memory: 64Mi
      limits:
        cpu: 50m
        memory: 64Mi
`, name, ns, runnerSet, runID, repository, image)
}
