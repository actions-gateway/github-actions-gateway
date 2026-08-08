# Runner template library

GAG ships three ready-to-apply worker pod shapes under
[`deploy/templates/`](https://github.com/actions-gateway/github-actions-gateway/tree/main/deploy/templates),
so you can start from a validated template instead of hand-authoring one. A
`RunnerSet` picks one by name, and moving to your own template later is the same
field pointing somewhere else.

Apply one, point a `RunnerSet` at it, done:

```bash
kubectl apply -k deploy/templates/plain
```

```yaml
apiVersion: actions-gateway.com/v2beta1
kind: RunnerSet
metadata:
  name: linux
  namespace: team-a
spec:
  gatewayRef: { name: team-a-gateway }
  templateRef:
    name: plain
    kind: ClusterRunnerTemplate
  runnerLabels: ["linux"]
```

## Choosing an entry

Pick by what your jobs do. Workload names are rows in this table, not template
names: "kind end-to-end tests" is the Docker-in-Docker shape, delivered as
`kata-dind` or `privileged-dind` depending on what your nodes can do.

| Your jobs | Entry | What you get |
|---|---|---|
| Unit tests, lint, deploys, anything that does not build container images | `plain` | No Docker daemon, no elevated capabilities. The only entry that composes with GAG's security gap-fill rather than opting out of it. |
| Build container images, run `docker compose`, run a nested cluster | `kata-dind` | A real Docker daemon inside a KVM micro-VM. An escape reaches a throwaway guest kernel, not your node. |
| The same, where nested virtualisation is unavailable | `privileged-dind` | The same daemon with no isolation. Trusted jobs only. |
| Build images with no daemon at all | none of these | Rootless BuildKit or Kaniko run fine under `plain`. See [In-runner image builds](in-runner-image-builds.md). |

`privileged-dind` is the fallback, not the starting point. A privileged dockerd
exposes the host kernel to every job that lands on that template, so a container
escape from any one job reaches the node. Try `kata-dind` first, and a rootless
builder before either.

## Prerequisites you supply

GAG never installs runtime handlers, `RuntimeClass` objects, or node pools. That
is deliberate and unchanged by the library
([Running DinD workloads under Kata](kata-dind-workloads.md) covers the split in
full). Each entry needs a different amount of groundwork.

### All three

The v2 CRDs must be installed (the opt-in `actions-gateway-crds-v2` chart), and
the namespace must be a marked tenant namespace per
[Tenant onboarding](tenant-onboarding.md).

### `kata-dind` and `privileged-dind`

**A build-capable runner image.** Both ship `spec.workerImage` set to
`example.invalid/build-capable-runner:replace-me` and you must replace it. The
runner container needs a Docker CLI on `PATH`; the sidecar supplies the daemon,
not the client, and the stock `ghcr.io/actions/actions-runner` image ships
neither. The reserved `.invalid` host is chosen so an unreplaced value fails at
image pull, immediately and legibly, rather than succeeding into a job that dies
on `docker: not found` twenty minutes in. `scripts/dogfood/e2e-runner/Dockerfile`
in this repo is a worked example.

**A namespace at PSA `privileged`, plus GAG's three gates.** Both entries need
capabilities that Pod Security Standards `baseline` forbids, and PSA has no
Kata-aware level in between. Tenants cannot self-elevate: the namespace needs
all four settings in one object.

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: team-a
  labels:
    actions-gateway.com/tenant: managed
    actions-gateway.com/security-profile: privileged
    pod-security.kubernetes.io/enforce: privileged
    actions-gateway.com/privileged-profile: allowed      # platform-set eligibility
  annotations:
    actions-gateway.com/allow-profile-downgrade: allowed # platform-set consent
```

Understand what this costs before you set it. Under the `privileged` profile
GAG's `applySecurityDefaults` gap-fill becomes a no-op by design, so the template
carries the entire security posture of its workers. That is the trade the library
is meant to improve on: you get a vetted capability set instead of a hand-rolled
one, not an exemption from thinking about it.

### `kata-dind` only

- Nodes with nested virtualisation, and a Kata runtime installed on them
  (kata-deploy or your distribution's equivalent).
- A `RuntimeClass` named **`kata`**. kata-deploy registers handler-named classes
  (`kata-qemu`, `kata-clh`, and so on); either register `kata` as an alias or
  patch `runtimeClassName` to the handler you have.
- A default `StorageClass` that supports `volumeMode: Block`. The template leaves
  `storageClassName` unset so your cluster default applies; most CSI block
  drivers qualify, most file and NFS drivers do not. Set it explicitly if yours
  does not.
- A metadata control: Workload Identity, IMDSv2 with hop limit 1, or a
  `NetworkPolicy` denying `169.254.169.254/32`. Kata bounds the kernel, not the
  pod network. GAG measured GKE's service-account token endpoint answering
  `HTTP 200` from inside a Kata guest, so the metadata server stays reachable
  even though the container escape path does not.

## Nothing ships as a cluster default

Every entry is opt-in and applied on its own. There is no aggregate
kustomization that applies all three, and no entry carries the
`actions-gateway.com/is-default-template` annotation.

That is a deliberate line rather than an oversight. A `ClusterRunnerTemplate`
marked as the cluster default applies to every `RunnerSet` that names no
template, so shipping one as a default would silently hand a privileged pod
shape to sets that never asked for it. Marking a default is your decision; the
resolution order is `templateRef`, then the gateway default, then the
cluster-default annotation.

## Forking an entry

Two supported paths.

**Copy it.** Take `deploy/templates/<entry>/template.yaml`, rename the object,
edit, and apply. Simple, and you own the result including any future fix that
lands upstream.

**Patch it with kustomize.** Keeps you on the shipped base, so an upstream fix
reaches you on the next pull.

Use **JSON 6902**, not a strategic merge, for anything that reaches into a list.
kustomize has no OpenAPI schema for a custom resource, so a strategic-merge
patch against one degrades to an RFC 7386 JSON merge patch, and that replaces a
list wholesale instead of merging it by key. A patch naming only
`initContainers[0].resources` silently drops that container's `image`,
`restartPolicy`, capability set and startup probe, and renders at exit 0.
Measured on kustomize v5.8.1, as embedded in kubectl 1.36. Scalars and maps are
safe either way; lists are not.

```yaml
# my-overlay/kustomization.yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ../deploy/templates/kata-dind
patches:
  - target:
      group: actions-gateway.com
      version: v2beta1
      kind: ClusterRunnerTemplate
      name: kata-dind
    patch: |
      - op: replace
        path: /spec/workerImage
        value: ghcr.io/myorg/build-runner:1.2.3
      - op: add
        path: /spec/podTemplate/spec/nodeSelector
        value:
          node-role: builders
```

GAG's own end-to-end suite is the worked example: the overlays under
`deploy/dogfood-e2e/overlays/` consume these bases and patch in their cluster
specifics and nothing else.

## Sizing

The resource requests and limits in `kata-dind` and `privileged-dind` are
measured, not guessed, but they are measured on **GAG's own end-to-end suite**:
a compile-heavy runner driving a nested Kubernetes cluster inside the sidecar.
That is a plausible starting point for an image-building runner and a poor one
for a job with a different shape.

Two things about the Kata entry that do not transfer from ordinary pod sizing:
CPU limits become the guest's vCPU count, and memory limits become the guest's
entire RAM including page cache. There is none of the burst-to-node headroom the
privileged variant's requests-only CPU gets. Add the `RuntimeClass` overhead
(250m CPU, 160Mi) when you size nodes.

Re-measure for your workload: [Worker rightsizing](worker-rightsizing.md).

## What may be added to the library

Only what CI exercises. A shipped golden template is an implicit claim that it
works, so membership is gated on evidence rather than on plausibility:

- every entry is applied to a real API server on each integration run, which
  proves it satisfies the CRD schema and the v2 reserved-field validation rules;
- `kata-dind` and `privileged-dind` are additionally the bases GAG's own dogfood
  tenant runs real end-to-end jobs on.

`make template-library-check` reconciles the shipped set against the exercised
set on every run of the local gate and in CI, in both directions. gVisor, sysbox
and rootless BuildKit are excluded for exactly this reason: they have no
template-level coverage here, so shipping them would claim a validation that
does not exist.

## Related

- [Running DinD workloads under Kata](kata-dind-workloads.md): the full Kata
  reference, including what GAG does and does not manage
- [In-runner image builds](in-runner-image-builds.md): the five build
  approaches and their security profiles
- [Tenant onboarding](tenant-onboarding.md): namespace marking, quotas, and the
  v2 object set
- [Worker rightsizing](worker-rightsizing.md): measuring your own numbers
