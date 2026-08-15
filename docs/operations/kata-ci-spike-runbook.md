# Kata-on-GKE runbook

Reproduce the Q226 result: a GitHub Actions runner running `dockerd` + `kind` inside a Kata Containers micro-VM on GKE, with **no** `privileged: true` on the runner pod.

**This was run live and passed** (GKE `1.35.5-gke.1241004`, Ubuntu 24.04 / containerd 2.1.5, `c2-standard-4` nested-virt, Kata 3.32.0 / QEMU).
Results and the full constraint list are in [Kata Containers on GKE](../plan/archive/kata-on-gke.md); the cluster-side how-to for operators is [Running DinD workloads under Kata](kata-dind-workloads.md).

Steps below mutate cloud resources.
Use a **throwaway project or cluster** — never a production one — and tear it down when finished.

## Before you start

- `gcloud` authenticated, with permission to create clusters.
- `kubectl` and `helm` on `PATH` (`make doctor` checks both).
- Pin `--project` / `--zone` on every `gcloud` call, and `--context` on every `kubectl` call.
  Parallel sessions share `~/.kube/config` and `~/.config/gcloud`.

```bash
export PROJECT=<throwaway-gcp-project>
export CLUSTER=<throwaway-cluster>
export ZONE=us-central1-a
export CTX="gke_${PROJECT}_${ZONE}_${CLUSTER}"
```

---

## Step 1 — Create a nested-virt cluster

> Acceptance criterion **#1**.

Create the nested-virt pool as the cluster's *initial* pool.
A separate `node-pools create` that hits a capacity stockout holds a cluster-level lock and blocks every other mutation — including deleting the stuck pool — for tens of minutes.

```bash
gcloud container clusters create "$CLUSTER" \
  --project="$PROJECT" --zone="$ZONE" \
  --num-nodes=1 --machine-type=c2-standard-4 \
  --image-type=UBUNTU_CONTAINERD \
  --enable-nested-virtualization \
  --disk-size=100 --release-channel=regular \
  --node-labels=gag.dev/kata-ci=true \
  --workload-pool="${PROJECT}.svc.id.goog"
```

`--enable-nested-virtualization` requires `UBUNTU_CONTAINERD` (or `COS_CONTAINERD` ≥ `1.28.4-gke.1083000`) and GKE **Standard** — Autopilot cannot do it.
The machine family must be one GCP supports for nested virt; it names the list when it rejects a create (measured 2026-08-02):

> A2, A3, C2, C3, C4, C4D, C4N, G2, H3, H4D, N1, N2, N4, N4D, Z3 and M4

**`C2D` and `N2D` are not on it**, nor is `e2`.
If you hit `ZONE_RESOURCE_POOL_EXHAUSTED`, that is a capacity stockout, not a quota error — try another family or zone.
Check the **per-family** quota too: `C2_CPUS` defaults to 8 on a fresh project, which is one node of an 8-vCPU shape, while `N2_CPUS` defaults to 200.

Harden the metadata server — Kata does **not** do this for you:

```bash
gcloud container node-pools update default-pool \
  --project="$PROJECT" --cluster="$CLUSTER" --zone="$ZONE" \
  --workload-metadata=GKE_METADATA        # recreates the nodes
```

**Verify `/dev/kvm` — the flag asserts intent, the device is the proof:**

```bash
kubectl --context "$CTX" debug node/"$(kubectl --context "$CTX" get nodes \
  -o jsonpath='{.items[0].metadata.name}')" -it --image=busybox -- ls -l /host/dev/kvm
```

**Expected:** `crw-rw---- ... 10, 232 /host/dev/kvm`.
If absent, nested virtualization did not take effect and nothing below will work.

---

## Step 2 — Install Kata and the RuntimeClass

> Acceptance criterion **#2**.

Upstream no longer publishes `kata-deploy.yaml` / `kata-rbac.yaml` release assets (they 404).
Install from the OCI Helm chart:

```bash
helm --kube-context "$CTX" install kata-deploy \
  oci://quay.io/kata-containers/kata-deploy-charts/kata-deploy \
  --version 3.32.0 -n kube-system \
  -f deploy/kata-ci/kata-values.yaml --wait --timeout 15m

kubectl --context "$CTX" apply -f deploy/kata-ci/runtimeclass.yaml
```

Confirm the runtime landed and the node self-labelled.
GKE preinstalls unrelated `gvisor` / `confidential-linked-runner` classes, so check the label too:

```bash
kubectl --context "$CTX" -n kube-system rollout status ds/kata-deploy --timeout=10m
kubectl --context "$CTX" get nodes -l katacontainers.io/kata-runtime=true
kubectl --context "$CTX" get runtimeclass kata kata-qemu
```

**Expected:** the DaemonSet is Ready, the node carries `katacontainers.io/kata-runtime=true` (applied by `kata-deploy`, *not* by you), and both classes exist.

---

## Step 3 — Start the unprivileged runner

> Acceptance criterion **#2** (continued).

```bash
kubectl --context "$CTX" apply -f deploy/kata-ci/runner-pod.yaml
kubectl --context "$CTX" wait --for=condition=Ready pod/kata-runner --timeout=7m
```

Prove it is unprivileged, and that the boundary is a real VM:

```bash
# Expect 0. Any other number means the invariant is broken.
kubectl --context "$CTX" get pod kata-runner -o json | grep -c '"privileged": true'

# Node kernel vs guest kernel MUST differ — identical means a silent runc fallback.
kubectl --context "$CTX" get node -o jsonpath='{.items[0].status.nodeInfo.kernelVersion}'; echo
kubectl --context "$CTX" exec kata-runner -- uname -r

# dockerd must be on overlay2. `vfs` means the block volume did not mount.
kubectl --context "$CTX" exec kata-runner -- docker info --format '{{.Driver}}'
```

**Expected:** `0`; two different kernels (spike saw `6.8.0-1054-gke` vs `6.18.35`); `overlay2`.

If `dockerd` fails, add **one** capability at a time and re-test.
Never set `privileged: true` — that defeats the entire design.

---

## Step 4 — `kind create cluster` inside the micro-VM

> Acceptance criteria **#3**, **#4**, **#6**.
> This is the headline claim.

`kind` needs the guest's `/dev/kmsg` bound into its node container:

```bash
kubectl --context "$CTX" exec kata-runner -- sh -c 'cat > /root/kind-config.yaml <<YAML
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
- role: control-plane
  extraMounts:
  - hostPath: /dev/kmsg
    containerPath: /dev/kmsg
YAML'

kubectl --context "$CTX" exec kata-runner -- \
  kind create cluster --name kata-spike --config /root/kind-config.yaml \
  --image "$KIND_NODE_IMAGE" --wait 300s     # same digest as .github/workflows/e2e-reusable.yml

kubectl --context "$CTX" exec kata-runner -- kubectl --context kind-kata-spike get nodes
kubectl --context "$CTX" exec kata-runner -- sh -c \
  'docker pull -q busybox:latest && kind load docker-image --name kata-spike busybox:latest'
```

**Expected:** the kind control-plane node reaches `Ready`, and `kind load` succeeds.
Spike timings: 58 s cold / 43 s warm for `kind create cluster`; 2 s for `kind load` — well inside the ≤ ~6 min ceiling.

---

## Step 5 — Confirm the metadata server is closed

Kata isolates the kernel, not the pod network.
Verify Workload Identity is doing its job.
**Never print the token body.**

```bash
kubectl --context "$CTX" run meta-id --image=busybox:1.36 --restart=Never \
  --overrides='{"spec":{"runtimeClassName":"kata"}}' --command -- sh -c \
  'wget -qO- --header="Metadata-Flavor: Google" \
    http://169.254.169.254/computeMetadata/v1/instance/service-accounts/default/email'
kubectl --context "$CTX" logs meta-id
```

**Expected:** `<project>.svc.id.goog` — the workload-pool identity.
If it returns a `*-compute@developer.gserviceaccount.com` address, the pod can mint **node** credentials: `GKE_METADATA` is not in effect and the runner is not safe for untrusted code.

---

## Step 6 — Tear down

Delete the PVC **first**.
A `Delete` reclaim policy only fires when the *PVC* is deleted — tearing down the cluster orphans the underlying PD, which keeps billing:

```bash
kubectl --context "$CTX" delete -f deploy/kata-ci/runner-pod.yaml   # pod + PVC
gcloud container clusters delete "$CLUSTER" --project="$PROJECT" --zone="$ZONE" --quiet
```

Confirm you are deleting the throwaway cluster, then check for orphans — the 100Gi `pd-balanced` behind the Block PVC is the one that gets left behind:

```bash
gcloud compute disks list     --project="$PROJECT"    # expect: Listed 0 items.
gcloud compute instances list --project="$PROJECT"
gcloud compute addresses list --project="$PROJECT"
```

Delete any survivor with `gcloud compute disks delete <name> --zone="$ZONE"`.

---

## What is still outstanding

`make e2e` inside the runner (acceptance criterion #5) has **not** been run: it needs a GAG e2e runner image bundling `dockerd`, `kind` and the Go/test toolchain, which does not exist yet.
Everything beneath it — Docker, `kind`, image loading, pod scheduling in the inner cluster — is proven above.
See [CI integration](../plan/archive/kata-on-gke.md#ci-integration--the-follow-up).
