# Milestone 1 — Remaining Test Gaps

← [Milestone 1](milestone-1.md)

---

## Overview

**Goal:** Fill the test gaps identified in a post-implementation coverage review of the Milestone 1 packages (`broker`, `githubapp`). Overall coverage sits at 72.6%; the gaps below are the meaningful ones — code paths that carry real production risk, not just syntactic line misses.

**Status:** All gaps implemented. Coverage improved from 72.6% → 80.3% (broker: 81.0%, githubapp: 79.4%).

All tests are unit tests using `httptest` stubs and in-memory fixtures. No network access or credentials required.

---

## Gaps

### Gap 1 — `DecryptSessionKey` completely untested ✓

**Package:** `broker`
**Function:** `DecryptSessionKey` in `crypto.go`
**Coverage:** 0% → 100%

`DecryptSessionKey` unwraps the RSA-OAEP (SHA-1) encrypted AES session key returned in the `CreateSession` response. It is called by every listener before any message can be decrypted, yet has no test at all. A mismatched hash function or wrong key format would produce a silent failure only discovered during a live probe run.

**Tests to add in `crypto_test.go`:**

| Test | What it verifies |
|---|---|
| `TestDecryptSessionKey_RoundTrip` | Generate a fresh 2048-bit RSA key pair; RSA-OAEP (SHA-1) encrypt a synthetic 32-byte AES key with the public key; call `DecryptSessionKey` with the private key; assert the result equals the original 32 bytes. |
| `TestDecryptSessionKey_WrongKey` | Encrypt with key A; decrypt with key B; assert a non-nil error is returned. |

**Implementation notes:**
- Use `rsa.GenerateKey` and `rsa.EncryptOAEP(sha1.New(), ...)` directly in the test to produce the ciphertext — this keeps the test self-contained without touching the crypto fixture.
- The test proves the SHA-1 OAEP pairing is correct, which is the only non-obvious constraint here (SHA-256 OAEP would silently fail against real GitHub sessions).

---

### Gap 2 — v2 flow paths in `CreateSession`, `GetMessage`, `DeleteSession` ✓

**Package:** `broker`
**Functions:** `CreateSession`, `GetMessage`, `DeleteSession` in `client.go`
**Coverage impact:** `GetMessage` 57% → 85.7%, `DeleteSession` 71% → 78.6%

All three methods branch on `UseV2Flow`. The v1 VSTS pool paths are well-covered; the v2 paths (`{BrokerURL}/session`, `{BrokerURL}/message?sessionId=...&status=online&runnerVersion=...`, `{BrokerURL}/session` DELETE) are completely untested despite being the code path used by modern runner registrations.

**Tests to add in `client_test.go`:**

| Test | What it verifies |
|---|---|
| `TestCreateSession_V2Flow_URL` | With `UseV2Flow: true`, assert the request goes to `{BrokerURL}/session` (no pool path, no `api-version` query param). |
| `TestGetMessage_V2Flow_URL` | With `UseV2Flow: true` and `RunnerVersion`, `RunnerOS`, `RunnerArch` set, assert the URL is `{BrokerURL}/message` with `sessionId`, `status=online`, `runnerVersion`, `os`, `architecture`, and `disableUpdate=false` query params. |
| `TestGetMessage_V2Flow_NoOptionalParams` | With `UseV2Flow: true` and only `RunnerVersion` empty, assert `runnerVersion` is absent from the URL (not `runnerVersion=`). |
| `TestDeleteSession_V2Flow_URL` | With `UseV2Flow: true`, assert the request goes to `{BrokerURL}/session` (ignoring the `sessionID` argument). |

**Implementation notes:**
- Add a `newV2TestClient` helper (mirrors `newTestClient` but sets `UseV2Flow: true`, `RunnerVersion`, `RunnerOS`, `RunnerArch`) to reduce repetition across these four tests.
- Use `r.URL.RawQuery` parsing in the handler to assert individual query params without brittle full-string matching.

---

### Gap 3 — `RenewJobLoop` error propagation ✓

**Package:** `broker`
**Function:** `RenewJobLoop` in `client.go`
**Coverage:** `errCh <- err` path now exercised by `TestRenewJobLoop_ErrorPropagated`

When `RenewJob` returns an error, `RenewJobLoop` must send that error to the returned channel and then exit. The current test (`TestRenewJob_Interval`) only exercises the happy path; the error branch is untested. A regression here would cause the AGC's renewal goroutine to silently swallow job-expiry errors.

**Test to add in `client_test.go`:**

| Test | What it verifies |
|---|---|
| `TestRenewJobLoop_ErrorPropagated` | Stub `/renewjob` returns 500. Send one tick to the `tickCh`. Assert the error channel receives a non-nil error. Assert the goroutine exits (drain `errCh`; goleak confirms no leak). |

**Implementation notes:**
- Use the manually-driven `tickCh` pattern from `TestRenewJob_Interval` — send one tick, then read from `errCh` with a short timeout.
- Assert `err.Error()` contains `"500"` to verify it is the RenewJob error, not a context error.

---

### Gap 4 — `parseRSAPrivateKey` PKCS#8 path ✓

**Package:** `githubapp`
**Function:** `parseRSAPrivateKey` in `auth.go`
**Coverage:** 38.5% → 92.3%

GitHub App private keys are distributed as PKCS#1 PEM files today, but the PKCS#8 path (`PRIVATE KEY`) was added for forward-compatibility. It has two sub-branches: RSA key (success) and non-RSA key (error). Neither is tested.

**Tests to add in `auth_test.go`:**

| Test | What it verifies |
|---|---|
| `TestToken_PKCS8Key` | Generate an RSA key; encode it as PKCS#8 PEM (`PRIVATE KEY` block type via `x509.MarshalPKCS8PrivateKey`); pass it to `NewInstallationTokenProvider`; assert no error and that `Token()` succeeds against a stub server. |
| `TestToken_PKCS8NonRSAKey` | Generate an ECDSA key; encode it as PKCS#8 PEM; pass it to `NewInstallationTokenProvider`; assert the error message contains `"not RSA"`. |
| `TestToken_UnsupportedPEMType` | Pass a PEM block with `Type = "CERTIFICATE"`; assert error contains `"unsupported PEM block type"`. |

**Implementation notes:**
- `x509.MarshalPKCS8PrivateKey` accepts `*rsa.PrivateKey` directly for the happy-path test.
- For the non-RSA test, `x509.MarshalPKCS8PrivateKey` with an `*ecdsa.PrivateKey` produces a valid PKCS#8 block with a non-RSA key.
- `TestToken_UnsupportedPEMType` requires no RSA key — construct the PEM block manually with `pem.EncodeToMemory`.

---

### Gap 5 — Minor gaps ✓

These are lower-priority; each is a one-test fix.

#### 5a — `CreateSession` non-400 error status ✓

**Code path:** `CreateSession` → `resp.StatusCode` is not 200, 201, or 400 (e.g. 503)
**Coverage:** the `fmt.Errorf("unexpected status %d")` branch is untested.

| Test | What it verifies |
|---|---|
| `TestCreateSession_UnexpectedStatus` | Stub returns 503. Assert error contains `"503"`. |

#### 5b — `AcquireJob` non-200 error status ✓

**Code path:** `AcquireJob` → `resp.StatusCode != 200` → error return
**Coverage:** untested → covered.

| Test | What it verifies |
|---|---|
| `TestAcquireJob_NonOKStatus` | Stub returns 409 with a body. Assert error contains `"409"` and the body text. |

#### 5c — `ParseRunnerRSAKey` with BOM-prefixed file ✓

**Code path:** `ParseRunnerRSAKey` → `stripBOM` removes the `.NET` UTF-8 BOM before JSON parsing.
**Coverage:** `stripBOM` is tested in `TestParseRunnerCredentials_DOTNETBOM` but not for the RSA params file.

| Test | What it verifies |
|---|---|
| `TestParseRunnerRSAKey_BOM` | Write a `.credentials_rsaparams` file with a `\xEF\xBB\xBF` prefix; assert `ParseRunnerRSAKey` succeeds. |

#### 5d — `FetchRunnerOAuthToken` missing `access_token` field ✓

**Code path:** server returns 200 with a JSON body that has an empty or missing `access_token` → error return.
**Coverage:** untested → covered.

| Test | What it verifies |
|---|---|
| `TestFetchRunnerOAuthToken_MissingAccessToken` | Stub returns 200 with `{"token_type":"Bearer"}` (no `access_token`). Assert error contains `"missing access_token"`. |

---

## Priority order

| Priority | Gap | Reason | Status |
|---|---|---|---|
| 1 | Gap 1 (`DecryptSessionKey`) | 0% coverage on crypto correctness; SHA-1 vs SHA-256 OAEP mismatch is silent | ✓ Done |
| 2 | Gap 2 (v2 flow) | Real production code path for modern runner registrations; 0% coverage | ✓ Done |
| 3 | Gap 3 (`RenewJobLoop` error) | Silent loss of job-expiry errors would cause the AGC to hold a dead job | ✓ Done |
| 4 | Gap 4 (PKCS#8) | Forward-compatibility path; low risk today but untested entirely | ✓ Done |
| 5 | Gap 5 (minor) | One test each; fill remaining line gaps | ✓ Done |
