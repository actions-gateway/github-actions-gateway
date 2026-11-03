//go:build e2e
// +build e2e

package utils

import (
	"cmp"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2" //nolint:revive,staticcheck
	. "github.com/onsi/gomega"    //nolint:revive
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
)

// This file holds the v2 tenant fixture the e2e suite applies: a typed struct with
// chainable presets, so a new tenant shape is a few fields rather than another copy
// of the whole document (Q414).
//
// The objects are built from the real API types and marshalled, not string-formatted,
// so a field renamed in api/v2alpha1 breaks the compile instead of silently applying
// a CR with an ignored (pruned) field.

// FakeGitHubURL is the githubURL every fixture tenant carries unless it sets its own:
// the in-cluster fakegithub Service, over https at the port it serves plaintext on.
// The AGC re-points a gateway at the stub only when githubURL already names the host
// STUB_BROKER_URL does, swapping in the plaintext scheme the CRD forbids writing, so
// any other host would send the scale-set listener to that host instead. The e2e
// suite deploys fakegithub as "fakegithub" in "e2e-infra" on 8080. Every fixture
// tenant shares the org, which is why each set's scale-set name is derived from its
// own name (see ScaleSetName).
const FakeGitHubURL = "https://fakegithub.e2e-infra.svc.cluster.local:8080/e2e-org"

// NeverSchedulesNodeSelector pins every worker pod Pending by demanding a node label
// nothing carries. A Pending pod still counts toward maxWorkers (activePodCount counts
// Pending), so a spec can hold capacity busy deterministically and free it on demand by
// deleting the pod. This is the seam the worker-ceiling e2e drives the admission gate
// with, without depending on a runner image that actually runs.
var NeverSchedulesNodeSelector = map[string]string{"q154.actions-gateway/never-schedules": "true"}

// RunnerSetFixture describes one RunnerSet, and the RunnerTemplate it binds, inside a
// TenantFixture. The zero value is not useful on its own — build it through a
// TenantFixture preset and adjust.
type RunnerSetFixture struct {
	// RunnerLabels are the runs-on labels this set matches after its scale-set name.
	// The first one also names the set (see TenantFixture.RunnerSetName).
	RunnerLabels []string
	// MaxWorkers, when non-zero, sets the worker pod-capacity ceiling the Q59
	// admission gate enforces. Zero omits the field (unbounded).
	MaxWorkers int32
	// WorkerImage is the runner image the AGC provisions worker pods from. It is also
	// the image of the template's `runner` container, so the two never disagree.
	WorkerImage string
	// CompletedPodTTL and PendingPodDeadline are the Q95 worker-pod lifecycle knobs.
	// Empty omits the field. Both are Kubernetes durations ("5m", "90s").
	CompletedPodTTL    string
	PendingPodDeadline string
	// NodeSelector is copied onto the worker pod template. Set it to
	// NeverSchedulesNodeSelector to hold every worker pod Pending.
	NodeSelector map[string]string
	// PriorityTiers assigns a PriorityClass to the worker pod by cumulative active-pod
	// count. Every name here must be on the platform PriorityClass allowlist or the GMC
	// webhook (and the priorityclass-allowlist-guard policy) rejects the RunnerSet.
	PriorityTiers []v2alpha1.PriorityTier
	// WorkerResources, when non-nil, is set on the template's `runner` container. The
	// AGC copies the podTemplate verbatim, so this is how a spec gives the worker pod a
	// resource footprint the scheduler has to reason about — an extended resource, for
	// instance, whose exhaustion makes a preemption deterministic.
	WorkerResources *corev1.ResourceRequirements
}

// TenantFixture is a v2 tenant — one ActionsGateway, the EgressProxy it defaults to,
// and zero or more RunnerSets each bound to its own RunnerTemplate — ready to apply
// into an e2e namespace.
type TenantFixture struct {
	Namespace  string
	Name       string
	SecretName string
	// GitHubURL is the gateway's githubURL. Empty means FakeGitHubURL; the live-GitHub
	// suite sets the real org or repo.
	GitHubURL string
	// LogLevel defaults to debug for every fixture tenant, on the gateway and the
	// proxy pool alike. The session/job specs gate a DumpAGCSessionDiagnostics dump on
	// failure, but the listener's per-session lifecycle trail — session start, job
	// received, heal, single-use recycle, idle shutdown — was demoted to Debug in the
	// logging audit (Q87, Theme D). At info a "no worker pod scheduled yet" timeout
	// (Q134/Q148) dumps only AGC startup logs and gives no hint why a job was never
	// acquired. The extra volume is negligible at these sizes and the dump is only
	// consumed on failure (Q148).
	LogLevel string
	// ProxyMinReplicas/ProxyMaxReplicas size the EgressProxy pool.
	ProxyMinReplicas int32
	ProxyMaxReplicas int32
	// NoProxyCIDRs are destinations workers reach directly rather than through the
	// proxy. The GMC always appends svc.cluster.local, which is how the AGC reaches the
	// in-cluster fakegithub Service; naming it here would be rejected, because it
	// matches the gateway's GitHub host and so would route GitHub around the proxy.
	NoProxyCIDRs []string
	RunnerSets   []RunnerSetFixture
}

// BaseTenant is the minimal tenant: a gateway and its proxy pool, no runner sets. It
// is what the provisioning, isolation, RBAC, HPA/PDB, teardown, security-profile,
// and resilience specs need — they assert on the GMC-provisioned control plane,
// which exists without any runner set.
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

// RunnerTenant is BaseTenant plus one minimal runner set, so the AGC has something to
// reconcile and opens a scale-set session at fakegithub — the prerequisite for every
// job-lifecycle spec.
func RunnerTenant(ns, name, secretName, workerImage string) TenantFixture {
	f := BaseTenant(ns, name, secretName)
	f.RunnerSets = []RunnerSetFixture{{
		RunnerLabels: []string{"e2e"},
		WorkerImage:  workerImage,
	}}
	return f
}

// PrivilegedTenantNamespaceLabels returns the namespace labels a tenant running the
// privileged security profile needs, for passing to CreateNamespace. v2 keeps the
// profile on the namespace rather than the gateway, and the privileged-profile grant
// is fail-closed and platform-applied: without it the namespace-security-profile-guard
// policy rejects the privileged profile label.
func PrivilegedTenantNamespaceLabels() map[string]string {
	return map[string]string{
		v2alpha1.PrivilegedProfileLabel: v2alpha1.PrivilegedProfileAllowed,
		v2alpha1.SecurityProfileLabel:   v2alpha1.SecurityProfilePrivileged,
	}
}

// AGCName is the gateway's AGC Deployment, ServiceAccount, and RoleBinding name.
func (f TenantFixture) AGCName() string { return f.Name + "-agc" }

// WorkloadPolicyName is the gateway's workload NetworkPolicy name.
func (f TenantFixture) WorkloadPolicyName() string { return f.Name + "-workload" }

// WorkerServiceAccountName is the ServiceAccount the AGC runs this gateway's worker
// pods as.
func (f TenantFixture) WorkerServiceAccountName() string { return f.Name + "-worker" }

// EgressProxyName is the EgressProxy object the gateway defaults to.
func (f TenantFixture) EgressProxyName() string { return f.Name + "-egress" }

// ProxyName is the name the EgressProxy's Deployment, Service, HPA, PDB, and
// NetworkPolicy share.
func (f TenantFixture) ProxyName() string { return f.EgressProxyName() + "-proxy" }

// ProxyPodSelector selects the EgressProxy pool's pods.
func (f TenantFixture) ProxyPodSelector() string {
	return "actions-gateway.com/egress-proxy=" + f.EgressProxyName()
}

// AGCPodSelector selects the gateway's AGC pods.
func (f TenantFixture) AGCPodSelector() string { return "app=" + f.AGCName() }

// RunnerSetName is the name of the i'th RunnerSet and of the RunnerTemplate it binds:
// "<gateway>-<first runner label>".
func (f TenantFixture) RunnerSetName(i int) string {
	return f.Name + "-" + f.RunnerSets[i].RunnerLabels[0]
}

// ScaleSetName is the name the i'th set registers its scale set under at fakegithub:
// its first runnerLabel, which the fixture sets to the RunnerSet's own name. Every
// fixture tenant shares one org URL and fakegithub is shared across parallel specs, so
// a label common to all of them would collide at admission and at fakegithub alike.
func (f TenantFixture) ScaleSetName(i int) string { return f.RunnerSetName(i) }

// WithLifecycle sets the Q95 worker-pod lifecycle knobs on every runner set, so a
// spec can prove reaping on a short clock instead of the production defaults. Both
// values are Kubernetes durations; an empty string leaves that knob unset.
func (f TenantFixture) WithLifecycle(completedPodTTL, pendingPodDeadline string) TenantFixture {
	f.RunnerSets = cloneSets(f.RunnerSets)
	for i := range f.RunnerSets {
		f.RunnerSets[i].CompletedPodTTL = completedPodTTL
		f.RunnerSets[i].PendingPodDeadline = pendingPodDeadline
	}
	return f
}

// WithWorkerCeiling sets maxWorkers on every runner set AND pins every worker pod
// Pending, which is what makes the ceiling observable: a Pending pod holds its slot
// until the test deletes it, so the admission gate can be driven to its full state
// without a runner image that actually runs. See NeverSchedulesNodeSelector.
func (f TenantFixture) WithWorkerCeiling(maxWorkers int32) TenantFixture {
	f.RunnerSets = cloneSets(f.RunnerSets)
	for i := range f.RunnerSets {
		f.RunnerSets[i].MaxWorkers = maxWorkers
		f.RunnerSets[i].NodeSelector = NeverSchedulesNodeSelector
	}
	return f
}

// WithProxyReplicas pins the proxy pool's replica range. Proxy pods carry required
// podAntiAffinity on kubernetes.io/hostname, so a replica costs a whole worker node;
// passing min == max stops the pool's HPA from claiming a second node on a startup
// CPU burst, which on the 2-worker e2e cluster is capacity another pool may need.
func (f TenantFixture) WithProxyReplicas(minReplicas, maxReplicas int32) TenantFixture {
	f.ProxyMinReplicas = minReplicas
	f.ProxyMaxReplicas = maxReplicas
	return f
}

// WithPriorityTier puts every worker pod in one PriorityClass tier, and gives the
// runner container the resource footprint the spec needs the scheduler to see. The two
// travel together because either alone cannot produce a preemption: the class decides
// who loses, and the footprint is what makes the node contended enough for anyone to.
//
// threshold is the cumulative active-pod count at which the tier is exhausted, so a
// threshold of 1 admits exactly one worker at this class. maxWorkers is deliberately
// left unset — the CRD requires it to equal the last tier's threshold when both are
// set, and the tier already carries the ceiling.
func (f TenantFixture) WithPriorityTier(priorityClassName string, threshold int32, resources corev1.ResourceRequirements) TenantFixture {
	f.RunnerSets = cloneSets(f.RunnerSets)
	for i := range f.RunnerSets {
		f.RunnerSets[i].PriorityTiers = []v2alpha1.PriorityTier{{
			PriorityClassName: priorityClassName,
			Threshold:         threshold,
		}}
		f.RunnerSets[i].WorkerResources = resources.DeepCopy()
	}
	return f
}

// WithNodePressureHeadroom gives every runner container the memory footprint the
// node-pressure eviction lever needs (Q1177): a request well below what the spec will
// make the worker use, and a limit well above it.
//
// Both halves are load-bearing. The kubelet's node-pressure ranking puts the pods using
// more than they requested first, ordered by how far over they are, so a low request is
// what makes this worker the victim rather than whatever else shares its node. The limit
// has to clear the spec's fill with room for the runner itself, or the container is
// OOMKilled at its own limit — an own-limit failure the eviction arm rightly declines,
// and exactly the shape this lever exists to avoid.
//
// It is the eviction lever because a pod-level ephemeral-storage limit, the lever before
// it, evicts the pod for its own limit: the kubelet writes that as Failed/Evicted too,
// but recovery withholds it (Q1177), so it can no longer prove a re-run lands.
//
// CPU is declared because the provisioner's resource defaulting is gap-fill and
// all-or-nothing (applyResourceDefaults): a container that sets *any* resource keeps the
// tenant's values verbatim and gets no defaults. The CPU value mirrors
// provisioner.DefaultWorkerResources, which cannot be imported from this module.
//
// Rides on WorkerResources rather than adding a field of its own: that is already the
// fixture's one way to give the runner container a footprint, and two mechanisms
// writing the same container's Resources would silently race on which one wins.
func (f TenantFixture) WithNodePressureHeadroom() TenantFixture {
	f.RunnerSets = cloneSets(f.RunnerSets)
	for i := range f.RunnerSets {
		f.RunnerSets[i].WorkerResources = NodePressureWorkerResources()
	}
	return f
}

// NodePressureWorkerResources is WithNodePressureHeadroom's footprint, exported for
// the specs that render their own RunnerTemplate.
func NodePressureWorkerResources() *corev1.ResourceRequirements {
	return &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("2Gi"),
		},
	}
}

// cloneSets copies the runner-set slice so a With* method returns a modified fixture
// without mutating the receiver's backing array — otherwise two fixtures derived from
// one preset would alias each other's sets.
func cloneSets(in []RunnerSetFixture) []RunnerSetFixture {
	out := make([]RunnerSetFixture, len(in))
	copy(out, in)
	return out
}

// ApplyWithWebhookRetry renders the fixture and applies it through
// ApplyManifestWithWebhookRetry, failing the spec on error. It is the ONLY fixture
// apply on purpose (Q392): every object in it goes through a GMC validating webhook,
// so every apply can hit the transient webhook-unreachable stall the parallel suite
// provokes (Q391) — there is no fixture apply that should skip the retry. A genuine
// webhook denial still fails on the first attempt. A spec that deliberately wants a
// one-shot apply — to assert on the admission outcome itself, the way
// manager_np_test.go does — should call ApplyManifest/ApplyManifestOutput with
// Manifest() directly rather than reintroduce a non-retrying method here.
func (f TenantFixture) ApplyWithWebhookRetry() {
	GinkgoHelper()
	Expect(ApplyManifestWithWebhookRetry(f.Manifest())).To(Succeed(),
		"apply tenant %s/%s", f.Namespace, f.Name)
}

// Manifest renders the fixture to the multi-document YAML `kubectl apply` consumes.
// Exported so a spec can assert on the manifest, or apply it through a different path.
func (f TenantFixture) Manifest() string {
	GinkgoHelper()
	docs := make([]string, 0, 2+2*len(f.RunnerSets))
	for _, obj := range f.Objects() {
		out, err := yaml.Marshal(obj)
		Expect(err).NotTo(HaveOccurred(), "marshal tenant %s/%s", f.Namespace, f.Name)
		docs = append(docs, string(out))
	}
	return strings.Join(docs, "---\n")
}

// Objects builds the typed v2alpha1 objects the fixture describes, in apply order:
// the EgressProxy, the ActionsGateway, then each RunnerTemplate and its RunnerSet.
func (f TenantFixture) Objects() []any {
	objs := []any{f.EgressProxy(), f.Gateway()}
	for i := range f.RunnerSets {
		objs = append(objs, f.runnerTemplate(i), f.runnerSet(i))
	}
	return objs
}

// Gateway builds the fixture's ActionsGateway, defaulting every set to its proxy pool.
func (f TenantFixture) Gateway() *v2alpha1.ActionsGateway {
	return &v2alpha1.ActionsGateway{
		TypeMeta:   metav1.TypeMeta{APIVersion: v2alpha1.GroupVersion.String(), Kind: "ActionsGateway"},
		ObjectMeta: metav1.ObjectMeta{Name: f.Name, Namespace: f.Namespace},
		Spec: v2alpha1.ActionsGatewaySpec{
			Credentials: v2alpha1.GitHubCredentials{
				Type:      v2alpha1.CredentialTypeGitHubApp,
				GitHubApp: &v2alpha1.LocalSecretReference{Name: f.SecretName},
			},
			GitHubURL:       cmp.Or(f.GitHubURL, FakeGitHubURL),
			DefaultProxyRef: &v2alpha1.ProxyObjectRef{Name: f.EgressProxyName()},
			LogLevel:        f.LogLevel,
		},
	}
}

// EgressProxy builds the fixture's proxy pool. Zero means "not set" for both replica
// bounds, so the pointer stays nil and the CRD default applies rather than a literal
// 0 replicas.
func (f TenantFixture) EgressProxy() *v2alpha1.EgressProxy {
	ep := &v2alpha1.EgressProxy{
		TypeMeta:   metav1.TypeMeta{APIVersion: v2alpha1.GroupVersion.String(), Kind: "EgressProxy"},
		ObjectMeta: metav1.ObjectMeta{Name: f.EgressProxyName(), Namespace: f.Namespace},
		Spec: v2alpha1.EgressProxySpec{
			NoProxyCIDRs: f.NoProxyCIDRs,
			LogLevel:     f.LogLevel,
		},
	}
	if f.ProxyMinReplicas > 0 {
		minReplicas := f.ProxyMinReplicas
		ep.Spec.MinReplicas = &minReplicas
	}
	if f.ProxyMaxReplicas > 0 {
		maxReplicas := f.ProxyMaxReplicas
		ep.Spec.MaxReplicas = &maxReplicas
	}
	return ep
}

// runnerTemplate builds the RunnerTemplate the i'th set binds.
func (f TenantFixture) runnerTemplate(i int) *v2alpha1.RunnerTemplate {
	s := f.RunnerSets[i]
	pod := corev1.PodSpec{
		NodeSelector: s.NodeSelector,
		Containers:   []corev1.Container{{Name: "runner", Image: s.WorkerImage}},
	}
	if s.WorkerResources != nil {
		pod.Containers[0].Resources = *s.WorkerResources
	}
	return &v2alpha1.RunnerTemplate{
		TypeMeta:   metav1.TypeMeta{APIVersion: v2alpha1.GroupVersion.String(), Kind: "RunnerTemplate"},
		ObjectMeta: metav1.ObjectMeta{Name: f.RunnerSetName(i), Namespace: f.Namespace},
		Spec: v2alpha1.RunnerTemplateSpec{
			WorkerImage: s.WorkerImage,
			PodTemplate: corev1.PodTemplateSpec{Spec: pod},
		},
	}
}

// runnerSet builds the i'th RunnerSet. Its first runnerLabel is ScaleSetName, ahead of
// the fixture's own labels.
func (f TenantFixture) runnerSet(i int) *v2alpha1.RunnerSet {
	s := f.RunnerSets[i]
	rs := &v2alpha1.RunnerSet{
		TypeMeta:   metav1.TypeMeta{APIVersion: v2alpha1.GroupVersion.String(), Kind: "RunnerSet"},
		ObjectMeta: metav1.ObjectMeta{Name: f.RunnerSetName(i), Namespace: f.Namespace},
		Spec: v2alpha1.RunnerSetSpec{
			GatewayRef:         v2alpha1.ObjectRef{Name: f.Name},
			TemplateRef:        &v2alpha1.ObjectRef{Name: f.RunnerSetName(i)},
			RunnerLabels:       append([]string{f.ScaleSetName(i)}, s.RunnerLabels...),
			PriorityTiers:      s.PriorityTiers,
			CompletedPodTTL:    mustParseDuration(s.CompletedPodTTL),
			PendingPodDeadline: mustParseDuration(s.PendingPodDeadline),
		},
	}
	if s.MaxWorkers > 0 {
		maxWorkers := s.MaxWorkers
		rs.Spec.MaxWorkers = &maxWorkers
	}
	return rs
}

// mustParseDuration converts a Kubernetes duration string to the pointer the API
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
