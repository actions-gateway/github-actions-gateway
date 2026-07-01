# Warn on Reap-Blocking Worker Sidecars

> **Status: ❌ Open — planned.** Tracked as [Q249](../STATUS.md#Q249). Discovered
> during the GKE-dogfood privileged-DinD e2e validation (2026-06-30): a
> `docker:dind` sidecar declared as a **regular** container kept the worker pod
> at `1/2` after the runner exited, so the AGC counted it as an active session
> and `maxWorkers` saturated — the [Q247](../STATUS.md#Q247) stranding symptom,
> reproduced deterministically. This surfaces that misconfiguration at admission
> + status + metrics and steers operators to native sidecars. **No custom reaper.**

## Goal

Detect when a worker `RunnerTemplate` / `ClusterRunnerTemplate` carries a
**regular (non-native) sidecar** that can prevent the worker pod from completing
— and surface it as a **non-blocking warning**, a **status condition**, and a
**metric**, with a per-sidecar opt-out. The intent is to *train operators toward
native sidecars*, not to enforce or to build a reaper.

## Background — why this, not a reaper

A GAG worker pod is one "main" runner container plus, optionally, sidecars. If a
sidecar is a **regular** `spec.containers[]` entry that runs forever (e.g.
`dockerd` for DinD), the pod never reaches `Completed` — a pod terminates only
when **all** its regular containers exit. The pod lingers `1/2`, the AGC keeps
counting it active, and the runner pool strands.

**The upstream fix already exists — we rely on it, not on a reaper.** Native
sidecars ([KEP-753](https://github.com/kubernetes/enhancements/tree/master/keps/sig-node/753-sidecar-containers)):
a `restartPolicy: Always` **init container** that Kubernetes tears down when the
main container exits, so the pod completes on its own. Beta / on-by-default in
**1.29**, GA in **1.33**. GAG should adopt this (in its own injected wrapper) and
steer operators to it; a bespoke pod-reaper would be solving upstream's problem
the hard way.

**Why static detection can't be precise.** Nothing in a pod spec says a container
"runs forever" (`dockerd` never exits; `busybox true` exits at once). So the
check is necessarily a **heuristic policy** — "a regular, non-runner container may
block reaping" — which is why it must be a **warning**, not an error, with an
opt-out.

## Prerequisite (verify; may split out)

The upstream mechanism only works if GAG doesn't break it: the AGC must
(a) inject its own long-running wrapper/sidecars as **native** sidecars, and
(b) **preserve** a `restartPolicy: Always` init container an operator authored,
through the Q235 wrapper injection (which already manipulates
`initContainers` / OCI image volumes — see
[archive/q235-worker-wrapper-injection.md](archive/q235-worker-wrapper-injection.md)).
Confirm both; if the AGC reorders or strips the field, fix that first — it is the
load-bearing part.

## Design — one detection, three outlets, one opt-out

**Detection (shared helper):** a template has a regular container besides the
runner whose name is **not** in the acknowledgment list. Reused by the webhook
and the reconciler so the verdict is identical everywhere.

**Outlets — all gated by the same opt-out**, so an alert never fires on a
sidecar the operator has declared self-exiting:

| Outlet | Role | Fires |
|---|---|---|
| **Admission warning** (validating webhook, non-blocking) | the **teacher** | at `kubectl apply` — when the config is written |
| **Status condition** (`PossibleReapBlockingSidecar=True` on the RunnerSet) | ongoing visibility + alert source | reconcile |
| **Metric** (gauge, labelled by namespace/template) | fleet alerting / dashboard | scrape |

**Opt-out = the training lever.** A per-template annotation naming the
sidecars the operator asserts exit cleanly:

```yaml
metadata:
  annotations:
    actions-gateway.com/self-exiting-sidecars: "metrics-agent,log-shipper"
```

- **Name-list, not a boolean** — a *newly added* unacknowledged sidecar still
  warns; a blanket `true` would let the next footgun through silently.
- Silences **all three** outlets for the named sidecars.
- Forces an **acknowledge-or-fix** decision: the warning can't be silenced
  without either converting to a native sidecar or consciously asserting "this
  one exits." Either way the operator has engaged with the concept — that is the
  training. Same shape as the existing `allow-profile-downgrade` /
  `privileged-profile` acknowledgment pattern.

**Warning, not error.** The heuristic is undecidable, so a hard block would
punish legitimate self-exiting sidecars. Admission *warnings* (`kubectl` prints
`Warning:` and proceeds) are the idiomatic non-blocking nudge.

## Deliverables

1. Detection helper (`identify regular non-runner containers not in the
   acknowledgment annotation`) + the annotation constant + godoc.
2. Validating webhook: emit an admission **Warning** on `RunnerTemplate` /
   `ClusterRunnerTemplate` create/update (extend the existing validators).
3. RunnerSet **status condition** `PossibleReapBlockingSidecar`, set/cleared by
   the reconciler from the *resolved* template.
4. **Metric** (gauge; e.g. `actions_gateway_reap_blocking_sidecar_templates`).
5. Docs: operator guidance in
   [in-runner-image-builds.md](../operations/in-runner-image-builds.md) /
   [kata-dind-workloads.md](../operations/kata-dind-workloads.md) — the
   native-sidecar requirement (K8s ≥1.29) + the acknowledgment annotation; a
   design note in [appendix-b-worker-isolation.md](../design/appendix-b-worker-isolation.md).
6. Tests: webhook warning emitted / suppressed by the annotation; condition
   set + cleared; metric; a native sidecar (`restartPolicy: Always` init
   container) is **not** flagged; the runner-only case is not flagged.

## Scope — in / out

**In:** detection + warning + condition + metric + annotation opt-out + docs +
tests.

**Out (separate items):**
- **AGC native-sidecar preservation** (the prerequisite above) — verify; fix in
  its own change if broken.
- **Un-clean-session GC backstop** — a pod left behind by a *crash* or a
  *superseded/orphaned* job (not a misconfig) strands regardless of sidecar
  shape. That belongs to **[Q247](../STATUS.md#Q247)**; the fix there is light
  GC (owner-reference / TTL / reconcile-time cleanup of pods whose session is
  gone), still **not** a bespoke reaper.

## Open questions

- Condition home: RunnerSet (operational, references the resolved template) vs
  `RunnerTemplate.status`. Lean RunnerSet.
- Metric shape: a gauge of templates-with-unacknowledged-sidecars vs a counter
  of pods observed lingering. The gauge is cheaper and config-oriented; start
  there.
- Should the runner container be identified by name/convention or by "the one
  the AGC gap-fills"? Reuse whatever the injection path already uses so the
  webhook and AGC agree on which container is the runner.

## Testing

`make check` + GMC envtest (webhook warning emission + annotation gating,
condition set/cleared, metric) — no live cluster needed; the behavior is
admission- and reconcile-level. The end-to-end reaping itself is a Kubernetes
guarantee (native sidecars) and is exercised incidentally by the dogfood e2e.
