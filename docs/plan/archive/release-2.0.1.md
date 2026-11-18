# Release 2.0.1

> **Status: shipped as `v2.0.1` on 2026-11-18**, tagged at `879e20c62` from the tree `v2.0.1-rc.2` validated at `74531a223` ([gate run 44322796149](https://github.com/actions-gateway/github-actions-gateway/actions/runs/44322796149)); `check-artifact-unchanged.sh` found none of the 8 files changed between them on the released surface.
> `rc.1` passed every leg too, and its teardown found the idle scale-set worker #2357 fixes.

## Scope ledger

What the `v2.0.1` tag carries, per the [scope-ledger convention](../../development/maintaining-backlog.md#cutting-a-release-the-scope-ledger).
No row carries a `2.0.1-gate` label: each was dispatched and merged in the same day, so the ledger records what landed rather than what the tag waits for.

| Q-ID | Item | PR | Status |
|---|---|---|---|
| Q1358 | A gateway created with the GMC finalizer already set bypasses the raced-pair refusal | #2352 | ✅ landed `f156d4c53` |
| Q1342 | A stored scale-set name collision freezes the established tenant's gateway | #2351 | ✅ landed `7633208bb` |
| Q1336 | An IPv6 API-server address in the AGC's `NO_PROXY` is bracketed, which `net/http` ignores | #2348 | ✅ landed `059134636` |
| Q1335 | One consumer's unwritable attribution projection stalls a shared `EgressProxy` pool | #2349 | ✅ landed `b2e6e7fc8` |
| Q1333 | A mixed-case or trailing-dot public GitHub `githubURL` is addressed as GitHub Enterprise Server | #2347 | ✅ landed `d584aa340` |
| Q1264 | Pin the proxy `NetworkPolicy` before the `Deployment` in envtest (test only; the reorder shipped in `v2.0.0`) | #2350 | ✅ landed `c50466ac3` |

## Pre-flight

Every verdict below was measured at `f156d4c53`, `main`'s tip after #2352 merged, on 2026-11-17.

| Check | Verdict |
|---|---|
| Version | `scripts/release/semver-floor.sh v2.0.0`: floor `PATCH`, set by the five `fix` commits; the three `feat` commits are CI and release tooling and ship nothing. |
| API surface | `scripts/release/api-surface-since.sh v2.0.0`: no API surface changed. No CRD, chart or `api/` file differs from `v2.0.0`. |
| `main` green | `scripts/release/check-gates-green.sh`: 0 gates not green, 2 path-skipped in the merge queue. Both ran in full on the `push` lane: `security-scan` in run 44229213304, every scan job green (its `report` job runs only on a schedule), and `unit-test` in run 44229213133. |
| Local gate | `make check` exit 0 on a branch at `f156d4c53`, with no failure line in its output. |

## Security

**No advisory accompanies this release, and one dependency bump closes a known vulnerability.** Measured 2026-11-17 with `trivy` 0.72.0 on the published `v2.0.0` images.
`build-runner`'s Docker plugins carried four fixable HIGH findings, and #2344's `docker:29-cli` bump clears CVE-2026-78669 and removes CVE-2026-78661 from buildx; the two left, CVE-2026-17106 and CVE-2026-78661 in compose, are excused until 2026-12-17 because no Docker release fixes them yet.
The `gmc`, `agc` and `proxy` images carried nothing, and GitHub's advisory database lists no advisory affecting `google.golang.org/grpc` 1.84.0 or `cel.dev/expr` 0.25.2, so those bumps close nothing known.
`scripts/release/check-vuln-policy.sh` passes at `f156d4c53`.

## Candidate validation

**`v2.0.1-rc.1`, tagged at `f156d4c53`, passed every gate leg in CI.** Gate run 44236535751's evidence names that commit and records `kata`, `crd-smoke`, `soak`, `storage`, `dind` and `dragonfly`, every leg in `scripts/dogfood/lib/gate-legs.sh`, and each soak reading in its `soak-readings` artifact is a pass.

The publish checked out by content before the gate ran: the Release is immutable with all 9 assets, `make verify-release` and `make verify-release-compat` (cosign v2.5.2) each report 25 OK and no failure, all seven images' provenance names `publish.yml@refs/tags/v2.0.1-rc.1` at `f156d4c53`, and a `cosign verify` against a wrong signer identity fails.

**The run is red because teardown could not drain, not because a leg failed.** The gate's log ends "The gate PASSED; it exits 1 because the cluster is not at rest", and the workflow's own reclaim step failed on the same drain.
The e2e leg's one CI job (run 44247655794, `e2e / e2e`, green) had two runner pods in the `ci-e2e` `RunnerSet`: one ran it and was reaped at 06:30Z, and the other registered at 06:05Z, logged `Listening for Jobs`, and received nothing.
The AGC reaps a `Running` worker only once it carries a job-completed stamp, so that pod had no deadline and pinned an `n2-standard-8` node until it was deleted by hand just before 09:00Z, after which `validate-release.sh --reclaim` scaled the system pool to 0 and released the lease.
The reaper and provisioner are unchanged between `v2.0.0` and `f156d4c53`, so this is not a regression the candidate introduced.
The cause, measured from the cluster's audit log in #2357: GitHub withdrew the job from a worker slow to start on a cold node and re-issued it, the slow worker took the re-issue, and the reclaim skipped the re-issue's worker because it was still `Pending`.

**`v2.0.1-rc.2`, tagged at `74531a223` with that fix, passed every gate leg in CI and tore down clean.** Gate run 44322796149's evidence names that commit and records all six legs; the three e2e runs (44326035978, 44334553779, 44341469221) each report 92 cases, 74 passed, 18 skipped and 0 failed; and `ops.sh at-rest` found no Compute Engine instances afterwards.
The publish checked out by content: the Release is immutable with all 9 assets, `make verify-release` and `make verify-release-compat` pass, all seven images' provenance names `publish.yml@refs/tags/v2.0.1-rc.2` at `74531a223`, and a `cosign verify` against a wrong signer identity fails.
The gate's side dispatch of `unit-test.yml` onto GAG runners (run 44325908424) failed in `scripts-test` on `gate-list-test`'s `queue-scope-underivable` case, rc 2 for a wanted 1 beside a `sort` broken-pipe error, while the push and merge-queue runs of the same commit passed; it is a test script, not a released artifact, and the leg set does not include it.

## Stable-tag pre-flight

Read 2026-11-18 against `origin/main` at `8b6727a6f`, after Karl approved promoting `rc.1`, and re-read for `rc.2` against `74531a223`: the freeze check passes over 0 changed files, `operator-caveats-since.sh v2.0.0` still reports no section, `make roadmap-check` passes, and #2357 adds no capability a marketing surface names.

- **Freeze**: `check-artifact-unchanged.sh f156d4c53 origin/main` passes; the 7 files changed since the candidate are none of them on the released surface.
- **Operator caveats**: `operator-caveats-since.sh v2.0.0` reports no new section, so the notes carry none.
- **Marketing**: no surface needs a change.
  The fixes make the cross-tenant scale-set name guard in `features.md` and `why-gag.md` hold for a gateway created with the finalizer, and add no capability.
- **Roadmap and `features.md`**: `make roadmap-check` passes, and nothing this release ships is a new capability.
- **Announce bar**: `highlight_for` names `v2.0.1`.
- **Pins**: the install and upgrade pages name `2.0.1`, and `make release-pins-check` reports 19 pins at `v2.0.1` and the exempt `v2.0.0`.
- **Notes**: the Validation section names `rc.2` and gate run 44322796149, and the commit count (22) assumes this plan's PR is the only merge before the tag.
