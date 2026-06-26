# Q201 — Workload-identity AGC provisioning + Vault kind e2e

**Goal:** complete the no-PEM delegation path (Q197) end to end so a
`credentials.type: WorkloadIdentity` gateway actually provisions and authenticates
to GitHub, instead of being admitted-but-failed-closed.

**Approach:** (1) GMC stamps the signer config env + a kubelet-projected
ServiceAccount-token volume on the workload-identity AGC Deployment (no GitHub App
Secret mount); (2) AGC `main.go` branches on the credential type and builds the
`githubapp/vaultsigner`-backed token provider (the Q197 interface, wired end to
end); (3) an in-cluster test-Vault kind e2e exercises the live no-PEM round-trip.

This is the deferred follow-up named in Q197 (PR #383) and in
[05-security.md §5.7](../design/05-security.md#57-workload-identity-the-no-pem-delegation-model).
The API shape, the `githubapp.Signer` interface, the `pemSigner` refactor, and the
`vaultsigner` implementation already shipped and are unit-tested in Q197 — this PR
consumes them.

## Security invariants (non-negotiable)

- **No App private key in the cluster, ever.** The workload-identity AGC mounts no
  GitHub App Secret. Its only credential is the kubelet-projected ServiceAccount
  token (audience-scoped to Vault, minted by the kubelet, not stored in a Secret),
  read fresh from disk at each Vault login.
- **No secret in env/logs/process args.** `appId`/`installationId`/Vault address +
  mounts/role are non-secret *configuration* and travel as env (mirroring the
  existing AGC env contract). The projected token and the Vault client token are
  file/response-body only — never env, never logged.
- **Secure-by-default unchanged.** `githubApp` (possession model) stays the
  default. The Vault address is HTTPS-by-default in the AGC; a plaintext address is
  permitted only under the existing dev/test signal (`STUB_AUTH_URL` set), exactly
  as the GitHub token-exchange base URL already is. Production GMCs never set it.

## The credential-wiring contract (GMC → AGC)

The GMC threads the credential method to the AGC via env (config, never secrets):

| Env | Source | Notes |
|---|---|---|
| `CREDENTIAL_TYPE` | `credentials.type` | `WorkloadIdentity` selects the no-PEM path; empty/`GitHubApp` ⇒ the existing PEM path |
| `GITHUB_APP_ID` | `workloadIdentity.appId` | non-secret App identity |
| `GITHUB_INSTALLATION_ID` | `workloadIdentity.installationId` | non-secret |
| `VAULT_ADDR` | `signer.vault.address` | HTTPS in prod |
| `VAULT_TRANSIT_MOUNT` | `signer.vault.transitMount` | optional (default `transit`) |
| `VAULT_TRANSIT_KEY` | `signer.vault.keyName` | the RSA transit key |
| `VAULT_AUTH_MOUNT` | `signer.vault.auth.mount` | optional (default `kubernetes`) |
| `VAULT_AUTH_ROLE` | `signer.vault.auth.role` | the Vault k8s-auth role |
| `VAULT_SA_TOKEN_PATH` | GMC mount path | path of the projected token file |

The GMC stamps a **projected ServiceAccount token volume** on the workload-identity
AGC pod: audience `vault`, short expiry (kubelet auto-rotates), mounted read-only at
`/var/run/secrets/actions-gateway/vault-token/token`. The AGC's existing
ServiceAccount (`<ag>-agc`) is the in-cluster identity; the operator binds it to a
Vault Kubernetes-auth role out of band (the "Vault role binding"). The audience is a
fixed `vault` for the MVP — see Future work.

The AGC's GitHub App credential Secret mount is **omitted entirely** for workload
identity (no Secret exists), and the `actions-gateway/github-app-secret` rollout
annotation is dropped.

## Scope of THIS PR

1. **GMC provisioning** (`builder.go`, `actionsgateway_v2_builder.go`,
   `actionsgateway_v2_controller.go`): branch the AGC Deployment build on credential
   type; stamp the signer env + projected token volume; remove the
   `WorkloadIdentityProvisioningPending` fail-closed branch so workload-identity
   gateways provision and reach Ready.
2. **AGC consumption** (`cmd/agc/main.go`): branch on `CREDENTIAL_TYPE`; build the
   `vaultsigner.Signer` + `NewInstallationTokenProviderWithSigner`. Extract a
   testable `buildTokenProvider` so the env→provider wiring is unit-covered without
   a live Vault.
3. **Tests:**
   - GMC unit (`actionsgateway_v2_test.go`): workload-identity AGC Deployment has
     the signer env + projected token volume, mounts no GitHub App Secret, drops the
     rollout annotation.
   - GMC envtest (`integration/v2_actionsgateway_test.go`): a workload-identity
     gateway with **no** App Secret provisions and goes Ready (no
     `CredentialUnavailable`).
   - AGC unit (`cmd/agc`): `buildTokenProvider` builds a Vault-backed provider from
     the workload-identity env and rejects a plaintext Vault address without the
     dev/test opt-in. The vaultsigner-against-mock-transit coverage already lives in
     `githubapp/vaultsigner` (Q197); a provider-level no-PEM-chain test confirms
     sign→assemble→token end to end against an httptest Vault + GitHub.
   - **kind e2e** (`cmd/gmc/test/e2e/vault_workload_identity_test.go`): deploy an
     in-cluster **test** Vault (dev mode, root token, transit + RSA key + k8s auth +
     a role bound to the AGC SA), create a `WorkloadIdentity` gateway, assert it
     reaches Ready and runs a job against fakegithub — the live no-PEM round-trip.
     Runs in CI's e2e gate (kindnet); no local kind run in this session (repo policy).
4. **Docs:** 05-security §5.7 (flip the "lands in a follow-up (Q201)" note to
   implemented), tenant-onboarding (operator Vault setup + the CNI egress caveat),
   this plan, STATUS (remove Q201 row, isolated commit).

## Test Vault (e2e, no real credentials)

The e2e stands up Vault in **dev mode** in `e2e-infra` (root token `root`, in-memory,
HTTP listener). Configuration via `kubectl exec … vault …`:

- `vault secrets enable transit` + `vault write -f transit/keys/agc type=rsa-2048`
- `vault auth enable kubernetes` + `vault write auth/kubernetes/config
  kubernetes_host=https://kubernetes.default.svc` (Vault auto-detects its in-cluster
  reviewer token + CA; its SA is bound to `system:auth-delegator`)
- a policy granting `update` on `transit/sign/agc`, and a role
  (`bound_service_account_names=<ag>-agc`, `bound_service_account_namespaces=<ns>`,
  `audience=vault`)

fakegithub accepts any App JWT (it never verifies the signature), so the e2e proves
the *path* — the AGC has no PEM yet still mints a token — not GitHub's crypto check.
The Vault transit key is dev-only and never leaves the test cluster. The AGC reaches
the dev Vault over HTTP, allowed by the dev/test `STUB_AUTH_URL` signal the e2e
already sets.

## Known limitation / follow-up

**AGC→Vault NetworkPolicy egress.** The per-tenant AGC NetworkPolicy default-denies
egress except DNS + GitHub CIDRs + kube API (+ proxy). Vault is a new destination the
API cannot express as a NetworkPolicy peer (Vault's address is an opaque URL, not a
selectable namespace/pod or a managed CIDR). On the e2e's kindnet CNI egress is not
enforced, so the round-trip validates; on a policy-enforcing CNI (Calico, the
production recommendation) the operator must add an egress allowance for their Vault
endpoint. Flagged as a Queue follow-up for first-class Vault egress (a `vaultPeer`
selector or CIDR on the AGC policy) and documented in tenant-onboarding.

## Status

- [x] GMC: branch AGC Deployment on credential type (signer env + projected token, no Secret mount)
- [x] GMC: remove the fail-closed WorkloadIdentity branch; provision + go Ready
- [x] AGC: `buildTokenProvider` branch (vaultsigner provider) wired in `main.go`
- [x] GMC unit + envtest; AGC unit (provider build); no-PEM chain covered by Q197 vaultsigner tests
- [x] kind e2e (in-cluster test Vault, no-PEM round-trip) + CI image mirror
- [x] docs (05-security §5.7, tenant-onboarding, this plan); Queue follow-up for Vault egress
- [x] `make check` green; e2e gate must RUN in CI (verify after PR opens)
</content>
</invoke>
