//go:build e2e
// +build e2e

package utils

import (
	"time"

	. "github.com/onsi/ginkgo/v2" //nolint:revive,staticcheck
	. "github.com/onsi/gomega"    //nolint:revive
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	agcv1alpha1 "github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	gmcv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/api/v1alpha1"
)

// This file holds the v1alpha1 tenant fixture the e2e suite applies. It replaces the
// four near-duplicate ApplyActionsGatewayCR* builders that preceded it (Q414), each of
// which carried its own copy of the same ~30-line YAML document and the same three
// explanatory comment blocks, and differed only in which RunnerGroup knobs it set.
// Adding a fifth for the DinD shape would have been the obvious move and the wrong
// one: the fixture is a typed struct with chainable presets, so a new tenant shape is
// a few fields rather than another copy of the whole document.
//
// The CR is built from the real API types and marshalled, not string-formatted, so a
// field renamed in cmd/gmc/api or cmd/agc/api breaks the compile instead of silently
// applying a CR with an ignored (pruned) field.

// e2eGitHubURL is the placeholder org URL every fixture tenant carries. gitHubURL is a
// required field, but the e2e AGC reaches fakegithub through the stub registrar
// (STUB_AUTH_URL/STUB_BROKER_URL win over GITHUB_ORG_URL), so this value is not used
// for registration in the default stub flow. The real-GitHub suite overrides it with
// AGC_EXTRA_GITHUB_ORG_URL.
const e2eGitHubURL = "https://github.com/e2e-org"

// NeverSchedulesNodeSelector pins every worker pod Pending by demanding a node label
// nothing carries. A Pending pod still counts toward maxWorkers (activePodCount counts
// Pending) and holds its reservation — the listener's job handler blocks in
// waitForCompletion until the pod reaches a terminal phase — so a spec can hold
// capacity busy deterministically and free it on demand by deleting the pod (the
// InformerPodWaiter treats deletion as completion). This is the seam the Q154
// worker-ceiling e2e drives the admission gate with, without depending on a runner
// image that actually runs.
var NeverSchedulesNodeSelector = map[string]string{"q154.actions-gateway/never-schedules": "true"}

// DinDSidecarImage is the Docker-in-Docker daemon image the DinD fixture runs. It
// matches the image the dogfood e2e tenant runs in production
// (deploy/dogfood-e2e/overlays/dind/resources.yaml), so the fixture migrates the same
// shape a real tenant would.
const DinDSidecarImage = "docker:27-dind"

// RunnerGroupFixture describes one v1 RunnerGroup inside a TenantFixture. The zero
// value is not useful on its own — build it through a TenantFixture preset and adjust.
type RunnerGroupFixture struct {
	// RunnerLabels are the runs-on labels this group matches.
	RunnerLabels []string
	// MaxListeners is the concurrency ceiling for the group's listener pool.
	MaxListeners int32
	// MaxWorkers, when non-zero, sets the worker pod-capacity ceiling the Q59
	// admission gate enforces. Zero omits the field (unbounded, the v1 default).
	MaxWorkers int32
	// WorkerImage is the runner image the AGC provisions worker pods from. It is also
	// the image of the fixture's `runner` container, so the two never disagree.
	WorkerImage string
	// CompletedPodTTL and PendingPodDeadline are the Q95 worker-pod lifecycle knobs.
	// Empty omits the field. Both are Kubernetes durations ("5m", "90s").
	CompletedPodTTL    string
	PendingPodDeadline string
	// NodeSelector is copied onto the worker pod template. Set it to
	// NeverSchedulesNodeSelector to hold every worker pod Pending.
	NodeSelector map[string]string
	// DinD adds a privileged Docker-in-Docker daemon as a NATIVE sidecar and points
	// the runner container at it. See withDinD for why the sidecar shape matters.
	DinD bool
}

// TenantFixture is a v1alpha1 tenant — one ActionsGateway with an inline proxy and
// zero or more inline runner groups — ready to apply into an e2e namespace.
type TenantFixture struct {
	Namespace  string
	Name       string
	SecretName string
	// SecurityProfile is the v1 per-gateway Pod Security Admission profile. Empty
	// means the baseline default. "privileged" additionally requires the namespace to
	// carry the platform eligibility grant — see PrivilegedTenantNamespaceLabels.
	SecurityProfile string
	// LogLevel defaults to debug for every fixture tenant. The session/job specs gate
	// a DumpAGCSessionDiagnostics dump on failure, but the listener's per-session
	// lifecycle trail — session start, job received, heal, single-use recycle, idle
	// shutdown — was demoted to Debug in the logging audit (Q87, Theme D). At info a
	// "no worker pod scheduled yet" timeout (Q134/Q148) dumps only AGC startup logs
	// and gives no hint why a job was never acquired. The extra volume is negligible
	// at these sizes and the dump is only consumed on failure (Q148).
	LogLevel string
	// ProxyMinReplicas/ProxyMaxReplicas size the inline egress proxy pool.
	ProxyMinReplicas int32
	ProxyMaxReplicas int32
	// NoProxyCIDRs are destinations workers reach directly rather than through the
	// proxy. Fixture tenants with a runner group include svc.cluster.local so the AGC
	// reaches the in-cluster fakegithub Service.
	NoProxyCIDRs []string
	RunnerGroups []RunnerGroupFixture
}

// BaseTenant is the minimal tenant: identity plus an inline proxy pool, no runner
// groups. It is what the provisioning, isolation, RBAC, HPA/PDB, teardown, security-
// profile, and resilience specs need — they assert on the GMC-provisioned control
// plane, which exists without any runner group.
func BaseTenant(ns, name, secretName string) TenantFixture {
	return TenantFixture{
		Namespace:        ns,
		Name:             name,
		SecretName:       secretName,
		LogLevel:         "debug",
		ProxyMinReplicas: 1,
		ProxyMaxReplicas: 3,
	}
}

// RunnerTenant is BaseTenant plus one minimal runner group, so the AGC has something
// to reconcile and can register broker sessions — the prerequisite for every
// job-lifecycle spec.
func RunnerTenant(ns, name, secretName, workerImage string) TenantFixture {
	f := BaseTenant(ns, name, secretName)
	f.NoProxyCIDRs = []string{"svc.cluster.local"}
	f.RunnerGroups = []RunnerGroupFixture{{
		RunnerLabels: []string{"e2e"},
		MaxListeners: 2,
		WorkerImage:  workerImage,
	}}
	return f
}

// DinDTenant is the representative Docker-in-Docker tenant (Q414): a runner container
// driving a privileged `docker:dind` daemon, exactly the shape the dogfood e2e tenant
// runs. It is the fixture that makes the v1→v2 migration path testable, because a
// privileged worker shape is the case that distinguishes the namespaced
// RunnerTemplate from the cluster-scoped ClusterRunnerTemplate on the v2 side.
//
// The gateway opts into securityProfile: privileged, which is the ONLY configuration
// v1's own admission webhook admits a privileged worker container under — and it
// additionally requires the namespace to carry the platform eligibility grant, so
// create the namespace with PrivilegedTenantNamespaceLabels().
func DinDTenant(ns, name, secretName, runnerImage string) TenantFixture {
	f := RunnerTenant(ns, name, secretName, runnerImage)
	f.SecurityProfile = "privileged"
	f.RunnerGroups[0].DinD = true
	return f
}

// PrivilegedTenantNamespaceLabels returns the namespace labels a privileged tenant
// needs, for passing to CreateNamespace. The privileged-profile grant is fail-closed
// and platform-applied: without it the GMC webhook rejects securityProfile:
// privileged, so a DinD tenant applied into an ungranted namespace never provisions.
func PrivilegedTenantNamespaceLabels() map[string]string {
	return map[string]string{
		gmcv1alpha1.PrivilegedProfileLabel: gmcv1alpha1.PrivilegedProfileAllowed,
	}
}

// WithLifecycle sets the Q95 worker-pod lifecycle knobs on every runner group, so a
// spec can prove reaping on a short clock instead of the production defaults. Both
// values are Kubernetes durations; an empty string leaves that knob unset.
func (f TenantFixture) WithLifecycle(completedPodTTL, pendingPodDeadline string) TenantFixture {
	f.RunnerGroups = cloneGroups(f.RunnerGroups)
	for i := range f.RunnerGroups {
		f.RunnerGroups[i].CompletedPodTTL = completedPodTTL
		f.RunnerGroups[i].PendingPodDeadline = pendingPodDeadline
	}
	return f
}

// WithWorkerCeiling sets maxWorkers on every runner group AND pins every worker pod
// Pending, which is what makes the ceiling observable: a Pending pod holds its slot
// until the test deletes it, so the admission gate can be driven to its full state
// without a runner image that actually runs. See NeverSchedulesNodeSelector.
func (f TenantFixture) WithWorkerCeiling(maxWorkers int32) TenantFixture {
	f.RunnerGroups = cloneGroups(f.RunnerGroups)
	for i := range f.RunnerGroups {
		f.RunnerGroups[i].MaxWorkers = maxWorkers
		f.RunnerGroups[i].NodeSelector = NeverSchedulesNodeSelector
	}
	return f
}

// cloneGroups copies the runner-group slice so a With* method returns a modified
// fixture without mutating the receiver's backing array — otherwise two fixtures
// derived from one preset would alias each other's groups.
func cloneGroups(in []RunnerGroupFixture) []RunnerGroupFixture {
	out := make([]RunnerGroupFixture, len(in))
	copy(out, in)
	return out
}

// Apply renders the fixture and applies it, failing the spec on error.
func (f TenantFixture) Apply() {
	GinkgoHelper()
	Expect(ApplyManifest(f.Manifest())).To(Succeed(),
		"apply ActionsGateway %s/%s", f.Namespace, f.Name)
}

// ApplyWithWebhookRetry applies the fixture through ApplyManifestWithWebhookRetry,
// riding out a transient GMC-webhook stall under the parallel suite (Q391). Use it in
// a BeforeAll whose apply triggers the validating webhook.
func (f TenantFixture) ApplyWithWebhookRetry() {
	GinkgoHelper()
	Expect(ApplyManifestWithWebhookRetry(f.Manifest())).To(Succeed(),
		"apply ActionsGateway %s/%s", f.Namespace, f.Name)
}

// Manifest renders the fixture to the YAML `kubectl apply` consumes. Exported so a
// spec can assert on the manifest, or apply it through a different path.
func (f TenantFixture) Manifest() string {
	GinkgoHelper()
	out, err := yaml.Marshal(f.Object())
	Expect(err).NotTo(HaveOccurred(), "marshal ActionsGateway %s/%s", f.Namespace, f.Name)
	return string(out)
}

// Object builds the typed v1alpha1 ActionsGateway the fixture describes.
func (f TenantFixture) Object() *gmcv1alpha1.ActionsGateway {
	groups := make([]agcv1alpha1.RunnerGroupSpec, 0, len(f.RunnerGroups))
	for _, g := range f.RunnerGroups {
		groups = append(groups, g.spec())
	}
	// Zero means "not set" for both proxy bounds, so leave the pointer nil and let the
	// CRD default apply rather than applying a literal 0 replicas.
	proxy := gmcv1alpha1.ProxyConfig{NoProxyCIDRs: f.NoProxyCIDRs}
	if f.ProxyMinReplicas > 0 {
		minReplicas := f.ProxyMinReplicas
		proxy.MinReplicas = &minReplicas
	}
	if f.ProxyMaxReplicas > 0 {
		maxReplicas := f.ProxyMaxReplicas
		proxy.MaxReplicas = &maxReplicas
	}
	return &gmcv1alpha1.ActionsGateway{
		TypeMeta: metav1.TypeMeta{
			APIVersion: gmcv1alpha1.GroupVersion.String(),
			Kind:       "ActionsGateway",
		},
		ObjectMeta: metav1.ObjectMeta{Name: f.Name, Namespace: f.Namespace},
		Spec: gmcv1alpha1.ActionsGatewaySpec{
			GitHubAppRef:    gmcv1alpha1.SecretReference{Name: f.SecretName},
			GitHubURL:       e2eGitHubURL,
			SecurityProfile: f.SecurityProfile,
			LogLevel:        f.LogLevel,
			Proxy:           proxy,
			RunnerGroups:    groups,
		},
	}
}

// spec builds the v1 RunnerGroupSpec for one fixture group.
func (g RunnerGroupFixture) spec() agcv1alpha1.RunnerGroupSpec {
	spec := agcv1alpha1.RunnerGroupSpec{
		RunnerLabels: g.RunnerLabels,
		MaxListeners: g.MaxListeners,
		WorkerImage:  g.WorkerImage,
		PodTemplate: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				NodeSelector: g.NodeSelector,
				Containers: []corev1.Container{{
					Name:  "runner",
					Image: g.WorkerImage,
				}},
			},
		},
	}
	if g.MaxWorkers > 0 {
		maxWorkers := g.MaxWorkers
		spec.MaxWorkers = &maxWorkers
	}
	spec.CompletedPodTTL = mustParseDuration(g.CompletedPodTTL)
	spec.PendingPodDeadline = mustParseDuration(g.PendingPodDeadline)
	if g.DinD {
		withDinD(&spec.PodTemplate.Spec)
	}
	return spec
}

// withDinD adds the Docker-in-Docker daemon to a worker pod shape and points the
// runner container at it.
//
// The daemon is a NATIVE sidecar — a restartPolicy: Always init container, Kubernetes
// >= 1.29 — not a regular container, and that is load-bearing rather than stylistic: a
// regular sidecar's dockerd never exits, so the pod never completes, the AGC keeps the
// session active, and the worker pool strands against maxWorkers (Q249). As a native
// sidecar, Kubernetes tears dockerd down when the runner container exits. The v2
// RunnerTemplate webhook emits a non-blocking admission warning for the regular-
// container form, so getting this wrong in a fixture would also make every migrated
// manifest carry a spurious warning.
//
// The daemon runs privileged, which is why a DinD tenant needs securityProfile:
// privileged and the platform eligibility grant on its namespace — and why, on the v2
// side, its worker shape can only live on a cluster-scoped ClusterRunnerTemplate.
func withDinD(pod *corev1.PodSpec) {
	privileged := true
	always := corev1.ContainerRestartPolicyAlways
	pod.InitContainers = append(pod.InitContainers, corev1.Container{
		Name:          "dind",
		Image:         DinDSidecarImage,
		RestartPolicy: &always,
		Args:          []string{"--host=tcp://0.0.0.0:2375", "--tls=false"},
		Env: []corev1.EnvVar{
			// Disable dockerd's automatic TLS: the daemon listens on plain TCP bound to
			// the pod's own network namespace, reachable only by the runner container.
			{Name: "DOCKER_TLS_CERTDIR", Value: ""},
		},
		SecurityContext: &corev1.SecurityContext{Privileged: &privileged},
		Resources: corev1.ResourceRequirements{
			// Deliberately small: an e2e fixture tenant never runs a real build. The
			// production sizing is measured per job class and lives in the dogfood
			// overlay (deploy/dogfood-e2e/overlays/dind, Q248), not here.
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			},
		},
	})
	for i := range pod.Containers {
		if pod.Containers[i].Name != "runner" {
			continue
		}
		pod.Containers[i].Env = append(pod.Containers[i].Env, corev1.EnvVar{
			Name: "DOCKER_HOST", Value: "tcp://localhost:2375",
		})
	}
}

// mustParseDuration converts a Kubernetes duration string to the pointer the v1 API
// uses, returning nil for the empty string so the field is omitted. An unparseable
// value fails the spec immediately rather than silently omitting the knob a test is
// about to assert on.
func mustParseDuration(s string) *metav1.Duration {
	GinkgoHelper()
	if s == "" {
		return nil
	}
	d, err := time.ParseDuration(s)
	Expect(err).NotTo(HaveOccurred(), "parse fixture duration %q", s)
	return &metav1.Duration{Duration: d}
}
