# docs/operations/

Operator-facing references. Audience: on-call SRE and platform engineers running the Gateway Manager Controller (GMC) and per-tenant Actions Gateway Controllers (AGCs).

| Doc | Audience | Purpose |
|---|---|---|
| [runbook.md](runbook.md) | On-call SRE | Production runbook — high-level operational procedures. For initial setup see [Getting Started](../getting-started.md). |
| [troubleshooting.md](troubleshooting.md) | On-call SRE, platform engineer | Symptom → diagnosis → remediation, organised by observable failure mode. |
| [observability.md](observability.md) | SRE, platform engineer | Prometheus metrics reference for GMC and AGC, including standard `controller-runtime` metrics. |
| [tenant-onboarding.md](tenant-onboarding.md) | Platform engineer | Step-by-step checklist for onboarding a new tenant team. |
| [upgrade.md](upgrade.md) | Platform engineer | Upgrade and rollback procedures. Strategy intent lives in [§2.6 of the architecture doc](../design/02-architecture.md#26-upgrade-strategy). |
| [../design/08-glossary.md](../design/08-glossary.md) | All | Canonical definitions for project terms (GMC, AGC, ActionsGateway, RunnerGroup, broker protocol identifiers). |

When adding a new failure mode an operator might observe, add a section to [troubleshooting.md](troubleshooting.md).
