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

**Gates run in more than one lane, so query by SHA and not by branch.** A `push`-lane run sits behind the previous push's per-ref concurrency group and can read `in_progress` or `pending` for a long time while the merge queue has already validated the identical commit.
`gh run list --branch main` **excludes** merge-queue runs, which is how a green commit reads as unvalidated:

```bash
for w in unit-test.yml integration-test.yml security-scan.yml e2e-test.yml; do
  printf "%-22s " "$w"
  gh run list --workflow=$w -L 14 --json headSha,event,conclusion,status \
    -q '[.[] | select(.headSha | startswith("<SHA>"))] | map("\(.event):\(.status)/\(.conclusion // "-")") | join("  ")'
  echo
done
```

All four green on the target SHA in *some* lane is the bar.

**A path-gated gate that skipped is not automatically a problem, and not automatically fine.** When docs-only merges sit on top of the last code change, prove the code is the same tree instead of re-running anything:

```bash
git diff <last-fully-validated-sha>..origin/main --stat -- \
  'api/**' 'cmd/**' 'broker/**' 'scaleset/**' 'githubapp/**' 'charts/**' 'config/**' 'deploy/**'
```

Empty means the shipped surface is byte-identical and the earlier full run still covers it.
Say which commit you are relying on.

Then confirm no gating row remains, and run the local gate **from a tree that matches the target**, a branch cut from `origin/main`, since a worktree cannot check out `main`:

```bash
git show origin/main:docs/STATUS.md | grep '^| <a id=' | grep -c '<X.Y>-gate'   # want 0
make check > tmp/check-rc.log 2>&1; echo "EXIT=$?"
```

Running `make check` on a stale branch validates the wrong tree and tells you nothing.

## 2. Re-run any pre-flight verdict whose scope has moved

The runbook records each pre-flight verdict in the release plan doc, and **a verdict covers the commit it was measured at**.
If scope reopened after a verdict was written, that verdict is stale by default.

Check the recorded window against the tag target before trusting it.
In the 1.5 cycle the API surface review had been recorded over an earlier window, scope reopened on eight rows, and one of them published a new condition type, two reasons and two metrics that no review had seen.
The marketing reconciliation from the same day was *not* re-checked, and a shipped cross-tenant guard reached no marketing surface at all.

At a candidate, these bind:

- **`main` green and the version chosen**: every tag.
- **API surface review**: pulled forward, because a rename decided after publication costs a new candidate.
- **The notes draft, interrogated**: the discovery step, not a formatting pass.
  Ask what each claim rests on; re-derive every number rather than copying it forward.
  Every wrong figure in the 1.5 notes was a *derived* value gone stale, not a typo.

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

The teardown trap covers normal exits and SIGTERM but not `kill -9` or a killed parent, which leave billable nodes up.
If the run dies hard, tell the user the reclaim command:

```bash
PROJECT=… CLUSTER=… ZONE=… REPO=… scripts/dogfood/validate-release.sh --reclaim
```

Confirm the cluster returned to 0 nodes by **asking the cluster** (`ops.sh at-rest`), not by reading the gate's own teardown line.

## 6. Stop and hand back the decision

Record the verdict in the release plan doc either way, then:

**Green.** Report the evidence (e2e counts, sizing legs, CRD smoke, the artifact checks) and **ask whether to promote**.
Do not tag a stable release, and do not treat the question as rhetorical.
Name what still binds at the stable tag but not at a candidate: the marketing reconciliation, the operator-caveat pass, the roadmap and `features.md` reconciliation, the announce-bar highlight, and substituting this run's verdict for the previous candidate's in the notes' Validation section.

**Red.** Report what failed, and separate the two questions that look alike:

- Was the failure the **candidate's code**?
  Then it needs a fix and a new candidate number.
- Was it the **gate or the pipeline**?
  Then the candidate may still be sound.
  Two of the three failures in the 1.5 cycle were the release tooling itself, not the product.

Propose action items in that split, with the mitigative fix and the preventative one named separately, and let the user choose.
A failed candidate is also a retro trigger.
