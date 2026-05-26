# Documentation Plan

## Status at a glance

Last refreshed 2026-05-25. Most of Phase 1 and Phase 3 has shipped; a
handful of small Phase 2 cross-references remain.

| # | Item | File | Status |
|---|---|---|---|
| 1.1 | Troubleshooting guide | [docs/operations/troubleshooting.md](../operations/troubleshooting.md) | ✅ Done |
| 1.2 | Production runbook | [docs/operations/runbook.md](../operations/runbook.md) | ✅ Done |
| 1.3 | Upgrade and rollback | [docs/operations/upgrade.md](../operations/upgrade.md) | ✅ Done |
| 2.1 | Fix `maxEvictionRetries` inconsistency | `docs/design/03-api-contracts.md §3.1` | ✅ Done — fields now in CRD spec (lines 364, 382) |
| 2.2 | Failure paths in operational flows | `docs/design/04-operational-flows.md §4.3` | ✅ Done — provisioning failure, broker error, eviction retry sections added |
| 2.3 | Worked examples in capacity-planning appendix | `docs/design/appendix-e-capacity-planning.md` | ❌ Open — no example scenarios present |
| 2.4 | Expand observability.md (alerts, symptoms, cardinality, ServiceMonitor) | [docs/operations/observability.md](../operations/observability.md) | ✅ Done |
| 2.5 | Credential rotation in getting-started | `docs/getting-started.md:101` | ✅ Done |
| 2.6 | `DefaultWorkerImage` note in API contracts | `docs/design/03-api-contracts.md` | ❌ Open — constant not mentioned |
| 2.7 | HPA silent-failure callout on ProxyConfig | `docs/design/03-api-contracts.md` | ❌ Open — `requests.cpu`-required note missing from `ProxyConfig` |
| 2.8 | Reading-path guide in design README | `docs/design/README.md` | ✅ Done |
| 3.1 | Network architecture doc | [docs/design/network-architecture.md](../design/network-architecture.md) | ✅ Done |
| 3.2 | Alerting and dashboards doc | `docs/operations/alerting.md` | ❌ Open — file does not exist |
| 3.3 | Tenant onboarding checklist | [docs/operations/tenant-onboarding.md](../operations/tenant-onboarding.md) | ✅ Done |
| 3.4 | Cost modeling appendix | [docs/design/appendix-f-cost-model.md](../design/appendix-f-cost-model.md) | ✅ Done |
| X | Token-refresh alerting threshold cross-link | `docs/design/07-test-plan.md §7.1` | ✅ Done |
| X | Appendices C, E in design README TOC | `docs/design/README.md` | ✅ Done |
| X | `observability.md` linked from design README | `docs/design/README.md` | ✅ Done |
| X | `getting-started.md` linked from design README | `docs/design/README.md` | ✅ Done |

### Open work (priority order)

1. **2.7** — HPA silent-failure callout. Highest operator-safety value
   for a one-paragraph change.
2. **2.6** — `DefaultWorkerImage` discoverability. Small.
3. **2.3** — Capacity-planning worked examples. Medium effort.
4. **3.2** — Alerting/dashboards reference. Largest remaining item;
   defer until a real Prometheus setup exists to source from.

---

## Current state

The design doc suite (16 files) covers architecture, API contracts, security, testing, and implementation planning well. Critical gaps are in production operations: there is no troubleshooting guide, no production runbook, and no upgrade/disaster recovery procedures. Several existing docs have incomplete failure-path coverage and lack worked examples. A handful of cross-reference and API contract inconsistencies need to be resolved before the system goes into production use.

---

## Audience map

Understanding who reads what determines scope and priority.

| Audience | Entry point | Primary needs |
| --- | --- | --- |
| Platform engineer (initial setup) | `getting-started.md` | Deployment steps, validation, GitHub App wiring |
| Tenant team | `getting-started.md` | CR authoring, RunnerGroup sizing, sharding guidance |
| On-call SRE | *(missing)* | Troubleshooting, metric → symptom mapping, runbooks |
| Architect / reviewer | `docs/design/README.md` | Architecture rationale, alternatives, API contracts |
| Security engineer | `docs/design/05-security.md` | Threat model, RBAC boundaries, incident response |
| Executive / budget owner | `docs/design/01-executive-summary.md` | Cost justification, risk, delivery timeline |

---

## Phase 1 — Fill critical operational gaps

These are blocking for production readiness. None of this content exists anywhere.

### 1.1 Troubleshooting guide — `docs/operations/troubleshooting.md`

Audience: on-call SRE, platform engineer.

Cover the following failure modes as named sections, each with: symptoms, likely cause, diagnostic steps (commands), resolution:

- GMC not provisioning tenant resources (CR accepted, nothing created)
- AGC CrashLoopBackOff or not acquiring jobs
- Worker pods stuck `Pending`
- Proxy pool not scaling (HPA silently not working — note the `resources.requests.cpu` requirement for HPA metric computation)
- `RateLimited` condition on `ActionsGateway` (> 10 minutes → on-call page)
- GitHub App Secret misconfiguration (`privateKey` format errors, wrong `appId`/`installationId`)
- Token refresh errors (`actions_gateway_token_refresh_errors_total` spiking)
- `RenewJob` failures (`actions_gateway_renewjob_errors_total` rising)
- Network connectivity failures (AGC cannot reach GitHub through proxy)
- `Evicted` worker pods exhausting retry budget

Also include a section: "How to validate a fresh deployment is healthy" — a checklist of commands and expected outputs a platform engineer can run immediately after deploying.

### 1.2 Production runbook — `docs/operations/runbook.md`

Audience: on-call SRE.

Sections:

- **Day-2 operations** — adding a tenant, adjusting quota, scaling `maxListeners`, rotating GitHub App credentials
- **Alerting** — which metrics to alert on, recommended thresholds (map to Appendix A SLO targets), whether an alert is page-worthy or ticket-worthy
- **SLO breach response** — what to do when `pod_creation_latency_seconds p95 > 15s`, when `active_sessions` flatlines, when `jobs_acquired_total` stops incrementing
- **Incident response** — GitHub App key compromise (isolate, rotate, re-apply), AGC total failure (manual job requeue steps), GMC total failure (what state persists vs. is lost)
- **On-call handoff checklist**

Reference `docs/operations/observability.md` and `docs/design/appendix-a-capacity-slos.md` rather than duplicating tables.

### 1.3 Upgrade and rollback procedures — `docs/operations/upgrade.md`

Audience: platform engineer.

The upgrade strategy is described in `docs/design/02-architecture.md §2.6` but only as intent. This doc translates that into steps:

- Pre-upgrade validation checklist
- Step-by-step: GMC upgrade (CRD migration, operator rollout)
- Step-by-step: AGC upgrade (per-tenant, single-replica drain behavior)
- Step-by-step: Proxy upgrade (HPA-aware rolling update)
- Post-upgrade validation
- Rollback procedure for each component
- Zero-downtime configuration (note: single-replica AGC means a brief drain window; document it)

---

## Phase 2 — Improve existing docs

These are not blocking but reduce clarity and correctness of what already exists.

### 2.1 Fix the `maxEvictionRetries` inconsistency

`docs/design/02-architecture.md §2.2` describes `maxEvictionRetries` per RunnerGroup. `docs/design/03-api-contracts.md §3.1` RunnerGroupSpec does not include this field. One of these is wrong. Resolve it:

- If the field exists in code: add it to the CRD spec in §3.1 with type, default, and semantics.
- If it doesn't exist yet: remove the reference from §2.2 or mark it as planned.

### 2.2 Add failure paths to `docs/design/04-operational-flows.md`

The two sequence diagrams cover only the happy path. Add a third diagram or annotated variants covering:

- Provisioning failure (GMC cannot create RBAC → condition set, retry behavior)
- Job acquisition failure (broker returns error → session loop behavior)
- Worker pod eviction (AGC detects `Evicted` → stop renewal → call rerun API)
- AGC crash mid-job (GitHub redelivery window, what the tenant observes)

### 2.3 Add worked examples to `docs/design/appendix-e-capacity-planning.md`

The decision tree and formulas are good but abstract. Add two or three concrete scenarios:

- "Team with 3 RunnerGroups, 20 concurrent GPU jobs at peak" — what to set for `maxListeners`, `maxWorkers`, `namespaceQuota`, and when to shard
- "CPU-only team, 100 jobs/day, bursty (10 concurrent max)" — minimal configuration
- "Large tenant hitting 250-session ceiling" — sharding walkthrough

### 2.4 Expand `docs/operations/observability.md`

Current state is a one-page metrics list. Add:

- **Alert rules** — recommended Prometheus alerting rules for the key metrics (threshold, `for` duration, severity)
- **Symptom → metric mapping** — "jobs are slow": check `pod_creation_latency_seconds`; "jobs randomly cancelled": check `renewjob_errors_total`; etc.
- **Metric cardinality note** — label dimensions are `namespace` + `runner_group`; warn about label explosion if runner group names are generated dynamically
- **How to access metrics** — port (`:8080/metrics` default), whether auth is required, how to scrape in common setups (Prometheus operator `ServiceMonitor`)

### 2.5 Add credential rotation to `docs/getting-started.md`

Currently the getting-started doc only covers initial setup. Add a section: "Rotating GitHub App credentials" — update the Secret in-place and describe what the AGC does (detects Secret change, refreshes token on next interval vs. requires restart).

### 2.6 Note the `DefaultWorkerImage` constant in `docs/design/03-api-contracts.md`

§3.1 mentions it is a compile-time constant but gives no way to discover or override it. Add: the name of the constant, where it is defined, and how to override it (build flag or env var, whichever applies).

### 2.7 Fix the HPA silent-failure note in `docs/design/03-api-contracts.md`

Add a callout in ProxyConfig: if `resources.requests.cpu` is unset or zero, the HPA cannot compute utilization and will silently stop scaling. This is the most dangerous misconfiguration and currently undocumented.

### 2.8 Add a reading-path guide to `docs/design/README.md`

Below the TOC, add: "Reading paths by role" — four bullet points mapping architect / operator / security engineer / developer to the recommended reading order for their concerns. Also add Appendices A–E to the main TOC (currently only B and D appear there).

---

## Phase 3 — New reference material

These expand coverage for production and compliance use. Lower urgency but high value once the system is in active use.

### 3.1 Network architecture — `docs/design/network-architecture.md`

A topology diagram and narrative covering:

- Which components initiate outbound connections (AGC, worker pod, proxy) and to which GitHub endpoints
- Which connections are in-cluster only
- How `NetworkPolicy` rules implement the boundary (with example policies or reference to generated output)
- DNS resolution strategy (in-cluster DNS vs. external)
- How to validate network isolation is enforced (commands to test from tenant namespace)

This fills the gap noted in §2 and §5 where network isolation is described but never diagrammed.

### 3.2 Alerting and dashboards — `docs/operations/alerting.md`

Separate from the metric reference in `observability.md`:

- Recommended Prometheus alerting rules (YAML, ready to apply)
- Grafana dashboard JSON or description of panels
- SLO recording rules (for burn-rate alerting against Appendix A targets)
- Runbook links per alert (back to `runbook.md`)

### 3.3 Tenant onboarding checklist — `docs/operations/tenant-onboarding.md`

A checklist form covering:

- Pre-conditions (namespace exists, GitHub App registered, quota approved)
- Steps (Secret → CR → validation)
- Validation commands and expected outputs
- Common first-day mistakes
- Success criteria ("your first job has run successfully when…")

### 3.4 Cost modeling — `docs/design/appendix-f-cost-model.md`

Audience: budget owners, platform team leads.

- Per-job cost breakdown (AGC goroutine time, proxy pod time, worker pod GPU/CPU time)
- Comparison: cost per 1,000 jobs under this system vs. ARC (tie back to §1 numbers)
- Tenant cost allocation methodology
- Cost optimization levers (proxy autoscaling aggressiveness, worker resource sizing)

---

## Cross-cutting fixes (no new files)

These should be done alongside whichever phase they touch.

| Issue | Location | Fix |
| --- | --- | --- |
| `actions_gateway_token_refresh_errors_total` threshold for alerting not defined | `docs/design/07-test-plan.md §7.1` | Define threshold (e.g. > 1/hr per Appendix A) or link to new `alerting.md` |
| `docs/plan/` reference links broken (§2.2, §7.2 reference milestone docs by path) | `docs/design/02-architecture.md`, `07-test-plan.md` | Verify paths match actual files; update or remove dead links |
| `observability.md` not referenced from `docs/design/README.md` | `docs/design/README.md` | Add to TOC under "Operations" heading |
| `getting-started.md` not referenced from `docs/design/README.md` | `docs/design/README.md` | Add "Getting started" link at top |
| Appendices C, E not in `docs/design/README.md` TOC | `docs/design/README.md` | Add both to the TOC table |
| README does not mention `docs/operations/observability.md` | `README.md` | The Observability section already links there — confirm it's correct after other changes |

---

## Prioritized delivery order

| Priority | Item | Effort | Blocking |
| --- | --- | --- | --- |
| 1 | Troubleshooting guide (1.1) | Large | Production readiness |
| 2 | Fix `maxEvictionRetries` inconsistency (2.1) | Small | API correctness |
| 3 | Fix HPA silent-failure note (2.7) | Small | Operator safety |
| 4 | Production runbook (1.2) | Large | On-call readiness |
| 5 | Upgrade/rollback procedures (1.3) | Medium | Operational safety |
| 6 | Add failure paths to operational flows (2.2) | Medium | Design completeness |
| 7 | Expand observability.md (2.4) | Medium | Monitoring |
| 8 | Add credential rotation to getting-started (2.5) | Small | Day-2 operations |
| 9 | Worked examples in appendix-e (2.3) | Medium | Operator usability |
| 10 | Fix `DefaultWorkerImage` note (2.6) | Small | Operator usability |
| 11 | Cross-cutting reference fixes | Small | Navigation |
| 12 | Reading-path guide in design README (2.8) | Small | Onboarding |
| 13 | Tenant onboarding checklist (3.3) | Medium | — |
| 14 | Network architecture doc (3.1) | Medium | — |
| 15 | Alerting and dashboards doc (3.2) | Large | — |
| 16 | Cost modeling appendix (3.4) | Medium | — |
