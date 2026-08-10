# Runner template library

Three ready-to-apply `ClusterRunnerTemplate` entries, so a new operator starts from a validated worker pod shape instead of hand-authoring one.
A `RunnerSet` selects an entry by name through `templateRef`; forking to your own template later is the same field pointing somewhere else.

The operator-facing walkthrough (prerequisites, how to adopt an entry, how to fork one) is [docs/operations/runner-template-library.md](../../docs/operations/runner-template-library.md).
This page is the map.

```text
deploy/templates/
  plain/            # no Docker, no elevated capabilities        <- start here
  kata-dind/        # Docker in a micro-VM, no privileged container
  privileged-dind/  # Docker with host-kernel exposure           <- fallback only
```

## Which entry

Pick by what the job does, not by what the template is called.
"kind e2e" is not a template name: it is the DinD shape, delivered as `kata-dind` or `privileged-dind` depending on what the cluster can do.

| Your jobs | Entry | Why |
|---|---|---|
| Unit tests, lint, deploys, anything that does not build images | `plain` | No daemon, no capabilities. The only entry that composes with the AGC's security gap-fill instead of opting out of it. |
| Build container images, run `docker compose`, run a nested cluster (kind) | `kata-dind` | A real Docker daemon whose blast radius is a throwaway guest kernel. Needs nested virtualisation and a Kata runtime on the nodes. |
| The same, where Kata is not available | `privileged-dind` | Same daemon, no isolation. Trusted jobs only. |
| Build images without a daemon at all | none of these | Rootless BuildKit or Kaniko run under `plain`. See [in-runner-image-builds.md](../../docs/operations/in-runner-image-builds.md). |

Reach for `kata-dind` first, and for a rootless builder before either: a privileged dockerd exposes the host kernel to every job that lands on it, so an escape reaches the node.

`privileged-dind` is not merely a fallback for the unlucky, though.
Kata needs nested virtualisation or bare metal, which rules out GPU builds (a GPU cannot be passed through into a nested guest), every AMD- and Arm-powered GCE machine family, and any AWS instance that is not `.metal`.
The operator doc [sets out where Kata is and is not an option](../../docs/operations/runner-template-library.md#choosing-an-entry).

## Before you apply

Each entry is opt-in and applied on its own:

```bash
kubectl apply -k deploy/templates/plain
```

There is no aggregate kustomization that applies all three, and no entry carries the `actions-gateway.com/is-default-template` annotation.
Choosing a cluster default stays an operator decision; the resolution ladder is `templateRef`, then the gateway default, then the cluster-default annotation.

Two entries need edits before they will run a job:

- **`kata-dind` and `privileged-dind` ship `spec.workerImage` pointing at `example.invalid/build-capable-runner:replace-me`.** The runner container needs a Docker CLI on `PATH`, which the stock `ghcr.io/actions/actions-runner` image does not ship (the sidecar provides the daemon, not the client).
  The reserved `.invalid` host makes an unreplaced value fail at image pull rather than succeed into a job that dies on `docker: not found`.
  [`scripts/dogfood/e2e-runner/Dockerfile`](../../scripts/dogfood/e2e-runner/Dockerfile) is a worked example of a build-capable runner image.
- **Both DinD entries need cluster prerequisites GAG deliberately never installs**: node pools, Kata runtime handlers, `RuntimeClass` objects, PSA labels.
  Each template's header comment lists its own; the operator doc collects them.

## Patching an entry

Use **JSON 6902**, not a strategic merge, for anything that reaches into a list.

kustomize has no OpenAPI schema for a CRD, so a strategic-merge patch against one degrades to an RFC 7386 JSON merge patch, and that **replaces a list wholesale instead of merging it by key**.
A patch that names only `initContainers[0].resources` silently drops that container's `image`, `restartPolicy`, capability set and startup probe, then renders at exit 0.
Measured on kustomize v5.8.1 (kubectl 1.36).

Scalars and maps are safe either way.
Lists are not.

```yaml
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
        path: /spec/podTemplate/spec/volumes/0/ephemeral/volumeClaimTemplate/spec/storageClassName
        value: my-block-sc
```

[`deploy/dogfood-e2e/overlays/`](../dogfood-e2e/README.md) is the worked example: GAG's own end-to-end suite runs on these bases, patching in its cluster specifics and nothing else.

## What may ship here

**Only what CI exercises.** A shipped golden template is an implicit claim that it works, so membership is gated on evidence, in two layers:

1. Every entry is applied to a real apiserver on every integration run (`TestTemplateLibrary_Admitted` in [`cmd/agc/internal/controller/integration`](../../cmd/agc/internal/controller/integration)), which proves it satisfies the CRD schema and the v2 reserved-field CEL rules.
2. `kata-dind` and `privileged-dind` are additionally the bases GAG's own dogfood e2e tenant runs real jobs on, which is what makes their pod shapes a validated claim rather than a plausible one.

[`scripts/ci/check-template-library.sh`](../../scripts/ci/check-template-library.sh) reconciles the two sets on every `make check`: an entry added here without a dogfood overlay consuming it fails the gate unless it is declared inert in that script, with the reason.
An overlay that stops consuming the library fails it too.

Excluded for exactly this reason, and revisited when the evidence changes: gVisor, sysbox and rootless BuildKit have no template-level coverage in this repo, so shipping them would claim a validation that does not exist ([appendix-b-worker-isolation.md](../../docs/design/appendix-b-worker-isolation.md)).
