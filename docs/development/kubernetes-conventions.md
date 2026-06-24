# Kubernetes API conventions

Project-specific conventions for the Kubernetes surface we author: label and
annotation keys/values, and the gotchas that have bitten us. Read this before
adding a new label, annotation, or CRD field that an operator sets by hand.

## Label & annotation value conventions

### Don't use boolean-looking values for string-matched labels/annotations

When a label or annotation value is **matched as a string** by our code (an
admission webhook, a controller, a `ValidatingAdmissionPolicy`), use an explicit
**enum keyword** — e.g. `allowed`, `enabled`, `managed` — never a
boolean-looking value (`true`, `false`, `yes`, `no`, `on`, `off`).

Why:

- **YAML coercion footgun.** In a manifest, `my-label: true` parses as a YAML
  boolean, not the string `"true"`. A Kubernetes label/annotation value must be
  a string, so the unquoted form either errors or has to be remembered as
  `"true"` (quoted) every time. YAML 1.1 coerces `yes`/`no`/`on`/`off` (and
  their capitalised variants) the same way, so the trap is wider than just
  `true`/`false`.
- **Self-documenting.** `actions-gateway.github.com/privileged-profile: allowed`
  reads as a deliberate grant. `…: "true"` carries no meaning and invites the
  reader to drop the quotes.

The value is always matched **exactly** and the check is **fail-closed**: any
value other than the sentinel keyword (and an absent label) is treated as "not
granted". So even if someone fat-fingers `true`, eligibility is denied rather
than silently granted.

**Worked example.** The privileged-profile eligibility gate (Q133) uses

```yaml
metadata:
  labels:
    actions-gateway.github.com/privileged-profile: allowed   # not "true"
```

See `PrivilegedProfileLabel` / `PrivilegedProfileAllowed` in
[`cmd/gmc/api/v1alpha1/actionsgateway_types.go`](../../cmd/gmc/api/v1alpha1/actionsgateway_types.go)
and [§5.3 of the security design](../design/05-security.md#privileged-eligibility-is-a-platform-decision).

**v2 operator-set label — namespace security profile.** v2 relocates the Pod
Security Admission level off the per-gateway `ActionsGateway.spec.securityProfile`
(v1) onto the **tenant namespace** (Q175 / appendix-h §H.16 #7): the operator sets

```yaml
metadata:
  labels:
    actions-gateway.com/security-profile: restricted   # baseline | restricted | privileged
```

on the namespace, and the GMC `NamespacePSAReconciler` stamps the
`pod-security.kubernetes.io/*` labels from it. The value follows the enum-keyword
convention above (not a boolean), and the `gmc-namespace-security-profile-guard`
ValidatingAdmissionPolicy fail-closes on an invalid value, a silent downgrade, or a
`privileged` selection without the `actions-gateway.com/privileged-profile=allowed`
eligibility label. See `SecurityProfileLabel` in
[`api/v2alpha1/shared_types.go`](../../api/v2alpha1/shared_types.go).

### Pre-existing `"true"` values are grandfathered

Two shipped keys predate this convention and still use `"true"`:

- `actions-gateway.github.com/tenant: "true"` — the managed-tenant marker label.
- `actions-gateway.github.com/allow-profile-downgrade: "true"` — the
  downgrade opt-in annotation.

These were **not** to be changed casually. The `tenant` marker in particular is
load-bearing: the `namespace-psa-guard` and `gmc-tenant-resource-guard`
`ValidatingAdmissionPolicy` objects, the onboarding scripts, and operator
runbooks all match it as `"true"`, so changing the value is a breaking change to
deployed clusters. The convention above applies to **new** keys; the existing
two stay as-is unless there is a separate, deliberate migration.

**The v2 API cutover is that deliberate migration.** v2 aligns both values to
self-documenting keywords (`tenant: managed`, `allow-profile-downgrade: allowed`)
on the renamed `actions-gateway.com/` domain (see `shared_types.go`). During the
v1/v2 coexistence window every consumer **dual-reads** both spellings, so deployed
clusters are not broken mid-cutover; the [M5 migration tool](../operations/migration-v1-to-v2.md)
relabels live namespaces additively, and the legacy `"true"` arms drop when
`v1alpha1` is removed (design [§H.12](../design/appendix-h-v2-api-decomposition.md#h12-folding-in-the-grandfathered-label-value-alignment-q147)).

## Label & annotation key conventions

Use the `actions-gateway.github.com/<name>` prefix for every label and
annotation key the project defines, matching the API group. Define the key (and
its sentinel value, if any) as an exported `const` in the relevant
`api/v1alpha1` package with godoc, and reference that const from controllers,
webhooks, and tests — never re-type the literal string, so a rename can't drift
between the producer and the consumers.

**v2 (`actions-gateway.com`) keys use the owned domain from birth** — the v2
kinds and their controllers prefix labels/annotations with `actions-gateway.com/`
(the group the project owns), defined as exported consts in the neutral `api/v2alpha1`
package. Controller-set v2 labels:

- `actions-gateway.com/gateway: <name>` — stamped by the v2 `ActionsGateway`
  reconciler on every AGC control-plane child (Deployment/SA/RoleBinding/Service/
  NetworkPolicy/Secret), so M3b's per-gateway naming has an identity to key on and
  operators can `kubectl get -l actions-gateway.com/gateway=<name>` a gateway's
  resources.
- `actions-gateway.com/runner-set: <name>` (`provisioner.LabelRunnerSet`) — stamped
  on every v2 worker pod and job Secret; the AGC `RunnerSet` controller's Pod watch
  and reaper filter on it. Distinct from the v1 `actions-gateway/runner-group` key so
  the v1 and v2 controllers' Pod watches never cross-wire during coexistence.

The shared `actions-gateway/component: workload` selector label is carried by both
v1 and v2 worker/AGC pods (it backs the workload NetworkPolicy selector), so the
egress-lockdown posture is identical across the two APIs.

Controller-set annotations on worker pods (both v1 and v2, stamped by the
provisioner at pod creation time from the AcquireJob payload):

- `actions-gateway.com/run-id` — GitHub workflow run ID.
- `actions-gateway.com/repository` — `owner/repo` the job belongs to.
- `actions-gateway.com/job-name` — job name as defined in the workflow YAML.
- `actions-gateway.com/workflow` — workflow file name.

These are best-effort: absent if the GitHub payload omitted the corresponding
`system.github.*` variable. Never use them for security enforcement — they are
informational annotations for operator visibility.

## Status conditions & alertable condition metrics

The CRDs report observed state with standard Kubernetes conditions
(`metav1.Condition`, keyed by `type`, surfaced via `kubectl describe`). Two
conventions keep them consistent and alertable.

### Two-tier "pressure / exceeded" ladder for capacity signals

When a controller surfaces pressure against a finite resource (e.g. the
namespace `ResourceQuota`), model it as a **two-tier ladder** rather than one
boolean, so operators can route a *warning* and a *page* differently:

- **`<Subject>QuotaPressure`** — *warning*. Predictive: the subject cannot grow
  to its configured ceiling within the **remaining** headroom (`hard − used`).
  This is load-dependent and may flap; alert on it with an `for:` debounce and
  do **not** page.
- **`<Subject>QuotaExceeded`** — *error*. Observed/imminent: creates are being
  rejected now, or no headroom remains for even one more unit. Page-worthy
  (still use `for:` to debounce).

Rules:

- **Polarity is abnormal-is-`True`** (matching `CredentialUnavailable`,
  `RateLimited`) — `True` means there is a problem.
- **The tiers are mutually exclusive**: when the error fires, force the warning
  to `False` (reason `Superseded`). Each condition then maps to exactly one
  alert severity with a plain `== True` rule and no Alertmanager inhibition.
- **Advisory unless stated**: a capacity condition does not gate `Ready` unless
  the subject is actually unavailable — surfacing a latent problem must not flip
  a healthy workload to not-ready.
- Shipped examples: `ProxyQuotaPressure`/`ProxyQuotaExceeded` on the
  `ActionsGateway` (GMC) and `WorkerQuotaPressure`/`WorkerQuotaExceeded` on the
  `RunnerGroup` (AGC). See [Q82](../plan/quota-pressure-conditions.md).

### Mirror alertable conditions as a controller-exported gauge

Every condition an operator should alert on is **also** exported as a Prometheus
gauge by the owning controller (`1` when the condition is `True`, `0`
otherwise), labelled by namespace + object name. This lets clusters alert
directly on the controller's `/metrics` endpoint **without depending on
kube-state-metrics** to scrape CRD conditions.

Implement it as a **scrape-time collector** that lists the CRs from the cached
reader and reads `.status.conditions` (see `proxyQuotaCollector` in `cmd/gmc`
and `workerQuotaCollector` in `cmd/agc`), not a reconcile-path gauge: a deleted
object simply stops being listed, so its series disappears with no stale-series
cleanup and no reconcile cost. Metric names mirror the condition
(`actions_gateway_proxy_quota_pressure`, `actions_gateway_worker_quota_exceeded`,
…).

## Kubernetes Events for lifecycle transitions

Controllers emit Kubernetes Events (via a controller-runtime `EventRecorder`) on
the owning CR for incident-worthy lifecycle transitions, so operators see them in
`kubectl describe` and event watchers — not only in metrics/conditions. Conventions
that keep them consistent and non-spammy:

- **`Reason` is PascalCase and stable** — it is a machine-matchable key operators
  filter on (`kubectl get events --field-selector reason=<X>`), so treat it like an
  API surface: don't rename it casually. Where an Event corresponds to a Prometheus
  counter, **mirror the metric name** in the `Reason` (e.g. the
  `actions_gateway_eviction_retries_exhausted_total` metric ↔ the
  `EvictionRetriesExhausted` Event) so the two correlate at a glance.
- **`Warning` vs `Normal` by severity** — `Warning` for a failure/abnormal terminal
  outcome, `Normal` for a benign transition.
- **Emit on transitions / terminal outcomes, never every reconcile** — an Event is
  an incident signal, not a heartbeat. Where a status condition already captures the
  steady state, the Event *complements* it (records the transition) rather than
  re-emitting on every requeue.
- **Record on the most useful object** — the owning CR an operator would
  `kubectl describe` (the reaper, and the Q170 job-lifecycle Events, record on the
  `RunnerGroup`/`RunnerSet`; the message names the affected pod/run/job).
- **Route deep-goroutine events back through the reconciler** — a listener or
  provisioner goroutine does not hold the live owner object the `EventRecorder`
  needs, so it pushes the event onto a buffered channel (non-blocking; drop on full)
  that the reconciler drains and records on the live object — mirroring the existing
  condition-update channel. The drain consumes each event once, so it is not
  re-emitted on later reconciles.
- Shipped examples: `WorkerPodStuckPending` (reaper), and the Q170 job-lifecycle set
  (`JobAcquisitionFailed`, `RunnerVersionTooOld`, `SessionUnauthorized`,
  `QuotaRetriesExhausted`, `EvictionRetriesExhausted`). The operator-facing catalogue
  lives in [troubleshooting.md](../operations/troubleshooting.md#job-lifecycle-events-on-a-runnergroup--runnerset).
