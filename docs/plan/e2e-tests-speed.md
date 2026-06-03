# E2E Test Speed Improvements

This document analyses where time is spent in the e2e suite and describes five concrete improvements, in order of estimated impact. Each section covers motivation, implementation steps, files affected, and estimated savings.

---

## Background — where time goes today

A typical CI run of `make e2e` breaks down roughly as follows:

| Phase | Typical duration |
|---|---|
| `make e2e-cluster` — kind create (3-node) | ~2 min |
| `make e2e-images` — 4 Docker builds, no layer cache | ~8 min |
| `make e2e-load-images` — kind load × 4 | ~2 min |
| `BeforeSuite` — cert-manager + metrics-server + GMC deploy | ~4 min |
| Test execution — 6 `Describe` suites, sequential | ~15–20 min |
| **Total** | **~30–40 min** (limit: 45 min) |

The test execution phase dominates and is where the biggest gains are available. Each `Describe` suite has its own `BeforeAll` that provisions a fresh namespace and waits for deployments to reach Ready (~1–3 min actual). Running them serially means those waits stack up; running them concurrently collapses them to the single longest wait.

The five improvements below are ordered by estimated CI savings.

---

## 1. Parallel Ginkgo execution

**Estimated savings: 10–15 min**

### Problem

Ginkgo runs all specs in a single OS thread by default. The six independent `Describe` suites — `E2E_GMC_Provisioning`, `E2E_GMC_Isolation`, `E2E_GMC_RBAC`, `E2E_GMC_HPA_PDB`, `E2E_GMC_Resilience`, and `E2E_AGC_JobLifecycle` — each spin up a dedicated tenant namespace and wait for their own `ActionsGateway` CR to provision. Those waits are completely independent and can overlap.

### Approach

Ginkgo v2 supports `--procs=N` (or `-p` for auto-detect). Each process receives a disjoint partition of the spec tree and runs it concurrently. The catch is that `BeforeSuite` runs once per process; without coordination, all six processes would each try to install cert-manager and deploy GMC.

The fix is to replace `BeforeSuite`/`AfterSuite` with `SynchronizedBeforeSuite`/`SynchronizedAfterSuite`. Node 0 does the shared, destructive setup; all other nodes wait for it to finish, then receive shared state (image names) via the byte slice return value.

### Implementation steps

1. **Replace `BeforeSuite` in `e2e_suite_test.go`**:

   ```go
   var _ = SynchronizedBeforeSuite(
       // Runs ONCE on process 0.
       func() []byte {
           // generate RSA key
           key, err := rsa.GenerateKey(rand.Reader, 2048)
           // ... encode to PEM, store in testRSAKeyPEM ...

           configureKubectlKubeRC()
           setupCertManager()
           setupMetricsServer()
           setupFakegithub()
           setupGMC()

           // Marshal shared state to pass to all nodes.
           data, _ := json.Marshal(suiteData{
               GMCImage:        envOrDefault("GMC_IMG", "gmc:e2e"),
               AGCImage:        envOrDefault("AGC_IMG", "agc:e2e"),
               ProxyImage:      envOrDefault("PROXY_IMG", "proxy:e2e"),
               FakegithubImage: envOrDefault("FAKEGITHUB_IMG", "fakegithub:e2e"),
               RSAKeyPEM:       testRSAKeyPEM,
           })
           return data
       },
       // Runs on ALL processes (including 0) after node 0 finishes.
       func(data []byte) {
           var sd suiteData
           _ = json.Unmarshal(data, &sd)
           gmcImage = sd.GMCImage
           agcImage = sd.AGCImage
           proxyImage = sd.ProxyImage
           fakegithubImage = sd.FakegithubImage
           testRSAKeyPEM = sd.RSAKeyPEM
       },
   )

   var _ = SynchronizedAfterSuite(
       func() { /* per-node teardown — nothing needed */ },
       func() {
           // Runs ONCE on process 0.
           teardownGMC()
           teardownFakegithub()
           teardownCertManager()
       },
   )

   type suiteData struct {
       GMCImage        string `json:"gmcImage"`
       AGCImage        string `json:"agcImage"`
       ProxyImage      string `json:"proxyImage"`
       FakegithubImage string `json:"fakegithubImage"`
       RSAKeyPEM       []byte `json:"rsaKeyPEM"`
   }
   ```

2. **Add `--procs=6` to the `e2e` Makefile target** (one proc per independent `Describe` suite):

   ```makefile
   e2e:
       cd cmd/gmc && KIND_CLUSTER=$(KIND_CLUSTER) \
           GMC_IMG=$(GMC_IMG) AGC_IMG=$(AGC_IMG) PROXY_IMG=$(PROXY_IMG) FAKEGITHUB_IMG=$(FAKEGITHUB_IMG) \
           go test -v -tags e2e -count=1 -timeout 30m ./test/e2e/... \
           -args -ginkgo.label-filter='!local-only' -ginkgo.procs=6
   ```

3. **Verify namespace uniqueness** — all six suites already use distinct namespace names (`tenant-provisioning`, `tenant-isolation-a/b`, `tenant-rbac`, `tenant-hpa-pdb`, `tenant-resilience`, `tenant-job-lifecycle`). No conflicts expected.

4. **Watch out for `Manager` suite global state** — `e2e_test.go`'s `Manager` `Describe` block labels the `gmc-system` namespace with `pod-security.kubernetes.io/enforce=restricted`. This is idempotent but should be verified as safe when running concurrently with other suites. The `metricsRoleBindingName` ClusterRoleBinding is also a cluster-scoped resource; if two processes try to create it simultaneously one will get a conflict. Guard with `--dry-run` or `IgnoreAlreadyExists`.

5. **`fakegithub` port-forward per process** — with parallel execution each process may need to access the fakegithub control API at the same time. The current approach (spin up a port-forward per call) works but is slow (see §2). After implementing §2, each process maintains its own local port on a distinct port number. Use `GinkgoParallelProcess()` to derive a unique local port: `19090 + GinkgoParallelProcess()`.

### Files

- `cmd/gmc/test/e2e/e2e_suite_test.go` — replace `BeforeSuite`/`AfterSuite`
- `Makefile` — add `-ginkgo.procs=6`
- `cmd/gmc/test/e2e/job_lifecycle_test.go` — update port derivation for fakegithub control API

---

## 2. Persistent port-forward for fakegithub control API ✓

**Estimated savings: 1–3 min** — **Implemented.**

### Problem

`fakegithubControlRequest` starts a new `kubectl port-forward` process, sleeps 500 ms for it to establish, makes one HTTP call, then kills the process — for every single call. Across `E2E_AGC_SessionRegistered`, `E2E_AGC_JobDelivered`, and `E2E_AGC_MultipleJobsQueued`, this is called 10–15 times = **5–8 seconds of pure sleep**, plus port-forward startup variance that can cause flakiness.

### Approach

Start one port-forward in the job lifecycle suite's `BeforeAll` and keep it alive until `AfterAll`. Expose a package-level `fakegithubLocalPort` variable. All helper functions use that port directly without spawning a new process.

With parallel execution (§1), derive the port from `GinkgoParallelProcess()` to avoid collision.

### Implementation steps

1. **Add suite-level port-forward lifecycle to `job_lifecycle_test.go`**:

   ```go
   var fakegithubPFCmd *exec.Cmd
   var fakegithubLocalPort string

   // in BeforeAll, after setupFakegithub is confirmed ready:
   fakegithubLocalPort = fmt.Sprintf("%d", 19090+GinkgoParallelProcess())
   fakegithubPFCmd = exec.Command("kubectl", "port-forward",
       "-n", infraNamespace,
       "service/"+fakegithubServiceName,
       fakegithubLocalPort+":9090",
   )
   Expect(fakegithubPFCmd.Start()).To(Succeed())
   // Wait for port to be listening.
   Eventually(func() error {
       resp, err := http.Get("http://localhost:" + fakegithubLocalPort + "/control/sessions")
       if err != nil { return err }
       resp.Body.Close()
       return nil
   }, 15*time.Second, 500*time.Millisecond).Should(Succeed())

   // in AfterAll:
   if fakegithubPFCmd != nil && fakegithubPFCmd.Process != nil {
       _ = fakegithubPFCmd.Process.Kill()
   }
   ```

2. **Simplify `fakegithubControlRequest`** — remove the port-forward lifecycle from inside the function; just use `fakegithubLocalPort` directly. Remove the `time.Sleep(500ms)`.

3. **Retry on HTTP error** — a single retry with 100 ms back-off handles rare cases where the persistent port-forward is momentarily unavailable (e.g., the pod was restarted by a resilience test running in parallel).

### Files

- `cmd/gmc/test/e2e/job_lifecycle_test.go` — add `BeforeAll`/`AfterAll` port-forward lifecycle, update `fakegithubControlRequest` to use `fakegithubLocalPort`

---

## 3. Reduce polling interval from 5 s to 2 s ✓

**Estimated savings: 1–2 min** — **Implemented.**

### Problem

Every `SetDefaultEventuallyPollingInterval(5 * time.Second)` means that when a condition is satisfied at time T, the test doesn't detect it until up to T + 5 s. The suite has roughly 30 `Eventually` assertions. With 5s polling the expected overshoot per assertion is ~2.5 s; with 2 s polling it drops to ~1 s, saving ~45 s per assertion in the worst case. Total expected savings across all assertions: **~1–2 min**.

### Implementation steps

1. Change every `SetDefaultEventuallyPollingInterval(5 * time.Second)` to `2 * time.Second` across all test files:
   - `provisioning_test.go`
   - `isolation_test.go`
   - `resilience_test.go`
   - `rbac_e2e_test.go`
   - `hpa_pdb_test.go`
   - `job_lifecycle_test.go`
   - `e2e_test.go` (uses `time.Second` already — no change needed)

2. Leave `hpa_pdb_test.go`'s HPA-load test at 10 s — HPA scale decisions come from metrics-server which has its own scrape interval, so polling faster provides no benefit there.

3. Reduce `fakegithubActiveSessions` poll from 5 s to 2 s inline (after §2 removes the 500 ms port-forward startup cost, faster polling is cheap).

### Files

- All `cmd/gmc/test/e2e/*_test.go` files except `hpa_pdb_test.go`'s local-only HPA scale test

---

## 4. Single-worker kind cluster for CI ✓

**Estimated savings: ~1 min** — **Implemented.**

### Problem

`test/kind-config.yaml` provisions 1 control-plane + 2 worker nodes. The only test that requires 2 workers is `E2E_GMC_ProxyPodScheduledOnWorker` — already tagged `local-only` and excluded from CI. A 2-node cluster (1 control-plane + 1 worker) spins up ~30–45 s faster and loads images faster (one fewer node to push to).

### Implementation steps

1. **Add `test/kind-config-ci.yaml`**:

   ```yaml
   kind: Cluster
   apiVersion: kind.x-k8s.io/v1alpha4
   nodes:
     - role: control-plane
     - role: worker
   ```

2. **Update `.github/workflows/e2e-test.yml`** to pass the CI config:

   ```yaml
   - name: Create kind cluster
     run: make e2e-cluster KIND_CLUSTER=actions-gateway-e2e KIND_CONFIG=test/kind-config-ci.yaml
   ```

3. **Keep `test/kind-config.yaml`** (3-node) as the default for local development where `local-only` tests run.

4. **Update `Makefile`** to document the two configs:

   ```makefile
   # KIND_CONFIG defaults to the 3-node local config.
   # CI uses KIND_CONFIG=test/kind-config-ci.yaml (2-node, no local-only tests).
   KIND_CONFIG ?= test/kind-config.yaml
   ```

### Files

- `test/kind-config-ci.yaml` — new file
- `.github/workflows/e2e-test.yml` — add `KIND_CONFIG=test/kind-config-ci.yaml`
- `Makefile` — update comment

---

## 5. Docker layer caching in CI ✓

**Estimated savings: 3–6 min on repeated runs** — **Implemented.**

### Problem

Each CI run builds four Docker images from scratch. The Go compilation layer — the heaviest — is invalidated whenever any `.go` file changes, which is every PR. However, the `go mod download` / vendor layer is stable across most commits and could be cached. GitHub Actions supports build cache via `actions/cache` or Docker's `--cache-from` with a registry.

### Approach

Use GitHub Actions' built-in Docker layer cache. The `docker/build-push-action` action supports `cache-from: type=gha` and `cache-to: type=gha,mode=max`, which stores layer blobs in the GitHub Actions cache without requiring a container registry.

Alternatively, add `--cache-from=type=local,src=/tmp/.buildx-cache` with a matching `actions/cache` step — simpler but requires manual cache key management.

The recommended approach is `docker/build-push-action` with `type=gha`.

### Implementation steps

1. **Add a `docker/setup-buildx-action` step** before image builds:

   ```yaml
   - name: Set up Docker Buildx
     uses: docker/setup-buildx-action@v3
   ```

2. **Replace `make e2e-images` with per-image build steps** using `docker/build-push-action`:

   ```yaml
   - name: Build GMC image
     uses: docker/build-push-action@v6
     with:
       context: .
       file: cmd/gmc/Dockerfile
       tags: ${{ env.GMC_IMG }}
       load: true
       cache-from: type=gha,scope=gmc
       cache-to: type=gha,mode=max,scope=gmc

   - name: Build AGC image
     uses: docker/build-push-action@v6
     with:
       context: .
       file: cmd/agc/Dockerfile
       tags: agc:e2e
       load: true
       cache-from: type=gha,scope=agc
       cache-to: type=gha,mode=max,scope=agc

   # ... proxy and fakegithub similarly
   ```

3. **Set `GMC_IMG` env var** at the job level using the git SHA, matching the Makefile convention:

   ```yaml
   env:
     GMC_IMG: gmc:e2e-${{ github.sha }}
   ```

4. **Remove `make e2e-images` step** (replaced by individual build steps above).

5. **Keep `make e2e-load-images`** unchanged — it still loads the locally-built images into kind.

### Files

- `.github/workflows/e2e-test.yml` — add Buildx setup, replace `make e2e-images` step

### Note on savings

The first run of a branch after the cache is primed sees the full benefit. A cold cache run (new Dockerfile, new Go dependencies) is no slower than today. The `go mod download` and base image layers are typically 60–70% of build time, so cached runs of PRs that only change `.go` files should save 3–6 min.

---

## 6. Fix `--procs` count: 8 suites, not 6

**Estimated savings: ~2–3 min**

### Problem

The Makefile uses `--procs=6` but there are 8 `Describe` blocks that run in CI:
`Manager`, `E2E_GMC_Provisioning`, `E2E_GMC_Isolation`, `E2E_GMC_RBAC`,
`E2E_GMC_HPA_PDB`, `E2E_GMC_Resilience`, `E2E_AGC_JobLifecycle`, and
`E2E_GMC_Teardown`. (`E2E_GitHub_RealDispatch` self-skips without credentials.)

With fewer processes than ordered containers, Ginkgo places 2 suites on 2
processes, serialising them and negating part of the parallelism benefit.

### Implementation steps

1. Change `-ginkgo.procs=6` to `-ginkgo.procs=8` in the `e2e` Makefile target.
   Alternatively use `-ginkgo.procs=-1` (auto-detect based on CPU count) so the
   count doesn't need to be updated when suites are added.

### Files

- `Makefile` — update `-ginkgo.procs=6` to `-ginkgo.procs=8`

---

## 7. Reduce `WaitForDeploymentReady` polling from 5 s to 2 s

**Estimated savings: ~45 s**

### Problem

`WaitForDeploymentReady` in `cmd/gmc/test/utils/resources.go` has a hardcoded
5 s polling interval. Improvement 3 reduced `SetDefaultEventuallyPollingInterval`
to 2 s across all test files, but this utility function was missed. It is the
most-called waiter in the suite — every `BeforeAll` invokes it.

### Implementation steps

1. Change the hardcoded `5*time.Second` interval in `WaitForDeploymentReady` to
   `2*time.Second`.

### Files

- `cmd/gmc/test/utils/resources.go` — line with `}, timeout, 5*time.Second`

---

## 8. Parallel deployment waits in the isolation suite

**Estimated savings: ~1–2 min**

### Problem

`E2E_GMC_TwoTenantsIndependentResources` in `isolation_test.go` calls
`WaitForDeploymentReady` for `nsA` then `nsB` sequentially, even though both
deployments were applied concurrently in `BeforeAll`:

```go
utils.WaitForDeploymentReady(nsA, "actions-gateway-proxy", 4*time.Minute)
utils.WaitForDeploymentReady(nsB, "actions-gateway-proxy", 4*time.Minute)
```

The second wait starts only after the first completes, adding up to one full
deployment-ready wait unnecessarily.

### Implementation steps

1. Check both deployments in a single `Eventually` that asserts ready replicas
   for both `nsA` and `nsB` in one closure, so both are polled together:

   ```go
   It("E2E_GMC_TwoTenantsIndependentResources: each tenant has its own proxy deployment", func() {
       Eventually(func(g Gomega) {
           for _, ns := range []string{nsA, nsB} {
               cmd := exec.Command("kubectl", "get", "deployment", "actions-gateway-proxy",
                   "-n", ns, "-o", "jsonpath={.status.readyReplicas}")
               out, err := utils.Run(cmd)
               g.Expect(err).NotTo(HaveOccurred())
               g.Expect(out).NotTo(BeEmpty())
               g.Expect(out).NotTo(Equal("0"))
           }
       }, 4*time.Minute, 2*time.Second).Should(Succeed())
   })
   ```

### Files

- `cmd/gmc/test/e2e/isolation_test.go`

---

## 9. Merge sequential teardown checks into one `Eventually`

**Estimated savings: ~30 s**

### Problem

`E2E_GMC_DeleteCRRemovesResources` in `teardown_test.go` uses four separate
`Eventually` blocks to check that proxy Deployment, AGC Deployment,
NetworkPolicy, and Service are removed after CR deletion. All four resources
disappear at roughly the same time (when the finalizer clears). Running four
separate polling loops serialises checks that could overlap.

### Implementation steps

1. Replace the four `Eventually` blocks with a single one that checks all four
   resources in one closure:

   ```go
   Eventually(func(g Gomega) {
       g.Expect(utils.ResourceExists("deployment",    tenantNS, "actions-gateway-proxy")).To(BeFalse())
       g.Expect(utils.ResourceExists("deployment",    tenantNS, "actions-gateway-agc")).To(BeFalse())
       g.Expect(utils.ResourceExists("networkpolicy", tenantNS, "actions-gateway")).To(BeFalse())
       g.Expect(utils.ResourceExists("service",       tenantNS, "actions-gateway-proxy")).To(BeFalse())
   }, 3*time.Minute, 2*time.Second).Should(Succeed())
   ```

### Files

- `cmd/gmc/test/e2e/teardown_test.go`

---

## 10. Isolate GMC controller restart from parallel suites

**Estimated savings: variance reduction (not raw speed)**

### Problem

`E2E_GMC_GMCRestartPreservesState` in `resilience_test.go` calls
`kubectl rollout restart deployment/gmc-controller-manager` against the shared
`gmc-system` namespace. During the ~60 s the controller pod is cycling, every
other parallel suite's reconcile loop stalls. Other suites have generous
`Eventually` timeouts so they should survive, but this adds timing variance and
can cause flakiness on a loaded CI runner.

### Approach

Label the resilience suite `Serial` (Ginkgo runs `Serial` specs only after all
parallel specs complete) or add a `local-only` label to the restart test so it
is excluded from CI. The pure-observation tests in `resilience_test.go`
(`E2E_GMC_ProxyRecoversAfterPodDelete`) can remain parallel.

### Files

- `cmd/gmc/test/e2e/resilience_test.go` — add `Label("local-only")` to the
  GMC restart `It` block, or wrap it in a `Serial` container

---

## 11. GitHub Actions log grouping (`--github-output`)

**Estimated savings: readability improvement, not raw speed**

### Problem

With 8 parallel processes each emitting `By(...)` steps, spec start/end lines,
and failure output, the Actions log is a stream of interleaved text from 8
sources. It is difficult to find which suite failed or which step a slow spec
is stuck on.

### Approach

Ginkgo v2's `--github-output` flag emits GitHub Actions workflow commands
(`::group::` / `::endgroup::` / `::error::`) around each spec. In the Actions
log viewer every suite collapses to a single line; failed suites expand
automatically. This is the lowest-effort improvement to CI observability.

### Implementation steps

1. Add `-ginkgo.github-output` to the `-args` list in the `e2e` Makefile target.

### Files

- `Makefile` — add `-ginkgo.github-output` to the `e2e` target

---

## 12. Progress reporting for slow specs (`--poll-progress-after`)

**Estimated savings: faster diagnosis, not raw speed**

### Problem

When a long-running spec (e.g. `WaitForDeploymentReady`) stalls in a parallel
process, there is no signal in the log until the timeout fires — which can be
3–4 minutes later. By that point other suites have already finished and the
failure is hard to attribute to a specific `By(...)` step.

### Approach

Ginkgo v2's `--poll-progress-after=Xs` flag prints a progress report for any
spec that has not completed within X seconds. The report shows the current
`By(...)` step and a goroutine trace, making it immediately clear which step is
hanging and in which process.

A 60 s threshold catches genuine stalls (deployment waits should complete well
under 3 min) without producing noise on healthy runs.

### Implementation steps

1. Add `-ginkgo.poll-progress-after=60s` to the `-args` list in the `e2e`
   Makefile target.

### Files

- `Makefile` — add `-ginkgo.poll-progress-after=60s` to the `e2e` target

---

## 13. JUnit report for PR test summary

**Estimated savings: readability improvement, not raw speed**

### Problem

After a parallel run there is no structured summary — pass/fail is buried in
hundreds of lines of log output. Reviewers and authors have to scroll to find
out which specific tests failed.

### Approach

Ginkgo v2's `-ginkgo.junit-report=e2e-report.xml` writes a JUnit XML file.
GitHub Actions can render this as a test summary table in the PR sidebar using
`actions/upload-artifact` and the built-in test reporter (no third-party action
required as of GitHub Actions runner v2.308+).

### Implementation steps

1. Add `-ginkgo.junit-report=/tmp/e2e-report.xml` to the `e2e` Makefile target.

2. Add an upload step to `.github/workflows/e2e-test.yml` after the `make e2e`
   step (runs even on failure):

   ```yaml
   - name: Upload e2e test report
     if: always()
     uses: actions/upload-artifact@v4
     with:
       name: e2e-junit-report
       path: /tmp/e2e-report.xml
   ```

### Files

- `Makefile` — add `-ginkgo.junit-report=/tmp/e2e-report.xml` to the `e2e` target
- `.github/workflows/e2e-test.yml` — add upload step

---

## 14. Replace CPU-burn HPA scale-up test with deterministic minReplicas patch

**Estimated savings: removes flakiness; unlocks running in CI**

### Problem

`E2E_GMC_HPAScalesUpUnderLoad` burns CPU inside the proxy pod and waits for
HPA to observe utilisation above its threshold and scale up. This is inherently
flaky because it depends on two independent scrape windows (metrics-server at
15 s, kube-controller-manager HPA sync at 15 s) both seeing high-enough CPU at
the same time — which is not guaranteed on a loaded CI runner where the `yes`
loop competes with everything else.

The test also loses the CPU-threshold coverage that justified it. That gap
belongs in a unit test on `buildHPA`, not in a slow integration loop.

### Approach

Patch `HPA.spec.minReplicas` from 1 to 2 directly. This bypasses the
metrics-server loop entirely and drives the HPA→Deployment control path
deterministically. Restore to 1 in `DeferCleanup`. Remove the `local-only`
label so it runs in CI.

Add unit tests in `builder_test.go` to cover the HPA spec fields the e2e test
no longer exercises: `ScaleTargetRef`, `Resource.Name`, `Target.Type`, and the
default min/max replica values.

### Implementation steps

1. **Replace the `It` block in `hpa_pdb_test.go`**:

   ```go
   It("E2E_GMC_HPADrivesScaleUp: HPA drives proxy Deployment replica count", func() {
       By("patching HPA minReplicas to 2 to trigger scale-up")
       cmd := exec.Command("kubectl", "patch", "hpa", "actions-gateway-proxy",
           "-n", tenantNS, "--type=merge", "-p", `{"spec":{"minReplicas":2}}`)
       _, err := utils.Run(cmd)
       Expect(err).NotTo(HaveOccurred())
       DeferCleanup(func() {
           cmd := exec.Command("kubectl", "patch", "hpa", "actions-gateway-proxy",
               "-n", tenantNS, "--type=merge", "-p", `{"spec":{"minReplicas":1}}`)
           _, _ = utils.Run(cmd)
       })

       By("waiting for HPA to report 2 current replicas")
       Eventually(func(g Gomega) {
           cmd := exec.Command("kubectl", "get", "hpa", "actions-gateway-proxy",
               "-n", tenantNS, "-o", "jsonpath={.status.currentReplicas}")
           out, err := utils.Run(cmd)
           g.Expect(err).NotTo(HaveOccurred())
           g.Expect(out).To(Equal("2"), "HPA has not driven scale-up yet")
       }, 2*time.Minute, 2*time.Second).Should(Succeed())
   })
   ```

2. **Add unit tests to `builder_test.go`** covering the fields no longer
   exercised by the e2e test:

   ```go
   func TestBuildHPA_ScaleTargetRef(t *testing.T) { ... }
   func TestBuildHPA_MetricTypeAndResourceName(t *testing.T) { ... }
   func TestBuildHPA_DefaultMinMaxReplicas(t *testing.T) { ... }
   ```

### Files

- `cmd/gmc/test/e2e/hpa_pdb_test.go` — replace CPU-burn `It`, remove `local-only`
- `cmd/gmc/internal/controller/builder_test.go` — add three HPA unit tests

---

## Recommended implementation order

| # | Change | Effort | Savings | Status |
|---|---|---|---|---|
| 3 | Reduce polling interval | 15 min | ~1–2 min | ✓ Done |
| 4 | Single-worker CI cluster | 15 min | ~1 min | ✓ Done |
| 2 | Persistent port-forward | 1 hour | ~1–3 min | ✓ Done |
| 5 | Docker layer cache | 2 hours | ~3–6 min (cached runs) | ✓ Done |
| 1 | Ginkgo parallel execution | 4–6 hours | ~10–15 min | ✓ Done |
| 7 | `WaitForDeploymentReady` polling 5s → 2s | 5 min | ~45 s | ✓ Done |
| 6 | Fix `--procs` count (6 → 8) | 5 min | ~2–3 min | ✓ Done |
| 11 | GitHub Actions log grouping | 5 min | readability | ✓ Done |
| 12 | Progress reporting for slow specs | 5 min | faster diagnosis | ✓ Done |
| 9 | Merge teardown `Eventually` blocks | 15 min | ~30 s | ✓ Done |
| 8 | Parallel isolation deployment waits | 15 min | ~1–2 min | ✓ Done |
| 13 | JUnit report for PR test summary | 30 min | readability | ✓ Done |
| 10 | Isolate GMC restart from parallel run | 30 min | variance reduction | ✓ Done |
| 14 | Deterministic HPA scale-up test | 1 hour | removes flakiness; CI-safe | ✓ Done |

---

## Round 2 — CI pipeline (job-level) optimizations

Round 1 (above) optimized the test phase. This round targets the **setup/scaffolding** around it, with a hard constraint: *no increase in billed Actions minutes*. The e2e job runs on a single `ubuntu-latest` runner, so wall-clock == billed minutes 1:1 — every change below cuts both, and none shard across runners or upsize the runner (the two moves that would trade minutes for latency).

| # | Change | Effort | Savings | Status |
|---|---|---|---|---|
| 15 | `concurrency: cancel-in-progress` (PR-scoped) | 5 min | up to ~35 min per superseded PR push | ✓ Done |
| 16 | Overlap image build with cluster create + cert-manager | 1 hour | ~2 min/run (cold cache) | ✓ Done |
| 17 | Pin `kind` to a release binary (was `go install @latest`) | 15 min | ~20–40 s + reproducibility | ✓ Done |
| 18 | Cache the kind node image across runs | 15 min | node-image pull time + Docker Hub rate-limit flake removal | ✓ Done |

### 15. Cancel superseded PR runs

`.github/workflows/e2e-test.yml` had no `concurrency` block, so every push to an open PR ran the full ~35 min job to completion even after a newer push made it moot. Added a workflow-level `concurrency` group keyed on `github.ref`, with `cancel-in-progress` gated to `pull_request` events so pushes to `main` (each a distinct post-merge gate) are never cancelled. The key uses `github.ref` rather than `github.head_ref`: for a PR it resolves to the unique `refs/pull/<n>/merge`, so a fork PR cannot pick a colliding source-branch name to cancel another PR's in-progress runs. Pure minutes saver, zero latency cost.

### 16. Overlap build with cluster bring-up

The image build (`docker buildx bake`) is the long pole on a cold GHA cache (~8 min) and depends only on the local registry, not the kind cluster. The registry bring-up was split out of `scripts/kind-with-registry.sh` into `scripts/start-registry.sh` (and a `make e2e-registry` target) so CI can start the registry first, kick the bake off in the background, and create the cluster + apply cert-manager (~2 min) underneath it. A `trap` kills the background build if cluster bring-up fails; the build's exit code is surfaced via `wait`.

### 17. Pin kind to a release binary

`go install sigs.k8s.io/kind@latest` compiled kind from source every run (~20–40 s) and pinned nothing — a new kind release could change CI behavior with no code change. Replaced with a checksum-verified download of a pinned `kind` release binary (`KIND_VERSION`, `KIND_BINARY_SHA256` in the workflow env).

### 18. Cache the kind node image

The ~1 GB `kindest/node` image was pulled from Docker Hub on every run — slow and a flake source under Hub rate limits. Added an `actions/cache` step keyed on the digest-pinned `KIND_NODE_IMAGE`, with a load-or-pull step that `docker save`s the image on a miss and `docker load`s it on a hit. The cluster is created with `KIND_NODE_IMAGE` set (via the new optional flag threaded through `kind-with-registry.sh`), pinning the cluster's K8s version. kind is handed the **tag-only** ref because `docker save`/`load` drops the registry digest, so a `@sha256:` ref would miss the pre-loaded image and trigger a re-pull; the pull/save still use the digest, so the cached content is pinned.

> Not done: caching the cert-manager images. Their pull already overlaps the background build (§16), and caching them would mean reintroducing a slow `kind load` or containerd surgery for little wall-clock gain.
