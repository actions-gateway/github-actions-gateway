# 6. Implementation Phasing & Delivery Milestones

← [Security](05-security.md) | [Back to index](README.md) | Next: [Test Plan →](07-test-plan.md)

---

The system is delivered across five milestones over roughly five weeks. Each milestone produces a deliverable and a verifiable success criterion; later milestones build on the artifacts of earlier ones (the probe binary becomes the AGC's polling implementation, the decrypted payload becomes the test fixture for the worker pod, and so on). Operators who prefer to leverage AI-assisted implementation can consult [Appendix C](appendix-c-ai-implementation.md) for prompting guidance and a discussion of the trade-offs — that material is optional and orthogonal to the milestone structure itself.

```
Phase 1: API Probe    Phase 2: AGC Core      Phase 3: Worker     Phase 4: GMC + Proxy  Phase 5: Harden & Ship
[Days 1-4]            [Days 5-10]            [Days 11-16]        [Days 17-22]          [Days 23-26]
- Wire protocol       - RunnerGroup CRD      - Pipe wrapper      - ActionsGateway CRD  - Security policies
- Auth + decrypt      - Goroutine loop       - Dockerfile        - GMC reconciler      - Multi-tenant load
- Broker fixtures     - AGC CRUD safe        - E2E smoke test    - Proxy + HPA deploy  - 1000-session burst
```

---

## Milestone 1: Wire Protocol Probe (Days 1–4)

* **Deliverable:** A standalone Go binary under `cmd/probe/` that runs the full pre-execution sequence: authenticate via GitHub App credentials → `POST /sessions` → long-poll `GET /message` → `POST /acquirejob` on the `run_service_url` extracted from the message body → start a `renewjob` loop every 60 seconds. The probe prints the decrypted job payload to stdout and continues renewing until cancelled. Decrypted payload is committed as a fixture under `testdata/`.
* **Success Criteria:** The probe acquires a real job and renews its lock at least three times without GitHub cancelling it. The committed payload becomes the ground-truth fixture for all subsequent integration tests.
* **Investigation — `AcknowledgeRunnerRequest`:** The official runner source (`MessageListener.cs`) calls `AcknowledgeRunnerRequestAsync(runnerRequestId, sessionId)` after handing a job to the worker. This endpoint is not documented and its role is unclear — it may be the broker API's replacement for the old `DeleteMessage` call, or a no-op acknowledgment. The probe should attempt this call after a successful `acquirejob` and observe whether omitting it causes any downstream issue (e.g., the same job being redelivered, or session errors). If confirmed necessary, add it to the execution flow in [§4.2](04-operational-flows.md#42-job-execution-flow-agc) and [§3.3](03-api-contracts.md#33-re-implemented-broker-api-endpoints).
* **Risk investigation — egress IP variance:** Before finishing this milestone, route the probe's broker API calls through a local two-pod proxy pool (two `httptest`-backed `CONNECT` proxies bound to different ports, simulating different egress IPs) and verify that `sessions`, `message`, `acquirejob`, and `renewjob` calls all succeed when each call lands on a different proxy. If any call fails or returns an unexpected status, pause and evaluate before proceeding to the proxy pool design in Milestone 4 — the fallback options are `sessionAffinity: ClientIP` on the proxy Service (low effort) or per-goroutine proxy assignment (higher fidelity).

* **Investigation — session reuse after `acquirejob`:** The adaptive listener model in [§2.2](02-architecture.md#22-tier-2--actions-gateway-controller-agc) requires that a goroutine can call `GET /message` again on the same `sessionId` immediately after a successful `POST /acquirejob`. Using the probe, acquire a job and then — without calling `DELETE /sessions` — immediately re-enter the `GET /message` long-poll on the same session. Confirm whether GitHub accepts the renewed poll and eventually delivers a second job, or returns a session error (e.g. 404, 410, or a protocol-level rejection). If session reuse is not permitted, the AGC must call `DELETE /sessions` and `POST /sessions` between each job, adding registration latency to every acquisition cycle. Document the observed behavior in [§3.3](03-api-contracts.md#33-re-implemented-broker-api-endpoints) and adjust the Session Multiplexer implementation plan in Milestone 2 accordingly.

* **Investigation — job delivery throttling by session count:** Confirm whether GitHub's broker delivers jobs only to sessions that were registered *before* the job was queued, or whether it will deliver to any session that starts polling after the job arrives. To test: queue two jobs simultaneously with only one session registered, then register a second session mid-queue and observe whether both sessions receive a job. If GitHub throttles delivery to the registered session count at queue time, the adaptive spawn-on-acquire model may miss jobs during bursts (a job arrives while the replacement listener's `POST /sessions` call is in flight). If GitHub delivers opportunistically to any ready session, the model is safe. If throttling is confirmed, evaluate pre-spawning a small warm standby pool (2–3 sessions per RunnerGroup) as a mitigation, and update [Appendix E](appendix-e-capacity-planning.md) accordingly.

---

## Milestone 2: AGC Controller & Reconciler (Days 5–10)

* **Deliverable:** A deployable AGC scaffolded with `controller-runtime`/`kubebuilder` that reconciles `RunnerGroup` CRs into adaptive listener goroutine pools: one permanent goroutine at rest, spawning additional goroutines on demand up to `maxListeners` during bursts, with idle goroutines shutting down once the queue drains. Includes the Token Manager (mutex-protected installation token with T-5min proactive refresh), the per-job RenewJob loop, the `maxWorkers` simple pod-count ceiling (enforced without PriorityClass when `priorityTiers` is absent), and the polling implementation lifted from the Milestone 1 probe. Unit tests cover create/update/delete lifecycle transitions, goroutine spawn/kill/leak detection (via `goleak.VerifyNone`), idle-shutdown triggering after 50 consecutive empty 202 responses, and clock-based assertions that token refresh fires before expiry without interrupting in-flight goroutines.
* **Success Criteria:** Creating, scaling `maxListeners`, and deleting a `RunnerGroup` in a local `kind` cluster produces no goroutine leaks (verified via `pprof` and `goleak`) and no orphaned Kubernetes resources. The goroutine count at rest is exactly one per RunnerGroup regardless of `maxListeners` setting. Token refresh completes without goroutines restarting.

---

## Milestone 3: Worker Pod & Pipe Handoff (Days 11–16)

* **Deliverable:** A worker container image plus the pod-provisioning logic in the AGC: Dockerfile, entrypoint wrapper (Go binary that reads the mounted payload Secret and writes to Named Pipes), Secret mount logic, and the `AcquireJob` → pod-create handoff sequence. The Named Pipe handoff is the underdocumented part of this milestone — start by validating the wrapper with the static decrypted payload from Milestone 1 before wiring it into pod creation, so the pipe semantics can be debugged without a live GitHub trigger in the loop.
* **Success Criteria:** A real workflow job dispatched from GitHub appears in the Actions UI with correct step output, timing, and a green checkmark. The worker container exits with code `0` on success, and both the pod and the job Secret are garbage-collected by the AGC.

---

## Milestone 4: Gateway Manager Controller + Proxy (Days 17–22)

* **Deliverable:** A second operator (`cmd/gmc/`) sharing the repo with the AGC, reconciling `ActionsGateway` CRs into the full tenant resource set: ServiceAccount, Role, RoleBinding, NetworkPolicy, ResourceQuota, proxy Deployment, proxy Service, PodDisruptionBudget, HPA, AGC Deployment, and bootstrap RunnerGroups. Includes a minimal stateless Go `CONNECT` proxy implementation (HTTPS tunneling only, no TLS termination), the admission webhook that rejects CRs in reserved namespaces, and `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY` injection into both the AGC Deployment and the worker pod template. RBAC test enumerates the GMC's generated ClusterRole rules and asserts no `*` verbs on `secrets`, `pods`, or `nodes` (regression guard against accidental escalation).
* **Success Criteria:** Applying two `ActionsGateway` CRs in a `kind` cluster produces two independent tenant namespaces, each with a running AGC and a proxy pool with at least `minReplicas` Ready. Deleting one CR tears down only that tenant's resources. Updating `spec.proxy.maxReplicas` causes the HPA to reflect the new bound within one reconcile cycle.

---

## Milestone 5: Hardening & Load Testing (Days 23–26)

* **Deliverable:** Production Helm chart or Kustomize overlays with locked-down Pod Security Standards, per-tenant ResourceQuotas, optional gVisor `RuntimeClass` configuration (see [Appendix B](appendix-b-worker-isolation.md)), hardened proxy pod specs (read-only root filesystem, no capabilities), and a multi-tenant load harness under `test/load/` that simulates multiple tenants in parallel against a staging cluster with the full GMC+AGC stack running.
* **Success Criteria:** 1,000 concurrent virtual runner sessions across 10 simulated tenants sustain a burst load with zero dropped messages, no cross-tenant resource visibility, and no deadlocked Go channels. The proxy HPA scales up under load and returns to `minReplicas` within 5 minutes of load subsiding. A `kube-bench` or `polaris` scan returns no critical findings.

---

← [Security](05-security.md) | [Back to index](README.md) | Next: [Test Plan →](07-test-plan.md)
