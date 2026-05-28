# Agent reference: kind cluster iteration

Reference for iterating against a real kind cluster — when unit tests and envtest can't observe the behavior you need (real CNI, kube-proxy DNAT, kubelet image pulls, TLS-over-tunnel, etc.). The full kind e2e test design lives in [`docs/plan/e2e-tests.md`](../plan/e2e-tests.md); this doc covers the operational reality of iterating fast against the cluster the suite stands up.

## Standing up the cluster

```bash
make e2e-cluster          # 3-node kind cluster + local OCI registry (idempotent)
make apply-cert-manager   # cert-manager (the GMC webhook depends on it)
make wait-cert-manager
make e2e-images           # builds and pushes gmc/agc/proxy/worker/fakegithub
```

The Makefile pipeline pushes to `localhost:5000` and the kind nodes pull from there on demand. The `scripts/kind-with-registry.sh` script wires the kind nodes' containerd to resolve `localhost:5000/*` against the host registry.

## Inner-loop gotchas

### Image tag caching

Kind nodes use `imagePullPolicy: IfNotPresent` and will keep serving the cached layer when you re-push the same tag. **Pushing to `localhost:5000/foo:e2e-abc123` a second time does not refresh what kubelet runs.**

Two options:
- Push to a unique tag per iteration (`-v2`, `-v3`, or a content hash) and update the deployment image:
  ```bash
  docker buildx bake --file docker-bake.hcl --set "agc.tags=localhost:5000/agc:e2e-d667096-v2" agc
  kubectl set env -n gmc-system deployment/gmc-controller-manager AGC_IMAGE=localhost:5000/agc:e2e-d667096-v2
  ```
- Or set `imagePullPolicy: Always` on the deployment template (only viable for components where you control the spec).

### `kubectl rollout restart` is sometimes a no-op

If the deployment spec hash hasn't changed, no new pod gets created. After bumping a referenced Secret/ConfigMap, or to force a fresh pull, run:

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

`buildAGCNetworkPolicy` in [`cmd/gmc/internal/controller/builder.go`](../../cmd/gmc/internal/controller/builder.go) does this. The full diagnosis (and why removing the workload label is **not** a fix) is in [`docs/plan/5b-root-cause.md`](../plan/5b-root-cause.md). The pattern generalises: any NP rule that allows egress through a Service should allow both the Service port and the backend pod (or host) port, unless you can guarantee they match.

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

The GMC rolls itself after env changes; tenant AGC pods pick up the new env on their next reconcile (force with `kubectl annotate actionsgateway <name> -n <ns> poke=$(date +%s) --overwrite`).

## Tightening the inner loop

A full `make e2e-up` run is ~10 minutes per cycle. To iterate on a single component:

1. Stand up the cluster + cert-manager + GMC once with `E2E_SKIP_TEARDOWN=true ginkgo run --focus '<spec>' ...`. The suite leaves the GMC, fakegithub, and cert-manager in place after it exits.
2. Rebuild the changed component only: `docker buildx bake --file docker-bake.hcl --set "<target>.tags=localhost:5000/<name>:<unique-tag>" <target>`.
3. Update the deployment image: `kubectl set image` (or `kubectl set env` for `AGC_IMAGE`/`PROXY_IMAGE`/`WORKER_IMAGE` on the GMC).
4. Force a fresh pod: `kubectl delete pod -l <selector>`.
5. Test the path with a label-matched `kubectl run` debug pod (above).

This drops each iteration from ~10 minutes to under a minute.

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
```

For the runner side, `gh run list --repo <org>/<repo>` and `gh run view <id> --json status,conclusion` give the GitHub-side view that `kubectl` can't.

## Cleanup

`make e2e-clean` deletes the cluster and the local registry. The `.build/` directory persists across sessions; remove it if you suspect stale tool binaries.
