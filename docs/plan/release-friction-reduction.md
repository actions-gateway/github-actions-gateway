# Release friction reduction

> **Status:** Planned. Motivated by the `v1.1.0-rc.7 → v1.1.0` cut (2026-07-12),
> which exercised the full release + pre-GA dogfood validation gate end-to-end for
> the first time and surfaced repeated manual friction and several footguns. This
> plan captures where the friction *structurally* comes from and the
> highest-leverage fixes, so we stop patching individual footguns.

## Motivation — friction observed during the v1.1.0 cut

Two distinct buckets, each with a single high-leverage root cause.

**Publish ceremony.** `publish.yml` produces the signed artifacts but hands back a
*broken, minimal* GitHub Release and a chart whose prerelease state depends on a
human-run PR. So every cut repeats:
- A manual **prerelease-flag flip PR** (`artifacthub.io/prerelease` `true`↔`false`)
  that must land *before* the tag and be flipped back for the next RC — pure
  ceremony, easy to forget.
- **Fixing the auto-created Release by hand**: it defaults `prerelease=false` (wrong
  for an RC) and auto-generates notes from the wrong base tag.
- **Copying the five image digests** into the release notes by hand.

**Validation orchestration.** The pre-GA dogfood gate
([release.md § Validate the release candidate on dogfood](../operations/release.md#validate-the-release-candidate-on-dogfood))
is correct in intent but was written from source-reading; running it revealed the
runbook is missing required env and ordering, and `setup.sh` has a silent footgun:
- `setup.sh` needs `APP_ID`, `INSTALLATION_ID`, `ASSUME_YES=1` (reads the App PEM
  from the macOS keychain) — undocumented in the gate steps.
- **`DOGFOOD_RUNNER_IMAGE` footgun**: re-running `setup.sh` *without* it silently
  resets the tenant's `RunnerTemplate` `workerImage` to the toolchain-less upstream
  default, so `make`/Go vanish and CI jobs fail `make: command not found`. Cost a
  full failed validation cycle.
- The cluster is at **0 nodes at rest** — `setup.sh`'s GMC-rollout wait times out
  until `start.sh` scales the system pool up.
- The **e2e leg is triggered by `gh run rerun`** (+ the `GAG_E2E_RUNNER` variable),
  not by `e2e-start.sh` (which only wires routing).
- The single `e2-standard-2` system node **can't fit the on-demand e2e AGC**
  alongside the always-on CI AGCs — needs a temporary +1 node.

## Bucket 1 — make `publish.yml` own the whole Release (highest leverage)

The pipeline already has every piece of data; it just doesn't finish the job.

1. **Derive `artifacthub.io/prerelease` from the tag at package time.** `publish.yml`
   already overrides the chart's `version`/`appVersion` from the tag; setting the
   annotation from whether the tag matches `-rc`/`0.` is the same mechanism (a `yq`
   edit before `helm package`). **Eliminates the flip PR entirely** — both
   directions. Update release.md § Chart version & metadata to match (it currently
   states the annotation is baked as-is and must be flipped by hand).
2. **Generate complete, correct release notes and set `--prerelease` on the
   Release.** Emit the five multi-arch **index** digests (already recorded to the run
   summary), the `make verify-release VERSION=<tag>` line, and a
   `compare/<prev>...<tag>` changelog into the Release body, with `--prerelease` set
   from the tag. **Eliminates** the manual `gh release edit`, the wrong-base notes,
   and the digest copy. Keep the "create only if absent, never clobber curated
   notes" guard.

After Bucket 1, Phase A collapses to: **push tag → verify.**

## Bucket 2 — one-command dogfood validation gate + footgun fixes

1. **`scripts/dogfood/validate-release.sh <rc-tag>`** — a single wrapper that runs
   setup → start → e2e-start → `gh run rerun` (e2e) → CRD artifact smoke → teardown,
   with the correct env baked in (`APP_ID`/`INSTALLATION_ID` resolved, `ASSUME_YES`,
   `DOGFOOD_RUNNER_IMAGE` preserved, node scaling for the e2e AGC), idempotent and
   self-cleaning. Turns an hour of footguns into a walk-away command.
2. **Fix the `setup.sh` `DOGFOOD_RUNNER_IMAGE` footgun** (agreed): when the env is
   unset, **preserve** the existing `RunnerTemplate` `workerImage` (read it back from
   the cluster) instead of resetting it, so an idempotent re-run can't silently
   regress the runner toolchain. Independently valuable — do this first; it protects
   every dogfood re-run, not just the release gate.

## Bucket 3 — cleanups from the same session

- **Correct release.md's gate section** with the real `setup.sh` env, the
  0-nodes→`start.sh` ordering, the `gh run rerun` e2e trigger, and the system-node
  +1-node contention note. (The section as merged in #600 is idealized.)
- **Narrow the Calico e2e path filter.** [`e2e-calico.yml`](../../.github/workflows/e2e-calico.yml)
  watches `charts/actions-gateway/**`, so a metadata-only `Chart.yaml` edit (an
  annotation/version bump) needlessly triggers the expensive, flaky (Q291) Calico
  lane. The NetworkPolicy templates it cares about live under
  `charts/actions-gateway/templates/**`; narrow the filter there (or exclude
  `Chart.yaml`) so metadata-only edits don't fire it.

## Sequencing

1. **Q295** — `setup.sh` `workerImage` preserve (small, protects every re-run now).
2. **Q293 — Bucket 1** (biggest bang, lowest risk: `yq` + a `gh release` step in an
   existing workflow). Removes the most-repeated manual steps.
3. **Q296** — Bucket 3 cleanups (release.md corrections + Calico filter).
4. **Q294 — Bucket 2** wrapper script (more code; removes the scariest, prod-cluster
   friction).

## Validation

Bucket 1 is first exercised on the *next* real `v*` tag — verify the published chart
carries the tag-derived prerelease flag and the Release has correct notes + flag,
exactly as this plan's motivating cut had to be fixed by hand. Bucket 2's wrapper is
validated by running the next pre-GA gate through it end-to-end.
