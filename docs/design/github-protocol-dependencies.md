# GitHub protocol dependency register

> **Audience:** Maintainer / platform engineer.
> This is a durable watch-list, not a tutorial.

## Why this register exists

GAG's core capability — multiplexing thousands of virtual runner sessions and provisioning workers on demand — rests on speaking GitHub's **runner-facing protocols directly**, not just the public REST API.
Those protocols are GitHub's to change, and several are **not** covered by a published wire specification or a compatibility guarantee.
[Appendix D](appendix-d-alternatives-considered.md) names this explicitly as the maintenance risk GAG takes on in exchange for its density and isolation advantages over ARC ("this design re-implements a significant portion of the broker API … which carries ongoing maintenance risk").

As of the Q264 scale-set work, GAG depends on **two** such protocols at once — and one of them (runner-scale-set) is officially **Public Preview**, where GitHub reserves the right to change interfaces.
The risk is not theoretical: the live scale-set probe (Q264 §2a) found the current backend **auto-assigns** jobs, a behaviour that deviates from the ARC-era sources the client was built from.
A protocol GAG assumed was stable had already drifted.

This register is the single place that answers, for each GitHub-internal protocol GAG speaks: **what is the source of truth, how stable is it, and what event should make us revisit it?** Update it whenever a protocol dependency is added, changes stability tier, or drifts in a way that bites.

## Stability tiers

| Tier | Meaning | Implication |
|---|---|---|
| **T1 — Documented & supported** | Public wire spec or REST reference + a compatibility commitment. | Treat as stable; changes are announced. |
| **T2 — Official client, no wire spec (Preview)** | An official client library exists but the wire protocol is undocumented and the surface may change (pre-1.0 / Public Preview). | Pin a version; own the semantics in our own fake/tests; watch for GA and for backend deviations. |
| **T3 — Reverse-engineered, unsupported** | No official client and no spec; behaviour inferred from `actions/runner` / ARC source and live probes. | Highest drift risk; a silent server-side change surfaces only as a live failure. Cover with a live probe + dogfood early-warning. |

## The register

| Protocol | What GAG uses it for | GAG code | Source of truth | Tier | Drift-watch trigger |
|---|---|---|---|---|---|
| **Classic per-runner broker** | Today's acquisition path: `CreateSession` / `GetMessage` / `AcquireJob` / `RenewJob` / `CompleteJob` per virtual runner session. | [`broker/client.go`](../../broker/client.go); brokertest fake | Reverse-engineered from `actions/runner` + ARC (Milestone 1 wire probe); GAG documents it in [03-api-contracts §3.3](03-api-contracts.md#33-re-implemented-broker-api-endpoints). | **T3** | Being retired (Q264): the classic machinery is removed at the v2beta1 graduation. Until then, the dogfood e2e is the early-warning for a silent broker change; a live wedge that is *not* an on-our-side seam is the signal (the fan-out saga was one such investigation). |
| **Runner scale-set** | Q264 acquisition path: one session per scale set, capacity-gated long-poll, per-job JIT config; single-acquirer, kills the fan-out. | [`scaleset/`](../../scaleset/) client + fake; [`cmd/agc/internal/scalesetlistener/`](../../cmd/agc/internal/scalesetlistener/); [`cmd/probe/scaleset.go`](../../cmd/probe/scaleset.go) live probe | Official [`actions/scaleset`](https://github.com/actions/scaleset) Go client (Public Preview, MIT) as the *reference*, cross-checked against ARC and live probes. GAG owns its own client (Q264 §5a-U6-C) rather than vendoring. **No wire-spec document**; the auto-assign backend contract is undocumented (upstream [actions/scaleset#107](https://github.com/actions/scaleset/issues/107), unanswered). | **T2** | **Q270**: `actions/scaleset` reaching GA/v1.0, *or* the auto-assign contract (#107) being documented. Either fires a revisit of the U6 vendor-vs-own decision and lifts the Preview caveat. The `PROBE_SCALESET_*` scenarios in `cmd/probe` are the live drift probe; run them before relying on a wire assumption — the probe drives the `scaleset` package itself (Q362), so a live run is evidence about the shipped client, and it reports the raw wire through the client's `ResponseObserver` hook rather than issuing its own requests. |

*Not in scope here:* the **public REST API** (registration-token endpoints, org/repo admin) is T1 and covered by GitHub's normal API compatibility — it is the *runner protocols above* that carry the drift risk this register tracks.

## Why it matters beyond maintenance

- **Positioning.** GAG's differentiator vs ARC is partly *protocol transparency* — making the broker contract explicit and testable where ARC leaves it implicit ([Appendix D](appendix-d-alternatives-considered.md)).
  Depending on a Preview protocol without tracking its maturity would quietly undercut that claim; this register is how the claim stays honest.
- **Secure-by-default.** A protocol change can move a security boundary (e.g. where a token is delivered).
  Both current protocols keep the App token out of the worker and all traffic behind the per-tenant egress proxy; any tier change must be re-checked against that invariant, not assumed to carry over.

## Maintenance

Update this register when:

1. A **new** GitHub-internal protocol dependency is added (new row + tier + code pointer + watch trigger).
2. A protocol **changes tier** — e.g. `actions/scaleset` reaches GA (T2 → T1), or a T1 REST surface GAG starts depending on in an undocumented way (T1 → T3).
3. A **drift incident** occurs — record what deviated, how it surfaced, and whether the fake/tests were updated to encode the new reality (as Q264 §2a did for auto-assign).
