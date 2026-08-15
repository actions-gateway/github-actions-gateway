# `v1.5.0-rc.2` tagged a stale commit, and an immutable Release made it permanent

**Date:** 2026-08-15.
**Impact:** one release-candidate number burned; five signed images bound to the wrong commit; a GitHub Release published with none of its nine assets.
No operator-facing artifact was affected, because prerelease tags are ignored by the released-chart e2e gate and no dogfood validation had been spent.

## What happened

A release-candidate cut for 1.5 was in progress.
Early on, a local `v1.5.0-rc.2` tag was created against what was then the head of `main`, `53283cd9f`.
The push was denied, because the session ran under a permission mode with no way to confirm an outward-facing tag push, so the tag existed locally and unpushed.

A later `git tag -d` reported the tag absent, and an independent check agreed, so the cut proceeded on the understanding that no tag existed.
Work continued for some hours: three pull requests merged, moving `main` three commits ahead of `53283cd9f`, including a rename that changed CRD field descriptions the release was specifically about.

The tag was then pushed.
It still pointed at `53283cd9f`.

`publish.yml` ran and half succeeded.
All five image jobs published and signed against the stale commit.
`chart-publish` failed:

```
HTTP 422: Cannot upload assets to an immutable release.
```

The failure was found only when the tag's commit was questioned directly, which also surfaced the staleness.
`v1.5.0-rc.3` was cut from the correct commit after the pipeline was repaired.

## Two independent faults

The wrong commit and the failed upload share no cause.
Either alone would have produced a bad candidate.

### Fault 1: a tag created in one moment and pushed in another

Nothing re-checked the tag's target at push time.
The runbook's procedure was three adjacent commands, which is safe only while creation and push stay adjacent; here they were separated by hours and three merges.

Three properties combined:

- **A denied push leaves durable state.** The guard blocked the outward-facing half of an operation meant to be atomic, and the created tag outlived its own correctness with nothing marking it stale.
- **`git tag -a` fails on an existing tag, so `&&` skips the push.** The documented chain silently does nothing when a leftover tag is present, while a bare `git push` sends that leftover instead.
  Both failure modes are quiet.
- **A worktree widens the gap.** A worktree cannot check out `main` while the primary holds it, so the tag is created against `origin/main` rather than a branch, and no checkout state reflects the target.

### Fault 2: create-then-upload is incompatible with immutable releases

`publish.yml` created the Release and attached assets to it afterwards.
Publishing seals an immutable Release, so every upload after creation is rejected: the signed v2 CRD manifest and its cosign bundle, `SHA256SUMS` and its bundle, and five `gag-migrate` binaries.

The repository's Releases became immutable between `v1.5.0-rc.1` and `v1.5.0-rc.2`.
The former reads `immutable: false` with nine assets, the latter `immutable: true` with zero.
Nothing in the workflow changed, so the change came from outside it, and no signal reported it until a release failed.

## Why the repair was a new candidate rather than a correction

A published immutable Release locks its tag to one commit; the tag cannot be moved or deleted while the Release exists.
So a tag pushed at the wrong commit is not recoverable in place, and the images already signed under it carry provenance binding them to that commit.
Cutting the next candidate number was the only option, which raises the cost of this class of mistake from "re-tag and move on" to "burn a number and re-publish".

## What changed

**Mitigative.** `v1.5.0-rc.3` was cut from the verified commit and published cleanly: `chart-publish` green, nine assets attached, Release published rather than draft.

**Preventative.**

- `publish.yml` creates the Release as a **draft**, attaches every asset, then publishes it, which is the order the platform documents for immutable releases.
  A run that dies part-way now leaves a draft rather than a published Release missing its assets, a failure an operator can see and finish rather than one that looks complete.
- The release runbook's tag procedure now requires the tag's target and `origin/main` to be printed and compared **before** the push, and states why: creation and push are two moments, and a leftover tag makes one command do nothing and the other do the wrong thing.
- The runbook records what immutability freezes (assets and the tag) and what stays editable (title and notes), so the curated-notes step is known to still work and a mis-tag is known to be unrecoverable.

## What this says about guards

The guard that denied the push was not the cause, and disabling it would not have helped: it was the reason the *first*, correct-at-the-time tag never went out from that session, and the deny message was accurate.
The lesson is narrower.
A guard that blocks one half of a two-step operation leaves the other half as state, and any procedure written as an atomic chain needs a verification step at the boundary where it can be interrupted.
