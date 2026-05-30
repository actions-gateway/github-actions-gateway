# NetworkPolicy port matching under kube-proxy DNAT

This doc is the canonical writeup of the kube-proxy DNAT vs. NetworkPolicy-port-match trap that the AGC `buildAGCNetworkPolicy` apiserver egress rule has to work around. Cited from [`docs/design/network-architecture.md`](../design/network-architecture.md), [`docs/operations/troubleshooting.md`](../operations/troubleshooting.md), [`kind-iteration.md`](kind-iteration.md), and the code at [`cmd/gmc/internal/controller/builder.go`](../../cmd/gmc/internal/controller/builder.go) (where the rule lists both `443` and `6443`).

**Status:** the recommended fix below landed; the AGC NP now lists both ports. The reproduction and analysis are preserved here because (1) the next reader tempted to "tidy up" the duplicate ports needs to find this, and (2) the same post-DNAT pattern recurs for ingress rules and for any Service whose backend port differs from the Service port — the worked example is the fastest way to recognise it.

## TL;DR

When the AGC connects to `kubernetes.default.svc` (Service ClusterIP `10.96.0.1:443`), kube-proxy DNATs the destination to the apiserver's host endpoint `<node-ip>:6443` *before* kindnet's `kube-network-policies` controller evaluates the packet. A NetworkPolicy that allows only port `443` sees post-DNAT port `6443` and drops the packet.

This is the **port axis** analogue of the `ipBlock: <ClusterIP>/32` trap PR #59 fixed for the proxy: NetworkPolicy port and IP matches are both evaluated against the post-DNAT 5-tuple, not the pre-DNAT one.

- Failure reproduces with **only the AGC NP** applying (workload label
  stripped). NP additivity is innocent.
- Adding port `6443` to the AGC NP's apiserver egress rule fixes it — *with
  both labels still applied*.

## Reproduction

Fresh kind cluster (v0.31.0, `kindest/node:v1.35.0`,
`kindest/kindnetd:v20251212-v0.29.0-alpha-105-g20ccfc88`), one namespace
`repro`, two NetworkPolicies copying the exact shape of `buildAGCNetworkPolicy`
and `buildWorkloadNetworkPolicy` at HEAD (commit `15c8c10`, before PR #62).
One debug pod (`alpine:3.20`) labelled with **both** labels:

```
app=actions-gateway-controller,actions-gateway/component=workload
```

Manifests are not checked in — copy `buildAGCNetworkPolicy` and `buildWorkloadNetworkPolicy` from [`cmd/gmc/internal/controller/builder.go`](../../cmd/gmc/internal/controller/builder.go) into a single YAML, point them at a `repro` namespace, add a stand-in proxy pod so the workload NP's proxy podSelector has a real target.

### Observations

| Pod labels                                                  | NPs that apply                | `nc -zv 10.96.0.1 443` |
|-------------------------------------------------------------|-------------------------------|------------------------|
| `app=actions-gateway-controller, component=workload`        | AGC NP **+** workload NP      | Times out (drop)       |
| `app=actions-gateway-controller`                            | AGC NP only                   | Times out (drop)       |
| `app=actions-gateway-controller, component=workload` + AGC NP patched to add a `port: 6443` egress rule | AGC NP + workload NP | **Open** (success)     |
| `app=actions-gateway-controller, component=workload` (additivity sanity check: `nc -zv <proxy-pod-ip> 8080`) | AGC NP + workload NP | Open (success)         |

The second row is the key finding: with **only** the AGC NP applying — i.e.
the exact configuration PR #62 makes permanent — the 443 connection still
fails. Whatever PR #59 observers thought they were testing when they reported
"removing the workload NP restored access" was either confounded by a race or
measured a different code path.

The fourth row confirms that NP additivity is fully functional in this
kindnet/kube-network-policies version: the workload NP's port-8080 rule is
honoured for the AGC pod even though the AGC NP knows nothing about it.

### kube-network-policies verdict log (kindnet `-v=4`)

With both labels applied and the original (port 443 only) AGC NP:

```
controller.go:567] "Evaluating packet" srcPod="repro/dbg-both" dstPod="none" packet=<
	[1] 10.244.2.2:34913 172.18.0.3:6443 TCP
 >
networkpolicy.go:218] "Pod is not allowed to connect to port" pod="repro/dbg-both" port=6443
networkpolicy.go:218] "Pod is not allowed to connect to port" pod="repro/dbg-both" port=6443
controller.go:597] "Egress NetworkPolicies" id=1 npolicies=2 allowed=false
controller.go:493] "Finished syncing packet" id=1 duration="936.461µs" verdict="drop"
```

With *only* the AGC NP applying (workload label stripped) — same destination,
same verdict:

```
[12] 10.244.2.2:43185 172.18.0.3:6443 TCP
"Pod is not allowed to connect to port" pod="repro/dbg-both" port=6443
"Egress NetworkPolicies" id=12 npolicies=1 allowed=false
verdict="drop"
```

The crucial line is the packet tuple: `… 172.18.0.3:6443`. The destination the
policy evaluator sees has already been DNAT-rewritten from `10.96.0.1:443` to
the apiserver host endpoint.

### Why post-DNAT? The nftables hook priorities

`kindnet-network-policies` registers two filter chains:

```
chain postrouting { type filter hook postrouting priority srcnat - 5; ... ip saddr @podips-v4 queue flags bypass to 101 }
chain prerouting  { type filter hook prerouting  priority dstnat + 5; ... ip saddr @podips-v4 queue flags bypass to 101 }
```

Priorities `srcnat - 5` (= 95) and `dstnat + 5` (= -95) place both hooks
**after** the corresponding NAT chains. Packets enrolled in `@podips-v4` (any
pod that is the subject of at least one NetworkPolicy) get NFQUEUE'd to
userspace queue 101, where the kube-network-policies controller decides
accept/drop based on the packet's *current* (post-DNAT) 5-tuple.

This is the same class of failure as the `ipBlock: <ClusterIP>/32` rule
PR #59 fixed for the proxy and which is already documented in
[`kind-iteration.md`](kind-iteration.md) § "NetworkPolicy enforces after
kube-proxy DNAT". The proxy case fixed the **IP** mismatch by switching
to a PodSelector; the apiserver case has the analogous **port** mismatch
and the fix is symmetric.

### Why does this happen in kind but presumably not in prod?

In kind, the `kubernetes` Service points to the apiserver running on the
control-plane node at port `6443`:

```
$ kubectl get endpointslice -n default
NAME         ADDRESSTYPE   PORTS   ENDPOINTS    AGE
kubernetes   IPv4          6443    172.18.0.3   59m
```

So the Service does port translation: `ClusterIP:443 → node-ip:6443`. In most
managed Kubernetes (GKE, EKS, AKS), the kubernetes Service's backend port is
`443` (the apiserver front-end LB or HAProxy listens on `443` directly), so
post-DNAT dport is `443`, and an NP rule on `443` matches. **The AGC NP works
in production by accident** — production happens to be the configuration the
rule was written for; kind isn't.

Other major NetworkPolicy implementations have the same constraint when the
apiserver Endpoints expose a different port: every reference NP example for
"allow access to kube-apiserver" either uses `port: 6443` explicitly, omits
the port restriction, or selects by Endpoints address. e.g. the
[cluster-kube-apiserver-operator policy](https://github.com/openshift/cluster-kube-apiserver-operator/pull/2029)
uses `port: 6443`.

## The fix

The minimal, environment-agnostic change is to ensure the AGC's apiserver
egress rule matches both the in-Service port (`443`, for production where the
Service does no port translation) and the kind-style host port (`6443`, for
kind clusters and any cluster where the apiserver Endpoints listen on
`6443`). The shipped rule in [`cmd/gmc/internal/controller/builder.go`](../../cmd/gmc/internal/controller/builder.go) `buildAGCNetworkPolicy` does this:

```go
{
    Ports: []networkingv1.NetworkPolicyPort{
        {Port: ptr(intstr.FromInt32(443))},
        {Port: ptr(intstr.FromInt32(6443))},
    },
},
```

The alternative — omitting the port restriction on the apiserver rule entirely
— is simpler but trades off precision: anything reachable on any port via the
AGC pod identity would be allowed. Listing both ports keeps the rule precise
(only apiserver-style ports) while working in both topologies.

The fix is symmetric with how PR #59 handled the proxy `ipBlock` → PodSelector
case: align the rule with the **post-DNAT** shape of the packet, not the
pre-DNAT one. The code comment on the rule calls out the reason explicitly so
the next reader (in particular: the next person tempted to "tidy up" the
duplicate ports) doesn't strip `6443` thinking it's redundant.

A regression unit test in [`builder_test.go`](../../cmd/gmc/internal/controller/builder_test.go) asserts both ports appear in the apiserver egress rule, and the integration test in [`network_policy_test.go`](../../cmd/gmc/internal/controller/integration/network_policy_test.go) covers the same property end-to-end against envtest.

The Tier-A e2e spec `E2E_GMC_TenantProvisioning_ProxyConnectWorks` in [`cmd/gmc/test/e2e/provisioning_test.go`](../../cmd/gmc/test/e2e/provisioning_test.go) is the live-cluster guard: a single label-matched debug pod doing `nc -zv 10.96.0.1 443` would have caught both PR #59's proxy `ipBlock` bug and the AGC `port: 443` bug from one spec, locally, in seconds.

## Why this isn't an upstream bug

This is documented expected behaviour of every well-known NetworkPolicy
implementation, not a bug. Cilium, Calico, kube-router, and
kube-network-policies all evaluate post-DNAT for ingress/egress port rules
because the alternative (recovering the pre-DNAT 5-tuple via conntrack at
policy eval time) is expensive and ambiguous in the presence of policy
chaining. The upstream
[`kubernetes-sigs/kube-network-policies` README](https://github.com/kubernetes-sigs/kube-network-policies)
makes no claim of pre-DNAT evaluation; the
[kindnet NP docs](https://kindnet.es/docs/user/network-policies/) link to it
unchanged.

Reference NetworkPolicy examples for "allow access to kube-apiserver"
consistently use `port: 6443` explicitly, omit the port restriction, or
select by Endpoints address — e.g. the
[cluster-kube-apiserver-operator policy](https://github.com/openshift/cluster-kube-apiserver-operator/pull/2029)
uses `port: 6443`.

## Generalising the pattern

The same trap applies to any NetworkPolicy rule whose port-restriction is
written against a Service ClusterIP rather than the backend pod or host
port. If a future rule allows egress through a Service whose backend port
differs from the Service port, either:

- list **both** the Service port and the backend port, or
- omit the port restriction on that rule, or
- replace the rule's peer/port with a PodSelector that matches the backend
  pods directly (this is what PR #59 did for the proxy `ipBlock` case).

The same caveat applies on the ingress side: an ingress rule whose port
matches the Service port can drop traffic if the backend pod listens on a
different port.
