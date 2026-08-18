# Release and Publishing

> **Audience:** Maintainer

How a maintainer cuts a release: tag → publish (build, push, sign, attest, and package + push + sign the chart) → verify → record digests.
This is the **maintainer** runbook for *producing* a release.
Operators *consuming* a release pin the published digests at install time — see [tenant-onboarding.md](tenant-onboarding.md) and the [chart README](../../charts/actions-gateway/README.md).

## What a release produces

> Operators *installing* a release pin the published digests at install time — see [install.md § Pin images by digest](install.md#pin-images-by-digest).

A release is a `vX.Y.Z` git tag plus its outputs:

- The five first-party images — `gmc`, `agc`, `proxy`, `worker`, `wrapper` — pushed to GHCR (`ghcr.io/actions-gateway/<name>`), each tagged `vX.Y.Z` and by long commit SHA.
  Each is **multi-arch** (`linux/amd64` + `linux/arm64`): the pushed artifact is an OCI image **index**, and the digest recorded everywhere (run summary, release notes, chart pins) is the index digest — the kubelet resolves the per-arch manifest from it at pull time, so one pinned digest schedules on both amd64 and arm64 (e.g.
  Graviton) nodes.
- A keyless **cosign signature** on every image (sigstore/Fulcio via GitHub Actions OIDC — no signing key, no stored secret), signed **recursively** — the index *and* each per-arch manifest — and an **SPDX-JSON SBOM per architecture** attached as a keyless cosign attestation to that architecture's manifest.
- A signed **SLSA build-provenance attestation** on every image (`actions/attest-build-provenance`), attached to the index digest as an OCI referrer.
  It is generated through the *same* keyless path as the signatures — the publish workflow's GitHub OIDC identity → a short-lived Fulcio cert → Rekor — so the provenance is **authenticated** (it records the workflow, repo, commit, and trigger that produced the image and cannot be forged by a pusher).
  This reaches **SLSA Build L2**; buildx's own *unsigned* default provenance is disabled in favour of it.
  Consumers verify it with `gh attestation verify` or `cosign verify-attestation` (see step 3).
- The **Helm chart**, packaged and pushed as an OCI artifact to `oci://ghcr.io/actions-gateway/charts/actions-gateway`, with its `version` and `appVersion` set to the release tag and a keyless **cosign signature** from the same Fulcio/Rekor flow as the images.
  Operators install it straight from the registry (`helm install … oci://…`) with the published image **digests** pinned — no `git clone` of the chart.
  OCI (over a `gh-pages` chart repo) is chosen so the chart reuses the images' registry, login, and keyless-signing path; Artifact Hub (see [`Chart.yaml`](../../charts/actions-gateway/Chart.yaml) annotations) indexes the OCI ref for discoverability.
- The **opt-in v2 CRD chart**, packaged and pushed alongside the main chart to `oci://ghcr.io/actions-gateway/charts/actions-gateway-crds-v2`, with the same version derivation and keyless signature.
  It ships only the v2alpha1 (`actions-gateway.com`) CRDs — separated from the main chart because the large pod-template CRDs would otherwise push the main chart's Helm release Secret past its 1 MiB limit (Q149).
  Operators install it only when adopting the v2 API.
  Both chart packages are produced by the same `chart-publish` job.
- The **signed v2 CRD manifest** (Q276), a pre-rendered `actions-gateway-crds-v2.yaml` (rendered for the default `gmc-system` namespace) attached to the tag's **GitHub Release** with a keyless **cosign blob signature** (`sign-blob` → a Sigstore bundle, `actions-gateway-crds-v2.yaml.cosign.bundle`).
  This is the helm-free manual install path — the v2 CRD chart is too large to `helm install`, so operators `kubectl apply --server-side -f …/releases/download/<tag>/actions-gateway-crds-v2.yaml`.
  The `chart-publish` job renders, signs, and uploads it (widening the job to `contents: write`).
- The **`gag-migrate` CLI binaries** (Q306), cross-compiled by `chart-publish` for the operator platform matrix (`linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64`, `windows/amd64`) via `scripts/release/build-migrate-binaries.sh` and attached to the **GitHub Release** as `gag-migrate-<tag>-<os>-<arch>` assets.
  A single `SHA256SUMS` manifest is keyless **cosign `sign-blob`**-signed (`SHA256SUMS.cosign.bundle`) — the same no-secret Fulcio/Rekor path as the v2 CRD manifest — so one signature covers the whole set (verify the manifest signature, then `sha256sum -c`).
  This is the one-shot v1→v2 migration tool, previously source-build-only.
- The **GitHub Release** itself (Q293), composed by `chart-publish`: the five image index digests, the `make verify-release` command, a generated changelog, and a tag-derived `--prerelease` flag.
  It is created only if the tag has no Release yet, so a maintainer's pre-tag curated notes are never clobbered.

Both the image and chart work are automated by the [`publish.yml`](../../.github/workflows/publish.yml) workflow, which triggers on the `v*` tag push (the `chart-publish` job runs after every image leg succeeds).
The maintainer's job is to cut the tag and verify the result.

> **Signing and chart publish are exercised for the first time on the first real `v*` tag.** Pull-request CI builds each image and generates its SBOM, but it does **not** push, sign, attest, or publish the chart (those need a registry push and the publish workflow's OIDC identity).
> The verification step below is therefore not optional on the first release — it is the only thing that proves the signing and chart-publish paths work.

## One-time setup (first release only)

1. **GHCR package visibility.** The first publish *creates* the `ghcr.io/actions-gateway/{gmc,agc,proxy,worker,wrapper}` image packages **and the `ghcr.io/actions-gateway/charts/actions-gateway` chart package**.
   They inherit the repository's visibility and may start **private**.
   For third parties to run `cosign verify` / `helm pull` (and for an air-gapped operator to pull), set each package to **public** in the org's GHCR package settings, or keep them private and distribute pull credentials.
   Verification by *this project's* CI and by anyone with pull access works either way — but the **released-chart upgrade gate** (`make chart-released-upgrade-check`, run by every e2e CI pass) pulls the chart and the released `gmc` image **anonymously**, so those two packages must be public for PR CI to stay green.
2. **Workflow permissions** are already declared in `publish.yml` (`packages: write` to push, `id-token: write` for keyless cosign and provenance, `attestations: write` for the build-provenance attestation).
   No repo secret is required — that is the point of keyless signing.

## When to cut

Everything below this section is *how* to cut a release.
This section is *whether* to — the question the rest of the page assumes has already been answered.

Start from the record, not from memory.
The delete-on-done backlog erases delivered work by design, so nothing in the backlog shows what has piled up since the last tag:

```bash
scripts/release/release-delta.sh
```

It reports, for `<last stable tag>..origin/main`, the commits by Conventional Commit type (breaking ones called out), the Queue rows closed in that window, the API diffstat, and the operator-facing pages touched.
Pass an explicit `FROM` (and optionally `TO`) to look at a different window.

Its type counts answer "how much landed", not "what may this be called".
For that:

```bash
scripts/release/semver-floor.sh
```

It reports the **minimum bump the merged work already requires**, the floor a release cannot be cut below, by reading the paths each commit touched rather than its subject.
That distinction is the whole point: of the 17 `feat` commits in the `v1.3.0..v1.4.0` window, 11 are dev tooling, CI, and docs that ship in no image and no chart, so the `feat` count is not the answer and never was.
The report names the commits that set the floor, with the shipped files that put each one there, and lists the ones it withheld so a dropped commit stays visible.

**A shipped path is where attribution starts, not where it ends.** A commit that edits only a godoc line inside a released package directory touches the surface and ships a byte-identical binary.
So each shipped Go file is read again on both sides of the commit and kept only when its token stream moved: comments and whitespace are not in that stream, while `//go:build` and `//go:embed` are, since those decide whether a file compiles and what goes into the binary.
Everything less certain is kept as shipping, including a chart file, a file that does not scan, and one that was added or deleted.
The floor is a floor, and the only costly error is dropping a commit that did change behaviour, so what the narrowing removed is printed under **Comment-only** rather than dropped quietly.
It still reproduces all four release windows to date as `minor`.

**The surface it checks against is derived, not listed.** It reads publish.yml's image matrix, follows each image's Dockerfile stage back through its `COPY --from=` edges to the `go build` that produced the binary, and expands those with `go list -deps`; the chart trees come from the same workflow's `helm package` calls.
Add an image or a chart and it is picked up with nothing to maintain.
The one thing not derivable is a release asset built by a script, the `gag-migrate` CLI, which is declared instead; `make semver-floor-sources-check` (part of `make check`) fails if publish.yml stops matching that declaration.

**It reports; it does not gate.** The floor is monotonic and saturates early.
Across the four release windows to date it reached `minor` at commit 15 of 341, 6 of 95, 40 of 463, and 16 of 121, so a gate on the transition would fire on 4 pull requests out of 1,020 and say nothing for the rest of every cycle.
The first shipping feature after a tag is the expected event, not an accident worth catching.

**A breaking marker is reported as an unresolved major, never as one.** Whether a `!` broke anything depends on whether the last tag had published the surface it changed, which is a field-level question.
All three markers in `v1.2.0..v1.3.0` changed surface `v1.2.0` never published (`capacityGate` and `windowStartTime` are both absent there), which is why `v1.3.0` shipped as a minor, and why a tool that promoted them to major would fail to reproduce the one release that tests it.
The report narrows the question with the part that is measurable, diffing the CRD property names `FROM` published against `TO`; a clean result means no published property was removed, and nothing more.
A published field whose type changed or whose enum narrowed, and the non-CRD contracts, stay with [`api-surface-since.sh`](#1-pre-flight) and the human reading it.

**The triggers.** Any one of these is reason enough to scope a release; none of them is automatic, and the report is the input to the judgement rather than a substitute for it:

- **A security fix users cannot get any other way.** Cut promptly, as a patch off the release branch if `main` carries unrelated risk ([Patch releases and backports](#patch-releases-and-backports)).
- **A headline capability landed.** Scope a minor around it — one thing an adopter would upgrade *for*, with the rest of the window riding along.
  That is the [1.3](../plan/release-1.3.md) pattern: worker right-sizing was the headline, and three dozen other changes shipped underneath it.
- **User-visible fixes accumulated with no feature.** A patch release.
  The bar is that an operator running the current tag is hitting something already fixed, not that the fix count crossed a number.
- **Internal-only churn.** Wait.
  Refactors, test coverage, CI work, and docs the site publishes continuously do not need a tag to reach anyone.

**The counterweight — a tag is not free.** Every GA cut spends the [release-candidate dogfood validation](#validate-the-release-candidate-on-dogfood): a real GKE cluster, a live e2e matrix, and a maintainer watching it.
That cost is what makes "enough" a real bar rather than a formality — it is also why the answer to a thin delta is *wait*, not *cut a small one*.

**Once you decide to cut, the question changes.** From that point the report stops being the view: write the release plan doc, open it with a [scope ledger](../development/maintaining-backlog.md#cutting-a-release-the-scope-ledger), and let the ledger's `-gate` rows answer "is it done?" until the tag.

## Release sequence

### 1. Pre-flight

**A recorded verdict covers the commit it was measured at, so record that commit with it.** Each step below ends in a verdict written to the release's plan doc, and a verdict reads as done long after the scope it covered has moved.
When scope reopens, every verdict taken before it is stale by default and re-runs unless someone can say why not.

1.5 is the worked example, and it went both ways.
The API surface review had been recorded over `v1.4.0..feabacdc4`; scope then reopened on eight rows, one of which published a new condition and two new series, and the review was re-run only because someone thought to ask what the recorded window covered.
The marketing reconciliation, recorded the same day, was **not** re-run, and the cross-tenant scale-set name guard reached no marketing surface at all until a question about the website found it.
Both reviews were correct when written.
Neither knew its scope had moved.

**Which of these bind when the tag is a release candidate.** The first two bind at every tag: an RC cut from a red `main` validates nothing, and the version you pick fixes the stable tag's.
The last four are stable-tag obligations, because a prerelease deploys no docs and its GitHub Release body is generated rather than curated, so there is no published surface for them to be wrong on yet.

**Two are worth pulling forward to the first RC anyway**, on the same argument: the RC is the artifact that gets validated, so anything decided after it is published costs a new candidate and another hour-long dogfood run.

The **API surface review is the exception worth pulling forward to the first RC**.
Its deadline really is the stable tag, but the RC is the artifact that gets validated, and a rename decided after the candidate is published costs a new RC and another hour-long dogfood run.
Reviewing it at the first RC and recording the verdict then is what keeps that cheap.

**A draft of the notes belongs at the first RC too, for the same reason and a stronger one.** Writing the notes is what forces the release's claims to be *stated*, and a stated claim is the only kind anyone can check.
Reviewing that draft is therefore a discovery step, not a formatting pass: it is reliably where the release's remaining defects surface, because nothing else in pre-flight asks the product to describe itself.

Interrogate the draft with three questions.
Each of them found something real in the 1.5 cycle, and each found it *after* the candidate was cut, which is what made it expensive:

1. **Does every claim in it still hold?** The 1.5 draft said multi-label registration closed a gap, and checking the neighbouring claims found `why-gag.md` asserting a capability was classic-only that had reached both tiers a release earlier.
2. **What does this release leave as a landmine for an operator who never reads these notes?** The best answers are the ones no caveat report can produce, because nothing changed in a file: 1.5's sharpest was that `helm upgrade` never applies CRDs and a structural schema prunes unknown fields silently, so a skipped step leaves a declared security boundary inert.
   The two caveats that were *not* landmines each shipped a condition or Event that fires exactly when an operator would otherwise be confused, which is the shape to aim for.
3. **Is every completeness or parity claim backed by something derived, or by a list someone maintains?** A curated inventory answers "nothing that someone recorded is missing" ([testing.md](../development/testing.md#generate-a-fixture-with-the-producers-own-code-never-by-hand)), and a release note is where that distinction becomes a published promise.

Record the verdict in the release's plan doc as with the other pre-flight steps, and file what it turns up as gating rows.
The point of doing it here rather than at the tag is that the answers are free before a candidate exists and cost a new RC afterwards: in 1.5 they arrived after `rc.1` had been cut, published and validated, and superseded it.

- `main` is green: every required gate passing on the commit you are about to tag.

  ```bash
  scripts/release/check-gates-green.sh origin/main
  ```

  It asks by commit across every lane, because `gh run list --branch main` hides merge-queue runs and so reports "not validated" about a commit that is.
  It reads **job** conclusions rather than the run's, because each `<workflow>-gate` job passes on `skipped` as readily as on `success` ([testing.md](../development/testing.md#path-gated-workflows-verify-the-heavy-gates-actually-ran)).

  **Expect `SKIPPED`, and do not read it as red.** It is the ordinary shape of a release tip: docs-only merges sit on top of the last code change, so the heavy jobs path-gate themselves away.
  All nine required workflows had skipped their heavy job on the commit `v1.5.0` was tagged at, and the release was sound, because the code was the tree an earlier commit had validated in full.
  Prove exactly that, and say which commit you are relying on:

  ```bash
  scripts/release/check-artifact-unchanged.sh <last-fully-validated-sha> origin/main
  ```

  A `NOT GREEN` line is the different answer: a lane failed, or none reported at all.
  That one blocks the tag.
  Run `make check` locally as a final gate, from a branch cut from `origin/main` so the tree matches the target.
- Choose the version `vX.Y.Z` (semver).
  The tag **must** match `v*` or `publish.yml` will refuse to publish.
- **Review the API surface this tag publishes for the first time.** A field, enum value, or default costs a rename to change before it ships and a conversion shim plus a deprecation window afterwards, so the tag is the moment the cheap window closes.
  Nothing lints this — it is judgement, not a gate that can pass or fail mechanically.

  ```bash
  scripts/release/api-surface-since.sh
  ```

  Apply the checklist in [api-review.md](../development/api-review.md#step-2--ask-these-of-each-addition) to each addition it lists, record the verdict in the release's plan doc, and file anything deferred as a Queue row carrying this release's gate label.
  **"Ship as-is, deliberately" is a valid and common outcome** — the point is that the shape is chosen rather than frozen by default.

  This step exists because it nearly did not happen: Q476 renamed `capacityGate.mode: On` days before 1.3.0 would have published it, and only because the question came up in an unrelated conversation.

- **Reconcile the marketing surfaces against what this tag actually ships.** `docs/index.md`, `docs/why-gag.md`, `README.md`, and `docs/features.md` make claims about GAG *and* about alternatives, and both halves rot: GAG gains capabilities the pages never learn about, and a competitor closes a gap the pages still assert.
  Neither is caught by any gate, because both are prose.

  Three questions, in this order:

  1. **Does anything shipped since the last tag belong on a marketing surface?** `docs/features.md` is the accurate inventory; the other three are curated.
     Nothing propagates upward on its own, so a capability that lands only in `features.md` is a capability nobody selling the project knows about.
  2. **Does every claim about GAG still describe GAG?** Check especially claims written against an older acquisition tier or API version, and any "by default" wording against the actual kubebuilder defaults in `api/`.
  3. **Does every claim about an alternative still hold?** Whether each one *says* when it was measured is no longer a question you have to ask of `why-gag.md`'s comparison table: `make comparison-stamps-check` fails the page if a verdict there carries no version and date.
     What is left is deciding which stamps have aged out, oldest first:

     ```bash
     scripts/docs/check-comparison-stamps.sh --report
     ```

     Re-read a stale cell at the current upstream version and re-stamp it, or drop it to the unverified state until someone can.
     Prose claims outside that table (the "where ARC is ahead" bullets, `README.md`, `docs/index.md`) are not gated and still need the eye.

  Record the verdict in the release's plan doc and file corrections as Queue rows carrying this release's gate label.

  This step exists because the 2026-08-06 competitive review found 11 rows in `why-gag.md` asserting a gap in Actions Runner Controller (ARC) with **no ARC version and no measurement date on any of them**.
  Two had gone false at datable upstream releases (ARC 0.13.1 and 0.14.0) and nobody noticed, one contradicted an explicit instruction in the project's own working notes, and one promised a recovery behaviour the code deliberately refuses.
  The same review found nine shipped capabilities that no marketing surface mentioned.
  Rot runs in both directions and only a deliberate pass catches it.

- **Review the operator-facing caveats this tag publishes, and curate the notes if there are any.** `publish.yml` writes a generated body from the commit log, which never says "this upgrade needs a manual step first".
  Richer notes are opt-in and must be created *before* you push the tag ([§ 5](#5-cut-the-github-release)), so the decision belongs here, not after.

  ```bash
  scripts/release/operator-caveats-since.sh
  ```

  It needs no bookkeeping to stay current: the [doc-update matrix](../development/doc-update-matrix.md) already requires an operator-visible change to land in `docs/operations/`, so the diff of those pages since the last tag already *is* the list — this only makes it scannable (added sections, bold-lead bullets, anything marked `BREAKING`) instead of a release-sized raw diff.

  Reading it is judgement: a clarification is not a caveat, and the script cannot tell them apart.
  What it guarantees is that you have **seen** them.
  Carry the real ones into a curated body:

  ```bash
  gh release create vX.Y.Z --draft --notes-file <file>
  ```

  This step exists because the alternative is remembering.
  The `v1.2.0`→next window accumulated a required pre-upgrade `kubectl apply`, a removed values key that fails the render, and a rollback that re-arms a cluster-wide outage — each recorded correctly in `docs/operations/upgrade.md` by the change that introduced it, and each invisible to anyone reading a generated changelog.

- **Reconcile [`docs/roadmap.md`](../roadmap.md) and [`docs/features.md`](../features.md) against [`docs/queue/`](https://github.com/actions-gateway/github-actions-gateway/blob/main/docs/queue/README.md) before you tag.** The same freeze that applies to the announce bar applies here: a stable tag deploys that tag's docs wholesale, so a stale roadmap is published permanently under that version.
  `make roadmap-check` catches the mechanical half — a roadmap bullet naming a deleted Queue row, or one sitting in the wrong section — and it runs in CI.
  What it cannot catch is the move itself: **work that shipped this cycle needs a `docs/features.md` line**, and a Deferred row describing a capability an adopter would ask about belongs in *Exploring*.
  A 2026-07-25 audit found six of seven near-term items already shipped.
- **Optional: refresh the docs-site announce bar's highlight.** The banner in [`overrides/main.html`](../../overrides/main.html) is the "vX.Y.Z is here" strip at the top of every page on the site.
  Its **version needs no action**: it is derived from the git tags at build time, so a stable tag names itself automatically ([website.md § The announce bar](../development/website.md#the-announce-bar)).
  What you may want to update is the one-line highlight after it, and the `highlight_for` version that guards it.
  Leave both alone and the banner reads *"vX.Y.Z is here.
  Read the release notes."*, which is correct but plainer; update them together and it leads with this release's headline.
  Land the change before you tag either way.

  This used to be a manual version bump, and every stable tag to date missed it: `v1.0.0` shipped saying *"Alpha, pre-1.0"*, and `v1.1.0` and `v1.2.0` both said *"v1.0.0 is here"*.
  Preview what the tag will render, standing in the version you are about to cut (without it the local build resolves the *current* newest tag, which is still the previous release):

  ```bash
  GAG_DOCS_RELEASE=vX.Y.Z make docs-build && awk '/md-banner/,/<\/aside>/' site/index.html
  ```

  **`publish.yml` enforces the result** via its `announce-bar` job, which every publishing job depends on: it builds the site at the tag and fails the release if the *rendered* banner does not name it, before any image is pushed.
  Prereleases are exempt (they publish no docs), as is a backport tag cut after a newer minor, since the banner advertises the newest release by design.

  (A `docs_ref` seed of an already-cut release pins `overrides/` to the current checkout, so re-seeding refreshes the highlight on past tags too.
  See [website.md § Seeding](../development/website.md#seeding-already-released-versions).)

#### Validate the release candidate on dogfood

Before promoting a release-candidate line to a **stable** `vX.Y.Z` tag, validate the *latest RC* functionally on the dogfood cluster.
`main`-green covers unit/integration/kind-e2e, but publishing an image the pipeline signed is not the same as proving it runs jobs — this gate exercises real GAG-provisions-runners-on-GKE behaviour the CI tiers can't observe.
Run it before every GA (`vX.Y.Z`) cut; skip it only for an RC-to-RC or a patch tag that changes nothing an operator runs.

The dogfood scripts pin GAG to any published ref via `GAG_IMAGE_TAG`, which resolves both as an image tag (`ghcr.io/actions-gateway/{gmc,agc,proxy,wrapper}:<ref>`) and as a git ref (for the matching CRDs) — an RC tag satisfies both by construction.

**One command runs the whole gate**, and it runs for the better part of an hour — a green `v1.3.0-rc.4` run took 39 minutes end to end — with nothing to type at it after the first confirmation.
`validate-release.sh` bakes in all the env and ordering below — deploy → route CI → on-demand e2e → dispatch the e2e matrix (run-scoped routing) → CRD smoke → teardown — is idempotent, and self-cleans back to 0 nodes on exit (success or failure — and on Ctrl-C, though [not on every ending](#a-killed-gate-is-reclaimed-by-the-next-one)).
On failure it first dumps a cluster snapshot (nodes, pods, unhealthy-pod detail, events) to the gate's output, because the teardown's scale-to-0 evicts every pod and destroys the evidence — read the `Failure diagnostics` section of a failed run's log (e.g. the `FailedScheduling` events) instead of re-running the gate to watch it fail again.
The [legs it runs](#the-legs-the-gate-runs) are documented at the end of this section, and are the recovery path if one needs re-running by hand.

**If you just merged something, the gate waits before it spends anything.** The gate's dispatched run enters the e2e workflow's per-ref concurrency group, whose single pending slot the next push to main would cancel it out of — and the latest `e2e-test.yml` run is usually the still-running push-run of the merge you just made.
The gate settles the lane up front — before the node scale-up, the deploy, and the e2e AGC — so a collision costs a wait, not a cluster cycle.
It polls for up to `E2E_WAIT_TIMEOUT` seconds (default 1800), then fails with the run id; `E2E_WAIT_TIMEOUT=0` fails immediately instead of waiting.

**The e2e leg's watch is bounded, so a run that never starts cannot hold the cluster.** The dispatched run is watched for up to `E2E_RUN_WATCH_TIMEOUT` seconds (default 5400 — 90 minutes: the 60-minute job ceiling plus the same 30 minutes of queue the settle wait allows).
Past that the gate fails with exit 124, names the run, and tears the cluster back down to 0 nodes as it would for any other e2e failure.
**The run itself keeps going on GitHub** — the deadline releases the nodes the *gate* is holding, not the ones the run is queued for, so cancel the run by hand if it is genuinely wedged.
A healthy leg finishes in 25–33 minutes; raise the variable rather than removing the bound if yours is legitimately slower.

The gate also checks every local tool it needs up front — including the pinned `cosign` the final CRD-smoke leg verifies with (`make cosign` downloads it to `.build/cosign`; `COSIGN=<path>` overrides) — so a missing binary fails the run before it spends anything, not 25 minutes in.

##### The gate reserves the e2e pool's CPU budget

**The gate's two legs compete for one project-wide CPU quota, so it caps the CI side before routing.** Its deploy leg routes CI to GAG, whose `workers` pool autoscales to 8 `e2-standard-4` nodes out of the same `CPUS_ALL_REGIONS` budget the e2e leg then needs 2 `n2-standard-8` nodes from.
When the budget cannot cover both, the e2e pool is what loses, and the autoscaler reports it as a bare `FailedScaleUp: GCE quota exceeded` that names no quota.
Two `v1.3.0-rc.5` runs died there, 25 minutes in.

Before it spends anything the gate now reads the live limit, takes the e2e and system pools' ceilings off it, and derives what is left for `workers`.
It prints the arithmetic:

```text
Reserving the e2e pool's CPU budget before CI can compete for it...
  CPUS_ALL_REGIONS: 64 vCPU, 0 already in use
  reserved: default-pool 2xe2-standard-2 = 4 vCPU, e2e 2xn2-standard-8 = 16 vCPU
  leaves workers 11 node(s) of e2-standard-4
```

**Most runs print that and change nothing.** At today's 64-vCPU limit the reservation leaves room for more `workers` nodes than the pool is configured to run, so no cap is applied.
It binds only when the budget has shrunk relative to the cluster, and then the gate holds the `workers` autoscale ceiling down for its own window and restores it in teardown.
CI queues behind the cap instead of starving the e2e leg.

**A preflight failure means free capacity, not a bigger quota.** Raising the limit moves the collision rather than removing it.
The usual cause is that something is still up from earlier work, most often the manually sized benchmark pool:

```bash
PROJECT=… CLUSTER=… ZONE=… scripts/dogfood/ops.sh at-rest
```

```bash
PROJECT=… CLUSTER=… ZONE=… scripts/dogfood/ops.sh pool-scale workers-od 0
```

If teardown could not restore the ceiling it says so and prints the `gcloud container clusters update` that puts it back.
A ceiling left low throttles everyone's CI, so run it.

##### A killed gate is reclaimed by the next one

**Self-cleaning covers most endings, not all of them.** Bash runs the teardown trap on Ctrl-C and on an ordinary `kill`, so those tear the cluster back down.
`kill -9`, a killed parent process, and a teardown interrupted part-way through do not — each leaves billable nodes up with no process left to release them, and twice that was caught only by hunting for a live teardown process by hand (Q640).

So the gate takes an ownership lease for exactly the window in which it owns cluster state, and reclaims an *orphaned* one — a lease for the same target whose owning process is gone — before it spends anything.
Running the gate again is therefore enough to end a leak the previous run started.
To do only that, without starting a validation run:

```bash
PROJECT=… CLUSTER=… ZONE=… REPO=… scripts/dogfood/validate-release.sh --reclaim
```

It reports a target nothing claims and exits, or tears an orphaned run's cluster back down to 0 nodes (confirming first, as the gate itself does).
Run it when a gate was killed and you are not about to start another one.

**The lease is the only thing it acts on**, because the alternative is worse than the leak.
A cluster that merely has nodes up is what a hand-run `setup.sh`/`start.sh` debugging session looks like, so nothing here infers an orphan from cluster state: no lease, no teardown.
A lease whose process is still alive means another gate is running, and the second gate refuses to start rather than tearing down the first one's environment.
A lease written by another host is reported and never acted on — a pid means nothing off the host that minted it.
Leases live in `${XDG_STATE_HOME:-~/.local/state}/github-actions-gateway/`, host-wide rather than per-checkout, so a gate killed in one worktree is visible to a gate started from another.

Two gaps remain, both by design.
A cluster left up by something *other* than this gate is not visible to the reclaim — nothing claims it, so nothing reclaims it; take it down with `scripts/dogfood/stop.sh`.
And a killed gate that is never followed by another run or a `--reclaim` still bills: the state is recorded, but nothing reads it until someone runs one of the two.

##### Confirming the cluster is actually at rest

The teardown reports what it did, not what the cluster is.
Every step in it is guarded so one failure cannot skip the rest, so `Teardown complete` prints even for a run whose `stop.sh` refused to scale down (a drain that will not converge is the usual reason).
After a failed gate, a killed one, or a `--reclaim`, ask the cluster separately:

```bash
PROJECT=… CLUSTER=… ZONE=… scripts/dogfood/ops.sh at-rest
```

It exits **0** at rest, **1** with instances still up, which it names, and **2** when the project could not be read.
That third status is the point: an unreadable project is not an idle one, and only one of the two is safe to walk away from.

**Do not read a node count off the cluster object instead.** `gcloud container clusters describe --format='value(currentNodeCount)'` prints an empty line for a cluster at 0 nodes.
The field is output-only and deprecated in the GKE API, and proto3 JSON omits an integer holding its default, so at zero the key is absent from the describe output rather than present and zero. gcloud prints empty for any absent key and exits 0 (measured on 577.0.0), which is also what it prints when the projection never resolved at all.
At rest and answered-nothing are then the same reading, and the safe-looking one wins: on 2026-08-09, just after a DNS outage, a teardown was nearly reported at rest on it (Q779).
The instance list has no such hole, because every instance carries a name: an empty list means no instances, and the check reads the probe's own exit status rather than inferring it from the emptiness.

##### Run it detached; the sentinel reports it back

**This is the default path.** Nobody should spend an hour watching a terminal for a gate built to be walked away from.
Launch it as a background task — an agent session's background task, or `nohup … &` by hand — from a checkout of any post-Q74 ref, with `PROJECT`/`CLUSTER`/`ZONE`/`REPO` exported (App IDs auto-resolved; the one-time `scripts/dogfood/e2e-setup.sh` must have run once):

```bash
ASSUME_YES=1 PROJECT=… CLUSTER=… ZONE=… REPO=… \
  nohup scripts/dogfood/validate-release.sh vX.Y.Z-rc.N >tmp/validate-release.log 2>&1 &
```

**`ASSUME_YES=1` is required when detaching.** The gate confirms the resolved target once before it spends anything, and a detached run has no stdin to answer with — without it the gate exits 1 immediately, having done nothing.
Leave it off when you run the gate in your own terminal, so the confirmation still gates a fat-finger.

Then launch the sentinel as a second background task.
It is what turns a silent hour into a report:

```bash
bash scripts/dogfood/release-sentinel.sh
```

It sleeps, and *exits* when there is something to say — a phase transition, a verdict, or a gate that has gone quiet for `RELEASE_SENTINEL_STALL` seconds (default 1200) **while GitHub reports no live run it could be waiting on**.
That exit is the report: it carries the phase, both clocks, the run URL, the latest e2e heartbeat, and what to do next.
Relaunch it after each report until a verdict arrives — in an agent session, the exit is what wakes the session, and the relay-and-relaunch loop is the session's job.
Reporting is therefore driven by what the gate does, not by a clock: nothing is spent on an interval where nothing changed.
Knobs: `RELEASE_SENTINEL_INTERVAL` (poll seconds, default 30 — it bounds how quickly a transition is *noticed*, never how often anything is reported), `RELEASE_SENTINEL_TIMEOUT` (watch budget, default 7200), `RELEASE_SENTINEL_STALL`.

**The sentinel's exit is a wake, never a verdict** — every event exits 0.
The verdict is the gate's own exit status, and the failure diagnostics are in the gate's log, not in the report.

**A quiet gate is not on its own a stalled one.** Through the ~25-minute e2e leg the only thing writing to the stream is the relayed spec heartbeat, and that needs a job log GitHub will sometimes not serve — one run answered every log fetch with `BlobNotFound` for its whole 30 minutes and then passed.
Since the stall threshold is shorter than a healthy leg, quiet alone reported a stall on every poll of that run.
So the sentinel reconciles the silence against the run's own status (`gh run view --json status`, the run record — a different endpoint from the log, and one that answered throughout that incident) and reports a stall only once the run is no longer live.
Phases with no run to consult — the deploy and teardown legs — are unchanged: quiet there is the gate's own.

A gate genuinely stuck on a run that never concludes is caught elsewhere, by the watch's own `E2E_RUN_WATCH_TIMEOUT`, which fails the gate and reaches the sentinel as a `failed` event.

**A stall is reported once, not once per relaunch.** The quiet does not clear when the report is read, so a watcher relaunched against it would exit immediately, forever.
The sentinel remembers the stall it reported in `tmp/release-validation-progress.jsonl.stall` and stays asleep through the same one; it speaks up again when the stream moves, or when the same silence has deepened by another full `RELEASE_SENTINEL_STALL`.
Nothing needs cleaning up between runs — the marker is keyed to the moment it went quiet.

##### Where is it right now?

Reading back an hour of log to answer that is the wrong shape, so the gate keeps its event stream rendered as one object in `tmp/release-validation-status.json`, rewritten atomically after every phase transition and every relayed e2e heartbeat.
This is what the sentinel reads, and it answers the same question directly for a human:

```bash
jq . tmp/release-validation-status.json
```

`gate` is `preflight` (settling the e2e lane, nothing spent yet), `running`, `passed`, or `failed`; `phase`, `elapsed`, `phaseElapsed` and `idle` say where and for how long; `runRepo`/`runId` name the dispatched e2e run, which is what makes a quiet leg checkable against GitHub rather than only against the stream; `heartbeat` carries the newest relayed spec line — absent for a whole leg when GitHub will not serve the job log, which is a fetch problem and not a stalled run; `failure` names the phase that broke — the one that broke first, not the teardown that followed it.
`scripts/dogfood/release-status.sh [stream-file]` renders the same object from any stream, including one whose gate process is gone.
`RELEASE_STATUS_FILE=` disables the file.

Underneath it, each phase transition is appended as one JSON line to `tmp/release-validation-progress.jsonl` — the stream both renderers read, and inspectable directly (`tail -f`) without disturbing the run.
Point `RELEASE_PROGRESS_FILE` elsewhere to move both files, since the status object defaults to `release-validation-status.json` beside the stream, or set `RELEASE_PROGRESS_FILE=` to disable both.
The gate's own output is unaffected either way.

**A `preflight` reading is the gate's own, never the last run's.** The gate empties the stream before its preflight and writes its first event after it, so the whole window renders `preflight` and a previous run's verdict is gone before anything can read it.
It has to be cleared that early because preflight is not brief — a settle wait alone runs to `E2E_WAIT_TIMEOUT` — and a spent stream still holds its terminal event.
Reading one is how the sentinel once reported `passed`, with the earlier RC's tag and a 101-hour elapsed time in its own output, for a run that had not started.
The one stream it leaves alone belongs to a gate whose lease is still `held`, which is the concurrent-gate case the lease refuses moments later anyway.

##### Running it in your own terminal instead

Equally supported, and equally legible — drop the `nohup`/`&`/`ASSUME_YES=1` and answer the confirmation:

```bash
PROJECT=… CLUSTER=… ZONE=… REPO=… scripts/dogfood/validate-release.sh vX.Y.Z-rc.N
```

**The gate narrates itself while it runs.** Each phase is announced as it starts (`==> [e2e] Running the e2e matrix on GAG runners`), and the e2e leg — the long one, ~25 minutes while runners autoscale in — relays the dispatched run's own spec heartbeat into your terminal every 30 s:

```
[e2e t+04:12] 31/73 specs | 29 ok, 1 failed, 1 skipped | running: E2E_GMC_Isolation cross... (3m58s)
```

That heartbeat comes out of the run's own job log, and GitHub does not always serve one — it served nothing for the whole 22-minute leg of the `v1.5.0-rc.1` run that passed.
When that happens the gate relays job-level progress off the run record instead, so the leg is never silent for its full duration:

```
[e2e run] 3/7 jobs done (3 ok) | running: e2e / e2e (calico)
```

A `[e2e run]` line means the log is unreadable, not that anything is wrong; it is replaced by the spec heartbeat the moment one arrives.
The run URL is printed before the watch begins if you would rather follow it in a browser.
When the e2e leg finishes — pass **or fail** — the run's JUnit report is rendered into your terminal: counts, every failing spec with its message, and the ten slowest specs.
A red gate names the specs that failed without you having to open the run.

None of that narration is a terminal redraw.
The gate's progress output is append-only with no cursor control, by design — which is why a detached run's captured log carries the same phase lines and heartbeats rather than a screenful of escape sequences, and why three identical heartbeat counts in a row are readable as a stall.

##### The legs the gate runs

What follows documents what `validate-release.sh` does, and is the recovery path if a leg needs re-running by hand.
From a detached checkout of the RC tag (`git switch --detach vX.Y.Z-rc.N`):

1. **Deploy the RC to dogfood.** `setup.sh` needs `APP_ID`, `INSTALLATION_ID`, and `ASSUME_YES=1` exported alongside `GAG_IMAGE_TAG` — it reads the GitHub App private key from the macOS keychain (not from an env var), so run it on a macOS host that has that keychain entry.
   The cluster sits at **0 nodes at rest**, so `setup.sh`'s GMC-rollout wait has nothing to schedule on and **times out** — that is expected; `scripts/dogfood/start.sh` then scales the system pool to one node and completes the rollout.
   So: `GAG_IMAGE_TAG=vX.Y.Z-rc.N APP_ID=… INSTALLATION_ID=… ASSUME_YES=1 scripts/dogfood/setup.sh` (a timed-out rollout wait here is fine), then `scripts/dogfood/start.sh`.
   Run the one-time `scripts/dogfood/e2e-setup.sh` first if the e2e node pool / GitHub App Secret aren't set up yet.
   The cluster, context pinning, and prod-guard cautions are in [gke-dogfood.md](../plan/gke-dogfood.md).
2. **Run the e2e job matrix on GAG runners.** This is two moves, not one.
   `scripts/dogfood/e2e-start.sh` spins up the on-demand e2e tenant's AGC — it does **not** start a run and does **not** touch routing.
   Trigger the matrix by **dispatching a run with routing scoped to it**:

   ```bash
   gh workflow run e2e-test.yml --ref main -f runner='"gag-ci-e2e"'
   ```

   (same for `e2e-calico.yml`).
   Only that dispatched run lands on the RC's GAG-provisioned runners; every concurrent PR and merge keeps its normal hosted runners.
   Do **not** reach for the repo-wide `GAG_E2E_RUNNER` variable here — flipping it routes every e2e job in the window, and a caught job wedged main CI when the teardown deleted the AGC under it (2026-07-31; the variable remains only as an `E2E_ROUTE_VAR=1` opt-in for a standing dogfood soak).
   **Node contention:** the on-demand e2e AGC (~500m CPU) does not fit on the single `e2-standard-2` system node beside the always-on CI AGCs (the CI AGC goes `Pending`/`Insufficient cpu`), so temporarily add a system node (e.g. scale `default-pool` to 2) for the duration of the e2e leg and scale it back after.
   **CPU budget:** running the legs by hand skips the reservation the gate applies, so if the e2e pool reports `FailedScaleUp` here, read [the reservation section above](#the-gate-reserves-the-e2e-pools-cpu-budget) rather than the family quotas.
   Require the matrix **green** — this is GAG running its own CI end-to-end on the RC images.
3. **Smoke the signed v2 CRD asset.** Download the RC release's `actions-gateway-crds-v2.yaml` + `.cosign.bundle`, `cosign verify-blob` against the publish identity (step 3 below), `kubectl apply --server-side` it, and assert the five v2 CRDs register — the helm-free install path operators actually use.
4. **Assert the sizing profiles actuated.** A profile that silently falls back to `Static` still provisions a healthy pod and still runs the matrix green, so without this leg every other check reports success while the release's headline feature sits inert.
   `sizing_leg` treats the two profiles differently on purpose:

   | Profile | Tenant | Behaviour |
   |---|---|---|
   | `NodeShare` | `gag-dogfood-e2e` | **Hard failure.** It needs no sample history, so it must report `sizingProfileState: Active` and derive the envelope's per-worker share. Anything else is a defect. |
   | `Throughput` | `gag-dogfood` | **Reported, never fatal**, and by now normally `Active`. The ≥20 samples per template container come from the CI tenant's ordinary traffic rather than this gate's ~7-job matrix, and that history is long past the threshold: measured `Active` on `v1.4.0-rc.1` (2026-08-09) and again on `v1.5.0-rc.1` (2026-08-14, `sampleCounts=[188]`). |
   | `Binpack` | — | Not re-asserted; live-validated 2026-07-25. |

   **`NOT VALIDATED THIS RUN` is the exception, not the expected reading — and it is two different problems, so read the state before reaching for the sample count.** The leg prints which one you have:

   | `sizingProfileState` | What it means | Fix |
   |---|---|---|
   | *empty* | `spec.sizing` is not on the live RunnerSet — a deploy gap, not a sample gap. A CR edit reaches the cluster only through `setup.sh`'s `apply_cr` or a direct patch; `scripts/dogfood/start.sh` resizes the pool and routes CI but **never applies CRs**, so no start can deploy it. | Re-run `scripts/dogfood/setup.sh`, or `kubectl patch` `runnersets.v2alpha1.actions-gateway.com/ci` in `gag-dogfood`. |
   | `AwaitingSamples` | The profile is deployed but a template container is below the threshold. Check the `sampleCounts` the leg prints for which one. | Let ordinary CI traffic run ~20 jobs per container. |

   Sample history needs no advance planning: the sampler tracks every worker pod regardless of `spec.sizing`, and the aggregate re-seeds from the persisted `status.sizingRecommendation` — so samples accrue without the profile configured and survive a stop/start rather than being re-earned.

5. **Tear down.** `scripts/dogfood/e2e-stop.sh`, then `scripts/dogfood/stop.sh` (dogfood scales to 0 at rest).

A red matrix, a failed CRD smoke, or a dead `NodeShare` profile is a **stop-ship for the GA tag**: fix forward and cut a new RC — never promote a known-bad RC to a stable tag.

### 2. Tag and push

**What may land between a validated candidate and the stable tag.** A candidate's dogfood validation covers the tree it was cut from, so anything merged after it that moves an artifact makes the verdict describe something other than what ships.
That is how `v1.5.0-rc.1` was superseded: it validated, eight rows merged on top, and its verdict stopped describing `main`.

The rule is **byte-identical artifacts, not "docs only"**.
Documentation changes are expected in this window and cannot be avoided, because the validation verdict does not exist until the candidate is tagged, so the notes' Validation section is necessarily written afterwards.
But the labels do not line up: `charts/actions-gateway/README.md` is a markdown file that **ships inside the chart tarball**, so a pure-docs pull request can change a published chart's bytes.
Check rather than classify:

```bash
scripts/release/check-artifact-unchanged.sh <validated-candidate-sha> origin/main
```

Exit 1 means the stable tag would ship something no candidate validated.
Revert it, or cut and validate a new candidate.

**Land [step 7](#7-bump-the-pinned-release-in-the-docs)'s pin bump on `main` before you tag a stable release**, and confirm the gate names the version you are about to cut:

```bash
make release-pins-check
```

The site builds each version from its own tag, so a bump landing *after* the tag never reaches that release's published page.
Three of the four releases cut since `1.0.0` shipped the previous version's install command as their landing page; `v1.3.0` escaped only because a hand-fix happened to land first.

Bumping early is possible because a tagged candidate makes it so: `check-release-pins.sh` accepts a pin naming a release that has a candidate and no stable tag yet.
It accepts the *current* release too, so a green gate is not on its own evidence the bump has landed: read the version it prints.

The pin bump is a documentation change, so it does not disturb the freeze check above.

```bash
git switch main && git pull --ff-only
git tag -a vX.Y.Z -m "Release vX.Y.Z"
```

**Check what the tag actually points at before pushing it.** These two must print the same commit:

```bash
git rev-parse "vX.Y.Z^{commit}" && git rev-parse origin/main
```

```bash
git push origin vX.Y.Z
```

The check is here because creating a tag and pushing it are two moments, and anything can happen between them.
A tag left over from an earlier attempt makes `git tag -a` fail, so a `&&` chain skips the push and does nothing — while a bare `git push` sends the **stale** tag instead.
That is how `v1.5.0-rc.2` published from a commit three merges behind, and a published immutable Release locks its tag, so the only repair was to burn the candidate number ([postmortem](../postmortems/2026-08-15-rc2-tagged-a-stale-commit.md)).
Cutting from a worktree makes the gap wider, since the tag is created against `origin/main` rather than a checked-out branch.

Pushing the tag starts `publish.yml`.
Watch it:

```bash
gh run watch "$(gh run list --workflow=publish.yml --branch=vX.Y.Z -L1 --json databaseId -q '.[0].databaseId')"
```

> A `workflow_dispatch` run with a `tag` input publishes the same way without a git tag — use it to dry-run the pipeline against a throwaway `vX.Y.Z-rc1` tag.

> **The docs site publishes this release too (Q238).** A **stable** `vX.Y.Z` tag also triggers [`pages.yml`](../../.github/workflows/pages.yml), which deploys the release's docs as a new version on [actions-gateway.com](https://actions-gateway.com) and moves the `stable` alias + the site's default root redirect to it — so the public docs default to this release, with the unreleased `main` docs kept behind an opt-in `dev` version.
> Prerelease tags (`0.x`, `-rc`/`-alpha`/`-beta`) do **not** deploy the site.
> Because each version builds from **its own tag**, the version pins in this tree are what the release publishes as its landing page, and they still name the previous release.
> [Step 7](#the-bump-on-main-does-not-reach-the-published-release) republishes them.
> The versioned-docs model, and the one-time `mike` seeding of releases cut before it landed, are documented in [website.md § Versioned deploy](../development/website.md#versioned-deploy-mike).

### 3. Verify the publish

Confirm every image **and the chart** was signed by *this* workflow before announcing the release.
The one-command check uses the pinned cosign (`make` downloads `COSIGN_VERSION` — the same version `publish.yml` signs with — into `.build/`):

```bash
make verify-release VERSION=vX.Y.Z
```

> **A broken or missing chart publish now reddens PR CI, not just operators.** CI's released-chart upgrade gate ([testing.md § The released-chart upgrade gate](../development/testing.md#the-released-chart-upgrade-gate-q507)) discovers the highest stable `vX.Y.Z` tag on the repo and `helm pull`s that chart version from GHCR on every e2e run.
> Pushing a stable tag whose `chart-publish` job failed therefore fails e2e on **every subsequent PR** until the publish is repaired (re-run the `publish.yml` run for the tag) or the tag is removed.
> Prerelease (`-rc`) tags are ignored by the gate.

This verifies the five image signatures (`gmc`, `agc`, `proxy`, `worker`, `wrapper`) plus the chart (whose tag is `X.Y.Z`, without the leading `v`) against the publish workflow's keyless identity.
It needs no credentials once the GHCR packages are public.
The equivalent explicit commands (and SBOM attestation retrieval) live in [security-operations.md § Image provenance](security-operations.md#image-provenance-signature--sbom-verification); each is a `cosign verify --certificate-identity-regexp '…/publish\.yml@refs/tags/v.*$' --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' <ref>`.

`make verify-release` covers the OCI artifacts (images + both charts) but not the GitHub Release's **signed v2 CRD manifest** asset (Q276), which is a *blob* signature — verify it against the same identity with `verify-blob`.
Download the manifest and its bundle from the release, then:

```bash
cosign verify-blob --bundle actions-gateway-crds-v2.yaml.cosign.bundle \
  --certificate-identity-regexp '^https://github.com/actions-gateway/github-actions-gateway/\.github/workflows/publish\.yml@refs/tags/v.*$' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
  actions-gateway-crds-v2.yaml >/dev/null && echo OK
```

A `cosign verify` failure is a **stop-ship**: do not announce the release until it passes.
Spot-check one SBOM attestation too so the attestation path is exercised — SBOM attestations are bound to the **per-arch manifest digests**, not the index, so resolve one first (the full command set is in [security-operations.md § Retrieve and inspect the SBOM](security-operations.md#retrieve-and-inspect-the-sbom)):

```bash
digest="$(docker buildx imagetools inspect ghcr.io/actions-gateway/gmc:vX.Y.Z --raw \
  | jq -r '.manifests[] | select(.platform.os == "linux" and .platform.architecture == "amd64") | .digest')"
cosign verify-attestation --type spdxjson \
  --certificate-identity-regexp '^https://github.com/actions-gateway/github-actions-gateway/\.github/workflows/publish\.yml@refs/tags/v.*$' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
  "ghcr.io/actions-gateway/gmc@${digest}" >/dev/null && echo OK
```

Also spot-check that the index actually carries both platforms (`docker buildx imagetools inspect ghcr.io/actions-gateway/gmc:vX.Y.Z` should list `linux/amd64` and `linux/arm64` manifests).

Finally, confirm the **build-provenance attestation** is present and was minted by *this* workflow.
The attestation binds to the **index** digest (unlike the per-arch SBOMs), so a tag reference resolves correctly:

```bash
# Verifies the signed SLSA provenance against the publish workflow's identity.
gh attestation verify oci://ghcr.io/actions-gateway/gmc:vX.Y.Z \
  --repo actions-gateway/github-actions-gateway \
  --signer-workflow actions-gateway/github-actions-gateway/.github/workflows/publish.yml
```

> **Exit 0 alone does not prove this ran.** `gh attestation verify` writes its summary only to a terminal — redirected to a file or captured in a variable it prints **nothing**, so a real verification and a silent no-op look identical.
> When you are not reading the output live, ask for something assertable and check it:
>
> ```bash
> gh attestation verify oci://ghcr.io/actions-gateway/gmc:vX.Y.Z \
>   --repo actions-gateway/github-actions-gateway \
>   --signer-workflow actions-gateway/github-actions-gateway/.github/workflows/publish.yml \
>   --format json \
>   | jq -r '.[0].verificationResult.signature.certificate
>            | "\(.buildSignerURI)\n\(.sourceRepositoryDigest)"'
> ```
>
> The workflow URI must end `publish.yml@refs/tags/vX.Y.Z` for *this* tag, and the digest must be the commit you tagged.
> That is the check — not the status.

The equivalent cosign command and the predicate-inspection one-liner are in [security-operations.md § Verify build provenance](security-operations.md#verify-build-provenance).
A provenance verification failure is the same **stop-ship** signal as a `cosign verify` failure.

### 4. Record the published digests

`publish.yml` writes each image's immutable `ghcr.io/.../<name>@sha256:…` ref to the **run summary** (the "Record published digest" step) **and into the GitHub Release notes** (step 5).
These are the **multi-arch index digests** — the single ref that serves both amd64 and arm64 nodes.
Operators pin the workload to the digest (`gmc`, `agc`, `proxy`, `worker`, `wrapper`), not the mutable `vX.Y.Z` tag.
You can also resolve a digest directly:

```bash
docker buildx imagetools inspect ghcr.io/actions-gateway/gmc:vX.Y.Z \
  --format '{{json .Manifest.Digest}}'
```

**Reconcile what you transcribed against what published.** The `Container images` section is the only part of a note that cannot be written before the tag, so it is copied in by hand after the release is already sealed, into the document operators take their `--set …image.digest=` pins from:

```bash
scripts/release/check-release-digests.sh vX.Y.Z
```

It asserts each digest matches the registry **and** is a multi-arch index, because a digest can be wrong in two ways: provenance binds to the index while SBOM attestations bind per-arch, so a per-arch digest pasted here pins operators to one architecture and still verifies cleanly against its own attestation.

### 5. Cut the GitHub Release

**`publish.yml` creates the GitHub Release itself** (Q293) — no manual step.
The `chart-publish` job's "Compose and create the GitHub Release" step writes the body with the five `name@sha256:…` **index digests**, the `make verify-release VERSION=vX.Y.Z` command, and a generated changelog (previous-tag compare link), and sets `--prerelease` from the tag (0.x or a `-rc`/`-alpha`/`-beta` suffix ⇒ prerelease; a stable `≥1.0.0` tag ⇒ latest).
So the default flow for this step is: **nothing — verify the auto-created Release looks right.**

The step only creates a Release when the tag has **none yet**, so it never clobbers curated notes.
If you want richer notes (highlights, upgrade caveats), **create the Release before pushing the tag** — e.g. `gh release create vX.Y.Z --draft --notes-file …` — and the pipeline will leave your body untouched while still attaching the signed v2 CRD manifest asset.

**The Release is created as a draft and published by the pipeline's last step**, because this repo's releases are immutable: publishing seals the assets, and every upload after that point fails `HTTP 422: Cannot upload assets to an immutable release`.
Draft, attach, then publish is [GitHub's own recommended order](https://docs.github.com/en/code-security/concepts/supply-chain-security/immutable-releases) for that reason, and it is what `chart-publish` now does.
Two consequences for this step: a pre-created Release of your own should be a **draft** (as above), since the pipeline publishes whatever draft it finds once the assets are on; and a run that dies before the final step leaves a draft rather than a published Release missing its CRD manifest and checksums.

**What immutability does and does not freeze.** The title and the notes stay editable after publication, so [step 4](#4-record-the-published-digests)'s `gh release edit --notes-file` still works and the curated body can be finished after the digests exist.
The assets and the **tag** do not: a published Release locks its tag to one commit, which cannot be moved or deleted while the Release exists.
So a tag pushed at the wrong commit is not recoverable by re-tagging — cut the next candidate number instead.
`v1.5.0-rc.2` was pushed at a commit three merges stale and had to be superseded by an rc.3.

#### Writing the curated notes

A minor release accumulates more than a generated changelog can convey, and the two inputs that feed it, `operator-caveats-since.sh` and the commit log, both mislead in specific ways.
What follows is the method, written after `v1.3.0`.

**Author the notes in [`docs/releases/`](https://github.com/actions-gateway/github-actions-gateway/tree/main/docs/releases), not in a scratch file.** One file per stable tag, `vX.Y.Z.md`, holding the release body verbatim — no front matter, and **no title heading** — the Releases page already renders the tag name as the page's `<h1>`, so a `# vX.Y.Z` in the body duplicates it.
Publish from it:

```bash
gh release edit vX.Y.Z --notes-file <(scripts/release/render-release-body.sh docs/releases/vX.Y.Z.md)
```

Publish through the renderer, never the file directly: the file is sentence-per-line by gate, and comment-flavour GFM turns each of those newlines into a `<br>`.
`make release-notes-check` covers the rest of what a machine can settle here (a duplicate `# ` heading, an in-page anchor that renders dead, a `helm` command carrying an image-style chart version) and reports collapsed height for the fold decision.

`v1.3.0`'s notes were drafted under `tmp/` and edited straight on the Release.
By the time they were right they had been through a wrong count, a dead anchor, 46 forced line breaks, two mismatched PR numbers, and a caveat that never said "GHES" — every one caught by hand, none by review, and none of it reviewable because the text was not in a diff.
In-repo makes each fix a diff and each published body reproducible from a commit.

These files are excluded from the docs site on **every** version, dev included: they are written for github.com's renderer, which the site is not.
The exclusion is spelled out in four places that must agree — `mkdocs.yml`, two `env:` blocks and one `export` in [`pages.yml`](../../.github/workflows/pages.yml), and `scripts/docs/docs-preview.sh`.

**Past bodies are retrievable, so the previous release is your template:**

```bash
gh release view vX.Y.Z --json body --jq .body
```

Use it to seed a new file when a tag predates this convention.

**Notes answer "what is in it" and "what must I do".
The docs answer "how" and "why".** Every explanation that can live in `upgrade.md` or an `operations/` page should, behind a link.
`v1.3.0`'s first draft ran ~1000 words of prose; cutting it to links lost nothing.
Link a Highlight from its bold lead, and link group headings rather than every line.

That rule shortens the *prose*.
It does not shorten the notes: `v1.3.0` shipped 2100 words and 25 links, because enumerations kept being added — a feature list, a fix list, the API surface, the condition reasons.
Prose is what gets cut; lists are what a reader actually searches.
Fold the lists (below) and the length costs nothing.

**A caveat that says a number moves is re-derived from the code, never paraphrased from the change that moved it.** `v1.5.0`'s job-duration caveat said the classic tier's old span *excluded* the pre-creation window, and that cost attribution had been reading *low*.
Both are inverted.
The span ran from job acquisition to the pod going terminal, so it charged the staging, quota-retry and `spec.scaleUp` throttle window, and attribution read high.
What let it through is that the commit and [the plan doc](../plan/release-1.5.md) were both correct: the note was written from a sibling artifact instead of from `provisioner.go`, and a paraphrase can reverse a direction while keeping every noun in place.
It then survived a full pre-flight and `rc.1`, because [`operator-caveats-since.sh`](https://github.com/actions-gateway/github-actions-gateway/blob/main/scripts/release/operator-caveats-since.sh) reports what changed and never judges a claim, by design.
So for any caveat asserting that a value rises, falls, widens or narrows, open the diff that moved it and read both states before writing which way (#1529).

##### The section skeleton

`v1.3.0` arrived at this order after several passes.
It is ordered by what a reader needs first, not by what took the most work:

| Section | Answers | Notes |
|---|---|---|
| *(one-line tagline)* | what this project is | for the reader who arrived from a search result |
| *(danger banner)* | is anything here going to hurt me | a GFM alert, above the fold; see below |
| **Highlights** | why upgrade | 3–5, each linked from its bold lead |
| **Upgrading** | what must I do | numbered steps; say which are guarded |
| **Deprecations** | what is going away | see below |
| **Everything since `<prev>`** | did my bug get fixed | folded lists with counts |
| **API and metric surface** | what changed in the contract | CRDs *and* metrics; see below |
| **Validation** | why should I believe you | receipts, not adjectives |
| **Project and tooling** | is this project healthy | contributor-facing; last for a reason |
| **Security** | is there anything I must patch for | state it even when the answer is no |
| **Verifying this release** | how do I check the artifacts | the `make verify-release` line |

**Say something about security even when there is nothing to report.** Silence reads as an omission to the one reader scanning specifically for it.
State plainly that no advisory accompanies the release, then list what it does carry — dependency security bumps, and any fix that hardens credential or trust handling without patching a reported vulnerability.
Name the scanning gates and when they run.
`v1.3.0` had no CVE and still warranted the section.

**Lead with a danger banner, and make it an alert.** GFM alerts — `> [!WARNING]`, `> [!CAUTION]`, `> [!NOTE]` — render as real coloured callouts in a release body (verified; the render check below counts them).
`v1.3.0` opens with a `[!WARNING]` naming the two required upgrade steps and the asymmetric rollback, because those are the only things that can hurt an operator who reads no further.
Use `[!NOTE]` for a scope caveat and `[!CAUTION]` for the one thing that is genuinely destructive.
Three alerts is a lot; more and none of them read as urgent.

**Write a Deprecations section even when nothing is removed** — saying so is the point, since "deprecation" reads as "removal" to a skimmer.
For each notice give the removal version, the migration path, and **whether the apiserver actually warns**.
`v1.3.0` deprecated `v2alpha1` (warns on every apply) alongside `v1alpha1`, which is removal-slated and emits **no** warning at all — so nothing reminds an operator it is going away.
That asymmetry is exactly what a reader cannot discover for themselves.

**Diff every surface an operator can see, not just the CRDs.** Each of these is enumerable, and each hides in a different file, so a review that reads only the Go diff misses most of them.
`v1.3.0` shipped five: CRD fields, metric names, Kubernetes Event reasons, condition reasons, and configuration (chart values, env tunables, CLI flags).
Diff each between the two tags mechanically rather than reading the changelog for them — the Event reasons and the metrics had no enumeration at all until they were diffed, and the notes had already been through several reviews.

Two traps.
**A rename reads as a removal** when the extraction is scoped to one directory: env vars first appeared to have 17 removals, all of which were code moving out of `cmd/`; re-running repo-wide showed zero.
**Adjacent string arguments read as the same thing**: `recordEvent(obj, type, reason, action, …)` puts a reason and an action side by side, so `ProvisionWorker` and `ApplyAGCAutoscaler` both survived extraction as reasons until each call site was checked.
Always report "none removed" when it is true — operators are looking for exactly that.

**Diff `docs/` as well, and link what is new from where it is actionable.** A new operator page is the strongest signal of a capability the notes forgot, and a heavily grown one shows where the release's real weight landed:

```bash
git diff --name-status v<prev>..origin/main -- docs/operations/ | grep '^A'
git diff --stat     v<prev>..origin/main -- docs/operations/ | sort -t'|' -k2 -rn | head
```

`v1.3.0` added three operator guides and grew `troubleshooting.md` by 36 sections.
Link a new guide from the bullet it serves rather than from a documentation inventory — `resourcequota-sizing.md` belongs on the quota-accounting upgrade note, where an operator hits the problem it solves.
A guide with no feature to attach to goes in the contributor-facing section with one line on why it exists.

**Give the API surface its own section, and lead it with any new CRD.** A new kind is not a field: the chart installs chart-root `crds/` on a *fresh install only*, so a new CRD is the reason "apply the CRDs" is step 1 of Upgrading, and the two must cross-reference.
Then fold the rest — new spec fields, new status fields, new condition reasons — grouped by kind, counted like any other fold.
`v1.3.0` listed 28 new condition reasons this way, and had to name `kubectl explain` and the signed CRD asset as the authority because no reference page existed.
Link the [generated API reference](../reference/api.md) instead, at **this release's** version path (`/X.Y.Z/reference/api/`), so the fields the notes name are one click from their descriptions.
It covers `v2beta1` only; a deprecated-version field still needs `kubectl explain`.

**Validation is receipts, not adjectives.** "Thoroughly tested" is worth nothing.
Link the run, quote the counts, and quote a value measured at the layer that matters: `v1.3.0` cites 73/73 specs with a run link, and a derived `1500m` observed on the pod where the templates asked for 2 and 3 CPU.
Ship the receipt wherever a claim is made, not only in this section — a feature line that links its own PR is a receipt too.

**Check the validation story against the plan doc, not memory.** This is the section a sceptical reader checks first, so a wrong detail here costs more than anywhere else.
`v1.3.0`'s draft claimed no candidate had ever cleared the gate and that rc.5 was the first to return a verdict; the plan doc records rc.4 passing the day before.
The true version was better anyway — five candidates, three aborted, rc.4 passed without catching a live worker pod, rc.5 caught one — and it is checkable, which the flattering version was not.

**Keep a contributor-facing section, and put it last.** Release, CI, docs-site, and tooling work ships in no image and no chart, so it does not belong in the change lists.
It still belongs in the notes: it is what a reader evaluating the project's health is looking for.
Fold it, label it as not user-facing, and let it sit below everything an operator needs.

**The caveats script reports headings that *changed*, not headings that are *new*.** A section edited in this window is listed exactly like one added in it.
Test each before repeating it:

```bash
git show <prev-tag>:docs/operations/upgrade.md | grep -qF "### <heading>" \
  && echo "pre-existing" || echo "new in this window"
```

`v1.3.0` listed two `BREAKING` headings and **neither** was a caveat for an upgrading operator: `priorityTiers` was already in `v1.2.0` (a pre-1.0 change), and `capacityGate.mode`'s removed values had only ever existed on `main`.
Repeating them unexamined would have sent operators after migrations they did not need.

**Promote by danger, not by label.** The most hazardous item in `v1.3.0` carried no `BREAKING` heading at all: the PriorityClass allowlist CRD apply, which affects every install and whose rollback re-arms a cluster-wide outage.
Read for consequence, not for keywords.

**Distinguish "breaking" from "guarded migration".** If skipping a step stops the upgrade with a message naming the fix, it is a required migration and saying "breaking change" overstates it.
If a wrong path fails silently, say so loudly.
State which of the two you mean.

**Enumerate from the commit log, classified by the paths each commit touched.** Conventional Commit subjects are already terse diagnoses, so they need only the prefix and Q-ID removed:

```bash
scripts/release/semver-floor.sh <prev-tag> HEAD --notes
```

Two lists come back and they sum to the window's `feat`/`fix`/`perf` count: **Ships** is the enumeration to curate, **Residue** is every commit reaching no released artifact.
A commit is admitted by the files it changed, checked against the surface `publish.yml` packages, so no scope string admits or excludes anything.
The sum is the reconciliation: a commit cannot leave one list without arriving in the other.
Keep the trailing `(#NNNN)`: GitHub auto-links a bare `#NNNN` in a release body, so every line becomes traceable for free.
Say in the notes that dev tooling, CI, and docs are excluded, so a reader does not read hundreds of commits as the user-visible change count.

**The scope allow-list this replaced failed in both directions, and its output showed neither.** It matched `^(feat|fix)\(<scope>\)` against a hand-maintained list of scopes, which silently omits any scope nobody thought of.
At `v1.3.0` the first pass kept 57 of 132 and dropped **seven shipping fixes**: two `fix(scalesetlistener)` (the pattern had `scaleset`, which does not match it), a compound `fix(agc,gmc)`, three `fix(migrate)` for the shipped `gag-migrate` binary, and one `fix(observability)`.
At `v1.4.0` it kept 15 of 58 and got one wrong each way: it dropped the scopeless `feat:` carrying Q166, a headline feature that matched nothing because it has no scope at all, and it kept a `feat(metrics)` touching only `claude-usage/`, because that module and the product's Prometheus metrics share a scope.
Paths settle all nine, and `--notes` puts each on the correct side of both windows.

**Read the residue anyway.
A release publishes things no image and no chart carries.** The surface is derived from `publish.yml`, so it sees only what that pipeline packages, and an artifact published any other way is invisible to it.
The runner template library (Q554) was one of `v1.4.0`'s three headline features and ships as `deploy/templates/`; it lands in the residue, not in Ships.
So the residue is ordered rather than dumped: commits that also changed `docs/operations/` come first, an added page ahead of an edited one, because [doc-update-matrix.md](../development/doc-update-matrix.md) requires an operator-visible change to land there.
At `v1.4.0` that ranks Q554 first of 43: ten rows carry the flag and thirty-three do not.
Read the ten, and treat the rest as dev tooling only after the flag has had its say.

**Cite the commit that did the work, not the one that filed it.** A Q-numbered backlog row and its implementation have near-identical subjects, so a `docs(plan)` commit reads exactly like the fix.
`v1.3.0` cited #988 — `docs(plan): file and scope Q507` — under the label of #1008, the gate itself; a reader clicking through would have landed on a planning row.
Resolve each number to its title before shipping, and look for the same work cited twice under two numbers.

**One fact per line, especially next to a procedure.** Distinct operator-facing changes run together into a paragraph read as background, and a paragraph sitting under a numbered list reads as a footnote to it.
`v1.3.0`'s Upgrading section closed with three unrelated changes — quota accounting, a dropped proxy label, a new apiserver warning — in one sentence-run below its two numbered steps; as a bulleted list with a bold lead each, the same words are scannable.
Prose is for framing.
Anything a reader might need to act on individually gets its own line.

**Fold long lists.** `<details><summary>` renders on the Releases page and keeps the top scannable.
That is what folding is for, and it is worth doing on its own terms.

**Folding is not a lever against index truncation, and the ordering is.** Measured 2026-08-15 against the live Releases index: **every stable release this project has cut is truncated** behind a "Read more" link, `v1.3.0`, `v1.4.0` and `v1.5.0` alike, including the one this runbook previously recorded as having been folded back under the limit.
The cut lands around **9k of source**, and the content past it is genuinely absent from the index page rather than merely hidden.

So the question is never "how do I get under the limit" — you will not.
It is **what does a reader see before clicking**, and the answer is decided by section order:

```bash
make release-notes-check
```

It names the sections above and below the cut for each note.
`v1.5.0`'s six folds all begin past it, which is why a seventh would have changed nothing, and why its Deprecations section is not visible on the index where `v1.3.0`'s and `v1.4.0`'s were.
A fold helps only when it collapses something that would otherwise sit inside the first 9k.

**When the content being folded is evidence, put the evidence in the `<summary>`.** A fold whose summary reads "Validation details" hides the receipt; one that reads `73/73 e2e specs on Kata microVM workers, on live GKE — the four legs, and what none of them assert` *is* the receipt, and a reader who never expands it has still seen the number.
That is the exception to the count-in-the-summary convention: enumerations carry a count, evidence carries the finding.

**Count what you list — and count the unit in the label.** State a count in a `<summary>` and it will be wrong the moment you curate the list.
`v1.3.0`'s draft claimed 25 features and listed 23.
Subtler: its "New spec fields (10)" had ten *bullets* carrying thirteen *fields*, because three bullets grouped related ones (`.minRequests` / `.maxRequests` / `.limitHeadroomPercent`).
Every other fold counted the noun in its own label; that one silently switched to bullets.
Count what the label says, then re-count after every edit — mechanically, not by eye.

**Caveat anything a validation run did not exercise.** The dogfood gate runs against github.com, so it says nothing about GHES.
A feature list that reads as finished support overclaims.
Check `docs/plan/archive/` for the feature's own "what this will not verify" section before describing it.
Then ask whether an unexercised feature belongs in Highlights at all — `v1.3.0` kept GHES there and paid for it with a caveat in three places.

**A caveat is a claim, so measure it before writing it.** Understating coverage is as wrong as overstating it, and easier to do accidentally because it feels safe.
`v1.3.0`'s draft said the capacity gate had "unit and envtest coverage only"; the repo has a live-cluster-autoscaler test for its matcher (#929) and 305 lines of e2e proving a quota-blocked job redelivers (#1028).
Before writing "only tested at tier X", grep for the feature at every higher tier — and if the true statement is narrower than the tidy one, ship the narrow statement.
What survived here was "the release gate does not *assert* it", which is checkable.

**A caveat must survive being read alone.** Every line in a folded list is read out of its heading's context — by search, by a linked anchor, by a skimmer.
The `v1.3.0` draft said "Untested against a real appliance" under a GHES heading, which says nothing at all once the heading scrolls away; it shipped as "Untested against a real **GHES** appliance".
Name the subject inside the caveat, and repeat the caveat at each place the feature is claimed rather than relying on proximity.

**Link the versioned docs site, not `main` and not `blob/`.** A reader of these notes should land on that release's instructions, and the site publishes a build per stable tag: `https://actions-gateway.com/X.Y.Z/operations/…`.
Mind the form — the site drops the leading `v` exactly like the chart does.
`v1.3.0` shipped 18 such links.
They 404 until the docs deploy for the tag completes, which is expected while the Release is still a draft.

**Verify every link and anchor.** A gate does it for you now (Q636):

```bash
make release-links-check
```

It builds `site/` if it is missing and resolves every `https://actions-gateway.com/X.Y.Z/…` link in `docs/releases/` against it — `…/operations/upgrade/#gmc-rollback` becomes `site/operations/upgrade/index.html` carrying `id="gmc-rollback"`.
Anchors are the usual failure, and the built site is the authoritative oracle: reading the ids MkDocs actually emitted beats re-deriving a slug by hand, which gets punctuation, backticks, and parenthesised clauses wrong.

Two things it cannot answer, so they stay yours.
A **third-party or github.com URL** has no local oracle and is only counted, not resolved.
And `site/` is built from the current tree, so only the **newest** notes file's version is resolvable — links naming an older release are reported as skipped with their count, never quietly passed.
That is also the shape to watch when the docs move on after a tag: the gate then reports a link the frozen published version still serves, and fixing the note is right only if the next release would inherit the same broken link.

The site-side ids for one page, when you want to read them directly:

```bash
grep -oE 'id="[^"]*"' site/operations/upgrade/index.html | sed 's/id="//;s/"//'
```

**Do not hard-wrap**, but do not unwrap the file either, because `md-reflow-check` keeps every tracked markdown file at sentence-per-line and `docs/releases/` is not exempt.
GitHub renders a release body with comment-flavour GFM, where a single newline becomes `<br>`, so the two rules pull in opposite directions and the gate is the one that wins.
`v1.5.0` published with 59 `<br>` for that reason, and the count had grown every release: 20, 22, then 33 in-paragraph breaks at `v1.3.0`, `v1.4.0`, `v1.5.0`.

Render at publish time and both rules hold.
The source keeps its per-sentence diffs; the body reads as paragraphs:

```bash
scripts/release/render-release-body.sh docs/releases/vX.Y.Z.md > tmp/body.md
```

It joins paragraphs, list-item continuations and blockquote bodies, and leaves structure alone: fences, tables, headings, HTML and the bullets themselves come through byte for byte, and a `> [!WARNING]` marker keeps its own line or the alert stops rendering as one.
On `v1.5.0` it takes 59 `<br>` to 0 with the word sequence unchanged.

Check this against the **renderer**, not the source.
`gh release view --json body` returns the raw Markdown, which never contains `<br>` however badly it is wrapped, so grepping that is a check that cannot fail.
Render it the way GitHub will:

```bash
gh api -X POST /markdown -f mode=gfm -f "text=$(cat docs/releases/vX.Y.Z.md)" \
  | grep -c '<br>'
```

`mode=gfm` is the comment flavour; `mode=markdown` is not, and reports 0 on a hard-wrapped file.
The same render confirms the rest of the GitHub-only markup survived — expect one `markdown-alert-*` class per `> [!…]` block, one `<details>` per fold, and no literal `[!` anywhere.

**In-page anchors do not work in a release body.** Release-body headings carry no `id`, so `[Upgrading](#upgrading)` is a dead link.
Refer to a section by name in bold instead.
Verify on a published release rather than trusting this — the page does emit ids, but only on GitHub's own chrome:

```bash
curl -sS https://github.com/<owner>/<repo>/releases/tag/<tag> \
  | grep -oE '<h[1-6][^>]*>' | grep -c 'id='
```

**So a table of contents is not available, and should not be faked.** An unlinked list of section names is dead weight that costs collapsed height against the truncation limit while navigating nothing.
The folds already serve that role: a collapsed `<details>` is a labelled one-line entry, so a body with ten folds reads as an outline whether or not the reader expands any of them.
Navigation comes from section order and the danger banner, not from a ToC.
(The in-repo copy under `docs/releases/` does get GitHub's auto-generated file outline for free, which is a second reason not to hand-roll one.)

**Watch the chart-version form.** Images are tagged `vX.Y.Z`, charts `X.Y.Z`.
A copy-pasteable `helm` command with a `v` in it fails.

**Curating the notes means adding the image digests by hand — after the tag.** The compose step in `publish.yml` writes the five index digests, the verify command, and a changelog, but it runs **only when the tag has no Release yet**.
A curated draft is exactly that condition, so the step logs `already exists; leaving its notes and flags untouched` and the digests never appear.
They also cannot be written in advance: the images are built *from* the tag.

So after `publish.yml` goes green, resolve them and amend the notes file:

```bash
for img in gmc agc proxy worker wrapper; do
  docker buildx imagetools inspect "ghcr.io/actions-gateway/${img}:vX.Y.Z" \
    --format '{{json .Manifest.Digest}}'
done
```

Confirm they are **index** digests, not per-arch manifests — the mediaType must be a manifest list, and the index must carry both `linux/amd64` and `linux/arm64`.
A per-arch digest pins the workload to one architecture and fails to schedule on the other.
This makes the tagged copy of the notes file permanently one section behind the published body, which is intended; `docs/releases/README.md` § Image digests explains why.

**Run the `deslop` skill over the draft before publishing.** Release notes are the most-read prose the project ships.

##### Before publishing: the mechanical checks

Every rule above that can be checked by a machine, in the order they are cheapest to run.
None of these is a substitute for reading the notes — but each one caught a defect in `v1.3.0` that several careful readings had not.

| Check | How | What it catches |
|---|---|---|
| Fold counts | recount every `<summary>` against its bullets, counting the noun the label names | a count that drifted during curation |
| Enumerations | reconcile each surface fold against the tag-to-tag diff **both ways** | a name listed that no longer exists, or shipped and never listed |
| Citations resolve | `gh pr view <n> --json title` for each `#NNNN` | a planning commit cited as the implementation |
| Citations ship | check each cited commit against the **Ships** list from `semver-floor.sh --notes` | a non-product commit listed as a feature |
| Anchors | resolve every site URL against a built `site/`, **with one planted bad anchor** | a heading that moved, and a checker that silently resolves nothing |
| Rendering | `gh api -X POST /markdown -f mode=gfm` | hard-wrapped `<br>`s, alerts that did not render, literal `[!` |
| Published body | re-fetch and `diff` against the file | an edit made on the Release and not in the repo |

Two habits make the difference.
**Plant a known failure in anything that reports "all clear"** — a checker with a broken query and a clean file produce identical output.
And **report the negative when it is true**: "16 metrics added, none removed" and "13 spec fields, nothing removed" are what an operator is actually scanning for, and neither is worth stating unless it was measured.

### 6. Chart version & metadata

The `chart-publish` job sets the published chart's `version` and `appVersion` to the release tag (with the leading `v` stripped, since chart SemVer forbids it), so there is **no manual Chart.yaml version bump** to remember — the in-repo `version`/`appVersion` are dev placeholders the pipeline overrides at package time.
The **prerelease annotation is likewise derived from the tag** now, so nothing here needs a hand-flip.
The remaining items below are one-time setup or guardrails, not per-release steps:

- **Prerelease annotation — derived, not hand-flipped (Q293).** [`Chart.yaml`](../../charts/actions-gateway/Chart.yaml) carries `artifacthub.io/prerelease`, but its committed value is a **dev placeholder**: `publish.yml` overrides it with `yq` before `helm package`, setting `"true"` for a `0.x` or `-rc`/`-alpha`/`-beta` tag and `"false"` for a stable `≥1.0.0` tag (same test that sets the Release's `--prerelease` flag).
  There is **no flip PR** to land before or after a cut.
  The v2 CRD chart is stamped the same way.
- **Artifact Hub listing.** Discoverability metadata (description, keywords, prerelease flag) ships in the chart's own annotations.
  Ownership verification uses [`artifacthub-repo.yml`](../../artifacthub-repo.yml) at the repo root — register the OCI repository in the Artifact Hub control panel, copy the assigned `repositoryID` into that file, and push it to the registry as the repository-metadata OCI artifact (the file's header documents the exact steps).
  This is a one-time control-panel action, not part of `publish.yml`.
- **Empty `values.yaml` digests.** **Do not** commit real `sha256:…` digests into [`values.yaml`](../../charts/actions-gateway/values.yaml).
  The empty `digest` fields are the *secure default*: an unconfigured install fails closed (the GMC rejects floating AGC/proxy tags at startup) until the operator pins a real digest at install time.
  Baking a digest into the shipped chart would defeat that fail-closed posture and immediately go stale.
  The published digests belong in the **release notes** (step 5), which is where the operator copies them from.

### 7. Bump the pinned release in the docs

The adopter-facing pages transcribe the chart version, the image tag, and the release-notes URL by hand.
Nothing in the pipeline rewrites them, so after the tag they still advertise the *previous* release — and a reader following one installs a release behind, digests and all.
A gate runs the bump instead of remembering it:

```bash
make release-pins-check
```

It resolves the newest stable `vX.Y.Z` tag and fails with the file and line of every pin that still names an older one, across the five pages that tell a reader which version to install — `README.md`, `docs/index.md`, and `docs/operations/`'s `install.md`, `upgrade.md`, and `gitops.md`.
Bump each site it reports (charts drop the leading `v`; `gitops.md` carries the Argo `targetRevision` and both Flux forms) and re-run until it is green.

Two things it deliberately leaves alone, both documented alongside the extractor it shares with the published-site check in [`scripts/lib/common.sh`](../../scripts/lib/common.sh): a line beginning `Measured on kind`, whose version records what was actually installed for a measurement — bumping it would falsify the record — and `v2.0.0`, the announced `v1alpha1`/`v2alpha1` removal release.
A page that yields *no* pin at all is a failure rather than a pass, so a pin that moves out of the scan's reach is reported instead of silently going unchecked.

Landing the bump is a normal PR; the gate is part of `make check` and of the `doc-links` CI workflow, so a stale pin reddens every subsequent PR until it is fixed.
This step exists because `v1.3.0` shipped without it: `README.md`, `docs/index.md`, and `install.md` were fixed by hand while `upgrade.md` and `gitops.md` kept pointing at `1.2.0`, and `install.md`'s own patch-line hint still read `1.2.z` (Q638).

The release plan's own row in [`docs/plan/README.md`](../plan/README.md) is the other hand-written claim the tag falsifies, and it is gated the same way:

```bash
make plan-index-check
```

Once `vX.Y.0` is published, that gate rejects an open marker (❌, 🔲, 🚧) on the `release-X.Y.md` row.
Mark it ✅, or ⚠️ if a Queue row genuinely remains, and say what shipped.
This half exists because `v1.3.0` shipped without it too: the 1.3 row read `❌ Open` on `main` for the nine days after the tag, and every rule the gate had at the time was green on it (Q802, Q812).

#### The bump on `main` does not reach the published release

Landing it on `main` fixes `make check` and the `dev` docs, and **nothing else**.
The site builds each version from its tag ([step 2](#2-tag-and-push)), so `/X.Y.Z/` is frozen at what the tag's tree said, which is the *previous* release's pins.
So are `stable` and the root redirect a visitor lands on.
`make release-pins-check` cannot catch this: it reads the working tree, not the published site.
`make verify-published-docs` is the half that reads the site, and it is the last thing this step does.

The two facts are stated separately above and were never reconciled.
Three of the four releases cut since `1.0.0` published the previous version's install command as their landing page.
Measured on the live site 2026-08-10: `/1.1.0/` and `/1.2.0/` both advertise `--version 1.0.0`, and `/1.4.0/` advertised `1.3.0` together with the `v1.3.0` CRD manifest URL, as did `stable` and the root redirect, for the three hours until it was reported.
`v1.3.0` is correct only because Q638's hand-fix happened to land before that tag.

Nor can the bump simply move ahead of the tag: [`check-release-pins.sh`](../../scripts/docs/check-release-pins.sh) compares each pin for **equality** with the newest stable tag, so a pin naming the release about to be cut fails exactly as loudly as one naming the release before it, reddening `make check` and `doc-links` for every PR in the window.
The `GAG_RELEASE_TAG` override is a gate-testing hook, set nowhere in CI.

So the bump lands twice, and the release's own docs are republished from the second copy:

```bash
git switch -c release-X.Y vX.Y.Z
git cherry-pick <the pin-bump commit from main>
./scripts/docs/check-release-pins.sh    # must be green on this branch
git push -u origin release-X.Y
```

```bash
gh workflow run pages --ref main \
  -f version=X.Y.Z -f alias=stable -f set_default=true -f docs_ref=release-X.Y
```

`release-X.Y` is the tag plus the pin bump and nothing else — verify with `git diff --stat vX.Y.Z..release-X.Y` before pushing, because anything else on it publishes as documentation of a release that never shipped it.
Keep the branch: it is the backport line [patch releases](#patch-releases-and-backports) already want, and `GAG_DOCS_SOURCE_REF` points the published version's source links at it.

For a backport patch to an older line, drop `-f alias=stable -f set_default=true`.
Those belong to the highest release, and a dispatch applies them verbatim rather than checking ([pages.yml](../../.github/workflows/pages.yml)).

Then confirm the published pages, not the branch:

```bash
make verify-published-docs VERSION=vX.Y.Z
```

It reads the four site pages that pin a version (the landing page, and `operations/`'s `install`, `upgrade` and `gitops`), plus the `stable` alias and the root redirect, and fails with the page and the version each one actually advertises.
Add `ARGS=--no-stable` for a backport to an older line, whose dispatch above drops the alias.
This is the same comparison [`check-release-pins.sh`](../../scripts/docs/check-release-pins.sh) makes against the working tree, run against what the tree published; the two share one literal extractor so a pin shape the source gate catches cannot slip past this one.

Do not spot-check it with `curl … | grep`: the theme wraps the leading digit of a highlighted version in its own `<span>`, so `--version 1.0.0` is not one string in the served HTML and a grep for it matches nothing, which is exactly what a correct page returns too.
`/1.1.0/` and `/1.2.0/` still advertise `--version 1.0.0` today and both read as clean that way.

### 8. Hand off to operators

Operators install/upgrade straight from the **published OCI chart** with the digests pinned via `--set`, exactly as [install.md § Pin images by digest](install.md#pin-images-by-digest) and [upgrade.md](upgrade.md) document (`X.Y.Z` is the release tag without the `v`):

```bash
helm install gag oci://ghcr.io/actions-gateway/charts/actions-gateway --version X.Y.Z \
  --set gmc.image.digest=sha256:<gmc> \
  --set agc.image.digest=sha256:<agc> \
  --set proxy.image.digest=sha256:<proxy> \
  --set wrapper.image.digest=sha256:<wrapper>
```

## Checks that stay human, and why

Most of this runbook is judgement, and a gate that guessed at judgement would fail good releases until someone switched it off.
The rules below **look** mechanical, were measured against the shipped notes, and were rejected on the evidence.
They are recorded here so the next reader reaches the measurement before rebuilding the check.

- **`<summary>` counts against the list beneath them.** The convention is that an enumerating fold carries a count, so "does the number match the number of bullets" reads like arithmetic.
  It is not.
  `v1.5.0`'s `New condition types (2) and reasons (8)` sits above **four** bullets and is correct: two of them add reasons to conditions that already existed, and the eight reasons are spread across all four.
  Any rule simple enough to state here flags that section, so the gate's first act would be to report correct notes as broken.
  Check counts when curating a list, which is when the list changes and when you can see what it enumerates.

- **`blob/` links.** The rule is real (link the versioned site so a reader lands on *that* release's instructions), but it has a legitimate exception, and `v1.5.0`'s single `blob/` link is it.
  Postmortems are not published to the site, so there is no versioned URL to point at and the source tree is the only target.
  A gate here needs an allowlist, and an allowlist of "documents that exist outside the site" is a second inventory to maintain for one link per release.

- **Whether the notes are truncated on the Releases index.** They always are.
  Every stable release this project has cut sits behind a "Read more", so a gate that failed on truncation would fail every release, and there is no threshold to get under.
  `make release-notes-check` therefore **reports** which sections fall above and below the cut, because that decides what a reader sees before clicking, and the ordering is a judgement call worth making deliberately rather than a rule worth enforcing.

What *is* gated is listed in [scripts/README.md](https://github.com/actions-gateway/github-actions-gateway/blob/main/scripts/README.md)'s `release/` table.
The dividing line is whether a wrong answer is decidable from the file alone: a duplicate `# ` heading is, and whether a caveat is a landmine is not.

## Patch releases and backports

A patch release (`vX.Y.Z+1`) is **bugfix-only** by SemVer.
That has a release- engineering consequence: **do not tag a patch from `main` once `main` has merged features headed for the next minor** — doing so ships those unreleased features in the patch's images *and* chart, and advertises them in the patch's docs (the site builds each version from its tag).
Tag a patch from the released line instead:

- **Ephemeral branch off the tag (branchless-friendly).** For a one-off patch: `git switch --detach vX.Y.Z`, `git switch -c release-X.Y`, cherry-pick the fix, tag `vX.Y.(Z+1)`, push the tag.
  Delete the branch afterward if you don't maintain the line.
- **Long-lived release branch.** If you support multiple minor lines at once, keep a `release-X.Y` branch at the minor's tag and land backported fixes on it.

You only need a branch/backport **when `main` isn't itself the intended patch** — i.e. when it has diverged past the release with content you don't want in the patch.
If `main` is clean and ready to ship, that's the next **minor** (`vX.(Y+1).0`), not a patch.

> **A release line carries its own validation harness.** `validate-release.sh` and the dogfood scripts under it live in this repo, and the gate runs *the scripts in your checkout* against *the artifacts of the tag*.
> A `release-X.Y` branch cut from `vX.Y.0` therefore inherits whatever harness that commit had — so a fix to the gate only reaches a patch line if it was in the minor tag, or is cherry-picked like any other fix.
>
> The consequence is a rule for the **minor** cut, not the patch: tag `vX.Y.0` from a commit that carries the harness fixes you want the line to keep.
> Cutting it from an older RC's commit to make the tag byte-match the validated artifact is the trap — it strands every gate fix found *during* that release's validation, which is exactly when gate fixes tend to be found.
> `v1.3.0` is the worked example: rc.5's validation produced two harness fixes ([Q627](../plan/release-1.3.md#Q627) and the e2e watch deadline), and a `release-1.3` branch cut from rc.5's commit would reproduce both on every future `v1.3.x`.
>
> Validating an RC whose product code matches `main` is unaffected: the images and CRDs come from the tag, the harness from your checkout, so the two move independently by design.

The **docs site follows automatically**: `mike` builds each version from its own tag, so a patch tagged off the release line publishes docs with only that line's content — no unreleased features.
And the site's `stable` alias + default root redirect move **only to the highest released version**, so a backport patch released *after* a newer minor updates its own version's docs without demoting the site (see [website.md § Versioned deploy](../development/website.md#versioned-deploy-mike)).
No docs-specific branch is ever required beyond what the release itself needs.

## Rollback

A release is just a tag and a set of immutable, digest-addressed images — nothing is destructive.
To roll an *installed* release back, re-pin the previous digests and `helm rollback`/`helm upgrade`; the procedure and post-rollback validation are in [upgrade.md](upgrade.md).
A bad tag can be superseded by a higher patch release; do not retag an existing `vX.Y.Z` (it would break the digest↔tag binding consumers rely on).

## The worker images: `wrapper` and `worker`

`publish.yml` builds and signs **two** worker-related images, both holding the same `cmd/worker` wrapper that feeds the job payload into `Runner.Worker`:

- **`ghcr.io/actions-gateway/wrapper`** — a ~2 MB `FROM scratch` image with just the wrapper binary.
  The GMC forwards it to every AGC (`WRAPPER_IMAGE`), whose provisioner injects it into each worker pod — as a read-only OCI image volume (K8s ≥ 1.33) or via an initContainer below that — so the runner container can be the **unmodified upstream `ghcr.io/actions/actions-runner`** (or any tenant `workerImage`).
  This is what makes `DefaultWorkerImage` (still the digest-pinned upstream `actions-runner`, runner version locked to `agent.version` — see [building.md](../development/building.md#runner-version-pin-lockstep)) actually run jobs (Q235, [plan](../plan/archive/q235-worker-wrapper-injection.md)).
- **`ghcr.io/actions-gateway/worker`** — the full upstream `actions-runner` + the wrapper as `ENTRYPOINT` (~520 MB).
  Kept as an optional batteries-included image; unnecessary once injection is enabled, since the runner image is the upstream one with the wrapper injected.
  Retiring it is tracked separately.

Only `wrapper` is digest-pinned in the chart (`wrapper.image.digest`, like `agc`/`proxy`), so a release must publish the `wrapper` image and pin its digest for the default install to run jobs.
The `worker` image has no chart `image` block — it is the optional batteries-included image a tenant opts into via its per-RunnerGroup `workerImage`, not a chart-provisioned one — so nothing in the chart pins it.

## PR CI vs publish — what runs where

| Stage | Build image | Generate SBOM | Push to GHCR | Sign + SBOM-attest | Provenance attest |
|---|---|---|---|---|---|
| Pull request (`security-scan.yml`) | ✅ | ✅ (artifact) | — | — | — |
| Release tag (`publish.yml`) | ✅ | ✅ (attached) | ✅ | ✅ keyless | ✅ keyless (SLSA L2) |

PR CI proves the image builds and the SBOM generates so those paths can't silently break; signing, SBOM attestation, and build-provenance attestation are all first exercised on a real `v*` tag, which is why step 3 verification matters on every release.

`publish.yml` also runs one **pre-publish gate**, `announce-bar`, that every publishing job depends on.
It builds the docs site at the tag and asserts the rendered banner names it (see [Pre-flight](#1-pre-flight)), so a docs-site banner advertising the wrong version stops the release before an image, chart, or GitHub Release exists, rather than after.

## Supply-chain integrity of the pipeline itself

The publish job holds `packages: write` + `id-token: write` + `attestations: write`: its ambient OIDC identity *is* the release trust root.
A hijacked upstream action tag executing in that job could push and keyless-sign malicious images as the legitimate publish identity.
Three controls keep the pipeline itself trustworthy.

### Actions are pinned to full commit SHAs

Every `uses:` in the repo is pinned to a full 40-char commit SHA with a trailing `# vX.Y.Z` comment — never a floating tag (`@v4`) or branch.
A tag is mutable: whoever controls the upstream repo can repoint it at new code, which would then run inside the privileged publish job.
A SHA is immutable.
The comment is not decoration: it is what Dependabot reads to know which release a SHA is, so a pin without one is a pin nothing will bump.

`make uses-pinned-check` enforces both halves, and the CI `uses-pinned` job (`unit-test.yml`) runs it on any workflow change.
Exempt shapes, because neither is a mutable third-party reference: a local `./…` action or reusable workflow, which is in-tree code the same PR reviews, and a `docker://` image, which must carry an `@sha256:` digest instead.
An unparseable workflow fails the gate rather than being skipped.

The runtime tool downloads in the publish path are pinned the same way: `cosign` via `sigstore/cosign-installer` with an explicit `cosign-release` (kept in step with `COSIGN_VERSION` in the Makefile so a local `make verify-release` uses the same version that signed), and `syft` by `SYFT_VERSION` plus the `SYFT_SHA256` of its release tarball, downloaded through [`scripts/fetch/download-verified.sh`](../../scripts/fetch/download-verified.sh).
That download used to go through `anchore/sbom-action/download-syft`, which fetches anchore's `install.sh` and runs it with no retry, so one transient CDN denial failed the step outright (Q806).
The helper retries and refuses to write bytes that miss the pinned digest, so absorbing the flake tightened the pin rather than loosening it: the release now names the exact syft bytes instead of trusting an installer to resolve them.

**Bumping a pinned action.** Dependabot's `github-actions` ecosystem ([`.github/dependabot.yml`](../../.github/dependabot.yml)) opens weekly PRs that bump both the SHA *and* the `# vX.Y.Z` comment, so the pins don't rot — review and merge those like any dependency PR.
To pin or bump by hand, resolve the tag to its commit SHA and keep the comment in sync:

```bash
gh api repos/<owner>/<action>/commits/<tag> --jq '.sha'
# -> uses: <owner>/<action>@<sha> # <tag>
```

`SYFT_VERSION`/`SYFT_SHA256` are **not** Dependabot-managed (a tool download, not an action ref).
[`updatecli.d/syft.yaml`](../../updatecli.d/syft.yaml) bumps the pair weekly, in `publish.yml` and `security-scan.yml` together, so the released images and the PR-time scan are described by the same syft.

The two workflow gates divide the work, and it is worth knowing which answers what.
`actionlint` checks that a `uses:` ref is present and well formed, so a pin edited down to a bare `owner/repo` fails the PR.
But measured against v1.7.12, the version `tools/` pins, it accepts `@v7.0.1`, `@v4` and `@main` at exit 0, resolving the action's inputs off the tag while doing so.
It reads the ref and never asserts it is a SHA.
`uses-pinned-check` is the gate for that, and its scope is wider: actionlint lints only the root `.github/workflows/` directory, while the pin gate covers every workflow and composite action in the tree, including `cmd/gmc/.github/workflows/`: kubebuilder scaffolding GitHub never runs, whose refs no gate had read before.

### Signing identity is tags-only

Releases are cut by pushing a `v*` tag, so a legitimate keyless signature's Fulcio certificate records `publish.yml` running from `refs/tags/vX.Y.Z`.
Two layers enforce that a signature can only ever be a tag signature:

- **publish.yml refuses to run from a non-tag ref.** Both publish jobs' "Resolve publish tag" step rejects any `GITHUB_REF` that isn't `refs/tags/…`, so a `workflow_dispatch` run from a branch can't even reach the sign step.
- **`make verify-release` only accepts a tags identity.** The `--certificate-identity-regexp` is anchored to `…/publish\.yml@refs/tags/v.*$` (sourced from `release_identity_regexp` in [`scripts/lib/common.sh`](../../scripts/lib/common.sh)), so a signature minted from `refs/heads/…` is rejected even if one were somehow produced.
  The `scripts/release/verify-release-test.sh` assertions (run by `make check` and CI) guard that the regexp stays tags-only.

Together these close the hole where repo-write could dispatch `publish.yml` from a scratch branch, overwrite a released GHCR version tag, and still pass verification.

### Build inputs and the signer binary are integrity-checked

The first two controls protect *who* runs the pipeline and *how* signatures are trusted; this one protects *what goes into* the signed artifacts and *the tool that verifies them*.

- **Vendored dependencies are gated against `go.sum`.** Images build with `go build -mod=vendor`, but `-mod=vendor` only checks `vendor/modules.txt` consistency — it never verifies that the vendored *source* matches the hashes in `go.sum`.
  A malicious or accidental edit under `vendor/` (or `tools/vendor/`) would otherwise compile straight into the signed release images.
  The `vendor-check` job (in `unit-test.yml`, single source of truth `make vendor-check` → `scripts/go/vendor-check.sh`) re-runs the workspace-vendor flow — which re-fetches every module verified against `go.sum` — and fails on any diff against the committed trees.
  A Dependabot `go.mod` bump legitimately fails this gate until a follow-up vendor sync lands; that is the intended signal (see [go-workspaces.md § Changing dependencies](../development/go-workspaces.md#changing-dependencies)).
- **The cosign verify binary is checksummed.** GitHub release assets are mutable for an existing tag, so a raw download of the release verifier can't be trusted on its own.
  The publish pipeline obtains cosign via the SHA-pinned `sigstore/cosign-installer` action (which performs its own signature verification); the *local* verify path (`make verify-release` → `scripts/release/download-cosign.sh`) pins the expected SHA256 per platform in-repo and refuses to install a binary whose bytes don't match.
  Bumping `COSIGN_VERSION` must add the new digests to that script (it fails closed on an unpinned version) — the same deliberate-pin discipline as `KIND_BINARY_SHA256` in `e2e-test.yml`.
