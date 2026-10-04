# Dragonfly as the Mirror Backend — Q539

> **Status (2026-10-04): Phase 1 measured, Phase 2 built and validated on kind, Phase 3 owed a booked dogfood session.** Dragonfly fails the [mirror contract](q408-untrusted-pr-egress.md#35-the-mirror-role-is-a-contract) when it is the endpoint workers reach, so it is built as the **back end** of the Distribution mirrors rather than a substitute for them ([§2](#2-unfronted-dragonfly-fails-the-contract-by-construction)). §2–§6 were measured in local Docker and §7 on a kind cluster; nothing here has run on the dogfood cluster.

Q408 validated the untrusted-PR egress posture with CNCF Distribution as the mirror, and [§6](q408-untrusted-pr-egress.md#6-follow-on-validations-q539-q540) of that plan scheduled Dragonfly as the alternate backend to grade against the same four-property contract.
This plan is that grading, and the build it led to.

**Versions measured.** Dragonfly client (dfdaemon) `v1.5.7` (`dragonflyoss/client@sha256:e5a4479c3e9c…`, source at tag `v1.5.7`, commit `ada717934e86`), scheduler `v2.5.2`, Helm chart `dragonfly-1.8.5` (packaged chart sha256 `8567532e15c8…`).
Distribution `registry:3.1.1`, the digest `deploy/registry-mirror/` pins, as the reference arm.
A different client version is a different measurement.

## 1. The lab

One Docker network holding: a writable `registry:3.1.1` as the upstream, an nginx **canary** whose access log records each request's source IP, a fake scheduler (a gRPC health server that answers any other method with an empty message, enough for dfdaemon to finish startup), dfdaemon in seed-peer mode with its registry mirror pointed at the upstream, and a Distribution proxy pointed at the same upstream.
Every probe path carries a per-backend label, so a canary line names both the probe and the backend, and its source IP says whether the backend made the request or the probe container did.

The probes are the [§3.5](q408-untrusted-pr-egress.md#35-the-mirror-role-is-a-contract) properties turned into requests a hostile job could send.

## 2. Unfronted Dragonfly fails the contract by construction

| Probe, sent to the mirror endpoint | Distribution 3.1.1 | dfdaemon v1.5.7 |
|---|---|---|
| Pull a blob seeded in the upstream (control) | 200, content matches | 200, content matches |
| Blob push: `POST` an upload, then `PUT` the content | refused, no upload session | **201, and the blob reads back from the upstream directly** |
| `X-Dragonfly-Registry: http://canary` header | 404, canary silent | **reached the canary** |
| `?ns=http://canary` query, and the same with the key percent-encoded (`%6Es`) | 404, canary silent | **both reached the canary** |
| Absolute-URI `GET` and `POST` to the canary | 404, canary silent | **both reached the canary** |
| `CONNECT` to `https://example.com` | refused | **200, "Example Domain"** |

Every canary line came from dfdaemon's address, never the probe container's.
So a worker that can reach dfdaemon's proxy port can reach any host on the internet and can push to any registry it names; properties 2 (fixed upstream set) and 3 (read-only) both fail.

**Nothing in its configuration closes either one.** The proxy is a general forward proxy (`dragonfly-client/src/proxy/mod.rs`): a request with a host is forwarded to that host, `CONNECT` is answered with a TLS interception tunnel to any host, a mirror-form request takes its upstream from `X-Dragonfly-Registry` before `ns` before the configured address (`make_registry_mirror_request`), and every non-`GET` method is forwarded directly ("fall through to direct proxy to avoid data loss").
The proxy config (`dragonfly-client-config/src/dfdaemon.rs`, `struct Proxy`) offers rules, basic auth and a CA, and no destination or method restriction.
The manager-pushed block list applies only to P2P download tasks and only denies what it lists, so it can neither cover the direct path nor express an allowlist.

Property 1 holds: the chart's seed client is a StatefulSet with `hostNetwork: false` behind a ClusterIP Service.
Property 4 holds wherever workers are plain HTTP clients of the seed peer, but the four failures above are reachable by exactly that client.

## 3. A filter in front holds the contract and breaks Docker Hub

An haproxy in front of dfdaemon, on the image `deploy/registry-mirror/` already pins, with four rules: only `GET`/`HEAD`, only origin-form request targets, every `X-Dragonfly-*` header deleted, and the query string dropped (`set-uri %[path]`).

Against the same battery it matched Distribution: zero canary hits, no upload session, `CONNECT` and absolute URIs refused, `h2c` prior knowledge refused, and the seeded blob still pulled.
Removing one rule at a time and requiring the bypass it blocks to return:

| Rule removed | Result |
|---|---|
| Header delete | both header probes reach the canary again |
| Query drop | both `ns` probes reach the canary again |
| Method restriction | no blob lands, but the `POST` opens an upload session on the upstream: forwarded, which property 3 forbids. The `PUT` fails only because the query drop also strips its `digest` |
| Origin-form check | nothing returns: the `/v2/` path rule and the method rule overlap it |

**It does not work against Docker Hub.** Through dfdaemon, Hub answers a manifest request with **401** and `Bearer realm="https://auth.docker.io/token"`, which the client must then fetch itself.
Distribution's proxy fetched the same manifest with **200**, because it performs that token exchange server-side.
A worker under the Q408 posture reaches cluster DNS, GitHub and the mirrors, so it cannot reach `auth.docker.io`.
That last step is the posture's definition, not a probe taken here.

## 4. The shape that works: Distribution in front, Dragonfly behind

**Distribution 3.1.1 will not fetch a token for a remote it does not recognise.** `remoteAuthChallenger.challengeManager()` (`registry/proxy/proxyregistry.go`) drops any bearer challenge whose realm is neither the remote's own host nor on its registrable domain, and never trusts one for a single-label host.
So `remoteurl: http://<dragonfly-service>` sees Hub's challenge and discards it: measured, a Distribution pointed at dfdaemon logged `Challenge established with upstream` and answered 404, with every upstream request returning 401.
The token itself is not the obstacle: an anonymous Hub token attached by hand passed through dfdaemon and returned 200.

**So Distribution keeps its real `remoteurl` and sends its outbound traffic through dfdaemon** as an HTTPS proxy, which is Dragonfly's documented integration:

- `REGISTRY_PROXY_REMOTEURL=https://registry-1.docker.io`, unchanged, so the realm check passes.
- `HTTPS_PROXY=http://<seed-client>:4001` on the registry container, which Go's default transport honours for every upstream call, token requests included.
- dfdaemon terminates the tunnel with a certificate from a CA it is given (`proxy.server.caCert`/`caKey`), and the registry container trusts that CA alone: `SSL_CERT_FILE` names it and `SSL_CERT_DIR` names the directory holding only it.
  Both are needed, because Go also reads the image's populated `/etc/ssl/certs` unless `SSL_CERT_DIR` is set: with `SSL_CERT_FILE` alone, a registry with no proxy at all fetched the alpine index from Hub with 200, and with both it refused at startup with `x509: certificate signed by unknown authority`.
- dfdaemon verifies the upstream only because `proxy.registryMirror.cert` names the image's public bundle (`/etc/ssl/certs/ca-certificates.crt`).
  Left unset, v1.5.7 builds its direct-path client with no certificate verifier at all (`proxy/mod.rs`, whose comment says "native roots"): a `CONNECT` through it to `self-signed.badssl.com` and to `expired.badssl.com` each returned 200.
  Set, both failed, and `registry-1.docker.io/v2/` still answered 401.
- Proxy rules `blobs/sha256.*` and `manifests/sha256.*` send content-addressed `GET`s through the P2P path; everything else, tag lookups and token requests, goes direct.

Measured end to end against Hub, before `registryMirror.cert` and `SSL_CERT_DIR` were added: `library/alpine:3.20`'s index and amd64 manifest returned 200, and its first layer returned 200 at 3,630,321 bytes with a sha256 equal to its digest. dfdaemon's log shows the layer taking the P2P path (one "proxy HTTPS request via dfdaemon by rule config" line) and every other request going direct (twelve "directly to remote server" lines), with `auth.docker.io` named on eight log lines.
Re-measured with both added: the same layer returned 200 at the same size and digest, with two requests taking the P2P path.

**The contract is Distribution's again, and Dragonfly is not on the worker's path.** Workers reach the five Distribution Services exactly as under Q408, whose properties were measured there; the dfdaemon proxy, the open forward proxy of §2, is admitted only from the mirror pods by NetworkPolicy.
What Dragonfly adds is the back end: P2P distribution and dedup of layer blobs across seed peers, in place of five independent upstream fetches.

**What moves.** Distribution no longer verifies upstream TLS itself; it verifies dfdaemon's interception certificate and relies on dfdaemon to verify the upstream. dfdaemon does so on the direct path, which carries tag lookups and token requests, against the same public roots Distribution used.
The P2P path does not: dfdaemon's HTTP back end fetches with no certificate verification whatever the proxy config says (read from source at v1.5.7, not probed).
The rules limit that path to digest-addressed content, so a tampered blob or manifest has the wrong digest; a client pulling by digest rejects it, and whether Distribution itself rejects a proxied blob whose bytes do not match its digest is unmeasured here.
Tag-to-digest resolution therefore trusts dfdaemon as well as the upstream; a digest-pinned pull still re-verifies client-side, exactly as [q408 §3.1](q408-untrusted-pr-egress.md#31-the-mirror--one-pull-through-cache-per-upstream) argues.
Anyone holding the CA's key can impersonate any upstream to the mirrors.
It is stored only in `gag-dragonfly`, and readable beyond it by any principal with cluster-wide Secret read, the GMC's `manager-role` among them.
The start script creates it with `kubectl create` rather than `apply`, which would copy the key into a `last-applied-configuration` annotation that `kubectl describe` prints.

**Startup order matters.** Distribution pings its remote while starting and panics if the ping fails, so a mirror pod started before dfdaemon is listening crash-loops until it is; measured once, recovered on restart.
So does load order: dfdaemon and Distribution both read the CA at startup, so a CA minted under running pods is not seen by either until they restart.

**The key must be PKCS#8.** Helm's `genCA` emits a PKCS#1 RSA key, which dfdaemon v1.5.7 rejects ("Could not parse key pair") and then silently falls back to a self-generated CA the mirrors do not trust.
`genPrivateKey "ed25519"` with `genCAWithKey` emits PKCS#8 (`BEGIN PRIVATE KEY`), which it loads.

## 5. Per content class

[caching-and-worker-storage.md](caching-and-worker-storage.md) asks for the contract graded per content class rather than once.

| Class | Can one Dragonfly deployment serve it under the contract? |
|---|---|
| Images | Yes, as the back end of §4. Not as the worker-facing endpoint (§2) |
| Artifacts and `actions/cache` restores | Not as a fixed upstream. The read path is a `GET` of a signed URL whose host names the storage account, so a filter can pin no tighter than the `*.blob.core.windows.net` suffix, which admits every account on Azure. Read from the source and the filter's rules, not probed |
| `actions/cache` saves | No. dfdaemon forwards every non-`GET` to the origin rather than storing it (§2), so it holds no write path to contract. This stays [Q215](../queue/Q215.md)'s question |

## 6. Non-root

Both images declare no user, and chart 1.8.5 exposes no pod or container security context.
Run as uid 65532 with a read-only root filesystem, every capability dropped and `no-new-privileges`, both start and serve: dfdaemon with writable `/var/lib/dragonfly`, `/var/log/dragonfly`, `/var/run/dragonfly`, `/var/cache/dragonfly` and `/tmp`; the scheduler with those plus `/usr/local/dragonfly` and `/etc/dragonfly`, where it writes its dynconfig at startup.
So the namespace can enforce PSA `restricted`, and the security context is a patch over the chart's render.

## 7. Phases

- **Phase 1 — grade Dragonfly against the contract. ✅ Done (2026-10-04, local).** §2–§6.
- **Phase 2 — build the back end. ✅ Done (2026-10-04, kind).** `deploy/dragonfly/`: a scheduler and one seed peer with no manager, node DaemonSet or injector (so no MySQL and no Redis; the seed finds the scheduler through a static dynconfig), digest-pinned, non-root under PSA `restricted`, with NetworkPolicies admitting only the mirror pods to the proxy port.
  `deploy/registry-mirror/components/dragonfly-backend` points the five registry containers at it, and `overlays/dragonfly` applies it.
  The CA comes from `deploy/dragonfly/ca-chart`, Helm's built-in certificate functions as the GAG chart already uses them for its webhook certificate, so no cert-manager; its key goes from `helm template` to `kubectl create` through a pipe and never touches disk.
  `scripts/dogfood/e2e-start.sh` gains `E2E_MIRROR_BACKEND=dragonfly`, minting the CA only when its Secret is absent and restarting both sides when it does; the default stays Distribution alone, and `e2e-stop.sh` scales Dragonfly to zero but keeps the CA.

  Measured by running the script's own functions against a kind cluster with kindnet enforcing NetworkPolicy:

  | Reading | Result |
  |---|---|
  | A worker pod pulls `library/alpine` through `mirror-docker-io` | index, manifest and arm64 layer 200; layer 4,092,319 bytes, sha256 equal to its digest |
  | The other four mirrors | manifest 200 from each |
  | Push to a mirror | 405 |
  | The seed peer's log | names all five upstream hosts and `auth.docker.io`; 2 requests by P2P rule, 35 direct, 0 errors |
  | A worker pod, and a pod in another namespace, to the seed proxy | timeout |
  | A mirror-labelled pod to the seed proxy | 200 |
  | The other-namespace pod, with the three policies selecting the seed pod deleted | 200; restored, timeout again. The worker is also held by its own egress policy, so this inversion needs a pod without one |
  | Re-running the start script | CA unchanged, no restart; the mirrors' copy matches the Secret's byte for byte |

  kind has no GMC, so the lab applied a stand-in DNS egress policy for the worker namespace that the GMC-managed default-deny provides on dogfood.
- **Phase 3 — validate on dogfood.
  Not started; needs a booked session.** Held on 2026-10-04 while another session was using the cluster.
  The Q408 Phase-4 sequence with `E2E_MIRROR_BACKEND=dragonfly`: the [§3.7](q408-untrusted-pr-egress.md#37-the-phase-2-validation-battery) battery, one Kata e2e run whose in-job negatives must pass unchanged, and the mirror hit counts.
  Two readings this variant adds: blob `GET`s in dfdaemon's log taking the P2P path, and a worker unable to reach the seed peer's proxy port.

## 8. What this plan does not cover

The node layer, where a `hostNetwork` dfdaemon serves the kubelet's own pulls, is [Q540](../queue/Q540.md). §2 applies there directly: chart 1.8.5's node client listens on `0.0.0.0:4001` on every node, so whether a worker pod can reach a node address on that port decides whether the open proxy is on the worker's path.
Unmeasured.
