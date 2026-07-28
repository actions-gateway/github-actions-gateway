# Q444 — PriorityClass VAP cannot resolve its params

**Status: OPEN.** The defect is real and reproduced. The mechanism is *not*
established, and the first attempted fix was reverted.

Symptom: every `runnergroups` / `runnersets` / `runnertemplates` write is denied
with

```
ValidatingAdmissionPolicy 'gmc-priorityclass-allowlist-guard' with binding
'gmc-priorityclass-allowlist-guard-binding' denied request: failed to configure
binding: no params found for policy binding with `Deny` parameterNotFoundAction
```

while the param ConfigMap sits at exactly the name and namespace the binding
references. Because `parameterNotFoundAction: Deny` resolves params *before* any
per-object matching, this denies **every** matched write, class-naming or not —
a total outage of the product's CRs, cluster-wide.

Observed on Kubernetes 1.35.5 and 1.36.1. Prior sighting:
[`archive/q414-dind-tenant-fixture.md`](archive/q414-dind-tenant-fixture.md)
§ Local-loop notes.

## Established by measurement

1. **The broken state is per-kube-apiserver-process.** A genuine restart clears
   it in ~3s with **zero object changes**.

   Restart it with `crictl stop` on the container and confirm via the container's
   `createdAt` changing. **`kubectl delete pod -n kube-system kube-apiserver-…`
   does not restart it** — that recreates the static pod's *mirror object* while
   the container keeps running (`restartCount` stays 0). A conclusion was drawn
   from that non-restart during this investigation and had to be withdrawn.

2. **Once broken, ConfigMaps created afterwards stay invisible to param
   resolution.** A *fresh* `helm install` on an already-broken apiserver fails
   the same way, with the binding pointing at a ConfigMap that demonstrably
   exists (verified name, namespace, UID, data, namespace phase `Active`).
   So "uninstall/reinstall" is not the trigger so much as the first casualty.

3. **It is paramKind-specific.** Two identically-shaped policies created at the
   same instant, differing only in `paramKind`: the ServiceAccount one resolves
   its param, the ConfigMap one cannot.

4. **CI reproduces it on both CNI lanes** (kindnet and calico), after the e2e
   suite, so it is not CNI-specific.

## Ruled out

**Retaining the paramKind-bearing policy across the uninstall is not the fix.**
Shipped as `07061175`, reverted in `70b4b351`. CI showed the policy retained (the
check logs it) and params still never resolving — the probe retried a full 90s.

**No single object deletion reproduces the teardown.** Against a freshly
restarted apiserver, all four of these recover cleanly:

| scenario | result |
|---|---|
| binding + param ConfigMap deleted, policy retained | recovers |
| binding deleted, ConfigMap retained, policy retained | recovers |
| ConfigMap deleted only, policy + binding live | recovers |
| control, nothing deleted | recovers |

So neither "the last policy naming the GVR was deleted" nor "the binding was
deleted" nor "the ConfigMap was deleted" is sufficient on its own.

## Open questions — where to pick this up

- What actually kills the informer? The scenarios above are the obvious
  candidates and all of them recovered, so the trigger is something they do not
  capture: object churn *rate*, an interleaving of deletes, or cluster load.
- Does it need a cluster with many ConfigMaps? The param informer watches
  ConfigMaps cluster-wide, and both CI reproductions were on clusters where the
  full e2e suite had just run.
- Is this a known upstream apiserver bug? Not yet searched. Worth doing before
  more black-box bisection — the fix may be a version bump or an upstream issue
  to track rather than anything the chart can do.

## Reproducing

`scripts/chart-reinstall-check.sh` (`make chart-reinstall-check`) drives the
cycle against a cluster with the release already installed and prints a
diagnostic dump — policy, binding, `paramRef` target, the ConfigMap's existence
and UID, namespace phase — that separates a broken manifest from a broken
apiserver.

It is **deliberately not wired into CI** while Q444 is open: the defect is
unfixed, so it would pin every run red. Wiring it into `e2e-reusable.yml` (after
the e2e suite, which leaves the release up under `E2E_SKIP_TEARDOWN`) is part of
closing this out.

Note it does not fire on every run — a clean pass does not mean fixed.

## Operator impact while this is open

Recovery is a kube-apiserver restart, which is **not available on a managed
control plane** (GKE/EKS/AKS). There is no chart-side workaround today. The
blast radius and the recovery step are documented in
[`../operations/troubleshooting.md`](../operations/troubleshooting.md).
