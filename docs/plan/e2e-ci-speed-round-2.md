# e2e CI speed, round 2

**Status: Complete.** All six items shipped.
Two sub-items under §6 are closed decisions rather than open work: the metrics-server `--metric-resolution` drop was tried, measured, and reverted, and the kube-controller-manager HPA sync period is declined on flake-risk grounds.
Both are recorded below with their evidence.
No Queue or Deferred row carries a residual.

Second pass at the wall-clock cost of the `e2e / e2e` job, after [docker-image-speed.md](docker-image-speed.md) and [e2e-tests-speed.md](e2e-tests-speed.md) exhausted their own lists.
Everything here is either outside what those covered or a hole one of them explicitly left open.

## Status

| # | Change | Status |
|---|---|---|
| 1 | Deduplicate the wrapper compile | ✅ Done — folded into the shared `build-wrapper` stage |
| 2 | One shared builder stage for all six images | ✅ Done — single root `Dockerfile` |
| 3 | Make the dependency compile a GHA-cacheable layer | ✅ Done — `deps` stage, `GOCACHE` as a real directory |
| 4 | Overlap the runner disk cleanup with job setup | ✅ Done — detached cleanup + a barrier before the bake |
| 5 | De-serialize `E2E_AGC_WorkerPodLifecycle` | ✅ Done — owner-scoped enqueue, `Serial` dropped |
| 6 | Trim the HPA spec's fixed waits | ✅ Done — `Consistently` 30s → 15s shipped; metrics-server resolution tried, measured, reverted; the kcm sync period declined |

## Baseline

Measured on run [30228166140](https://github.com/actions-gateway/github-actions-gateway/actions/runs/30228166140) — a warm-cache success on `main`, 12 min wall.

| Step | Time |
|---|---|
| Free runner disk space | 17–61 s |
| kind/helm/setup + image prepull & mirror steps | ~80 s |
| **Build images while cluster comes up** | **212 s** |
| **Run e2e tests** | **336 s** |
| everything else (cache restores, preloads, teardown) | ~20 s |

Two poles carry ~90% of the job.

### Pole 1 — the bake, not the cluster

The "build images while the kind cluster and cert-manager come up" step already overlaps cluster bring-up with the image build (round 1, §16).
From the run log, that overlap is fully effective and no longer where the time goes:

| Point in the step | t+ |
|---|---|
| cluster created, nodes Ready | 40 s |
| cert-manager images preloaded, manifest applied | 58 s |
| **blocked waiting on bake** | 58 s → 213 s |

So the step is the bake, and the bake is its six `go build` steps:

```
gmc 177s │ agc 170s │ proxy 74s │ fakegithub 69s │ worker 41s │ wrapper 41s
```

~570 s of compile contending for a 4-vCPU runner, every run, because:

- **Each image compiled the shared dependency closure separately.** Six Dockerfiles meant six `builder` stages; BuildKit can only share a build step between targets when it is the same stage in the same Dockerfile, so the `--mount=type=cache` on `/root/.cache/go-build` that round 1 added (§2) never paid off across targets — all six started cold simultaneously and raced.
- **`GOCACHE` was cold on every run.** Round 1 noted this and left it: "the cache mount … does not persist across runs unless BuildKit state itself is cached."
  `docker/setup-buildx-action` boots a fresh builder each run, so it never is.
  Meanwhile `cache-to type=gha` could not help either, because `COPY . .` invalidates the build layer on every PR.
- **The wrapper binary was compiled twice.** `cmd/worker/Dockerfile` and `cmd/worker/Dockerfile.wrapper` ran an identical `go build ./cmd/worker` in identical builder stages (log steps `#41` and `#47`, 41 s each).
  Two files, so BuildKit could not deduplicate them.

### Pole 2 — the suite's serial tail

From the JUnit report of the same run: 66 active specs, 796 s of spec time, 294 s wall.

- The parallel phase does 646 s of spec time in ~144 s — 4.5× on `--procs 6` against 4 vCPUs.
  That is healthy and is not worth further tuning.
- The six `Serial` specs are **150 s, over half the suite wall**, and Ginkgo always runs `Serial` last, so it is a pure tail:

  | Spec | Time |
  |---|---|
  | `E2E_GMC_HPADrivesScaleUp` | 78.4 s |
  | `E2E_AGC_StuckPendingPodReaped` | 32.2 s |
  | `E2E_AGC_CompletedPodReaped` | 15.8 s |
  | `E2E_AGC_WorkerPodOwnerRef` | 12.3 s |
  | `E2E_GMC_ProxyRecoversAfterPodDelete` | 8.0 s |
  | `E2E_GMC_HPAExists` | 3.4 s |

Fixed sleeps and `Consistently` blocks elsewhere in the suite are negligible — two `Consistently` calls and three real sleeps in the whole package.
Round 1 (§3, §7, §9) already took that ground.

---

## 1–3. One Dockerfile, one shared dependency compile ✓

### Approach (shipped)

All six images collapse into named stages of a single root [`Dockerfile`](../../Dockerfile), selected with `--target`:

```
deps    golang + workspace manifests + vendor/ → warm Go build cache
  └─ src    + first-party source
       ├─ build-gmc / build-agc / build-proxy / build-wrapper / build-fakegithub
       └─ gmc / agc / proxy / worker / wrapper / fakegithub
```

Three properties do the work:

1. **`deps` copies only `go.work`, `go.work.sum`, the ten module `go.mod` files, and `vendor/`** — never first-party source.
   The layer is therefore keyed on the dependency set alone, so any PR that does not touch `vendor/` restores it from the Actions cache instead of compiling.
2. **`GOCACHE` is a real directory in the layer (`/gocache`), not a `--mount=type=cache`.** This is the fix for the hole round 1 left: a cache mount lives in BuildKit's own state, which `cache-to type=gha` does not export and which a freshly-booted CI builder never has.
   A plain directory is part of the layer, so the layer cache carries it between runs.
3. **`build-wrapper` feeds both the `worker` and `wrapper` images**, so the wrapper binary is compiled once and the two images ship byte-identical binaries by construction rather than by convention.

`deps` compiles every vendored package.
The package list comes from `vendor/modules.txt`, filtered through `go list -e` to drop packages whose build constraints exclude every file on the target platform (`golang.org/x/sys/windows`, `.../plan9`, …) — `go build` fails fast on those and would abort the warm-up.

### `-trimpath` is load-bearing

The warm-up must pass the **same** `-trimpath` the binary builds use.
It is an input to every package's build-cache key, so a warm-up without it populates entries the real builds can never hit — the stage appears to work and saves nothing.
Measured locally, building `gmc` against:

| deps cache | gmc build |
|---|---|
| warmed **without** `-trimpath` | 92.3 s CPU |
| warmed **with** `-trimpath` | 3.4 s CPU |

For the same reason `build-fakegithub` now passes `-trimpath` too.
It is a test image and does not need reproducibility, but without the flag it misses the shared cache (11.9 s CPU vs 0.3 s).

### Measured locally (Apple Silicon, warm `deps`)

| Build | Time |
|---|---|
| `deps` stage, cold | 10.4 s wall / 125 s CPU |
| `gmc` | 2.2 s |
| `agc` | 2.2 s |
| `proxy`, `worker`, `wrapper`, `fakegithub` | < 1 s each |
| full `docker buildx bake`, `deps` cached | 7.5 s |

Cache size is ~1.1 GB, which is why `docker-bake.hcl` exports **one** cache scope (`scope=images`) rather than the previous scope-per-image: six scopes would store the same layer six times against the repo's 10 GB Actions-cache budget.

### Trade-offs accepted

- **A `vendor/` bump pays for a full-tree compile.** The `deps` stage compiles every vendored package, including test-only dependencies (ginkgo, envtest) that no image links.
  On a dependabot vendor bump that is more work than the old per-image closures.
  It is paid once and then cached for every subsequent PR, which is the trade we want.
- **`publish.yml` executes `deps` for real.** Release builds are `no-cache: true` for supply-chain reasons (Q127), so they cannot benefit from the stage and compile the whole tree per leg per architecture.
  Measured cost is a couple of minutes on the smaller legs, well inside the job's 30-minute timeout.
  The alternative — an ARG-selected base so only the bake path warms — was rejected: it would make e2e exercise a different stage graph than the one releases build from, and the saving is on a job that runs a handful of times a year.

### Files

- [Dockerfile](../../Dockerfile) (new) — replaces `cmd/{gmc,agc,proxy,worker}/Dockerfile`, `cmd/worker/Dockerfile.wrapper`, `test/fakegithub/Dockerfile` (all deleted)
- [docker-bake.hcl](../../docker-bake.hcl) — `target` per image, one cache scope
- [.github/workflows/publish.yml](../../.github/workflows/publish.yml), [security-scan.yml](../../.github/workflows/security-scan.yml), [dockerfile-lint.yml](../../.github/workflows/dockerfile-lint.yml) — matrices select a stage instead of a file
- [scripts/security/trivy-scan.sh](../../scripts/security/trivy-scan.sh) — `--target`; also gained the `wrapper` leg it was missing relative to the CI matrix
- [cmd/agc/names/runner_version_test.go](../../cmd/agc/names/runner_version_test.go) — the runner-version lockstep guard reads the root Dockerfile
- Path filters in `e2e-test.yml`, `e2e-calico.yml`, `security-scan.yml`, `unit-test.yml` — the Dockerfiles used to sit under `cmd/**` and `test/**` and were covered incidentally; at the repo root the file needs its own entry or an image-only change silently skips those gates

---

## 4. Overlap the runner disk cleanup with job setup ✓

**Saving: 17–61 s**

"Free runner disk space" (Q292's ENOSPC mitigation) deletes ~15–20 GB of unused toolchains and was pure serial critical path before any other step.
Nothing until the bake needs that headroom.

### Approach (shipped)

The deletions moved into [scripts/e2e/free-runner-disk.sh](../../scripts/e2e/free-runner-disk.sh) and now run in two workflow steps:

- **Start freeing runner disk space**, at the top of the job, launches the script under `setsid` so tearing the step down cannot take the cleanup with it.
  The deletions then overlap with setup-go, setup-helm, the kind install, the buildx boot, and the four cache-restore/mirror steps.
- **Wait for the disk cleanup to finish**, immediately before the bake, blocks on a sentinel the script writes as its last action.

The barrier is what makes this safe rather than a re-run of Q292.
Everything above it is light on disk; everything below it — six baked images plus the kind-loads — is exactly what exhausted the filesystem before.
The sentinel is written last and only on the success path, so a deletion that fails under `set -e` looks identical to a dead process: the waiter times out (180 s, chosen to catch a dead process rather than a slow one) and runs the cleanup synchronously instead of building on a half-cleaned runner.
Re-deleting paths the background run already removed is a no-op.

## 5. De-serialize `E2E_AGC_WorkerPodLifecycle` ✓

**Saving: ~60 s off the serial tail**

Its three specs were `Serial` for a reason that no longer held.
The comment said session IDs carry no tenant identity, so the suite enqueued a job onto *every* active fakegithub session and could not run alongside other session-consuming suites.
But fakegithub grew an owner filter since — `GET /control/sessions?owner=<prefix>`, already used by `singleuse_selfheal`, `acquire_admission`, and `job_lifecycle` via `fakegithubActiveSessionsForOwner`.

### Approach (shipped)

- The enqueue loop is scoped by owner prefix to this suite's own two tenants instead of spraying every active session.
- Those tenants get distinct ActionsGateway names (`podclean-ag`, `podstuck-ag`) where both were `test-ag` before.
  Session ownerName is `<runnerGroup>-<agentIndex>` and RunnerGroup name is `<ag>-<first label>`, so the ActionsGateway name is what makes the prefix selective — and sharing `test-ag` with `job_lifecycle` is precisely what made the spray unavoidable.
- `Serial` dropped; `Ordered` kept.

Dropping `Serial` does not disturb the shared `fakegithubLocalPort` package var.
Six sibling suites (`job_lifecycle`, `acquire_admission`, `singleuse_selfheal`, `vault_workload_identity`, `v2_multigateway`, `worker_securitycontext`) already assign it from their own base port in their own `BeforeAll` without being `Serial`: Ginkgo runs an `Ordered` container's specs contiguously within one process, so no other container can reassign it mid-suite.
This suite keeps its own base port (19300).

The general rule behind that — the two guarantees, and the mutual exclusion it does *not* grant — is in [testing.md](../development/testing.md#ordered-containers-run-whole-in-one-process--which-is-why-a-suite-can-hold-package-state).

## 6. Trim the HPA spec's fixed waits ✓

**Saving: ~15 s off the serial tail, plus part of the ScalingActive wait**

`E2E_GMC_HPADrivesScaleUp` is 78 s, of which 30 s was a fixed `Consistently(30*time.Second, ...)` — the Q283 regression guard that the reconciler does not claim `.spec.replicas` back from the HPA.

### Approach (shipped)

- **`Consistently` 30 s → 15 s.** What determines whether a revert is caught is the reconcile rate, not the wall-clock window; every proxy pod reaching Ready re-triggers a reconcile, and at 2 s polling 15 s still samples the Deployment eight times across many passes.
  This is a fixed cost paid on every run inside a Serial spec, so it lands whole on the critical tail.

### Tried and reverted: metrics-server `--metric-resolution` 15 s → 10 s

The HPA cannot report `ScalingActive=True` until metrics-server has scraped, so the default resolution sits directly in front of this spec.
Lowering it to 10 s — metrics-server's minimum *accepted* value — looked like free latency.

It is not.
`--metric-resolution` must also **exceed kubelet's housekeeping interval** (10 s by default).
At exactly 10 s metrics-server keeps re-reading unchanged cAdvisor samples, discards them as duplicate timestamps, and never serves usage at all — so the outcome is not a slower HPA but a dead one.

On PR #874 both specs that gate on `ScalingActive=True` timed out:

| Spec | Timeout | Message |
|---|---|---|
| `E2E_GMC_HPADrivesScaleUp` | 300 s | metrics-server not serving metrics |
| `E2E_AGC_SkippedJobIsRedeliveredAfterCapacityFrees` | 240 s | metrics-server not serving metrics |

Reverted, with the reasoning recorded inline at the patch site so the next person to spot that 15 s does not re-derive it the expensive way.
The lesson is the repo's own rule: this was shipped on a recalled documentation fact instead of a measurement, and the e2e leg is what caught it.

### Not done: the kube-controller-manager HPA sync period

The other bound on `ScalingActive` is kube-controller-manager's 15 s `--horizontal-pod-autoscaler-sync-period`, settable through a kind `kubeadmConfigPatches` block.
Left alone deliberately: it raises the reconcile rate cluster-wide, on a 4-vCPU runner, for perhaps 5 s on one spec — and CPU starvation on this cluster is the documented mechanism behind the Q300 kindnet flake family.
Not a trade worth making for 5 s.

## Not doing

- **A larger runner.** Both poles are CPU-bound on a 4-vCPU `ubuntu-latest` and both scale close to linearly, and `vars.GAG_E2E_RUNNER` already routes this job to self-hosted runners when set — so this is a config decision, not a code change, and it is orthogonal to everything above.
- **Precompiling the Ginkgo test binary.** The "Run e2e tests" step is 336 s against 294 s of specs; the ~18 s of Ginkgo compile/setup in between is not worth a change.
