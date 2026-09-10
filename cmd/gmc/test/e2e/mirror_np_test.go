//go:build e2e
// +build e2e

package e2e

import (
	"fmt"
	"os/exec"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/actions-gateway/github-actions-gateway/gmc/test/utils"
)

const (
	// mirrorNamespace and mirrorNPName are the shipped names — deploy/registry-mirror.
	mirrorNamespace = "gag-registry-mirror"
	mirrorNPName    = "registry-mirror-worker-access"

	// mirrorTenantNS is the tenant namespace the BASE's worker-side policy names in
	// its metadata.namespace, so the render will not apply without it. Created empty
	// and left unmarked: nothing probes from it, and marking it would give the
	// positive below a second namespace it could have been admitted from.
	mirrorTenantNS = "gag-dogfood-e2e"

	// The probe client namespaces. mirrorMarkedNS carries the v2 managed-tenant
	// marker the shared ingress peer selects on; mirrorUnmarkedNS carries only the
	// v1-domain marker utils.CreateNamespace stamps, which that peer matches on
	// nothing (the component's own "V1-DOMAIN TENANTS ARE NOT ADMITTED" note).
	//
	// NEITHER IS gag-dogfood-e2e, and that is the control. The base's ingress admits
	// that one namespace literally, so a run in which the shared component silently
	// failed to compose would deny the positive below rather than pass it.
	mirrorMarkedNS   = "gag-np-mirror-marked"
	mirrorUnmarkedNS = "gag-np-mirror-unmarked"

	// The instance kept at one replica; the other four are scaled to zero. One
	// running pod is all `podSelector: app=registry-mirror` needs, and the probes
	// address this instance's Service.
	mirrorLiveDeploy = "mirror-docker-io"
)

// mirrorScaledDown are the four instances the lane does not need. Nothing probes
// them and each costs a pod on a kind node running the whole suite six-way.
var mirrorScaledDown = []string{
	"mirror-ghcr-io", "mirror-quay-io", "mirror-registry-k8s-io", "mirror-gcr-io",
}

// E2E_Mirror_SharedTenantsNP grades the registry mirror's SHARED-topology ingress
// peer against live traffic (Q1039).
//
// The peer is one `from` element with two selectors, which ANDs them: a namespace
// carrying `actions-gateway.com/tenant: managed` AND a pod carrying
// `actions-gateway/component: workload` (Q1026). Both halves ship enforced on
// operator clusters and neither had ever been driven: check-registry-mirror-render.sh
// (Q1024) renders the overlay, and a render proves the YAML composes — it cannot
// show that the peers admitted match the pods that connect. That failure is
// fail-closed and total: a client the peer misses black-holes every pull it makes.
//
// The three specs are the positive and the two halves of the AND. Together they
// also close the fail-OPEN direction, which is the one invisible in a green suite:
// a policy whose podSelector stopped matching the mirror pods selects nothing and
// admits everyone, so both negatives would go red.
//
// Calico-only, via the BeforeAll skip. kindnet accepts NetworkPolicy objects and
// does not reliably drop the traffic (Q7b/Q119), and that holds for ingress as
// well as egress — manager_np_test.go gates its metrics-ingress specs on the same
// read for the same reason. Keeping the apply in the suite rather than in
// e2e-reusable.yml is what leaves the kindnet lane paying nothing for it.
//
// WHAT THIS DOES NOT GRADE. The four wired clients (dockerd, the ref-rewrite
// `docker pull`, helm's OCI client, buildkit) are Kata-job wiring and the probe
// here is a curl pod, so the lane grades the peer EXPRESSION and not that a real
// worker's source address resolves to its workload-labelled pod under GKE
// Dataplane V2 across the Kata bridge. That reading is
// scripts/dogfood/e2e-mirror-clients.sh (Q1048).
// ContinueOnFailure: the two negatives read different halves of one AND, so the
// default Ordered behaviour -- stop after the first failure -- loses the second
// reading exactly when the first says the peer is wrong. Measured on the Q1039
// inversion run: with the peer's podSelector dropped, DeniesUnlabeledPod failed
// and DeniesUnmarkedNamespace reported `skipped 0s`, which reads like a pass in
// any count over the report and is not one. With the decorator it reports
// `passed 23.0s`.
//
// This buys a second reading; it does NOT make either negative self-sufficient.
// A mirror that is not serving fails the positive and lets both negatives pass
// on CURL_RC=7/HTTP_CODE=000 for the wrong reason, so the three results are read
// together or not at all -- the positive is the serving check, which is why its
// own failure message says the negatives below would be vacuous. The run is red
// either way. BeforeAll failures still skip every spec, correctly: there is no
// cluster state to probe.
var _ = Describe("E2E_Mirror_SharedTenantsNP", Ordered, ContinueOnFailure, func() {
	// /v2/ is answered by the registry's own API layer, which is why it is the
	// readiness probe. That is deliberate: what grades the peer is whether a
	// connection completes an HTTP exchange on 5000, and an upstream manifest fetch
	// would add the Docker Hub dependency this lane mirrors images to avoid while
	// grading the mirror's upstream path — topology-independent, and already graded
	// on dogfood by e2e-mirror-validate.sh.
	mirrorURL := fmt.Sprintf("http://%s.%s.svc.cluster.local:5000/v2/",
		mirrorLiveDeploy, mirrorNamespace)

	// The anti-vacuity gate for the two negatives, run in the SAME pod ahead of the
	// blocked leg. Calico programs a new pod's endpoint after the container can
	// already run, and a CI-side window has outlasted a 10 s connect-timeout
	// (Q1015), so a bare negative can report a drop that is the window rather than
	// the policy. Reaching the apiserver first disqualifies that: the probe
	// namespaces carry no egress policy, so this is admitted from any endpoint that
	// is programmed at all.
	gateURL := "--insecure https://kubernetes.default.svc.cluster.local:443/version"

	BeforeAll(func() {
		if !egressEnforcingCNI() {
			Skip("cluster CNI does not enforce NetworkPolicy (kindnet); recreate with `make e2e-cluster KIND_CNI=calico` (Q7b/Q119)")
		}

		By("creating the tenant namespace the base's worker-side policy is written into")
		utils.CreateNamespace(mirrorTenantNS, nil)
		DeferCleanup(func() { utils.DeleteNamespace(mirrorTenantNS) })

		By("creating the marked and unmarked probe client namespaces")
		utils.CreateNamespace(mirrorMarkedNS, map[string]string{
			"actions-gateway.com/tenant": "managed",
		})
		DeferCleanup(func() { utils.DeleteNamespace(mirrorMarkedNS) })
		utils.CreateNamespace(mirrorUnmarkedNS, nil)
		DeferCleanup(func() { utils.DeleteNamespace(mirrorUnmarkedNS) })

		By("applying the shipped shared-tenants overlay (images re-pointed at the lane's local registry)")
		projectDir, err := utils.GetProjectDir()
		Expect(err).NotTo(HaveOccurred(), "resolve project dir")
		overlay := projectDir + "/test/e2e/testdata/registry-mirror-shared"
		out, err := utils.Run(exec.Command("kubectl", "apply", "-k", overlay))
		Expect(err).NotTo(HaveOccurred(), "apply the shared-tenants overlay; output:\n%s", out)
		DeferCleanup(func() {
			_, _ = utils.Run(exec.Command("kubectl", "delete", "-k", overlay,
				"--ignore-not-found", "--wait=false"))
		})

		By("scaling the four instances nothing probes to zero")
		for _, d := range mirrorScaledDown {
			_, err := utils.Run(exec.Command("kubectl", "scale", "deployment", d,
				"-n", mirrorNamespace, "--replicas=0"))
			Expect(err).NotTo(HaveOccurred(), "scale %s to zero", d)
		}

		By("waiting for the one remaining mirror instance to become Available")
		utils.WaitForDeploymentReady(mirrorNamespace, mirrorLiveDeploy, 4*time.Minute)

		// The policy selecting nothing is fail-OPEN, so read that it selects the pod
		// that is actually running rather than trusting the render. The two negatives
		// below are the behavioural half of the same check; this one names the cause
		// when they go red.
		By("verifying the shared ingress policy exists and selects the running mirror pod")
		Expect(utils.ResourceExists("networkpolicy", mirrorNamespace, mirrorNPName)).To(BeTrue(),
			"the shared overlay applied but %s/%s is absent", mirrorNamespace, mirrorNPName)
		pods, err := utils.Run(exec.Command("kubectl", "get", "pods",
			"-n", mirrorNamespace, "-l", "app=registry-mirror",
			"-o", "jsonpath={.items[*].metadata.name}"))
		Expect(err).NotTo(HaveOccurred(), "list mirror pods by the policy's podSelector")
		Expect(pods).NotTo(BeEmpty(),
			"no pod carries app=registry-mirror, so %s selects nothing and admits everyone (fail-open)", mirrorNPName)
	})

	// Two of the three specs assert a drop, so a spurious allow is a claim about the
	// enforcer as much as the policy (Q747, #1417).
	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			utils.DumpProvisioningDiagnostics(mirrorNamespace, mirrorLiveDeploy,
				mirrorMarkedNS, mirrorUnmarkedNS)
			utils.DumpCNIEnforcerState()
		}
	})

	It("E2E_Mirror_SharedTenantsNP_AdmitsMarkedNamespaceWorkloadPod: a workload pod in a marked namespace reaches the mirror on 5000", func() {
		// Gate and assertion share the URL: the gate is the "poll until the allow is
		// live in the dataplane" loop Q159 added for the metrics positive, folded into
		// the asserting pod so a freshly scheduled source never re-races Calico's
		// per-endpoint programming. A peer that never admits exhausts the budget and
		// reports GATE_RC≠0, so it cannot pass vacuously.
		By("positive: a workload-labelled pod in the marked namespace completes an HTTP exchange with the mirror")
		logs := runGatedEgressProbe(mirrorMarkedNS, "mirror-np-admitted", true, mirrorURL, mirrorURL)
		Expect(logs).To(ContainSubstring("GATE_RC=0"),
			"the shared ingress peer never admitted a workload-labelled pod in a marked namespace within the gate budget; "+
				"if the component failed to compose, the base's single-namespace peer is in force and denies this by design; logs:\n%s", logs)
		Expect(logs).To(MatchRegexp(`CURL_RC=0(\s|$)`),
			"workload-labelled pod in a marked namespace could not reach the mirror on 5000; logs:\n%s", logs)
		Expect(logs).To(ContainSubstring("HTTP_CODE=200"),
			"the connection was admitted but /v2/ did not answer 200 — the mirror is not serving, so the negatives below would be vacuous; logs:\n%s", logs)
	})

	It("E2E_Mirror_SharedTenantsNP_DeniesUnlabeledPod: an unlabelled pod in a marked namespace is blocked", func() {
		By("negative: an unlabelled pod in the marked namespace cannot reach the mirror on 5000")
		logs := runGatedEgressProbe(mirrorMarkedNS, "mirror-np-unlabeled", false, gateURL, mirrorURL)
		Expect(logs).To(ContainSubstring("GATE_RC=0"),
			"the probe pod could not reach the apiserver either, so its endpoint was never programmed and the drop below says nothing (Q1015); logs:\n%s", logs)
		Expect(logs).To(MatchRegexp(`CURL_RC=(7|28)(\s|$)`),
			"an UNLABELLED pod in a marked namespace was not blocked — the peer's podSelector half of the AND is not in force (Q1026); logs:\n%s", logs)
		Expect(logs).To(ContainSubstring("HTTP_CODE=000"),
			"an unlabelled pod completed an HTTP exchange with the mirror; logs:\n%s", logs)
	})

	It("E2E_Mirror_SharedTenantsNP_DeniesUnmarkedNamespace: a workload pod in an unmarked namespace is blocked", func() {
		By("negative: a workload-labelled pod in the unmarked namespace cannot reach the mirror on 5000")
		logs := runGatedEgressProbe(mirrorUnmarkedNS, "mirror-np-unmarked", true, gateURL, mirrorURL)
		Expect(logs).To(ContainSubstring("GATE_RC=0"),
			"the probe pod could not reach the apiserver either, so its endpoint was never programmed and the drop below says nothing (Q1015); logs:\n%s", logs)
		Expect(logs).To(MatchRegexp(`CURL_RC=(7|28)(\s|$)`),
			"a workload-labelled pod in an UNMARKED namespace was not blocked — the peer's namespaceSelector half of the AND is not in force; logs:\n%s", logs)
		Expect(logs).To(ContainSubstring("HTTP_CODE=000"),
			"a pod in an unmarked namespace completed an HTTP exchange with the mirror; logs:\n%s", logs)
	})
})
