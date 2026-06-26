# docs/operations/

Operator-facing references. Audience: on-call SRE and platform engineers running the Gateway Manager Controller (GMC) and per-tenant Actions Gateway Controllers (AGCs).

| Doc | Personas | Purpose |
|---|---|---|
| [runbook.md](runbook.md) | SRE | Production runbook — high-level operational procedures. For initial setup see [Getting Started](../getting-started.md). |
| [backup-restore.md](backup-restore.md) | SRE, Platform engineer | Backup posture (GitOps + etcd) and a recovery runbook for restoring a deleted or corrupted `ActionsGateway` CR. |
| [troubleshooting.md](troubleshooting.md) | SRE, Platform engineer | Symptom → diagnosis → remediation, organised by observable failure mode. |
| [observability.md](observability.md) | SRE, Platform engineer | Prometheus metrics reference for GMC, AGC, and proxy, including standard `controller-runtime` metrics. |
| [security-operations.md](security-operations.md) | SRE, Security | Abuse-detection runbook — maps the threat model's abuse heuristics to operator alerts and compromise-response playbooks. |
| [tenant-onboarding.md](tenant-onboarding.md) | Platform engineer | Step-by-step checklist for onboarding a new tenant team. |
| [service-mesh-coexistence.md](service-mesh-coexistence.md) | Platform engineer, SRE | Running GAG alongside Istio/Linkerd/ambient — injection opt-out, sidecar lifecycle, and egress exclusions so the per-tenant proxy is honored. |
| [migration-from-arc.md](migration-from-arc.md) | Platform engineer | Coming from Actions Runner Controller (ARC) scale-set mode — concept mapping, behavioral differences, and a worked one-runner-group migration. |
| [install.md](install.md) | Platform engineer | Install the GMC with the `actions-gateway` Helm chart — prerequisites, digest pinning, healthy-install verification, uninstall. |
| [upgrade.md](upgrade.md) | Platform engineer | Upgrade and rollback procedures. Strategy intent lives in [§2.6 of the architecture doc](../design/02-architecture.md#26-upgrade-strategy). |
| [release.md](release.md) | Maintainer | How to cut a release: tag → publish (build, push, keyless-sign, SBOM-attest) → verify → record digests → bump the chart. |
| [../design/08-glossary.md](../design/08-glossary.md) | All | Canonical definitions for project terms (GMC, AGC, ActionsGateway, RunnerGroup, broker protocol identifiers). |

When adding a new failure mode an operator might observe, add a section to [troubleshooting.md](troubleshooting.md).
