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
- **Upstream: this is a kube-apiserver bug, and it is filed.** Our evidence is
  posted on [kubernetes/kubernetes#130887](https://github.com/kubernetes/kubernetes/issues/130887#issuecomment-5105004944),
  which reports the same symptom on the same `paramKind` and is open and
  untriaged. Helm is not involved in param resolution at all; it only did the
  delete/create churn.

  Related but **not** the same bug:
  [#122658](https://github.com/kubernetes/kubernetes/issues/122658) /
  [PR #123003](https://github.com/kubernetes/kubernetes/pull/123003) — there a
  CRD `paramKind` fails because discovery has not caught up and an apiserver
  restart *causes* it; the fix (retry a failed paramKind sync) merged for v1.30
  in Jan 2024, long before the versions we see this on. Ours is a core type that
  is always discoverable, and a restart *fixes* it.

  So the realistic path to closing Q444 is upstream, not chart-side. Watch
  #130887; if it moves, re-test and wire the reproducer into CI.

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

**Our own dogfood is exposed.** [`scripts/dogfood/setup.sh`](../../scripts/dogfood/setup.sh)
runs `helm upgrade --install gag charts/actions-gateway` with no
`admissionPolicy` override, and the chart defaults `admissionPolicy.enabled:
true`. Dogfood is GKE, so if that cluster ever enters this state we cannot
restart its apiserver: every `runnergroups`/`runnersets` write stays denied
until a control-plane version upgrade happens to recycle the process. The
mitigation available there is the same one operators get, `admissionPolicy.enabled=false`
plus the GMC webhook allowlist.

This exposure is why the item stays in the Queue rather than moving to
Deferred. Parking it would mean waiting on an upstream issue that has carried
`sig/api-machinery` since 2025-03 and is still `needs-triage`, while we hold
the risk.

**Surface this in the next release's notes.** While Q444 is open it is exactly
the "upgrade caveat" the curated-notes path in
[`../operations/release.md`](../operations/release.md) exists for. An operator
upgrading to a release that ships `admissionPolicy.enabled: true` should learn
about it from the notes, not from a denied write. The install-time decision is
documented at
[install.md § Known defect (Q444)](../operations/install.md#known-defect-q444-the-policy-can-stop-resolving-its-parameters);
the release notes only need a line and that link.
