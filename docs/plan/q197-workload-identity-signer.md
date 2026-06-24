# Q197 — Workload-identity credentials (external signer)

**Goal:** add the second `spec.credentials` union member — `workloadIdentity` —
so a gateway can authenticate to GitHub by having an **external** signer sign the
App JWT, with **no GitHub App private key ever in the cluster**.

**Approach:** (1) extend the Q196 discriminated union with a `WorkloadIdentity`
member + CEL `iff`; (2) introduce a `githubapp.Signer` interface so JWT signing is
pluggable; (3) ship the MVP external signer — HashiCorp Vault transit (sign op) +
Vault Kubernetes auth — behind that interface so cloud KMS slots in later without
another breaking change.

This builds directly on Q196 (merged): the discriminated `spec.credentials` union
already exists with `githubApp` as the first member. We **do not reshape the
parent** — we add a second member per the existing `+unionDiscriminator`.

## Why now (the v2beta1 freeze)

`alpha → beta` is the last free breaking change. Q196 fixed the union *shape*;
Q197 validates it against a *real* second consumer (the signer interface + a Vault
impl), so the beta shape ships with both auth methods designed-and-built, not
designed-and-unbuilt. See [appendix-h §H.15](../design/appendix-h-v2-api-decomposition.md#h15-other-breaking-changes-worth-batching)
and [v2beta1.md](v2beta1.md).

## Security invariants (non-negotiable)

- **Secure-by-default unchanged.** In-cluster PEM (`githubApp`) stays the default;
  the external signer is an explicit opt-in union member. No existing validation or
  default is relaxed. See [05-security.md](../design/05-security.md) and
  [[feedback_secure_by_default]].
- **No private key in the cluster, ever.** The whole point: the App JWT is signed
  by an external trust anchor (Vault transit) that the AGC reaches by proving its
  pod identity (Vault Kubernetes auth). The AGC never reads, holds, or mounts the
  App private key under this method.
- **No secret in logs/env/process args.** The pod's projected ServiceAccount
  token (the Vault login credential) and the Vault client token are read from
  files / response bodies and never logged, never placed in an env var, never
  interpolated into an error. The HTTPS-for-credential-channel rule that guards
  the GitHub token exchange applies equally to the Vault address (HTTPS by
  default; plaintext only under an explicit dev/test opt-in).

## Scope of THIS PR (and the deferred follow-up)

**In this PR (the API shape + the signer machinery + tests/docs):**

1. **API** (`api/v2alpha1`): `CredentialTypeWorkloadIdentity` enum value;
   `WorkloadIdentity` member on `GitHubCredentials`; `ExternalSigner` (provider
   discriminator) + `VaultSigner` + `VaultKubernetesAuth` types; a per-member CEL
   `iff` rule that *extends* the union (never an N-way "exactly one of"); a
   per-provider CEL `iff` on `ExternalSigner`. Regenerate deepcopy + CRDs + sync
   Helm chart CRDs.
2. **`githubapp.Signer` interface**: pluggable JWT signing. `pemSigner` wraps the
   existing in-cluster crypto path (RS256/EdDSA) with **zero behavior change**.
   `NewInstallationTokenProviderWithSigner` builds a provider around any `Signer`.
3. **`githubapp/vaultsigner`**: the MVP external signer. Vault Kubernetes auth
   login (projected SA token → Vault client token, cached to its lease) + Vault
   transit `sign` (RSA pkcs1v15 + sha2-256 = RS256). HTTPS-by-default with an
   explicit dev/test opt-in. Unit-tested against an `httptest` Vault mock — no
   real Vault, no real secret.
4. **GMC v2 controller guard**: a `workloadIdentity` gateway must not error-loop
   on the absent GitHub App Secret. The controller branches on
   `credentials.type`: for `GitHubApp` the existing Secret check runs; for
   `WorkloadIdentity` it skips the Secret check and fails closed with an honest
   `CredentialUnavailable`/`WorkloadIdentityProvisioningPending` condition (the
   runtime AGC provisioning lands in the follow-up below).
5. **envtest** (`v2alpha1_crd_test.go`): the union accepts a well-formed
   `workloadIdentity`, and rejects type/member mismatches and a `vault`-less
   `Vault` provider.
6. **Docs**: appendix-h §H.15, 05-security.md (new threat-model row + the no-PEM
   delegation model), v2beta1.md (shipped shape), and operator docs
   (tenant-onboarding: how to configure workload identity).

**Deferred to a follow-up (Q201), the kind-e2e tier per [v2beta1.md](v2beta1.md#testing):**
full GMC provisioning of a `workloadIdentity` AGC Deployment (stamp the signer
env, project the SA token volume, bind the AGC ServiceAccount to its Vault role),
the AGC `main.go` consumption branch (build the vault-signer provider), and the
in-cluster Vault + transit + k8s-auth kind e2e that exercises the live no-PEM
round-trip. That tier needs a kind cluster; the signer interface + Vault impl it
will consume are fully built and unit-tested here.

## Signer design (cloud-KMS-ready)

```go
// githubapp.Signer signs the GitHub App JWT. Two trust models implement it:
//   - pemSigner: in-cluster, holds the App private key (the possession model).
//   - vaultsigner.Signer: external, delegates signing to Vault transit (no key
//     in cluster — the delegation model). Cloud KMS signers add more impls.
type Signer interface {
    JWTAlg() string                                       // "RS256" | "EdDSA"
    Sign(ctx context.Context, signingInput []byte) ([]byte, error) // raw signature
}
```

The provider builds the JWS `header.payload` via golang-jwt's `SigningString()`,
hands it to `Signer.Sign`, and assembles `header.payload.base64url(sig)`. This
keeps JWT assembly in one place and lets each signer own only the cryptographic
sign. Adding GCP/AWS/Azure KMS = a new `Signer` impl + a new `ExternalSigner`
provider enum value + member — additive, no breaking change.

## Test plan

- `make check` (gofmt/lint/shellcheck/unit) green.
- `githubapp`: unit tests for `pemSigner` (RS256 + EdDSA, signature verifies),
  the signer-backed provider (mints a token end-to-end against an httptest GitHub
  mock using a fake signer), and `vaultsigner` (login + sign happy path; login
  cache reuse + re-login on expiry; HTTPS enforcement + opt-in; Vault error
  bodies surfaced without leaking the SA/client token; signature decodes to a
  valid RS256 signature verifiable with the public key).
- gmc envtest (`v2alpha1_crd_test.go`): workload-identity union accept/reject
  cases under real-apiserver CEL.

## Status

- [x] API types + CEL (`WorkloadIdentity`/`ExternalSigner`/`VaultSigner`/`VaultKubernetesAuth`; per-member + per-provider iff)
- [x] Regenerate deepcopy/CRD + chart sync
- [x] `githubapp.Signer` + pemSigner refactor + `NewInstallationTokenProviderWithSigner`
- [x] `githubapp/vaultsigner` (Vault transit + k8s auth, HTTPS-by-default, secret-safe) + unit tests (mock Vault, RS256 verifies, login cache/re-login, error paths, no token leak)
- [x] GMC controller guard (workload-identity fails closed, `WorkloadIdentityProvisioningPending`)
- [x] envtest validation cases (union accept/reject + defaulting; reconciler fail-closed)
- [x] docs (appendix-h §H.15, 05-security §5.7, v2beta1, tenant-onboarding)
- [x] follow-up Queue row (Q201)
- [x] `make check` green; PR opened
