---
name: release-candidate
description: Cut and validate a release candidate end to end, then stop and ask whether to promote it. Use when asked for a "new release candidate", to "cut an rc", "cut rc.N", "tag a release candidate", or to validate or promote one. Runs the pre-flight checks that bind at a candidate, cuts the tag, verifies the publish and its signatures, runs the dogfood validation, and then halts: green hands the promote decision back with the evidence, red reports action items. Never tags a stable release on its own.
---

# Cut a release candidate

[`docs/operations/release.md`](../../../docs/operations/release.md) is the procedure and stays canonical.
This skill is the **sequence, the stopping points, and the traps that have actually cost a candidate**.
Follow it and read the runbook for each step's detail.

Two rules govern the whole run:

- **Every "it passed" is a claim.** Reconcile status against output, and check the SHA rather than the lane.
  Details below, each one paid for.
- **You cut and validate candidates.
  You never tag a stable release.** Promotion is the user's call and the skill ends by asking for it.

## 1. Establish the target, then check that target

Resolve the tag target once, as a SHA, and use it for every check afterwards:

```bash
git fetch origin main && git rev-parse origin/main
```

```bash
scripts/release/check-gates-green.sh origin/main
```

**It asks by commit, not by branch, and that is the point.** `gh run list --branch main` excludes merge-queue runs, and the queue is where a commit is validated before it lands, while the `push`-lane run for the same commit sits behind the previous push's concurrency group reading `pending`.
Querying the branch says "not validated" about a commit that is.
The script passes a gate when *some* lane ran it in full on the SHA, and names the lane that answered.

**Expect `SKIPPED`, and do not read it as either green or red.** It is the ordinary shape of a release tip: docs-only merges sit on top of the last code change, so the heavy jobs path-gate themselves away.
Every one of the nine required workflows had skipped its real job on the commit `v1.5.0` was tagged at, and the release was still sound, because the code was the same tree an earlier commit had validated.
Prove exactly that rather than re-running anything:

```bash
scripts/release/check-artifact-unchanged.sh <last-fully-validated-sha> origin/main
```

Exit 0 means nothing on the released surface moved, so the earlier full run still covers it.
Say which commit you are relying on.

A `NOT GREEN` line is the different answer: a lane failed, or none reported at all.
That one blocks the tag.

Then confirm no gating row remains, and run the local gate **from a tree that matches the target**, a branch cut from `origin/main`, since a worktree cannot check out `main`:

```bash
grep -rlE '^[[:space:]]*-[[:space:]]*<X\.Y>-gate[[:space:]]*$' docs/queue/Q*.md   # want no output
make check > tmp/check-rc.log 2>&1; echo "EXIT=$?"
```

Running `make check` on a stale branch validates the wrong tree and tells you nothing.

**The gating-row check names the item files explicitly, and that is the point.** A gate label is a frontmatter list entry on a row, so anchoring to that line keeps `docs/queue/README.md`'s label glossary from matching every release at once.
Naming `docs/queue/Q*.md` also makes a wrong path fail loudly instead of quietly: this check used to read `docs/STATUS.md`, which the item-store migration removed, and `git show` then exited 128 while a `grep -c` over its error text printed `0`.
Zero is the reassuring answer here, so the check confirmed a clean release from a file that no longer existed.
Confirm it can still find one before trusting a clean result, e.g. `1.7-gate` matches `Q408.md` today.

## 2. Re-run any pre-flight verdict whose scope has moved

The runbook records each pre-flight verdict in the release plan doc, and **a verdict covers the commit it was measured at**.
If scope reopened after a verdict was written, that verdict is stale by default.

Check the recorded window against the tag target before trusting it.
In the 1.5 cycle the API surface review had been recorded over an earlier window, scope reopened on eight rows, and one of them published a new condition type, two reasons and two metrics that no review had seen.
The marketing reconciliation from the same day was *not* re-checked, and a shipped cross-tenant guard reached no marketing surface at all.

At a candidate, these bind:

- **`main` green and the version chosen**: every tag.
- **API surface review**: pulled forward, because a rename decided after publication costs a new candidate.
- **The notes draft, interrogated** through `verify-claims`: the discovery step, not a formatting pass.
  Ask what each claim rests on; re-derive every number rather than copying it forward.
  Every wrong figure in the 1.5 notes was a *derived* value gone stale, not a typo.
  The other three prose passes (`readability`, `deslop`, `semantic-remediation`) bind at the stable tag, when the text publishes.

The rest are stable-tag obligations.
Say which you are deferring and why.

## 3. Cut the tag, and check where it points before pushing

```bash
git tag -a vX.Y.Z-rc.N origin/main -m "Release vX.Y.Z-rc.N"
git rev-parse "vX.Y.Z-rc.N^{commit}" && git rev-parse origin/main   # must match
git push origin vX.Y.Z-rc.N
```

**The comparison is not ceremony.** Creating and pushing a tag are two moments, and a leftover tag from an earlier attempt makes `git tag -a` fail so an `&&` chain skips the push, while a bare `git push` sends the stale one.
`v1.5.0-rc.2` published from a commit three merges behind exactly this way ([postmortem](../../../docs/postmortems/2026-08-15-rc2-tagged-a-stale-commit.md)).

A published Release is immutable: **the tag is locked to its commit and cannot be moved or deleted**.
A mis-tag is not repairable: it costs the candidate number.

If a guard denies the push, hand the command to the user rather than working around it, and say the tag already exists locally so they do not create a second one.

## 4. Verify the publish by content

Green jobs are not the check.
A published Release with no assets also reports five green image jobs.

```bash
gh api repos/<owner>/<repo>/releases/tags/vX.Y.Z-rc.N \
  --jq '{draft, prerelease, immutable, asset_count: (.assets|length)}'
```

Want `draft: false` and the full asset count (**9** for this repo: CRD manifest + bundle, `SHA256SUMS` + bundle, five `gag-migrate` binaries).
A **draft** left behind means the run died before its final step.
Assets are attached to a draft and the last step publishes it, because publishing seals an immutable Release.

Then the signatures, and read the output rather than the exit code:

```bash
make verify-release VERSION=vX.Y.Z-rc.N > tmp/verify.log 2>&1; echo "EXIT=$?"
```

**The check that discriminates is the provenance digest**, not the signatures.
Signatures prove who built it; only the digest proves *what* was built, and it is the one check that would have caught the stale-commit candidate while all seven signatures passed:

```bash
gh attestation verify oci://<image>:vX.Y.Z-rc.N --repo <owner>/<repo> \
  --signer-workflow <owner>/<repo>/.github/workflows/publish.yml --format json > tmp/prov.json
jq -r '.[0].verificationResult.signature.certificate
       | "\(.buildSignerURI)\n\(.sourceRepositoryDigest)"' tmp/prov.json
```

`gh attestation verify` **prints nothing when redirected**, so exit 0 and a silent no-op are indistinguishable.
Read the JSON.
The URI must end `publish.yml@refs/tags/<this tag>` and the digest must equal the target SHA.

Optional and worth it: `cosign verify-blob` on the CRD manifest, and an SBOM attestation against a **per-arch** digest (SBOMs bind per-arch, provenance binds to the index, and swapping them looks like a missing attestation).
Re-run each against a deliberately wrong identity and require failure; a check that cannot fail proves nothing.

## 5. Validate on dogfood

Confirm the cluster is at rest before spending, which also proves the target you resolved:

```bash
PROJECT=… CLUSTER=… ZONE=… scripts/dogfood/ops.sh at-rest
```

Then the gate, backgrounded, with a 60-minute Bash timeout:

```bash
ASSUME_YES=1 PROJECT=… CLUSTER=… ZONE=… REPO=… \
  scripts/agent/record-launch.sh scripts/dogfood/validate-release.sh vX.Y.Z-rc.N > tmp/validate.log 2>&1
```

`ASSUME_YES=1` is required when detaching and it **skips the gate's own target confirmation**, so verify the target yourself first, as above, and say you did.

**The four values are in [gke-dogfood.md § Variables](../../../docs/plan/gke-dogfood.md#variables)** — `actions-gateway-dogfood`, `gag-dogfood`, `us-east1-b`, `actions-gateway/github-actions-gateway`.
They are not secrets.
Substitute them rather than asking for them.

**A harness refusal on this launch is not a decision the user has to make, and relaying it as one is the defect.** The identical command was permitted for one candidate and refused for the next a few hours later on 2026-08-29.
Auto mode classifies with the session transcript as part of its input, so the verdict tracks the conversation rather than the command, and a session that has accumulated refusals and workaround talk draws more of them: 0 refusals in that session's first 77 tool calls, 6 in the 12 that followed the first one.
Retry the plain form once.
If it is refused again, check the transcripts under `~/.claude/projects/` (read-exempt) for a session that already ran it, which separates *was refused* from *was never tried* — a `tool_use` whose `input.command` names the script, and the `tool_result` after it.
Hand back only after that, and say which of the two you established.

The teardown trap covers normal exits and SIGTERM but not `kill -9` or a killed parent, which leave billable nodes up.
If the run dies hard, tell the user the reclaim command:

```bash
PROJECT=… CLUSTER=… ZONE=… REPO=… scripts/dogfood/validate-release.sh --reclaim
```

Confirm the cluster returned to 0 nodes by **asking the cluster** (`ops.sh at-rest`), not by reading the gate's own teardown line.

## 6. Stop and hand back the decision

Record the verdict in the release plan doc either way, then:

### What may land between a validated candidate and the stable tag

A candidate's validation covers the tree it was cut from, so anything merged after it that moves an artifact makes the verdict cover something other than what ships.
That is how `v1.5.0-rc.1` was superseded: it validated, eight rows merged on top, and its verdict stopped describing `main`.

**The rule is byte-identical artifacts, not "docs only".** Documentation changes are expected in this window and cannot be avoided, because the validation verdict does not exist until the candidate is tagged, so the notes' Validation section is necessarily written afterwards.
But the labels do not line up: `charts/actions-gateway/README.md` is a markdown file that **ships inside the chart tarball**, so a pure-docs pull request can change a published chart's bytes.

Check rather than classify, before tagging the stable release:

```bash
scripts/release/check-artifact-unchanged.sh <validated-candidate-sha> origin/main
```

Exit 1 means the stable tag would ship something no candidate validated: revert it, or cut and validate a new candidate.

**Green.** Report the evidence (e2e counts, sizing legs, CRD smoke, the artifact checks) and **ask whether to promote**.
Do not tag a stable release, and do not treat the question as rhetorical.
Name what still binds at the stable tag but not at a candidate: the marketing reconciliation, the operator-caveat pass, the roadmap and `features.md` reconciliation, the announce-bar highlight, and substituting this run's verdict for the previous candidate's in the notes' Validation section.
If the answer is yes, the [`release`](../release/SKILL.md) skill carries those out; this one ends here.

**Red.** Report what failed, and separate the two questions that look alike:

- Was the failure the **candidate's code**?
  Then it needs a fix and a new candidate number.
- Was it the **gate or the pipeline**?
  Then the candidate may still be sound.
  Two of the three failures in the 1.5 cycle were the release tooling itself, not the product.

Propose action items in that split, with the mitigative fix and the preventative one named separately, and let the user choose.
A failed candidate is also a retro trigger.
