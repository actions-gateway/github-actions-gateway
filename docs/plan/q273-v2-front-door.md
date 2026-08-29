# Q273 — Make v2 the front door + exemplary v1→v2 migration (plan)

**Status:** ▶ Started 2026-07-06.
**Scope:** the *do-now* front-door / positioning / onboarding / migration slice of [Q273](../queue/Q273.md).
Full "v2-only" (v1 removal) stays gated on v2beta1 (Q74) — **this plan removes nothing**.
Strategy source: [v1-classic-sunset-review.md §6.2](v1-classic-sunset-review.md).

## Goal (one sentence)

Make v2 (`actions-gateway.com/v2alpha1` — `ActionsGateway` + `RunnerSet` + `RunnerTemplate` + `EgressProxy`) the recommended front door across README / onboarding / positioning, keep v1 (`v1alpha1`, classic) reachable-but-clearly-secondary with deprecation banners, and make the `gag-migrate` v1→v2 story exemplary.

## Approach

Align the whole doc surface to the posture [v1alpha1-deprecation.md](../operations/v1alpha1-deprecation.md) *already states* ("new tenants onboard on v2") — today README / getting-started / roadmap still contradict it by calling v1 "the standard path."
Route new users to v2 (the better shape), be honest it is alpha-but-recommended, mark v1 the stable-but-deprecated legacy path.
Positioning stays true whether or not the sibling P5 ScaleSet-default flip has merged yet.

## Doc-ownership partition (avoid collision with the Q264-P5 chip)

**P5 owns:** the code default flip + the `acquisitionProtocol` CRD field docs + the classic-protocol operator reference (in `docs/operations/` protocol section and the `tenant-onboarding` field reference).
**This chip owns:** everything front-door / positioning / onboarding-flow / migration.
- **Do NOT edit** the `acquisitionProtocol` field reference or classic-protocol operator docs.
  On conflict, rebase and defer to P5 on protocol-field specifics.
- Positioning frames the single-acquirer ScaleSet model as v2's acquisition story **without hard-asserting the shipped default is already ScaleSet** (true whether or not P5 has merged).
  PR body flags this coupling so P5 can tighten to "default" language on flip.

## Work items

1. **Front door → v2** (recommended path is v2, v1 secondary):
   - `docs/getting-started.md` — restructure to lead with the v2 object set; move the v1 walkthrough to a clearly-secondary "legacy v1 API" section.
   - `README.md` — Quick Start / Installation: name v2 the recommended tenant API, link the migration guide; keep v1 reachable.
   - `docs/operations/tenant-onboarding.md` — v2-first flow ordering (NOT the acquisitionProtocol field — P5's).
   - `charts/actions-gateway/templates/sample-gateway.yaml` — assess; keep v1 sample valid but add a v2 pointer if user-facing.
   - `docs/roadmap.md` — stop calling v1 "the standard path"; v2 = recommended.
2. **Positioning (§4.7):** `docs/why-gag.md`, `docs/index.md` — v2-first, ARC-current.
   Keep the goroutine-listener density claims (they hold under ScaleSet per review §3.2).
   Route sample CRs / capability rows to v2 as the recommended shape.
3. **Deprecate-v1 banners:** admonition banners ("v1 is deprecated — start on v2") on v1-primary doc pages (getting-started v1 section, tenant-onboarding v1 parts, why-gag v1 sample).
   Banners only — no removal.
4. **Migration exemplary:** polish `cmd/gmc/migrate/main.go` UX + `docs/operations/ migration-v1-to-v2.md` so v1→v2 is smooth and well-documented.

## Guardrails

- Fresh `claude/` worktree; rebased on origin/main.
  No cluster / no dogfood.
- `docs/STATUS.md` edit is an ISOLATED commit (keep Q273 row; note do-now routing done, full v2-only still gated on Q74).
- Human docs never link to `CLAUDE.md`.
  `make check` (+ docs build if touched) green before PR.
  No Claude attribution.
