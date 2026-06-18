# Plan: Single-source chart webhook + remaining roles (Q143)

← [STATUS](../STATUS.md) · extends [Q142](drop-kustomize.md)

## Goal

Extend the Q142 single-source + drift-gate pattern (CRDs, manager-role) to the
chart resources the plan left as a follow-up: the **validating webhook config**
and the **agc-tenant-role**. Eliminate every remaining hand-copy that can drift
from its authoritative source, with **zero change to the rendered RBAC/webhook**
(no permission change).

## Ground truth (verified before designing)

The Q143 Queue note (and the drop-kustomize "Out of scope" bullet) assumed all
four resources still have a `config/` duplicate to generate from. They don't —
slice C of Q142 already removed most of them:

| Resource | Live `config/` source? | Other duplicate today | Action |
|---|---|---|---|
| Validating webhook | **Yes** — `cmd/gmc/config/webhook/manifests.yaml` (controller-gen; also loaded by the GMC integration suite via envtest `WebhookInstallOptions`) | chart `templates/webhook.yaml` (hand-derived) | **Generate the chart copy from config + drift gate** |
| agc-tenant-role | No (`config/agc-tenant-role` removed in slice C) | a drifted Go mirror in `suite_integration_test.go` | **Shared `files/` fragment read by chart + test** |
| metrics-auth / metrics-reader | No (removed in slice C) | none | Already single-sourced (chart is sole copy) — document, no-op |
| leader-election | No (removed in slice C) | none | Already single-sourced — document, no-op |

Two important safety facts:

- **agc-tenant-role must NOT be generated from the AGC markers**
  (`cmd/agc/config/rbac/role.yaml`, ClusterRole `agc-role`). That role grants
  `create`/`delete` on `runnergroups` and `patch` on `secrets`; the deployed
  agc-tenant-role deliberately withholds them (least privilege — AGCs reconcile
  RunnerGroups, they don't create/delete them). Generating from the markers
  would be a **privilege escalation** — forbidden by "secure by default". The
  chart's hand-authored role is authoritative; we single-source it *structurally*
  (one file, two readers) rather than from controller-gen.
- The Go mirror `installAGCTenantClusterRole` had already **drifted** from the
  chart (it granted `watch` on secrets and omitted `update` + the events rules) —
  so the RBAC-scope integration test was exercising a different role than ships.
  Sharing one fragment fixes that and prevents recurrence.

metrics/leader-election have exactly one copy each (the chart), no controller-gen
generator exists for these standard scaffolding roles, and no test mirrors them —
so there is nothing to single-source. Adding a generator with a single reader
would be indirection without drift protection.

## Changes

### A. Webhook — full-file generator + drift gate (the CRD precedent)

- `scripts/sync-chart-webhook.sh` transforms `cmd/gmc/config/webhook/manifests.yaml`
  into `charts/actions-gateway/templates/webhook.yaml`, re-injecting the chart's
  Helm templating (name prefix + labels, the `certManager.enabled` CA-inject
  annotation, `namespace: {{ .Release.Namespace }}`, and the non-cert-manager
  caBundle block). The webhook *body* (admissionReviewVersions, failurePolicy,
  name, rules, sideEffects, service path) is copied verbatim — that is the
  marker-generated, drift-prone content.
- `make chart-webhook` writes it; `make chart-webhook-check` (`--check`) renders
  to a temp file and diffs against the committed chart copy.
- Wired into `make check` and `make manifest-validate`; `manifest-validate.yml`
  picks it up via `make manifest-validate` and gains the script to its
  paths-filter.

### B. agc-tenant-role — shared rules fragment

- `charts/actions-gateway/files/agc-tenant-role-rules.yaml` holds the policy
  rules (with their explanatory comments), reproduced byte-for-byte from today's
  chart template — the authoritative copy.
- `templates/agc-tenant-role.yaml` embeds it via `.Files.Get | nindent 0`
  (mirrors `manager-role` embedding `manager-role-rules.yaml`).
- `installAGCTenantClusterRole` in the GMC integration suite reads and unmarshals
  the same fragment instead of hardcoding rules — eliminating the drifted mirror.

### C. metrics / leader-election — documented no-op

Already single-sourced post-slice-C; recorded here and in drop-kustomize.md.

## Verification

- `helm template` (cert-manager on **and** off) byte-identical before/after for
  the webhook and agc-tenant-role resources.
- `make check` + `make chart-webhook-check` + `helm lint`/`template` green.
- GMC integration suite (envtest) green — the RBAC-scope test now runs against
  the real shipped role.
