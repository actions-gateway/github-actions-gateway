# Agent reference: kind cluster iteration

Reference for iterating against a real kind cluster — when unit tests and envtest can't observe the behavior you need (real CNI, kube-proxy DNAT, kubelet image pulls, TLS-over-tunnel, etc.). The full kind e2e test design lives in [`docs/design/07-test-plan.md`](../design/07-test-plan.md) §7.3; this doc covers the operational reality of iterating fast against the cluster the suite stands up.

## Standing up the cluster

```bash
make e2e-cluster          # 3-node kind cluster + local OCI registry (idempotent)
make apply-cert-manager   # cert-manager (the GMC webhook depends on it)
make wait-cert-manager
make e2e-images           # builds and pushes gmc/agc/proxy/worker/wrapper/fakegithub
```

The Makefile pipeline pushes to `127.0.0.1:5000` and the kind nodes pull from there on demand. The `scripts/e2e/kind-with-registry.sh` script wires the kind nodes' containerd to resolve `127.0.0.1:5000/*` against the host registry. (The literal IPv4 loopback is used, not `localhost`: the registry is published IPv4-only, so a pusher that resolves `localhost` to IPv6 `[::1]` first fails intermittently with "connection refused".)

### CNI selection: kindnet (default) vs Calico

`make e2e-cluster` builds a kindnet cluster by default. kindnet's bundled `kube-network-policies` enforcer does **not** drop egress traffic for the NetworkPolicy negative cases (two CI iterations observed successful HTTP exchanges the workload NP does not authorise — see [`docs/plan/worker-egress-proxy.md`](../plan/worker-egress-proxy.md)). To observe egress NetworkPolicy *enforcement* at runtime, build the Calico profile instead:

```bash
make e2e-cluster KIND_CNI=calico   # disableDefaultCNI + pinned Calico manifest
```

The script creates the cluster with `disableDefaultCNI: true` and `podSubnet: 192.168.0.0/16` (Calico's default pool), applies the Calico manifest pinned by `CALICO_VERSION` in the root Makefile, pins `IP_AUTODETECTION_METHOD=kubernetes-internal-ip` on `calico-node` (kind nodes have several interfaces; Calico's default `first-found` autodetection can bind BIRD to the wrong one and stall the rollout), and waits for `calico-node` rollout + node readiness — dumping `calico-node` state and logs if that gate times out. A CNI cannot be swapped in place — if the cluster already exists with kindnet, the script errors and tells you to `make e2e-cluster-delete` first.

The runtime egress-negative e2e specs (`E2E_GMC_TenantProvisioning_WorkloadEgressBlockedToNonProxyPod`, `E2E_GMC_TenantProvisioning_WorkerCannotReachK8sAPI`) detect the CNI at runtime and skip themselves on kindnet, so the standard CI flow is unaffected; they only assert enforcement on a Calico/Cilium cluster.

On the kindnet lane the script also **removes the CPU limit kind ships on the kindnetd DaemonSet** (100m). kindnetd is in-band for far more than packet forwarding: its embedded `kube-network-policies` enforcer verdicts the first packet(s) of each new policied connection in userspace (nfqueue), and its NRI plugin sits in the pod-sandbox-creation path. At 100m it is CFS-throttled even at idle, and under e2e load a starved enforcer times out *allowed* traffic (webhook calls, broker long-polls) while leaving *fresh* NetworkPolicies unenforced for minutes — the Q300 cross-spec flake signature. The CPU request and memory limit are unchanged.

## Inner-loop gotchas

### Target the cluster explicitly — don't trust the active context

`~/.kube/config` is a single shared file. When multiple sessions run on one machine, any session's `kind create`, `gcloud … get-credentials`, or `kubectl config use-context` rewrites `current-context` for *everyone* — so a bare `kubectl get pods` you fire mid-task can silently hit the wrong cluster, and a write can land somewhere it shouldn't.

Pin the target on every ad-hoc command instead of relying on the active context:

```bash
kubectl --context kind-<cluster> get pods -A            # kind e2e (see KIND_CLUSTER)
kubectl --context gke_<project>_<zone>_<cluster> get ns # GKE dogfood
```

The repo scripts already do this — `scripts/e2e/kind-with-registry.sh` threads `kubectl --context "kind-${KIND_CLUSTER}"` through every call, and `scripts/lib/common.sh` (`gke_get_credentials_and_verify`) fails closed if `current-context` isn't the expected GKE context before any write. Match that pattern in ad-hoc commands.

The same ambient-state hazard applies to **`gcloud`**: the active project/account/region live in the shared `~/.config/gcloud` active configuration, so a parallel `gcloud config set` repoints your invocations too. Pass `--project`, `--account`, and `--zone`/`--region` explicitly on each command rather than depending on `gcloud config` (or scope a private config with `CLOUDSDK_ACTIVE_CONFIG_NAME` / `gcloud --configuration=<name>`).

#### The e2e suite has no `--context` to pin — give it a private `KUBECONFIG`

`make e2e` threads `KIND_CLUSTER` through for the scripts that read it, but the suite's own `kubectl` calls (`utils.Run(exec.Command("kubectl", …))`) carry no `--context`, so they follow whatever `current-context` happens to be. There is nothing to pin per-command, and `kubectl config use-context` is exactly the shared-state write the rule above forbids — it repoints every parallel session.

Point the run at its own kubeconfig instead:

```bash
kind get kubeconfig --name actions-gateway-e2e > tmp/kubeconfig
```

then set `KUBECONFIG=$PWD/tmp/kubeconfig` on the `ginkgo`/`make e2e` invocation. The suite and everything it shells out to inherit it, no shared file is touched, and a parallel session's `use-context` cannot steal the run mid-flight. `tmp/` at the repo root is gitignored and inside the workspace, which is where scratch files belong.

Without it the first `kubectl` fails against the empty default — `dial tcp [::1]:8080: connect: connection refused`, usually surfacing as a cert-manager install failure in `SynchronizedBeforeSuite` rather than as anything about contexts.

### Verify the resolved target before any mutating command

An explicit `--context` still points *somewhere*. Before a destructive verb (`apply`/`delete`/`scale`/`patch`/`rollout` on kubectl, `upgrade`/`uninstall` on helm, `delete`/`destroy` on gcloud/terraform), confirm the effective target isn't a live/prod environment — echo it and stop if it looks like prod (the shared GKE dogfood cluster counts):

```bash
kubectl config current-context
gcloud config get-value project
```

Never run a destructive verb against a prod-looking target without explicit user confirmation.

The dogfood cluster (`gag-dogfood` / project `actions-gateway-dogfood`) is hard-classified prod via the checked-in [`.claude/prod-guard.json`](../../.claude/prod-guard.json), so the prod-guard hook denies ad-hoc destructive commands against it — prefix an intentional one with `PROD_GUARD_OVERRIDE=<reason>`. Lifecycle scripts (`scripts/dogfood/*`) run as `bash …`, which the hook doesn't parse, so they're unaffected. The echo-and-confirm convention still applies to every other prod-looking target, and for contributors who don't have the hook installed.

If a legitimate non-prod target keeps prompting because prod-guard can't classify it, add it to `.claude/prod-guard.json` under `nonprod` rather than approving repeatedly — and never `use-context`/`config set` to dodge the prompt, since that repoints every parallel session (see above).

### A state change you observe is not necessarily one you caused

The same sharing that makes ambient context dangerous to *write* makes before/after readings dangerous to *interpret*. A parallel session can delete pods, bounce a controller, resize a pool, or run a lifecycle script between your two reads — so "busy at T1, empty at T2" establishes only that it changed, never that your action, or the passage of time, is what changed it.

This matters most when the inference becomes a claim: a commit message, a plan doc, or a Queue row asserting a mechanism. Q577's diagnostics shipped alongside exactly that error — worker pods on the dogfood cluster went from eight to zero across two reads, which was reported as a drain converging unaided when a parallel session had in fact cleared it by hand. The design survived because its reasoning was independent, not because the premise held.

Before attributing an observed change:

- **Check for other actors.** `gh pr list` for in-flight sessions on the same subject, and ask the user — they are usually the other actor, and they know.
- **Prefer a mechanism reading over a boundary reading.** Controller logs, events, and `.status` say *what happened*; two snapshots either side of a gap say only that something did.
- **State what you measured, separately from what you infer.** "Pods absent at 13:15Z" is durable; "the drain converged" is a hypothesis about why.

This is the shared-resource case of the general rule that a claim cites a measurement — here the measurement is real and the *causal step* is what goes unverified.

### Image tag caching

Kind nodes use `imagePullPolicy: IfNotPresent` and will keep serving the cached layer when you re-push the same tag. **Pushing to `127.0.0.1:5000/foo:e2e-abc123` a second time does not refresh what kubelet runs.**

Two options:
- Push to a unique tag per iteration (`-v2`, `-v3`, or a content hash) and update the deployment image:
  ```bash
  docker buildx bake --file docker-bake.hcl --set "agc.tags=127.0.0.1:5000/agc:e2e-d667096-v2" agc
  kubectl set env -n gmc-system deployment/gmc-controller-manager AGC_IMAGE=127.0.0.1:5000/agc:e2e-d667096-v2
  ```
- Or set `imagePullPolicy: Always` on the deployment template (only viable for components where you control the spec).

### GMC rejects floating `AGC_IMAGE`/`PROXY_IMAGE` tags by default

The GMC requires `AGC_IMAGE` and `PROXY_IMAGE` to be pinned by `@sha256:` digest, so a floating tag like `127.0.0.1:5000/agc:e2e-d667096-v2` makes it exit at startup with a "not digest-pinned" error. For local iteration, start the GMC with `--allow-floating-image-tags=true` (the e2e suite patches this flag in automatically):

```bash
kubectl patch deployment -n gmc-system gmc-controller-manager --type=json \
  -p='[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--allow-floating-image-tags=true"}]'
```

### `kubectl rollout restart` is sometimes a no-op

If the deployment spec hash hasn't changed, no new pod gets created. On a GMC older than the Q552 fix, a GMC-managed Deployment (the tenant AGC, the egress proxy pool) never changes hash on a restart at all: the reconcile reverted the `kubectl.kubernetes.io/restartedAt` annotation that carries it, so kubectl reported the old ReplicaSet as rolled out.

Either way — after bumping a referenced Secret/ConfigMap, to force a fresh pull, or on a pre-fix GMC — the workaround is the same:

```bash
kubectl delete pod -n <ns> -l <selector>
```

The Deployment controller will recreate it with the latest spec and pull policy.

### Distroless pods can't be `kubectl exec`'d

The AGC, GMC, and proxy images are distroless — no shell, no `nc`, no `curl`. For connectivity checks from a pod that *should* be allowed by NetworkPolicy, spawn a temporary debugger with the same labels as the real pod:

```bash
kubectl run dbg --image=alpine --restart=Never --rm -i \
  --labels='actions-gateway/component=workload,app=actions-gateway-controller' \
  --command -- sh -c '
    apk add --no-cache curl bind-tools >/dev/null 2>&1
    nc -zv -w 5 actions-gateway-proxy 8080
    curl -sv --max-time 10 --proxy-insecure --proxy https://actions-gateway-proxy:8080 https://api.github.com/zen
  '
```

NetworkPolicy enforces on labels, so the test only validates the path if the labels match the real pod's. Ephemeral containers (`kubectl debug --image=...`) don't work on the kind versions this project pins (`failed to call webhook` / `no kind "EphemeralContainers" is registered`).

### NetworkPolicy enforces after kube-proxy DNAT

This is the trap that caused half the bugs in PR #59. When a pod connects to a Service ClusterIP, kube-proxy rewrites the destination to a Pod IP **before** the NetworkPolicy layer sees the packet. An egress rule like:

```yaml
- ports: [{port: 8080}]
  to:
  - ipBlock: {cidr: 10.96.123.103/32}   # the Service ClusterIP
```

never matches real packets. The fix is to target the destination by pod selector:

```yaml
- ports: [{port: 8080}]
  to:
  - podSelector:
      matchLabels: {app: actions-gateway-proxy}
```

Same problem in reverse: a NetworkPolicy ingress rule targeting a Service ClusterIP doesn't work either.

#### The port-axis variant: kube-apiserver access in kind

The same DNAT-before-NP-enforcement pattern bites the **port** of NP rules, not just the destination. When a pod connects to `kubernetes.default.svc` (ClusterIP `10.96.0.1:443`), kube-proxy DNATs to the apiserver's host endpoint — in kind, that's `<node-ip>:6443`. By the time `kube-network-policies` evaluates the packet, the destination port is **6443**, not 443. An NP rule like:

```yaml
- ports: [{port: 443}]   # apiserver in production: 443→443
```

silently drops every k8s API call in kind, even though it works in production where the Service backends listen on 443. The fix is to allow both ports explicitly:

```yaml
- ports:
  - {port: 443}    # production: kubernetes Service backends on 443
  - {port: 6443}   # kind: post-DNAT host port of the apiserver
```

`agcAPIServerEgressRule` in [`cmd/gmc/internal/controller/shared_networkpolicy.go`](../../cmd/gmc/internal/controller/shared_networkpolicy.go) does this. The full diagnosis (and why removing the workload label is **not** a fix) is in [`networkpolicy-port-matching.md`](networkpolicy-port-matching.md). The pattern generalises: any NP rule that allows egress through a Service should allow both the Service port and the backend pod (or host) port, unless you can guarantee they match.

## Pointing AGC at fakegithub vs real GitHub

The GMC has an `--allow-agc-extra-env=true` flag (set by the e2e suite) that forwards any `AGC_EXTRA_*` env vars from the GMC pod into the AGC Deployments it creates. The suite uses this to point AGC at fakegithub:

```bash
kubectl set env -n gmc-system deployment/gmc-controller-manager \
  AGC_EXTRA_GITHUB_API_BASE_URL=http://fakegithub.e2e-infra.svc.cluster.local:8080 \
  AGC_EXTRA_GITHUB_BROKER_URL=http://fakegithub.e2e-infra.svc.cluster.local:8080 \
  AGC_EXTRA_STUB_AUTH_URL=http://fakegithub.e2e-infra.svc.cluster.local:8080/token \
  AGC_EXTRA_STUB_BROKER_URL=http://fakegithub.e2e-infra.svc.cluster.local:8080
```

To swap to real GitHub, unset those (suffix `-`) and set `AGC_EXTRA_GITHUB_ORG_URL`:

```bash
kubectl set env -n gmc-system deployment/gmc-controller-manager \
  AGC_EXTRA_GITHUB_API_BASE_URL- \
  AGC_EXTRA_GITHUB_BROKER_URL- \
  AGC_EXTRA_STUB_AUTH_URL- \
  AGC_EXTRA_STUB_BROKER_URL- \
  AGC_EXTRA_GITHUB_ORG_URL=https://github.com/<org>/<repo>
```

Since the `gitHubURL` field became required, a GMC-provisioned AGC always carries `GITHUB_ORG_URL` (threaded from `spec.gitHubURL`). The AGC therefore selects its registrar **stub-first**: when both `STUB_AUTH_URL` and `STUB_BROKER_URL` are set it uses the stub registrar regardless of `GITHUB_ORG_URL`, so the fakegithub overrides above win; unsetting the stub pair (as the real-GitHub swap does) falls through to the GitHub registrar.

The GMC rolls itself after env changes; tenant AGC pods pick up the new env on their next reconcile (force with `kubectl annotate actionsgateway <name> -n <ns> poke=$(date +%s) --overwrite`).

## Tightening the inner loop

A full `make e2e-up` run is ~10 minutes per cycle. To iterate on a single component:

1. Stand up the cluster + cert-manager + GMC once with `E2E_SKIP_TEARDOWN=true ginkgo run --focus '<spec>' ...`. The suite leaves the GMC, fakegithub, and cert-manager in place after it exits.
2. Rebuild the changed component only: `docker buildx bake --file docker-bake.hcl --set "<target>.tags=127.0.0.1:5000/<name>:<unique-tag>" <target>`.
3. Update the deployment image: `kubectl set image` (or `kubectl set env` for `AGC_IMAGE`/`PROXY_IMAGE`/`WRAPPER_IMAGE` on the GMC).
4. Force a fresh pod: `kubectl delete pod -l <selector>`.
5. Test the path with a label-matched `kubectl run` debug pod (above).

This drops each iteration from ~10 minutes to under a minute.

### Re-running the suite over skipped-teardown state

Part of what a skipped teardown leaves behind is a GMC Deployment whose `.args` are owned by the `kubectl-patch` field manager: the suite appends `--allow-agc-extra-env=true` with `kubectl patch --type=json`, and `args` is an atomic list, so that patch claims all of it. Helm 4 applies server-side, so the next run's `helm upgrade` fails instead of overwriting:

```
Apply failed with 1 conflict: conflict with "kubectl-patch" using apps/v1:
.spec.template.spec.containers[name="manager"].args
```

`setupGMC` deletes the leftover Deployment before `make deploy` for exactly this reason (Q590), and Helm recreates it chart-owned. Nothing else the skipped teardown left is touched, so the fakegithub, cert-manager, and tenant-namespace state of a failed run stays inspectable right up until you start the next run.

An ad-hoc `helm upgrade` against a standing release hits the same conflict. `--force-conflicts` reclaims ownership (Helm 4 only: Helm 3 has no server-side apply, so there is nothing to reclaim), which is what [`scripts/e2e/chart-upgrade-check.sh`](../../scripts/e2e/chart-upgrade-check.sh) does in its normalize step, deliberately *not* on the upgrade its assertions rest on.

## Watching what's actually happening

Distroless pods log to stdout. Useful one-shots:

```bash
# GMC
kubectl logs -n gmc-system deployment/gmc-controller-manager --tail=50

# AGC in a tenant
kubectl logs -n <tenant-ns> deployment/actions-gateway-controller --tail=50

# Worker pods (selected by the managed-by label; the canonical worker labels are
# app.kubernetes.io/managed-by=actions-gateway-controller and
# actions-gateway/component=workload)
kubectl logs -n <tenant-ns> -l app.kubernetes.io/managed-by=actions-gateway-controller --tail=50

# Fakegithub control API (sessions, enqueued jobs, rerun calls)
kubectl port-forward -n e2e-infra svc/fakegithub 9090:9090 &
curl -s http://localhost:9090/control/sessions
# Optional ?owner=<prefix> filters to one RunnerGroup's sessions (ownerName is "<group>-<index>")
curl -s 'http://localhost:9090/control/sessions?owner=my-ag-'
# Single-use JIT runner simulation (Q114): a job acquisition consumes the
# delivering session's runner record; scope with owner= to avoid affecting
# other suites on the shared instance
curl -s -X POST 'http://localhost:9090/control/singleuse?enabled=true&owner=my-ag-'
# Eviction auto-retry calls the AGC has made (the rerun-failed-jobs POST is the
# only externally visible sign that eviction recovery fired). Filter by run so
# the count is one spec's own rather than the whole process's.
curl -s http://localhost:9090/control/reruns
curl -s 'http://localhost:9090/control/reruns?run=%2Fruns%2F12345%2F'
```

For the runner side, `gh run list --repo <org>/<repo>` and `gh run view <id> --json status,conclusion` give the GitHub-side view that `kubectl` can't.

## Cleanup

`make e2e-clean` deletes the cluster and the local registry, and also removes `.build/`. Other targets leave `.build/` in place, so it otherwise persists across sessions — remove it manually (`rm -rf .build`) if you suspect stale tool binaries and aren't running the full `make e2e-clean`.
