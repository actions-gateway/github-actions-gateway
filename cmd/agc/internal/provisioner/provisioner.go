// Package provisioner creates and manages ephemeral worker pods for acquired
// GitHub Actions jobs.
//
// The Provisioner replaces the M2 stubJobHandler: it stages a short-lived
// Kubernetes Secret containing the raw AcquireJob payload, creates an
// Ephemeral Worker Pod that mounts the Secret, watches for pod completion,
// and cleans up the Secret when the pod terminates.
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
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// tracer is the OpenTelemetry tracer for the provisioner. It resolves to the
// global provider, which is the no-op provider unless main.go's tracing.Init
// installed an exporter — so these spans cost almost nothing when tracing is off.
var tracer = otel.Tracer(tracing.InstrumentationName)

const (
	labelManagedBy = "app.kubernetes.io/managed-by"
	// LabelRunnerGroup is stamped on every worker pod (and job Secret) with the
	// owning RunnerGroup's name as its value. It is the contract the RunnerGroup
	// controller's Pod watch filters and maps on, so it is exported.
	LabelRunnerGroup = "actions-gateway/runner-group"
	labelPlanID      = "actions-gateway/plan-id"

	// Recommended Kubernetes labels (app.kubernetes.io/*) stamped on every
	// worker pod so k9s, Prometheus relabel rules, and `kubectl get -l` work
	// out of the box without operators learning the project-specific keys.
	labelAppName      = "app.kubernetes.io/name"
	labelAppInstance  = "app.kubernetes.io/instance"
	labelAppComponent = "app.kubernetes.io/component"
	labelAppPartOf    = "app.kubernetes.io/part-of"

	// workerAppName / workerComponent / partOf are the recommended-label
	// values for worker pods.
	workerAppName   = "actions-runner"
	workerComponent = "runner"
	partOf          = "actions-gateway"

	// securityProfilePrivileged / securityProfileRestricted are the PSA
	// enforcement levels (mirrored from ActionsGateway.spec.securityProfile)
	// that gate how much of the secure-by-default worker SecurityContext
	// buildPod stamps. Any other value (including the empty string and the
	// "baseline" default) gets the baseline hardening set.
	securityProfilePrivileged = "privileged"
	securityProfileRestricted = "restricted"
	managerName               = names.ControllerName

	// DefaultWorkerImage is the fallback worker image when RunnerGroup.Spec.WorkerImage
	// is empty. Combine with an immutable digest for production deployments.
	//
	// Aligned with the Actions Runner Controller (ARC) gha-runner-scale-set chart,
	// which defaults to ghcr.io/actions/actions-runner. Tenants copy-pasting from
	// ARC examples see the same image name. Operators override at AGC startup via
	// --worker-image (env: WORKER_IMAGE); the per-RunnerGroup workerImage field
	// overrides further.
	//
	// The version (2.334.0) MUST match the FROM line in cmd/worker/Dockerfile —
	// see the runner-version bump procedure in that file's header comment.
	DefaultWorkerImage = "ghcr.io/actions/actions-runner:2.334.0"

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
)

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
	Client             client.Client
	Metrics            *listener.Metrics
	Log                *slog.Logger
	MaxEvictionRetries int
	EvictionRetryDelay time.Duration
	MaxQuotaRetries    int
	QuotaRetryDelay    time.Duration
	PollInterval       time.Duration
	DefaultWorkerImage string
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

	// HTTPClient is used for GitHub API calls. Defaults to http.DefaultClient.
	HTTPClient *http.Client

	// evictionCounts tracks per run_id eviction retry counts.
	evictionCounts sync.Map // key: run_id (string) → value: int
}

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

// HandlerFor returns a JobHandlerFunc bound to the given RunnerGroup.
// The returned function is injected into listener.Config.JobHandler.
// Per-RunnerGroup settings override the provisioner-level defaults.
func (p *Provisioner) HandlerFor(rg *v1alpha1.RunnerGroup) listener.JobHandlerFunc {
	maxEviction := p.MaxEvictionRetries
	if rg.Spec.MaxEvictionRetries != nil {
		maxEviction = int(*rg.Spec.MaxEvictionRetries)
	}
	evictionDelay := p.EvictionRetryDelay
	if rg.Spec.EvictionRetryDelay != nil && rg.Spec.EvictionRetryDelay.Duration > 0 {
		evictionDelay = rg.Spec.EvictionRetryDelay.Duration
	}
	maxQuota := p.MaxQuotaRetries
	if rg.Spec.MaxQuotaRetries != nil {
		maxQuota = int(*rg.Spec.MaxQuotaRetries)
	}
	quotaDelay := p.QuotaRetryDelay
	if rg.Spec.QuotaRetryDelay != nil && rg.Spec.QuotaRetryDelay.Duration > 0 {
		quotaDelay = rg.Spec.QuotaRetryDelay.Duration
	}
	return func(ctx context.Context, runServiceURL, planID string, payload []byte, jitConfig string) error {
		return p.provision(ctx, rg, planID, payload, jitConfig, maxEviction, evictionDelay, maxQuota, quotaDelay)
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
			fmt.Sscanf(v.Value, "%d", &runID)
		}
	}
	if runID == 0 {
		runID = ap.RunID
	}
	return
}

func (p *Provisioner) provision(ctx context.Context, rg *v1alpha1.RunnerGroup, planID string, payload []byte, jitConfig string, maxEviction int, evictionDelay time.Duration, maxQuota int, quotaDelay time.Duration) (err error) {
	log := p.logFor(rg)
	start := time.Now()

	safePlanID := safeName(planID)
	secretName := "job-" + safePlanID
	podName := fmt.Sprintf("runner-%s-%s", safeName(rg.Name), safePlanID)
	// Keep pod names ≤63 chars (Kubernetes DNS label limit).
	if len(podName) > 63 {
		podName = podName[:63]
	}

	// Root span for the whole job-provisioning path. Child spans below break out
	// the latency of each phase (secret staging, pod-count, pod creation, and the
	// wait for completion — usually the long pole). The deferred closure stamps
	// the span's error status from the named return so any early exit is visible.
	ctx, span := tracer.Start(ctx, "Provisioner.provision", trace.WithAttributes(
		attribute.String("runnergroup.namespace", rg.Namespace),
		attribute.String("runnergroup.name", rg.Name),
		attribute.String("plan.id", planID),
		attribute.String("pod.name", podName),
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

	// 1. Stage the job Secret.
	if err = traceStep(ctx, "stageJobSecret", func(ctx context.Context) error {
		secret := p.buildSecret(rg, secretName, planID, payload, jitConfig)
		return p.Client.Create(ctx, secret)
	}); err != nil {
		return fmt.Errorf("provisioner: create Secret %s: %w", secretName, err)
	}
	log.Info("job Secret created", "secret", secretName)

	// 2. Count active pods for ceiling check.
	var count int32
	if err = traceStep(ctx, "countActivePods", func(ctx context.Context) error {
		var cErr error
		count, cErr = p.activePodCount(ctx, rg.Namespace, rg.Name)
		return cErr
	}); err != nil {
		_ = p.deleteSecret(ctx, rg.Namespace, secretName)
		return fmt.Errorf("provisioner: count active pods: %w", err)
	}
	span.SetAttributes(attribute.Int("active_pods", int(count)))

	// 3. Ceiling enforcement.
	priorityClass, held := p.ceilingCheck(rg, count)
	span.SetAttributes(attribute.Bool("ceiling.held", held), attribute.String("priority_class", priorityClass))
	if held {
		log.Info("pod held by concurrency ceiling", "activePods", count)
		_ = p.deleteSecret(ctx, rg.Namespace, secretName)
		err = fmt.Errorf("provisioner: concurrency ceiling reached (%d active pods)", count)
		return err
	}

	// 4. Build and create the pod (with quota retry).
	if err = traceStep(ctx, "createPod", func(ctx context.Context) error {
		pod := p.buildPod(rg, podName, secretName, priorityClass)
		return p.createPodWithQuotaRetry(ctx, rg, pod, maxQuota, quotaDelay, log)
	}); err != nil {
		_ = p.deleteSecret(ctx, rg.Namespace, secretName)
		return fmt.Errorf("provisioner: create Pod %s: %w", podName, err)
	}
	log.Info("worker pod created", "pod", podName, "priorityClass", priorityClass)

	// 5. Watch for pod completion (event-driven when a Waiter is wired; poll fallback otherwise).
	var phase corev1.PodPhase
	var reason string
	if err = traceStep(ctx, "waitForCompletion", func(ctx context.Context) error {
		var wErr error
		phase, reason, wErr = p.waitForCompletion(ctx, rg.Namespace, podName)
		return wErr
	}); err != nil {
		// Context cancelled or unrecoverable watch error.
		_ = p.deleteSecret(ctx, rg.Namespace, secretName)
		return err
	}

	duration := time.Since(start)
	span.SetAttributes(
		attribute.String("pod.phase", string(phase)),
		attribute.String("pod.reason", reason),
		attribute.Float64("duration_seconds", duration.Seconds()),
	)
	log.Info("worker pod completed", "pod", podName, "phase", phase, "reason", reason, "duration", duration)
	if p.Metrics != nil {
		p.Metrics.JobDuration.WithLabelValues(rg.Namespace, rg.Name).Observe(duration.Seconds())
	}

	// 6. Eviction handling.
	if phase == corev1.PodFailed && reason == "Evicted" {
		p.handleEviction(ctx, rg, owner, repo, runID, log, maxEviction, evictionDelay)
	}

	// 7. Cleanup.
	_ = p.deleteSecret(ctx, rg.Namespace, secretName)
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
func (p *Provisioner) createPodWithQuotaRetry(ctx context.Context, rg *v1alpha1.RunnerGroup, pod *corev1.Pod, maxRetries int, retryDelay time.Duration, log *slog.Logger) error {
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
					p.Metrics.QuotaRetriesExhausted.WithLabelValues(rg.Namespace, rg.Name).Inc()
				}
			}
			return err
		}
		log.Info("pod creation blocked by namespace quota; retrying",
			"pod", pod.Name, "attempt", attempt+1, "maxRetries", maxRetries, "delay", retryDelay)
		if p.Metrics != nil {
			p.Metrics.QuotaRetries.WithLabelValues(rg.Namespace, rg.Name).Inc()
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

func (p *Provisioner) handleEviction(ctx context.Context, rg *v1alpha1.RunnerGroup, owner, repo, runID string, log *slog.Logger, maxRetries int, retryDelay time.Duration) {
	if runID == "0" || runID == "" {
		log.Warn("pod evicted but run_id unknown; skipping auto-retry")
		return
	}

	actual, _ := p.evictionCounts.LoadOrStore(runID, 0)
	count := actual.(int)
	if count >= maxRetries {
		log.Warn("eviction retry budget exhausted; manual rerun required",
			"runID", runID, "maxRetries", maxRetries)
		if p.Metrics != nil {
			p.Metrics.EvictionRetriesExhausted.WithLabelValues(rg.Namespace, rg.Name).Inc()
		}
		p.evictionCounts.Delete(runID)
		return
	}

	p.evictionCounts.Store(runID, count+1)
	log.Info("pod evicted; scheduling auto-retry", "runID", runID, "attempt", count+1)
	if p.Metrics != nil {
		p.Metrics.EvictionRetries.WithLabelValues(rg.Namespace, rg.Name).Inc()
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
		log.Info("eviction auto-retry triggered", "runID", runID, "attempt", count+1)
	}
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
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("rerun API call: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	// GitHub returns 201 Created on success.
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("rerun API returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// activePodCount returns the number of Running or Pending worker pods for the given group.
func (p *Provisioner) activePodCount(ctx context.Context, namespace, groupName string) (int32, error) {
	var podList corev1.PodList
	sel := labels.Set{LabelRunnerGroup: groupName}
	if err := p.Client.List(ctx, &podList,
		client.InNamespace(namespace),
		client.MatchingLabels(sel),
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
// the pod should be held due to a concurrency ceiling.
func (p *Provisioner) ceilingCheck(rg *v1alpha1.RunnerGroup, activePods int32) (priorityClass string, held bool) {
	if len(rg.Spec.PriorityTiers) > 0 {
		for _, tier := range rg.Spec.PriorityTiers {
			if activePods < tier.Threshold {
				return tier.PriorityClassName, false
			}
		}
		// All tiers exhausted.
		return "", true
	}
	if rg.Spec.MaxWorkers != nil && activePods >= *rg.Spec.MaxWorkers {
		return "", true
	}
	return "", false
}

func (p *Provisioner) buildSecret(rg *v1alpha1.RunnerGroup, name, planID string, payload []byte, jitConfig string) *corev1.Secret {
	data := map[string][]byte{
		payloadKey: payload,
		planIDKey:  []byte(planID),
	}
	if jitConfig != "" {
		data[jitConfigKey] = []byte(jitConfig)
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: rg.Namespace,
			Labels: map[string]string{
				labelManagedBy:   managerName,
				LabelRunnerGroup: rg.Name,
			},
		},
		Data: data,
	}
}

func (p *Provisioner) buildPod(rg *v1alpha1.RunnerGroup, podName, secretName, priorityClass string) *corev1.Pod {
	// Start from the tenant's PodTemplate.
	template := rg.Spec.PodTemplate.DeepCopy()

	workerImage := rg.Spec.WorkerImage
	if workerImage == "" {
		if p.DefaultWorkerImage != "" {
			workerImage = p.DefaultWorkerImage
		} else {
			workerImage = DefaultWorkerImage
		}
	}

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
	if p.ProxyTLSSecretName != "" {
		caMode := int32(0o444)
		template.Spec.Volumes = append(template.Spec.Volumes, corev1.Volume{
			Name: proxyCAVolumeName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: p.ProxyTLSSecretName,
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
	if p.ProxyTLSSecretName != "" {
		proxyCACertPath = proxyCAMountPath + "/" + proxyCAFileName
	}
	proxyEnvs := []corev1.EnvVar{
		{Name: "HTTP_PROXY", Value: p.HTTPProxy},
		{Name: "HTTPS_PROXY", Value: p.HTTPSProxy},
		{Name: "NO_PROXY", Value: p.NoProxy},
		{Name: "PROXY_CA_CERT_PATH", Value: proxyCACertPath},
	}
	c.Env = mergeEnvOverride(c.Env, proxyEnvs)

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
	p.applySecurityDefaults(&template.Spec)
	p.applyResourceDefaults(&template.Spec)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: rg.Namespace,
			Labels: map[string]string{
				labelManagedBy:   managerName,
				LabelRunnerGroup: rg.Name,
				labelPlanID:      safeName(podName), // use pod name fragment as stable label
				// "actions-gateway/component: workload" matches the workload NetworkPolicy
				// podSelector so worker egress is restricted to the per-tenant proxy only.
				"actions-gateway/component": "workload",
				// Recommended app.kubernetes.io/* labels for tooling interop.
				labelAppName:      workerAppName,
				labelAppInstance:  rg.Name,
				labelAppComponent: workerComponent,
				labelAppPartOf:    partOf,
			},
		},
		Spec: template.Spec,
	}

	if priorityClass != "" {
		pod.Spec.PriorityClassName = priorityClass
	}

	return pod
}

// applySecurityDefaults stamps a secure-by-default SecurityContext onto the
// worker PodSpec, scaled to the tenant's PSA profile. It gap-fills only: any
// field the tenant set explicitly in the PodTemplate is preserved.
//
//   - privileged: no-op. This profile exists precisely so DinD and
//     host-capability workloads can opt out; stamping defaults would defeat it.
//   - baseline (and the empty default): pod-level runAsNonRoot + seccomp
//     RuntimeDefault. Both are compatible with the standard non-root runner
//     image and, crucially, do not block in-job privilege escalation (sudo),
//     which baseline PSA permits and many CI jobs rely on.
//   - restricted: additionally stamps the per-container PSA-restricted floor
//     (allowPrivilegeEscalation=false + drop ALL capabilities). Without it the
//     namespace's PodSecurity admission rejects the pod. Blocking sudo/caps is
//     expected here because the tenant explicitly chose high isolation.
func (p *Provisioner) applySecurityDefaults(spec *corev1.PodSpec) {
	profile := strings.ToLower(strings.TrimSpace(p.SecurityProfile))
	if profile == securityProfilePrivileged {
		return
	}

	if spec.SecurityContext == nil {
		spec.SecurityContext = &corev1.PodSecurityContext{}
	}
	if spec.SecurityContext.RunAsNonRoot == nil {
		spec.SecurityContext.RunAsNonRoot = ptr.To(true)
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

func (p *Provisioner) deleteSecret(ctx context.Context, namespace, name string) error {
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
	if err := p.Client.Delete(ctx, s); client.IgnoreNotFound(err) != nil {
		p.logFor(nil).Warn("failed to delete job Secret", "secret", name, "error", err)
		return err
	}
	return nil
}

func (p *Provisioner) logFor(rg *v1alpha1.RunnerGroup) *slog.Logger {
	log := p.Log
	if log == nil {
		log = slog.Default()
	}
	if rg != nil {
		return log.With("namespace", rg.Namespace, "runnerGroup", rg.Name)
	}
	return log
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
