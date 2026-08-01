//go:build e2e
// +build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"sort"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/actions-gateway/github-actions-gateway/gmc/test/utils"
)

// E2E_AGC_WorkerNodeDrain is the Q421 measurement: does a node drain reach
// eviction recovery?
//
// The two disruptions are different mechanisms and the AGC only handles one of
// them. The kubelet's node-pressure eviction leaves the worker pod in PodFailed
// with Status.Reason "Evicted", which both tiers act on — classic inline in
// provision(), scale-set from the owning reconciler (Q417). `kubectl drain` does
// something else entirely: it cordons the node and POSTs to each pod's eviction
// subresource, and an admitted eviction is a graceful DELETE. Reading the code
// says that never produces reason "Evicted" and so never reaches recovery, and
// Q417 shipped on exactly that reasoning — that the drain path is already owned
// by the worker wrapper's SIGTERM relay (Q385). Reading is a hypothesis; this
// spec is the measurement.
//
// It is the tier that has a kubelet, which is what the envtest half
// (cmd/agc/internal/controller/integration/drain_eviction_test.go) cannot supply:
// there a worker pod has no node, so eviction removes it instantly and the
// question of what a real `kubectl drain` does to a real scheduled pod cannot even
// be asked. Here the pod is bound to a node, the AGC's provisioning goroutine is
// blocked on it, and a real drain is what takes it away.
//
// # Why the drained pod is deliberately held Pending
//
// The worker pod here never starts its container: its image cannot be pulled, so
// it is *scheduled* (it has a nodeName, and a drain evicts it) and stays Pending
// indefinitely, holding the AGC's waitForCompletion open until the drain removes
// it. That is the only way to make the drain the cause of the pod's removal at
// this tier. A worker pod running the real runner image against fakegithub exits
// on its own within seconds — fakegithub's synthetic payload is not a job the
// runner can execute — so a drain aimed at it would land on a pod that had already
// failed by itself, and "no rerun fired" would be measuring the runner's exit
// rather than the drain.
//
// What that costs, stated plainly: this spec does not exercise the wrapper's
// SIGTERM relay, because there is no live container to signal. That half is
// Q385's, covered by the wrapper unit tests, and its end-to-end form needs a real
// GitHub job to report — a live-GitHub question, not a fake-GitHub one. What is measured
// here is the half that decides recovery: what a real drain does to the worker
// *pod object*, which is the only thing either tier's eviction detection reads.
//
// The held-Pending shape also pins this spec on the side of the Q502 boundary that
// stays unrecovered — and it is the spec that caught a mark-only rule crossing it: a
// real kubelet publishes a transient Failed-with-deletionTimestamp even for a deleted
// worker whose container never started, so recovery must (and does) additionally
// require a recorded container exit that the mark predates. This worker has none, no
// report ever reached GitHub, and "no rerun fired" remains the correct assertion. The
// recovered side — a running worker publishing Failed with the mark and the exit — is
// the envtest TerminalWithMark pair. Design boundary:
// docs/design/04-operational-flows.md §4.2; operator-facing behaviour:
// docs/operations/troubleshooting.md, "Draining a Worker Auto-Re-Runs the Jobs It
// Interrupts".
//
// Serial and multi-node. Serial because it cordons a node and sets fakegithub's
// global AcquireJob response; multi-node because a cordoned node must leave
// somewhere for the cluster's other workloads to live.
var _ = Describe("E2E_AGC_WorkerNodeDrain", Ordered, Serial, Label("multi-node"), func() {
	const (
		tenantNS   = "tenant-drain"
		agName     = "drainprobe-ag"
		secretName = "github-app-secret" //nolint:gosec // G101: the NAME of a Kubernetes Secret object, not a credential value.

		// runID is this spec's own workflow run. It is what scopes the rerun
		// assertion to this spec: /control/reruns is process-wide, and another
		// spec's (or a later experiment's) rerun must not be readable as ours.
		runID = "4210421"
		// repoOwner/repoName complete the run identity handleEviction needs. With
		// any of the three missing it returns early and no rerun fires for reasons
		// that have nothing to do with drain — which would make this spec pass
		// while measuring nothing.
		repoOwner = "drainorg"
		repoName  = "drainrepo"

		// unpullableImage holds the worker pod scheduled-but-Pending for as long as
		// the spec needs it (see the package-level note above). `.invalid` is a
		// reserved TLD, so the pull can never succeed — the same seam
		// E2E_AGC_WorkerPodLifecycle uses for its stuck-Pending arm.
		unpullableImage = "registry.invalid/drain-probe-runner:none"
		// pendingDeadline must outlast the whole spec: the Q95 reaper deletes a
		// stuck-Pending worker once it elapses, and a reaped pod would remove the
		// drain's subject before the drain ran.
		pendingDeadline = "30m"
	)

	var (
		drainPFCmd  *exec.Cmd
		drainedNode string
		// rgName is resolved from the cluster, not composed from agName. The GMC
		// derives a bootstrap RunnerGroup's name as "<gateway>-<first label>-<hash>",
		// and the hash is what both the worker-pod label selector and the drain's
		// --pod-selector key on — compose the name by hand and both silently match
		// nothing, which reads as "the job never produced a worker".
		rgName string
	)

	BeforeAll(func() {
		utils.CreateNamespace(tenantNS, nil)
		utils.CreateGitHubAppSecret(tenantNS, secretName, 12345, 67890, testRSAKeyPEM)
		utils.ApplyFakegithubEgressNetworkPolicy(tenantNS)

		// ApplyWithWebhookRetry, not Apply: a Serial spec can be the first CR applied
		// after the GMC rollout, before its validating webhook is serving, and the
		// bare apply then fails with a webhook context-deadline rather than anything
		// this spec is about.
		utils.RunnerTenant(tenantNS, agName, secretName, unpullableImage).
			WithLifecycle("30m", pendingDeadline).
			ApplyWithWebhookRetry()

		By("waiting for the AGC to be ready and fully reconciled")
		utils.WaitForDeploymentReady(tenantNS, agcName, 4*time.Minute)
		utils.WaitForRunnerGroupReconciled(tenantNS, 4*time.Minute)

		By("resolving the RunnerGroup name the GMC actually created")
		out, err := utils.Run(exec.Command("kubectl", "get", "runnergroup",
			"-n", tenantNS, "-o", "jsonpath={.items[0].metadata.name}"))
		Expect(err).NotTo(HaveOccurred())
		rgName = strings.TrimSpace(out)
		Expect(rgName).NotTo(BeEmpty(), "no RunnerGroup in %s to drain workers of", tenantNS)

		By("starting port-forward to the fakegithub control API")
		fakegithubLocalPort = fmt.Sprintf("%d", 19890+GinkgoParallelProcess())
		drainPFCmd = exec.Command("kubectl", "port-forward",
			"-n", infraNamespace,
			"service/"+fakegithubServiceName,
			fakegithubLocalPort+":9090",
		)
		Expect(drainPFCmd.Start()).To(Succeed())
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
		// Uncordon first and unconditionally: leaving a node unschedulable would
		// strand every suite that runs after this one, including this suite's own
		// teardown pods.
		if drainedNode != "" {
			By("uncordoning " + drainedNode)
			out, err := utils.Run(exec.Command("kubectl", "uncordon", drainedNode))
			if err != nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "uncordon %s failed: %v\n%s\n", drainedNode, err, out)
			}
		}
		if drainPFCmd != nil && drainPFCmd.Process != nil {
			_ = drainPFCmd.Process.Kill()
		}
		utils.DeleteActionsGatewayCR(tenantNS, agName)
		utils.DeleteNamespace(tenantNS)
	})

	It("E2E_AGC_DrainedWorkerGetsNoAutomaticRerun: a drained worker pod is deleted, never Evicted, and triggers no rerun", func() {
		By("confirming a rerun would be observable if one fired")
		// The whole spec turns on an absence, so the way it could be observed has to
		// be checked first. The AGC builds the rerun-failed-jobs URL from
		// GITHUB_API_BASE_URL; if that did not point at fakegithub, a rerun would
		// leave for the real api.github.com and fakegithub would report zero no
		// matter what the drain did.
		Expect(agcEnvValue(tenantNS, agcName, "GITHUB_API_BASE_URL")).To(ContainSubstring(fakegithubServiceName),
			"the AGC must address fakegithub for REST calls, or an absent rerun proves nothing")

		By("recording the pre-existing rerun count for this run")
		// Scoped to this spec's run, so a rerun another spec triggered cannot be
		// mistaken for ours (and vice versa).
		Expect(rerunCountForRun(runID)).To(Equal(0),
			"no rerun may exist for this spec's run before the spec has done anything")

		By("pinning the next AcquireJob response to a payload carrying a complete run identity")
		// The default fakegithub response carries no run identity, and handleEviction
		// returns early without one — so a rerun could not fire regardless of the
		// drain, and the measurement would be vacuous. This is a global on the shared
		// fakegithub, which is why the spec is Serial; it is reset the moment the
		// worker pod exists and the payload has been consumed.
		fakegithubSvcURL := fmt.Sprintf("http://%s.%s.svc.cluster.local:%s",
			fakegithubServiceName, infraNamespace, fakegithubServicePort)
		setAcquireJobResponse(map[string]interface{}{
			"plan": map[string]string{"planId": "drain-plan-" + runID},
			// The identity rides in the serialised `github` context — the shape a real
			// acquirejob body uses, and so the shape this spec must exercise. Building
			// it as system.github.* variables instead would assert against a shape
			// GitHub never sends, which is how the classic tier came to ship unable to
			// name a real job's run at all (Q495). The top-level `run_id` the payload
			// also accepts is an int64 there, and a string in it makes the whole
			// payload fail to unmarshal — costing the identity this spec depends on
			// rather than adding to it.
			"contextData": map[string]interface{}{
				"github": map[string]interface{}{
					"t": 2,
					"d": []map[string]interface{}{
						{"k": "repository", "v": repoOwner + "/" + repoName},
						{"k": "run_id", "v": runID},
					},
				},
			},
			"resources": map[string]interface{}{
				"endpoints": []map[string]interface{}{{
					"name": "SystemVssConnection",
					"url":  fakegithubSvcURL,
					"authorization": map[string]interface{}{
						"scheme":     "OAuth",
						"parameters": map[string]string{"AccessToken": "drain-job-token"},
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
			"jobId":           "drain-job-1",
			"jobName":         "drain probe",
			"run_service_url": fakegithubSvcURL,
		})

		By("waiting for the worker pod to be scheduled onto a node")
		var podName string
		Eventually(func(g Gomega) {
			podName, drainedNode = workerPodAndNode(g, tenantNS, rgName)
			g.Expect(podName).NotTo(BeEmpty(), "no worker pod yet")
			g.Expect(drainedNode).NotTo(BeEmpty(), "worker pod %s is not scheduled yet", podName)
		}, 4*time.Minute, 1*time.Second).Should(Succeed())
		// AddReportEntry, not GinkgoWriter: this spec exists to produce a measurement,
		// and GinkgoWriter output is discarded on a passing spec — exactly the run
		// whose numbers we want. Report entries survive into the suite output and the
		// JUnit report either way.
		AddReportEntry("Q421 drained worker pod", podName+" on node "+drainedNode)

		// The payload has been read; let the shared fakegithub go back to its default
		// so nothing after this spec inherits our run identity.
		setAcquireJobResponse(nil)

		By("confirming the drain's subject is a live worker, not one that already ended")
		// The load-bearing precondition. If the pod had reached a terminal phase on
		// its own, the drain would be evicting a corpse and every assertion below
		// would be about the runner's exit rather than the drain.
		Expect(podPhase(tenantNS, podName)).To(Equal("Pending"),
			"the worker pod must still be live when the drain starts")

		By("recording the node-disruption-safety annotations the AGC stamped")
		// The AGC marks every worker pod safe-to-evict=false / do-not-disrupt so
		// autoscalers and deschedulers leave a mid-job worker alone. `kubectl drain`
		// honours none of them — they are advisory to those controllers only — which
		// is exactly why the drain path needs its own answer rather than inheriting
		// theirs.
		safeToEvict := podAnnotation(tenantNS, podName, "cluster-autoscaler.kubernetes.io/safe-to-evict")
		AddReportEntry("Q421 safe-to-evict annotation on the drained worker", safeToEvict)
		Expect(safeToEvict).To(Equal("false"),
			"worker pods must carry the disruption-safety marker, or this spec is not draining a normally-protected pod")

		By("sampling the worker pod's phase/reason across the drain")
		// The measurement itself. An eviction is not instantaneous — the kubelet gets
		// a grace period — so the pod may publish a terminal phase before the object
		// is removed. Which phase (and with what reason) is exactly what decides
		// whether recovery could fire, and it is only visible while it is happening.
		observed := newPhaseRecorder(tenantNS, podName)
		stopSampling := observed.start(200 * time.Millisecond)

		By("cordoning the node and draining this tenant's worker pods off it")
		// A real `kubectl drain`, scoped by --pod-selector to this RunnerGroup's
		// workers. The scope is what keeps a shared e2e cluster usable: an unscoped
		// drain would also evict the GMC, cert-manager and fakegithub onto the other
		// node and destabilise every spec that follows. The per-pod action is
		// identical either way — cordon, then POST the eviction subresource — and
		// that action is the whole subject of the experiment.
		//
		// --force is required because a worker pod's controller is a RunnerGroup CR,
		// which kubectl cannot resolve as a known controller kind; an operator
		// draining a node with GAG workers on it hits the same requirement.
		drainOut, drainErr := utils.Run(exec.Command("kubectl", "drain", drainedNode,
			"--pod-selector", "actions-gateway/runner-group="+rgName,
			"--ignore-daemonsets",
			"--delete-emptydir-data",
			"--force",
			"--timeout=3m",
		))
		AddReportEntry("Q421 kubectl drain output", drainOut)
		Expect(drainErr).NotTo(HaveOccurred(), "drain must complete: %s", drainOut)

		By("waiting for the worker pod to be gone")
		Eventually(func(g Gomega) {
			out, err := utils.Run(exec.Command("kubectl", "get", "pod", podName,
				"-n", tenantNS, "--ignore-not-found", "-o", "name"))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(out)).To(BeEmpty(), "worker pod %s still exists", podName)
		}, 2*time.Minute, 1*time.Second).Should(Succeed())

		stopSampling()
		seq := observed.sequence()
		AddReportEntry("Q421 observed pod phase/reason sequence", strings.Join(seq, " -> "))

		By("asserting the drained pod never took the shape recovery acts on")
		Expect(seq).NotTo(BeEmpty(), "the sampler saw nothing; the measurement did not run")
		Expect(seq).NotTo(ContainElement("Failed/Evicted"),
			"a drained worker reached PodFailed/Evicted — the kubelet-eviction shape. "+
				"That contradicts the premise Q417 scoped eviction detection on, and means "+
				"the drain path DOES reach recovery: re-read the experiment before trusting either")

		By("asserting no automatic rerun was requested for this run")
		// handleEviction waits out evictionRetryDelay before calling GitHub, so a
		// rerun that was going to fire needs room to appear. Consistently is the
		// right assertion shape here: the claim is that none ever arrives.
		Consistently(func(g Gomega) {
			g.Expect(rerunCountForRun(runID)).To(Equal(0),
				"a drained worker triggered an automatic rerun; the AGC now recovers the drain path "+
					"and this experiment's premise (and Q417's) needs revisiting")
		}, 45*time.Second, 3*time.Second).Should(Succeed())
	})
})

// agcEnvValue reads one env var off an AGC Deployment's manager container —
// the shared v1 deployment (agcName) or a v2 per-gateway "<gw>-agc" one.
func agcEnvValue(ns, deploy, name string) string {
	GinkgoHelper()
	out, err := utils.Run(exec.Command("kubectl", "get", "deployment", deploy,
		"-n", ns,
		"-o", fmt.Sprintf("jsonpath={.spec.template.spec.containers[0].env[?(@.name=='%s')].value}", name),
	))
	Expect(err).NotTo(HaveOccurred())
	return strings.TrimSpace(out)
}

// agcPodIdentity returns the UIDs of an AGC Deployment's pods, sorted and joined —
// the identity of the control-plane process(es) a test is running against. A change
// between two reads means the AGC that observed an event is gone, and any in-process
// state it held (a delayed re-run, an in-flight session) went with it.
func agcPodIdentity(ns, deploy string) string {
	GinkgoHelper()
	out, err := utils.Run(exec.Command("kubectl", "get", "pods",
		"-n", ns, "-l", "app="+deploy,
		"-o", "jsonpath={range .items[*]}{.metadata.uid}{\"\\n\"}{end}"))
	Expect(err).NotTo(HaveOccurred())
	uids := strings.Fields(out)
	sort.Strings(uids)
	return strings.Join(uids, ",")
}

// podPhase returns a pod's current .status.phase.
func podPhase(ns, name string) string {
	GinkgoHelper()
	out, err := utils.Run(exec.Command("kubectl", "get", "pod", name,
		"-n", ns, "-o", "jsonpath={.status.phase}"))
	Expect(err).NotTo(HaveOccurred())
	return strings.TrimSpace(out)
}

// podAnnotation returns one annotation off a pod, or "" when it is absent.
func podAnnotation(ns, name, key string) string {
	GinkgoHelper()
	out, err := utils.Run(exec.Command("kubectl", "get", "pod", name,
		"-n", ns, "-o", fmt.Sprintf("jsonpath={.metadata.annotations['%s']}", strings.ReplaceAll(key, ".", `\.`))))
	Expect(err).NotTo(HaveOccurred())
	return strings.TrimSpace(out)
}

// workerPodAndNode returns the first worker pod of rgName in ns and the node it
// is bound to (empty while unscheduled).
func workerPodAndNode(g Gomega, ns, rgName string) (podName, nodeName string) {
	out, err := utils.Run(exec.Command("kubectl", "get", "pods",
		"-n", ns,
		"-l", "actions-gateway/runner-group="+rgName,
		"-o", "jsonpath={.items[0].metadata.name} {.items[0].spec.nodeName}",
	))
	g.Expect(err).NotTo(HaveOccurred())
	fields := strings.Fields(out)
	if len(fields) > 0 {
		podName = fields[0]
	}
	if len(fields) > 1 {
		nodeName = fields[1]
	}
	return podName, nodeName
}

// setAcquireJobResponse pins (or, with a nil body, resets) fakegithub's AcquireJob
// response through the control API.
func setAcquireJobResponse(payload map[string]interface{}) {
	GinkgoHelper()
	var body []byte
	if payload != nil {
		var err error
		body, err = json.Marshal(payload)
		Expect(err).NotTo(HaveOccurred())
	}
	fakegithubControlRequest(nil, "POST", "/control/acquirejob", body)
}

// rerunCountForRun reports how many rerun-failed-jobs calls fakegithub has
// accepted (201) for the given workflow run. Filtering by run rather than
// reading the global count is what makes the assertion this spec's own.
func rerunCountForRun(runID string) int {
	GinkgoHelper()
	out := fakegithubControlRequest(nil, "GET", "/control/reruns?run=%2Fruns%2F"+runID+"%2F", nil)
	var result struct {
		Count int `json:"count"`
	}
	Expect(json.Unmarshal([]byte(out), &result)).To(Succeed(), "parse reruns response: %s", out)
	return result.Count
}

// waitForRerun polls until a rerun-failed-jobs call has landed for runID, and
// reports whether one did inside timeout. It returns rather than asserting, so a
// caller can tell a re-run that is genuinely missing from one whose recovery was
// invalidated by something the spec does not control.
func waitForRerun(runID string, timeout, interval time.Duration) bool {
	GinkgoHelper()
	deadline := time.Now().Add(timeout)
	for {
		if rerunCountForRun(runID) >= 1 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(interval)
	}
}

// refusedRerunCountForRun reports how many rerun-failed-jobs calls fakegithub
// refused with the 403 still-running answer for the given run — the run must
// have been marked non-concluded via setRunConcluded (Q517).
func refusedRerunCountForRun(runID string) int {
	GinkgoHelper()
	out := fakegithubControlRequest(nil, "GET", "/control/reruns?run=%2Fruns%2F"+runID+"%2F", nil)
	var result struct {
		RefusedCount int `json:"refusedCount"`
	}
	Expect(json.Unmarshal([]byte(out), &result)).To(Succeed(), "parse reruns response: %s", out)
	return result.RefusedCount
}

// setRunConcluded marks a workflow run (non-)concluded on fakegithub: a
// non-concluded run refuses rerun-failed-jobs with 403 "This workflow is
// already running", the answer real GitHub gives until it concludes the
// original run (Q503/Q517).
func setRunConcluded(runID string, concluded bool) {
	GinkgoHelper()
	fakegithubControlRequest(nil, "POST",
		fmt.Sprintf("/control/runstate?run=%s&concluded=%t", runID, concluded), nil)
}

// phaseRecorder samples a pod's phase/reason on an interval and keeps the
// distinct values in the order they were first seen. A drain is a window, not an
// instant: the interesting states exist only while it is in flight, so they have
// to be captured as they happen rather than read afterwards from a pod that no
// longer exists.
type phaseRecorder struct {
	namespace string
	podName   string
	jsonPath  string
	seen      chan string
	done      chan struct{}
}

func newPhaseRecorder(namespace, podName string) *phaseRecorder {
	return newFieldRecorder(namespace, podName, "{.status.phase}/{.status.reason}")
}

// newFieldRecorder is newPhaseRecorder over an arbitrary jsonpath, for a spec that
// needs a different projection of the same "sample it while it is happening"
// behaviour — Q459 samples the deletionTimestamp alongside the phase, because
// whether one is set at the moment a terminal phase is published is exactly what
// distinguishes a disrupted worker from a job that failed on its own.
func newFieldRecorder(namespace, podName, jsonPath string) *phaseRecorder {
	return &phaseRecorder{
		namespace: namespace,
		podName:   podName,
		jsonPath:  jsonPath,
		// Buffered well past the number of samples a drain window produces, so a
		// sample is never dropped and never blocks the sampler.
		seen: make(chan string, 4096),
		done: make(chan struct{}),
	}
}

// start begins sampling and returns a function that stops it and blocks until the
// sampling goroutine has exited.
func (r *phaseRecorder) start(interval time.Duration) func() {
	stopped := make(chan struct{})
	go func() {
		defer close(r.done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stopped:
				return
			case <-ticker.C:
				out, err := utils.Run(exec.Command("kubectl", "get", "pod", r.podName,
					"-n", r.namespace, "--ignore-not-found",
					"-o", "jsonpath="+r.jsonPath,
				))
				if err != nil {
					continue
				}
				if s := strings.TrimSpace(out); s != "" {
					select {
					case r.seen <- s:
					default:
					}
				}
			}
		}
	}()
	return func() {
		close(stopped)
		<-r.done
	}
}

// sequence returns the distinct phase/reason values in first-seen order. Call it
// only after the stop function returned by start has run.
func (r *phaseRecorder) sequence() []string {
	close(r.seen)
	var out []string
	seen := map[string]bool{}
	for s := range r.seen {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
