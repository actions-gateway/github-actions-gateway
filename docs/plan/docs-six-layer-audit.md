# Six-Layer Documentation Audit

Bring the `docs/` set into line with the six-layer model of technical documentation
(terminology, cross-references, version/conditional logic, metadata/taxonomy,
navigation/hierarchy, reusable blocks). This is a **consistency audit plus small
fixes**, not a content-gap roadmap (that is [`docs.md`](docs.md), Phases 1–3, done)
and not a restructure.

## Status at a glance

| Layer | Concern | Status | Work |
|---|---|---|---|
| 1 | Terminology consistency | ⚠️ audit | Per-page acronym first-use; glossary discoverability; `AGC`-rename fallout |
| 2 | Cross-reference architecture | ✅ healthy | One broken link to fix; optional link-check CI gate |
| 3 | Version / conditional logic | ✅ N/A by design | Audit implemented-vs-planned prose; keep single-version |
| 4 | Metadata / taxonomy | ⚠️ audit | README index tables are the taxonomy — verify completeness; **no front matter** |
| 5 | Navigation / hierarchy | ⚠️ gap | No `docs/README.md` landing page; orphan + heading sweep |
| 6 | Reusable content blocks | ⚠️ audit | Canonical-home + cross-link for drifting duplicates; **no partials mechanism** |

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

1. **Per-page acronym first-use.** Each page is read standalone (no framework nav
   chrome), so the CLAUDE.md rule "spell out acronyms on first use" must hold *per
   page*, not once across the set. Audit `GMC`, `AGC`, `ARC`, `CR`, `CRD`, `HPA`,
   `PDB`, `PSA`, `NP`/`NetworkPolicy`, `SLO` first-use expansion in each doc.
2. **Glossary discoverability.** The glossary lives under `design/` — architects find
   it, operators and developers may not. Link it from
   [`docs/development/README.md`](../development/README.md) and
   [`docs/operations/README.md`](../operations/README.md).
3. **`AGC`-rename fallout.** An archived plan
   ([`archive/rename-agc-to-controller.md`](archive/rename-agc-to-controller.md))
   shows a past rename effort. `rg -i "\bcontroller\b"` and confirm no loose
   "controller" is used where "AGC" / "Actions Gateway Controller" is meant; confirm
   the glossary definition still matches code.
4. **Casing consistency** for protocol terms (`Broker` vs `broker`, `Run Service`,
   `sessionId`, `planId`) against the glossary's chosen forms.

---

## Layer 2 — Cross-reference architecture · ✅ (one fix)

A repo-wide relative-`.md`-link scan came back clean for project-owned docs except
for a single dead link. Cross-references use relative `.md` paths plus `§X.Y` anchors
and breadcrumb prev/next nav in the design docs.

Tasks:

1. **Fix the one broken link:** `docs/plan/archive/milestone-2-tests.md` links to
   `milestone-2.md`, which moved up a directory — should be `../milestone-2.md`.
2. **Anchor spot-check.** `§X.Y`-style anchor links break silently when a heading is
   renamed. Spot-check the high-traffic anchors in `02-architecture.md` and
   `03-api-contracts.md` referenced from other docs.
3. **(Optional) link-check CI gate.** A lightweight markdown-link-check step in
   `unit-test.yml` would fit the repo's recent CI-gate culture (the lint gate just
   landed). Flagged as optional — decide separately; do not bundle.

---

## Layer 3 — Version / conditional logic · ✅ N/A by design

No product versioning; docs describe `main`. The repo's analogue is the
implemented-vs-planned distinction, which CLAUDE.md explicitly warns about
("investigation findings marked ✅ must be end-to-end verified, not source-read").

Task:

1. **Implemented-vs-planned prose audit.** Scan design/architecture docs for prose
   that describes planned-but-unimplemented behavior as if shipped. Tag any
   aspirational "the X does Y" with a `(planned)` marker or move it under a clearly
   future-tense heading. Cross-check against `docs/STATUS.md` ⚠️ rows.

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

## Layer 6 — Reusable content blocks · ⚠️ (canonical-home pattern)

No include mechanism and none will be added (see Decisions). Framework-free reuse =
pick one canonical location and cross-link to it; do not paste-and-drift.

Tasks:

1. **De-duplicate drifting blocks.** Known candidates:
   - The per-module `go test` command list appears in both `CLAUDE.md` and
     [`docs/development/testing.md`](../development/testing.md). Pick `testing.md` as
     canonical; have the other point to it.
   - Prerequisite lists and the AGC/GMC one-line definitions restated inline vs. the
     glossary.
2. For each duplicate found, keep the fullest copy in the most discoverable home and
   replace the others with a link. Only consolidate where the copies have actually
   drifted or are likely to — do not over-factor.

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
| 8 | *(optional, decide separately)* link-check CI gate | 2 | S |

Terminology renames (task 3) go in their own commit, separate from prose edits, so
the rename is reviewable as one diff. Navigation changes ship together with the page
move that requires them.
