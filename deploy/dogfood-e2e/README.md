# GAG dogfood e2e — worker-isolation reference architectures

Two ways to run GAG's own e2e suite (`kind`-in-Docker-in-Docker) **on GAG itself**,
sharing one base and differing only in the **worker isolation** mechanism. Deploy
one or the other; the overlay directory *is* the architectural diff.

```
deploy/dogfood-e2e/
  base/                 # isolation-agnostic: namespace, quota, ActionsGateway, RunnerSet
  overlays/
    dind/               # privileged DinD  — simple, NO isolation (trusted CI only)   ← validated
    kata/               # Kata micro-VM    — strong isolation (untrusted PRs)          ← planned (Q226)
```

## The two variants

| | **dind** (privileged) | **kata** (planned) |
|---|---|---|
| Isolation | none — host kernel exposure | KVM micro-VM (`kata-qemu`) |
| Security profile | `privileged` (+ eligibility label + downgrade annotation) | `baseline` |
| Nodes | any (`e2-standard-8` spot) | nested-virt only (`n2-standard-4` + `--enable-nested-virtualization`) |
| Extra cluster setup | none | Kata DaemonSet + `kata-qemu` RuntimeClass |
| DinD sidecar | `privileged: true` | unprivileged (micro-VM is the boundary) |
| Egress | broad (open) — trusted only | can pair with a tighter policy + in-cluster mirror |
| Cost | lower | higher (N2 + nested virt) |
| Use for | **trusted CI / dogfood only** | **untrusted / OSS PRs** |

Both use one build-capable runner image
(`scripts/dogfood/e2e-runner/Dockerfile` —
docker CLI + buildx + helm + kubectl + jq; added by the Q231 wiring PR) and the wired
[`e2e-reusable.yml`](../../.github/workflows/e2e-reusable.yml) (`GAG_E2E_RUNNER`).

To see the difference: `diff -r overlays/dind overlays/kata`, or diff the rendered
output (`kubectl kustomize overlays/dind` vs `overlays/kata`).

## Deploy

Prerequisites not expressed in kustomize (they're cluster infra / credentials):

1. **Node pool** — `dind`: a normal spot pool tainted `dedicated=e2e` (e.g.
   `e2-standard-8`, **no** nested virt). `kata`: `n2-standard-4` with
   `--enable-nested-virtualization` + the Kata DaemonSet + `kata-qemu` RuntimeClass.
2. **App credential Secret** (not in git):
   ```bash
   kubectl create secret generic github-app-v1 -n gag-dogfood-e2e \
     --from-literal=appId=$APP_ID --from-literal=installationId=$INSTALLATION_ID \
     --from-file=privateKey=app.pem
   ```
   (The namespace must exist first: `kubectl create ns gag-dogfood-e2e`.)

Then apply the variant:

```bash
kubectl apply -k deploy/dogfood-e2e/overlays/dind   # or .../overlays/kata (planned)
```

Route e2e to it: `gh variable set GAG_E2E_RUNNER --body '"gag-ci-e2e"'` (unset ⇒
github-hosted). The RunnerSet is authored at **v2beta1** — ScaleSet-only, so it
declares exactly one `runnerLabel` (`gag-ci-e2e`), and `GAG_E2E_RUNNER` is that
single JSON string, not a Classic multi-label array. Prefer the on-demand
`scripts/dogfood/e2e-start.sh` / `e2e-stop.sh`, which set this **and** spin the
tenant AGC up/down (Q231). This is a dogfood/dev config, not a shipped product
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
  The durable answer (in-cluster mirror; FQDN enforcement is not available on GKE
  Dataplane V2 — [Q245](../../docs/STATUS.md#Q245)) is the hardened path that pairs
  with the Kata variant.

## Status

- **dind:** privileged DinD confirmed working on GKE COS cgroup v2 (daemon up,
  native-sidecar reaping verified). Full **Calico e2e ran clean-green on GAG**
  (2026-07-07): pod `Completed`, no OOM, ~18 min. The
  [Q247](../../docs/STATUS.md#Q247) session-orphaning is intermittent, not
  deterministic — this run concluded cleanly; Q247 tracks hardening it.
- **Pod sizing is measured, not guessed** ([Q248](../../docs/STATUS.md#Q248)): the
  worker pod's `requests`/`limits` in [`overlays/dind/resources.yaml`](overlays/dind/resources.yaml)
  are derived from that run's peak — the `runner` is CPU-heavy (~4940m peak, the
  e2e specs + CLI), the `dind` sidecar is memory-heavy (~2343Mi peak, the in-DinD
  `kind`+Calico cluster). CPU is requests-only (no limit → bursts); `runner(3)+dind(1)=4`
  vCPU of requests packs exactly one worker pod per `e2-standard-8` node, so the two
  `maxWorkers` pods land on two nodes and concurrent e2e legs don't CPU-throttle each
  other. Full rationale + the measured table:
  [dogfood-runner-rightsizing.md § e2e worker sizing](../../docs/plan/dogfood-runner-rightsizing.md#e2e-worker-sizing--measured-then-derived-dind-2026-07-07).
- **kata:** planned ([Q226](../../docs/STATUS.md#Q226)). Note the measured runner
  peak (~5 vCPU) exceeds a whole `n2-standard-4`, so the Kata node in
  [`scripts/dogfood/e2e-setup.sh`](../../scripts/dogfood/e2e-setup.sh) needs to grow
  (e.g. `n2-standard-8`) before that path is sized — the DinD pod requests do not
  port 1:1 to the smaller Kata node.

Tracked under [Q231](../../docs/STATUS.md#Q231) (dogfood e2e on GKE).
