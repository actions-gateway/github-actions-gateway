# GAG dogfood e2e — worker-isolation reference architectures

Two ways to run GAG's own e2e suite (`kind`-in-Docker-in-Docker) **on GAG itself**,
sharing one base and differing only in the **worker isolation** mechanism. Deploy
one or the other; the overlay directory *is* the architectural diff.

```
deploy/dogfood-e2e/
  base/                 # isolation-agnostic: namespace, quota, ActionsGateway, RunnerSet
  overlays/
    dind/               # privileged DinD  — simple, NO isolation (trusted CI only)   ← validated
    kata/               # Kata micro-VM    — strong isolation (untrusted PRs)          ← built (Q286); live AC#5 run pending
```

## The two variants

| | **dind** (privileged) | **kata** |
|---|---|---|
| Isolation | none — host kernel exposure | KVM micro-VM (`kata` RuntimeClass) |
| Security profile | `privileged` (+ eligibility label + downgrade annotation) | same labels¹ — but **no privileged container** behind them |
| Nodes | any | nested-virt only (`c2-standard-8` + `--enable-nested-virtualization` + Workload Identity) |
| Extra cluster setup | none | Kata DaemonSet + `kata`/`kata-qemu` RuntimeClasses (`e2e-setup.sh`) |
| DinD sidecar | `privileged: true` | unprivileged (micro-VM is the boundary); six-step entrypoint; ephemeral Block PVC |
| CPU sizing | requests-only (bursts) | explicit CPU **limits** — Kata sizes the guest's vCPUs from them |
| Egress | broad (open) — trusted only | same for now; the untrusted-PR posture needs a tighter policy + in-cluster mirror (future) |
| Cost | lower | higher (C2 + nested virt + 100Gi ephemeral PD per worker) |
| Use for | **trusted CI / dogfood only** | **untrusted / OSS PRs** (once egress is tightened) |

¹ PSS **baseline** forbids the capability adds the unprivileged dockerd needs
(`SYS_ADMIN`, `NET_ADMIN`, `SYS_RESOURCE`, `SYS_PTRACE`, `NET_RAW`) and PSA is not
Kata-aware, so the namespace still needs the privileged PSA level (envtest-verified —
see [overlays/kata/resources.yaml](overlays/kata/resources.yaml)). What pins the pod
unprivileged is the platform-owned `ClusterRunnerTemplate`, not the PSA label.

Both use one build-capable runner image
(`scripts/dogfood/e2e-runner/Dockerfile` —
docker CLI + buildx + helm + kubectl + jq; added by the Q231 wiring PR) and the wired
[`e2e-reusable.yml`](../../.github/workflows/e2e-reusable.yml) (`GAG_E2E_RUNNER`).

To see the difference: `diff -r overlays/dind overlays/kata`, or diff the rendered
output (`kubectl kustomize overlays/dind` vs `overlays/kata`).

## Deploy

Prerequisites not expressed in kustomize (they're cluster infra / credentials):

1. **Node pool** — `scripts/dogfood/e2e-setup.sh` provisions one pool that serves
   both variants: `c2-standard-8` spot, tainted `dedicated=e2e`, with
   `--enable-nested-virtualization` + `--workload-metadata=GKE_METADATA` (hard
   prerequisites for `kata`; inert for `dind`), plus the Kata DaemonSet and the
   `kata`/`kata-qemu` RuntimeClasses.
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

Route e2e to it: `gh variable set GAG_E2E_RUNNER --body '"gag-ci-e2e"'` (unset ⇒
github-hosted). The RunnerSet is authored at **v2beta1** — ScaleSet-only, so it
declares exactly one `runnerLabel` (`gag-ci-e2e`), and `GAG_E2E_RUNNER` is that
single JSON string, not a Classic multi-label array. Prefer the on-demand
`scripts/dogfood/e2e-start.sh` / `e2e-stop.sh`, which set this **and** spin the
tenant AGC up/down (Q231); `E2E_VARIANT=dind|kata` selects the overlay (default
`dind` until the Q286 flip). This is a dogfood/dev config, not a shipped product
install.

## Load-bearing caveats (learned the hard way, 2026-06-30)

- **The DinD sidecar MUST be a native sidecar** (`restartPolicy: Always` init
  container, K8s ≥1.29). A regular sidecar's `dockerd` never exits, so the pod
  never completes, the AGC keeps the session active, and `maxWorkers` strands.
  Validated: the AGC preserves the native-sidecar init container through its Q235
  wrapper injection. GAG warns (non-blocking) when a template declares a regular
  reap-blocking sidecar — see [Q249: reap-blocking-sidecar warning](../../docs/plan/archive/worker-sidecar-reap-warning.md)
  and [Sidecar containers must be native sidecars](../../docs/operations/in-runner-image-builds.md#sidecar-containers-must-be-native-sidecars-q249).
- **Privileged is platform-gated four ways** — the namespace needs
  `tenant=managed`, `security-profile=privileged`, the platform
  `privileged-profile=allowed` label, **and** the `allow-profile-downgrade=allowed`
  annotation; and the privileged pod shape must be a cluster-scoped
  `ClusterRunnerTemplate` (a namespaced `RunnerTemplate` refuses privileged
  containers). Tenants cannot self-elevate.
- **e2e needs broad egress** (Docker Hub / quay / registry.k8s.io / helm CDN),
  which GAG's default-deny + GitHub-only worker `NetworkPolicy` blocks and v2 has
  no opt-out for. The `dind` overlay opens egress additively — **trusted-only**.
  The durable answer (in-cluster mirror; GKE's FQDN enforcement needs the opt-in
  `--enable-fqdn-network-policy`, which dogfood does not enable — [Q245](../../docs/plan/q245-fqdn-intent-backend-split.md))
  is the hardened path that pairs with the Kata variant.

## Status

- **dind:** privileged DinD confirmed working on GKE COS cgroup v2 (daemon up,
  native-sidecar reaping verified). Full **Calico e2e ran clean-green on GAG**
  (2026-07-07): pod `Completed`, no OOM, ~18 min. The
  [Q247](../../docs/plan/gke-dogfood.md) session-orphaning was intermittent, not
  deterministic — this run concluded cleanly; Q247's renewal + self-teardown
  fixes (resolved) hardened it.
- **Pod sizing is measured, not guessed** ([Q248](../../docs/STATUS.md#Q248)): the
  worker pod's `requests`/`limits` in [`overlays/dind/resources.yaml`](overlays/dind/resources.yaml)
  are derived from that run's peak — the `runner` is CPU-heavy (~4940m peak, the
  e2e specs + CLI), the `dind` sidecar is memory-heavy (~2343Mi peak, the in-DinD
  `kind`+Calico cluster). CPU is requests-only (no limit → bursts); `runner(3)+dind(1)=4`
  vCPU of requests packs exactly one worker pod per `e2-standard-8` node, so the two
  `maxWorkers` pods land on two nodes and concurrent e2e legs don't CPU-throttle each
  other. Full rationale + the measured table:
  [dogfood-runner-rightsizing.md § e2e worker sizing](../../docs/plan/dogfood-runner-rightsizing.md#e2e-worker-sizing--measured-then-derived-dind-2026-07-07).
- **kata:** overlay built ([Q286](../../docs/STATUS.md#Q286)); the remaining gate is
  a live green `make e2e` through it, then the default flip
  (checklist: [kata-on-gke.md](../../docs/plan/kata-on-gke.md#live-validation-checklist-the-remaining-q286-gate)).
  Sizing ports the Q248 measurements onto `c2-standard-8` with explicit CPU limits
  (runner 5 / dind 3) because Kata turns CPU limits into guest vCPUs — the dind
  overlay's requests-only idiom would cap the whole guest at the default vCPU count.

The dogfood e2e path (this tree + `scripts/dogfood/e2e-{setup,start,stop}.sh`)
is authored at `actions-gateway.com/v2beta1` (ScaleSet, single `runnerLabel`) and
was live-validated green on GAG on 2026-07-07 (Q231, done). The Kata isolation
variant remains open under [Q286](../../docs/STATUS.md#Q286).
