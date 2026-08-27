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

## What is established, and what is not

Established: `gh-pages` held the correct tree immediately after the run; the artifact that deployed did not; every step of the run reported success.

**Not established: why.** Two hypotheses survive and this document does not pick one.

1. `git archive gh-pages` resolved a ref that was not the one `mike` had just written.
   The step runs after the push, in the same workspace, so this requires the local `refs/heads/gh-pages` to differ from what was pushed.
2. Propagation took unusually long, and the manual republish merely coincided with it resolving.

One thing was ruled out: the two `pages.yml` runs in the window did not overlap.
The `main` run finished at 23:23:14Z and the tag run started at 23:24:12Z, so they did not race the shared `gh-pages` branch, and the tag run's deployment was the last to become active.

An early reading called this a race between the push and the assembly.
The timestamps refute it: they are sequential steps in one job, 7 milliseconds apart, push first.
The correction is recorded here because the wrong mechanism was stated confidently before it was measured.

## Contributing factors

**The run's own verification cannot fail on this failure.** The `Verify the artifact serves a reachable root` step asserts `_site/index.html` and `_site/CNAME` exist.
Both are true of a stale version tree, so a deploy that publishes the wrong content passes its own check and reports green.

**The only check that reads the live site is manual and post-hoc.** `verify-published-docs` is what caught this, and it runs when a release engineer chooses to run it.
Nothing in CI compares what was deployed against what was meant to be deployed.

**A green pipeline is the strongest available signal, and it was wrong.** Every automated indicator agreed the release had published.
The disagreement only appeared by asking the site.

## Action items

**Mitigative** — done: the version was republished and verified.

**Preventative** — [Q1000](../queue/Q1000.md): establish which hypothesis holds, then make the publish job assert its own artifact contains the version being deployed, and that `_site/versions.json` lists it with the resolved alias.
A deploy that publishes the wrong tree should fail its own run rather than wait for someone to read the live site.

## What this is a case of

The repo already records that three of the four releases before 1.5.0 published the previous version's install command as their landing page, and that no working-tree gate saw it.
`verify-published-docs` exists because of that, and it worked here.
The gap that remains is that it is the *last* line of defence rather than a gate, so the failure is caught by a human remembering to look, at whatever point they look.
