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

	// scaleUp is the opt-in, per-RunnerGroup token bucket that rate-limits worker-pod
	// CREATION (Q223), gating each pod creation in provision/ProvisionScaleSetWorker
	// when the owner sets spec.scaleUp. Default-off: a nil ScaleUpConfig makes every
	// wait a no-op. Its zero value is ready to use. See scaleuplimiter.go.
	scaleUp scaleUpLimiter
}

// NewProvisioner creates a Provisioner with sensible defaults.
func NewProvisioner(c client.Client, m *runnercore.Metrics, log *slog.Logger) *Provisioner {
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
	return func(ctx context.Context, runServiceURL, planID string, payload []byte, jitConfig string) (broker.TaskResult, error) {
		return p.provision(ctx, target, planID, payload, jitConfig)
	}
}

// provision stages the job wiring, creates the worker pod, and waits for it to
// reach a terminal phase. The returned broker.TaskResult is the POD-PHASE PROXY of
// the outcome (PodFailed→failed, else succeeded) that the listener reports when it
// fans completion out to the deduped sibling deliveries of a fanned-out job (Q260
// Option A); it is empty on any error path that returns before the pod reached a
// terminal phase (the fan-out treats an empty result as succeeded).
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
	var phase corev1.PodPhase
	var reason string
	if err = traceStep(ctx, "waitForCompletion", func(ctx context.Context) error {
		var wErr error
		phase, reason, wErr = p.waitForCompletion(ctx, key.Namespace, podName)
		return wErr
	}); err != nil {
		// Context cancelled or unrecoverable watch error.
		_ = p.deleteSecret(ctx, key.Namespace, secretName)
		return "", err
	}
	// Pod-phase proxy of the job outcome for the listener's fan-out completion of a
	// fanned-out job's deduped sibling deliveries (Q260 Option A). The AGC does not
	// learn the workflow's real result (only the worker's runner binary reports it,
	// for the winner's own delivery), so PodFailed→failed and anything else
	// (Succeeded, or a terminal phase we cannot map) →succeeded is the honest proxy.
	if phase == corev1.PodFailed {
		result = broker.TaskResultFailed
	} else {
		result = broker.TaskResultSucceeded
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

	// 7. Eviction handling.
	if phase == corev1.PodFailed && reason == "Evicted" {
		p.handleEviction(ctx, target, owner, repo, runID, log, spec.MaxEvictionRetries, spec.EvictionRetryDelay)
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
func (p *Provisioner) ProvisionScaleSetWorker(ctx context.Context, target Target, jobID, jitConfig string) error {
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

	safeJob := safeName(jobID)
	secretName := scaleSetSecretName(jobID)
	podName := fmt.Sprintf("runner-%s-%s", safeName(key.Name), safeJob)
	if len(podName) > 63 { // Kubernetes DNS label limit
		podName = podName[:63]
	}
	log = log.With("podName", podName, "jobID", jobID)

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
		return fmt.Errorf("provisioner: concurrency ceiling reached (%d active pods); listener will retry", count)
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
	pod := p.buildPod(target, spec, podName, secretName, priorityClass, jobMeta{})
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

// CleanupScaleSetJob deletes the per-job JIT-config Secret staged for jobID by
// ProvisionScaleSetWorker. It is the steady-state reclaim point for the scale-set path
// (Q373): the Secret cannot be deleted when the worker pod is created (the pod mounts
// it), so the scale-set listener calls this on the terminal JobCompleted for the job,
// at which point the runner has consumed its JIT config and exited.
//
// It is safe to call for a job whose pod is still terminating: the kubelet has long
// since materialized the mounted volume and does not tear a running pod down when its
// Secret disappears. It is also safe — and deliberate — for a job completed before its
// pod ever started (a cancellation): that pod can no longer mount the Secret, so it
// stalls Pending and the reconciler's pending-deadline reaper collects it, which is the
// right outcome for a job that will never run.
//
// It is idempotent (a NotFound is success), so a replayed completion message, or a
// completion for a job whose Secret a failure path already unstaged, is a no-op.
func (p *Provisioner) CleanupScaleSetJob(ctx context.Context, target Target, jobID string) error {
	key := target.Key()
	name := scaleSetSecretName(jobID)
	if err := p.deleteSecret(ctx, key.Namespace, name); err != nil {
		return fmt.Errorf("provisioner: delete scale-set Secret %s: %w", name, err)
	}
	p.logForKey(key).Debug("scale-set job Secret reclaimed", "secret", name, "jobID", jobID)
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
