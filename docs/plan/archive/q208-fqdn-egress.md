# Q208 — CNI-native FQDN egress opt-in (Cilium/Calico DNS policy)

**Goal:** Let operators on a DNS-aware policy CNI express the proxy pool's GitHub
egress allowlist as native FQDN rules (`toFQDNs` / Calico `domains`) instead of the
GMC's 24h GitHub-CIDR reconcile — opt-in, additive, default unchanged.

**Approach:** Mirror the existing `managedNetworkPolicy` opt-out on the v2
`EgressProxy` with a new `egressPolicyMode` enum (`CIDR` default | `CiliumFQDN` |
`CalicoFQDN`). In an FQDN mode the GMC emits a CNI-native policy scoped to the
GitHub FQDNs and drops the CIDR rule from the standard NetworkPolicy; the
IPRangeReconciler skips FQDN-mode proxies. Scope is **EgressProxy only** (the v2
shared-egress surface where `managedNetworkPolicy` lives); v1 ActionsGateway and v2
direct egress are out of scope (noted as future work).

## Security posture (secure-by-default)

- Default stays `CIDR` — works on every CNI, no behavior change.
- FQDN mode is **fail-closed**: the standard `networkingv1` NetworkPolicy still
  selects the proxy pods with `policyTypes: [Egress, Ingress]` and only a DNS
  egress rule + the workload ingress rule — so GitHub egress is *denied* unless the
  CNI-native policy re-allows it. If the CNI cannot enforce the native policy (wrong
  CNI / CRD absent), the apply fails loud (reconcile `Degraded`) and egress stays
  denied. Selecting an FQDN mode can never silently open egress.
- `egressPolicyMode` has no effect when `managedNetworkPolicy: false` (operator owns
  the whole policy).

## Deliverables

1. `api/v2alpha1/egressproxy_types.go` — `EgressPolicyMode` type + `EgressPolicyMode`
   field (enum, default `CIDR`); regenerate deepcopy + CRD.
2. `cmd/gmc/internal/controller/egressproxy_fqdn.go` — GitHub FQDN constant set +
   `buildEgressProxyCiliumNetworkPolicy` / `buildEgressProxyCalicoNetworkPolicy`
   (unstructured) + mode helpers.
3. `egressproxy_builder.go` — `buildEgressProxyNetworkPolicy` drops the CIDR rule in
   FQDN mode.
4. `egressproxy_controller.go` — apply the selected CNI policy, delete the
   other-mode / disabled CNI policies (tolerant of NotFound + NoKindMatch).
5. `ipranges.go` — skip CIDR-patching FQDN-mode EgressProxies.
6. Tests: unit (builders + standard-NP-no-CIDR) and envtest (emission per mode;
   stub Cilium/Calico CRDs installed in the suite so the unstructured apply lands).
7. Docs: `docs/design/05-security.md`, `docs/design/network-architecture.md`,
   `docs/operations/security-operations.md` (new `egressPolicyMode` section with CNI
   prerequisites), CRD reference.

## Testing

`make check` (gofmt + lint + unit) + GMC envtest integration suite.
