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
| The same, where Kata is not an option (see below) | `privileged-dind` | The same daemon with no isolation. Trusted jobs only. |
| Build images with no daemon at all | none of these | Rootless BuildKit or Kaniko run fine under `plain`. See [In-runner image builds](in-runner-image-builds.md). |

Prefer `kata-dind` when you can have it, and a rootless builder before either. A
privileged dockerd exposes the host kernel to every job that lands on that
template, so a container escape from any one job reaches the node.

**But "when you can have it" excludes more than it sounds like.** Kata needs a
KVM-capable host, which on a cloud provider means nested virtualisation or bare
metal, and that is not universally available:

- **GPU builds are the clearest case.** Getting a GPU into a Kata guest needs
  VFIO/IOMMU passthrough, which a cloud instance does not expose to a nested
  guest; NVIDIA's GPU Operator supports Kata in single-GPU-passthrough mode only,
  and does not support configuring only some GPUs on a node for it. If your jobs
  build on GPU instances, `privileged-dind` is the realistic path, not a
  concession.
- **CPU architecture and machine family.** Google Compute Engine excludes E2,
  memory-optimized, H4D, and every AMD- and Arm-powered VM from nested
  virtualisation. On AWS, nested virtualisation means a `.metal` instance.
- Where nested virtualisation does work, expect a measured performance cost:
  GCE documents 10% or more for CPU-bound work, potentially more for I/O-bound.

So treat this as a real choice rather than a fallback with a bad reputation. If
Kata is unavailable to you, the meaningful comparison is `privileged-dind`
against a rootless builder, and the question is whether your jobs genuinely need
a Docker daemon. Where you land on `privileged-dind`, confine it: a dedicated
node pool, trusted jobs only, and never a fork's pull request.

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

Read the next section before you write the patch. It is the single most
expensive thing to get wrong here, and it fails silently.

### A strategic-merge patch against a custom resource deletes list entries

kustomize decides how to merge a list from the OpenAPI schema of the type being
patched. It ships schemas for built-in Kubernetes types only, so for a
`ClusterRunnerTemplate` it has none, and a strategic-merge patch degrades to an
RFC 7386 JSON merge patch. Under RFC 7386 a list is **replaced wholesale**
rather than merged by key.

Concretely: this patch, which reads like it adjusts one field,

```yaml
patches:
  - patch: |
      apiVersion: actions-gateway.com/v2beta1
      kind: ClusterRunnerTemplate
      metadata:
        name: kata-dind
      spec:
        podTemplate:
          spec:
            initContainers:
              - name: dind
                resources:
                  requests:
                    cpu: "3"
```

produces a `dind` container with *only* `name` and `resources`. Its `image`,
`restartPolicy: Always`, the entire capability set, the startup probe and the
`volumeDevices` binding the block volume are gone. `kubectl kustomize` prints
this and exits **0**. The pod then fails at admission or, worse, starts without
the daemon the runner is configured to reach. Measured on kustomize v5.8.1, as
embedded in kubectl 1.36.

Scalars and maps are safe either way. Lists are not, and `containers`,
`initContainers`, `volumes` and `tolerations` are all lists.

There are two ways out.

**Option 1, JSON 6902. What this repo uses, and the one to reach for.** A
targeted op list needs no schema, so it cannot degrade:

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
      - op: replace
        path: /spec/podTemplate/spec/initContainers/0/resources/requests/cpu
        value: "3"
```

The cost is that a JSON pointer carries a list index, so a patch survives on the
base's ordering. Pin what you depend on with a comment, or prefer the whole-map
`op: replace` over a deep path when the base might reorder.

**Option 2, teach kustomize the schema.** Supply an OpenAPI document that
declares the merge key, and strategic merge starts behaving the way it does for
a `Deployment`:

```json
{
  "definitions": {
    "com.actions-gateway.v2beta1.ClusterRunnerTemplate": {
      "type": "object",
      "properties": {
        "spec": { "type": "object", "properties": {
          "podTemplate": { "type": "object", "properties": {
            "spec": { "type": "object", "properties": {
              "initContainers": {
                "type": "array",
                "items": { "type": "object" },
                "x-kubernetes-patch-merge-key": "name",
                "x-kubernetes-patch-strategy": "merge"
              }
            }}
          }}
        }}
      },
      "x-kubernetes-group-version-kind": [
        { "group": "actions-gateway.com", "kind": "ClusterRunnerTemplate", "version": "v2beta1" }
      ]
    }
  }
}
```

```yaml
openapi:
  path: crt-schema.json
```

Verified working under kubectl's embedded kustomize, with no standalone
`kustomize` binary: the patch above then changes only `requests.cpu` and leaves
the image, `restartPolicy`, capabilities, probe and `volumeDevices` intact, and
strategic merges against built-in types in the same kustomization keep working.

**The trap in option 2, which has the same signature as the bug.** A custom
schema *replaces* kustomize's built-in one rather than extending it, so a
`$ref` into a built-in definition does not resolve. Writing the obvious thing,

```json
"podTemplate": { "$ref": "#/definitions/io.k8s.api.core.v1.PodTemplateSpec" }
```

leaves the ref dangling, kustomize finds no merge key, and it falls straight
back to wholesale replacement, silently, at exit 0. That is indistinguishable
from having written no schema at all. Declare the merge keys inline on the paths
you patch, as above, and confirm by rendering and diffing rather than by the
exit code.

This repo stays on option 1 because option 2 means hand-maintaining a schema
against a CRD that changes, and a schema that drifts fails open.

GAG's own end-to-end suite is the worked example for option 1: the overlays
under `deploy/dogfood-e2e/overlays/` consume these bases and patch in their
cluster specifics and nothing else. `make template-library-check` rejects a
strategic-merge patch against a `ClusterRunnerTemplate` in this repo, and
compares each overlay's render against its base to catch a list that lost
entries by any route.

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
