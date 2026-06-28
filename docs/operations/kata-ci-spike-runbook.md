# Kata-on-GKE spike runbook

Step-by-step go/no-go for the Q226 spike: prove that a GitHub Actions runner can
run `dockerd` + `kind` + GAG's e2e suite inside a Kata Containers micro-VM on
GKE, with **no** `privileged: true` on the runner pod. Each step below maps to
one of the six [spike acceptance criteria](../plan/kata-on-gke.md#spike-acceptance-criteria).

The artifacts this runbook applies are authored and statically validated
(`make manifest-validate`, `make shellcheck`); see
[deploy/kata-ci/](../../deploy/kata-ci/). What remains is the **live** validation
below.

## Before you start

> **This is the live half. It needs a GKE cluster and authenticated GCP
> credentials.** Steps tagged **🔴 LIVE** mutate cloud resources or a cluster.
> Steps tagged **🟢 OFFLINE** can be run now without credentials. Do not run the
> 🔴 steps until [Q224](../STATUS.md)'s GKE cluster is available.

Prerequisites for the live run:

- An existing **GKE Standard** cluster (Autopilot blocks nested virtualization).
- `gcloud` authenticated to the project, with permission to create node pools.
- `kubectl` pointed at the cluster.
- The GAG e2e runner image (the one bundling `dockerd`, `kind`, and the test
  toolchain) pushed to a registry the cluster can pull. Set its reference in
  [`deploy/kata-ci/runner-pod.yaml`](../../deploy/kata-ci/runner-pod.yaml)
  (`image:` — replace `REPLACE_ME`, pin by digest).

Set the environment used by the provisioning script (no real values are
committed):

```bash
export PROJECT=<your-gcp-project>
export CLUSTER=<your-gke-standard-cluster>
export REGION=<your-region>        # or set LOCATION_FLAG=--zone for a zonal cluster
```

Resolve the `kindest/node` image version GAG's e2e suite pins, so step 3 uses the
same one:

```bash
grep -rEn "kindest/node" .github/ scripts/ test/ | head
```

---

## Step 0 — Preview the provisioning command 🟢 OFFLINE

Confirm the node-pool invocation is what you expect before spending cloud time.

```bash
DRY_RUN=1 PROJECT=demo CLUSTER=demo REGION=us-central1 scripts/kata-node-pool.sh
```

**Expected:** the fully-expanded `gcloud container node-pools create …` command
prints, including `--enable-nested-virtualization`, an n2/n2d/c2/c2d machine
type, and `--node-labels katacontainers.io/kata-runtime=true`. Nothing is
created.

---

## Step 1 — Provision the nested-virt node pool 🔴 LIVE

> Acceptance criterion **#1**: a GKE Standard node pool with nested
> virtualization can be provisioned with documented steps.

```bash
scripts/kata-node-pool.sh
```

**VERIFY** the nested-virt enablement surface against current GKE docs first
(the gcloud flag names evolve — see the note in the script header). Then confirm
`/dev/kvm` is present on a node:

```bash
NODE=$(kubectl get nodes -l katacontainers.io/kata-runtime=true \
  -o jsonpath='{.items[0].metadata.name}')
kubectl debug node/"$NODE" -it --image=busybox -- ls -l /host/dev/kvm
```

**Expected:** `/host/dev/kvm` exists (a character device). If it does not,
nested virtualization is not enabled — the spike cannot proceed; record the
gcloud surface that *does* enable it and update `scripts/kata-node-pool.sh`.

---

## Step 2 — Install Kata + start the unprivileged runner 🔴 LIVE

> Acceptance criterion **#2**: a runner pod with `runtimeClassName: kata` (no
> privileged context) starts and a `dockerd` daemon runs inside it.

Install the runtime, register the RuntimeClasses, then schedule the runner:

```bash
kubectl apply -f deploy/kata-ci/kata-deploy.yaml
kubectl -n kube-system rollout status ds/kata-deploy --timeout=10m
kubectl apply -f deploy/kata-ci/runtimeclass.yaml
kubectl get runtimeclass kata kata-qemu

kubectl apply -f deploy/kata-ci/runner-pod.yaml
kubectl wait --for=condition=Ready pod/kata-runner --timeout=5m
```

Confirm the pod is genuinely unprivileged and that `dockerd` came up inside the
guest VM:

```bash
# No privileged context anywhere on the pod (expect: empty output).
kubectl get pod kata-runner -o jsonpath='{.spec.containers[*].securityContext.privileged}{"\n"}'
# dockerd reachable inside the Kata guest.
kubectl exec kata-runner -- docker info
```

**Expected:** `privileged` prints `false` (never `true`); `docker info` reports a
healthy daemon. If `dockerd` fails to start, add ONE capability at a time to the
runner pod's `securityContext.capabilities.add` (start from the documented
minimal set) and re-test — **never** set `privileged: true`. Record the final
capability set for the reference architecture doc.

---

## Step 3 — Create a kind cluster inside the runner 🔴 LIVE

> Acceptance criterion **#3**: `kind create cluster` (same `kindest/node` image
> version as GAG's e2e suite) completes inside the runner pod.

```bash
kubectl exec kata-runner -- \
  kind create cluster --image kindest/node:vX.Y.Z   # use the version from "Before you start"
kubectl exec kata-runner -- kubectl --context kind-kind get nodes
```

**Expected:** the kind control-plane node reaches `Ready`. Capture wall-clock for
step 6.

---

## Step 4 — Load an image into the kind cluster 🔴 LIVE

> Acceptance criterion **#4**: `kind load docker-image` loads an image.

```bash
kubectl exec kata-runner -- sh -c 'docker pull busybox:latest && kind load docker-image busybox:latest'
kubectl exec kata-runner -- docker exec kind-control-plane crictl images | grep busybox
```

**Expected:** `busybox` appears in the kind node's image list.

---

## Step 5 — Run the GAG e2e suite inside the runner 🔴 LIVE

> Acceptance criterion **#5**: `make e2e` passes inside the runner pod on that
> cluster.

```bash
kubectl exec kata-runner -- sh -c 'cd /workspace/github-actions-gateway && make e2e'
```

(Adjust the path to wherever the runner image checks out the repo.)

**Expected:** the e2e suite passes — the same green result it produces under
privileged DinD today.

---

## Step 6 — Check the startup-overhead ceiling 🔴 LIVE

> Acceptance criterion **#6**: `kind create cluster` inside Kata is ≤ 3× the
> current baseline (~2 min → ceiling ~6 min).

Time the step-3 command (e.g. wrap it in `time`, or compare timestamps):

```bash
kubectl exec kata-runner -- sh -c \
  'time kind create cluster --image kindest/node:vX.Y.Z'
```

**Expected:** wall-clock ≤ ~6 min. Record the actual number.

---

## Verdict

The spike is a **go** only if all six criteria pass. Record the outcome (and the
final runner-pod capability set, measured timings, and the confirmed gcloud
nested-virt flags) in [docs/plan/kata-on-gke.md](../plan/kata-on-gke.md).

If any criterion fails with no known fix inside the timebox, the recommendation
reverts to privileged DinD on a dedicated locked-down node pool (Tier 3 of the
[reference architecture](../plan/kata-on-gke.md#reference-architecture-deliverable)),
with the security trade-off documented explicitly.
