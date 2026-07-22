# Kubernetes API conventions

Project-specific conventions for the Kubernetes surface we author: label and
annotation keys/values, status conditions, Events, pod shutdown behaviour — and
the gotchas that have bitten us. Read this before adding a new label,
annotation, or CRD field that an operator sets by hand, or before writing the
shutdown path of any binary that runs in a pod.

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

### Recommended `app.kubernetes.io/*` labels (Q205)

Every object the GMC or AGC creates also carries the Kubernetes
[recommended labels](https://kubernetes.io/docs/concepts/overview/working-with-objects/common-labels/)
— `app.kubernetes.io/{name,instance,component,part-of,managed-by}` (and `version`
where a meaningful one exists) — stamped via the shared
[`api/apilabels`](../../api/apilabels/labels.go) helper so the GMC and AGC never
diverge on the keys or the `part-of` value. They are **additive metadata**: stamp
them *alongside* the functional selector labels above, never in place of them, and
never build a controller's pod/Service selector on them (an operator may relabel
them). `apilabels.Merge` preserves any existing key, so it cannot clobber a
selector label. The canonical per-component values and operator `kubectl -l`
recipes live in [observability.md](../operations/observability-metrics.md#selecting-gag-objects-with-the-recommended-labels).

Controller-set annotations on worker pods (both v1 and v2, stamped by the
provisioner at pod creation time from the AcquireJob payload):

- `actions-gateway.com/run-id` — GitHub workflow run ID.
- `actions-gateway.com/repository` — `owner/repo` the job belongs to.
- `actions-gateway.com/job-name` — job name as defined in the workflow YAML.
- `actions-gateway.com/workflow` — workflow file name.

These are best-effort: absent if the GitHub payload omitted the corresponding
`system.github.*` variable. Never use them for security enforcement — they are
informational annotations for operator visibility.

The provisioner also gap-fills three **node-disruption-safety** annotations on
every worker pod, so a node autoscaler or the descheduler does not evict a pod
mid-job and strand the CI run (these are third-party well-known keys, not our
`actions-gateway.com/` domain):

- `karpenter.sh/do-not-disrupt: "true"` — Karpenter consolidation/drift opt-out.
- `cluster-autoscaler.kubernetes.io/safe-to-evict: "false"` — Cluster Autoscaler scale-down opt-out.
- `descheduler.alpha.kubernetes.io/prefer-no-eviction: "true"` — descheduler opt-out (current well-known key; the older `…/evict` is opt-*in* only).

Gap-fill only: a value for any of these keys set in the runner's
`podTemplate.metadata.annotations` wins (mirroring the SecurityContext gap-fill).
Only these three keys are honored from the template; arbitrary `podTemplate`
annotations are not copied onto worker pods. The markers live on the pod, so they
release automatically when the pod is torn down on job completion. Operator-facing
detail in [observability.md](../operations/observability-metrics.md#node-disruption-safety-annotations).

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
  `RunnerGroup` (AGC). See [Q82](../plan/archive/quota-pressure-conditions.md).

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

## Graceful shutdown (SIGTERM)

Every binary we ship runs in a pod, so every one of them gets SIGTERM on each
rollout, node drain, eviction, and scale-down. Getting this wrong is quiet: the
process exits cleanly, the rollout looks green, and the damage shows up
elsewhere — as leaked GitHub-side sessions, as CI jobs whose network was cut
mid-request, as jobs GitHub waits out rather than being told about.

Read this before writing or changing any shutdown path.

### The lifecycle facts the rules below follow from

- **Endpoint removal is concurrent with SIGTERM, not ordered before it.** Marking
  the pod terminating, removing it from EndpointSlices, and the kubelet starting
  its shutdown are independent control loops; none waits for the others. A pod
  can therefore receive SIGTERM while kube-proxy, an ingress, or a mesh sidecar
  is still routing new connections to it.
- **`preStop` runs before SIGTERM**, and the time it takes is **deducted from**
  `terminationGracePeriodSeconds` — it is not extra budget.
- **SIGTERM goes to PID 1 of each container only.** Child processes are not
  signalled. At grace expiry SIGKILL goes to every process in the cgroup.
- **The grace period is a deadline, not an allowance.** Anything still running
  when it expires is killed outright, mid-write.

### Rules

**1. A process must not exit while work it owns is unfinished.**

"Work it owns" is anything the outside world is still counting on: an open
upstream session, an in-flight request, an unreported job result, a held lease.
Enumerate it explicitly for each binary — the failure mode is always something
nobody thought to enumerate.

**2. With controller-runtime, `mgr.Start` only waits for what the manager
knows about.** Goroutines you spawn yourself from `Reconcile` are invisible to
it: `mgr.Start` returns, `main` returns, and the process exits out from under
them. Register the drain as a `manager.Runnable` that blocks on the manager
context and then waits for your goroutines, so it runs inside the manager's
graceful shutdown:

```go
func (s *listenerShutdown) Start(ctx context.Context) error {
    <-ctx.Done()
    <-s.stop() // returns a done channel; blocks until every goroutine has exited
    return nil
}

// Run on every replica, not just the leader: a replica that has lost leadership
// still owns the goroutines it spawned.
func (s *listenerShutdown) NeedLeaderElection() bool { return false }
```

This is what Q222 got wrong — the AGC leaked a GitHub-side session per in-flight
listener on every rollout.

**3. Cleanup that runs *because* of cancellation must not run *on* the cancelled
context.** A teardown `DELETE`/`PATCH`/report issued on the context that was just
cancelled fails instantly, and "best-effort, error discarded" then means "never
happened". Use a context detached from the cancelled one, with its own bound:

```go
ctx, cancel := context.WithTimeout(context.Background(), cleanupBudget)
defer cancel()
```

Bound it, retry inside the bound (a teardown call is usually the *only* one that
work will ever get), and log loudly with the resource's identity when you give
up — a silent leak is unactionable. Q222 shipped both halves of this.

**4. `http.Server.Shutdown` does not wait for hijacked connections.** From the
stdlib: *"Shutdown does not attempt to close nor wait for hijacked connections
such as WebSockets. The caller of Shutdown should separately notify such
long-lived connections of shutdown and wait for them to close."* Any CONNECT
tunnel, WebSocket, or upgraded stream needs its own tracking (a `WaitGroup` or a
connection set, plus `Server.RegisterOnShutdown`) — `Shutdown` returning is not
evidence they finished.

The egress proxy is the worked example (Q384). `cmd/proxy` registers each tunnel
with a tracker **before** hijacking the client connection — up to the hijack
net/http still counts the connection as active and `Shutdown` waits for it, so
registering first is what closes the window in which `Shutdown` could return
between a hijack and its registration and see an empty tracker. Shutdown then
waits on the tracker *after* `Shutdown` returns, by which point the listener is
closed and the tracked set can only shrink.

**5. If your PID 1 supervises a child, forward the signal.** The child never gets
SIGTERM on its own. A wrapper that `exec.Command`s a real workload must catch
SIGTERM, forward it, and wait — otherwise the child runs on until the cgroup
SIGKILL with no chance to report its outcome. Our worker wrapper does this in
`relayTerminationSignals` ([cmd/worker/main.go](../../cmd/worker/main.go)): it
forwards SIGTERM/SIGINT to `Runner.Worker` (or `run.sh`), waits for it inside a
bounded grace (`WORKER_SHUTDOWN_GRACE`, default 25s), and kills it with a logged
reason if it overruns. Propagate the child's exit code, including the
128+signal encoding for a signalled child — `os.ProcessState.ExitCode` reports
-1 there, and `os.Exit(-1)` silently becomes 255.

**6. Anything serving traffic through a Service needs a `preStop` sleep.**
Because of the concurrency in the first bullet above, SIGTERM is not a signal
that traffic has stopped arriving. A short `preStop` sleep (a few seconds, no
process cooperation required) lets endpoint removal propagate before the process
starts refusing work. Size `terminationGracePeriodSeconds` as
`preStop + drain budget + headroom`.

**Use the native `sleep` handler, not `exec: ["sleep", …]`.** Our images are
distroless: there is no shell and no `sleep` binary, so an exec hook fails at
runtime and the pod proceeds straight to SIGTERM — reintroducing the race, but
silently. The native handler (KEP-3960) is beta and on by default from
Kubernetes 1.30, which is the project's blocking install floor, so every
supported cluster honours it.

The egress proxy is the worked example (Q386). Both proxy builders share one set
of constants in [cmd/gmc/internal/controller/builder.go](../../cmd/gmc/internal/controller/builder.go),
stated as arithmetic rather than a literal so the manifest's claim stays checkable
against the code that has to fit inside it:

```go
proxyTerminationGracePeriodSeconds = proxyPreStopSleepSeconds + // 10s: endpoint removal propagates
    proxyDrainBudgetSeconds +                                   // 45s: Q384 tunnel drain deadline
    proxyDrainTailSeconds +                                     //  7s: force-close unwind + health shutdown
    proxyExitHeadroomSeconds                                    // 13s: process exit, kubelet jitter
```

Note the drain budget is **mirrored**, not imported — `cmd/proxy` is a separate
Go module in the workspace. A unit test asserts the mirror and the arithmetic, so
raising one side without the other fails rather than letting SIGKILL land
mid-drain and quietly undo Q384.

**7. State the budget in the manifest comment, and keep the code inside it.**
`terminationGracePeriodSeconds` is a claim about how long shutdown takes; if the
code's drain is unbounded, or the comment describes a drain the code doesn't
perform, the two silently diverge. Prefer a bounded drain whose worst case you
can name over `context.Background()` with no deadline.

### Review checklist

For any binary that runs in a pod:

- [ ] What does this process own that the outside world is waiting on? Is each
      item drained before exit?
- [ ] Does anything wait for the goroutines this process spawns, or does `main`
      just return?
- [ ] Does teardown run on a context detached from the cancelled one, bounded,
      and retried?
- [ ] Are hijacked/upgraded connections tracked separately from
      `http.Server.Shutdown`?
- [ ] If there is a child process, is SIGTERM forwarded to it?
- [ ] Does the pod serve traffic through a Service (⇒ needs `preStop`)?
- [ ] Is `terminationGracePeriodSeconds` ≥ `preStop` + the worst-case drain, and
      does its comment match what the code actually does?
- [ ] Is there a test that cancels the context and asserts the cleanup happened
      — not merely that the process exited?

That last one is the one that catches regressions. A shutdown test asserting
only "it exited" passes against every bug on this page.
