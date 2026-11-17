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

Then the [vulnerability policy](../../../docs/operations/release.md#the-vulnerability-policy), against the same SHA:

```bash
scripts/release/check-vuln-policy.sh origin/main
```

**Green gates are not this check.** They hold the scan that ran when the commit merged, and an advisory published since then does not reach them; this one wants a scan of the SHA from the last 24 hours.
A tip whose last code merge was days ago reads `fresh scan FAIL`: re-ask it with `gh workflow run security-scan.yml --ref main` and re-run once it completes.
Keep the inherited totals and excused CVEs it prints for the notes' Security section.

Then confirm no gating row remains, and run the local gate **from a tree that matches the target**, a branch cut from `origin/main`, since a worktree cannot check out `main`:

```bash
grep -rlE '^[[:space:]]*-[[:space:]]*<X\.Y>-gate[[:space:]]*$' docs/queue/Q*.md   # want no output
make check > tmp/check-rc.log 2>&1; echo "EXIT=$?"
```

Running `make check` on a stale branch validates the wrong tree and tells you nothing.

**The gating-row check names the item files explicitly, and that is the point.** A gate label is a frontmatter list entry on a row, so anchoring to that line keeps `docs/queue/README.md`'s label glossary from matching every release at once.
Naming `docs/queue/Q*.md` also makes a wrong path fail loudly instead of quietly: this check used to read `docs/STATUS.md`, which the item-store migration removed, and `git show` then exited 128 while a `grep -c` over its error text printed `0`.
Zero is the reassuring answer here, so the check confirmed a clean release from a file that no longer existed.
Confirm it can still find one before trusting a clean result: point the same pattern at a gate label some open row carries today and require a match.

## 2. Re-run any pre-flight verdict whose scope has moved

The runbook records each pre-flight verdict in the release plan doc, and **a verdict covers the commit it was measured at**.
If scope reopened after a verdict was written, that verdict is stale by default.

Check the recorded window against the tag target before trusting it.
In the 1.5 cycle the API surface review had been recorded over an earlier window, scope reopened on eight rows, and one of them published a new condition type, two reasons and two metrics that no review had seen.
The marketing reconciliation from the same day was *not* re-checked, and a shipped cross-tenant guard reached no marketing surface at all.

Before the cut, the ones whose findings change an artifact:

- **`main` green, the vulnerability policy met, and the version chosen**: every tag.
- **API surface review**: a rename decided after publication costs a new candidate.
- **Both chart READMEs against the tree being tagged** (`charts/actions-gateway/README.md`, `charts/actions-gateway-crds-v2/README.md`): they ship inside the tarballs, so a fix after the cut costs a candidate.
  Every `values.yaml` key has a table row, and the install and upgrade commands, the Kubernetes floor, and links into `docs/operations/` still hold.
  No gate checks the table yet, and none can check the prose; at `v2.0.0-rc.1`'s target six top-level keys had no row.
- **The notes draft, interrogated** through `verify-claims`: the discovery step, not a formatting pass.
  Ask what each claim rests on; re-derive every number rather than copying it forward.
  Every wrong figure in the 1.5 notes was a *derived* value gone stale, not a typo.

The docs-only rest wait for step 5, where the validation run leaves the time idle.
On a later candidate, re-read each recorded verdict from its commit rather than redoing it.

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
make verify-release-compat VERSION=vX.Y.Z-rc.N > tmp/verify-compat.log 2>&1; echo "EXIT=$?"
```

The second line verifies with cosign v2, which operators run; the first uses the signer's own cosign and passes a candidate only v3 can read.
A compat failure blocks the promote.
Both check the image signatures, every per-arch SBOM attestation, both charts, and the Release's blob bundles.

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

Optional and worth it: re-run one `cosign verify` against a deliberately wrong identity and require failure; a check that cannot fail proves nothing.

## 5. Validate on dogfood, in CI

**Pushing the tag already started the gate, and only that run counts.** `validate-candidate.yml` dispatches the gate on `main` for the tag, and `publish.yml` refuses a stable tag unless CI runs of it have passed every leg for one candidate commit (Q880).
A local gate run records nothing a stable tag can use.
`v1.10.0-rc.1` and `rc.2` were each run locally from this step's old text, about 80 minutes of dogfood apiece, while rc.1's CI run waited eight hours for an approval nobody had been asked for.

Find the dispatched run, and confirm the cluster is at rest before anyone approves it:

```bash
gh run list --workflow validate-candidate.yml --limit 4 \
  --json databaseId,status,displayTitle --jq '.[]|"\(.databaseId) \(.status) \(.displayTitle)"'
PROJECT=… CLUSTER=… ZONE=… scripts/dogfood/ops.sh at-rest
```

**The four values are in [gke-dogfood.md § Variables](../../../docs/plan/gke-dogfood.md#variables)** — `actions-gateway-dogfood`, `gag-dogfood`, `us-east1-b`, `actions-gateway/github-actions-gateway`.
They are not secrets.

**A superseded candidate's run blocks the next one.** The runs share the `dogfood-release-gate` concurrency group without cancelling, so a newer candidate's run sits `pending` behind an older one `waiting` for approval.
Cancel the superseded run (`gh run cancel <id>`); its candidate is not going to ship.

**The approval is the user's, so ask for it and then check it arrived.** The run waits in the `dogfood-validation` environment: open the run, choose **Review deployments**, tick the environment, then **Approve and deploy**.
Never approve it yourself: `gh` here acts as the maintainer's account, so an agent's approval cannot be told apart from theirs and empties the gate.
Confirm the approval registered before reporting the run as started, since a first attempt on 2026-10-26 recorded nothing:

```bash
gh api repos/<owner>/<repo>/actions/runs/<id>/approvals --jq '.[]|"\(.state) \(.user.login)"'
```

Watch the run to a terminal state with Monitor, never a Bash poll.
Then read the verdict from its evidence artifact, not from the run's colour:

```bash
gh run download <id> --pattern 'gate-evidence-*' --dir tmp/evidence
```

It names the tag, the commit and one `leg <name>` line per leg that passed; a failed run still uploads the legs that passed before it failed.
Want the commit to equal the tag target and every leg in `scripts/dogfood/lib/gate-legs.sh` present.
`scripts/release/check-validated-candidate.sh vX.Y.Z` answers the same question across runs, but only once the stable tag exists.

### While the gate runs, do the stable tag's docs work

The approval wait and ~80 minutes of dogfood are idle time, so spend them on the first candidate's docs-only pre-flight: the marketing reconciliation's first two questions, `operator-caveats-since.sh`, the roadmap and `features.md` reconciliation, the announce-bar highlight, and the notes' `readability`, `deslop` and `semantic-remediation` passes.
Record each verdict in the release plan doc with the commit it was measured at, so the stable tag re-reads only the delta ([release.md § 1](../../../docs/operations/release.md#1-pre-flight)).

**A docs edit merged now must not touch `charts/actions-gateway/README.md`.** It ships inside the chart tarball, so a pure-docs pull request moves an artifact the gate is validating.
`scripts/release/check-artifact-unchanged.sh <candidate-sha> origin/main` is the check; exit 1 means the stable tag would ship something no candidate validated.

### Locally, for diagnosis only

Run the gate locally to iterate on the gate itself or to diagnose a failed CI leg, never as the candidate's validation:

```bash
ASSUME_YES=1 PROJECT=… CLUSTER=… ZONE=… REPO=… \
  scripts/agent/record-launch.sh scripts/dogfood/validate-release.sh [--legs <leg>] vX.Y.Z-rc.N > tmp/validate.log 2>&1
```

Background it with a 60-minute Bash timeout.
`ASSUME_YES=1` **skips the gate's own target confirmation**, so run `ops.sh at-rest` first and say you did.
A local run and a CI one read the same lease, so whichever starts second refuses.
**A local run spends one-way legs:** a storage sweep rewrites the cluster's objects once, so a CI run after it takes the no-op path and its evidence no longer shows a migration.

If an auto-mode refusal stops the launch, retry the plain form once; the classifier reads the transcript, so the same command can pass in one session and fail in the next.
The teardown trap misses `kill -9` and a killed parent, which leave billable nodes up; reclaim with `validate-release.sh --reclaim` under the same four variables.

**Either way, confirm the cluster returned to 0 nodes by asking the cluster** (`ops.sh at-rest`), not by reading the gate's own teardown line.

## 6. Stop and hand back the decision

Record the verdict in the release plan doc either way, then:

**The freeze runs from the candidate to the stable tag, and its rule is byte-identical artifacts, not "docs only".** That is how `v1.5.0-rc.1` was superseded: it validated, eight rows merged on top, and its verdict stopped describing `main`.
The step 5 check covers the whole window, so re-run it before the stable tag.

**Green.** Report the evidence (e2e counts, sizing legs, CRD smoke, the artifact checks) and **ask whether to promote**.
Do not tag a stable release, and do not treat the question as rhetorical.
Name what still binds at the stable tag: the delta re-read of each verdict from its recorded commit, the full competitor-stamp report and `make roadmap-check`, and what only validation unlocks (the Validation section, the image digests, the pin bump, the versioned docs deploy).
If the answer is yes, the [`release`](../release/SKILL.md) skill carries those out; this one ends here.

**Red.** Report what failed, and separate the two questions that look alike:

- Was the failure the **candidate's code**?
  Then it needs a fix and a new candidate number.
- Was it the **gate or the pipeline**?
  Then the candidate may still be sound.
  Two of the three failures in the 1.5 cycle were the release tooling itself, not the product.

Either way, once the cause is fixed, re-run only the leg that failed in CI (`gh workflow run validate-candidate.yml --ref main -f tag=vX.Y.Z-rc.N -f legs=<leg>`): the legs before it are already recorded.

Propose action items in that split, with the mitigative fix and the preventative one named separately, and let the user choose.
A failed candidate is also a retro trigger.
