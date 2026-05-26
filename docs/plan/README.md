# Plans

Living planning docs for in-flight work. Each file is a self-contained
plan with rationale, scope, and (where appropriate) a status table
near the top. This README is the index.

Last refreshed 2026-05-25. Status reflects the plan's own
"Status at a glance" section where present, or a quick read against
current code otherwise. Authoritative state always lives in the
individual file.

Legend: ✅ done, ⚠️ partial / mixed, ❌ open, ⓘ informational
(forward-looking spec or design rationale, no progress to track).

## Implementation roadmap

The five-milestone delivery from
[docs/design/06-implementation-phases.md](../design/06-implementation-phases.md).

| Plan | Scope | Status |
|---|---|---|
| [milestone-1.md](milestone-1.md) | Wire-protocol probe; broker + githubapp packages | ✅ Done |
| [milestone-2.md](milestone-2.md) | AGC controller, reconciler, agent pool, token manager | ⚠️ Mostly done — code shipped; goroutine-leak integration suite + live `kind` `activeSessions == 1` verification still open |
| [milestone-3.md](milestone-3.md) | Worker pod, Named Pipe handoff, pod provisioner, eviction retry | ⚠️ Code done — end-to-end green-checkmark gated on Investigation A (Named Pipe protocol) |
| [milestone-4.md](milestone-4.md) | GMC, ActionsGateway CRD, proxy binary, webhook, TLS pinning | ⚠️ Code done — live `kind` multi-tenant validation pending (blocked on M3) |
| [milestone-5.md](milestone-5.md) | Hardening + 1,000-session load testing + posture audit + packaging | ⚠️ Security-half done via security.md W2/W7/W8 + ResourceQuota; packaging, `test/load/` harness, `kube-bench`/`polaris` scan, gVisor validation still open |

## Security

| Plan | Scope | Status |
|---|---|---|
| [security.md](security.md) | OWASP-style code review with finding-level workstreams | ⚠️ Phase 1 + 2 + 3 backlog all done in code; **M-11b** (live Ed25519 GitHub probe) and Phase 1 live `kind` validation remain |
| [worker-egress-proxy.md](worker-egress-proxy.md) | Worker traffic must route through per-tenant proxy pool | ⚠️ Code done (NetworkPolicy split, commit `4932ce7`); live `curl` validation pending |

## Test plans

The "spec" plans translate `docs/design/07-test-plan.md` into concrete
test files. The "gaps" plans track holes found by review of existing
tests.

| Plan | Scope | Status |
|---|---|---|
| [integration-tests.md](integration-tests.md) | envtest layout, fakegithub, per-controller integration suites | ⓘ Spec — source of truth for what the integration suite covers |
| [e2e-tests.md](e2e-tests.md) | `kind` layout, cluster addons, Tier A/B test surfaces | ⓘ Spec |
| [milestone-1-tests.md](milestone-1-tests.md) | M1 unit-test coverage gaps | ✅ Done — all five gaps closed |
| [milestone-2-tests.md](milestone-2-tests.md) | M2 unit + envtest gaps (11 items) | ❌ Open — gaps documented, fixes not landed; envtest goroutine-leak suite is the headline item |
| [milestone-3-tests.md](milestone-3-tests.md) | M3 metric/decryption/eviction test gaps | ❌ Open — H/M/L items not yet implemented |
| [milestone-4-tests.md](milestone-4-tests.md) | M4 builder + IPRange + webhook test gaps (8 items) | ❌ Open — buildNoProxy bug fix is the headline item |

## Speed improvements

Performance plans for build and test pipelines. Each has inline ✓
markers per item.

| Plan | Scope | Status |
|---|---|---|
| [docker-image-speed.md](docker-image-speed.md) | Image build + load-into-kind time | ⚠️ Has own Status table — §1/2/4/5 done; §7/8/9/12 still TODO |
| [unit-tests-speed.md](unit-tests-speed.md) | Four targeted unit-test latency cuts (~6s total) | ❌ Open — no ✓ markers on any of the four items |
| [integration-tests-speed.md](integration-tests-speed.md) | Five integration polling/sleep cuts | ❌ Open — no status markers; items are pure proposals |
| [e2e-tests-speed.md](e2e-tests-speed.md) | Five e2e suite improvements | ⚠️ Mixed — §2, §3 marked ✓; §1, §4, §5 not |

## Cross-cutting

| Plan | Scope | Status |
|---|---|---|
| [gaps.md](gaps.md) | Three code-level fixes surfaced by doc audit (CRD eviction fields, proxy resource merge, credential rotation observability) | ❌ All 3 open |
| [docs.md](docs.md) | Documentation roadmap across phases | ⚠️ Phase 1 fully done; 4 items open in Phase 2/3 |
| [make.md](make.md) | Makefile UX (help target, e2e workflow, image var consistency) | ⚠️ Phase 1 done; Phase 2 has open drift items (image vars, envtest, `all` semantics) |

## Conventions

When adding a new plan:

- Put it at the top of the file: a one-paragraph "what and why," then a
  **Status at a glance** table if there are 3+ discrete work items with
  mixed state. The table is the index a returning reader scans first.
- Cite code with file:line links. They go stale, but stale links are
  easier to fix than missing ones.
- Mark deferred or accepted items explicitly (⚠️ Partial — *what was
  accepted and why*). Silent omissions become land mines.
- Once everything in a plan ships, leave the plan in place with the
  status table updated to ✅ Done. Don't delete it — the rationale
  is more valuable than the diff.

Add a row to this README when creating or completing a plan.
