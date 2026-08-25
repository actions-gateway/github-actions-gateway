# Shared worker storage with a ReadWriteMany volume

> **Audience:** Tenant operator, Platform engineer

Workers are storage-less by default: the Actions Gateway Controller (AGC) mounts a job-payload Secret and the certificate authority (CA) projections into a worker pod and nothing else, and it deletes the pod when the job ends.
Jobs that must pass files to each other need a `ReadWriteMany` (RWX) volume, and that volume is entirely a tenant's `RunnerTemplate` concern: GAG neither provisions it nor gets in its way.

This page is the reference architecture for doing that: what to put in the `podTemplate`, the one field that decides whether the runner can write at all, and which storage classes the arrangement has actually been exercised against.

**Scope:** v2 `RunnerSet` workers via `RunnerTemplate.podTemplate`.
The same shape works on a v1 `RunnerGroup.spec.podTemplate`, which is [deprecated](v1alpha1-deprecation.md).

## The stance: GAG provisions no worker storage, and never will

Three properties follow from one worker pod per job, and they are design rather than a gap:

- **No worker survives its job.** A volume is something a *new* worker mounts, never state a worker keeps.
- **The AGC provisions no claim of its own.** It appends its volumes to whatever the template declares, so a claim you declare arrives at the pod untouched.
- **A pre-existing claim costs a tenant no quota per worker.** The namespace `ResourceQuota` charges a `PersistentVolumeClaim` when the claim object is created, so ten workers mounting one shared claim are charged for one.
  A generic *ephemeral* volume is charged per pod instead, and the AGC's admission gate counts it, so see [ResourceQuota sizing](resourcequota-sizing.md).

So the deliverable here is an integration, not a storage system.
Anything a Kubernetes `PersistentVolumeClaim` can express, a worker can mount.

## Set `fsGroup` to the runner UID, or the job fails on its first write

This is the one requirement that is not obvious, and skipping it produces a job that starts cleanly and dies mid-step.

A freshly provisioned volume's root directory belongs to `root`.
The AGC gap-fills `runAsUser: 1001` on every profile except `privileged`, so the runner is neither `root` nor in any group that owns the directory, and its first write gets `Permission denied`.
Pod-level `fsGroup` is what fixes it: the kubelet adds the group to the container's supplementary groups and applies it to the volume, after which the runner can write.

```yaml
apiVersion: actions-gateway.com/v2beta1
kind: RunnerTemplate
metadata:
  name: shared-workspace
spec:
  podTemplate:
    spec:
      securityContext:
        # Must equal the runner UID.
        fsGroup: 1001
        fsGroupChangePolicy: OnRootMismatch
      containers:
        - name: runner
          volumeMounts:
            - name: shared
              mountPath: /mnt/shared
      volumes:
        - name: shared
          persistentVolumeClaim:
            claimName: team-a-shared
```

Two notes on that block:

- **`fsGroupChangePolicy: OnRootMismatch` is worth setting on any volume with real content in it.** The default (`Always`) walks the whole tree on every pod start, which on a shared volume several jobs have been filling is paid by every worker.
- **`fsGroup` only works when the storage driver honours it.** Read the driver's declared policy before relying on it: `kubectl get csidriver <name> -o jsonpath='{.spec.fsGroupPolicy}'`.
  `File` means the kubelet applies the change; `None` means it does not, and the export or share must then grant the runner UID access itself.
- **Declaring a pod `securityContext` here does not weaken the profile.** The AGC gap-fills `runAsNonRoot`, `runAsUser` and `seccompProfile` field by field, so a block that sets only `fsGroup` still receives all three.
  That differs from the [runner template library](runner-template-library.md)'s `plain` entry, where the *absence* of a **container** `securityContext` is load-bearing.

The claim is an ordinary namespaced `PersistentVolumeClaim` the tenant creates once, ahead of any job:

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: team-a-shared
spec:
  accessModes: [ReadWriteMany]
  storageClassName: <YOUR_RWX_CLASS>
  resources:
    requests:
      storage: 50Gi
```

## What this has been exercised against

Measured 2026-08-24 by `make test-rwx-storage`, which runs the pod the AGC really builds on two nodes of a kind cluster and requires them to exchange files through one claim:

| Storage class | Backend | Result |
|---|---|---|
| `gag-rwx-nfs` | csi-driver-nfs v4.13.4 over an in-cluster NFS server, `fsGroupPolicy: File` | Two workers on two nodes read and wrote each other's files. Without `fsGroup` the write failed with `Permission denied`. |

Nothing else has been exercised, and that is the honest state.
Amazon EFS, Google Cloud Filestore, Azure Files, CephFS and Longhorn all advertise RWX and all should work, but "should" is what this page exists to avoid asserting.
Treat them as unvalidated until someone runs the harness against one.
`make test-rwx-storage` takes the class to test in `RWX_STORAGE_CLASS`, so validating your own is one run: [testing.md § The shared worker storage validation](../development/testing.md#the-shared-worker-storage-validation).

The failure mode a wrong class produces is not subtle.
A `ReadWriteOnce` class either refuses the claim outright or binds it to one node, at which point the second worker is unschedulable and its job never starts.

## Coming from ARC's `containerMode: kubernetes`

ARC runs a job's `container:` and `services:` steps as separate pods sharing a provisioned volume, and that volume is the RWX dependency.
The volume half of it is the arrangement above: one claim, mounted by the workers of one runner set.

The pod-per-step half is not ported.
GAG runs one worker pod per job, so `container:` and `services:` steps run inside that pod, which today means Docker-in-Docker, under Kata Containers at `securityProfile: baseline` or privileged.
[Migrate from ARC § Security profiles](migration-from-arc.md#security-profiles) maps the choice, and [Kata DinD workloads](kata-dind-workloads.md) covers the non-privileged route.

## Where this bites in production

- **A shared volume is shared inside one namespace, and only there.** A `PersistentVolumeClaim` is namespaced, so one tenant's workers cannot mount another tenant's claim.
  Do not try to widen that with a cluster-scoped `PersistentVolume` two namespaces bind: it is the exfiltration path the [threat model](../design/05-security.md) exists to close, and nothing in GAG polices it for you.
- **Concurrent jobs write concurrently.** RWX gives several workers one filesystem, not coordination.
  Jobs that write the same path need their own locking, or their own subdirectory per run.
- **Nothing garbage-collects it.** The volume outlives every worker by design, so a shared cache that only ever grows will fill and start failing jobs on `ENOSPC`.
  Give the claim a size an operator watches, and prune it from a scheduled workflow.
- **`securityProfile: restricted` is compatible.** Pod Security Standards permit `persistentVolumeClaim` at every level, and `fsGroup` is unrestricted, so the arrangement above needs no relaxation.
