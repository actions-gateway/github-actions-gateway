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
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/listener"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/runnercore"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/tracing"
	"github.com/actions-gateway/github-actions-gateway/agc/names"
	"github.com/actions-gateway/github-actions-gateway/broker"
	"github.com/actions-gateway/github-actions-gateway/githubapp/httpx"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// tracer is the OpenTelemetry tracer for the provisioner. It resolves to the
// global provider, which is the no-op provider unless main.go's tracing.Init
// installed an exporter — so these spans cost almost nothing when tracing is off.
var tracer = otel.Tracer(tracing.InstrumentationName)

// defaultProvisionerClient is the bounded fallback used when Provisioner.HTTPClient
// is nil. Shared so the nil path does not allocate a connection pool per call.
//
// Built lazily (sync.OnceValue), NOT at package init, for the same reason as
// agentpool's defaultRegistrarClient: the AGC patches http.DefaultTransport with the
// per-tenant egress proxy CA in main(), after this package's vars initialize. An
// eager clone would trust only the system roots and fail any GitHub call routed
// through the proxy with "certificate signed by unknown authority" (Q219).
var defaultProvisionerClient = sync.OnceValue(httpx.NewClient)

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
	// the WORKER_IMAGE environment variable (set by the GMC on the AGC Deployment);
	// the per-RunnerGroup workerImage field overrides further.
	//
	// Sourced from names.DefaultWorkerImage (built from names.RunnerVersion) so the
	// runner version stays locked to the agent.version the AGC registers and to the
	// FROM line in the root Dockerfile's `worker` stage — see the bump procedure
	// in that stage's header comment and the lockstep test in
	// cmd/agc/names/runner_version_test.go.
	DefaultWorkerImage = names.DefaultWorkerImage

	// defaultWorkerRunAsUser is the numeric UID applySecurityDefaults stamps
	// alongside runAsNonRoot:true on the baseline/restricted profiles. The
	// actions-runner image (DefaultWorkerImage, and the cmd/worker image built
	// from it) declares a NON-NUMERIC user (`USER runner`). kubelet's
	// runAsNonRoot enforcement can only PROVE a container is non-root against a
	// numeric UID — with only a username it rejects the pod at admission with
	// `CreateContainerConfigError: container has runAsNonRoot and image has
	// non-numeric user`. Pinning the runner's own UID (1001 — see the
	// `USER runner (UID 1001)` line in the root Dockerfile's `worker` stage and
	// the upstream actions/runner-images base) lets kubelet verify non-root
	// without changing which user the runner actually runs as. (Q115)
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
	runnerContainer = WorkerContainerName

	// workerModeEnvVar / workerModeScaleSetValue switch the worker wrapper into the
	// Q264 scale-set path: instead of the classic pipes handoff to Runner.Worker, the
	// pod runs the full runner (run.sh --jitconfig) and pulls its own job (§2.4). Set
	// only by ProvisionScaleSetWorker; unset for classic sets (default behaviour).
	workerModeEnvVar        = "WORKER_MODE"
	workerModeScaleSetValue = "scaleset"

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

	// githubCAVolumeName / githubCAMountPath / githubCAFileName describe how the
	// operator-supplied GitHub CA bundle is projected into the worker pod (Q536).
	// Same shape and same reason as the proxy CA above, one hop further out: on a
	// GHES appliance behind a private CA the runner's own calls — checkout, log and
	// artifact upload — fail the handshake unless the appliance's CA is in its trust
	// store. The path matches the AGC's own mount in
	// [cmd/gmc/internal/controller/actionsgateway_v2_builder.go].
	githubCAVolumeName = "github-ca"
	githubCAMountPath  = "/etc/actions-gateway/github-ca"
	githubCAFileName   = "ca.crt"

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

	// DefaultMaxWorkerLifetime is the effective worker-pod lifetime cap when
	// spec.maxWorkerLifetime is omitted — the provision-time deadline that bounds a
	// worker orphaned while the AGC was down (Q438). It is applied as the pod's
	// activeDeadlineSeconds, so the kubelet enforces it with no live AGC.
	//
	// Twelve hours is 2× GitHub's own 360-minute default job timeout: a job this
	// kills has explicitly declared a `timeout-minutes` more than twice the default
	// it would otherwise have received. That anchor matters because the job's real
	// timeout never reaches the AGC — neither the scale-set JobAssigned message nor
	// (on the tier that has one) the classic acquire response carries it, so a
	// derived deadline is not available and an invented one has to justify itself.
	//
	// It also strictly tightens a bound GitHub already imposes: a self-hosted job is
	// terminated at 5 days regardless. Going lower was considered and rejected —
	// between 8 and 12 hours the affected job population is effectively identical
	// (both are far past the 6-hour default), so the extra aggression buys little
	// while making a legitimate long job likelier to die on a default nobody chose.
	// Full reasoning: design/03-api-contracts.md (maxWorkerLifetime) and the
	// operator view in operations/troubleshooting.md.
	DefaultMaxWorkerLifetime = 12 * time.Hour
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

// EffectiveMaxWorkerLifetime returns the group's worker-pod lifetime cap,
// applying DefaultMaxWorkerLifetime when the field is omitted. Zero means the
// operator disabled the cap.
func EffectiveMaxWorkerLifetime(rg *v1alpha1.RunnerGroup) time.Duration {
	return MaxWorkerLifetimeOrDefault(rg.Spec.MaxWorkerLifetime)
}

// Provisioner creates and manages worker pods for acquired GitHub Actions jobs.
type Provisioner struct {
	Client  client.Client
	Metrics *runnercore.Metrics
	// Events records owner-scoped Kubernetes Events for v1 RunnerGroup provisioning
	// incidents (quota/eviction-retry exhaustion), routed through the runnerGroupTarget
	// seam — the only Target the Provisioner itself constructs. The v2 RunnerSet path
	// carries its own recorder on runnerSetTarget (built by the RunnerSet reconciler),
	// since one Provisioner is shared across both owners. Nil disables event recording.
	Events             runnercore.EventRecorder
	Log                *slog.Logger
	MaxEvictionRetries int
	EvictionRetryDelay time.Duration
	// EvictionRerunWindow bounds how long a single disruption recovery keeps
	// retrying a re-run that GitHub refuses with "This workflow is already
	// running" — the refusal every re-run gets until GitHub concludes the
	// original run, ~10 minutes after an ungraceful kill (Q503). Zero means the
	// default (15 minutes).
	EvictionRerunWindow time.Duration
	// EvictionRerunRetryInterval paces the refused re-run attempts inside the
	// window. Zero means the default (30 seconds).
	EvictionRerunRetryInterval time.Duration
	MaxQuotaRetries            int
	QuotaRetryDelay            time.Duration
	PollInterval               time.Duration
	DefaultWorkerImage         string

	// DisableQuotaAdmission turns OFF the admission gate's namespace-ResourceQuota
	// rung (#784), reverting to the pre-#784 behaviour where quota exhaustion is
	// discovered only after AcquireJob, by createPodWithQuotaRetry burning lock time.
	// Opt-out rather than opt-in: leaving jobs queued at GitHub strictly dominates
	// claiming them and then failing to place them, so the safer path is the default.
	// main.go sets it from AGC_QUOTA_ADMISSION=false — an escape hatch for a cluster
	// whose ResourceQuota .status is unreliable enough to starve a tenant. There is
	// deliberately no tenant-facing CRD field for it: on a GMC-provisioned gateway the
	// AGC's env is reachable only via the testing-only AGC_EXTRA_* passthrough, so the
	// opt-out gets promoted to a real field if a production case ever needs it.
	DisableQuotaAdmission bool

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

	// GitHubCAConfigMapName names a ConfigMap in the tenant namespace whose ca.crt
	// key is the CA bundle fronting this gateway's GHES appliance
	// (ActionsGateway.spec.githubCABundleRef). When non-empty the provisioner
	// projects it into the worker pod at githubCAMountPath/githubCAFileName so the
	// wrapper can add it to the runner's trust store alongside the proxy CA. Empty
	// (the default) skips the mount — public GitHub needs no extra trust.
	GitHubCAConfigMapName string

	// Waiter blocks until a worker pod reaches a terminal phase. When set
	// (production wires an InformerPodWaiter via main.go), completion is
	// event-driven off the shared Pod informer. When nil, provision falls back
	// to polling Client every PollInterval — used by the fake-client unit tests,
	// which have no informer, and as a defensive fallback.
	Waiter PodWaiter

	// TokenFunc returns a valid GitHub App installation token for API calls.
	// If nil, eviction auto-retry is logged but the rerun API is not called.
	TokenFunc func(ctx context.Context) (string, error)

	// GitHubAPIURL is the base URL for the GitHub REST API — the same endpoint the
	// installation token was minted against.
	//
	// REQUIRED for the disruption auto-retry path; there is deliberately no default
	// (Q504): a default that diverges from the token exchange's endpoint posts a
	// valid installation token to a host that never issued it (a bare 401 on GHES).
	// Resolve it with githubapp.ResolveAPIBaseURL so the two cannot drift.
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

	// scaleUp is the opt-in, per-RunnerGroup token bucket that rate-limits worker-pod
	// CREATION (Q223), gating each pod creation in provision/ProvisionScaleSetWorker
	// when the owner sets spec.scaleUp. Default-off: a nil ScaleUpConfig makes every
	// wait a no-op. Its zero value is ready to use. See scaleuplimiter.go.
	scaleUp scaleUpLimiter
}

// NewProvisioner creates a Provisioner with sensible defaults.
func NewProvisioner(c client.Client, m *runnercore.Metrics, log *slog.Logger) *Provisioner {
	return &Provisioner{
		Client:                     c,
		Metrics:                    m,
		Log:                        log,
		MaxEvictionRetries:         2,
		EvictionRetryDelay:         5 * time.Second,
		EvictionRerunWindow:        defaultEvictionRerunWindow,
		EvictionRerunRetryInterval: defaultEvictionRerunRetryInterval,
		MaxQuotaRetries:            5,
		QuotaRetryDelay:            30 * time.Second,
		PollInterval:               5 * time.Second,
		DefaultWorkerImage:         DefaultWorkerImage,
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
	return func(ctx context.Context, runServiceURL, planID string, payload []byte, jitConfig string) (broker.TaskResult, error) {
		return p.provision(ctx, target, planID, payload, jitConfig)
	}
}

// provision stages the job wiring, creates the worker pod, and waits for it to
// reach a terminal phase. The returned broker.TaskResult is the POD-PHASE PROXY of
// the outcome (PodFailed→failed, a worker removed before it ran→abandoned, else
// succeeded) that the listener reports for this job's assignments (Q260 Option A);
// it is empty on any error path that returns before the pod reached a terminal
// phase (the fan-out treats an empty result as succeeded).
func (p *Provisioner) provision(ctx context.Context, target Target, planID string, payload []byte, jitConfig string) (result broker.TaskResult, err error) {
	key := target.Key()
	log := p.logForKey(key)

	// Resolve the current provisioning spec for this job. For v1 this re-reads the
	// fresh RunnerGroup; for v2 it re-resolves the RunnerSet's RunnerTemplate and
	// EgressProxy. A resolution error fails the job fail-closed — no Secret, no pod
	// — so no worker wiring is ever created while a reference is unresolved (§H.7).
	spec, err := target.Resolve(ctx)
	if err != nil {
		log.Warn("provisioning spec unresolved; failing job without creating a pod", "error", err)
		return "", fmt.Errorf("provisioner: resolve provisioning spec: %w", err)
	}
	start := time.Now()

	secretName := "job-" + safeName(planID)
	// workerPodName owns the 63-char DNS-label budget and the validity of the
	// result; never truncate the assembled name here (Q467).
	podName := workerPodName(key.Name, planID)
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
	// A malformed payload only degrades eviction-retry context, so log and
	// continue rather than failing provisioning — a silent drop would hide
	// genuine payload corruption.
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
		return "", fmt.Errorf("provisioner: create Secret %s: %w", secretName, err)
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
		return "", fmt.Errorf("provisioner: count active pods: %w", err)
	}
	span.SetAttributes(attribute.Int("gateway.active_pods", int(count)))

	// 3. Ceiling enforcement.
	priorityClass, held := ceilingCheck(spec, count)
	span.SetAttributes(attribute.Bool("gateway.ceiling_held", held), attribute.String("gateway.priority_class", priorityClass))
	if held {
		log.Info("pod held by concurrency ceiling", "activePods", count)
		_ = p.deleteSecret(ctx, key.Namespace, secretName)
		err = fmt.Errorf("provisioner: concurrency ceiling reached (%d active pods)", count)
		return "", err
	}

	// 4. Scale-up rate limit (Q223): when the owner opts in via spec.scaleUp, wait
	// for a token before creating the pod so a burst of simultaneously-acquired jobs
	// ramps up in waves instead of all at once (default-off is a no-op). A ctx
	// cancellation here (AGC shutdown, or the renew loop tearing the job down on a
	// lost lock) abandons the job without a pod — same shape as a quota-retry
	// cancellation — after cleaning up the staged Secret.
	if err = traceStep(ctx, "scaleUpRateLimit", func(ctx context.Context) error {
		throttled, wErr := p.scaleUp.wait(ctx, key.String(), spec.ScaleUp)
		if throttled && p.Metrics != nil {
			p.Metrics.ScaleUpThrottled.WithLabelValues(key.Namespace, key.Name).Inc()
		}
		return wErr
	}); err != nil {
		_ = p.deleteSecret(ctx, key.Namespace, secretName)
		return "", err
	}

	// 5. Build and create the pod (with quota retry).
	if err = traceStep(ctx, "createPod", func(ctx context.Context) error {
		pod := p.buildPod(target, spec, podName, secretName, priorityClass, meta)
		return p.createPodWithQuotaRetry(ctx, target, pod, spec.MaxQuotaRetries, spec.QuotaRetryDelay, log)
	}); err != nil {
		_ = p.deleteSecret(ctx, key.Namespace, secretName)
		return "", fmt.Errorf("provisioner: create Pod %s: %w", podName, err)
	}
	// Per-pod hot-path line; podName is on the logger context. Debug (Q87, Theme D).
	log.Debug("worker pod created", "priorityClass", priorityClass)

	// 6. Watch for pod completion (event-driven when a Waiter is wired; poll fallback otherwise).
	var outcome PodOutcome
	if err = traceStep(ctx, "waitForCompletion", func(ctx context.Context) error {
		var wErr error
		outcome, wErr = p.waitForCompletion(ctx, key.Namespace, podName)
		return wErr
	}); err != nil {
		// Context cancelled or unrecoverable watch error. When the listener abandoned
		// this one job, the pod is still running a job nothing will ever report, so
		// reclaim it (Q501); a shutdown cancels every job context at once and must
		// leave live workers alone. reclaimAbandonedWorker checks the cause.
		p.reclaimAbandonedWorker(ctx, target, podName, log)
		_ = p.deleteSecret(ctx, key.Namespace, secretName)
		return "", err
	}
	duration := time.Since(start)
	span.SetAttributes(
		attribute.String("gateway.pod.phase", string(outcome.Phase)),
		attribute.String("gateway.pod.reason", outcome.Reason),
		attribute.Bool("gateway.pod.preempted", outcome.Preempted),
		attribute.Bool("gateway.pod.externally_deleted", outcome.ExternallyDeleted),
		attribute.Bool("gateway.pod.deleted_before_start", outcome.DeletedBeforeStart),
		attribute.Float64("gateway.provision.duration_seconds", duration.Seconds()),
	)
	// Per-pod completion line; podName is on the logger context. Debug (Q87, Theme D).
	log.Debug("worker pod completed",
		"phase", outcome.Phase, "reason", outcome.Reason, "preempted", outcome.Preempted,
		"externallyDeleted", outcome.ExternallyDeleted,
		"deletedBeforeStart", outcome.DeletedBeforeStart, "duration", duration)
	if p.Metrics != nil {
		p.Metrics.JobDuration.WithLabelValues(key.Namespace, key.Name).Observe(duration.Seconds())
	}

	// 7. Disruption recovery. Started here because this goroutine owns the pod and
	// still holds the payload's identity; the scale-set tier, which has neither,
	// recovers from the owning reconciler instead (RecoverEvictedScaleSetWorkers).
	// The recovery itself outlives this goroutine on handleEviction's own bounded
	// context — its re-run cannot land until GitHub concludes the original run,
	// ~10 minutes after an ungraceful kill (Q503), and holding the TaskResult (and
	// the fan-out completion behind it) for that long is not an option — so the
	// done channel is deliberately not waited on.
	//
	// Three causes reach the same recovery. The kubelet's node-pressure eviction is the
	// PodFailed/Evicted shape; kube-scheduler preemption deletes its victim instead, so
	// it is recognised by the DisruptionTarget condition the outcome carried out of the
	// wait (Q497); and a graceful external deletion — a drain, or a `kubectl delete pod`
	// — is recognised by the deletion mark the outcome carried out of the wait (Q502).
	// The deletion arm requires PodFailed: a worker whose job finished cleanly inside
	// the termination grace period reported its own success and has nothing to re-run.
	// The arms are ordered most-specific first — they are mutually exclusive in
	// practice, and either way a single call spends a single slot of the run's one
	// shared retry budget.
	var cause string
	switch {
	case outcome.Phase == corev1.PodFailed && outcome.Reason == podReasonEvicted:
		cause = recoveryCauseEviction
	case outcome.Preempted:
		cause = recoveryCausePreemption
	case outcome.Phase == corev1.PodFailed && outcome.ExternallyDeleted:
		cause = recoveryCauseDeletion
	}
	if cause != "" {
		_ = p.handleEviction(ctx, target, owner, repo, runID, log, spec.MaxEvictionRetries, spec.EvictionRetryDelay, evictionTierClassic, cause)
	}

	// Pod-phase proxy of the job outcome, which the listener reports for this job's
	// assignments (Q260 Option A). The AGC does not learn the workflow's real result —
	// only the worker's runner binary reports it, and only for a job it actually ran —
	// so PodFailed→failed and anything else→succeeded is the honest proxy.
	//
	// A worker taken away before any container started ran no step and registered no
	// runner, so neither value describes it: succeeded concluded an assignment whose
	// job never ran, and the listener had nothing to distinguish it from a clean run
	// (Q628). It is reported abandoned instead, which tells the listener to report
	// nothing for the delivery — completing it would conclude the run green
	// (Q645/Q676) — and leave the job to the acquire lock's lapse.
	//
	// Not when a recovery armed above: those causes already re-run the job, and
	// preemption's own victim is most often a pod that never started (Q497). One
	// recovery per disruption, not two.
	switch {
	case outcome.DeletedBeforeStart && cause == "":
		result = broker.TaskResultAbandoned
		log.Warn("worker pod was removed before it ran; reporting the job as abandoned so the assignment is left to GitHub's lock lapse",
			"phase", outcome.Phase, "duration", duration)
	case outcome.Phase == corev1.PodFailed:
		result = broker.TaskResultFailed
	default:
		result = broker.TaskResultSucceeded
	}

	// 8. Cleanup. The job Secret is always deleted here. The pod is deleted
	// immediately only when the owner's completedPodTTL is zero; otherwise the
	// owner's reconciler reaper deletes it once the TTL elapses — the reaper is
	// also the restart-safe backstop for pods no goroutine watches.
	if spec.CompletedPodTTL == 0 {
		_ = p.deletePod(ctx, key.Namespace, podName)
	}
	_ = p.deleteSecret(ctx, key.Namespace, secretName)
	return result, nil
}

// ScaleSetJob is one scale-set-assigned job to provision a worker for: the identity
// the scale-set listener minted (JobID, JITConfig) plus the workflow-run identity the
// assignment message carried.
//
// The run identity is what makes eviction recovery possible on this tier. The classic
// tier reads owner/repo/run_id out of the AcquireJob payload it holds in memory for
// the life of the job; a scale-set worker has no payload and no owning goroutine, so
// the identity is stamped onto the worker pod at creation and read back off the pod
// when the pod turns up evicted (Q417).
//
// Owner, Repository, and RunID are empty together when the assignment carried no
// complete identity — the worker provisions and runs normally, and only automatic
// eviction recovery degrades. JobName is cosmetic.
type ScaleSetJob struct {
	JobID     string
	JITConfig string
	// RunnerName is the name the listener pre-registered this job's runner under, and
	// is stamped on the pod as AnnotationRunnerName so the reap path can deregister the
	// record it leaves behind (Q550). Empty stamps nothing.
	RunnerName string

	Owner      string
	Repository string
	RunID      string
	JobName    string
}

// jobMeta converts the job's run identity into the annotation set shared with the
// classic path, so both tiers stamp one vocabulary of worker-pod annotations
// (actions-gateway.com/run-id, /repository, /job-name). The classic path derives the
// same shape from the acquire payload via jobMetaFrom.
func (j ScaleSetJob) jobMeta() jobMeta {
	m := jobMeta{runID: j.RunID, jobName: j.JobName}
	if j.Owner != "" && j.Repository != "" {
		m.repository = j.Owner + "/" + j.Repository
	}
	return m
}

// ProvisionScaleSetWorker stages a JIT-config Secret and creates a
// run.sh --jitconfig worker pod for one scale-set-assigned job, then returns without
// waiting for the job (fire-and-forget). Unlike the classic provision(), the runner
// pulls and completes its OWN job through its own broker session (§2.4), so the AGC
// neither hands off an acquired payload nor blocks on completion — the scale-set
// listener observes the terminal JobCompleted on its queue instead.
//
// It is idempotent per jobID: the Secret and pod are named deterministically from the
// jobID, so a job replayed to a re-created session is a no-op (an AlreadyExists is
// treated as success). Capacity is already gated upstream by the listener's advertised
// X-ScaleSetMaxCapacity, so a concurrency-ceiling hit here is a narrow race the
// listener retries on its next poll.
//
// Cleanup: every exit that fails BEFORE the worker pod exists deletes the Secret this
// call staged, so a credential-bearing Secret never outlives a job that never ran
// (Q373). In steady state the Secret must outlive this method — the pod mounts it — so
// it is reclaimed by CleanupScaleSetJob, which the scale-set listener calls on the
// terminal JobCompleted for the job. The Secret and pod also carry the RunnerSet
// OwnerRef, so both cascade-GC when the set is deleted; the reconciler's reaper deletes
// the terminal pod per spec.completedPodTTL.
func (p *Provisioner) ProvisionScaleSetWorker(ctx context.Context, target Target, job ScaleSetJob) error {
	jobID, jitConfig := job.JobID, job.JITConfig
	if jitConfig == "" {
		return fmt.Errorf("provisioner: scale-set worker for job %q has no JIT config", jobID)
	}
	key := target.Key()
	log := p.logForKey(key)

	spec, err := target.Resolve(ctx)
	if err != nil {
		log.Warn("provisioning spec unresolved; not creating a scale-set worker", "error", err)
		return fmt.Errorf("provisioner: resolve provisioning spec: %w", err)
	}

	secretName := scaleSetSecretName(jobID)
	podName := scaleSetPodName(key.Name, jobID)
	// runID pairs with jobID because one run has many jobs: without it two provisioning
	// lines cannot say whether they are sibling jobs or one job redelivered (Q661). The
	// pod annotation carries the same pairing only once the pod exists.
	log = log.With("podName", podName, "jobID", jobID, "runID", job.RunID)

	workerVersion := imageVersion(p.resolveWorkerImage(spec))

	// 1. Stage the JIT-config Secret. There is no acquired payload — the runner pulls
	//    its own job — so payload is nil and only the jitconfig blob is staged.
	//
	// staged records whether THIS call created the Secret. Only then may a failure exit
	// below delete it: an AlreadyExists means an earlier delivery of the same job staged
	// it, and that delivery may already have a live worker pod mounting it (or be about
	// to create one). Deleting another delivery's Secret would strand its pod in
	// ContainerCreating, so a replay cleans up nothing it does not own (Q373).
	staged := false
	secret := p.buildSecret(target, secretName, jobID, workerVersion, nil, jitConfig)
	if err := p.Client.Create(ctx, secret); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("provisioner: create scale-set Secret %s: %w", secretName, err)
		}
	} else {
		staged = true
	}
	// unstage deletes the Secret on a failure exit that leaves no pod behind to mount it.
	// One of those exits is a ctx cancellation (AGC shutdown mid-throttle), on which a
	// delete issued under the same ctx would fail and leak the Secret — so a cancelled
	// ctx falls back to a short detached one.
	unstage := func() {
		if !staged {
			return
		}
		dctx := ctx
		if ctx.Err() != nil {
			var cancel context.CancelFunc
			dctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), secretCleanupTimeout)
			defer cancel()
		}
		_ = p.deleteSecret(dctx, key.Namespace, secretName)
	}

	// 2. Priority-tier selection (capacity is gated upstream; a held result is a race).
	count, err := p.activePodCount(ctx, key.Namespace, target.PodOwnerLabels())
	if err != nil {
		unstage()
		return fmt.Errorf("provisioner: count active pods: %w", err)
	}
	priorityClass, held := ceilingCheck(spec, count)
	if held {
		unstage()
		// Typed, so the listener re-offers the job on a backoff instead of holding the
		// queue cursor for an immediate redelivery that would find the same full ceiling
		// (Q576). The listener normally asks CheckScaleSetCapacity before it mints the
		// JIT config, so reaching this is the race that check cannot close.
		return &CeilingReachedError{ActivePods: count}
	}

	// 3. Scale-up rate limit (Q223): gate pod creation on the owner's opt-in token
	// bucket so a burst of scale-set assignments ramps up in waves (default-off is a
	// no-op). A ctx cancellation abandons this assignment; the scale-set listener
	// re-drives it on its next poll.
	throttled, wErr := p.scaleUp.wait(ctx, key.String(), spec.ScaleUp)
	if throttled && p.Metrics != nil {
		p.Metrics.ScaleUpThrottled.WithLabelValues(key.Namespace, key.Name).Inc()
	}
	if wErr != nil {
		unstage()
		return wErr
	}

	// 4. Build the pod and switch the wrapper into scale-set mode (run.sh --jitconfig).
	//    The pod carries the assignment's run identity as annotations and the tier
	//    marker as a label: fire-and-forget provisioning keeps no process state about
	//    this job, so the pod itself has to be the durable, restart-safe record of
	//    which run to re-run if it is evicted (Q417) — the same reason Q420 put the
	//    reap deadline on the pod rather than in memory.
	pod := p.buildPod(target, spec, podName, secretName, priorityClass, job.jobMeta())
	pod.Labels[LabelAcquisitionProtocol] = AcquisitionProtocolScaleSet
	if job.RunnerName != "" {
		if pod.Annotations == nil {
			pod.Annotations = map[string]string{}
		}
		pod.Annotations[AnnotationRunnerName] = job.RunnerName
	}
	setScaleSetWorkerMode(pod)

	if err := p.createPodWithQuotaRetry(ctx, target, pod, spec.MaxQuotaRetries, spec.QuotaRetryDelay, log); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Idempotent: a replayed job already has its worker pod — and that pod mounts
			// this Secret, so the Secret must survive. It is reclaimed with every other
			// steady-state Secret by CleanupScaleSetJob on the job's completion.
			return nil
		}
		unstage()
		return fmt.Errorf("provisioner: create scale-set Pod %s: %w", podName, err)
	}
	log.Debug("scale-set worker pod created", "priorityClass", priorityClass)
	return nil
}

// secretCleanupTimeout bounds a Secret delete issued on a detached context — the
// cleanup paths that run after the caller's context is already cancelled.
const secretCleanupTimeout = 10 * time.Second

// scaleSetSecretName is the deterministic per-job Secret name for the scale-set worker
// path. Both the staging site (ProvisionScaleSetWorker) and the reclaim site
// (CleanupScaleSetJob) derive the name here so they can never drift apart.
func scaleSetSecretName(jobID string) string { return "job-ss-" + safeName(jobID) }

// scaleSetPodName is the deterministic per-job worker-pod name for the scale-set
// path, derived from the owning set's name and the job ID. Like scaleSetSecretName
// it is the single derivation site, so the creating side (ProvisionScaleSetWorker)
// and the completion-stamping side (CleanupScaleSetJob) cannot drift apart.
//
// It shares workerPodName with the v1 path, which owns the 63-char DNS-label budget:
// this tier truncated the assembled name identically, so the invalid-name defect did
// not disappear when a tenant migrated to v2 (Q467). Renaming does mean an AGC
// upgraded mid-job computes a different name for a v1-era in-flight pod than the one
// that created it; markJobCompleted treats that as NotFound and skips the stamp, so
// the worker still runs to completion and completedPodTTL still reaps it.
func scaleSetPodName(ownerName, jobID string) string {
	return workerPodName(ownerName, jobID)
}

// CleanupScaleSetJob deletes the per-job JIT-config Secret staged for jobID by
// ProvisionScaleSetWorker. It is the steady-state reclaim point for the scale-set path
// (Q373): the Secret cannot be deleted when the worker pod is created (the pod mounts
// it), so the scale-set listener calls this on the terminal JobCompleted for the job,
// at which point the runner has consumed its JIT config and exited.
//
// It is safe to call for a job whose pod is still terminating: the kubelet has long
// since materialized the mounted volume and does not tear a running pod down when its
// Secret disappears.
//
// A job completed before its pod ever started (a cancellation, or a replayed queue) is
// the case the listener works to avoid reaching: it handles a batch's completions before
// its assignments and refuses to provision a job already known completed, so no pod is
// built for a job whose Secret is already reclaimed (Q575). When the completion arrives
// after the pod exists anyway, the pod can no longer mount the Secret — markJobCompleted
// stamps it below, and the reaper collects it on completedJobPendingGrace rather than
// leaving it to sit out the unrelated pendingPodDeadline.
//
// It is idempotent (a NotFound is success), so a replayed completion message, or a
// completion for a job whose Secret a failure path already unstaged, is a no-op.
//
// It also stamps AnnotationJobCompletedAt on the job's worker pod, which is what gives
// a still-Running scale-set worker a reap deadline (Q420) — see markJobCompleted.
func (p *Provisioner) CleanupScaleSetJob(ctx context.Context, target Target, jobID string) error {
	key := target.Key()
	name := scaleSetSecretName(jobID)
	if err := p.deleteSecret(ctx, key.Namespace, name); err != nil {
		return fmt.Errorf("provisioner: delete scale-set Secret %s: %w", name, err)
	}
	p.logForKey(key).Debug("scale-set job Secret reclaimed", "secret", name, "jobID", jobID)
	return p.markJobCompleted(ctx, key.Namespace, scaleSetPodName(key.Name, jobID), jobID)
}

// markJobCompleted stamps AnnotationJobCompletedAt on a scale-set worker pod, recording
// when GitHub declared the pod's job terminal. It is the only writer of that annotation.
//
// Why it exists: the scale-set tier provisions fire-and-forget, so no goroutine owns a
// Running worker the way the classic path's provision() does. A worker that registers
// but never receives its job — its assignment lapsed, was cancelled, or completed
// elsewhere — sits at "Listening for Jobs" forever, and the reaper counted PodRunning as
// active with no deadline of any kind, so it held a concurrency slot and a node until an
// operator deleted it by hand (Q420). The annotation converts "GitHub says this job is
// over" into a durable, restart-safe deadline the reaper can act on, rather than the
// process-scoped state a fire-and-forget provisioner has no way to keep.
//
// It is set-once: a replayed completion (a re-created session polls from cursor 0) must
// not push the deadline back, so an already-stamped pod is left alone. A pod that does
// not exist is not an error — a job cancelled before its worker was created has nothing
// to stamp, and the listener will not build one for it afterwards (Q575).
//
// A Pending pod is stamped as well as a Running one, and the reaper reads the stamp in
// both arms: Pending means the pod never mounted its now-reclaimed Secret and can only
// be collected, Running means the runner is shutting down (Q420).
//
// A pod that has already reached a terminal phase — the ordinary case, where the runner
// ran the job and exited — is left unstamped: completedPodTTL already owns it, so the
// stamp would buy nothing and cost one write per job.
func (p *Provisioner) markJobCompleted(ctx context.Context, namespace, podName, jobID string) error {
	var pod corev1.Pod
	if err := p.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: podName}, &pod); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("provisioner: get scale-set worker pod %s: %w", podName, err)
	}
	switch pod.Status.Phase {
	case corev1.PodSucceeded, corev1.PodFailed, corev1.PodUnknown:
		return nil
	}
	if _, ok := pod.Annotations[AnnotationJobCompletedAt]; ok {
		return nil
	}
	patch := client.MergeFrom(pod.DeepCopy())
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[AnnotationJobCompletedAt] = p.nowFn().UTC().Format(time.RFC3339)
	if err := p.Client.Patch(ctx, &pod, patch); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("provisioner: stamp job completion on worker pod %s: %w", podName, err)
	}
	p.logFor().Debug("stamped job completion on worker pod", "pod", podName, "jobID", jobID)
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
