---
name: release
description: Promote a validated release candidate to a stable tag and finish the release. Use when asked to "tag the release", "promote the candidate", "ship vX.Y.Z", "cut the stable release", or to finish or complete a release already tagged. Covers the pre-flight that binds only at a stable tag, the freeze check, tagging, publish verification, and every post-tag step through to operator hand-off. Expects a candidate that has already passed dogfood validation, which the release-candidate skill produces.
---

# Promote a candidate to a stable release

[`docs/operations/release.md`](../../../docs/operations/release.md) is the procedure and stays canonical.
This skill is the order, the checks that discriminate, and the traps that have cost a release.

Start from a candidate the `release-candidate` skill has validated.
If none has passed, stop and cut one: a stable tag is not the place to discover a defect, and it cannot be moved once published.

**A stable tag does more than a candidate.** It publishes the docs site as a new version, moves the `stable` alias and the root redirect, and arms the released-chart e2e gate that every future PR pulls from.
A broken chart publish on a prerelease affects nobody; on a stable tag it reddens everyone's CI until it is repaired.

## 1. The pre-flight that binds only here

A candidate defers these because a prerelease deploys no docs and its Release body is generated.
At a stable tag they publish, so run each and record the verdict in the release plan doc.

**Re-run any verdict recorded before the scope moved.** A recorded verdict reads as done long after the thing it checked has changed; that is how the 1.5 marketing reconciliation missed a shipped cross-tenant guard that reached no surface at all.

- **Marketing reconciliation** across `docs/index.md`, `why-gag.md`, `README.md`, `features.md`.
  Ask what shipped that no surface mentions, then whether every claim still describes the product, then whether every competitor claim still holds.
  `make comparison-stamps-check` gates the last one's stamps, not their truth.
- **Operator caveats**: `scripts/release/operator-caveats-since.sh <last-tag>`.
  Read the **new sections** rather than the bullet count, and confirm each is in the notes.
- **Roadmap and `features.md`** against `docs/STATUS.md`: `make roadmap-check`.
  Work that shipped this cycle needs a `features.md` line.
- **Announce bar**: `overrides/main.html`'s `highlight_for` names this version.
  `publish.yml` fails the release if the rendered banner does not.
- **The notes' Validation section names the candidate that actually passed**, not an earlier one.

## 2. Check the freeze before tagging

A candidate's validation covers the tree it was cut from.
Anything merged after it that moves an artifact makes the verdict describe something other than what ships.

```bash
scripts/release/check-artifact-unchanged.sh <validated-candidate-sha> origin/main
```

**The rule is byte-identical artifacts, not "docs only".** Doc changes are unavoidable here, because the validation verdict does not exist until the candidate is tagged, so the notes' Validation section is necessarily written afterwards.
But the labels do not line up: `charts/actions-gateway/README.md` is markdown that ships inside the chart tarball.

Exit 1 means the tag would ship something no candidate validated.
Revert it, or cut and validate another candidate.

## 3. Bump the pins, then tag

**The pin bump lands before the tag**, because the site builds each version from its own tag and a bump landing afterwards never reaches that release's page.
`check-release-pins.sh` accepts a release that has a candidate and no stable tag yet, which is what makes early pinning possible.

```bash
make release-pins-check
```

It accepts the *current* release too, so green is not evidence the bump landed.
Read the version it prints.

Then tag, and check where the tag points before pushing:

```bash
git tag -a vX.Y.Z origin/main -m "Release vX.Y.Z"
git rev-parse "vX.Y.Z^{commit}" && git rev-parse origin/main   # must match
git push origin vX.Y.Z
```

Creating and pushing are two moments.
A leftover tag from an earlier attempt makes `git tag -a` fail so an `&&` chain skips the push, while a bare `git push` sends the stale one, and a published Release **locks its tag to one commit**.
`v1.5.0-rc.2` was lost exactly this way ([postmortem](../../../docs/postmortems/2026-08-15-rc2-tagged-a-stale-commit.md)).

If a guard denies the push, hand the command over and say the tag already exists locally so nobody creates a second.

## 4. Verify what published

Green jobs are not the check: a Release with zero assets still shows five green image jobs.

```bash
gh api repos/<owner>/<repo>/releases/tags/vX.Y.Z --jq '{draft, prerelease, asset_count: (.assets|length)}'
make verify-release VERSION=vX.Y.Z > tmp/verify.log 2>&1; echo "EXIT=$?"
```

Want `draft: false`, `prerelease: false`, and the full asset count.

**The provenance digest is the check that discriminates.** Signatures prove who built it; only the digest proves *what* was built, and it is the one check that would have caught the stale-commit candidate while all seven signatures passed.
`gh attestation verify` prints nothing when redirected, so read `--format json` and compare `sourceRepositoryDigest` against the tagged commit.

`make_latest` is a write parameter GitHub does not echo back, so a `null` there proves nothing.
Ask `/releases/latest` which tag it names.

## 5. Finish the notes, then the site

**The `Container images` section can only be written now**, because the index digests do not exist until the pipeline has run.
Resolve them from the registry rather than transcribing:

```bash
docker buildx imagetools inspect ghcr.io/<owner>/<image>:vX.Y.Z --format '{{.Manifest.Digest}}'
```

These are index digests, one ref serving both architectures.
SBOM attestations bind to **per-arch** digests instead, so swapping them looks like a missing attestation.

Land that in `docs/releases/vX.Y.Z.md`, then reconcile it against what actually published rather than re-reading what you typed:

```bash
scripts/release/check-release-digests.sh vX.Y.Z
```

**Four prose passes over the draft before it publishes**, in order: `verify-claims`, `readability`, `deslop`, then `semantic-remediation` last, since the editing passes are what introduce the defects it looks for.
[release.md § The prose passes](../../../docs/operations/release.md#the-prose-passes) says what each one owns.

Then publish the body.
This works because immutability freezes assets and the tag but leaves title and notes editable:

```bash
gh release edit vX.Y.Z --notes-file <(scripts/release/render-release-body.sh docs/releases/vX.Y.Z.md)
```

**Publish through the renderer, never the file.** `md-reflow-check` keeps the note at sentence-per-line, and comment-flavour GFM turns each of those newlines into a `<br>`: `v1.5.0` published with 59 of them.
The renderer joins paragraphs, list continuations and quote bodies and leaves structure alone.
`make release-notes-check` covers the rest of what a machine can settle here; [release.md § Checks that stay human](../../../docs/operations/release.md#checks-that-stay-human-and-why) records what was measured and deliberately left to you.

**Then republish the version's docs.** Landing the pin bump on `main` fixes `make check` and the `dev` docs and nothing else, so this step is skipped only when step 3's bump made it into the tag's own tree:

```bash
git switch -c release-X.Y vX.Y.Z
git cherry-pick <the pin-bump commit from main>
git diff --stat vX.Y.Z..release-X.Y     # the bump and nothing else
git push -u origin release-X.Y
gh workflow run pages --ref main -f version=X.Y.Z -f alias=stable -f set_default=true -f docs_ref=release-X.Y
```

Anything else on that branch publishes as documentation of a release that never shipped.
Keep the branch: it is the backport line patch releases want.

```bash
make verify-published-docs VERSION=vX.Y.Z
```

This reads the **live site**, which `release-pins-check` cannot: that one reads the working tree.
Three of the four releases before 1.5.0 published the previous version's install command as their landing page, and no working-tree gate saw it.

## 6. Expect `main` to go red, and clear it

Tagging arms two gates immediately, and they fail every PR in the repo until fixed:

- `release-pins-check`, unless step 3's bump already landed
- `plan-index-check`, because the release's plan row still reads open while the tag says shipped

Neither is a missed step.
The pin gate compares against the newest stable tag, so it cannot be satisfied before that tag exists; the plan index states the rule outright, that the tag is the fact and the cell is the claim.

Mark the release row shipped, naming the tag and the candidate that validated it.

## 7. Hand off, and close the loop

Operators install from the published OCI chart with the digests pinned by `--set` ([install.md](../../../docs/operations/install.md)).
The digests from step 5 are the input.

Then report what shipped, with the evidence rather than an assertion: the validation legs, the artifact checks, and the published-docs verdict.
Offer a retrospective — a release that took more than one candidate has earned one, and the findings that matter are the ones nothing gated.
