# v2beta1 graduation plan

**Design source of truth:** [Appendix H — v2 API Decomposition](../design/appendix-h-v2-api-decomposition.md)
(the CRD set and resolved decisions) and [v2-api.md](v2-api.md) (the `v2alpha1`
build). This doc holds the *graduation*: what must land before cutting
`v2alpha1 → v2beta1`, in what order, and the design decisions that gate it.

**Goal.** Graduate the v2 API from `v2alpha1` to `v2beta1` — the first stability
contract (*won't be removed; changes carry a migration path; production-relyable*)
— with the credential shape correct before the freeze.

**Approach.** `alpha → beta` is the **last free breaking change**: alpha carries
no stability promise, and once beta is signed the conversion webhook must
round-trip served versions for every later change. So the gate is narrow — get
the shape right, then cut. Five blockers land first (a broker-compatibility
sweep, the credentials discriminated-union reshape, the workload-identity
feature that validates that union, gVisor worker-isolation validation, and
worker-pod disruption-safety defaults), then the graduation itself.

Two of the five are *not* shape blockers but **production-reliability** blockers:
beta's contract is "production-relyable," so the worker posture is confirmed
before signing — gVisor isolation holds (Q15) **and** routine cluster churn (node
consolidation, descheduler) cannot strand a running job (Q218). Neither is a CRD
shape change — both are cheap, additive runtime behavior — but both gate the beta
*quality* bar exactly as the gVisor check does.

## Why graduate now

The decision and its rationale live in
[v2-api.md § API maturity & graduation](v2-api.md#api-maturity--graduation-v2alpha1--v2beta1--v2).
In brief: the architecture review found the breaking surface fully covered;
everything else outstanding is additive. We do **not** gate on external adoption —
`v1alpha1` never carried a stability contract either, and a `v3` re-cut remains
the escape hatch if the shape proves wrong. Beta's "production-relyable" signal is
itself an adoption driver (nobody relies on an alpha that may vanish without
notice).

## The blocker sequence

Ordered in the [Queue](../STATUS.md). **Q191/Q196/Q197/Q15/Q218 are independent and
run in parallel; Q74 waits for all five.**

### 1. Q191 — Broker-compatibility sweep *(run first)*

GAG re-implements the GitHub broker protocol, so a protocol gap could force a CRD
change. Run this *before* freezing the beta shape: expand `cmd/probe` into a
compatibility suite that exercises the full broker surface and publish a compat
report. If it surfaces a needed field, that field lands in the beta shape; if
it's clean, we freeze with confidence. Turns the silent-break risk into a
managed, visible asset.

**Done.** The `cmd/probe/compat` suite asserts every documented broker contract
(§3.2 credential crypto, §3.3 endpoints, §3.4 payload shapes, §3.5 rate-limit
handling) against the in-process broker model; it runs credential-free in
`make check`/CI on every change, with the live-against-real-GitHub probe
(`cmd/probe`) as its companion. The published result is
[broker-compatibility.md](../development/broker-compatibility.md): all documented
contracts pass, so no protocol gap forces a field into the beta shape.

### 2. Q196 — Credentials discriminated-union shape *(shipped in `v2alpha1`)*

Nest the credential reference under a parent with an **explicit discriminator**
(see [Design decisions](#design-decisions)). This is the one genuinely
shape-breaking change the last-free-break exists for. It ships **in `v2alpha1`**:
alpha carries no stability contract, so the reshape is free now, and the beta cut
then inherits the correct shape — the conversion webhook (Q74) round-trips the
credentials block as an identity rather than reshaping it.

### 3. Q197 — Workload-identity credentials *(the second union member)* — **shipped**

Built the `workloadIdentity` member **before the cut**, so both auth methods ship
in the first beta shape and the union is validated against a real second consumer
— not a designed-but-unbuilt one. Shipped: the `WorkloadIdentity` union member +
CEL `iff`; a `githubapp.Signer` interface; the MVP Vault transit signer + Vault
Kubernetes auth (`githubapp/vaultsigner`, unit-tested against a mock Vault); the
no-PEM provider path (`NewInstallationTokenProviderWithSigner`). Cloud KMS
providers (AWS/GCP/Azure) are additive follow-ups behind the same signer
interface. The GMC runtime provisioning of a workload-identity AGC + the
in-cluster Vault kind e2e land in Q201 (the kind-e2e tier below); until then a
`WorkloadIdentity` gateway is admitted but fails closed.

### 4. Q15 — gVisor RuntimeClass validation

Beta is the *production-relyable* contract, so the worker-isolation posture
should be confirmed before signing it: validate that a worker pod with
`RuntimeClass=gvisor` actually runs under `runsc` and that the sandbox holds.
Now **free** to test — `minikube` + the `gvisor` addon (systrap platform, no
nested virtualization) runs locally on a Mac and on a stock CI runner (see
[testing.md](../development/testing.md)). Independent of the API-shape blockers;
runs in parallel.

### 5. Q218 — Worker-pod disruption-safety defaults

Beta is the *production-relyable* contract, so a beta-quality runner platform must
not let routine cluster operations kill a running CI job. By default a Karpenter
consolidation event, a Cluster Autoscaler scale-down, or a Descheduler eviction
will reschedule a worker pod mid-job — stranding the GitHub Actions run. Stamp the
disruption-safety annotations on worker pods — `karpenter.sh/do-not-disrupt`,
`cluster-autoscaler.kubernetes.io/safe-to-evict: "false"`, and a descheduler
exclusion — as a secure-default gap-fill, the same way the AGC already stamps
`runAsNonRoot`/`seccompProfile` when a tenant omits them (a tenant value always
wins). Not a CRD shape change; additive runtime behavior, independent of the
API-shape blockers, runs in parallel. A per-`RunnerSet` toggle, if wanted, is an
additive field that can land post-beta.

**Recommended before the freeze (not a hard gate):** Q205 — apply
`app.kubernetes.io/*` recommended labels across all objects and align metric/span
names to Prometheus/OTel semantic conventions. Adding labels later is non-breaking,
but operators build selectors, dashboards, and alerts against the observable
surface the moment beta says "production-relyable," so the convention is cheapest
to settle now (the same cheap-now/expensive-later logic as M1's field-naming pass).

### 6. Q74 — The graduation cut

After the above: add `Hub`/`Convertible` conversion-webhook stubs, add `v2beta1`
as a served version, mark it the storage version, run the storage migration, then
drop the superseded served version per the
[graduation ladder](v2-api.md#api-maturity--graduation-v2alpha1--v2beta1--v2). Because
Q196 already shipped `spec.credentials` in `v2alpha1`, the conversion webhook maps the
credentials block as an **identity** (`v2alpha1.credentials ↔ v2beta1.credentials`) — no
reshape — and only handles whatever other fields differ between the served versions. It
is the in-place graduation the webhook exists for, distinct from the M5 fan-out tool (a
webhook cannot express a fan-out).

## Design decisions

### Credentials: explicit-discriminator parent

**Decided:** nest `githubAppRef` under `spec.credentials` with an explicit
`+unionDiscriminator` `type` field. This reverses the
[§H.15](../design/appendix-h-v2-api-decomposition.md#h15-other-breaking-changes-worth-batching)
"keep the single field" stance.

```yaml
# v1alpha1 — required, top-level (the shape v2 migrates away from)
spec:
  githubAppRef: { name: acme-github-app }       # Secret: {appId, installationId, privateKey}

# v2alpha1 (shipped, Q196) and v2beta1 (target) — discriminated union
spec:
  credentials:
    type: GitHubApp                             # +unionDiscriminator: GitHubApp | WorkloadIdentity
    githubApp:                                  # set iff type == GitHubApp
      name: acme-github-app                     # name-only Secret ref (possession model)
```

- **Why a common parent.** GitHub App auth and workload-identity auth are
  mutually exclusive with non-overlapping field sets — the textbook discriminated
  union. A `type` discriminator makes "pick one" structural (the schema *is* the
  doc) instead of an invisible CEL "exactly one of"; it does not degrade into an
  N-way CEL rule as methods are added; and it gives the `CredentialUnavailable`
  condition, validation, and rotation semantics one home.
- **Why now.** A flat `workloadIdentityRef` sibling is *mechanically* additive but
  additive *into a permanently worse shape*: once `githubAppRef` is top-level
  under beta it can never move under a parent without a breaking change + storage
  migration. The beta cut is the last moment to pick the parent shape for free.
- **Why explicit, not implicit.** Adding a *required* `type` after beta is
  breaking; matching the k8s convention now makes member #2 a clean enum
  extension.

### Workload identity: a different config, Vault-first

Workload identity is not "a different Secret" — it is a different trust model, so
it needs a different field set:

- **GitHub App = possession model.** Hold the App's RSA private key.
  `{appId, installationId, privateKeyRef → Secret}`. Signing material sits at rest
  in the namespace.
- **Workload identity = delegation model.** Hold no key. The signing material
  lives in an external trust anchor (Vault, cloud KMS/HSM) or is federated via
  OIDC. The pod proves its identity (Vault Kubernetes auth / IRSA / GKE WI /
  Azure WI) and the anchor signs the JWT or releases a short-lived token. **No
  `privateKey` field at all.**

```yaml
spec:
  credentials:
    type: WorkloadIdentity
    workloadIdentity:                           # set iff type == WorkloadIdentity; no PEM anywhere
      appId: 12345                              # non-secret; inline (exact sub-shape settled in impl)
      installationId: 67890
      signer:
        provider: vault                         # vault | gcpkms | awskms | azurekeyvault
        keyRef: transit/keys/github-app
      # the pod's cloud/Vault identity binding is on the GMC-stamped ServiceAccount, not inline
```

`privateKeyRef` is meaningless under workload identity; `signer.{provider,keyRef}`
is meaningless under GitHub App. Rotation (you rotate the Secret vs. the anchor
rotates), RBAC (mount a Secret vs. bind an annotated ServiceAccount), validation
(parse a PEM vs. resolve a key + verify the identity binding), and failure modes
(`SecretNotFound` vs. `SignerUnreachable`) all differ. It is also on-strategy:
workload identity removes the App key from the cluster entirely — the
strict-upgrade direction of the secure-by-default principle.

**MVP = Vault transit.** The first signer implementation is Vault transit +
Kubernetes auth, because it is **kind-validatable** (see Testing), serves the
managed-PKI/Vault operator persona (cf. [Q174](../STATUS.md)), and avoids
three-cloud-SDK sprawl. Cloud KMS providers slot in behind the same signer
interface as additive follow-ups.

## Testing

| Blocker | Tier | How |
|---|---|---|
| Q191 broker-compat | unit + live | `cmd/probe/compat` suite asserts every documented contract in `make check`/CI (credential-free, in-process model); live `cmd/probe` binary confirms against real GitHub. Report: [broker-compatibility.md](../development/broker-compatibility.md) ✅ |
| Q196 credentials shape | envtest | Discriminator enum (required, known value) + per-member CEL `iff` ("the named member is set, others absent") under real-apiserver semantics; migration golden regenerated. Conversion round-trip lands with Q74 (identity for credentials). |
| Q197 workload identity | kind e2e | **Vault in-cluster + transit engine + k8s auth**: the AGC pod's projected SA token → Vault authenticates it → Vault transit signs the App JWT. Exercises the whole no-PEM delegation flow with no cloud. Real IRSA/GKE/Azure identity binding stays manual / cloud-CI |
| Q74 graduation | envtest | Conversion round-trip for all five kinds; storage-migration dry-run |

## Definition of done (v2beta1)

- Q191 compat report published; any field it forced is in the beta shape.
- `spec.credentials` shipped with an explicit discriminator; **both** `githubApp`
  and `workloadIdentity` (Vault MVP) members built and tested.
- `v2beta1` served + storage version; conversion webhook round-trips
  `v2alpha1 ↔ v2beta1`; storage migration run.
- Migration tool golden output regenerated for the new served version.
- gVisor `RuntimeClass` isolation validated (minikube + gvisor addon), local + CI.
- Worker pods carry the disruption-safety defaults (Q218) so node
  consolidation/descheduler cannot strand a running job; verified under envtest +
  the kind e2e.
- §H.15 and the affected appendix/operator docs updated to the shipped shape.

## Out of scope (additive, post-beta)

- Cloud KMS workload-identity providers (AWS/GCP/Azure) — additive impls behind
  the Q197 signer interface.
- PAT or other credential methods — additive union members.
- The real-cluster capacity run ([Q181](../STATUS.md)), AGC HA
  ([Q169](../STATUS.md)), and proxy feature/sharing items (Q19/Q166/Q173/Q174) —
  all additive and independently triggered.
