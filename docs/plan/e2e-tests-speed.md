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

## Recommended implementation order

| # | Change | Effort | Savings | Status |
|---|---|---|---|---|
| 3 | Reduce polling interval | 15 min | ~1–2 min | ✓ Done |
| 4 | Single-worker CI cluster | 15 min | ~1 min | ✓ Done |
| 2 | Persistent port-forward | 1 hour | ~1–3 min | ✓ Done |
| 5 | Docker layer cache | 2 hours | ~3–6 min (cached runs) | ✓ Done |
| 1 | Ginkgo parallel execution | 4–6 hours | ~10–15 min | ✓ Done |

Change 1 is the remaining item: replacing `BeforeSuite`/`AfterSuite` with `SynchronizedBeforeSuite`/`SynchronizedAfterSuite` and adding `--procs=6`. It deserves its own PR with careful testing.

Changes 2–4 compound with 1: once parallel execution is in place, each suite's deployment wait overlaps with the others, making the polling interval savings multiply by the number of suites.
