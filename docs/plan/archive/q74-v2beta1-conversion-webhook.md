# Q74 — v2alpha1 → v2beta1 graduation: conversion webhook

**Strategic context:** [v2beta1.md](../v2beta1.md) (the graduation plan; this is its §6 "graduation cut" in mechanical detail), [v2-api.md § API maturity & graduation](../v2-api.md#api-maturity--graduation-v2alpha1--v2beta1--v2) (the ladder), and [q264-scale-set-protocol.md §5a-U7/U8](q264-scale-set-protocol-phases.md#u8--support-matrix-policy) (the protocol-field strip + the P5-first sequencing).
This doc is the implementation blueprint for the conversion-webhook step, to execute once its dependency clears.

**Status:** ✅ Done (2026-07-06). v2beta1 shipped as served + storage/hub alongside v2alpha1, GMC-hosted conversion webhook wired, `acquisitionProtocol`/`maxListeners` stripped from the v2beta1 RunnerSet (annotation round-trip preserves them), chart + e2e install the conversion stanza, unit + envtest round-trip tests green.
The classic machinery / v1alpha1 removal (dropping v2alpha1, the destructive storage sweep) is the separate deprecation-window step tracked under Q264's residual — deliberately **not** part of this change (§Scope "Machinery + coexist").

## Blocked on Q264 P5 (the load-bearing finding)

Q74 is the **last rung** of the [U8 ladder](q264-scale-set-protocol-phases.md#u8--support-matrix-policy), not an independent task:

> P5 default flip → one-minor Classic deprecation (= v1alpha1 deprecation) → v2beta1 graduation ships ScaleSet-only

So the ScaleSet-only v2beta1 cut must **follow** Q264 P5 (flip the default `acquisitionProtocol` to `ScaleSet` + the positioning rewrite) and a one-minor Classic deprecation window.
Two reasons it is ordered this way:

1. **Positioning coherence.** A ScaleSet-only `v2beta1` while `v2alpha1` still *defaults to Classic* inverts the signal — the "more stable" version would force ScaleSet while the alpha hands you Classic.
   P5 makes ScaleSet the default + recommended protocol before the beta hard-commits to it.
2. **The graduation gate.** The [sunset review §6.4](../v1-classic-sunset-review.md) gates the cut on *clean-green + a stability soak of the scale-set path*.
   P4 clean-green landed (PR #545); the soak + the P5 flip have not — and P5's dogfood migration **is** that soak.

**Backlog-drift note (why this wasn't obvious).** [v2beta1.md §6](../v2beta1.md) predates the `acquisitionProtocol` field (added later by Q264 P3a, #534) and describes Q74 as a near-identity graduation; [Q264 §5a-U7/U8](q264-scale-set-protocol-phases.md#u7--where-the-protocol-selector-lives) then assigned Q74 the "strip the field + follow P5" requirement, but neither v2beta1.md §6 nor the Q74 Queue note tracked it.
Reconciled 2026-07-06 (this doc + the v2beta1.md §6 cross-ref + the Q74 STATUS note).

## Scope (once unblocked)

Everything **except** the RunnerSet protocol fields is a clean **identity** conversion: every shape/quality blocker the beta shape needed — Q191 broker-compat, Q196 credentials discriminated union, Q197 workload identity, Q205 well-known labels, Q218 disruption-safety — already shipped into `v2alpha1` (Q15 gVisor is demoted, not a hard gate).
The two exceptions, per [U7](q264-scale-set-protocol-phases.md#u7--where-the-protocol-selector-lives):

- **`RunnerSet.spec.acquisitionProtocol`** — v2alpha1-only; `v2beta1` never serves `Classic`, so the field is **dropped** at the graduation.
- **`RunnerSet.spec.maxListeners`** — meaningless under ScaleSet (one session per set; concurrency is `maxWorkers`/`priorityTiers` via the capacity header), so it is **removed** from `v2beta1.RunnerSet` at the same hop.

**Storage/served strategy (maintainer decision, 2026-07-06): "Machinery + coexist."** Add `v2beta1` as served **and** storage; keep `v2alpha1` served (coexistence / rollback — and the Classic on-ramp: `gag-migrate` maps v1 groups to `v2alpha1` Classic RunnerSets, tenants opt into ScaleSet by editing the set, then move to `v2beta1`).
Do **not** run the destructive storage-migration sweep or drop `v2alpha1` in this change.

## Design decisions (settled during the 2026-07-06 spike)

- **Hub = `v2beta1`** (the storage version); `v2alpha1` is the Convertible spoke.
  Only the spoke carries `ConvertTo`/`ConvertFrom`.
- **Conversion webhook is GMC-hosted.** All five v2 *admission* webhooks are already GMC-hosted (even the AGC-reconciled kinds), served off the GMC webhook server.
  The conversion webhook follows the same topology — one `/convert` endpoint for all five kinds. controller-runtime's `NewWebhookManagedBy(mgr, &v2beta1.Kind{}).Complete()` registers `/convert` for a Hub with no validator/defaulter.
- **Identity fields convert via a JSON round-trip.** `ObjectMeta` is the shared `metav1` type (direct assign); `Spec`/`Status` marshal from the spoke and unmarshal into the hub — lossless for identical shapes, self-maintaining, and far less error-prone than hand-writing field assignments through the embedded `PodTemplateSpec`.
- **RunnerSet protocol fields need an annotation round-trip** (the one non-identity case).
  `ConvertTo` (spoke→hub) stashes `acquisitionProtocol` (and `maxListeners`) in an annotation on the `v2beta1` object; `ConvertFrom` (hub→spoke) restores them so a coexistence-era `v2alpha1` set (Classic *or* ScaleSet) round-trips losslessly and is never silently re-protocol'd.
  A `v2beta1`-native set (no annotation) restores to the ScaleSet default.
- **Admission validation is not regressed.** The v2alpha1 validating webhooks stay in force; the generated config uses the apiserver default `matchPolicy=Equivalent`, so a `v2beta1` create/update is converted to `v2alpha1` and run through those validators — no need to duplicate them on the hub.
- **envtest auto-wires conversion.** `envtest`'s `modifyConversionWebhooks` (controller-runtime `pkg/envtest/crd.go`) auto-enables the conversion webhook on any CRD whose kind is convertible in the test scheme and redirects it to the local serving host + CA.
  So the integration suite needs only: register `v2beta1` in the test scheme + serve `/convert` on the test webhook manager.
  No CRD-file conversion stanza is required for tests.
- **Production CRD conversion stanza is a chart-sync-layer injection.** The deployment-specific `spec.conversion` (strategy `Webhook`, `clientConfig` → the GMC webhook service, cert-manager CA annotation) is Helm-templated, injected by `scripts/manifest/sync-chart-crds.sh` — the authoritative `api/config/crd` output stays conversion-free (controller-gen emits none, and envtest overrides it anyway).
  Mirrors how `sync-chart-webhook.sh` re-injects Helm wiring.

## Work breakdown (when unblocked)

1. **`api/v2beta1/` package** — copy the five kinds + shared types (`groupversion_info`, the four `*_types`, `shared_types`, `conditions`, `sidecar`); package + version → `v2beta1`; `+kubebuilder:storageversion` on the five root kinds; **drop `acquisitionProtocol` + `maxListeners` from `RunnerSetSpec`**; `Hub()` markers.
2. **`api/v2alpha1/conversion.go`** — `ConvertTo`/`ConvertFrom` for all five kinds (identity via JSON round-trip) + the RunnerSet annotation round-trip for the two dropped fields.
3. **Codegen** — `make -C api generate` (deepcopy + CRDs; each CRD gains `v2beta1`, storage flips to it).
4. **GMC wiring** — `v2beta1.AddToScheme`; `SetupConversionWebhooksWithManager` (`/convert` for the five Hub kinds), registered in `cmd/gmc/cmd/main.go`.
5. **Tests** — unit round-trip in `api/` (exhaustive fields incl. the annotation path)
   + envtest round-trip in the GMC integration suite (real apiserver → `/convert`, register `v2beta1` in the test scheme + serve `/convert`).
6. **Chart** — inject `spec.conversion` in `sync-chart-crds.sh`; `make chart-crds` + `make chart-crds-check chart-webhook-check` green.
7. **Operator docs** — v2 install / CRD reference (a conversion webhook changes what an operator installs + adds a served version), per the [doc-update matrix](../../development/doc-update-matrix.md).
8. **STATUS + PR** — remove the Q74 Queue row (isolated `docs/STATUS.md` commit); `make check` + chart drift + GMC integration green; PR referencing Q74.

## Security

Identity conversion preserves every field (credentials union discriminator, immutability CEL, PSA/egress wiring); nothing is defaulted away or relaxed.
The RunnerSet field strip is a *protocol* narrowing (ScaleSet-only), reviewed as part of the Q264 sunset — no security-property regression (egress isolation, no-token-to-worker, JIT-credential surface all unchanged; §7 of the sunset review).
