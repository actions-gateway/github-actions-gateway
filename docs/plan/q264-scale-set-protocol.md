# Q264 — Remove the deprecated classic acquisition machinery

**Status:** RESIDUAL ONLY.
The scale-set migration (Option E) is fully shipped — phases **P0–P5 landed**, ScaleSet is the **default** acquisition protocol since v1.1.0, and Classic is **deprecated**.
The full P0–P5 record — protocol reverse-engineering, the Investigation E/E2 live probes, the U6–U8 decisions of record, and the P1–P5 execution narrative — lives in the archived phase narrative: [q264-scale-set-protocol-phases.md](archive/q264-scale-set-protocol-phases.md).

This doc now tracks only the **open residual** ([Q264](../queue/Q264.md)): the terminal PR that removes the classic acquisition machinery and the transitional API fields.
It is the last step of Option E's retirement ladder — one isolated PR, gated on two independent conditions.

## The gate (both must hold)

1. **The one-minor classic deprecation window from v1.1.0 elapses** (i.e. v1.2.0 ships).
   ScaleSet became the default and Classic was deprecated in v1.1.0's release notes, which starts the window.
2. **v1alpha1 tenants have migrated off** via `gag-migrate` ([Q273](../queue/Q273.md)).
   Classic is v1alpha1's *only* acquisition path, so removing it necessarily ends v1alpha1's ability to acquire jobs — the classic deprecation window **is** the v1alpha1 migration window.

The removal was originally expected to ride the Q74 v2beta1 graduation hop (that hop is where the transitional fields were to be stripped). v2beta1 has since graduated **without** carrying the removal, so it is no longer bundled — it is now the terminal step, gated on the two conditions above.
When both hold, one isolated PR does the removal.
Sequence it **after** v1alpha1 is itself deprecated (tenants moved via `gag-migrate`); announcing the two deprecations together turns the fan-out-free model into the concrete incentive to complete the v1→v2 migration.

## What the removal PR deletes

The classic-protocol acquisition tier (full "Discarded" delta, with the *why* per component, is in [archive §3](archive/q264-scale-set-protocol-phases.md#discarded-the-classic-protocol-acquisition-tier)):

| Machinery | Where |
|---|---|
| Classic broker client (`CreateSession`/`GetMessage`/`AcquireJob`/`RenewJob`/`CompleteJob`/`DeleteSession`) | [broker/client.go](../../broker/client.go) |
| Agent pool: N pre-registered JIT agents, per-agent Secrets, single-use recycle + heal ladder (Q114) | [cmd/agc/internal/agentpool/](../../cmd/agc/internal/agentpool/pool.go) |
| Multiplexer: `maxListeners` sessions, `SpawnReplacement`, poller accounting (Q152), planID `claimJob` dedup | [multiplexer.go](../../cmd/agc/internal/listener/multiplexer.go) |
| Listener goroutine: per-delivery `handleJob`, `StartRenewLoop` (Q247), `completeAbandonedDelivery` | [goroutine.go](../../cmd/agc/internal/listener/goroutine.go) |
| M3 pipes handoff: payload Secret → wrapper → `Runner.Worker spawnclient` | [cmd/worker/main.go](../../cmd/worker/main.go), provisioner payload staging |
| Q260 fan-out accounting model + tests | [broker/brokertest/server.go](../../broker/brokertest/server.go) |

Plus the **transitional API fields**: `acquisitionProtocol` and `maxListeners` on the v2alpha1 `RunnerSet`.
These exist **only on v2alpha1**, as the per-set canary/rollback lever P3–P4 needed (rollback had to be a field edit, not an AGC image downgrade). v2beta1 never served `Classic` — the graduation conversion strips both fields — so the end state is that the protocol is an API-version property (v1 = classic/deprecated, v2 = scale-set) with no enum surviving.
Decision of record: [archive §5a-U7](archive/q264-scale-set-protocol-phases.md#u7--where-the-protocol-selector-lives) and [§5a-U8](archive/q264-scale-set-protocol-phases.md#u8--support-matrix-policy).

## Why removing classic is safe now

Keeping classic forever re-imports the dual-protocol maintenance burden Option E exists to end.
Retiring it at cutover strands nobody: scale sets cover dotcom + all vendor-supported GHES (≥ 3.9; the floor excludes zero supported deployments).
P4's clean-green live validation (2026-07-05) confirmed the scale-set path handles the real CI matrix pristine — see [archive §6-P4](archive/q264-scale-set-protocol-phases.md#6-phased-execution-path-no-big-bang-rewrite).
No security property regresses (single-acquirer topology, same egress/isolation/NetworkPolicy).
