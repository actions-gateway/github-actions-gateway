# GAG dogfood e2e — worker-isolation reference architectures

Two ways to run GAG's own e2e suite (`kind`-in-Docker-in-Docker) **on GAG itself**, sharing one base and differing only in the **worker isolation** mechanism.
Deploy one or the other; the overlay directory *is* the architectural diff.

```
deploy/dogfood-e2e/
  base/                 # isolation-agnostic: namespace, quota, ActionsGateway, RunnerSet
  overlays/
    dind/               # privileged DinD  — simple, NO isolation (trusted CI only)   ← opt-in fallback
    kata/               # Kata micro-VM    — strong isolation (untrusted PRs)          ← the default (live-validated, Q286)
```

**The worker pod shape is not here.** Each overlay consumes a [shipped library entry](../templates/README.md) as a second base (`../../../templates/kata-dind`, `../../../templates/privileged-dind`) and patches in only this cluster's specifics: the build-capable runner image, the GKE node pool and taint, and (kata) the block StorageClass.
That inversion is the point: an operator applying `deploy/templates/kata-dind` gets byte-for-byte what this suite runs jobs on, minus declared patches, so the shipped artifact cannot silently drift from its validation (Q554).

Two consequences worth knowing before editing an overlay:

- **Patch the template with JSON 6902, never a strategic merge.** kustomize has no schema for a CRD, so a strategic merge degrades to an RFC 7386 JSON merge patch and replaces lists wholesale: a patch naming `initContainers` drops the dind container's image, restartPolicy, capabilities and probe, at exit 0.
  `make template-library-check` fails on it, and also on an overlay that stops consuming the library.
- The template objects are named for the library entry (`kata-dind`, `privileged-dind`), not for this tenant.

## The two variants

| | **dind** (privileged) | **kata** |
|---|---|---|
| Isolation | none — host kernel exposure | KVM micro-VM (`kata` RuntimeClass) |
| Security profile | `privileged` (+ eligibility label + downgrade annotation) | same labels¹ — but **no privileged container** behind them |
| Nodes | any | nested-virt only (`c2-standard-8` + `--enable-nested-virtualization` + Workload Identity) |
| Extra cluster setup | none | Kata DaemonSet + `kata`/`kata-qemu` RuntimeClasses (`e2e-setup.sh`) |
| DinD sidecar | `privileged: true` | unprivileged (micro-VM is the boundary); six-step entrypoint; ephemeral Block PVC |
| CPU sizing | requests-only (bursts) | explicit CPU **limits** — Kata sizes the guest's vCPUs from them |
| Runner CPU request | derived — the shared `NodeShare` profile overrides the template² | derived the same way; the CPU **limit** is untouched, so guest vCPU sizing is unaffected |
| Egress | broad (open) — trusted only | same for now; the untrusted-PR posture needs a tighter policy + in-cluster mirror (future) |
| Cost | lower | higher (C2 + nested virt + 100Gi ephemeral PD per worker) |
| Use for | **trusted CI / dogfood only** | **untrusted / OSS PRs** (once egress is tightened) |

¹ PSS **baseline** forbids the capability adds the unprivileged dockerd needs (`SYS_ADMIN`, `NET_ADMIN`, `SYS_RESOURCE`, `SYS_PTRACE`, `NET_RAW`) and PSA is not Kata-aware, so the namespace still needs the privileged PSA level (envtest-verified — see [`templates/kata-dind/template.yaml`](../templates/kata-dind/template.yaml)).
What pins the pod unprivileged is the platform-owned `ClusterRunnerTemplate`, not the PSA label.

² The shared RunnerSet in [base/resources.yaml](base/resources.yaml) selects the `NodeShare` sizing profile, so the runner container's CPU **request** is derived (envelope ÷ `workersPerNode`) rather than taken from the template.
This tenant is where the release gate validates that profile — it needs no usage history, so it actuates on the first job, and `validate-release.sh` fails the RC if it does not.
The declared envelope is deliberately **below** both variants' static request, so actuation can only lower a worker's ask; tightening it needs a measured node allocatable ([Q448](../../docs/STATUS.md#Q448)).

Both use one build-capable runner image (`scripts/dogfood/e2e-runner/Dockerfile` — docker CLI + buildx + helm + kubectl + jq; added by the Q231 wiring PR) and the wired [`e2e-reusable.yml`](../../.github/workflows/e2e-reusable.yml) (`GAG_E2E_RUNNER`).

To see the difference: `diff -r overlays/dind overlays/kata`, or diff the rendered output (`kubectl kustomize overlays/dind` vs `overlays/kata`).

## Deploy

Prerequisites not expressed in kustomize (they're cluster infra / credentials):

1. **Node pool** — `scripts/dogfood/e2e-setup.sh` provisions one pool that serves both variants: `c2-standard-8` spot, tainted `dedicated=e2e`, with `--enable-nested-virtualization` + `--workload-metadata=GKE_METADATA` (hard prerequisites for `kata`; inert for `dind`), plus the Kata DaemonSet and the `kata`/`kata-qemu` RuntimeClasses.
2. **App credential Secret** (not in git):
   ```bash
   kubectl create secret generic github-app-v1 -n gag-dogfood-e2e \
     --from-literal=appId=$APP_ID --from-literal=installationId=$INSTALLATION_ID \
     --from-file=privateKey=app.pem
   ```
   (The namespace must exist first: `kubectl create ns gag-dogfood-e2e`.)

Then apply the variant:

```bash
kubectl apply -k deploy/dogfood-e2e/overlays/dind   # or .../overlays/kata
```

Route e2e to it: `gh variable set GAG_E2E_RUNNER --body '"gag-ci-e2e"'` (unset ⇒ github-hosted).
The RunnerSet is authored at **v2beta1** — ScaleSet-only, so it declares exactly one `runnerLabel` (`gag-ci-e2e`), and `GAG_E2E_RUNNER` is that single JSON string, not a Classic multi-label array.
Prefer the on-demand `scripts/dogfood/e2e-start.sh` / `e2e-stop.sh`, which set this **and** spin the tenant AGC up/down (Q231); `E2E_VARIANT=kata|dind` selects the overlay (default `kata` since the Q286 flip; `dind` is the explicit opt-in fallback for environments without nested virtualization).
This is a dogfood/dev config, not a shipped product install.

### Why `kubectl apply -k`, not a standalone kustomize binary

These overlays (and [`deploy/athens/`](../athens/kustomization.yaml)) are rendered by kubectl's **embedded** kustomize; the repo deliberately carries no standalone `kustomize` binary (the product install path is Helm-only — [drop-kustomize](../../docs/plan/archive/drop-kustomize.md), Q142).
Deliberate because:

- Every kustomization here uses only `resources` lists + inline strategic-merge `patches` — semantics frozen since kustomize v3.
  The embedded copy's lag behind standalone releases (a few months per kubectl minor since 1.21 — the years-stale reputation dates from the ≤1.20 era's frozen v2.0.3) can't bite features we don't use.
- The embedded version is pinned by the kubectl version, which is already a managed, registered dependency.
  An unpinned standalone binary would *add* version skew across contributor machines and CI, not remove it.
- One less host dependency to install, register in `scripts/ci/check-tools.sh`, and supply-chain-audit.

**Revisit trigger:** an overlay needs a kustomize feature or bugfix the embedded copy doesn't have.
Then re-add the binary via the `scripts/ci/check-tools.sh` registry — version-pinned this time — rather than working around it.

## Load-bearing caveats (learned the hard way, 2026-06-30)

- **The DinD sidecar MUST be a native sidecar** (`restartPolicy: Always` init container, K8s ≥1.29).
  A regular sidecar's `dockerd` never exits, so the pod never completes, the AGC keeps the session active, and `maxWorkers` strands.
  Validated: the AGC preserves the native-sidecar init container through its Q235 wrapper injection.
  GAG warns (non-blocking) when a template declares a regular reap-blocking sidecar — see [Q249: reap-blocking-sidecar warning](../../docs/plan/archive/worker-sidecar-reap-warning.md) and [Sidecar containers must be native sidecars](../../docs/operations/in-runner-image-builds.md#sidecar-containers-must-be-native-sidecars-q249).
- **Privileged is platform-gated four ways** — the namespace needs `tenant=managed`, `security-profile=privileged`, the platform `privileged-profile=allowed` label, **and** the `allow-profile-downgrade=allowed` annotation; and the privileged pod shape must be a cluster-scoped `ClusterRunnerTemplate` (a namespaced `RunnerTemplate` refuses privileged containers).
  Tenants cannot self-elevate.
- **e2e needs broad egress** (Docker Hub / quay / registry.k8s.io / helm CDN), which GAG's default-deny + GitHub-only worker `NetworkPolicy` blocks and v2 has no opt-out for.
  The `dind` overlay opens egress additively — **trusted-only**.
  The durable answer (in-cluster mirror; GKE's FQDN enforcement needs the opt-in `--enable-fqdn-network-policy`, which dogfood does not enable — [Q245](../../docs/plan/q245-fqdn-intent-backend-split.md)) is the hardened path that pairs with the Kata variant.

## Status

- **dind:** privileged DinD confirmed working on GKE COS cgroup v2 (daemon up, native-sidecar reaping verified).
  Full **Calico e2e ran clean-green on GAG** (2026-07-07): pod `Completed`, no OOM, ~18 min.
  The [Q247](../../docs/plan/archive/gke-dogfood-turnup-findings.md) session-orphaning was intermittent, not deterministic — this run concluded cleanly; Q247's renewal + self-teardown fixes (resolved) hardened it.
- **Pod sizing is measured, not guessed** ([Q248](../../docs/STATUS.md#Q248)): the worker pod's `requests`/`limits`, now shipped in [`templates/privileged-dind/template.yaml`](../templates/privileged-dind/template.yaml), are derived from that run's peak — the `runner` is CPU-heavy (~4940m peak, the e2e specs + CLI), the `dind` sidecar is memory-heavy (~2343Mi peak, the in-DinD `kind`+Calico cluster).
  CPU is requests-only (no limit → bursts); `runner(3)+dind(1)=4` vCPU of requests packs exactly one worker pod per `e2-standard-8` node, so the two `maxWorkers` pods land on two nodes and concurrent e2e legs don't CPU-throttle each other.
  Full rationale + the measured table: [dogfood-runner-rightsizing.md § e2e worker sizing](../../docs/plan/dogfood-runner-rightsizing.md#e2e-worker-sizing--measured-then-derived-dind-2026-07-07).
- **kata:** live-validated green end-to-end on GAG (2026-07-16, Q286) and now the default variant.
  The live session root-caused and fixed five wiring defects (Workload Identity prerequisite, helm label typing, kata-deploy tolerations, autoscale-from-zero labeling, busybox-blkid skipping the mkfs) and added a dockerd `startupProbe` so the runner cannot take jobs before the daemon is up — the findings are recorded in [kata-on-gke.md](../../docs/plan/archive/kata-on-gke.md#what-the-live-session-found-2026-07-16).
  Sizing is Kata-specific, not a straight port of the Q248 dind measurements: Kata turns CPU limits into guest vCPUs and memory limits into the guest's whole RAM (page cache included), with none of the dind overlay's burst-to-node headroom — the first live run with the dind-derived split starved the in-dind kind cluster (calico-node probe timeouts).
  The template splits the guest evenly (dind 4 cpu / 8Gi, runner 4 cpu / 4Gi); rationale inline in [`templates/kata-dind/template.yaml`](../templates/kata-dind/template.yaml).

The dogfood e2e path (this tree + `scripts/dogfood/e2e-{setup,start,stop}.sh`) is authored at `actions-gateway.com/v2beta1` (ScaleSet, single `runnerLabel`) and was live-validated green on GAG on 2026-07-07 (Q231, done).
The Kata isolation variant was live-validated green and made the default on 2026-07-17 ([Q286 — archive/kata-on-gke.md](../../docs/plan/archive/kata-on-gke.md)).
