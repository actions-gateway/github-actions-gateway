# Six-Layer Documentation Audit

Bring the `docs/` set into line with the six-layer model of technical documentation
(terminology, cross-references, version/conditional logic, metadata/taxonomy,
navigation/hierarchy, reusable blocks). This is a **consistency audit plus small
fixes**, not a content-gap roadmap (that is [`docs.md`](docs.md), Phases 1–3, done)
and not a restructure.

## Status at a glance

| Layer | Concern | Status | Outcome |
|---|---|---|---|
| 1 | Terminology consistency | ✅ done | Glossary linked from all section READMEs; GMC/AGC expanded in appendices; rest left to glossary (decision recorded) |
| 2 | Cross-reference architecture | ✅ done | 0 broken file links; all 127 in-page anchors validated, 5 pre-existing breaks fixed; link-check CI gate delivered (Q52, `make doc-links`) |
| 3 | Version / conditional logic | ✅ done | **6 documented metrics reconciled with code (Q51)** — `pod_creation_latency_seconds`, `managed_gateways`, `ip_range_updates_total` implemented; `reconcile_errors_total` re-pointed to the controller-runtime built-in; `proxy_replicas` marked `(planned)` |
| 4 | Metadata / taxonomy | ✅ done | README indexes complete; k8s-audit plan added to plan index; no front matter (by decision) |
| 5 | Navigation / hierarchy | ✅ done | Added `docs/README.md` landing page + root link; no orphans; heading hierarchy clean |
| 6 | Reusable content blocks | ✅ done | `go test` list canonical in testing.md; human docs no longer link to CLAUDE.md; no partials mechanism (by decision) |

**All findings resolved.** The Layer 3 metrics gap was closed by Q51 (see
[q51-metrics-reconcile.md](q51-metrics-reconcile.md)). The optional Layer 2
link-check CI gate was subsequently delivered by Q52 (`scripts/check-doc-links.sh`,
slug-aware, wired into `make check`).

---

## Context: this is a framework-free docset

`docs/` is plain GitHub-native Markdown — no MkDocs/Sphinx/Docusaurus, no YAML front
matter, no include/partial mechanism, no versioned-docs tree. That is a deliberate
choice (renders natively on GitHub, single source of truth is git). It matters here
because three of the six layers assume framework machinery this repo intentionally
does not have.

**Decisions — what this audit will _not_ do** (each would add an abstraction the repo
deliberately avoids, against the CLAUDE.md "smallest change / no new patterns" rule):

- **No YAML front matter** (Layer 4). Nothing renders it; it would be dead metadata.
  The repo's real taxonomy is the per-directory `README.md` index tables.
- **No partials / transclusion mechanism** (Layer 6). GitHub cannot transclude.
  Framework-free reuse = one canonical home + a cross-link.
- **No versioned-docs tree or conditional markup** (Layer 3). Docs describe `main`.
  The analogue of "version" here is implemented-vs-planned status.

If any of these later becomes warranted (e.g. the docs move to a real site
generator), that is its own decision to raise explicitly — not part of this audit.

---

## Layer 1 — Terminology consistency · ⚠️

Source of truth: [`docs/design/08-glossary.md`](../design/08-glossary.md) — solid,
20+ terms, `GMC`/`AGC` used consistently across the set.

Tasks:

1. **Per-page acronym first-use** — ✅ scoped & partially applied. The audit found
   `GMC`/`AGC` used without an in-file expansion in the low-traffic appendices
   (A, B, D, E, G) and in the core design docs (03–07, network-architecture,
   appendices C/F), plus standard Kubernetes acronyms (`HPA`, `CRD`, `RBAC`, `PDB`,
   `PSA`, `CNI`, `SLO`) across ~20 files.
   - **Applied:** expanded `GMC`/`AGC` at first use in appendices A, B, D, E, G.
   - **Decision (do not pursue further):** leave the core design docs and the
     standard Kubernetes acronyms as-is. The design suite is read sequentially with
     `GMC`/`AGC` expanded in 01/02, every term is defined in
     [`08-glossary.md`](../design/08-glossary.md), and the glossary is now linked from
     every section README (task 2). Mass-expanding the flagship docs is high churn for
     low marginal value to a Kubernetes-operator audience. Revisit only if the docs
     move to a publishing system where pages are indexed/served individually.
2. **Glossary discoverability** — ✅ done. Linked from
   [`docs/development/README.md`](../development/README.md) and
   [`docs/operations/README.md`](../operations/README.md).
3. **`AGC`-rename fallout** — ✅ checked. The archived
   [`archive/rename-agc-to-controller.md`](archive/rename-agc-to-controller.md) was an
   on-cluster *resource-name* rename (`actions-gateway-agc` → `actions-gateway-controller`),
   not a rename of the `AGC` acronym; the glossary definition still stands and docs use
   `AGC` consistently.
4. **Casing consistency** for protocol terms (`Broker` vs `broker`, `Run Service`,
   `sessionId`, `planId`) against the glossary's chosen forms — spot-checked, no
   systematic drift found.

---

## Layer 2 — Cross-reference architecture · ✅

Cross-references use relative `.md` paths plus `§X.Y` anchors and breadcrumb prev/next
nav in the design docs. After fixes: 0 broken file links and 0 broken in-page anchors
across the doc set.

Tasks:

1. **Fix the one broken link** — ✅ done. `docs/plan/archive/milestone-2-tests.md`
   linked to `milestone-2.md` (moved up a directory) → `../milestone-2.md`. Full
   relative-`.md`-link scan: 0 broken project links.
2. **Anchor spot-check** — ✅ done (full check, not just spot). Validated all 127
   in-page `#anchor` links against the GitHub heading-slug algorithm; found and fixed
   5 pre-existing broken anchors: 3 in `design/README.md` (TOC pointed at richer text
   than the actual `01-executive-summary.md` subheadings), 1 in `02-architecture.md`
   (`§11.A` said `investigation`, heading is `protocol`), and 1 in `security.md` (the
   `worker-egress-proxy.md` "Implementation status" heading carried a volatile
   date+commit — stabilized the heading so the anchor is durable). Now 0 broken anchors.
3. **Link-check CI gate — delivered (Q52).** A slug-aware markdown link/anchor check (`scripts/check-doc-links.sh`, run by `make doc-links` in `make check`). Originally framed as a lightweight markdown-link-check step in
   `unit-test.yml` would fit the repo's recent CI-gate culture (the lint gate just
   landed). Flagged as optional — decide separately; do not bundle.

---

## Layer 3 — Version / conditional logic · ✅ N/A by design

No product versioning; docs describe `main`. The repo's analogue is the
implemented-vs-planned distinction, which CLAUDE.md explicitly warns about
("investigation findings marked ✅ must be end-to-end verified, not source-read").

Task:

1. **Implemented-vs-planned prose audit** — ✅ audited; one significant finding.
   The highest-signal check was cross-referencing every documented Prometheus metric
   name against the metric literals actually registered in non-test Go
   (`cmd/proxy/proxy.go`, `cmd/agc/internal/listener/metrics.go`). **15 metrics are
   implemented; the docs reference 6 that have no code definition at all** — operator
   docs document telemetry an operator cannot scrape.

   | Documented metric | Originally implemented? | Resolution (Q51) |
   |---|---|---|
   | `actions_gateway_pod_creation_latency_seconds` | ❌ no | **Implemented** — histogram observed in the AGC `InformerPodWaiter` (pod creation → runner container start) |
   | `actions_gateway_managed_gateways` | ❌ no | **Implemented** — GMC scrape-time collector listing `ActionsGateway` CRs |
   | `actions_gateway_reconcile_errors_total` | ❌ no — controller-runtime emits `controller_runtime_reconcile_errors_total` instead | **Re-pointed** — docs now reference the controller-runtime built-in |
   | `actions_gateway_ip_range_updates_total` | ❌ no | **Implemented** — GMC counter incremented on each successful NetworkPolicy patch |
   | `actions_gateway_proxy_replicas` | ❌ no | **Marked `(planned)`** — proxy HPA autoscaling (Milestone 5), plan-only |
   | `actions_gateway_proxy_tunnel_duration_seconds` | ✅ yes (M-17/M-18, 2026-05-31) | No change — already implemented |

   (`_bucket`/`_sum` suffixes on histograms are Prometheus-derived, not separate
   metrics — not counted as gaps.)

   **Resolved by Q51** — see [q51-metrics-reconcile.md](q51-metrics-reconcile.md)
   for the per-metric rationale. Docs and code now agree: every documented
   non-`(planned)` metric is registered, and the re-pointed name matches what
   controller-runtime actually emits.

   No other class of "described as shipped but not implemented" prose was found:
   design docs describe intended behavior in present tense (conventional for a design
   suite), and the milestone status lives in `docs/STATUS.md`.

---

## Layer 4 — Metadata / taxonomy · ⚠️ (no front matter)

The repo has no YAML front matter and will not gain any (see Decisions). The real
metadata layer is the per-directory `README.md` index — one row per doc with a
one-line description and (in design/ops) an audience.

Tasks:

1. **Index completeness sweep.** For each of `design/`, `development/`, `operations/`,
   `plan/` (and the `cmd/`, `test/`, `scripts/`, `tools/` READMEs): every doc present
   is listed, no listed doc is missing, and each one-line description still matches
   the doc's content.
2. **Resolve stragglers.** `k8s-best-practices.md` appears only in `docs/STATUS.md`,
   not in `plan/README.md`'s index — confirm intended and add a row if not.

---

## Layer 5 — Navigation / information hierarchy · ⚠️ (real gap)

Source of truth: per-directory `README.md` indexes, design-doc breadcrumbs, and the
role-based reading paths in `design/README.md`.

Tasks:

1. **Add `docs/README.md` landing page** — the highest-value single fix. `docs/` root
   holds `getting-started.md`, `STATUS.md`, and four subdirs with nothing tying them
   together. Add a top-level index: one line per subdir + the two root docs, plus the
   role-based entry points (mirror, don't duplicate, the `design/README.md` paths).
2. **Orphan check.** Confirm every doc is reachable from some index — appendices,
   `network-architecture.md` (un-numbered, sits outside the breadcrumb sequence),
   `k8s-best-practices.md`.
3. **Heading hierarchy + case sweep.** Confirm each design doc uses `#` title / `##`
   sections consistently and heading case (sentence vs title) is uniform within the
   set.

---

## Layer 6 — Reusable content blocks · ✅ done

No include mechanism and none will be added (see Decisions). Framework-free reuse =
pick one canonical location and cross-link to it; do not paste-and-drift.

**Direction constraint (important):** human-facing docs must never link to `CLAUDE.md`.
`CLAUDE.md` (symlinked `AGENTS.md`) is the entrypoint *for Claude only*; humans start at
`README.md`. So a shared block lives canonically in the `docs/` tree, and `CLAUDE.md`
may hold its own self-contained copy or link *to* the human doc — never the reverse.

Outcome:

1. The per-module `go test` command list appeared in both `CLAUDE.md` and
   [`docs/development/testing.md`](../development/testing.md). Resolved by moving the
   command block into a new **Running tests** section in `testing.md` (the canonical
   human home) and replacing the `CLAUDE.md` copy with a link *to* `testing.md` —
   single source of truth, in the correct direction (`CLAUDE.md` → docs). (An earlier
   pass had `testing.md` link *to* `CLAUDE.md`; reverted, as it inverted the direction.)
2. Other human docs that linked to `CLAUDE.md` were redirected to human docs:
   `docs/operations/README.md` (dropped the doc-update-checklist link) and
   `CONTRIBUTING.md` (now points at `docs/design/05-security.md`).
3. No other drift-prone duplicates worth factoring (the keychain command and AGC/GMC
   definitions each have a single home).

---

## Prioritized delivery order

| # | Task | Layer | Size |
|---|---|---|---|
| 1 | Add `docs/README.md` landing page | 5 | S |
| 2 | Fix broken archived-doc link | 2 | S (1 line) |
| 3 | Terminology audit + link glossary from dev/ops READMEs | 1 | S |
| 4 | README-index completeness/accuracy sweep | 4 | S |
| 5 | Orphan + heading-consistency sweep | 5 | S |
| 6 | Implemented-vs-planned prose audit | 3 | M |
| 7 | De-duplicate drifting blocks | 6 | S |
| 8 | link-check CI gate — ✅ delivered (Q52, `make doc-links`) | 2 | S |

Terminology renames (task 3) go in their own commit, separate from prose edits, so
the rename is reviewable as one diff. Navigation changes ship together with the page
move that requires them.
