# Q1184: proxy failure attribution, implementation plan

Builds [05-security.md § Proxy failure attribution](../design/05-security.md#proxy-failure-attribution), which settles the posture: off by default on two switches, failures only in a per-namespace in-memory buffer, the consumer namespace learned from the port a connection arrives on, and an AGC reading only its own namespace's records over a dedicated mTLS listener.
The row is [Q1184](../queue/Q1184.md).
Its consumer, classified retry ([Q1183](../queue/Q1183.md)), is blocked, so until it lands nothing acts on what this builds.

## Status

| Phase | Scope | Status |
|---|---|---|
| 1 | Proxy binary: attributed CONNECT listeners, the failure buffer, the mTLS read endpoint | ✅ Shipped in #2112 (`cmd/proxy/attribution.go`) |
| 2a | API and the pool: the two switches, the `allowedNamespaces` cap, port allocation, Service ports, per-port ingress, the attribution PKI, the proxy environment | ✅ Shipped in #2122 |
| 2b | The consumer: projecting client certificates, carrying the port to the AGC, moving workers' `HTTPS_PROXY`, the AGC's egress, operator docs | ✅ Shipped in #2125 |
| 3 | AGC: pulling every replica, the epoch and cursor, the join on its own gateway's pod lifetimes | ❌ In review (#2127) |
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

**2b, the consumer side.** What moves a namespace onto its port:

- **One projection carries both the port and the certificate.** For each attributed namespace the GMC writes a `proxy-attribution-<pool-namespace>-<pool>` ConfigMap (the port, the read Service's host and port, and the server name to verify) and a TLS Secret of the same name holding the namespace's client certificate.
  A pool in the consumer's own namespace is projected the same way, since the AGC otherwise hardcodes `:8080` for it.
  The ConfigMap's presence is the decision: the AGC overrides the port it resolved with the projected one, and uses the resolved port when there is none.
- **Projection waits for the rollout.** A namespace is projected only once it holds its port in the rolled-out Deployment and every replica runs that template, because a Service port with no listener on some replicas refuses part of the traffic sent to it.
  Withdrawal is immediate: new jobs go back to the shared port at once.
  A withdrawn Secret is blanked, since the GMC holds no delete on Secrets, and it is read by name before its ConfigMap goes, since the GMC reads Secrets only in tenant namespaces and never lists them cluster-wide ([Q1188](../design/05-security.md#gmc-privilege-escalation-blast-radius-and-compensating-controls)); a failed blank leaves the ConfigMap to find the namespace by on the next reconcile.
- **Workers move; the AGC does not.** Only the workers' `HTTPS_PROXY` changes, read per job, so nothing in the consumer's namespace restarts, and the AGC's own control-plane traffic stays on the shared port because its failures are not a job's.
- **Egress admits the range.** An opted-in namespace's workload policy admits the whole attributed range on the same proxy peers as `:8080`, and its AGC policy the read port; which port within the range is the pool's to decide, and its ingress pins each namespace to its own.
- **Opting out holds the port for running jobs.** A departing namespace the pool still admits keeps its listener and its workload ingress rule for the longest `maxWorkerLifetime` of its RunnerSets, counted from the reconcile that withdraws its projection and recorded as `actions-gateway.com/attribution-held-until` on the pool Deployment's own metadata, so recording it rolls nothing.
  Its client certificate stays in the bundle, so a departure leaves the pod template unchanged; the pool rolls when the hold ends.
  The consumer keeps its egress to the range while any pool it reaches lists it, and re-reads at the release.
  A withdrawn grant is not held: sharing ingress already refuses that namespace on every port.
- **The rollout gate closes the port-reuse window.** Lowest-free allocation can give a joining namespace a port another just left, while replicas that have not rolled still map that port to the old namespace.
  The joiner is projected only once every replica maps the port to it, and the leaver's workers are refused on it by ingress from the same reconcile, so no failure lands in the wrong partition.
  A held port is not reallocated until it is released, which keeps the window closed while the leaver still listens on it.

## Phase 3: the AGC reads its records

A new package, `cmd/agc/internal/proxyattribution`, run as a manager runnable on every AGC replica, since each replica's sessions own their own pods.
Nothing calls its lookup until classified retry ([Q1183](../queue/Q1183.md)) lands, so this phase's observable output is a counter.

- **It reads only while its own gateway is opted in.** Each poll re-reads the gateway, so turning `proxyFailureAttribution` off stops the reads and drops what is held, with no restart.
- **Which pools: derived, not listed.** The AGC's Role can `get` ConfigMaps but not `list` them, so it names each projection from the pools its own RunnerSets resolve to (`proxyRef`, else the gateway's `defaultProxyRef`), the same resolution the worker's port comes from.
  A pool with no projection in the namespace is skipped.
- **Every replica, through the headless Service.** It resolves the projection's `read-host` to the replica addresses and reads each one, verifying the server certificate against the projected CA with the projected `server-name`, and presenting the projected client certificate.
- **Epoch and cursor per replica address.** A changed epoch means the replica restarted and its buffer is new, so the cursor resets and the replica is read from the start.
- **A failed read is a gap, not an error.** The CA rewrite on a namespace-set change reaches the projected Secret before the pool rolls, so a handshake can fail until it does.
  A failed read, a failed lookup of the projection or of DNS, and a replica briefly missing from DNS all keep its cursor, so the next good read neither repeats nor skips a record.
  A cursor goes only after six minutes without a good read, past the proxy's five-minute retention, when reading that replica from the start can return nothing already held.
  The proxy keeps records for five minutes and the AGC polls every 30 seconds, so a poll or two missed loses nothing.
- **The join is on the pod's address window, from the pod informer.** A window opens at the pod's `PodReadyToStartContainers` condition (the sandbox, and so the address, exists), falling back to `status.startTime`.
  It closes when the pod gives the address up: a terminal phase, the condition turning false, or the AGC observing the delete, whichever comes first, since a terminal pod's address can be reused before its object is deleted (KEP-3085's description of the condition, not measured on a cluster here).
  Every pod in the namespace gets a window, not only this gateway's: a sibling gateway's pods share the namespace's partition, and one holding an address while an own pod's window is open must make that record ambiguous rather than land it on the own pod.
  A record is attributed only when exactly one window holds it and that window is an own pod's; one within two seconds of either edge is discarded rather than guessed at.
  A closed window is kept for the proxy's five-minute retention so a record read after its pod is gone still joins.
- **What is kept.** Attributed records by pod UID, for ten minutes, at most 64 per pod: the outcome, the dial class, the host and port, and the time.
  `Lookup(uid)` returns them; the UID rather than the name, because a repeat delivery of a job reuses its pod's name.
- **The counter.** `actions_gateway_proxy_failure_records_total{namespace, outcome, result}` counts each record read as `attributed` or `discarded`, and `actions_gateway_proxy_failure_reads_total{namespace, result}` each replica read as `ok` or `failed`, which is how an operator sees the CA-roll gap.

## Phase 4: the enforcement measurement

The row makes a ship requirement of measuring, on the Calico lane, that a worker is refused on another consumer's port: the proxy learns the consumer from the port alone, so that refusal is the whole of what stops one tenant's failures landing in another's buffer.
One new e2e file, `cmd/gmc/test/e2e/v2_proxy_attribution_test.go`, holding a pool in a provider namespace that attributes for two consumer namespaces, each with an opted-in gateway.

- **The refusal, Calico only.** One workload-labelled pod in the first consumer namespace waits until its own port answers (the proxy refuses a CONNECT to a host off its allowlist with a 403), then dials the second consumer's port and must get no answer at all.
  Gating in the same pod is what makes the negative non-vacuous, as for the other enforcement specs: a separate control pod proves only its own endpoint was programmed.
  It skips on kindnet with the others, since that enforcer does not drop NetworkPolicy negatives.
- **A denial attributed end to end, both lanes.** A pod carrying a RunnerSet's label, so the AGC counts it as its own worker, waits past the reader's two-second edge margin, CONNECTs to a denied host through its namespace's port, and waits again before exiting so the record does not sit on the close edge either.
  The gateway's `actions_gateway_proxy_failure_records_total{outcome="denied",result="attributed"}` must then rise, which only a read over mTLS, a matched epoch and cursor, and the address-window join together produce.
  Nothing else observes the join until classified retry ([Q1183](../queue/Q1183.md)) calls the lookup.
- **The scrape stays in the cluster.** A pod in the consumer namespace mounts the gateway's metrics client Secret and reads the AGC's metrics endpoint, so no certificate leaves the cluster; the namespace carries the `metrics: enabled` label the AGC's ingress rule admits scrapers from.
