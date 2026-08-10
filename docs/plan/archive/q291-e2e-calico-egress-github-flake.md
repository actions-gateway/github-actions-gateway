# Q291 — e2e-calico egress-to-GitHub reachability flake

**Status:** open.
Mitigation (retry-budget widening) shipped; a clean soak on `main` is still required before closing.

## Symptom

Three `cmd/gmc/test/e2e` specs — all of which reach **real** api.github.com / github.com — red the whole `e2e-calico` leg together, in an identical set, while the cluster and Calico bring-up come up clean (49 Passed | 3 Failed | 6 Skipped):

- `E2E_V2_MultiGateway_ProxyConnectWorks` ([v2_multigateway_test.go](../../../cmd/gmc/test/e2e/v2_multigateway_test.go)) — `curl: (56) CONNECT tunnel failed, response 502`, `HTTP_CODE=000`.
- `E2E_GMC_TenantProvisioning_ProxyConnectWorks` ([provisioning_test.go](../../../cmd/gmc/test/e2e/provisioning_test.go)) — same 502.
- `E2E_V2_DirectEgress_ReachesGitHub` ([direct_egress_test.go](../../../cmd/gmc/test/e2e/direct_egress_test.go)) — `curl: (28) Connection timed out after 30002 milliseconds` ×3.

## Recurrence

Not a one-off.
Identical failure set on two `main` runs a week apart:

| Date | Run | Trigger | Result |
|---|---|---|---|
| 2026-07-04 | 28712311067 | push | 49 Passed / 3 Failed / 6 Skipped — same 3 specs |
| 2026-07-11 | 29162764494 | workflow_dispatch | 49 Passed / 3 Failed / 6 Skipped — same 3 specs |

(Two other `e2e-calico` reds in the same window were unrelated infra: run 28956589306 = "No space left on device"; run 28497797865 = "runner lost communication" — both CI-host-resource symptoms, see *Open question* below.)

This flake was never filed before: the Q256 Flake-watch row noted "the one red run failed only on unrelated egress-to-GitHub specs" but did not track it.

## Mechanism (verified against source, not just logs)

All three failures are the **same leg**: the workload's *outbound* hop to GitHub, not proxy readiness.

- The proxy's `/readyz` closes as soon as the **CONNECT listener binds** ([cmd/proxy/proxy.go](../../../cmd/proxy/proxy.go)); it does **not** gate on egress-to-GitHub working.
  So a `WaitForDeploymentReady` gate would not help — the proxy was already serving.
- The proxy emits **502 only when its own `net.DialTimeout` to api.github.com:443 fails** (`cmd/proxy`: `TestProxy_DialFailure`; never 504).
  A 502 therefore means the proxy is up but its upstream dial was dropped — a brief egress-dataplane window on the Calico lane **before Felix programs the new ipBlock rules**, or a genuine transient to GitHub.
- The direct-egress `(28)` timeout is the same window on the workload NP path (no proxy): three 30 s attempts = exactly the 90 s `--retry-max-time` budget, i.e. the budget was fully exhausted before the dataplane settled.

Both proxy-connect specs gate only on the NetworkPolicy **object** gaining GitHub `ipBlock` peers (populated by the IPRangeReconciler) — which does not imply Felix has **programmed** those rules into the dataplane.
Under CI load the programming/transient window outlasts the existing curl retry budget (60 s proxy-connect / 90 s direct), so the specs fire and give up too early.

## Mitigation (shipped)

Widen the **bounded** outbound-GitHub retry budget on all three specs, and the pod-phase ceiling to match.
This does **not** weaken any assertion: a genuine persistent proxy / NP / TLS regression still fails on every retry and still reds the spec (`--retry-all-errors` only adds bounded retries; it cannot turn a deterministic failure green).
It only gives a transient Felix-programming / external blip more room to self-heal.

| Spec | Before | After |
|---|---|---|
| both proxy-connect | `--retry 5 --retry-delay 2 --retry-max-time 60`, ceiling 2 min | `--retry 8 --retry-delay 2 --retry-max-time 150`, ceiling 4 min |
| direct-egress | `--retry 5 --retry-delay 2 --retry-max-time 90`, ceiling 3 min | `--retry 8 --retry-delay 2 --retry-max-time 150`, ceiling 4 min |

`--retry-max-time 150` + one final `--max-time 30` attempt ≤ 180 s worst case, comfortably inside the 4 min (240 s) pod-phase ceiling, so a real persistent failure still terminates well before the wait times out.

## Open question — is a retry-budget bump the root fix, or just a mitigation?

The two same-window infra reds (disk-full, runner-lost-comms) and the `FailedMount … failed to sync secret cache: timed out waiting for the condition` warnings on the proxy pod in run 29162764494 all point to **CI-host resource pressure** on the e2e-calico runner.
If the true cause is an overloaded runner (kubelet/apiserver cache-sync stalls that also slow Felix), widening the retry budget is a mitigation that papers over it rather than a root-cause fix.
Because the flake reproduces only intermittently (~2 sightings), **a single green CI run does not prove the fix** — keep this item open until `e2e-calico` soaks clean on `main` across several runs.
If it recurs after the budget bump, investigate runner resource headroom (Q286 Kata/dedicated-pool work may be the real lever) rather than widening the budget again.

## Not in scope

- Q256 (Calico BIRD/nodename bring-up) is a **separate** concern, fixed in PR #590; do not touch the bring-up script for this.
- Do not weaken what the specs assert (200/403-only, non-empty body, pod `Succeeded`).
