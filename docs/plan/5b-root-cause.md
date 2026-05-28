# Queue item 5b root-cause diagnosis: AGC 443 egress under kindnet

## TL;DR

**The original "NetworkPolicy additivity is broken" hypothesis is wrong.** The
AGC's 443 egress failure under kind is a kube-proxy-DNAT / NetworkPolicy-port
mismatch, identical in shape to the `ipBlock: <ClusterIP>/32` trap PR #59
fixed for the proxy — just on the port axis instead of the IP axis.

In short: when the AGC connects to `kubernetes.default.svc` (Service ClusterIP
`10.96.0.1:443`), kube-proxy DNATs the destination to the apiserver's host
endpoint `<node-ip>:6443` *before* kindnet's `kube-network-policies`
controller evaluates the packet. The AGC NetworkPolicy allows port `443`, so
the post-DNAT port `6443` does not match → drop.

- Failure reproduces with **only the AGC NP** applying (workload label
  stripped). NP additivity is innocent.
- Adding port `6443` to the AGC NP's apiserver egress rule fixes it — *with
  both labels still applied*.
- [PR #62](https://github.com/actions-gateway/github-actions-gateway/pull/62)'s
  workaround (drop the workload label from AGC, expand AGC NP with the 8080
  proxy rule) does **not** fix the underlying 443-egress problem in kind. It
  rearranges the NP shape but the AGC NP still allows only `443`, which still
  doesn't match post-DNAT `6443`.

## Reproduction

Fresh kind cluster (v0.31.0, `kindest/node:v1.35.0`,
`kindest/kindnetd:v20251212-v0.29.0-alpha-105-g20ccfc88`), one namespace
`repro`, two NetworkPolicies copying the exact shape of `buildAGCNetworkPolicy`
and `buildWorkloadNetworkPolicy` at HEAD (commit `15c8c10`, before PR #62).
One debug pod (`alpine:3.20`) labelled with **both** labels:

```
app=actions-gateway-controller,actions-gateway/component=workload
```

Manifests live in [`/tmp/repro-nps.yaml`](../../tmp/repro-nps.yaml) — namespace,
two NPs, plus a stand-in proxy pod so the workload NP's proxy podSelector has a
real target.

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
[`docs/development/kind-iteration.md`](../development/kind-iteration.md) §
"NetworkPolicy enforces after kube-proxy DNAT". The proxy case fixed the
**IP** mismatch by switching to a PodSelector; the apiserver case has the
analogous **port** mismatch and the fix is symmetric.

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

## Why PR #62 doesn't fix the underlying problem

PR #62 makes two changes:

1. Drop `actions-gateway/component: workload` from the AGC pod template.
2. Expand the AGC NP with the DNS + `port: 8080 → proxy podSelector` rules
   that AGC previously inherited from the workload NP.

The **port-443 rule is unchanged** — it's still `port: 443` with no host
peer. With only the AGC NP applying, the kube-network-policies controller
will *still* see a post-DNAT 5-tuple of `<pod-ip>:<eph> → <node-ip>:6443` and
drop it for "Pod is not allowed to connect to port 6443".

The only way PR #62 could appear to fix the live-dry-run symptom is if
something *else* changed at the same time — a race in pod-IP enrollment into
`@podips-v4`, a connection that was established earlier and is riding the
`ct state established,related accept` rule indefinitely, a re-roll that
happened to land the AGC on a node where conntrack was warm, etc. None of
these are stable; the next cold restart would surface the same 443 drop.

## Recommended fix

The minimal, environment-agnostic change is to ensure the AGC's apiserver
egress rule matches both the in-Service port (`443`, for production where the
Service does no port translation) and the kind-style host port (`6443`, for
kind dry-runs and any cluster where the apiserver Endpoints listen on `6443`).

Two equivalent options in [`cmd/gmc/internal/controller/builder.go`](../../cmd/gmc/internal/controller/builder.go) `buildAGCNetworkPolicy`:

**Option A — allow both ports explicitly** (most precise):

```go
{
    Ports: []networkingv1.NetworkPolicyPort{
        {Port: ptr(intstr.FromInt32(443))},
        {Port: ptr(intstr.FromInt32(6443))},
    },
},
```

**Option B — omit the port restriction on the apiserver rule** (simpler, but
trades off precision because anything reachable on any port via the AGC pod
identity would be allowed):

```go
{}, // no port, no peer → allow all egress
```

Option A is the recommendation: it preserves the principle that the AGC may
only egress on apiserver-style ports, while accommodating both the kind and
production target-port conventions. It's symmetric with how PR #59 handled
the proxy `ipBlock` → PodSelector case: align the rule with the *post-DNAT*
shape of the packet.

A code comment on the rule should call out the reason explicitly so the next
reader (in particular: the next person tempted to "tidy up" the duplicate
ports) doesn't strip `6443` thinking it's redundant.

## Should PR #62 be reverted?

**Yes, in the strict sense that PR #62's stated rationale is wrong**: NP
additivity is fine; the AGC label combo was not the cause; "single NP per
pod" is not load-bearing for correctness. After Option A above lands, the
AGC pod can safely carry both `app: actions-gateway-controller` and
`actions-gateway/component: workload`, and both NPs will additively allow:
DNS + 443 + 6443 + 8080→proxy.

**However, the "single NP per pod" shape PR #62 introduces is independently
reasonable** — it has lower cognitive overhead and avoids implicit reliance
on NP additivity. If the team prefers that shape, keep PR #62, but:

- Add port `6443` (or omit the port) to the AGC NP's apiserver egress rule.
  Without this, PR #62 is shipping a known-broken default for the kind dry-run
  path.
- Update the comments PR #62 added in `builder.go` (currently they reference
  "whatever was actually breaking" as unknown) to point at this writeup so
  future readers don't re-litigate.

Either path resolves the live symptom. The non-negotiable change is the port
rule.

## Should an upstream issue be filed?

No — this is documented expected behaviour of every well-known
NetworkPolicy implementation, not a bug. Cilium, Calico, kube-router, and
kube-network-policies all evaluate post-DNAT for ingress/egress port rules
because the alternative (recovering the pre-DNAT 5-tuple via conntrack at
policy eval time) is expensive and ambiguous in the presence of policy
chaining. The upstream
[`kubernetes-sigs/kube-network-policies` README](https://github.com/kubernetes-sigs/kube-network-policies)
makes no claim of pre-DNAT evaluation; the
[kindnet NP docs](https://kindnet.es/docs/user/network-policies/) link to it
unchanged.

What is worth fixing upstream is the **discoverability** of this trap: it
took two bugs against this repo (the `ipBlock` proxy case in PR #59 and now
the port case here) to surface the pattern. Worth a short note in
`docs/development/kind-iteration.md` extending the existing "NetworkPolicy
enforces after kube-proxy DNAT" section to call out the port axis explicitly.

## Follow-up tasks

| ID | Action |
|----|--------|
| 1 | Patch `buildAGCNetworkPolicy` per Option A above; add a regression unit test asserting both `443` and `6443` appear in the apiserver egress rule with a comment explaining why both. |
| 2 | Extend `docs/development/kind-iteration.md` "NetworkPolicy enforces after kube-proxy DNAT" section with the port-mismatch variant and a pointer to this doc. |
| 3 | Decide on the single-vs-two-NPs-per-AGC question on aesthetic/maintainability grounds, independent of correctness. Update PR #62 comments either way. |
| 4 | Implement `E2E_GMC_TenantProvisioning_ProxyConnectWorks` (Queue item 5c) so this class of post-DNAT NP failure is caught by a single Tier-A kind e2e spec, per the pattern in [`docs/plan/e2e-tests.md`](e2e-tests.md). |

Item 4 is the highest leverage of the four: a single label-matched debug pod
in the e2e namespace doing `nc -zv 10.96.0.1 443` would have caught both
PR #59's proxy `ipBlock` bug and this AGC `port: 443` bug from a single
spec, locally, in seconds.
