# Q264 — Remove the deprecated classic acquisition machinery

**Status:** RESIDUAL ONLY, tracked by [Q1272](../queue/Q1272.md).
Q264 removed classic acquisition from the AGC and the GMC; what this doc still tracks is the classic code outside those two binaries.
The scale-set migration (Option E) is fully shipped — phases **P0–P5 landed**, ScaleSet is the **default** acquisition protocol since v1.1.0, and Classic is **deprecated**.
The full P0–P5 record — protocol reverse-engineering, the Investigation E/E2 live probes, the U6–U8 decisions of record, and the P1–P5 execution narrative — lives in the archived phase narrative: [q264-scale-set-protocol-phases.md](archive/q264-scale-set-protocol-phases.md).

Q264 removed the classic tier from the two binaries that ran it: the AGC's agent pool, multiplexer and listener, its classic provisioning path, and every AGC and GMC read of `acquisitionProtocol` and `maxListeners`.
A `v2alpha1` set written `Classic` now runs as ScaleSet, and is checked by the scale-set label guard like any other; the field declarations leave with the `v2alpha1` package in Q1167.
The gate this doc used to describe, the deprecation window and the `v1alpha1` migration, both held: Q273 removed `v1alpha1` first.

## What the removal deletes, and what is left

The classic-protocol acquisition tier (full "Discarded" delta, with the *why* per component, is in [archive §3](archive/q264-scale-set-protocol-phases.md#discarded-the-classic-protocol-acquisition-tier)):

| Machinery | Where | Status |
|---|---|---|
| Classic broker client (`CreateSession`/`GetMessage`/`AcquireJob`/`RenewJob`/`CompleteJob`/`DeleteSession`) | [broker/client.go](../../broker/client.go) | [Q1272](../queue/Q1272.md) |
| Agent pool: N pre-registered JIT agents, per-agent Secrets, single-use recycle + heal ladder (Q114) | [cmd/agc/internal/agentpool/](https://github.com/actions-gateway/github-actions-gateway/blob/abb6fcd7f/cmd/agc/internal/agentpool/pool.go) | removed by Q264 |
| Multiplexer: `maxListeners` sessions, `SpawnReplacement`, poller accounting (Q152), planID `claimJob` dedup | [multiplexer.go](https://github.com/actions-gateway/github-actions-gateway/blob/abb6fcd7f/cmd/agc/internal/listener/multiplexer.go) | removed by Q264 |
| Listener goroutine: per-delivery `handleJob`, `StartRenewLoop` (Q247), `completeAbandonedDelivery` | [goroutine.go](https://github.com/actions-gateway/github-actions-gateway/blob/abb6fcd7f/cmd/agc/internal/listener/goroutine.go) | removed by Q264 |
| M3 pipes handoff: payload Secret → wrapper → `Runner.Worker spawnclient` | [cmd/worker/main.go](../../cmd/worker/main.go); the provisioner's payload staging | staging removed by Q264; the worker half is [Q1272](../queue/Q1272.md) |
| Q260 fan-out accounting model + tests | [broker/brokertest/server.go](../../broker/brokertest/server.go) | [Q1272](../queue/Q1272.md) |

Plus the **transitional API fields**: `acquisitionProtocol` and `maxListeners` on the v2alpha1 `RunnerSet`.
These exist **only on v2alpha1**, as the per-set canary/rollback lever P3–P4 needed (rollback had to be a field edit, not an AGC image downgrade). v2beta1 never served `Classic` — the graduation conversion strips both fields — so the end state is that the protocol is an API-version property (v1 = classic/deprecated, v2 = scale-set) with no enum surviving.
Decision of record: [archive §5a-U7](archive/q264-scale-set-protocol-phases.md#u7--where-the-protocol-selector-lives) and [§5a-U8](archive/q264-scale-set-protocol-phases.md#u8--support-matrix-policy).

## Why removing classic is safe now

Keeping classic forever re-imports the dual-protocol maintenance burden Option E exists to end.
Retiring it at cutover strands nobody: scale sets cover dotcom + all vendor-supported GHES (≥ 3.9; the floor excludes zero supported deployments).
P4's clean-green live validation (2026-07-05) confirmed the scale-set path handles the real CI matrix pristine — see [archive §6-P4](archive/q264-scale-set-protocol-phases.md#6-phased-execution-path-no-big-bang-rewrite).
No security property regresses (single-acquirer topology, same egress/isolation/NetworkPolicy).
