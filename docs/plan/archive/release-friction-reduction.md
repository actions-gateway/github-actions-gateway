# Release friction reduction

> **Status:** Complete (all buckets shipped — Q293/Q294/Q295/Q296).
> Motivated by the `v1.1.0-rc.7 → v1.1.0` cut (2026-07-12), which exercised the full release + pre-GA dogfood validation gate end-to-end for the first time and surfaced repeated manual friction and several footguns.
> This plan captures where the friction *structurally* comes from and the highest-leverage fixes, so we stop patching individual footguns.
> Bucket 2's wrapper is code-complete + gate-clean; end-to-end validation against dogfood is a human pre-GA-gate step (see Validation).

## Motivation — friction observed during the v1.1.0 cut

Two distinct buckets, each with a single high-leverage root cause.

**Publish ceremony.** `publish.yml` produces the signed artifacts but hands back a *broken, minimal* GitHub Release and a chart whose prerelease state depends on a human-run PR.
So every cut repeats:
- A manual **prerelease-flag flip PR** (`artifacthub.io/prerelease` `true`↔`false`) that must land *before* the tag and be flipped back for the next RC — pure ceremony, easy to forget.
- **Fixing the auto-created Release by hand**: it defaults `prerelease=false` (wrong for an RC) and auto-generates notes from the wrong base tag.
- **Copying the five image digests** into the release notes by hand.

**Validation orchestration.** The pre-GA dogfood gate ([release.md § Validate the release candidate on dogfood](../../operations/release.md#validate-the-release-candidate-on-dogfood)) is correct in intent but was written from source-reading; running it revealed the runbook is missing required env and ordering, and `setup.sh` has a silent footgun:
- `setup.sh` needs `APP_ID`, `INSTALLATION_ID`, `ASSUME_YES=1` (reads the App PEM from the macOS keychain) — undocumented in the gate steps.
- **`DOGFOOD_RUNNER_IMAGE` footgun**: re-running `setup.sh` *without* it silently resets the tenant's `RunnerTemplate` `workerImage` to the toolchain-less upstream default, so `make`/Go vanish and CI jobs fail `make: command not found`.
  Cost a full failed validation cycle.
- The cluster is at **0 nodes at rest** — `setup.sh`'s GMC-rollout wait times out until `start.sh` scales the system pool up.
- The **e2e leg is triggered by `gh run rerun`** (+ the `GAG_E2E_RUNNER` variable), not by `e2e-start.sh` (which only wires routing).
- The single `e2-standard-2` system node **can't fit the on-demand e2e AGC** alongside the always-on CI AGCs — needs a temporary +1 node.

## Bucket 1 — make `publish.yml` own the whole Release (highest leverage) — DONE (Q293)

Implemented in `publish.yml`'s `chart-publish` job: the tag-resolution step now emits a `prerelease` output (0.x or a `-rc`/`-alpha`/`-beta` suffix ⇒ true), both chart-package steps `yq`-stamp `artifacthub.io/prerelease` from it before `helm package`, and a new "Compose and create the GitHub Release" step writes the five index digests + `make verify-release` line + generated changelog and sets `--prerelease`, guarded to create only when the tag has no Release yet.
First exercised on the next real `v*` tag (see Validation).

The pipeline already has every piece of data; it just doesn't finish the job.

1. **Derive `artifacthub.io/prerelease` from the tag at package time.** `publish.yml` already overrides the chart's `version`/`appVersion` from the tag; setting the annotation from whether the tag matches `-rc`/`0.` is the same mechanism (a `yq` edit before `helm package`). **Eliminates the flip PR entirely** — both directions.
   Update release.md § Chart version & metadata to match (it currently states the annotation is baked as-is and must be flipped by hand).
2. **Generate complete, correct release notes and set `--prerelease` on the Release.** Emit the five multi-arch **index** digests (already recorded to the run summary), the `make verify-release VERSION=<tag>` line, and a `compare/<prev>...<tag>` changelog into the Release body, with `--prerelease` set from the tag. **Eliminates** the manual `gh release edit`, the wrong-base notes, and the digest copy.
   Keep the "create only if absent, never clobber curated notes" guard.

After Bucket 1, Phase A collapses to: **push tag → verify.**

## Bucket 2 — one-command dogfood validation gate + footgun fixes

1. **`scripts/dogfood/validate-release.sh <rc-tag>`** — DONE (Q294).
   A single wrapper that runs setup → start → e2e-start → `gh run rerun` (e2e) → CRD artifact smoke → teardown, with the correct env baked in (`APP_ID` defaulted / `INSTALLATION_ID` auto-resolved via `gh api`, `ASSUME_YES=1` for the child scripts, `DOGFOOD_RUNNER_IMAGE` left untouched so setup.sh's Q295 preserve wins, system-pool scaled 0→1 before setup and +1 for the on-demand e2e AGC's contention window), idempotent and self-cleaning (EXIT trap tears back down to 0 nodes on success and failure). release.md's gate section now points at the one command.
   Turns an hour of footguns into a walk-away command.
   End-to-end run against dogfood is a human pre-GA-gate step (see Validation).
2. **Fix the `setup.sh` `DOGFOOD_RUNNER_IMAGE` footgun** (agreed) — DONE (Q295): when the env is unset, `apply_cr` now **preserves** the existing `RunnerTemplate` runner image (reads it back from the cluster with `kubectl get … -o jsonpath`) instead of resetting it, so an idempotent re-run can't silently regress the runner toolchain.
   Independently valuable — done first; it protects every dogfood re-run, not just the release gate.

## Bucket 3 — cleanups from the same session — DONE (Q296)

- **Correct release.md's gate section** — DONE.
  The [§ Validate the release candidate on dogfood](../../operations/release.md#validate-the-release-candidate-on-dogfood) steps now state the real `setup.sh` env (`APP_ID`, `INSTALLATION_ID`, `ASSUME_YES=1`; App PEM read from the macOS keychain), the 0-nodes→`start.sh` ordering (setup's GMC-rollout wait times out at rest and `start.sh` completes it), the `gh run rerun` e2e trigger (`e2e-start.sh` only wires routing), and the system-node +1-node contention note.
- **Narrow the Calico e2e path filter** — DONE.
  Both the `push.paths` list and the `changes` `calico` filter in [`e2e-calico.yml`](../../../.github/workflows/e2e-calico.yml) now watch `charts/actions-gateway/templates/**` instead of `charts/actions-gateway/**`, so a metadata-only `Chart.yaml` edit no longer fires the expensive, flaky (Q291) Calico lane while a NetworkPolicy-template change still does.
  Used a positive `templates/**` include (which simply doesn't match `Chart.yaml`) rather than a `!Chart.yaml` exclude, since dorny/paths-filter items are OR'd and a leading-`!` does not subtract.

## Sequencing

1. **Q295** ✅ DONE — `setup.sh` `apply_cr` now preserves the live RunnerTemplate's runner-container image when `DOGFOOD_RUNNER_IMAGE` is unset (reads it back with `kubectl get … -o jsonpath`), instead of resetting it to the image-less default.
   An idempotent re-run can no longer silently regress the runner toolchain.
2. **Q293 — Bucket 1** ✅ DONE — `yq` prerelease stamp + a `gh release` compose/create step in `publish.yml`.
   Removed the flip PR and the by-hand Release fixups.
3. **Q296 — Bucket 3** ✅ DONE — release.md gate-section corrections + narrowed the Calico path filter to `charts/actions-gateway/templates/**`.
4. **Q294 — Bucket 2** ✅ DONE — `scripts/dogfood/validate-release.sh <rc-tag>`: the one-command, idempotent, self-cleaning wrapper for the whole pre-GA dogfood gate (setup → start → e2e-start → `gh run rerun` → CRD smoke → teardown). release.md's gate section points at it.
   End-to-end validation against dogfood is the human pre-GA-gate step.
   Removes the scariest, prod-cluster friction.

## Validation

Bucket 1 is first exercised on the *next* real `v*` tag — verify the published chart carries the tag-derived prerelease flag and the Release has correct notes + flag, exactly as this plan's motivating cut had to be fixed by hand.
Bucket 2's wrapper is validated by running the next pre-GA gate through it end-to-end.
