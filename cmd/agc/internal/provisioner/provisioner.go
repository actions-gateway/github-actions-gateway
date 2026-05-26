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

	"github.com/karlkfi/github-actions-gateway/agc/api/v1alpha1"
	"github.com/karlkfi/github-actions-gateway/agc/internal/listener"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	labelManagedBy   = "app.kubernetes.io/managed-by"
	labelRunnerGroup = "actions-gateway/runner-group"
	labelPlanID      = "actions-gateway/plan-id"
	managerName      = "actions-gateway-agc"

	// DefaultWorkerImage is the fallback worker image when RunnerGroup.Spec.WorkerImage
	// is empty. Combine with an immutable digest for production deployments.
	DefaultWorkerImage = "ghcr.io/actions/runner:2.327.1"

	payloadMountPath = "/run/secrets/job-payload"
	payloadKey       = "payload"
	planIDKey        = "plan-id"
	runnerContainer  = "runner"
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
	// HTTPProxy, HTTPSProxy, and NoProxy are injected into the runner container
	// env of every worker pod. Set from the AGC's own environment by main.go.
	HTTPProxy  string
	HTTPSProxy string
	NoProxy    string

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
	return func(ctx context.Context, runServiceURL, planID string, payload []byte) error {
		return p.provision(ctx, rg, planID, payload, maxEviction, evictionDelay, maxQuota, quotaDelay)
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

func (p *Provisioner) provision(ctx context.Context, rg *v1alpha1.RunnerGroup, planID string, payload []byte, maxEviction int, evictionDelay time.Duration, maxQuota int, quotaDelay time.Duration) error {
	log := p.logFor(rg)
	start := time.Now()

	safePlanID := safeName(planID)
	secretName := "job-" + safePlanID
	podName := fmt.Sprintf("runner-%s-%s", safeName(rg.Name), safePlanID)
	// Keep pod names ≤63 chars (Kubernetes DNS label limit).
	if len(podName) > 63 {
		podName = podName[:63]
	}

	// Extract owner/repo/run_id for eviction retry (best-effort; missing is fine).
	var ap acquirePayload
	_ = json.Unmarshal(payload, &ap)
	owner, repo, runIDInt := ap.repoInfo()
	runID := fmt.Sprintf("%d", runIDInt)

	// 1. Stage the job Secret.
	secret := p.buildSecret(rg, secretName, planID, payload)
	if err := p.Client.Create(ctx, secret); err != nil {
		return fmt.Errorf("provisioner: create Secret %s: %w", secretName, err)
	}
	log.Info("job Secret created", "secret", secretName)

	// 2. Count active pods for ceiling check.
	count, err := p.activePodCount(ctx, rg.Namespace, rg.Name)
	if err != nil {
		_ = p.deleteSecret(ctx, rg.Namespace, secretName)
		return fmt.Errorf("provisioner: count active pods: %w", err)
	}

	// 3. Ceiling enforcement.
	priorityClass, held := p.ceilingCheck(rg, count)
	if held {
		log.Info("pod held by concurrency ceiling", "activePods", count)
		_ = p.deleteSecret(ctx, rg.Namespace, secretName)
		return fmt.Errorf("provisioner: concurrency ceiling reached (%d active pods)", count)
	}

	// 4. Build and create the pod (with quota retry).
	pod := p.buildPod(rg, podName, secretName, priorityClass)
	if err := p.createPodWithQuotaRetry(ctx, rg, pod, maxQuota, quotaDelay, log); err != nil {
		_ = p.deleteSecret(ctx, rg.Namespace, secretName)
		return fmt.Errorf("provisioner: create Pod %s: %w", podName, err)
	}
	log.Info("worker pod created", "pod", podName, "priorityClass", priorityClass)

	// 5. Watch for pod completion.
	phase, reason, err := p.waitForPodCompletion(ctx, rg.Namespace, podName)
	if err != nil {
		// Context cancelled or unrecoverable watch error.
		_ = p.deleteSecret(ctx, rg.Namespace, secretName)
		return err
	}

	duration := time.Since(start)
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
	sel := labels.Set{labelRunnerGroup: groupName}
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

func (p *Provisioner) buildSecret(rg *v1alpha1.RunnerGroup, name, planID string, payload []byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: rg.Namespace,
			Labels: map[string]string{
				labelManagedBy:   managerName,
				labelRunnerGroup: rg.Name,
			},
		},
		Data: map[string][]byte{
			payloadKey: payload,
			planIDKey:  []byte(planID),
		},
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

	// Inject proxy env vars into the runner container (controller-enforced invariants).
	proxyEnvs := []corev1.EnvVar{
		{Name: "HTTP_PROXY", Value: p.HTTPProxy},
		{Name: "HTTPS_PROXY", Value: p.HTTPSProxy},
		{Name: "NO_PROXY", Value: p.NoProxy},
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

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: rg.Namespace,
			Labels: map[string]string{
				labelManagedBy:   managerName,
				labelRunnerGroup: rg.Name,
				labelPlanID:      safeName(podName), // use pod name fragment as stable label
				// "actions-gateway/component: workload" matches the workload NetworkPolicy
				// podSelector so worker egress is restricted to the per-tenant proxy only.
				"actions-gateway/component": "workload",
			},
		},
		Spec: template.Spec,
	}

	if priorityClass != "" {
		pod.Spec.PriorityClassName = priorityClass
	}

	return pod
}

// waitForPodCompletion polls until the pod reaches a terminal phase.
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
