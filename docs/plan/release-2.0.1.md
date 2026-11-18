# Release 2.0.1

> **Status: scoped 2026-11-17, every fix landed, no candidate cut yet.** A patch release from `main`: six bug and security fixes since `v2.0.0`, and the dependency bumps that merged beside them.

## Why this release exists

The maintainer asked on 2026-11-17 for a `v2.0.1` carrying the dependency bumps merged since `v2.0.0`, with any security or bug fixes on the backlog that could land first.
Five rows were picked from the backlog that day, and a sixth, Q1358, was filed while one of them was under review and added.

The bumps turned out to close one known vulnerability, in `build-runner`'s Docker binaries (see [Security](#security)), so the fixes are most of the reason to ship.
Three of them change who can reach what: Q1358 closes a way round the raced scale-set name guard, Q1336 stops an IPv6 cluster's AGC sending its Kubernetes API traffic through the tenant's egress proxy, and Q1333 stops a mixed-case `githubURL` sending the AGC's GitHub App token exchange to a GitHub Enterprise Server path on `github.com`.

`main` carried nothing else that ships, so the patch is cut from `main` rather than from a release branch.

## Scope ledger

What the `v2.0.1` tag carries, per the [scope-ledger convention](../development/maintaining-backlog.md#cutting-a-release-the-scope-ledger).
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

No candidate has been cut.
