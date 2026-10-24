# Q1184: proxy failure attribution, implementation plan

Builds [05-security.md § Proxy failure attribution](../design/05-security.md#proxy-failure-attribution), which settles the posture: off by default on two switches, failures only in a per-namespace in-memory buffer, the consumer namespace learned from the port a connection arrives on, and an AGC reading only its own namespace's records over a dedicated mTLS listener.
The row is [Q1184](../queue/Q1184.md).
Its consumer, classified retry ([Q1183](../queue/Q1183.md)), is blocked, so until it lands nothing reads what this builds.

## Status

| Phase | Scope | Status |
|---|---|---|
| 1 | Proxy binary: attributed CONNECT listeners, the failure buffer, the mTLS read endpoint | ✅ Shipped in #2112 (`cmd/proxy/attribution.go`) |
| 2a | API and the pool: the two switches, the `allowedNamespaces` cap, port allocation, Service ports, per-port ingress, the attribution PKI, the proxy environment | ❌ In progress |
| 2b | The consumer: projecting client certificates, carrying the port to the AGC, moving workers' `HTTPS_PROXY`, the AGC's egress, operator docs | ❌ Open |
| 3 | AGC: pulling every replica, the epoch and cursor, the join on its own gateway's pod lifetimes | ❌ Open |
| 4 | Calico e2e: a worker refused on another namespace's port, and a denial attributed end to end | ❌ Open |

Each phase is its own PR.
The row stays open until phase 4 lands.

## Phase 1: the proxy binary

Off unless configured: with no attributed ports the proxy behaves exactly as today.

- **Attributed listeners.** `PROXY_ATTRIBUTION_PORTS` is a comma-separated `namespace=port` list.
  Each entry is one more CONNECT listener on the same `http.Server`, so shutdown's linger, drain and `ConnState` stamp cover it with no new code.
  `ConnContext` maps each connection's local port to its namespace and carries it into `handleConnect`; the listener is not wrapped, so the relay keeps its `*net.TCPConn` half-close.
  The existing `PROXY_PORT` listener stays untagged, which is what makes it the unattributed port.
- **The failure buffer.** On a denial or a dial failure arriving on a tagged listener, `handleConnect` records the namespace, the client source address, the time, the outcome, a dial-failure class, and the capped host and port.
  Records are partitioned by namespace, capped per namespace and per source address within it, and expire after a fixed window.
  Every record carries a sequence number counted per namespace, so a reader's gaps reveal nothing about other tenants, and the store a random epoch drawn at start, so a reader can tell a restart from a quiet period.
- **The read endpoint.** `PROXY_ATTRIBUTION_TLS_CERT_FILE`, `_KEY_FILE` and `_CLIENT_CA_FILE` configure an mTLS listener on `PROXY_ATTRIBUTION_PORT` (default 8444).
  `GET /v1/failures?after=<seq>` returns the epoch and the records of the namespace the verified client certificate's common name maps to, after that sequence.
  A name not in the port map is refused 403, so removing a namespace from the map ends its access.
  Attributed ports without the TLS files fail startup: per-job data is never served unauthenticated.

## Implementation decisions the design left open

- **How the proxy learns its port map: environment, so a change rolls the pool.** The destination allowlist already reaches the proxy as environment and rolls the pool when it changes, and a namespace joining or leaving attribution is rarer than an allowlist edit.
  A mounted, reloaded file would avoid the rollout at the cost of dynamic listener management in the one component untrusted code talks to; revisit only if the rollouts are measured to hurt.
- **How the AGC reaches each replica: a headless Service's DNS records.** It needs no RBAC in the provider's namespace, which a cross-namespace reader does not have, and the AGC already has cluster DNS egress.
  The AGC verifies the server certificate against the attribution CA with the regular Service name as the server name, so the dial address does not need to be in the certificate.

## Phase 2: the API and the pool

Phase 2 is two PRs, because moving a namespace onto its port is not the GMC's alone to do.
The AGC computes each worker's `HTTPS_PROXY` itself on every acquired job (`cmd/agc/internal/controller/runnerset_target.go`), from the share ConfigMap for a pool in another namespace and from a hardcoded `:8080` for a pool in its own, so the move needs an AGC change as well as a GMC one.

**2a, the pool side.** Everything the provider namespace holds, inert to consumers until 2b moves them:

- **The fields.** `EgressProxy.spec.failureAttribution` (`Disabled` by default, or `PerConsumerPort`) and `ActionsGateway.spec.proxyFailureAttribution` (`Disabled` by default, or `PerJob`), identical in v2, v2beta1 and v2alpha1.
  Each names the method rather than an on-state, and neither uses `Off`, which YAML reads as a boolean.
  While the pool's is `PerConsumerPort`, a CEL rule caps `spec.sharing.allowedNamespaces` at 32.
- **Which namespaces get a port.** A namespace that references the pool, from a gateway's `defaultProxyRef` or a RunnerSet's `proxyRef`, that the pool admits (its own namespace, or one in `allowedNamespaces`), and where any gateway has opted in.
- **Allocation.** Ports start at 8100, one per attributed namespace.
  The current map is read back from the live Deployment's `PROXY_ATTRIBUTION_PORTS`, so an existing namespace keeps its port across reconciles and only a new one is assigned, to the lowest free port.
  The map lives in the owned object rather than in status because a controller must not make a decision from a value only status carries ([api-review.md](../development/api-review.md#status-reports-it-does-not-control)), and a lost Deployment only reassigns ports.
- **The Service and a headless Service.** Each port becomes a `attr-<port>` port on `<ep>-proxy` targeting the same container port by number.
  `<ep>-proxy-attr` is a headless Service on the read port, whose DNS records name each replica for phase 3.
- **Ingress.** One rule per attributed namespace admits that namespace's workload pods on its port only, and one admits its AGC pods (`app.kubernetes.io/name: actions-gateway-controller` with the workload label, both in one peer) on the read port.
  Worker pods never carry the AGC's name label: the provisioner builds their labels from a fixed set and does not pass template labels through.
- **The PKI, regenerated rather than kept.** One bundle holds a CA, a server certificate for the `<ep>-proxy` Service names, and a client certificate per attributed namespace with that namespace as its common name.
  The CA key is never persisted, as with the metrics bundle: the whole bundle regenerates when the namespace set changes or nears expiry.
  A namespace-set change already rolls the pool through `PROXY_ATTRIBUTION_PORTS`, and a hash of the CA in the pod template rolls it on renewal.
  The proxy reads its CA only at start, so until each replica rolls, it rejects the new client certificates: phase 3's reader treats a failed handshake with one replica as a gap in that replica's records, not as an error.
  The client certificates wait in the provider namespace until 2b projects them.
  The GMC holds no delete verb on Secrets, so when nothing is attributed it empties the two Secrets rather than deleting them; a pool that never attributed makes no call for them at all.
- **Off is unchanged.** A pool with no attributed namespace gets none of the above, and its pod template is byte-for-byte what it was, so upgrading the GMC rolls no pool.

**2b, the consumer side.** Projecting each namespace's client certificate into it and pruning it on revocation, carrying the attributed port to the AGC for a pool in its own namespace as well as another, the AGC moving its workers' `HTTPS_PROXY`, the AGC's egress to the read port, and the operator docs.
It also owes the port-reuse window: lowest-free allocation can give a joining namespace a port another just left, while replicas that have not rolled still map that port to the old namespace, so a joiner's failures can land in the leaver's partition until the roll completes.
Moving a namespace only after the pool has rolled, or not reusing a port freed in the same rollout, closes it.

## Phase 3 and 4 outline

- **Phase 3** pulls every replica on an interval, resets the cursor on an epoch change, keeps records only for its own gateway's pods inside each pod's address lifetime, and exposes a lookup the retry classes will call.
- **Phase 4** is the enforcement measurement the row makes a ship requirement, on the Calico lane only, because the kindnet lane's enforcement specs skip themselves.
