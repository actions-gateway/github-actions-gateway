# In-runner image builds: choosing a build approach and security profile

**Audience:** Platform engineers and tenant owners. **Goal:** pick an
image-build approach (`docker build` and friends) that builds container
images *inside* a GitHub Actions Gateway (GAG) worker pod while staying
inside the strictest Pod Security Admission (PSA) profile that still works.

Building container images is the single most common heavyweight runner
workload, and it collides head-on with pod-security hardening: the classic
recipe — Docker-in-Docker (DinD) with a privileged container — requires the
cluster's least-restrictive posture. It is almost never the approach you
actually need. Rootless build tooling builds the same images under
`baseline` (and sometimes `restricted`) PSA, with no privileged container
and no host-kernel exposure.

This page maps each build approach to the GAG `securityProfile` it
requires, the configuration it needs, and its caveats, then gives a
decision table. The profiles themselves — what each one admits, how the
`privileged` opt-in is gated, and the floor invariants that apply at every
profile — are defined in
[Security § 5.3](../design/05-security.md#53-security-profiles-and-the-privileged-opt-in);
read that first if the three profile names are unfamiliar.

## How the security profiles constrain a build

`ActionsGateway.spec.securityProfile` stamps a PSA `enforce` level on the
tenant namespace. Three values exist, ranked least-to-most restrictive as
`privileged → baseline → restricted`:

| Profile | What it admits | Relevance to builds |
|---|---|---|
| `baseline` *(default)* | No privileged containers, no host namespaces, no `hostPath`, no dangerous capabilities. **Permits** running as root (uid 0) inside the container and `seccompProfile: Unconfined`. | The home of rootless build tooling: BuildKit-rootless, Kaniko, and Sysbox-backed DinD all fit here. |
| `restricted` | The `baseline` set **plus** `runAsNonRoot: true`, `capabilities.drop: [ALL]`, `allowPrivilegeEscalation: false`, and `seccompProfile: RuntimeDefault`/`Localhost` (Unconfined is **forbidden**). | The tightest profile some build tools can meet — but only the ones that need neither root-in-container nor a relaxed seccomp profile. |
| `privileged` | No admission restrictions at all. | The only profile that admits a `privileged: true` container — i.e. classic DinD. High container-escape risk; gated behind a platform-granted namespace label. |

Two GAG behaviours shape what a build pod may do:

- **Floor invariants apply at every profile.** Regardless of
  `securityProfile`, the AGC overwrites every worker pod with
  `hostPID/hostNetwork/hostIPC: false` and projects no Kubernetes API
  token. None of these block image builds, but they mean a build tool can
  never reach for host namespaces as an escape hatch. See
  [Security § Floor invariants](../design/05-security.md#floor-invariants-apply-at-every-profile).
- **Secure-by-default SecurityContext gap-fills, and the tenant can opt
  out.** Under `baseline` the AGC stamps `runAsNonRoot: true` +
  `runAsUser: 1001` by default. A build tool that must run as root in its
  container (Kaniko, a Sysbox-backed daemon) sets `runAsNonRoot: false`
  **explicitly in its own `PodTemplate`** — an explicit value always wins
  over the gap-fill, so this needs **no** profile escalation. See
  [Security § Secure-by-default pod SecurityContext](../design/05-security.md#secure-by-default-pod-securitycontext-and-resources).

## Approach 1 — BuildKit (rootless)

`buildkitd` in rootless mode (`docker buildx` against a rootless BuildKit
instance, or the `moby/buildkit:*-rootless` image as a sidecar/standalone
pod) builds images entirely in user space — no daemon socket, no
privileged container.

- **Recommended profile:** `baseline`. Rootless BuildKit relies on
  unprivileged user namespaces and, on many kernels, needs a relaxed
  seccomp/AppArmor profile for the syscalls it uses to set up those
  namespaces. `baseline` permits `seccompProfile: Unconfined` and the
  `unconfined` AppArmor annotation; `restricted` forbids both.
- **`restricted`?** Best-effort only. On a recent-enough kernel where the
  default seccomp profile (`RuntimeDefault`) already permits the needed
  unshare syscalls and BuildKit runs fully as non-root, it *can* meet
  `restricted` — but this is kernel- and version-dependent, so treat
  `baseline` as the safe target and validate `restricted` on your own
  nodes before relying on it.
- **What it can do:** full `Dockerfile` feature set, build cache,
  multi-stage builds, multi-arch via QEMU/emulation, registry push.
- **What it can't do:** anything requiring a real privileged daemon (it
  doesn't need one). Some host-bind-mount build features are unavailable
  in rootless mode.
- **Config notes:** run `buildkitd` as a non-root user; do **not** set
  `securityContext.privileged`. Registry credentials are read from a
  mounted `~/.docker/config.json` (or a BuildKit secret), never from an
  environment variable — see [Registry authentication](#registry-authentication-for-all-approaches).

## Approach 2 — Kaniko

Kaniko (`gcr.io/kaniko-project/executor`) builds from a `Dockerfile`
without a daemon and **without privileged mode**, executing each build
step inside its own container and snapshotting the filesystem.

- **Recommended profile:** `baseline`. Kaniko extracts the base image and
  executes `RUN` steps against the container's own root filesystem, so it
  conventionally runs as root (uid 0) in its container. Under `baseline`,
  set `runAsNonRoot: false` explicitly in the worker `PodTemplate` (the
  gap-filled default is `runAsNonRoot: true`); no privileged container and
  no profile escalation are required.
- **`restricted`?** Possible with care: Kaniko can run as a non-root user,
  but you must ensure every path it writes is owned by that user (the
  filesystem it manipulates is the container root), which is fragile for
  arbitrary base images. Prefer `baseline` unless a compliance mandate
  forces `restricted` and you control the `Dockerfile`.
- **Caveat — it is not a sandbox.** Kaniko runs `RUN` commands directly in
  the build container with that container's privileges. A malicious
  `Dockerfile` executes with whatever the pod is allowed to do — PSA
  (and, if you need it, a sandbox runtime) is the boundary, not Kaniko
  itself. Do not treat Kaniko as isolation for untrusted build inputs.
- **Caching:** Kaniko caches layers in a registry repo (`--cache=true
  --cache-repo=<registry>/cache`) or a mounted cache volume — not in a
  local daemon. Plan a cache registry path and its credentials.
- **Registry auth:** via a mounted `config.json`; see
  [Registry authentication](#registry-authentication-for-all-approaches).

## Approach 3 — Sysbox (unprivileged DinD via a RuntimeClass)

[Sysbox](https://github.com/nestybox/sysbox) is a container *runtime* that
lets a container run a full inner Docker daemon (or systemd) **without**
the privileged flag, by virtualizing `procfs`/`sysfs` and using the Linux
user-namespace. It is selected per-pod with a `RuntimeClass`.

- **Recommended profile:** `baseline`. A Sysbox-backed pod runs an inner
  daemon as root *inside* the container (mapped to an unprivileged range on
  the host), so it needs root-in-container — which `baseline` permits and
  `restricted` (with its `runAsNonRoot` requirement) does not. Crucially,
  it needs **no** `privileged: true` container, so it stays out of the
  `privileged` profile entirely. This is Sysbox's whole value proposition:
  "real" DinD at `baseline` rather than `privileged`.
- **RuntimeClass × profile interplay:**
  - A platform admin must install the Sysbox runtime on nodes and create
    the `RuntimeClass` (e.g. `sysbox-runc`) cluster-wide. The GMC never
    installs runtime handlers or `RuntimeClass` objects — that is a
    cluster-admin operation, the same model as the gVisor/Kata sandbox
    runtimes in [Appendix B](../design/appendix-b-worker-isolation.md).
  - The tenant references it in the worker `PodTemplate`:
    `spec.runtimeClassName: sysbox-runc`. The AGC honours a
    tenant-set `runtimeClassName` and applies no override that strips it.
  - Sysbox is itself an isolation boundary (user-namespace + filesystem
    virtualization), so it is the recommended way to run DinD-style
    workloads when you cannot adopt rootless BuildKit/Kaniko and want to
    avoid the `privileged` profile.
- **Config notes:** set `runtimeClassName`, run the inner daemon as root
  in-container (`runAsNonRoot: false` in the `PodTemplate`), and **do
  not** set `privileged: true`.

## Approach 4 — Plain privileged DinD (avoid where possible)

The classic recipe — a `docker:dind` sidecar or a runner container with
`securityContext.privileged: true` — runs an inner Docker daemon with full
host-kernel access.

- **Required profile:** `privileged`, and nothing less. Under `baseline`
  and `restricted` the GMC validating webhook rejects any
  `privileged: true` worker container outright, and PSA would reject the
  pod even if the webhook were bypassed (PSA is the backstop on both
  paths). The `privileged` profile is the *only* one that admits it. See
  [Security § Privileged worker containers](../design/05-security.md#privileged-worker-containers-are-admitted-only-under-the-privileged-profile).
- **`privileged` is gated twice.** Selecting it is necessary but not
  sufficient: the namespace must also be labelled
  `actions-gateway.github.com/privileged-profile: allowed` by a platform
  administrator (a tenant cannot self-grant it), and an update that lowers
  rank into `privileged` needs the
  `actions-gateway.github.com/allow-profile-downgrade` annotation. See
  [Privileged eligibility is a platform decision](../design/05-security.md#privileged-eligibility-is-a-platform-decision)
  and the
  [tenant-onboarding privileged checklist](tenant-onboarding.md).
- **The security trade-off.** A privileged container is effectively root on
  the node's kernel: a container escape escapes to the *host*, breaking the
  cross-tenant isolation the per-tenant model promises. Container-escape
  risk is High by design — admission imposes no restrictions.
- **Steer away from it.** BuildKit-rootless or Kaniko cover the vast
  majority of `docker build` needs at `baseline` with no privileged
  container; Sysbox covers the cases that genuinely need an inner daemon.
  Reach for plain privileged DinD only when none of those fit.
- **If you must, add a sandbox runtime.** Pair `privileged` with
  `runtimeClassName: kata-containers` (or `gvisor`) so the privileged
  workload escapes only into a microVM/sandbox kernel, not the host. This
  pattern is documented in
  [Security § Pairing privileged with a sandbox runtime](../design/05-security.md#pairing-privileged-with-a-sandbox-runtime)
  and the runtime trade-offs in
  [Appendix B](../design/appendix-b-worker-isolation.md).

## Decision table

Pick the topmost row that matches your build need.

| Build need | Recommended approach | `securityProfile` (PSA `enforce`) | Privileged container? | Notes |
|---|---|---|---|---|
| Standard `docker build` / `buildx`, no host mounts | **BuildKit (rootless)** | `baseline` | No | Tightest common path; try `restricted` only after validating on your kernel. |
| `Dockerfile` build, no daemon wanted, registry-side cache | **Kaniko** | `baseline` (set `runAsNonRoot: false`) | No | Not a sandbox — PSA is the boundary; plan a cache repo + auth. |
| Compliance mandate forces `restricted`, you control the `Dockerfile` | **Kaniko (non-root)** *or* **BuildKit-rootless (validated)** | `restricted` | No | Kernel/ownership-sensitive; verify end to end first. |
| "Real" inner Docker daemon / `systemd` (compose, nested containers) without `privileged` | **Sysbox** (`runtimeClassName`) | `baseline` | No | Platform admin installs the Sysbox runtime + `RuntimeClass`. |
| Workload genuinely requires a privileged Docker daemon and Sysbox is unavailable | **Privileged DinD** (last resort) | `privileged` | Yes | Needs the platform `privileged-profile: allowed` namespace label; pair with `kata`/`gvisor`. |
| Kernel modules / host capabilities beyond DinD | **Privileged** + sandbox runtime | `privileged` | Yes | Same gating; sandbox runtime strongly recommended. |

## Registry authentication (all approaches)

Every approach needs registry credentials to pull base images and push the
result. Mount them as a file, never an environment variable:

- Mount a Docker `config.json` (e.g. from a Kubernetes Secret of type
  `kubernetes.io/dockerconfigjson`) into the build container and point the
  tool at it (`DOCKER_CONFIG` directory, BuildKit secret, or Kaniko's
  default `~/.docker/config.json`). Passing a token through an environment
  variable leaks it into process listings, logs, and child processes — see
  the secrets-handling guidance in
  [Security operations](security-operations.md).
- Prefer short-lived, scoped registry tokens over long-lived credentials,
  and scope each tenant's pull/push permissions to the registry paths that
  tenant owns.

For pulling the *worker image itself* from a private registry (e.g.
Harbor) with digest pinning, see
[Tenant onboarding](tenant-onboarding.md) and
[Security § Supply-chain](../design/05-security.md).

## Mixing build and non-build workloads

PSA enforcement is namespace-scoped: every pod in a tenant namespace is
evaluated against the same profile. A tenant that needs both a hardened
default and a privileged (or Sysbox) build lane deploys **two
`ActionsGateway` CRs in two namespaces** — for example a `baseline`
gateway for tests and a separate build gateway — and routes jobs with
`runs-on:` labels. The reasoning is in
[Security § Mixing privileged and non-privileged workloads](../design/05-security.md#mixing-privileged-and-non-privileged-workloads).

## Related

- [Security § 5.3 — Security profiles and the privileged opt-in](../design/05-security.md#53-security-profiles-and-the-privileged-opt-in) — the authoritative profile model.
- [Appendix B — Worker isolation](../design/appendix-b-worker-isolation.md) — `runc` vs gVisor vs Kata sandbox runtimes.
- [Tenant onboarding](tenant-onboarding.md) — granting privileged eligibility to a namespace.
- [Admission policies](admission-policies.md) — Kyverno/Gatekeeper compatibility for GAG worker pods.
- [Troubleshooting](troubleshooting.md) — privileged-profile and privileged-container rejection symptoms.
