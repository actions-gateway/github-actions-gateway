# `v1.6.0` tagged, published, and the docs site kept serving 1.5.0

**Date:** 2026-08-26.
**Impact:** for roughly 25 minutes after the stable tag published, `https://actions-gateway.com/1.6.0/` returned 404 and the site's default (`/` and `/stable/`) served 1.5.0, so a reader arriving during that window got the previous release's install command.
No artifact was affected: images, chart, assets, signatures and provenance were all correct and verified, and the GitHub Release was complete.

## What happened

`v1.6.0` was pushed at 23:24:12Z, having been validated on `v1.6.0-rc.2` and frozen against it.
`pages.yml` ran for the tag and finished green, every step including `Deploy to GitHub Pages`.

Inside that run, `mike` built the version and pushed `898d30bb6..36c3acfc5` to `gh-pages` at 23:25:05.550Z, adding `1.6.0` and moving the `stable` alias onto it.
The next step in the same job assembled the Pages artifact at 23:25:05.557Z:

```bash
git archive gh-pages | tar -x -C _site
```

The Pages deployment reported success at 23:25:21Z.

The site did not have the release.
`/1.6.0/` returned 404 and the served `versions.json` listed 1.5.0 as `stable` with no 1.6.0 entry, unchanged under cache-busting requests ten and twenty minutes later.
Meanwhile `gh-pages` itself was correct throughout: its `versions.json` carried `1.6.0` with `aliases: ["stable"]`, and a `1.6.0` directory existed.

`make verify-published-docs VERSION=v1.6.0` found it, exiting 1 with `the published docs for 1.6.0 do not advertise 1.6.0`.
A manual `workflow_dispatch` of `pages.yml` (`version=1.6.0 alias=stable set_default=true docs_ref=v1.6.0`) republished, after which the same gate exited 0 and `/stable/` resolved to `/1.6.0/`.

## Why: the artifact was correct, and the site did not serve it

Settled 2026-08-27 (Q1000), against the artifact the failing run uploaded.
`actions/upload-pages-artifact` keeps it, and it had not expired:

```bash
gh api /repos/actions-gateway/github-actions-gateway/actions/runs/33023216267/artifacts
gh run download 33023216267 -n github-pages -D ./artifact
tar -xOf ./artifact/artifact.tar ./versions.json
```

That artifact carries a `./1.6.0` directory, and its `versions.json` lists `1.6.0` with `aliases: ["stable"]`.
So `git archive gh-pages` resolved the ref `mike` had just written, and the first hypothesis is refuted: nothing in the workflow assembled a stale tree.
The deployment side agrees.
Deployment `6113682780` went `success` at 23:25:21Z and marked the previous one `inactive` a second later, so the correct artifact was the active deployment for the whole window the site was serving the old one.

What the site served during that window matches `898d30bb6` exactly, the `gh-pages` commit from *before* the tag run's `mike` step.
That is the previous deployment's content, not a partial or intermediate one: the run also passed through `5f8cd7940`, which lists `1.6.0` with `stable` still on `1.5.0`, and the site never showed that.

So the failure was on the serving side, between an accepted deployment and what the CDN returned.
This document does not claim to know which layer, because nothing outside GitHub can see it.
What it does establish is where the failure was **not**: every step the repository controls produced the right bytes.

An early reading called this a race between the push and the assembly.
The timestamps refute it: they are sequential steps in one job, 7 milliseconds apart, push first.
The correction is recorded here because the wrong mechanism was stated confidently before it was measured, and the artifact that settles it was one API call away the whole time.

## Contributing factors

**The run's own verification cannot fail on a stale tree.** The `Verify the artifact serves a reachable root` step asserted `_site/index.html` and `_site/CNAME` exist.
Both are true of a stale version tree, so a deploy that published the wrong content would have passed its own check and reported green.
That gap was real and is now closed, and it is worth being explicit that it is not this incident's cause: the artifact here was correct, so the assertion added for it passes on this run.
Reading the two as one thing is what the measurement above exists to prevent.

**The only check that reads the live site is manual and post-hoc.** `verify-published-docs` is what caught this, and it runs when a release engineer chooses to run it.
Nothing in CI compares what was deployed against what was meant to be deployed.

**A green pipeline is the strongest available signal, and it was wrong.** Every automated indicator agreed the release had published.
The disagreement only appeared by asking the site.

## Action items

**Mitigative** — done: the version was republished and verified.

**Preventative** — done, Q1000.
The publish job now runs two checks instead of one.
[`verify-pages-artifact.sh`](../../scripts/pages/verify-pages-artifact.sh) asserts, before the upload, that `_site` carries the version being deployed and that `versions.json` lists it with the alias the run claimed.
[`verify-pages-live.sh`](../../scripts/pages/verify-pages-live.sh) then reads the site after the deploy and polls for up to five minutes, failing the run with the republish command when it never arrives.
The second is what covers this incident.
The first covers the tree-side failure this was mistaken for, which nothing had ever checked.
Neither replaces `verify-published-docs`, which reads the version pins inside the pages and cannot run at tag time (see [release.md step 7](../operations/release.md#the-bump-on-main-does-not-reach-the-published-release)).

## What this is a case of

The repo already records that three of the four releases before 1.5.0 published the previous version's install command as their landing page, and that no working-tree gate saw it.
`verify-published-docs` exists because of that, and it worked here.
The gap was that it was the *last* line of defence rather than a gate, so the failure was caught by a human remembering to look, at whatever point they looked.
That is what `verify-pages-live.sh` closes: the same question, asked by the run that caused it, within five minutes rather than whenever someone thinks to check.
`verify-published-docs` keeps its own job, which is the pins, and stays a hand step because a tag cannot pass it.

## Recurrence: every stable tag since

`verify-pages-live.sh` has failed at every stable tag after `v1.6.0`, so the incident was not a one-off.
Measured 2026-11-22 from each run's live-check step (`gh run list --workflow pages`, `gh run view <id> --log-failed`) and the Pages deployments API (`gh api repos/actions-gateway/github-actions-gateway/deployments?environment=github-pages`, then each deployment's `/statuses`):

| Run | Ref | Commit already deployed on `main` | Live check |
|---|---|---|---|
| `v1.9.0`, 2026-10-06 | tag push | yes, 2 min earlier | failed; a `main` dispatch of the same commit passed |
| `v1.10.0`, 2026-10-27 | tag push | yes, 34 min earlier | failed; a `main` dispatch of the same commit passed |
| `v1.10.0`, 2026-11-02 | dispatch on the tag | no, the site had moved on | passed |
| `v2.0.0`, 2026-11-17 | dispatch on the tag | yes, 6 min earlier | failed; a `main` dispatch of the same commit passed |
| `v2.0.1`, 2026-11-18 | dispatch on the tag | yes, 13 min earlier | failed; a `main` dispatch of the same commit passed |

The `github-pages` deployment statuses do not separate these: they record the publish job's outcome, not whether Pages activated the deployment, so a failed live check shows as a deployment `failure` and the `v1.10.0` release candidates' tag-ref deployments show `success` in about 30 seconds.
The `main` push deployments in the same window are no evidence either, since they skip the live check.
What separates the runs is the run's own live check, and the four `main` dispatches that repaired a release passed it.
The `v2.0.0` and `v2.0.1` runs were dispatches on the tag ref, so the earlier fix (Q1041, dispatch instead of push) did not change the outcome.
That is five runs and no controlled test, and why Pages serves the previous tree is not known.
The `handoff` job now dispatches on `main` with `docs_ref` set to the tag (Q1355).
The first stable tag cut after that change is the test, and this section should be amended with its result.
