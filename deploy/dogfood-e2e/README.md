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
([`scripts/dogfood/e2e-runner/Dockerfile`](../../scripts/dogfood/e2e-runner/Dockerfile) —
docker CLI + buildx + helm + kubectl + jq) and the wired
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

Route e2e to it: `gh variable set GAG_E2E_RUNNER --body '["self-hosted","linux","gag-ci-e2e"]'`
(unset ⇒ github-hosted). This is a dogfood/dev config, not a shipped product install.

## Load-bearing caveats (learned the hard way, 2026-06-30)

- **The DinD sidecar MUST be a native sidecar** (`restartPolicy: Always` init
  container, K8s ≥1.29). A regular sidecar's `dockerd` never exits, so the pod
  never completes, the AGC keeps the session active, and `maxWorkers` strands.
  Validated: the AGC preserves the native-sidecar init container through its Q235
  wrapper injection. See [Q249](../../docs/STATUS.md#Q249).
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
  native-sidecar reaping verified). Full green e2e is pending the
  [Q247](../../docs/STATUS.md#Q247) session-orphaning fix — every run's runner
  completes its work but the job is orphaned at `CompleteJobAsync`.
- **kata:** planned ([Q226](../../docs/STATUS.md#Q226)).

Tracked under [Q231](../../docs/STATUS.md#Q231) (dogfood e2e on GKE).
