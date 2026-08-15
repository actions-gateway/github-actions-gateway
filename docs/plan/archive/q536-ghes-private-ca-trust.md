# Q536 — Trust a private CA in front of a GHES appliance

Q506 shipped Option A, which made GHES a supported endpoint: the AGC's API base URL, the FQDN allowlist, and the CONNECT suffix list all derive from `spec.githubURL`.
Its audit named one adjacent gap and declined to fix it — nothing in the product lets an operator extend the AGC's TLS trust, so a GHES appliance fronted by a private or internal certificate authority still fails the handshake.
This plan closes that gap: an optional ConfigMap reference on the `ActionsGateway` whose PEM bundle is added to the trust pool of both the AGC and the worker pods it provisions.

## Status at a glance

| # | Item | Status |
|---|---|---|
| 0 | Measure the defect and the absence of a configuration surface | ✅ Done — see [What was measured](#what-was-measured) |
| 1 | `spec.githubCABundleRef` on `v2alpha1` + `v2beta1`, with the reference resolved fail-closed | ✅ Done |
| 2 | GMC mounts the bundle on the AGC Deployment and names it for the provisioner | ✅ Done |
| 3 | AGC merges the bundle into the shared transport trust pool | ✅ Done |
| 4 | Provisioner projects the bundle into worker pods; the wrapper concatenates it | ✅ Done |
| 5 | Docs: onboarding, upgrade note, troubleshooting, design | ✅ Done |

## What was measured

The Queue row asserts a mechanism.
Both halves of it hold, and neither rests on a live appliance.

**The trust pool rejects a private CA.** The AGC's pool builder seeds from `x509.SystemCertPool` and appends the egress proxy's self-signed CA ([`trustpool.go`](../../../cmd/agc/internal/transport/trustpool.go), named `BuildProxyTrustPool` before this change).
The repo already asserts the consequence: `TestBuildTrustPool_RejectsUnrelatedCA` ([`trustpool_test.go`](../../../cmd/agc/internal/transport/trustpool_test.go)) signs a leaf with a CA that is in neither the system store nor the proxy CA and requires `Verify` to fail.
A GHES appliance behind a private CA is exactly that leaf.
The test was written to prove the pool does not *over*-trust; read the other way it is the measurement this row needs.

Routing through the egress proxy does not change it.
The proxy tunnels with `CONNECT`, so the AGC↔GHES TLS session is end to end and the AGC validates the appliance's own certificate against the same pool.

**No configuration surface exists to extend it.** Swept, all negative:

| Surface | Result |
|---|---|
| `api/v2alpha1`, `api/v2beta1`, `cmd/gmc/api/v1alpha1` | No CA/trust/certificate field on any kind |
| `charts/actions-gateway/values.yaml` | Certificate values are the webhook and metrics serving certs only |
| GMC flags ([`flags.go`](../../../cmd/gmc/cmd/flags.go)) | No CA flag; nothing that adds a volume to an AGC Deployment |
| AGC Deployment builders (v1 `builder.go`, v2 `actionsgateway_v2_builder.go`) | The only CA volume is the proxy TLS Secret, mounted only when egress is proxied |
| `--allow-agc-extra-env` | Env only, no volume — and `AGC_EXTRA_SSL_CERT_FILE` would name a path nothing mounts. Documented as testing-only |

So the gap is the missing mount, not anything the appliance does — the same shape as Q506 finding #1, whose defect was the missing injection.

## Scope

**In:** the AGC control plane and the worker pods it provisions.
A GHES tenant whose AGC trusts the appliance but whose runners do not is still broken — `actions/checkout`, log upload, and artifact upload all talk to the appliance from the worker pod.

**Out:**

- **The GMC.** Its only GitHub call is the `api.github.com/meta` IP-range fetch, which is public-GitHub-only by construction (Q506 #3) and is not addressed to a tenant's appliance.
- **The egress proxy.** It terminates no TLS to GitHub; a `CONNECT` tunnel is opaque bytes.
  Its own serving certificate is a separate, GMC-issued CA that this bundle neither replaces nor extends.
- **`v1alpha1`.** The API is frozen and removed at `v2.0.0` ([deprecation notice](../../operations/v1alpha1-deprecation.md)); every field added since — `scheduling`, `agcAutoscaling`, `clusterCapacity` — is v2-only.
  A GHES tenant behind a private CA must be on v2.
- **Egress reachability.** Trusting the appliance's CA does not put its address space in the NetworkPolicy.
  That is Q506 #3, already surfaced as `GitHubEgressIncomplete`, and it stays the operator's obligation.

## Design

### The field

`ActionsGatewaySpec.githubCABundleRef` — optional, name-only reference to a ConfigMap in the gateway's own namespace, carrying the bundle under `ca.crt`.

Applying the [api-review](../../development/api-review.md) rules:

- **Whose fact is it?** The tenant's.
  It is the same party that supplies `spec.githubURL`, and it is the only party that knows which CA fronts that host.
  The blast radius is contained: the bundle widens the trust of that tenant's own AGC pod and its own worker pods and reaches nothing else, and a tenant that wanted to point at a hostile endpoint could already do so with `githubURL`.
- **ConfigMap, not Secret.** A CA certificate is public material.
  A Secret would put it behind the credential RBAC surface for no benefit and read as if the contents were sensitive.
- **Additive, never replacing.** The system roots stay trusted, so a gateway on public GitHub with a bundle set (a mixed estate applying one manifest) behaves identically.
  Replacing would be the insecure-by-default direction and is not offered.
- **Fixed key `ca.crt`.** Matches `metricsCACertKey` and the upstream `kube-root-ca.crt` convention.
  A `key` override is reachable additively later if demand appears.
- **Optional pointer, no default.** Unset and "empty bundle" are different states; unset must mean "system roots only", which is the safe direction.
- **A new ref type, not `LocalObjectRef`.** `LocalObjectRef` carries the v2 52-character object-name budget (§H.6); a ConfigMap is a core object with the 253-character DNS-subdomain budget.
  `LocalConfigMapReference` mirrors the existing `LocalSecretReference` shape.

### Resolution is a runtime condition, not admission

Per §H.7, a reference to a not-yet-applied object is well formed.
The GMC resolves it in the reconcile and fails closed the same way `defaultProxyRef` does — `Degraded=True` plus `Ready=False`, via the existing `setNotReady` — with two new reasons:

| Situation | Reason |
|---|---|
| ConfigMap absent | `CABundleNotFound` |
| Present but no `ca.crt`, or it holds no parseable certificate | `CABundleInvalid` |

No new condition type.
`ProxyNotFound` already sets `Degraded` for the sibling case of an unresolvable reference, and a condition type carries no version and no conversion path — the cheaper surface is the right one.

Parsing in the GMC rather than letting the AGC fail at startup is deliberate: a `Degraded` condition naming the ConfigMap is a better operator signal than a `CrashLoopBackOff`.

**The read is uncached and the recovery is a poll, not a watch.** The sibling credential Secret converges on a `WatchesMetadata`, and that was the first design here too.
It does not work: `buildCacheOptions` pins the GMC's ConfigMap informer to a single object in the GMC's *own* namespace precisely so the GMC needs no cluster-wide ConfigMap read (Q188/Q242 G.1).
Watching a tenant ConfigMap would undo that scoping.
So the reconciler reads through `mgr.GetAPIReader()` and returns a one-minute `RequeueAfter` while the reference is unresolved — the same shape the VPA CRD reprobe already uses, and the reason the RBAC grant is `get` alone with no `list`/`watch`.

### The mount and the two consumers

| Hop | Carrier |
|---|---|
| GMC → AGC pod | ConfigMap volume `github-ca` at `/etc/actions-gateway/github-ca/ca.crt`, plus `GITHUB_CA_CONFIGMAP_NAME` on the Deployment |
| AGC → worker pod | The provisioner reads `GITHUB_CA_CONFIGMAP_NAME` and projects the same ConfigMap into the runner container, setting `GITHUB_CA_CERT_PATH` |
| Worker wrapper → runner | The existing bundle concatenation gains a second input alongside `PROXY_CA_CERT_PATH`; `SSL_CERT_FILE` still points at the combined file |

Both hops copy a mechanism that already exists for the proxy CA (`PROXY_TLS_SECRET_NAME` → provisioner → `PROXY_CA_CERT_PATH` → wrapper), so this adds a second input to each stage rather than a new pathway.

`BuildProxyTrustPool` becomes `BuildTrustPool(pems ...[]byte)`: same semantics (all-empty returns `(nil, nil)`; a non-empty PEM with no parseable certificate is an error), one more source.
The name stops claiming the pool is proxy-specific, and the file moves with it (`proxytrust.go` → `trustpool.go`).
The worker wrapper's `installProxyCATrust` generalises the same way, to `installCATrust(runnerHome, caPaths...)` writing `ca-bundle.crt`.

The two sources differ in what an absent file means, and the difference is the opt-in.
The proxy CA is optional at runtime, so absent (and empty) is a logged no-op and only a genuine read failure is fatal — Q520's rule, which landed on `main` mid-build and which the merged `configureTrustPool` keeps verbatim.
The GitHub CA bundle is read only when `GITHUB_CA_CONFIGMAP_NAME` is set, which *is* the gateway's explicit opt-in, so absent, unreadable, and empty are all fatal: there is no "not configured" reading of any of them, and running untrusting is the failure the whole change exists to remove.

## Acceptance

- A gateway with `githubCABundleRef` set produces an AGC Deployment mounting that ConfigMap and carrying `GITHUB_CA_CONFIGMAP_NAME`; a gateway without it produces a byte-identical Deployment to today's.
- A gateway naming a ConfigMap that does not exist, or one without a parseable `ca.crt`, is `Ready=False`/`Degraded=True` with the reason above and provisions no AGC — and recovers without operator action once the ConfigMap is applied.
- The AGC's shared transport validates a leaf signed by the supplied CA, still validates a system-rooted leaf, and still rejects an unrelated CA's leaf.
- A worker pod for such a gateway mounts the bundle and its `SSL_CERT_FILE` target contains both the system roots and the supplied CA.
- Each of the above fails when its mechanism is deleted — a test that passes on a missing mount is the negative-assertion trap.
- Operator docs state what a GHES tenant behind a private CA must supply, and the troubleshooting page maps the `x509: certificate signed by unknown authority` symptom to this field.

## What this plan will not verify

No GHES appliance is involved.
The end-to-end claim — that a real appliance behind a real internal CA completes the handshake once the bundle is supplied — stays unobserved, exactly as Q506's own fix did.
What is verifiable here is that the bundle reaches both pools and that those pools then accept a leaf signed by it, which is the whole of the mechanism this plan adds.
