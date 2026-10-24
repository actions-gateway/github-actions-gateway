# Q1184: proxy failure attribution, implementation plan

Builds [05-security.md § Proxy failure attribution](../design/05-security.md#proxy-failure-attribution), which settles the posture: off by default on two switches, failures only in a per-namespace in-memory buffer, the consumer namespace learned from the port a connection arrives on, and an AGC reading only its own namespace's records over a dedicated mTLS listener.
The row is [Q1184](../queue/Q1184.md).
Its consumer, classified retry ([Q1183](../queue/Q1183.md)), is blocked, so until it lands nothing reads what this builds.

## Status

| Phase | Scope | Status |
|---|---|---|
| 1 | Proxy binary: attributed CONNECT listeners, the failure buffer, the mTLS read endpoint | ❌ Built in `cmd/proxy/attribution.go`, in review; inert until phase 2 sets its environment |
| 2 | API and GMC: the two switches, the `allowedNamespaces` cap, port allocation, Service ports, per-port ingress, the attribution PKI, moving a namespace's `HTTPS_PROXY` | ❌ Open |
| 3 | AGC: pulling every replica, the epoch and cursor, the join on its own gateway's pod lifetimes | ❌ Open |
| 4 | Calico e2e: a worker refused on another namespace's port, and a denial attributed end to end | ❌ Open |

Each phase is its own PR.
The row stays open until phase 4 lands.

## Phase 1: the proxy binary

Off unless configured: with no attributed ports the proxy behaves exactly as today.

- **Attributed listeners.** `PROXY_ATTRIBUTION_PORTS` is a comma-separated `namespace=port` list.
  Each entry is one more CONNECT listener on the same `http.Server`, so shutdown's linger, drain and `ConnState` stamp cover it with no new code.
  A wrapped listener tags each accepted connection with its namespace, and `ConnContext` carries it into `handleConnect`.
  The existing `PROXY_PORT` listener stays untagged, which is what makes it the unattributed port.
- **The failure buffer.** On a denial or a dial failure arriving on a tagged listener, `handleConnect` records the namespace, the client source address, the time, the outcome, a dial-failure class, and the capped host and port.
  Records are partitioned by namespace, capped per namespace and per source address within it, and expire after a fixed window.
  Every record carries a per-process sequence number, and the store a random epoch drawn at start, so a reader can tell a restart from a quiet period.
- **The read endpoint.** `PROXY_ATTRIBUTION_TLS_CERT_FILE`, `_KEY_FILE` and `_CLIENT_CA_FILE` configure an mTLS listener on `PROXY_ATTRIBUTION_PORT` (default 8444).
  `GET /v1/failures?after=<seq>` returns the epoch and the records of the namespace the verified client certificate's common name maps to, after that sequence.
  A name not in the port map is refused 403, so removing a namespace from the map ends its access.
  Attributed ports without the TLS files fail startup: per-job data is never served unauthenticated.

## Implementation decisions the design left open

- **How the proxy learns its port map: environment, so a change rolls the pool.** The destination allowlist already reaches the proxy as environment and rolls the pool when it changes, and a namespace joining or leaving attribution is rarer than an allowlist edit.
  A mounted, reloaded file would avoid the rollout at the cost of dynamic listener management in the one component untrusted code talks to; revisit only if the rollouts are measured to hurt.
- **How the AGC reaches each replica: a headless Service's DNS records.** It needs no RBAC in the provider's namespace, which a cross-namespace reader does not have, and the AGC already has cluster DNS egress.
  The AGC verifies the server certificate against the attribution CA with the regular Service name as the server name, so the dial address does not need to be in the certificate.

## Phase 2 to 4 outline

- **Phase 2** adds `EgressProxy` and `ActionsGateway` fields (named under [api-review.md](../development/api-review.md)), a `MaxItems` on `allowedNamespaces` enforced when attribution is on, port allocation stable across reconciles, the Service ports and a headless Service, numbered per-port ingress rules, an attribution CA with a server certificate and one client certificate per attributed namespace projected into that namespace, the AGC's egress to the read port, and the proxy environment.
  Operator docs move with it, since this phase is the first an operator can observe.
- **Phase 3** pulls every replica on an interval, resets the cursor on an epoch change, keeps records only for its own gateway's pods inside each pod's address lifetime, and exposes a lookup the retry classes will call.
- **Phase 4** is the enforcement measurement the row makes a ship requirement, on the Calico lane only, because the kindnet lane's enforcement specs skip themselves.
