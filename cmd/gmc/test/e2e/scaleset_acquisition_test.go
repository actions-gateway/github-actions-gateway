//go:build e2e
// +build e2e

package e2e

import (
	"encoding/json"
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

// E2E_AGC_ScaleSetAcquisition is the Q528 gate: the scale-set tier's ACQUISITION
// half, run end-to-end on a real cluster.
//
// Its sibling E2E_AGC_ScaleSetRecovery covers the other half and says plainly that
// this one is missing — the e2e fakegithub spoke only the classic broker protocol,
// so a scale-set listener's session could never open there and that spec has to
// stage its worker pod by hand. fakegithub now serves the scale-set protocol too
// (scaleset/scalesetstub, the same model the scaleset client's unit tests run
// against), which is what makes this spec possible.
//
// # What runs for real here, and what does not
//
// Real: the deployed AGC's two-hop bootstrap (registration-token, then the
// RemoteAuth runner-registration hop), scale-set resolution and creation, the
// message-queue session, the capacity-advertising long poll, the JobAvailable→
// acquire claim, generatejitconfig, and ProvisionScaleSetWorker creating a worker
// pod on a real kubelet under the chart's real agc-tenant-role. Every one of those
// had previously run only against envtest and the in-process stub.
//
// Not real: the runner. The JIT config fakegithub mints is a syntactic placeholder,
// so nothing starts from it — the same boundary every fake-GitHub spec has (the
// classic job-lifecycle specs assert a worker pod appears and stop there too). A
// real runner executing a real job belongs to the live-GitHub tier
// (E2E_GitHub_RealDispatch).
//
// So the terminal assertion is the staged JIT-config Secret rather than a pod
// phase: ProvisionScaleSetWorker stages the blob as a per-job Secret the pod mounts,
// and its presence is what proves the whole chain — assignment, JIT mint, Secret
// stage, pod create — completed. The container's own exit says nothing either way.
//
// # Why this spec must not run in parallel with another scale-set spec
//
// The acquire-flow toggle is process-wide on fakegithub (the flow is a property of
// the backend, not of a tenant), unlike the classic tier's owner-scoped single-use
// and redelivery toggles. Both flows are exercised inside one Ordered container so
// the ordering is under this spec's control.
var _ = Describe("E2E_AGC_ScaleSetAcquisition", Ordered, func() {
	const (
		tenantNS   = "tenant-ss-acquire"
		gwName     = "ssacq"
		agcDeploy  = gwName + "-agc"
		setName    = "set-ssacq"
		secretName = "github-app-secret" //nolint:gosec // G101: the NAME of a Kubernetes Secret object, not a credential value.

		// The scale set's name IS the RunnerSet's single runs-on label (CEL enforces
		// exactly one for a ScaleSet set), so this is also how the control API
		// addresses the scale set the AGC registered.
		scaleSetLabel = "e2e-ssacq"

		// The worker-pod stamps ProvisionScaleSetWorker applies, restated as literals
		// because this module cannot import the AGC's internal provisioner package.
		// A constant moving in cmd/agc/internal/provisioner/{target,payload}.go fails
		// this spec loudly rather than silently — the right direction for drift.
		labelRunnerSet           = "actions-gateway.com/runner-set"
		labelAcquisitionProtocol = "actions-gateway.com/acquisition-protocol"
		annotationRunID          = "actions-gateway.com/run-id"
		annotationRepository     = "actions-gateway.com/repository"
	)

	var ssacqPFCmd *exec.Cmd

	BeforeAll(func() {
		utils.CreateNamespace(tenantNS, map[string]string{
			"actions-gateway.com/tenant": "managed",
		})
		utils.CreateGitHubAppSecret(tenantNS, secretName, 12345, 67890, testRSAKeyPEM)
		utils.ApplyFakegithubEgressNetworkPolicy(tenantNS)

		By("applying the v2 object set: one ActionsGateway, one template, one ScaleSet-protocol RunnerSet")
		Expect(utils.ApplyManifestWithWebhookRetry(
			scaleSetAcquisitionManifest(tenantNS, secretName, workerImage))).To(Succeed())

		By("waiting for the per-gateway AGC Deployment to become ready")
		utils.WaitForDeploymentReady(tenantNS, agcDeploy, 4*time.Minute)

		By("starting port-forward to the fakegithub control API")
		fakegithubLocalPort = fmt.Sprintf("%d", 20290+GinkgoParallelProcess())
		ssacqPFCmd = exec.Command("kubectl", "port-forward",
			"-n", infraNamespace,
			"service/"+fakegithubServiceName,
			fakegithubLocalPort+":9090",
		)
		Expect(ssacqPFCmd.Start()).To(Succeed())
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
		// Leave the shared fakegithub on its default flow, whatever this spec did to
		// it: the toggle is process-wide and outlives the tenant.
		_, _ = scaleSetControl(http.MethodPost, "/control/scaleset/acquireflow?ghes=false")

		if ssacqPFCmd != nil && ssacqPFCmd.Process != nil {
			_ = ssacqPFCmd.Process.Kill()
		}
		// Delete the tenant CRs in dependency order, WAITING on each, before the
		// namespace: the RunnerSet's agentpool-cleanup finalizer is cleared by the AGC,
		// which lives in this namespace, so a bare namespace delete races the AGC pod's
		// own termination and a lost race wedges the namespace in Terminating.
		_, _ = utils.Run(exec.Command("kubectl", "delete", "runnerset", setName,
			"-n", tenantNS, "--ignore-not-found", "--timeout=2m"))
		_, _ = utils.Run(exec.Command("kubectl", "delete", "actionsgateways.actions-gateway.com", gwName,
			"-n", tenantNS, "--ignore-not-found", "--timeout=2m"))
		_, _ = utils.Run(exec.Command("kubectl", "delete", "clusterrolebinding",
			"agc-clusterrunnertemplate-reader."+tenantNS+"."+gwName, "--ignore-not-found"))
		utils.DeleteNamespace(tenantNS)
	})

	It("E2E_AGC_ScaleSetListenerOpensItsSessionAgainstFakegithub: the listener completes the two-hop bootstrap and registers a live message-queue session", func() {
		By("confirming the AGC is pointed at the stub for the scale-set bootstrap")
		// The scale-set endpoints are derived from spec.gitHubURL unless the stub env
		// re-points them, and gitHubURL is pinned to https by the CRD and the webhook
		// so it can never name plaintext fakegithub. Without this pair the bootstrap
		// leaves for real GitHub and every assertion below fails for the wrong reason.
		Expect(agcEnvValue(tenantNS, agcDeploy, "STUB_BROKER_URL")).To(ContainSubstring(fakegithubServiceName),
			"the AGC must address fakegithub for the scale-set bootstrap")

		By("waiting for fakegithub to report a live session for the scale set")
		// Server-side, not log-scraped: an absent session and a session that opened and
		// died are different failures, and only the server can tell them apart.
		Eventually(func(g Gomega) {
			state := scaleSetState(g)
			g.Expect(state["activeSession"]).To(BeTrue(),
				"no live message-queue session for scale set %q; the listener never completed "+
					"its bootstrap (check the AGC logs for 'scale-set listener failed to start')", scaleSetLabel)
		}, 4*time.Minute, 2*time.Second).Should(Succeed())
	})

	It("E2E_AGC_ScaleSetAssignedJobProvisionsAWorker: an auto-assigned job is provisioned as a worker pod carrying the assignment's run identity", func() {
		By("enqueuing a job on the scale set")
		job := scaleSetEnqueue()

		By("waiting for the worker pod the assignment provisioned")
		var podName string
		Eventually(func(g Gomega) {
			podName = firstScaleSetWorkerPod(g, tenantNS, setName)
			g.Expect(podName).NotTo(BeEmpty(),
				"no scale-set worker pod was provisioned; the assignment never reached "+
					"ProvisionScaleSetWorker (JIT mint or capacity gate)")
		}, 3*time.Minute, 2*time.Second).Should(Succeed())

		By("asserting the pod carries the assignment's run identity")
		// This is the only place the scale-set tier learns which run a job belongs to,
		// and the pod is where it has to survive for eviction recovery to read it back
		// after the pod is gone (Q417). Asserted against what fakegithub said it would
		// deliver, not against restated literals.
		Expect(podAnnotation(tenantNS, podName, annotationRunID)).
			To(Equal(fmt.Sprintf("%d", job.WorkflowRunID)))
		Expect(podAnnotation(tenantNS, podName, annotationRepository)).
			To(Equal(job.OwnerName + "/" + job.RepositoryName))
		Expect(podLabel(tenantNS, podName, labelAcquisitionProtocol)).To(Equal("ScaleSet"))
		Expect(podLabel(tenantNS, podName, labelRunnerSet)).To(Equal(setName))

		By("asserting the worker was switched into scale-set mode")
		Expect(podContainerEnv(tenantNS, podName, "runner", "WORKER_MODE")).To(Equal("scaleset"),
			"the worker must run the wrapper's --jitconfig path, not the classic payload path")

		By("asserting the JIT config fakegithub minted was staged for the worker")
		// The pod mounts this Secret, so its absence would strand the worker in
		// ContainerCreating. Asserting the Secret directly rather than waiting for a
		// pod phase is what this venue supports: the AGC replaces the runner
		// container's command with the wrapper, and fakegithub's JIT config is a
		// syntactic placeholder no real runner starts from — so the container's own
		// exit says nothing about acquisition either way.
		Expect(jitConfigSecretBlob(tenantNS, podName)).NotTo(BeEmpty(),
			"no JIT config was staged for job %s; generatejitconfig completed but the blob "+
				"never reached the provisioner", job.JobID)

		By("asserting the server counts the job as assigned")
		Eventually(func(g Gomega) {
			g.Expect(scaleSetState(g)["assignedJobs"]).To(BeNumerically(">=", 1))
		}, 30*time.Second, time.Second).Should(Succeed())
	})

	It("E2E_AGC_ScaleSetGHESFlowClaimsBeforeProvisioning: an offered job is claimed with acquirejobs and only then provisioned", func() {
		By("recording the acquirejobs count before switching flows")
		// Not assumed to be zero: the count is stub-wide, and the auto-assign job
		// above may have produced claims of its own on a retry.
		var before float64
		Eventually(func(g Gomega) {
			g.Expect(scaleSetState(g)["acquireJobsCalls"]).To(BeAssignableToTypeOf(float64(0)))
			before = scaleSetState(g)["acquireJobsCalls"].(float64)
		}, 30*time.Second, time.Second).Should(Succeed())

		By("switching fakegithub to the GHES JobAvailable→acquire flow")
		_, err := scaleSetControl(http.MethodPost, "/control/scaleset/acquireflow?ghes=true")
		Expect(err).NotTo(HaveOccurred())

		By("enqueuing a job that must be claimed before it is assigned")
		job := scaleSetEnqueue()

		By("asserting the AGC claimed the offer")
		// The rung auto-assign skips entirely. Without it a GHES tenant's jobs are
		// offered and never taken, which looks identical to no jobs being queued.
		Eventually(func(g Gomega) {
			g.Expect(scaleSetState(g)["acquireJobsCalls"]).To(BeNumerically(">", before),
				"the AGC never issued acquirejobs for the offered job; a GHES tenant would never run it")
		}, 2*time.Minute, 2*time.Second).Should(Succeed())

		By("waiting for the claimed job's worker pod")
		Eventually(func(g Gomega) {
			pods := scaleSetWorkerPodsWithRunID(g, tenantNS, setName, fmt.Sprintf("%d", job.WorkflowRunID))
			g.Expect(pods).NotTo(BeEmpty(),
				"the claimed job produced no worker pod; the claim landed but the assignment "+
					"that follows it never reached the provisioner")
		}, 3*time.Minute, 2*time.Second).Should(Succeed())

		// The server's ordered call log, attached to the report. Diagnostics, not
		// evidence — the assertions above are the gate. It exists because this spec's
		// whole chain runs in-cluster in tens of milliseconds, which makes a genuine
		// pass and a vacuous one indistinguishable from spec timings alone.
		Eventually(func(g Gomega) {
			AddReportEntry("Q528 fakegithub scale-set call log",
				fmt.Sprintf("%v", scaleSetState(g)["calls"]))
		}, 30*time.Second, time.Second).Should(Succeed())
	})

	It("E2E_AGC_ScaleSetSessionIsReleasedOnRunnerSetDelete: deleting the RunnerSet tears the message-queue session down", func() {
		By("deleting the RunnerSet")
		_, err := utils.Run(exec.Command("kubectl", "delete", "runnerset", setName,
			"-n", tenantNS, "--ignore-not-found", "--timeout=2m"))
		Expect(err).NotTo(HaveOccurred())

		By("asserting fakegithub no longer holds a session for the scale set")
		// A leaked session is not cosmetic: the backend allows one active session per
		// scale set, so a later listener for the same set is refused 409 and the tenant
		// silently stops acquiring.
		Eventually(func(g Gomega) {
			g.Expect(scaleSetState(g)["activeSession"]).To(BeFalse(),
				"the session outlived its RunnerSet; the next listener for this scale set would be refused")
		}, 2*time.Minute, 2*time.Second).Should(Succeed())
	})
})

// scaleSetJob mirrors the identity fakegithub's control API reports for an enqueued
// scale-set job (scalesetstub.Job) — the values its assignment will carry.
type scaleSetJob struct {
	RunnerRequestID int64  `json:"runnerRequestId"`
	JobID           string `json:"jobId"`
	OwnerName       string `json:"ownerName"`
	RepositoryName  string `json:"repositoryName"`
	WorkflowRunID   int64  `json:"workflowRunId"`
}

// scaleSetControl issues one request against the fakegithub control API through the
// spec's port-forward and returns the response body.
func scaleSetControl(method, path string) (string, error) {
	req, err := http.NewRequest(method, "http://localhost:"+fakegithubLocalPort+path, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return string(body), fmt.Errorf("%s %s: status %d: %s", method, path, resp.StatusCode, body)
	}
	return string(body), nil
}

// scaleSetEnqueue queues one job on the spec's scale set and returns the identity
// its assignment will carry.
func scaleSetEnqueue() scaleSetJob {
	GinkgoHelper()
	body, err := scaleSetControl(http.MethodPost, "/control/scaleset/enqueue?name=e2e-ssacq")
	Expect(err).NotTo(HaveOccurred(), "enqueue a scale-set job")
	var job scaleSetJob
	Expect(json.Unmarshal([]byte(body), &job)).To(Succeed())
	Expect(job.JobID).NotTo(BeEmpty())
	return job
}

// scaleSetState reads fakegithub's server-side view of the spec's scale set. It is
// polled inside Eventually blocks, so a transient port-forward hiccup fails the
// iteration rather than the spec.
func scaleSetState(g Gomega) map[string]any {
	body, err := scaleSetControl(http.MethodGet, "/control/scaleset/state?name=e2e-ssacq")
	g.Expect(err).NotTo(HaveOccurred(), "read scale-set state")
	var state map[string]any
	g.Expect(json.Unmarshal([]byte(body), &state)).To(Succeed())
	return state
}

// scaleSetWorkerPodSelector selects the RunnerSet's worker pods on the scale-set tier.
func scaleSetWorkerPodSelector(runnerSet string) string {
	return fmt.Sprintf(
		"actions-gateway.com/runner-set=%s,actions-gateway.com/acquisition-protocol=ScaleSet", runnerSet)
}

// firstScaleSetWorkerPod returns the name of one worker pod owned by the RunnerSet on
// the scale-set tier, or "" when none exists yet. The jsonpath ranges rather than
// indexing: an `.items[0]` on an empty list is a kubectl error, which would surface a
// pod that has not appeared yet as a tooling failure.
func firstScaleSetWorkerPod(g Gomega, ns, runnerSet string) string {
	out, err := utils.Run(exec.Command("kubectl", "get", "pods", "-n", ns,
		"-l", scaleSetWorkerPodSelector(runnerSet),
		"-o", `jsonpath={range .items[*]}{.metadata.name}{"\n"}{end}`))
	g.Expect(err).NotTo(HaveOccurred())
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

// scaleSetWorkerPodsWithRunID returns the worker pods provisioned for one workflow
// run — the way to tell a specific job's pod apart from a sibling job's.
func scaleSetWorkerPodsWithRunID(g Gomega, ns, runnerSet, runID string) []string {
	out, err := utils.Run(exec.Command("kubectl", "get", "pods", "-n", ns,
		"-l", scaleSetWorkerPodSelector(runnerSet),
		"-o", `jsonpath={range .items[?(@.metadata.annotations.actions-gateway\.com/run-id=="`+runID+`")]}{.metadata.name}{"\n"}{end}`))
	g.Expect(err).NotTo(HaveOccurred())
	var names []string
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			names = append(names, line)
		}
	}
	return names
}

// jitConfigSecretBlob returns the JIT config out of the Secret a worker pod actually
// mounts, or "" when that Secret does not exist. Reading the name off the pod rather
// than re-deriving provisioner.scaleSetSecretName keeps the assertion about the pod
// being able to start, not about the naming rule agreeing with a copy of itself.
func jitConfigSecretBlob(ns, pod string) string {
	GinkgoHelper()
	name, err := utils.Run(exec.Command("kubectl", "get", "pod", pod, "-n", ns,
		"-o", `jsonpath={.spec.volumes[?(@.name=="job-payload")].secret.secretName}`))
	Expect(err).NotTo(HaveOccurred())
	name = strings.TrimSpace(name)
	Expect(name).NotTo(BeEmpty(), "worker pod %s mounts no job-payload Secret", pod)

	out, err := utils.Run(exec.Command("kubectl", "get", "secret", name,
		"-n", ns, "--ignore-not-found", "-o", "jsonpath={.data.jitconfig}"))
	Expect(err).NotTo(HaveOccurred())
	return strings.TrimSpace(out)
}

// podLabel returns one label off a pod, or "" when it is absent.
func podLabel(ns, name, key string) string {
	GinkgoHelper()
	out, err := utils.Run(exec.Command("kubectl", "get", "pod", name,
		"-n", ns, "-o", fmt.Sprintf("jsonpath={.metadata.labels['%s']}", strings.ReplaceAll(key, ".", `\.`))))
	Expect(err).NotTo(HaveOccurred())
	return strings.TrimSpace(out)
}

// podContainerEnv returns one env var's literal value off a named container.
func podContainerEnv(ns, pod, container, envName string) string {
	GinkgoHelper()
	out, err := utils.Run(exec.Command("kubectl", "get", "pod", pod, "-n", ns,
		"-o", fmt.Sprintf(
			`jsonpath={.spec.containers[?(@.name=="%s")].env[?(@.name=="%s")].value}`, container, envName)))
	Expect(err).NotTo(HaveOccurred())
	return strings.TrimSpace(out)
}

// scaleSetAcquisitionManifest renders the tenant: one gateway, one template, one
// ScaleSet-protocol RunnerSet.
//
// The gateway's githubURL is the ordinary https placeholder every fixture tenant
// carries. It is NOT how the AGC reaches fakegithub here: the CRD pattern and the
// webhook pin the field to https, so the scale-set bootstrap is re-pointed at the
// stub by the STUB_AUTH_URL + STUB_BROKER_URL pair the suite already injects for the
// classic tier (cmd/agc/config.go, scaleSetStubBaseURL). The org path is still read
// off githubURL, so the REST prefix the client derives is a real one.
//
// The worker container's command is deliberately NOT overridden: the provisioner
// replaces it with the wrapper binary either way (provisioner/pod.go), so a template
// command would be silently discarded and read as if it were in effect. What the
// container then does is out of scope — the spec asserts the pod's shape and the
// staged JIT config, not the runner's execution.
func scaleSetAcquisitionManifest(ns, secretName, workerImage string) string {
	return fmt.Sprintf(`apiVersion: actions-gateway.com/v2alpha1
kind: ActionsGateway
metadata:
  name: ssacq
  namespace: %[1]s
spec:
  githubURL: https://github.com/ssacqorg
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
        resources:
          requests:
            cpu: 50m
            memory: 64Mi
          limits:
            cpu: 50m
            memory: 64Mi
---
apiVersion: actions-gateway.com/v2alpha1
kind: RunnerSet
metadata:
  name: set-ssacq
  namespace: %[1]s
spec:
  gatewayRef:
    name: ssacq
  templateRef:
    name: tmpl
  acquisitionProtocol: ScaleSet
  runnerLabels: ["e2e-ssacq"]
`, ns, secretName, workerImage)
}
