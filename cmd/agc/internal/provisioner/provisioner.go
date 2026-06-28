// Package provisioner creates and manages ephemeral worker pods for acquired
// GitHub Actions jobs.
//
// The Provisioner replaces the M2 stubJobHandler: it stages a short-lived
// Kubernetes Secret containing the raw AcquireJob payload, creates an
// Ephemeral Worker Pod that mounts the Secret, watches for pod completion,
// and cleans up the Secret when the pod terminates. Both objects carry a
// controller OwnerReference to the RunnerGroup so CR/namespace deletion
// garbage-collects them; the pod itself is deleted on completion when the
// group's completedPodTTL is zero, and otherwise by the RunnerGroup
// reconciler's reaper once the TTL elapses (stuck-Pending pods are reaped
// after pendingPodDeadline by the same reaper).
//
// It enforces the concurrency ceilings from the RunnerGroup spec:
//   - priorityTiers: assign PriorityClass by cumulative pod count; hold if at last tier ceiling.
//   - maxWorkers: simple pod-count ceiling without PriorityClass.
package provisioner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"net/http"
	neturl "net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/listener"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/tracing"
	"github.com/actions-gateway/github-actions-gateway/agc/names"
	"github.com/actions-gateway/github-actions-gateway/api/apilabels"
	"github.com/actions-gateway/github-actions-gateway/githubapp/httpx"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// tracer is the OpenTelemetry tracer for the provisioner. It resolves to the
// global provider, which is the no-op provider unless main.go's tracing.Init
// installed an exporter — so these spans cost almost nothing when tracing is off.
var tracer = otel.Tracer(tracing.InstrumentationName)

// defaultProvisionerClient is the bounded fallback used when Provisioner.HTTPClient
// is nil. Shared so the nil path does not allocate a connection pool per call.
var defaultProvisionerClient = httpx.NewClient()

const (
	// LabelRunnerGroup is stamped on every worker pod (and job Secret) with the
	// owning RunnerGroup's name as its value. It is the contract the RunnerGroup
	// controller's Pod watch filters and maps on, so it is exported.
	LabelRunnerGroup = "actions-gateway/runner-group"
	labelPlanID      = "actions-gateway/plan-id"

	// workerAppName / workerComponent are the app.kubernetes.io/name and
	// app.kubernetes.io/component values for worker pods and their job Secrets.
	// The full recommended app.kubernetes.io/* set (incl. part-of, managed-by,
	// version) is stamped via apilabels — see buildPod / buildSecret — so k9s,
	// Prometheus relabel rules, and `kubectl get -l` work out of the box without
	// operators learning the project-specific keys.
	workerAppName   = "actions-runner"
	workerComponent = "runner"

	// Node-disruption-safety annotation keys stamped on worker pods so the
	// common cluster autoscalers and the descheduler do not evict a pod while
	// it is running a CI job (which would strand the job). A worker pod is a
	// Job-like, non-replicated unit of work: there is no replacement once it is
	// killed, so consolidation/scale-down/descheduling must leave it alone for
	// the (bounded) lifetime of the job. See applyDisruptionSafetyDefaults.
	//   - annoKarpenterDoNotDisrupt blocks Karpenter consolidation/drift
	//     disruption (karpenter.sh/do-not-disrupt: "true").
	//   - annoSafeToEvict tells the cluster-autoscaler the pod is not safe to
	//     evict on scale-down (cluster-autoscaler.kubernetes.io/safe-to-evict:
	//     "false").
	//   - annoDeschedulerPreferNoEviction is the current well-known descheduler
	//     opt-out (descheduler.alpha.kubernetes.io/prefer-no-eviction: "true").
	//     The older descheduler.alpha.kubernetes.io/evict key is opt-IN only —
	//     its value is ignored, so it cannot express "do not evict".
	annoKarpenterDoNotDisrupt       = "karpenter.sh/do-not-disrupt"
	annoSafeToEvict                 = "cluster-autoscaler.kubernetes.io/safe-to-evict"
	annoDeschedulerPreferNoEviction = "descheduler.alpha.kubernetes.io/prefer-no-eviction"

	// securityProfilePrivileged / securityProfileRestricted are the PSA
	// enforcement levels (mirrored from ActionsGateway.spec.securityProfile)
	// that gate how much of the secure-by-default worker SecurityContext
	// buildPod stamps. Any other value (including the empty string and the
	// "baseline" default) gets the baseline hardening set.
	securityProfilePrivileged = "privileged"
	securityProfileRestricted = "restricted"
	managerName               = names.ControllerName

	// DefaultWorkerImage is the fallback worker image when RunnerGroup.Spec.WorkerImage
	// is empty. It is digest-pinned (secure-by-default; a bare tag is mutable).
	//
	// Aligned with the Actions Runner Controller (ARC) gha-runner-scale-set chart,
	// which defaults to ghcr.io/actions/actions-runner. Tenants copy-pasting from
	// ARC examples see the same image name. Operators override at AGC startup via
	// --worker-image (env: WORKER_IMAGE); the per-RunnerGroup workerImage field
	// overrides further.
	//
	// Sourced from names.DefaultWorkerImage (built from names.RunnerVersion) so the
	// runner version stays locked to the agent.version the AGC registers and to the
	// FROM line in cmd/worker/Dockerfile — see the bump procedure in that file's
	// header comment and the lockstep test in cmd/agc/names/runner_version_test.go.
	DefaultWorkerImage = names.DefaultWorkerImage

	// defaultWorkerRunAsUser is the numeric UID applySecurityDefaults stamps
	// alongside runAsNonRoot:true on the baseline/restricted profiles. The
	// actions-runner image (DefaultWorkerImage, and the cmd/worker image built
	// from it) declares a NON-NUMERIC user (`USER runner`). kubelet's
	// runAsNonRoot enforcement can only PROVE a container is non-root against a
	// numeric UID — with only a username it rejects the pod at admission with
	// `CreateContainerConfigError: container has runAsNonRoot and image has
	// non-numeric user`. Pinning the runner's own UID (1001 — see the
	// `USER runner (UID 1001)` line in cmd/worker/Dockerfile and the upstream
	// actions/runner-images base) lets kubelet verify non-root without changing
	// which user the runner actually runs as. (Q115)
	defaultWorkerRunAsUser int64 = 1001

	payloadMountPath = "/run/secrets/job-payload"
	payloadKey       = "payload"
	planIDKey        = "plan-id"
	// jitConfigKey is the Secret data key that carries the agent's
	// encoded_jit_config blob to the worker wrapper. The wrapper base64-decodes
	// the value, JSON-unmarshals the file map, and writes each entry to
	// /home/runner/<filename> before exec'ing Runner.Worker. Absent or empty
	// values are tolerated by the wrapper for backwards compatibility with
	// stub-registrar agents.
	jitConfigKey    = "jitconfig"
	runnerContainer = "runner"

	// wrapperVolumeName / wrapperMountDir / wrapperInitName describe how the GAG
	// worker wrapper is injected into a worker pod (Q235) so the runner container
	// can be the unmodified upstream actions-runner image. The wrapper binary lands
	// at wrapperMountDir/wrapper either via a read-only OCI image volume (≥1.33) or
	// an initContainer that runs `wrapper install wrapperMountDir` into an emptyDir;
	// the runner container's command is overridden to that path.
	wrapperVolumeName = "gag-wrapper"
	wrapperMountDir   = "/opt/actions-gateway"
	wrapperBinName    = "wrapper"
	wrapperInitName   = "gag-wrapper-install"

	// proxyCAVolumeName / proxyCAMountPath / proxyCAFileName describe how the
	// per-tenant egress-proxy CA cert is projected into the worker pod. The
	// runner image's default OS trust store does not include the
	// cert-manager-issued self-signed CA that signs the proxy's TLS cert, so
	// Runner.Worker's outbound HTTPS calls through HTTPS_PROXY fail with
	// UntrustedRoot. The worker wrapper reads the cert from this path and
	// publishes it via SSL_CERT_FILE before exec'ing Runner.Worker. The path
	// matches the AGC's own mount in [cmd/gmc/internal/controller/builder.go]
	// (buildAGCDeployment) for symmetry.
	proxyCAVolumeName = "proxy-ca"
	proxyCAMountPath  = "/etc/actions-gateway/proxy-ca"
	proxyCAFileName   = "tls.crt"

	// DefaultCompletedPodTTL is the effective retention for worker pods in a
	// terminal phase when RunnerGroup.Spec.CompletedPodTTL is omitted. Long
	// enough for an operator to inspect a just-failed pod, short enough to keep
	// accumulation bounded at (job rate × TTL).
	DefaultCompletedPodTTL = 5 * time.Minute

	// DefaultPendingPodDeadline is the effective stuck-Pending deadline when
	// RunnerGroup.Spec.PendingPodDeadline is omitted. Generous for image pulls;
	// raise the field on clusters where legitimate scheduling is slow (e.g.
	// autoscaled GPU node pools).
	DefaultPendingPodDeadline = 10 * time.Minute
)

// EffectiveCompletedPodTTL returns the group's terminal-pod retention,
// applying DefaultCompletedPodTTL when the field is omitted.
func EffectiveCompletedPodTTL(rg *v1alpha1.RunnerGroup) time.Duration {
	return CompletedPodTTLOrDefault(rg.Spec.CompletedPodTTL)
}

// EffectivePendingPodDeadline returns the group's stuck-Pending deadline,
// applying DefaultPendingPodDeadline when the field is omitted.
func EffectivePendingPodDeadline(rg *v1alpha1.RunnerGroup) time.Duration {
	return PendingPodDeadlineOrDefault(rg.Spec.PendingPodDeadline)
}

// defaultWorkerCPU / defaultWorkerMemory are the resource requests *and*
// limits stamped on a worker container when the tenant PodTemplate omits them.
// Without these a worker pod is Best-Effort QoS: the first thing the kubelet
// evicts under node pressure, which burns the eviction-retry budget fast.
// Setting requests == limits makes a single-container worker pod Guaranteed QoS.
var (
	defaultWorkerCPU    = resource.MustParse("500m")
	defaultWorkerMemory = resource.MustParse("1Gi")
)

// Provisioner creates and manages worker pods for acquired GitHub Actions jobs.
type Provisioner struct {
	Client  client.Client
	Metrics *listener.Metrics
	// Events records owner-scoped Kubernetes Events for v1 RunnerGroup provisioning
	// incidents (quota/eviction-retry exhaustion), routed through the runnerGroupTarget
	// seam — the only Target the Provisioner itself constructs. The v2 RunnerSet path
	// carries its own recorder on runnerSetTarget (built by the RunnerSet reconciler),
	// since one Provisioner is shared across both owners. Nil disables event recording.
	Events             listener.EventRecorder
	Log                *slog.Logger
	MaxEvictionRetries int
	EvictionRetryDelay time.Duration
	MaxQuotaRetries    int
	QuotaRetryDelay    time.Duration
	PollInterval       time.Duration
	DefaultWorkerImage string

	// WrapperImage is the GAG worker-wrapper image (ghcr.io/actions-gateway/wrapper,
	// a ~2 MB scratch image holding just the wrapper binary). When non-empty, the
	// provisioner injects the wrapper into every worker pod and overrides the
	// runner container's command to run it — so the runner image can be the
	// unmodified upstream actions-runner (or any actions/runner-derived image)
	// rather than a baked-in wrapper image (Q235). Empty disables injection: the
	// worker image is then expected to carry the wrapper as its own entrypoint
	// (the pre-Q235 behaviour, kept for tests and the legacy worker image).
	WrapperImage string
	// UseImageVolume selects the wrapper delivery mechanism when WrapperImage is
	// set: true mounts WrapperImage as a read-only OCI image volume (K8s ≥ 1.33,
	// no init container — lowest latency); false copies the binary in via an
	// initContainer + emptyDir (works on any version). main.go resolves this from
	// the cluster version and the WRAPPER_DELIVERY override before constructing
	// the Provisioner.
	UseImageVolume bool
	// WorkerSA is the ServiceAccount name assigned to worker pods.
	WorkerSA string

	// SecurityProfile mirrors the tenant's ActionsGateway.spec.securityProfile
	// (baseline, restricted, or privileged), propagated from the GMC via the
	// SECURITY_PROFILE env var. It controls how much of the secure-by-default
	// worker SecurityContext buildPod stamps:
	//   - "" / "baseline": runAsNonRoot + seccomp RuntimeDefault (does not break
	//     in-job privilege escalation such as sudo);
	//   - "restricted": the full PSA-restricted container floor (also
	//     allowPrivilegeEscalation=false + drop ALL capabilities), required or
	//     else the namespace's PodSecurity admission rejects the pod;
	//   - "privileged": no SecurityContext defaults, so DinD / host-capability
	//     workloads can opt in via their PodTemplate.
	// Resource defaults are stamped on every profile.
	SecurityProfile string
	// HTTPProxy, HTTPSProxy, and NoProxy are injected into the runner container
	// env of every worker pod. Set from the AGC's own environment by main.go.
	HTTPProxy  string
	HTTPSProxy string
	NoProxy    string

	// ProxyTLSSecretName names a Secret in the tenant namespace whose tls.crt
	// key is the per-tenant egress-proxy CA certificate. When non-empty the
	// provisioner projects that key (cert only — never tls.key) into the
	// worker pod at proxyCAMountPath/proxyCAFileName so the worker entrypoint
	// wrapper can trust the proxy's TLS cert. Empty (the default) skips the
	// mount, which is the right behaviour for tests and any deployment that
	// runs without the per-tenant egress proxy.
	ProxyTLSSecretName string

	// Waiter blocks until a worker pod reaches a terminal phase. When set
	// (production wires an InformerPodWaiter via main.go), completion is
	// event-driven off the shared Pod informer. When nil, provision falls back
	// to polling Client every PollInterval — used by the fake-client unit tests,
	// which have no informer, and as a defensive fallback.
	Waiter PodWaiter

	// TokenFunc returns a valid GitHub App installation token for API calls.
	// If nil, eviction auto-retry is logged but the rerun API is not called.
	TokenFunc func(ctx context.Context) (string, error)

	// GitHubAPIURL is the base URL for the GitHub REST API.
	// Defaults to "https://api.github.com"; override in tests.
	GitHubAPIURL string

	// HTTPClient is used for GitHub API calls. nil uses a bounded
	// httpx.NewClient() (Q138) so a slow GitHub endpoint cannot wedge the caller.
	HTTPClient *http.Client

	// evictionCounts tracks per run_id eviction retry counts. The value carries
	// the running count plus the time of the last eviction touch, which the
	// background sweeper uses to reclaim entries for runs that can no longer be
	// evicted (Q141). Q106 made the count a hard lifetime cap (no delete on
	// exhaustion), so without the sweep one entry would leak per distinct evicted
	// run_id for the process lifetime.
	evictionCounts sync.Map // key: run_id (string) → value: evictionEntry

	// evictionLocks serializes the per-run check-and-increment of
	// evictionCounts so the budget is never exceeded under concurrency (Q106).
	// It is a fixed-size sharded lock keyed by a hash of run_id: bounded by
	// construction (no per-key map to grow or reap), while still letting
	// distinct runs evict concurrently. The zero value is ready to use.
	evictionLocks [evictionLockShards]sync.Mutex

	// now returns the current time. nil means time.Now; tests override it to
	// drive the eviction-counter TTL sweep deterministically.
	now func() time.Time

	// admission is the in-memory reservation counter that gates job acquisition
	// on worker capacity before AcquireJob claims the job from GitHub (Q59). Its
	// zero value is ready to use, so a struct-literal Provisioner (tests) gets a
	// working gate without explicit initialization. See admission.go.
	admission admissionGate
}

// evictionEntry is the value stored in evictionCounts. count is the number of
// reruns already reserved for the run (capped at maxRetries — Q106); lastUpdate
// is the time of the most recent eviction of the run, refreshed on every
// eviction whether or not a retry slot was granted. The sweeper reclaims an
// entry once lastUpdate is older than the TTL, by which point the run can no
// longer produce an evicted pod (see sweepEvictionCounts).
type evictionEntry struct {
	count      int
	lastUpdate time.Time
}

// evictionLockShards is the number of mutexes in the sharded eviction lock.
// Eviction is a rare, node-pressure-driven event, so a small fixed pool keeps
// contention between distinct run_ids negligible without unbounded growth.
const evictionLockShards = 64

const (
	// defaultEvictionCounterTTL bounds how long a per-run eviction-retry counter
	// is retained after the run's last eviction. It is chosen well beyond a
	// realistic GitHub Actions run lifetime: an entry is reclaimed only once its
	// run can no longer produce an evicted pod, because reclaiming a live run's
	// counter would reset it to zero and refill the retry budget — the Q106 bug.
	// (Q141)
	defaultEvictionCounterTTL = 24 * time.Hour
	// defaultEvictionSweepInterval is how often the background sweeper scans
	// evictionCounts for entries older than the TTL.
	defaultEvictionSweepInterval = time.Hour
)

// NewProvisioner creates a Provisioner with sensible defaults.
func NewProvisioner(c client.Client, m *listener.Metrics, log *slog.Logger) *Provisioner {
	return &Provisioner{
		Client:             c,
		Metrics:            m,
		Log:                log,
		MaxEvictionRetries: 2,
		EvictionRetryDelay: 5 * time.Second,
		MaxQuotaRetries:    5,
		QuotaRetryDelay:    30 * time.Second,
		PollInterval:       5 * time.Second,
		DefaultWorkerImage: DefaultWorkerImage,
	}
}

// HandlerFor returns a JobHandlerFunc bound to the given RunnerGroup, for the v1
// RunnerGroup controller. It wraps the RunnerGroup in the v1 Target adapter and
// delegates to Handle, so v1 and v2 share one provisioning path.
//
// The rg passed here is a snapshot captured when the listener started; the
// adapter re-reads the current RunnerGroup on every acquired job so podTemplate
// (and other spec) edits made after listener start take effect on the next job
// without an AGC restart (Q117).
func (p *Provisioner) HandlerFor(rg *v1alpha1.RunnerGroup) listener.JobHandlerFunc {
	return p.Handle(p.runnerGroupTarget(rg))
}

// Handle returns a JobHandlerFunc bound to the given Target, injected into
// listener.Config.JobHandler. On each acquired job it resolves the target's
// current provisioning spec and provisions a worker pod; a resolution failure
// (v2: a missing RunnerTemplate/EgressProxy) fails the job fail-closed without
// creating a pod. v1 wires it via HandlerFor; the v2 RunnerSet controller wires
// it directly with a RunnerSet-backed Target.
func (p *Provisioner) Handle(target Target) listener.JobHandlerFunc {
	return func(ctx context.Context, runServiceURL, planID string, payload []byte, jitConfig string) error {
		return p.provision(ctx, target, planID, payload, jitConfig)
	}
}

// acquirePayload extracts eviction-retry fields from the raw AcquireJob response.
// GitHub Actions embeds workflow context in the "variables" map as
// {"system.github.run_id": {"value":"12345"}, "system.github.repository": {"value":"owner/repo"}}.
type acquirePayload struct {
	RunID     int64                       `json:"run_id"` // top-level field; may be absent
	Variables map[string]variableEnvValue `json:"variables"`
}

type variableEnvValue struct {
	Value string `json:"value"`
}

// repoInfo extracts the owner, repo, and run ID from the parsed payload.
// Returns empty strings/zero if the fields are not present.
func (ap *acquirePayload) repoInfo() (owner, repo string, runID int64) {
	if ap.Variables != nil {
		if v, ok := ap.Variables["system.github.repository"]; ok {
			parts := strings.SplitN(v.Value, "/", 2)
			if len(parts) == 2 {
				owner, repo = parts[0], parts[1]
			}
		}
		if v, ok := ap.Variables["system.github.run_id"]; ok {
			// A malformed run_id leaves runID at 0, falling back to ap.RunID below.
			if _, err := fmt.Sscanf(v.Value, "%d", &runID); err != nil {
				runID = 0
			}
		}
	}
	if runID == 0 {
		runID = ap.RunID
	}
	return
}

// jobMeta holds GitHub Actions context extracted from the AcquireJob payload.
// All fields are best-effort: an absent or malformed payload leaves them empty,
// which is benign — the annotations are simply omitted from the pod.
type jobMeta struct {
	runID      string // numeric GitHub run ID, e.g. "12345678"
	repository string // "owner/repo"
	jobName    string // job name from workflow YAML, e.g. "build"
	workflow   string // workflow name, e.g. "CI"
}

// jobMetaFrom extracts the job annotation fields from a parsed AcquireJob payload.
func jobMetaFrom(ap acquirePayload) jobMeta {
	var m jobMeta
	if ap.Variables != nil {
		if v, ok := ap.Variables["system.github.run_id"]; ok {
			m.runID = v.Value
		}
		if v, ok := ap.Variables["system.github.repository"]; ok {
			m.repository = v.Value
		}
		if v, ok := ap.Variables["system.github.job"]; ok {
			m.jobName = v.Value
		}
		if v, ok := ap.Variables["system.github.workflow"]; ok {
			m.workflow = v.Value
		}
	}
	if m.runID == "" && ap.RunID != 0 {
		m.runID = fmt.Sprintf("%d", ap.RunID)
	}
	return m
}

// podAnnotations returns the actions-gateway.com/* annotations to stamp on
// worker pods. Only non-empty fields are included so pods created from
// minimal/stub payloads don't carry zero-value keys.
func (m jobMeta) podAnnotations() map[string]string {
	a := make(map[string]string, 4)
	if m.runID != "" {
		a["actions-gateway.com/run-id"] = m.runID
	}
	if m.repository != "" {
		a["actions-gateway.com/repository"] = m.repository
	}
	if m.jobName != "" {
		a["actions-gateway.com/job-name"] = m.jobName
	}
	if m.workflow != "" {
		a["actions-gateway.com/workflow"] = m.workflow
	}
	if len(a) == 0 {
		return nil
	}
	return a
}

// disruptionSafetyDefaults is the gap-fill set of node-disruption-safety
// annotations stamped on every worker pod. Each value is the marker the
// respective component honors to skip evicting a running pod.
var disruptionSafetyDefaults = map[string]string{
	annoKarpenterDoNotDisrupt:       "true",
	annoSafeToEvict:                 "false",
	annoDeschedulerPreferNoEviction: "true",
}

// applyDisruptionSafetyDefaults gap-fills the node-disruption-safety annotations
// (see disruptionSafetyDefaults) onto the worker pod annotation map, mirroring
// the secure-by-default SecurityContext gap-fill: a controller-managed default
// that a tenant can still override per key.
//
// A worker pod runs exactly one CI job and has no replica/controller behind it,
// so an autoscaler or descheduler that evicts it mid-job strands that job with
// no replacement. Stamping these markers makes the pod "production-relyable" on
// the clusters operators actually run (Karpenter, cluster-autoscaler,
// descheduler) without per-tenant configuration. The markers ride on the worker
// pod itself, so they vanish the moment the pod is torn down on job completion
// (immediately when completedPodTTL is 0, otherwise by the reaper) — they can
// never pin a dead pod.
//
// Overridable: a tenant who manages disruption another way (a PodDisruptionBudget,
// or a job they know is safe to interrupt) can set any of these keys to a
// different value in their PodTemplate metadata and that explicit value wins.
// Only these three keys are honored from the template; arbitrary template
// annotations are not copied onto the worker pod.
func applyDisruptionSafetyDefaults(dst, templateAnnotations map[string]string) map[string]string {
	if dst == nil {
		dst = make(map[string]string, len(disruptionSafetyDefaults))
	}
	for key, def := range disruptionSafetyDefaults {
		if v, ok := templateAnnotations[key]; ok {
			dst[key] = v // explicit tenant override wins
			continue
		}
		if _, ok := dst[key]; !ok {
			dst[key] = def
		}
	}
	return dst
}

func (p *Provisioner) provision(ctx context.Context, target Target, planID string, payload []byte, jitConfig string) (err error) {
	key := target.Key()
	log := p.logForKey(key)

	// Resolve the current provisioning spec for this job. For v1 this re-reads the
	// fresh RunnerGroup; for v2 it re-resolves the RunnerSet's RunnerTemplate and
	// EgressProxy. A resolution error fails the job fail-closed — no Secret, no pod
	// — so no worker wiring is ever created while a reference is unresolved (§H.7).
	spec, err := target.Resolve(ctx)
	if err != nil {
		log.Warn("provisioning spec unresolved; failing job without creating a pod", "error", err)
		return fmt.Errorf("provisioner: resolve provisioning spec: %w", err)
	}
	start := time.Now()

	safePlanID := safeName(planID)
	secretName := "job-" + safePlanID
	podName := fmt.Sprintf("runner-%s-%s", safeName(key.Name), safePlanID)
	// Keep pod names ≤63 chars (Kubernetes DNS label limit).
	if len(podName) > 63 {
		podName = podName[:63]
	}
	// Scope every line for this job to its worker pod (atop namespace/owner
	// from logForKey), so a session→job→pod trail is followable (Q87, Theme F).
	log = log.With("podName", podName)

	// Root span for the whole job-provisioning path. Child spans below break out
	// the latency of each phase (secret staging, pod-count, pod creation, and the
	// wait for completion — usually the long pole). The deferred closure stamps
	// the span's error status from the named return so any early exit is visible.
	ctx, span := tracer.Start(ctx, "Provisioner.provision", trace.WithAttributes(
		semconv.K8SNamespaceName(key.Namespace),
		attribute.String("gateway.owner.name", key.Name),
		attribute.String("gateway.plan.id", planID),
		semconv.K8SPodName(podName),
	))
	defer func() {
		if err != nil {
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()
	}()

	// Extract owner/repo/run_id for eviction retry (best-effort; missing is fine).
	// A malformed payload only degrades eviction-retry context, so we log and
	// continue rather than failing provisioning — but we no longer swallow the
	// error silently, since that hid genuine payload corruption.
	var ap acquirePayload
	if err := json.Unmarshal(payload, &ap); err != nil {
		log.Warn("could not parse AcquireJob payload for eviction-retry context; continuing without it", "error", err)
	}
	owner, repo, runIDInt := ap.repoInfo()
	runID := fmt.Sprintf("%d", runIDInt)
	meta := jobMetaFrom(ap)

	ownerLabels := target.PodOwnerLabels()
	workerVersion := imageVersion(p.resolveWorkerImage(spec))

	// 1. Stage the job Secret.
	if err = traceStep(ctx, "stageJobSecret", func(ctx context.Context) error {
		secret := p.buildSecret(target, secretName, planID, workerVersion, payload, jitConfig)
		return p.Client.Create(ctx, secret)
	}); err != nil {
		return fmt.Errorf("provisioner: create Secret %s: %w", secretName, err)
	}
	// One of three lines per provisioned pod; Debug to keep per-job volume down
	// at scale (Q87, Theme D).
	log.Debug("job Secret created", "secret", secretName)

	// 2. Count active pods for ceiling check.
	var count int32
	if err = traceStep(ctx, "countActivePods", func(ctx context.Context) error {
		var cErr error
		count, cErr = p.activePodCount(ctx, key.Namespace, ownerLabels)
		return cErr
	}); err != nil {
		_ = p.deleteSecret(ctx, key.Namespace, secretName)
		return fmt.Errorf("provisioner: count active pods: %w", err)
	}
	span.SetAttributes(attribute.Int("gateway.active_pods", int(count)))

	// 3. Ceiling enforcement.
	priorityClass, held := ceilingCheck(spec, count)
	span.SetAttributes(attribute.Bool("gateway.ceiling_held", held), attribute.String("gateway.priority_class", priorityClass))
	if held {
		log.Info("pod held by concurrency ceiling", "activePods", count)
		_ = p.deleteSecret(ctx, key.Namespace, secretName)
		err = fmt.Errorf("provisioner: concurrency ceiling reached (%d active pods)", count)
		return err
	}

	// 4. Build and create the pod (with quota retry).
	if err = traceStep(ctx, "createPod", func(ctx context.Context) error {
		pod := p.buildPod(target, spec, podName, secretName, priorityClass, meta)
		return p.createPodWithQuotaRetry(ctx, target, pod, spec.MaxQuotaRetries, spec.QuotaRetryDelay, log)
	}); err != nil {
		_ = p.deleteSecret(ctx, key.Namespace, secretName)
		return fmt.Errorf("provisioner: create Pod %s: %w", podName, err)
	}
	// Per-pod hot-path line; podName is on the logger context. Debug (Q87, Theme D).
	log.Debug("worker pod created", "priorityClass", priorityClass)

	// 5. Watch for pod completion (event-driven when a Waiter is wired; poll fallback otherwise).
	var phase corev1.PodPhase
	var reason string
	if err = traceStep(ctx, "waitForCompletion", func(ctx context.Context) error {
		var wErr error
		phase, reason, wErr = p.waitForCompletion(ctx, key.Namespace, podName)
		return wErr
	}); err != nil {
		// Context cancelled or unrecoverable watch error.
		_ = p.deleteSecret(ctx, key.Namespace, secretName)
		return err
	}

	duration := time.Since(start)
	span.SetAttributes(
		attribute.String("gateway.pod.phase", string(phase)),
		attribute.String("gateway.pod.reason", reason),
		attribute.Float64("gateway.provision.duration_seconds", duration.Seconds()),
	)
	// Per-pod completion line; podName is on the logger context. Debug (Q87, Theme D).
	log.Debug("worker pod completed", "phase", phase, "reason", reason, "duration", duration)
	if p.Metrics != nil {
		p.Metrics.JobDuration.WithLabelValues(key.Namespace, key.Name).Observe(duration.Seconds())
	}

	// 6. Eviction handling.
	if phase == corev1.PodFailed && reason == "Evicted" {
		p.handleEviction(ctx, target, owner, repo, runID, log, spec.MaxEvictionRetries, spec.EvictionRetryDelay)
	}

	// 7. Cleanup. The job Secret is always deleted here. The pod is deleted
	// immediately only when the owner's completedPodTTL is zero; otherwise the
	// owner's reconciler reaper deletes it once the TTL elapses — the reaper is
	// also the restart-safe backstop for pods no goroutine watches.
	if spec.CompletedPodTTL == 0 {
		_ = p.deletePod(ctx, key.Namespace, podName)
	}
	_ = p.deleteSecret(ctx, key.Namespace, secretName)
	return nil
}

// traceStep runs fn inside a child span named name (parented to the span carried
// by ctx), recording and stamping any error fn returns and always ending the
// span. It centralises the start/record/end boilerplate for the provision
// phases. When tracing is disabled the span is a no-op, so the only overhead is
// the closure call.
func traceStep(ctx context.Context, name string, fn func(context.Context) error) error {
	ctx, span := tracer.Start(ctx, name)
	defer span.End()
	if err := fn(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	return nil
}

// createPodWithQuotaRetry attempts to create pod, retrying up to maxRetries times
// when the namespace ResourceQuota is exhausted. Other errors are returned immediately.
// target is used to record an owner-scoped Event when the retry budget is exhausted.
func (p *Provisioner) createPodWithQuotaRetry(ctx context.Context, target Target, pod *corev1.Pod, maxRetries int, retryDelay time.Duration, log *slog.Logger) error {
	key := target.Key()
	for attempt := 0; ; attempt++ {
		err := p.Client.Create(ctx, pod)
		if err == nil {
			return nil
		}
		// Non-quota errors are never retried.
		if !isQuotaError(err) {
			return err
		}
		// maxRetries==0 means quota retry is disabled; return immediately without
		// counting as "exhausted" (disabled is a policy choice, not a budget failure).
		if maxRetries == 0 || attempt >= maxRetries {
			if maxRetries > 0 {
				log.Warn("quota retry budget exhausted; abandoning pod creation",
					"pod", pod.Name, "attempts", attempt+1)
				if p.Metrics != nil {
					p.Metrics.QuotaRetriesExhausted.WithLabelValues(key.Namespace, key.Name).Inc()
				}
				target.RecordEvent(corev1.EventTypeWarning, "QuotaRetriesExhausted", "ProvisionWorker",
					fmt.Sprintf("worker pod creation abandoned after exhausting the namespace ResourceQuota retry budget (%d retries); raise the namespace ResourceQuota or lower the worker concurrency ceiling", maxRetries))
			}
			return err
		}
		log.Info("pod creation blocked by namespace quota; retrying",
			"pod", pod.Name, "attempt", attempt+1, "maxRetries", maxRetries, "delay", retryDelay)
		if p.Metrics != nil {
			p.Metrics.QuotaRetries.WithLabelValues(key.Namespace, key.Name).Inc()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(retryDelay):
		}
	}
}

// isQuotaError reports whether err is a Kubernetes API error caused by a namespace
// ResourceQuota being exceeded. Quota errors are Forbidden (403) and their message
// contains "exceeded quota".
func isQuotaError(err error) bool {
	return apierrors.IsForbidden(err) && strings.Contains(err.Error(), "exceeded quota")
}

func (p *Provisioner) handleEviction(ctx context.Context, target Target, owner, repo, runID string, log *slog.Logger, maxRetries int, retryDelay time.Duration) {
	key := target.Key()
	if runID == "0" || runID == "" {
		log.Warn("pod evicted but run_id unknown; skipping auto-retry")
		return
	}

	// Reserve a retry slot atomically. This guards against the read-modify-write
	// race where two concurrent evictions of the same run both read the same
	// count, both pass the budget check, and both fire a rerun — exceeding
	// maxRetries (Q106). At most maxRetries evictions ever pass the gate, so the
	// rerun API is called at most maxRetries times per run.
	attempt, ok := p.reserveEvictionRetry(runID, maxRetries)
	if !ok {
		log.Warn("eviction retry budget exhausted; manual rerun required",
			"runID", runID, "maxRetries", maxRetries)
		if p.Metrics != nil {
			p.Metrics.EvictionRetriesExhausted.WithLabelValues(key.Namespace, key.Name).Inc()
		}
		target.RecordEvent(corev1.EventTypeWarning, "EvictionRetriesExhausted", "RetryEvictedJob",
			fmt.Sprintf("worker pod for run %s was evicted and the auto-retry budget (%d) is exhausted; a manual re-run is required", runID, maxRetries))
		return
	}

	log.Info("pod evicted; scheduling auto-retry", "runID", runID, "attempt", attempt)
	if p.Metrics != nil {
		p.Metrics.EvictionRetries.WithLabelValues(key.Namespace, key.Name).Inc()
	}

	// Brief delay before calling GitHub so any in-flight state settles.
	select {
	case <-ctx.Done():
		return
	case <-time.After(retryDelay):
	}

	if err := p.rerunFailedJobs(ctx, owner, repo, runID, log); err != nil {
		log.Error("eviction auto-retry failed; manual rerun may be required",
			"runID", runID, "error", err)
	} else {
		log.Info("eviction auto-retry triggered", "runID", runID, "attempt", attempt)
	}
}

// reserveEvictionRetry atomically checks the per-run eviction-retry budget and,
// if a slot remains, increments the count and returns the 1-based attempt
// number. It returns ok=false once the budget is exhausted. Serializing the
// check-and-increment per run_id — under the sharded evictionLocks — is what
// guarantees N concurrent evictions of the same run trigger at most maxRetries
// reruns (Q106). The lock is held only across the counter update, never across
// the retry delay or the GitHub API call.
func (p *Provisioner) reserveEvictionRetry(runID string, maxRetries int) (attempt int, ok bool) {
	mu := &p.evictionLocks[evictionShard(runID)]
	mu.Lock()
	defer mu.Unlock()

	now := p.nowFn()
	var count int
	if v, loaded := p.evictionCounts.Load(runID); loaded {
		count = v.(evictionEntry).count
	}
	if count >= maxRetries {
		// Budget is a hard lifetime cap: leave the count pinned so every later
		// eviction of this run is a no-op. We deliberately do NOT delete the
		// entry here — deleting reset the count to zero and let the budget refill
		// on the next eviction, which both defeats the cap and (combined with the
		// concurrent read-modify-write) is the Q106 over-budget bug. We DO refresh
		// lastUpdate: an exhausted but still-evicting run is provably live, so its
		// entry must not be a sweep candidate yet (Q141).
		p.evictionCounts.Store(runID, evictionEntry{count: count, lastUpdate: now})
		return 0, false
	}
	p.evictionCounts.Store(runID, evictionEntry{count: count + 1, lastUpdate: now})
	return count + 1, true
}

// nowFn returns the current time, honouring the test-injected p.now override.
func (p *Provisioner) nowFn() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// sweepEvictionCounts deletes per-run eviction-retry counters whose last
// eviction was more than ttl ago, returning the number of entries reclaimed.
//
// Correctness rests on a single fact: an evicted worker pod only ever exists for
// a live run, so a run that has produced no eviction for ttl can no longer
// produce one. With ttl chosen well beyond a realistic run lifetime, an entry is
// reclaimed only after its run is provably dead — so the LoadOrStore that a later
// eviction would do (refilling the budget to zero) can never happen for a run we
// swept. That preserves the Q106 invariant (at most maxRetries reruns per live
// run) while bounding evictionCounts to run_ids evicted within the trailing ttl
// window. The per-entry shard lock is taken (and lastUpdate re-checked under it)
// so a concurrent reserveEvictionRetry that just refreshed the entry is never
// raced away.
func (p *Provisioner) sweepEvictionCounts(ttl time.Duration) int {
	now := p.nowFn()
	var swept int
	p.evictionCounts.Range(func(key, value any) bool {
		if now.Sub(value.(evictionEntry).lastUpdate) <= ttl {
			return true
		}
		runID := key.(string)
		mu := &p.evictionLocks[evictionShard(runID)]
		mu.Lock()
		if v, loaded := p.evictionCounts.Load(runID); loaded &&
			now.Sub(v.(evictionEntry).lastUpdate) > ttl {
			p.evictionCounts.Delete(runID)
			swept++
		}
		mu.Unlock()
		return true
	})
	return swept
}

// EvictionSweeper periodically reclaims expired entries from a Provisioner's
// eviction-retry counter map. It implements sigs.k8s.io/controller-runtime/pkg/
// manager.Runnable; wire it with mgr.Add. Each AGC replica manages its own
// counters, so it runs on every replica (NeedLeaderElection is false).
type EvictionSweeper struct {
	p        *Provisioner
	interval time.Duration
	ttl      time.Duration
}

// NewEvictionSweeper returns an EvictionSweeper for p using the default sweep
// interval and counter TTL.
func NewEvictionSweeper(p *Provisioner) *EvictionSweeper {
	return &EvictionSweeper{
		p:        p,
		interval: defaultEvictionSweepInterval,
		ttl:      defaultEvictionCounterTTL,
	}
}

// Start runs the sweep loop until ctx is cancelled. It satisfies
// sigs.k8s.io/controller-runtime/pkg/manager.Runnable.
func (s *EvictionSweeper) Start(ctx context.Context) error {
	t := time.NewTicker(s.interval)
	defer t.Stop()
	log := s.p.logFor()
	log.Info("eviction-counter sweeper started", "interval", s.interval, "ttl", s.ttl)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if n := s.p.sweepEvictionCounts(s.ttl); n > 0 {
				log.Info("reclaimed expired eviction-retry counters", "count", n)
			}
		}
	}
}

// NeedLeaderElection reports that the sweeper runs on every replica, not only
// the leader: each AGC instance owns the eviction counters for the pods it
// provisioned.
func (s *EvictionSweeper) NeedLeaderElection() bool { return false }

// evictionShard maps a run_id to one of evictionLockShards mutex indices.
func evictionShard(runID string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(runID))
	return h.Sum32() % evictionLockShards
}

// rerunFailedJobs calls POST /repos/{owner}/{repo}/actions/runs/{run_id}/rerun-failed-jobs.
func (p *Provisioner) rerunFailedJobs(ctx context.Context, owner, repo, runID string, log *slog.Logger) error {
	if owner == "" || repo == "" {
		log.Warn("owner/repo unknown; cannot trigger rerun", "runID", runID)
		return nil
	}
	if !repoSegmentRE.MatchString(owner) || !repoSegmentRE.MatchString(repo) {
		return fmt.Errorf("invalid owner/repo characters: %q/%q", owner, repo)
	}
	if p.TokenFunc == nil {
		log.Warn("TokenFunc not configured; cannot trigger rerun", "runID", runID)
		return nil
	}

	token, err := p.TokenFunc(ctx)
	if err != nil {
		return fmt.Errorf("get installation token: %w", err)
	}

	apiBase := p.GitHubAPIURL
	if apiBase == "" {
		apiBase = "https://api.github.com"
	}
	apiURL := fmt.Sprintf("%s/repos/%s/%s/actions/runs/%s/rerun-failed-jobs",
		apiBase,
		neturl.PathEscape(owner),
		neturl.PathEscape(repo),
		neturl.PathEscape(runID))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader([]byte("{}")))
	if err != nil {
		return fmt.Errorf("build rerun request: %w", err)
	}
	req.Header.Set("Authorization", "token "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")

	hc := p.HTTPClient
	if hc == nil {
		hc = defaultProvisionerClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("rerun API call: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	// GitHub returns 201 Created on success.
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("rerun API returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// activePodCount returns the number of Running or Pending worker pods matching
// the owner's pod-identity label selector.
func (p *Provisioner) activePodCount(ctx context.Context, namespace string, selector map[string]string) (int32, error) {
	var podList corev1.PodList
	if err := p.Client.List(ctx, &podList,
		client.InNamespace(namespace),
		client.MatchingLabels(selector),
	); err != nil {
		return 0, err
	}
	var count int32
	for _, pod := range podList.Items {
		if pod.Status.Phase == corev1.PodRunning || pod.Status.Phase == corev1.PodPending {
			count++
		}
	}
	return count, nil
}

// ceilingCheck returns the PriorityClassName to assign (may be "") and whether
// the pod should be held due to a concurrency ceiling, from the resolved spec.
func ceilingCheck(spec *ResolvedSpec, activePods int32) (priorityClass string, held bool) {
	if len(spec.PriorityTiers) > 0 {
		for _, tier := range spec.PriorityTiers {
			if activePods < tier.Threshold {
				return tier.PriorityClassName, false
			}
		}
		// All tiers exhausted.
		return "", true
	}
	if spec.MaxWorkers != nil && activePods >= *spec.MaxWorkers {
		return "", true
	}
	return "", false
}

// resolveWorkerImage returns the worker image used for a provision: the
// per-RunnerGroup override, else the AGC --worker-image flag, else the
// digest-pinned built-in default. Shared by buildPod and the version-label
// computation so both agree on which image (hence which runner version) a pod runs.
func (p *Provisioner) resolveWorkerImage(spec *ResolvedSpec) string {
	if spec.WorkerImage != "" {
		return spec.WorkerImage
	}
	if p.DefaultWorkerImage != "" {
		return p.DefaultWorkerImage
	}
	return DefaultWorkerImage
}

// imageVersion extracts the tag from a container image reference for the
// app.kubernetes.io/version label. A digest-only or untagged reference has no
// version to report, so it falls back to names.RunnerVersion (the pinned default
// the project ships). Examples:
//
//	ghcr.io/actions/actions-runner:2.335.1@sha256:…  -> 2.335.1
//	ghcr.io/actions/actions-runner:2.335.1           -> 2.335.1
//	ghcr.io/actions/actions-runner@sha256:…          -> names.RunnerVersion
func imageVersion(image string) string {
	// Strip any digest first so an '@sha256:' colon is never mistaken for a tag.
	if at := strings.IndexByte(image, '@'); at >= 0 {
		image = image[:at]
	}
	// A tag follows the last ':' that comes after the last '/' — a registry port
	// (host:5000/repo) has its colon before the final path separator.
	if colon := strings.LastIndexByte(image, ':'); colon > strings.LastIndexByte(image, '/') {
		if tag := image[colon+1:]; tag != "" {
			return tag
		}
	}
	return names.RunnerVersion
}

func (p *Provisioner) buildSecret(target Target, name, planID, version string, payload []byte, jitConfig string) *corev1.Secret {
	data := map[string][]byte{
		payloadKey: payload,
		planIDKey:  []byte(planID),
	}
	if jitConfig != "" {
		data[jitConfigKey] = []byte(jitConfig)
	}
	// Recommended app.kubernetes.io/* metadata so the job Secret groups with the
	// worker pod it backs; the owner-identity label(s) the controller filters on
	// layer on top. managed-by is the AGC (managerName) — it, not the GMC, creates
	// these objects.
	labels := apilabels.Recommended(workerAppName, target.Key().Name, workerComponent, version, managerName)
	for k, v := range target.PodOwnerLabels() {
		labels[k] = v
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       target.Key().Namespace,
			Labels:          labels,
			OwnerReferences: []metav1.OwnerReference{target.OwnerRef()},
		},
		Data: data,
	}
}

func (p *Provisioner) buildPod(target Target, spec *ResolvedSpec, podName, secretName, priorityClass string, meta jobMeta) *corev1.Pod {
	// Start from the resolved PodTemplate.
	template := spec.PodTemplate.DeepCopy()

	workerImage := p.resolveWorkerImage(spec)

	// Ensure a container named "runner" exists.
	runnerIdx := -1
	for i, c := range template.Spec.Containers {
		if c.Name == runnerContainer {
			runnerIdx = i
			break
		}
	}
	if runnerIdx == -1 {
		template.Spec.Containers = append([]corev1.Container{{
			Name:  runnerContainer,
			Image: workerImage,
		}}, template.Spec.Containers...)
		runnerIdx = 0
	}

	// Inject Secret volume.
	volumeName := "job-payload"
	template.Spec.Volumes = append(template.Spec.Volumes, corev1.Volume{
		Name: volumeName,
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: secretName},
		},
	})

	// Mount into runner container and set env var.
	c := &template.Spec.Containers[runnerIdx]
	c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{
		Name:      volumeName,
		MountPath: payloadMountPath,
		ReadOnly:  true,
	})
	c.Env = append(c.Env, corev1.EnvVar{
		Name:  "PAYLOAD_SECRET_PATH",
		Value: payloadMountPath,
	})

	// Project the per-tenant egress-proxy CA cert into the runner container.
	// Cert only — Items restricts the projection to tls.crt so the private key
	// never reaches the worker pod. Mount mode 0o444 + the PodSpec FSGroup keep
	// the cert world-readable to the runner user (UID 1001 in the actions-runner
	// base image) without requiring write capability.
	if spec.ProxyTLSSecretName != "" {
		caMode := int32(0o444)
		template.Spec.Volumes = append(template.Spec.Volumes, corev1.Volume{
			Name: proxyCAVolumeName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: spec.ProxyTLSSecretName,
					Items: []corev1.KeyToPath{{
						Key:  corev1.TLSCertKey,
						Path: proxyCAFileName,
					}},
					DefaultMode: &caMode,
				},
			},
		})
		c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{
			Name:      proxyCAVolumeName,
			MountPath: proxyCAMountPath,
			ReadOnly:  true,
		})
	}

	// Inject proxy env vars into the runner container (controller-enforced invariants).
	// PROXY_CA_CERT_PATH tells the worker wrapper where to find the mounted CA;
	// empty when ProxyTLSSecretName is unset, in which case the wrapper skips
	// the trust-store install and HTTPS_PROXY traffic falls back to whatever
	// the base image already trusts.
	proxyCACertPath := ""
	if spec.ProxyTLSSecretName != "" {
		proxyCACertPath = proxyCAMountPath + "/" + proxyCAFileName
	}
	proxyEnvs := []corev1.EnvVar{
		{Name: "HTTP_PROXY", Value: spec.HTTPProxy},
		{Name: "HTTPS_PROXY", Value: spec.HTTPSProxy},
		{Name: "NO_PROXY", Value: spec.NoProxy},
		{Name: "PROXY_CA_CERT_PATH", Value: proxyCACertPath},
	}
	c.Env = mergeEnvOverride(c.Env, proxyEnvs)

	// Inject the GAG worker wrapper (Q235) so the runner image can be an
	// unmodified upstream actions-runner. No-op when WrapperImage is unset
	// (legacy: the worker image carries the wrapper as its own entrypoint). Must
	// run before applySecurityDefaults so the initContainer is hardened too.
	if p.WrapperImage != "" {
		p.injectWrapper(&template.Spec, runnerIdx)
	}

	// Overwrite reserved fields (controller-enforced invariants).
	sa := p.WorkerSA
	autoMount := false
	template.Spec.AutomountServiceAccountToken = &autoMount
	if sa != "" {
		template.Spec.ServiceAccountName = sa
	}
	hostFalse := false
	_ = hostFalse
	template.Spec.HostPID = false
	template.Spec.HostNetwork = false
	template.Spec.HostIPC = false
	template.Spec.RestartPolicy = corev1.RestartPolicyNever

	// Secure-by-default pod hardening. Both helpers gap-fill: an explicit value
	// in the tenant PodTemplate always wins, so a tenant can still opt out of
	// any individual default (e.g. runAsNonRoot:false for a root-based image).
	applySecurityDefaults(&template.Spec, spec.SecurityProfile)
	p.applyResourceDefaults(&template.Spec)

	key := target.Key()
	// Recommended app.kubernetes.io/* metadata for tooling interop. managed-by is
	// the AGC (managerName); version is the resolved runner image's version.
	labels := apilabels.Recommended(workerAppName, key.Name, workerComponent, imageVersion(workerImage), managerName)
	// Functional labels (additive to, never overwritten by, the recommended set):
	//   actions-gateway/component: workload — matches the workload NetworkPolicy
	//     podSelector so worker egress is restricted to the per-tenant proxy only.
	//   actions-gateway/plan-id — stable per-pod fragment for owner-scoped lookups.
	labels["actions-gateway/component"] = "workload"
	labels[labelPlanID] = safeName(podName)
	// Owner-identity label(s): LabelRunnerGroup for v1, LabelRunnerSet for v2 —
	// the key the owning controller's Pod watch and reaper filter on.
	for k, v := range target.PodOwnerLabels() {
		labels[k] = v
	}

	// Stamp node-disruption-safety defaults (gap-filled; a tenant PodTemplate
	// annotation for any of these keys wins) so consolidation/scale-down/
	// descheduling don't evict the pod mid-job and strand the CI run.
	annotations := applyDisruptionSafetyDefaults(meta.podAnnotations(), template.ObjectMeta.Annotations)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            podName,
			Namespace:       key.Namespace,
			Labels:          labels,
			Annotations:     annotations,
			OwnerReferences: []metav1.OwnerReference{target.OwnerRef()},
		},
		Spec: template.Spec,
	}

	if priorityClass != "" {
		pod.Spec.PriorityClassName = priorityClass
	}

	return pod
}

// injectWrapper delivers the GAG worker wrapper into the pod and points the
// runner container's command at it, so the runner image can be the unmodified
// upstream actions-runner (or any actions/runner-derived image) instead of a
// baked-in wrapper image (Q235). The binary is exposed at wrapperMountDir/wrapper
// either as a read-only OCI image volume (UseImageVolume; K8s ≥ 1.33, no init
// container) or copied into an emptyDir by an initContainer that self-installs
// from the wrapper image. Called from buildPod before applySecurityDefaults so
// the initContainer inherits the secure-by-default SecurityContext.
func (p *Provisioner) injectWrapper(spec *corev1.PodSpec, runnerIdx int) {
	if p.UseImageVolume {
		spec.Volumes = append(spec.Volumes, corev1.Volume{
			Name: wrapperVolumeName,
			VolumeSource: corev1.VolumeSource{
				Image: &corev1.ImageVolumeSource{
					Reference:  p.WrapperImage,
					PullPolicy: corev1.PullIfNotPresent,
				},
			},
		})
	} else {
		spec.Volumes = append(spec.Volumes, corev1.Volume{
			Name:         wrapperVolumeName,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		})
		// The scratch wrapper image has no shell/cp, so the binary copies itself
		// into the shared emptyDir via its `install` subcommand.
		spec.InitContainers = append(spec.InitContainers, corev1.Container{
			Name:    wrapperInitName,
			Image:   p.WrapperImage,
			Command: []string{"/" + wrapperBinName, "install", wrapperMountDir},
			VolumeMounts: []corev1.VolumeMount{{
				Name:      wrapperVolumeName,
				MountPath: wrapperMountDir,
			}},
		})
	}

	c := &spec.Containers[runnerIdx]
	c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{
		Name:      wrapperVolumeName,
		MountPath: wrapperMountDir,
		ReadOnly:  true,
	})
	// Override the image entrypoint with the injected wrapper. Clear Args too: a
	// command override drops the image's default CMD, and any tenant-set Args were
	// meant for the original entrypoint, not the wrapper.
	c.Command = []string{wrapperMountDir + "/" + wrapperBinName}
	c.Args = nil
}

// applySecurityDefaults stamps a secure-by-default SecurityContext onto the
// worker PodSpec, scaled to the tenant's PSA profile. It gap-fills only: any
// field the tenant set explicitly in the PodTemplate is preserved.
//
//   - privileged: no-op. This profile exists precisely so DinD and
//     host-capability workloads can opt out; stamping defaults would defeat it.
//   - baseline (and the empty default): pod-level runAsNonRoot + runAsUser +
//     seccomp RuntimeDefault. runAsUser is gap-filled to the runner image's
//     numeric UID (defaultWorkerRunAsUser) whenever non-root is being enforced,
//     because kubelet cannot verify runAsNonRoot against the image's
//     non-numeric `USER runner` and would otherwise reject the pod at admission
//     (Q115). All three are compatible with the standard non-root runner image
//     and, crucially, do not block in-job privilege escalation (sudo), which
//     baseline PSA permits and many CI jobs rely on.
//   - restricted: additionally stamps the per-container PSA-restricted floor
//     (allowPrivilegeEscalation=false + drop ALL capabilities). Without it the
//     namespace's PodSecurity admission rejects the pod. Blocking sudo/caps is
//     expected here because the tenant explicitly chose high isolation.
func applySecurityDefaults(spec *corev1.PodSpec, securityProfile string) {
	profile := strings.ToLower(strings.TrimSpace(securityProfile))
	if profile == securityProfilePrivileged {
		return
	}

	if spec.SecurityContext == nil {
		spec.SecurityContext = &corev1.PodSecurityContext{}
	}
	if spec.SecurityContext.RunAsNonRoot == nil {
		spec.SecurityContext.RunAsNonRoot = ptr.To(true)
	}
	// Gap-fill a numeric runAsUser only while non-root is actually enforced and
	// the tenant didn't pick a UID. kubelet can only verify runAsNonRoot against
	// a numeric UID; the runner image's non-numeric `USER runner` otherwise
	// trips CreateContainerConfigError at admission (Q115). Skipped when a tenant
	// opted out with runAsNonRoot:false (a root-based image) so we don't force a
	// UID that contradicts their choice, and gap-fill so an explicit per-pod or
	// per-container runAsUser still wins.
	if r := spec.SecurityContext.RunAsNonRoot; r != nil && *r && spec.SecurityContext.RunAsUser == nil {
		spec.SecurityContext.RunAsUser = ptr.To(defaultWorkerRunAsUser)
	}
	if spec.SecurityContext.SeccompProfile == nil {
		spec.SecurityContext.SeccompProfile = &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}
	}

	if profile != securityProfileRestricted {
		return
	}

	harden := func(containers []corev1.Container) {
		for i := range containers {
			if containers[i].SecurityContext == nil {
				containers[i].SecurityContext = &corev1.SecurityContext{}
			}
			sc := containers[i].SecurityContext
			if sc.AllowPrivilegeEscalation == nil {
				sc.AllowPrivilegeEscalation = ptr.To(false)
			}
			if sc.Capabilities == nil {
				sc.Capabilities = &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}
			}
			if sc.RunAsNonRoot == nil {
				sc.RunAsNonRoot = ptr.To(true)
			}
			if sc.SeccompProfile == nil {
				sc.SeccompProfile = &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}
			}
		}
	}
	harden(spec.Containers)
	harden(spec.InitContainers)
}

// applyResourceDefaults stamps default CPU/memory requests and limits onto any
// regular worker container that declares neither, on every profile. Init
// containers are left untouched: their requests inflate the pod's effective
// scheduling footprint and are usually short-lived setup steps. Gap-fill only —
// a container that sets either requests or limits keeps the tenant's values.
func (p *Provisioner) applyResourceDefaults(spec *corev1.PodSpec) {
	for i := range spec.Containers {
		r := &spec.Containers[i].Resources
		if len(r.Requests) > 0 || len(r.Limits) > 0 {
			continue
		}
		r.Requests = corev1.ResourceList{
			corev1.ResourceCPU:    defaultWorkerCPU,
			corev1.ResourceMemory: defaultWorkerMemory,
		}
		r.Limits = corev1.ResourceList{
			corev1.ResourceCPU:    defaultWorkerCPU,
			corev1.ResourceMemory: defaultWorkerMemory,
		}
	}
}

// waitForCompletion blocks until the pod reaches a terminal phase, delegating to
// the event-driven Waiter when one is wired (production) and otherwise falling
// back to the poll loop (fake-client unit tests). Returns the final phase, reason
// (for eviction detection), and any error.
func (p *Provisioner) waitForCompletion(ctx context.Context, namespace, podName string) (corev1.PodPhase, string, error) {
	if p.Waiter != nil {
		return p.Waiter.WaitForCompletion(ctx, namespace, podName)
	}
	return p.waitForPodCompletion(ctx, namespace, podName)
}

// waitForPodCompletion polls until the pod reaches a terminal phase. It is the
// fallback used when no Waiter is wired; production replaces it with the
// event-driven InformerPodWaiter (see Provisioner.Waiter).
// Returns the final phase, reason (for eviction detection), and any error.
func (p *Provisioner) waitForPodCompletion(ctx context.Context, namespace, podName string) (corev1.PodPhase, string, error) {
	interval := p.PollInterval
	if interval == 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-ticker.C:
			var pod corev1.Pod
			if err := p.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: podName}, &pod); err != nil {
				if client.IgnoreNotFound(err) == nil {
					// Pod was deleted externally; treat as completion.
					return corev1.PodSucceeded, "", nil
				}
				return "", "", fmt.Errorf("provisioner: watch pod: %w", err)
			}
			switch pod.Status.Phase {
			case corev1.PodSucceeded, corev1.PodFailed, corev1.PodUnknown:
				return pod.Status.Phase, pod.Status.Reason, nil
			}
		}
	}
}

// deletePod deletes a worker pod, tolerating NotFound (the reaper or an
// external actor may have removed it first).
func (p *Provisioner) deletePod(ctx context.Context, namespace, name string) error {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
	if err := p.Client.Delete(ctx, pod); client.IgnoreNotFound(err) != nil {
		p.logFor().Warn("failed to delete worker pod", "pod", name, "error", err)
		return err
	}
	return nil
}

func (p *Provisioner) deleteSecret(ctx context.Context, namespace, name string) error {
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
	if err := p.Client.Delete(ctx, s); client.IgnoreNotFound(err) != nil {
		p.logFor().Warn("failed to delete job Secret", "secret", name, "error", err)
		return err
	}
	return nil
}

// logFor returns the provisioner's base logger (or slog.Default when unset).
func (p *Provisioner) logFor() *slog.Logger {
	if p.Log == nil {
		return slog.Default()
	}
	return p.Log
}

// logForKey returns the base logger scoped to the owning object's namespace/name
// so a session→job→pod trail is followable (Q87, Theme F).
func (p *Provisioner) logForKey(key client.ObjectKey) *slog.Logger {
	return p.logFor().With("namespace", key.Namespace, "owner", key.Name)
}

// mergeEnvOverride appends or replaces env vars in base with those in overrides.
// Entries in overrides take precedence; base entries with the same Name are dropped.
func mergeEnvOverride(base, overrides []corev1.EnvVar) []corev1.EnvVar {
	names := make(map[string]struct{}, len(overrides))
	for _, e := range overrides {
		names[e.Name] = struct{}{}
	}
	result := make([]corev1.EnvVar, 0, len(base)+len(overrides))
	for _, e := range base {
		if _, drop := names[e.Name]; !drop {
			result = append(result, e)
		}
	}
	return append(result, overrides...)
}

// dnsLabelRe matches characters not allowed in Kubernetes DNS labels.
var dnsLabelRe = regexp.MustCompile(`[^a-z0-9-]`)

// repoSegmentRE accepts only the characters GitHub allows in org/repo names.
// Must start with an alphanumeric character so that ".." and other dot-leading
// strings cannot produce path-traversal sequences in the API URL.
var repoSegmentRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// safeName converts an arbitrary string into a Kubernetes-safe DNS label
// (lowercase, alphanumeric and hyphens only). The output is at most 48 chars:
// up to 40 sanitised chars from the input, a "-" separator, and 7 hex chars
// derived from a SHA-256 hash of the original string. The hash suffix ensures
// uniqueness when two different inputs share the same 40-char sanitised prefix.
func safeName(s string) string {
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(s)))[:7]
	s = strings.ToLower(s)
	s = dnsLabelRe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 40 {
		s = s[:40]
	}
	s = strings.TrimRight(s, "-") // re-trim after truncation
	if s == "" {
		s = "job"
	}
	return s + "-" + hash
}
