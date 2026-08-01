//go:build e2e
// +build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/actions-gateway/github-actions-gateway/gmc/test/utils"
)

// E2E_Migration_V1ToV2 covers `gag-migrate` on a live cluster (Q414). Until this, the
// migration tool had unit tests only: FanOut was proved to emit the right Go structs,
// and nothing proved the apiserver would ACCEPT them or that the GMC would reconcile
// the result into a working tenant. That gap hid a real defect — a privileged
// (Docker-in-Docker) tenant fanned out to a namespaced RunnerTemplate the v2 admission
// webhook rejects, so `--apply` failed after the EgressProxy was already created and
// left the namespace half-migrated.
//
// The tenant here is deliberately the DinD shape (utils.DinDTenant): a runner
// container driving a privileged `docker:dind` native sidecar, matching what the
// dogfood e2e tenant actually runs. It is the shape that exercises the
// RunnerTemplate-vs-ClusterRunnerTemplate split, and it is the tenant Q415 migrates
// for real on the dogfood cluster.
//
// Worker pods are never provisioned: no job is enqueued, so nothing pulls the DinD
// image or asks kind for a privileged container. The spec is about the fan-out, the
// admission verdict, and the reconcile — not about running a build.
var _ = Describe("E2E_Migration_V1ToV2", Ordered, func() {
	const (
		tenantNS   = "tenant-migrate-dind"
		agName     = "dind-tenant"
		secretName = "github-app-secret" //nolint:gosec // G101: the NAME of a Kubernetes Secret object, not a credential value.

		// v2 per-gateway derived names (§H.16 #1): the migrated gateway keeps the v1
		// name, its AGC control plane is <gateway>-agc, and the EgressProxy the fan-out
		// extracts from the inline proxy is <gateway>-egress (whose pool Deployment is
		// in turn <proxy>-proxy).
		migratedAGC   = agName + "-agc"
		migratedProxy = agName + "-egress"
		proxyDeploy   = migratedProxy + "-proxy"
	)

	// clusterTemplateSelector finds the cluster-scoped templates this migration
	// created. It is also how an operator finds them: they are the one migration
	// output namespace deletion does not reclaim, which is why the tool stamps the
	// provenance label at all.
	clusterTemplateSelector := fmt.Sprintf("%s=%s", v2alpha1.MigratedFromNamespaceLabel, tenantNS)

	BeforeAll(func() {
		By("creating a privileged-eligible tenant namespace")
		// The privileged-profile grant is platform-applied and fail-closed: without it
		// the GMC webhook rejects securityProfile: privileged and the v1 DinD tenant
		// never provisions at all.
		utils.CreateNamespace(tenantNS, utils.PrivilegedTenantNamespaceLabels())
		utils.CreateGitHubAppSecret(tenantNS, secretName, 12345, 67890, testRSAKeyPEM)
		utils.ApplyFakegithubEgressNetworkPolicy(tenantNS)

		By("applying the v1 DinD tenant")
		// Deliberately NOT pinned to one unautoscalable replica. That pin was the Q570
		// workaround for the collision Q582 fixed — the two pools shared a selector
		// label, so they repelled each other by hostname anti-affinity and a v1 pool
		// free to autoscale left the migrated pool nowhere to schedule. Keeping the pin
		// would also keep this spec blind to a regression of exactly that.
		utils.DinDTenant(tenantNS, agName, secretName, workerImage).
			ApplyWithWebhookRetry()

		By("waiting for the v1 AGC control plane to be ready")
		// Migrating a tenant that never came up would prove nothing: the point is that
		// a LIVE v1 tenant migrates, so establish it first.
		utils.WaitForDeploymentReady(tenantNS, agcName, 4*time.Minute)

		By("building gag-migrate")
		_, err := utils.Run(exec.Command("make", "build-migrate"))
		Expect(err).NotTo(HaveOccurred(), "build gag-migrate")
	})

	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			utils.DumpAGCSessionDiagnostics(tenantNS, agcName, infraNamespace, fakegithubServiceName)
		}
	})

	AfterAll(func() {
		// Delete the tenant CRs in dependency order, WAITING on each, before the
		// namespace: every agentpool-cleanup finalizer is cleared by an AGC that lives
		// in THIS namespace, so a bare namespace delete races those AGC pods' own
		// termination and a lost race wedges the namespace in Terminating (Q585).
		// Migration leaves a v1 and a v2 tenant side by side, so both drain here.
		//
		// The v2 pools go first and by --all: the RunnerSet inherits a content-hashed
		// name, and v2's gateway teardown deliberately does NOT delete RunnerSets (they
		// reference the gateway but are not owned by it), so nothing else would drain
		// them while the migrated AGC is still up to do it.
		_, _ = utils.Run(exec.Command("kubectl", "delete", "runnerset", "--all",
			"-n", tenantNS, "--ignore-not-found", "--timeout=2m"))
		// Then the v2 gateway, so the GMC tears the migrated AGC control plane down
		// while it can still reconcile. The EgressProxy carries no finalizer (§H.8) and
		// is reclaimed by the namespace cascade.
		_, _ = utils.Run(exec.Command("kubectl", "delete", "actionsgateways.actions-gateway.com", agName,
			"-n", tenantNS, "--ignore-not-found", "--timeout=2m"))
		// Then the v1 gateway, which coexisted with v2 through the migration. Its own
		// teardown deletes the v1 RunnerGroups and waits them out before removing the
		// v1 AGC, so the v1 pools need no separate delete.
		utils.DeleteActionsGatewayCR(tenantNS, agName)

		// Cluster-scoped objects are NOT garbage-collected by deleting the namespace —
		// the whole reason the tool stamps a provenance label. Reclaim them by that
		// label, exactly as the runbook tells an operator to. Last, once nothing in the
		// namespace can still reference them.
		_, _ = utils.Run(exec.Command("kubectl", "delete", "clusterrunnertemplate",
			"-l", clusterTemplateSelector, "--ignore-not-found"))
		_, _ = utils.Run(exec.Command("kubectl", "delete", "clusterrolebinding",
			"agc-clusterrunnertemplate-reader."+tenantNS+"."+agName, "--ignore-not-found"))
		utils.DeleteNamespace(tenantNS)
	})

	SetDefaultEventuallyTimeout(3 * time.Minute)
	SetDefaultEventuallyPollingInterval(2 * time.Second)

	It("E2E_Migration_DryRunEmitsTheV2ObjectSetAndWritesNothing", func() {
		By("running gag-migrate in its default dry-run mode")
		out, err := runMigrate(tenantNS)
		Expect(err).NotTo(HaveOccurred(), "gag-migrate dry-run failed:\n%s", out)

		By("verifying the fan-out covers every v2 kind the tenant decomposes into")
		for _, kind := range []string{
			"kind: ActionsGateway",
			"kind: EgressProxy",
			"kind: RunnerSet",
			// The headline: the privileged worker shape lands on the CLUSTER-scoped
			// kind. A namespaced RunnerTemplate carrying a privileged container is
			// exactly what the v2 webhook refuses.
			"kind: ClusterRunnerTemplate",
		} {
			Expect(out).To(ContainSubstring(kind), "dry-run output missing %q:\n%s", kind, out)
		}
		Expect(out).NotTo(ContainSubstring("kind: RunnerTemplate\n"),
			"a privileged shape must NOT be emitted as a namespaced RunnerTemplate:\n%s", out)

		By("verifying the operator is warned that a cluster-scoped object will be created")
		Expect(out).To(ContainSubstring("CLUSTER-SCOPED"),
			"dry-run must warn about the cluster-scoped object before --apply:\n%s", out)
		Expect(out).To(ContainSubstring(v2alpha1.MigratedFromNamespaceLabel),
			"the warning must name the label an operator finds the object by:\n%s", out)

		By("verifying the operator is warned that the namespace patch needs a downgrade opt-in")
		// Without this warning the operator only discovers the blocker when --apply
		// fails partway, after the cluster-scoped template is already created.
		Expect(out).To(ContainSubstring("will REJECT the namespace patch"),
			"dry-run must warn that namespace-security-profile-guard blocks the relocation:\n%s", out)
		Expect(out).To(ContainSubstring(v2alpha1.AllowProfileDowngradeAnnotation),
			"the warning must name the annotation that unblocks it:\n%s", out)

		By("verifying the dry-run wrote nothing")
		// Dry-run is the default precisely so review comes before mutation; a dry-run
		// that created objects would make the review meaningless.
		Expect(utils.ResourceExists("actionsgateways.actions-gateway.com", tenantNS, agName)).To(BeFalse(),
			"dry-run must not create the v2 ActionsGateway")
		Expect(utils.ResourceExists("egressproxies.actions-gateway.com", tenantNS, migratedProxy)).To(BeFalse(),
			"dry-run must not create the EgressProxy")
		Expect(clusterTemplateNames(clusterTemplateSelector)).To(BeEmpty(),
			"dry-run must not create a ClusterRunnerTemplate")
	})

	It("E2E_Migration_ApplyCreatesAnAdmissibleV2ObjectSet", func() {
		By("granting the profile-downgrade opt-in the dry-run asked for")
		// The operator step the previous spec's warning prescribes, performed here
		// verbatim. Relocating `privileged` onto a namespace that has never carried the
		// v2 security-profile label presents as baseline→privileged — a downgrade, since
		// privileged is the LEAST restrictive level — so namespace-security-profile-guard
		// refuses the patch without this annotation. The tool deliberately does not write
		// it: opting into a downgrade is the operator's decision, not the tool's.
		_, err := utils.Run(exec.Command("kubectl", "annotate", "namespace", tenantNS,
			v2alpha1.AllowProfileDowngradeAnnotation+"="+v2alpha1.AllowProfileDowngradeAllowed, "--overwrite"))
		Expect(err).NotTo(HaveOccurred(), "annotate namespace with the downgrade opt-in")

		By("running gag-migrate --apply")
		out, err := runMigrate(tenantNS, "--apply", "--assume-yes")
		Expect(err).NotTo(HaveOccurred(), "gag-migrate --apply failed:\n%s", out)

		By("verifying every emitted object was admitted")
		Expect(utils.ResourceExists("actionsgateways.actions-gateway.com", tenantNS, agName)).To(BeTrue())
		Expect(utils.ResourceExists("egressproxies.actions-gateway.com", tenantNS, migratedProxy)).To(BeTrue())

		// The RunnerSet inherits the name of the standalone RunnerGroup CR the GMC
		// materialized from the inline entry, which carries a content hash — so read
		// it rather than reconstructing it here and coupling this spec to the hash.
		setNames := resourceNames("runnersets.actions-gateway.com", tenantNS)
		Expect(setNames).To(HaveLen(1), "one v1 RunnerGroup fans out to exactly one RunnerSet")
		setName := setNames[0]

		names := clusterTemplateNames(clusterTemplateSelector)
		Expect(names).To(HaveLen(1), "exactly one ClusterRunnerTemplate for the one privileged group")
		Expect(names[0]).To(HavePrefix("crt-" + tenantNS + "-"))

		By("verifying the RunnerSet's templateRef names the cluster-scoped kind")
		// Without an explicit kind the referent defaults to a namespaced
		// RunnerTemplate, and the set would sit Ready=False/TemplateNotFound forever.
		// The version is qualified explicitly: templateRef.kind and maxListeners are
		// v2alpha1 fields the ScaleSet-only v2beta1 storage version strips, so an
		// unqualified read would not see them (cf. Q398).
		Expect(jsonpathValue("runnersets.v2alpha1.actions-gateway.com", setName, tenantNS,
			"{.spec.templateRef.kind}")).To(Equal("ClusterRunnerTemplate"))
		Expect(jsonpathValue("runnersets.v2alpha1.actions-gateway.com", setName, tenantNS,
			"{.spec.templateRef.name}")).To(Equal(names[0]), "templateRef resolves to the emitted template")

		By("verifying the namespace metadata patch relocated the security profile")
		labels := namespaceLabels(tenantNS)
		Expect(labels).To(HaveKeyWithValue(v2alpha1.SecurityProfileLabel, "privileged"),
			"v1 hung securityProfile on the gateway; v2 relocates it to the namespace")
		Expect(labels).To(HaveKeyWithValue(v2alpha1.PrivilegedProfileLabel, v2alpha1.PrivilegedProfileAllowed),
			"the platform's existing privileged grant is carried onto the v2 domain, never invented")
		Expect(labels).To(HaveKeyWithValue(v2alpha1.TenantNamespaceMarkerLabel, v2alpha1.TenantNamespaceMarkerValue))

		By("verifying v1 still runs beside v2 (coexistence, so rollback stays possible)")
		// The tool never deletes v1 objects — the runbook has the operator validate v2
		// first and decommission v1 afterwards.
		Expect(utils.ResourceExists("actionsgateways.actions-gateway.github.com", tenantNS, agName)).To(BeTrue(),
			"the v1 ActionsGateway must survive the migration")
		Expect(labels).To(HaveKeyWithValue("actions-gateway.github.com/tenant", "true"),
			"the v1 namespace marker must survive so v1 admission keeps passing")
	})

	// multi-node: alone among the three, this spec needs a second worker for the pool.
	It("E2E_Migration_MigratedTenantReconcilesIntoAWorkingControlPlane", Label("multi-node"), func() {
		By("waiting for the migrated gateway's own AGC Deployment to become ready")
		// The decisive assertion: the GMC did not merely admit the manifests, it
		// reconciled them into a running per-gateway control plane. A migration that
		// produces admissible-but-unreconcilable objects would pass every check above.
		utils.WaitForDeploymentReady(tenantNS, migratedAGC, 4*time.Minute)

		By("waiting for the extracted EgressProxy pool to become ready")
		// §H.17 invariant 1: the fan-out always emits a proxy and always wires
		// defaultProxyRef, so a migrated tenant never silently falls through to direct
		// egress and lose its per-tenant egress-IP attribution.
		utils.WaitForDeploymentReady(tenantNS, proxyDeploy, 4*time.Minute)
	})
})

// runMigrate invokes the built gag-migrate binary against the e2e cluster, pinning
// the kube context explicitly rather than relying on the ambient current-context (a
// parallel session can repoint it). Output is stdout+stderr combined, so a caller can
// assert on both the manifest stream and the warning block.
//
// Deliberately a ONE-SHOT invocation, with no ApplyManifestWithWebhookRetry-style
// wrapper around it (Q461). The `--apply` path creates v2 objects through the same
// validating webhooks that stall transiently under `--procs 6` (Q391), but the retry
// belongs INSIDE the binary — a real operator hits the same blip, and a mid-fan-out
// abort strands already-created objects. gag-migrate retries its own transient
// webhook failures per-object, so this spec inherits the fix; wrapping the whole
// binary here would instead re-run the confirmation and warnings on every attempt and
// turn a genuine admission denial into a long stall. If this spec starts flaking on a
// webhook transport error again, fix cmd/gmc/migrate, not this helper.
func runMigrate(namespace string, extraArgs ...string) (string, error) {
	args := append([]string{"--namespace", namespace, "--context", currentKubeContext()}, extraArgs...)
	// G204: the binary NAME is variable here, so this is outside the
	// constant-binary gosec exclusion in .golangci.yml and is accepted by hand.
	// migrateBinaryPath() joins <this file's compile-time dir>/../../../../.build/
	// gag-migrate — a fixed path with no external input — and args are test-local.
	return utils.Run(exec.Command(migrateBinaryPath(), args...)) //nolint:gosec // G204: binary path is derived from runtime.Caller, not from input.
}

// migrateBinaryPath resolves <repo-root>/.build/gag-migrate — where cmd/gmc's
// build-migrate target puts it — from this file's compile-time location, so it is
// correct regardless of the working directory the e2e binary runs from.
func migrateBinaryPath() string {
	_, thisFile, _, ok := runtime.Caller(0)
	Expect(ok).To(BeTrue(), "resolve caller for gag-migrate path")
	root := filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "..")
	return filepath.Join(root, ".build", "gag-migrate")
}

// currentKubeContext reads the kubeconfig current-context so gag-migrate can be
// pointed at it explicitly.
func currentKubeContext() string {
	out, err := utils.Run(exec.Command("kubectl", "config", "current-context"))
	Expect(err).NotTo(HaveOccurred(), "read kubeconfig current-context")
	return strings.TrimSpace(out)
}

// jsonpathValue reads a single jsonpath expression off a named resource.
//
// It strips apiserver deprecation warnings, which is not optional here: utils.Run
// merges stderr into stdout, and EVERY read of a `v2alpha1`-qualified object emits
// `Warning: actions-gateway.com/v2alpha1 … is deprecated` on stderr (v2alpha1 is
// served-but-deprecated until v2.0.0). Without stripping, that line is concatenated
// onto the value and every comparison fails on a string that merely looks right.
func jsonpathValue(resource, name, ns, jsonpath string) string {
	GinkgoHelper()
	out, err := utils.Run(exec.Command("kubectl", "get", resource, name, "-n", ns, "-o", "jsonpath="+jsonpath))
	Expect(err).NotTo(HaveOccurred(), "read %s on %s/%s", jsonpath, ns, name)
	return stripAPIServerWarnings(out)
}

// stripAPIServerWarnings drops the `Warning:` lines kubectl writes to stderr — which
// utils.Run merges into its returned output — and trims the remainder.
func stripAPIServerWarnings(s string) string {
	var kept []string
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "Warning:") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

// clusterTemplateNames lists the ClusterRunnerTemplates matching a label selector.
func clusterTemplateNames(selector string) []string {
	out, err := utils.Run(exec.Command("kubectl", "get", "clusterrunnertemplates.actions-gateway.com",
		"-l", selector, "-o", `jsonpath={range .items[*]}{.metadata.name}{"\n"}{end}`))
	Expect(err).NotTo(HaveOccurred(), "list ClusterRunnerTemplates")
	return utils.GetNonEmptyLines(stripAPIServerWarnings(out))
}

// resourceNames lists the names of a namespaced resource.
func resourceNames(resource, ns string) []string {
	out, err := utils.Run(exec.Command("kubectl", "get", resource, "-n", ns,
		"-o", `jsonpath={range .items[*]}{.metadata.name}{"\n"}{end}`))
	Expect(err).NotTo(HaveOccurred(), "list %s in %s", resource, ns)
	return utils.GetNonEmptyLines(stripAPIServerWarnings(out))
}

// namespaceLabels reads a namespace's labels as a map, for asserting on the additive
// metadata patch the migration applies.
func namespaceLabels(ns string) map[string]string {
	out, err := utils.Run(exec.Command("kubectl", "get", "namespace", ns, "-o", "jsonpath={.metadata.labels}"))
	Expect(err).NotTo(HaveOccurred(), "read labels on namespace %s", ns)
	labels := map[string]string{}
	Expect(json.Unmarshal([]byte(stripAPIServerWarnings(out)), &labels)).To(Succeed(),
		"decode labels on namespace %s: %q", ns, out)
	return labels
}
