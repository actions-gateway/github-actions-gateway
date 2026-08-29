# Q986 — attributing an egress audit record to a tenant and a job

## Goal

Let an operator answer "which host did this job reach", from the proxy's opt-in per-connection audit record, on a shared pool as well as an unshared one.
That is Definition of Done #5 of [secure-multi-tenant-oss-ci.md](secure-multi-tenant-oss-ci.md), which the record does not meet today on either dimension.

## What the record is missing, and why one field is not enough

The record's seven fields name the pool, the destination, and the byte counts.
Nothing on it identifies the caller, which costs two separate things:

- **Tenant.** `namespace` is the downward-API namespace of the pool, so on a pool shared via `spec.sharing.allowedNamespaces` a record cannot say which consumer reached the destination.
- **Job.** No field ties a connection to a job on any pool, so the "which host each job reached" half is unmet even unshared.

Both need a per-connection source identifier, which Q564 declined: the client IP on every record is a per-worker movement log, and the trade had not been weighed.
Adding the IP alone still closes neither gap, and one measurement says why.

**Worker pods are deleted at job completion** ([completion.go](../../cmd/agc/internal/provisioner/completion.go)), so a source IP resolved after the fact against live cluster state resolves to nothing.
The pod that held the address is gone, and the address is back in the CNI's pool.
Whatever turns an IP into a tenant and a job has to be recorded while the pod still exists.

## Approach

Two records, joined by the operator's log pipeline on address plus time.

**The proxy** gains a third `auditLogging` value, `ConnectionsWithSource`, which adds `sourceIP` and `sourcePort` to the record it already writes.
The source address comes from the accepted connection, not from a request header, so a worker cannot forge it — the same property that makes the pool namespace trustworthy.

**The AGC** gains its own `auditLogging` field, defaulting `Off`, whose `WorkerAddresses` value writes one record when a worker pod's address is first observed and one when the pod goes away.
That record carries the consuming tenant's namespace and the job's run ID and repository, both of which the AGC already holds: the annotations are stamped on every worker pod ([payload.go](../../cmd/agc/internal/provisioner/payload.go)), and the shared pod informer already watches every worker pod on both acquisition tiers.

Neither record is a movement log by itself.
The proxy's says where a pool went; the AGC's says which job held an address, and names no destination at all.
The join is what produces attribution, which is why both halves are opted into separately and both default off.

### Why the proxy does not resolve the identity itself

The alternative is a proxy that maps the source address to a pod and writes `runID` directly, one self-contained line and no join.
It was rejected on privilege: `cmd/proxy/go.mod` pulls in no Kubernetes client at all today, and the proxy is the one component untrusted worker code talks to directly.
Giving it cross-namespace pod read, an informer, and a cache is a large increase in what a compromised proxy is worth, to save a join the log pipeline is already doing.

### Why two switches rather than one

An `EgressProxy` is referenced across namespaces, so driving the AGC's record from the proxy's field would make one `EgressProxy` edit roll every AGC deployment that references it.
The AG controller watches no `EgressProxy` today, so that coupling is also new machinery.
The cost of two switches is that an operator can turn on half: source IPs the AGC never bound, which read as unattributable rather than as anything false.
The operator docs lead with turning on both.

## Assumptions this rests on

- **The proxy sees the worker's pod IP.** Traffic from a worker pod to the proxy's ClusterIP is not source-NAT'd on the in-cluster path, so `RemoteAddr` is the pod's own address.
  A CNI that SNATs pod-to-service traffic breaks the join; the operator doc says to check this.
- **The AGC's own control-plane egress shares the pool.** Its connections carry a source address no bind record names, which is how an auditor tells control-plane traffic from a worker's.

## Scope

| Area | Change |
|---|---|
| `api/v2alpha1`, `api/v2beta1` | `EgressProxy.spec.auditLogging` gains `ConnectionsWithSource`; `ActionsGateway.spec.auditLogging` is new, `Off` or `WorkerAddresses` |
| `cmd/proxy` | Parse the new mode; add `sourceIP`/`sourcePort` to the record |
| `cmd/gmc` | Thread `AGC_AUDIT_LOGGING` onto the v2 AGC deployment; the proxy's value already passes through verbatim |
| `cmd/agc` | Emit the bind/release record from the shared pod informer |
| Docs | Record shape and the join in `observability-logging.md`; the trade in `05-security.md`; turning it on in `tenant-onboarding.md`; the DoD row in `secure-multi-tenant-oss-ci.md`; `features.md` and `why-gag.md` |

The deprecated `v1alpha1` `ActionsGateway` gets nothing: it is served until `v2.0.0` removes it, and this is a v2 capability.

## Status

✅ Shipped 2026-08-28.
Both opt-ins landed with their own unit coverage, and the Definition of Done #5 row in [secure-multi-tenant-oss-ci.md](secure-multi-tenant-oss-ci.md) is claimed.
Not yet exercised end-to-end on a cluster: the join's CNI assumption is asserted in the operator docs rather than measured on a dogfood run.
