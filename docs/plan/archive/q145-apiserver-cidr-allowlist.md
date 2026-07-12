# Q145 — Optional apiserver-CIDR allowlist for AGC egress

**Goal:** give operators an opt-in way to scope the AGC NetworkPolicy's
443/6443 (Kubernetes API server) egress rule to a known apiserver CIDR set,
closing the deliberately-accepted residual documented in 05-security §5.2
(Q127 item 7). Leaving it unset preserves today's any-destination behavior
byte-for-byte.

**Why a GMC flag, not a per-tenant CR field:** the apiserver endpoint is a
*cluster-wide* property — identical for every tenant in the cluster — and
tightening it is a platform/operator decision, not a tenant one. This mirrors
the existing `allowed-priority-classes` platform allowlist exactly: chart value
→ GMC flag → controller → NetworkPolicy. The post-DNAT-unpredictable trap (PR
#59) means we cannot pick a portable default, so the safe default stays
any-dest and the operator opts in only when their platform exposes a stable
apiserver CIDR.

## Secure-by-default contract

- **UNSET / empty** → render exactly today's rule: one egress rule with ports
  443 + 6443 and **no `To`** (any-destination). No behavior change. Clusters
  with unpredictable post-DNAT apiserver IPs keep working.
- **SET** → same ports, but `To: [ipBlock per CIDR]`. Strictly tighter. This is
  an opt-in *tightening*, never a loosening.

## Changes

1. `cmd/gmc/cmd/main.go` — add `--apiserver-cidrs` (comma-separated) flag;
   parse + validate each entry with `net.ParseCIDR` (fail-fast at startup on a
   malformed entry — a bad CIDR would otherwise yield an apiserver-rejected NP
   or silently mis-scope). Thread into the reconciler as `APIServerCIDRs`.
2. `cmd/gmc/internal/controller/actionsgateway_controller.go` — add
   `APIServerCIDRs []string` field; pass it to `buildAGCNetworkPolicy`.
3. `cmd/gmc/internal/controller/builder.go` — `buildAGCNetworkPolicy(ag,
   apiServerCIDRs)`: when non-empty, attach `ipBlock` peers to the 443/6443
   rule; when empty, unchanged. Update godoc.
4. Tests — update call sites; add a builder test asserting (a) unset → no `To`,
   (b) set → ports unchanged + one `ipBlock` peer per CIDR.
5. Chart — `values.yaml` `apiServerCIDRs: []` (secure-by-default comment);
   `deployment.yaml` `{{- with .Values.apiServerCIDRs }} --apiserver-cidrs=...`;
   `values.schema.json` array-of-CIDR-strings shape validation.
6. Docs — 05-security §5.2 residual row now operator-closable; plan item 7
   note; `docs/operations/security-operations.md` operator how-to.
7. `docs/STATUS.md` — remove Q145 row (own commit).

## Validation

`make check` (incl. helm lint/template) green; render the AGC NetworkPolicy
both with and without `apiServerCIDRs` set and confirm any-dest vs ipBlock.
