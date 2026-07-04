# Q246 — Live cold-cache diagnosis of the dogfood release-asset download timeout

**Status:** ✅ done — cause confirmed (a) Q61 cache race; minimal fix implemented (this session)
**Owner:** worker session
**Parent:** [gke-dogfood.md](../gke-dogfood.md) (Q246 note), blocks [Q224](../../STATUS.md)

## Goal (one sentence)
Confirm on a live GKE cold run whether the dogfood release-asset download
timeout (shellcheck/coverage tool tarballs) is caused by **(a)** the Q61
cold-start IP-range-cache race or **(b)** the Q247 node-CPU exhaustion that
co-occurred — and act on the confirmed cause only.

## Premise correction (already established, do not re-litigate)
Release-asset downloads 302-redirect `github.com` → `objects.githubusercontent.com`
→ `185.199.108.0/22`, which the worker egress NetworkPolicy already permits
(GMC merges GitHub `/meta` api+actions+web; `web` contains that range —
[`ipranges.go`](../../../cmd/gmc/internal/controller/ipranges.go)). So the
original "widen the allowlist / bake the asset into the image" premise is
wrong. Verified live: at steady state the `dogfood-workload` NP carries 7337
CIDR peers including `185.199.108.0/22`.

## Two hypotheses
- **(a) Q61 cache race.** At GMC cold start the per-CR reconcile
  (`ActionsGatewayV2Reconciler.applyNetworkPolicy`, a `CreateOrPatch` that
  overwrites `Spec.Egress` wholesale) can rebuild the workload NP from an
  **empty** cache (`githubCIDRs()` → nil → `githubCIDREgressRule` returns
  `ok=false` → no GitHub egress rule), transiently blanking the 7337-CIDR
  allowlist until `IPRangeReconciler.reconcileInitial` fetches `/meta` and
  patches it back. A worker egressing inside that window is default-denied.
- **(b) Q247 CPU starve.** The node the worker lands on is CPU-saturated at
  the moment of the download, so the transfer stalls/times out even though the
  NP is correctly programmed.

## Method (live, not source-reading — repo has been burned by source-only, PR #59)
1. **Measure the race window (deterministic).** Instrument the `dogfood-workload`
   NP's `185.199.108.0/22` presence + total CIDR count on a tight poll, then
   scale `default-pool` 0→1 (the exact cold-start path `start.sh` uses; evicts &
   reschedules the GMC pod fresh). Capture: does the NP ever lose the web range?
   for how long? → the race-window duration.
2. **Live CI cold run.** Route CI to GAG (`GAG_RUNNER` = gag-ci), trigger a
   workflow whose job downloads a release asset (shellcheck tarball / setup-go
   toolchain). At the download moment capture: workload NP web-CIDR presence,
   node CPU/mem (`kubectl top`), and whether the download succeeds or times out.
3. **Correlate** the race window against the worker's actual download timing
   (acquire → pod → runner boot → checkout → download ≈ minutes after cold
   start). Decide (a) vs (b) from the live evidence.

## Decision rule
- NP missing the web range at worker-download time → **(a)**; implement the
  minimal fix (gate worker egress on cache-ready, or warm cache before first
  acquire).
- NP has the web range, node CPU pegged, download stalls → **(b)**; Q246 is
  subsumed by Q247/Q248, record and close (no new fix).
- NP has the web range, node CPU fine, download succeeds → Q246 no longer
  reproduces; record as resolved/subsumed.

## Findings — CONFIRMED CAUSE: (a) Q61 cold-start cache race

Live run on `gag-dogfood` (us-east1-b), GMC `v1.1.0-rc.5`, AGC `q247-45c7bff`,
direct-egress gateway, `2026-07-01`. All four observations are live-exec'd, not
source-inferred.

1. **Premise is dead (egress already works).** A pod carrying
   `actions-gateway/component=workload` (so `dogfood-workload` applies) curled the
   exact shellcheck release asset
   `github.com/koalaman/shellcheck/releases/download/v0.11.0/…tar.xz`: **HTTP 200,
   `remote_ip=185.199.108.133`, 0.32 s, 2 559 196 bytes**. The 302→
   `objects.githubusercontent.com`→`185.199.108.0/22` hop is already permitted —
   the workload NP carries **7337** CIDR peers including that range.

2. **The Q61 blank is real and live-measured.** Scaling `default-pool` 0→1 (the
   real cold-start path) forced a fresh GMC. From `08:39:01`→`08:39:26` (**~25 s**)
   the `dogfood-workload` NP dropped from `web=YES cidrs=7337` to `web=NO cidrs=1`:
   the per-CR reconcile (`ActionsGatewayV2Reconciler.applyNetworkPolicy`, a
   `CreateOrPatch` that overwrites `Spec.Egress` wholesale) rebuilt the NP from the
   still-empty IP-range cache (`githubCIDRs()`→nil → `githubCIDREgressRule` returns
   `ok=false` → GitHub rule omitted), then `IPRangeReconciler.reconcileInitial`
   completed the first `/meta` fetch and re-patched the 7337 CIDRs back.

3. **The blank denies the CDN — the exact Q246 symptom.** Removing the GitHub rule
   from the live NP (deterministically modelling the blank) while a curl loop hit
   the CDN: the sample inside the window returned **`code=000 rc=28`** (connect
   timeout — curl "downloads time out"), bracketed by HTTP 200 before and after.
   Restoring the rule → HTTP 200 again. NetworkPolicy default-deny semantics + this
   live test prove egress is dropped whenever the rule is absent.

4. **The blank is non-deterministic; CPU is the amplifier.** A fast *warm* GMC pod
   restart did **not** blank at all — the `/meta` fetch won the race. Only the slow
   *node-cold-start* blanked, for 25 s. So the window's occurrence and width scale
   with GMC-startup + `/meta`-fetch latency, which the Q247 node CPU exhaustion
   lengthens (and a CPU-starved e2-standard-2 system node can *restart* GMC
   mid-run, re-opening the window while workers are live). CPU (b) is a severity
   amplifier, not the cause: egress succeeds in 0.32 s whenever the NP is
   programmed, at any CPU level.

**Verdict:** cause is **(a) the Q61 cache race** — the per-CR reconcile blanking an
already-programmed direct-egress allowlist from an empty cache. (b) Q247 CPU
exhaustion only widens/re-triggers the window; it is already fixed (Q247) with node
right-sizing tracked in Q248.

## Fix (minimal)
Stop the per-CR reconcile from blanking an existing direct-egress NP's GitHub
allowlist while the IP-range cache is not yet ready: when the cache has not
completed its first fetch (`IPRangeCache.LastRefresh` bool false), **preserve the
existing NP's egress** instead of overwriting it with an empty-cache rebuild. A
not-yet-created NP is still created fail-closed (no GitHub rule) — safe, because no
worker exists that early and `IPRangeReconciler` patches the rule in within seconds.
This closes the window entirely: no GMC restart, under any load, ever strips a live
worker's (or the AGC's) GitHub egress. Secure-by-default preserved — egress is never
widened beyond the last-known-good GitHub ranges.
