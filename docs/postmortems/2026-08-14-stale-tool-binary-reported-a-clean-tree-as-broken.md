# 2026-08-14: a stale tool binary reported a clean tree as broken

A gate run locally and the same gate run in CI answered differently about the same commit for five days.
The local answer was wrong, and nothing about the run looked wrong: it took no flag it did not know, printed plausible output, and named four real files.

Kept because the interesting part is not the build rule.
That is a one-line Makefile defect already tracked as [Q842](https://github.com/actions-gateway/github-actions-gateway/blob/main/docs/STATUS.md).
The interesting part is what a wrong verdict did once it was believed: it entered three pull request descriptions, all of which merged, and became the opening instruction of a dispatched worker session, gaining authority at each restatement without ever gaining evidence.

## Impact

- Three merged pull request descriptions ([#1483](https://github.com/actions-gateway/github-actions-gateway/pull/1483), [#1485](https://github.com/actions-gateway/github-actions-gateway/pull/1485), [#1486](https://github.com/actions-gateway/github-actions-gateway/pull/1486)) assert that `main` was failing `md-reflow-check`.
  It was not.
- A dispatched worker session was given "unbreak `main`" as its first instruction.
  Roughly half its run went to a defect that did not exist.
- No shipped artifact was affected.
  `main` was green throughout, the release was never blocked, and no code changed on the strength of the claim.

## What the defect was

`make md-reflow-check` builds `.build/mdreflow` from the vendored `tools/` module and runs it over the tree.
The build rule declares eight vendored tool binaries with **no prerequisites**:

```make
$(ACTIONLINT) $(CONTROLLER_GEN) $(CRD_REF_DOCS) $(KUBEBUILDER) $(SETUP_ENVTEST) $(GOLANGCI_LINT) $(GOVULNCHECK) $(MDREFLOW):
```

An empty dependency list means make treats an existing binary as current forever, whatever the pinned version says.
So a binary built before a pin bump keeps serving every later invocation of a target built around the new one.

CI never sees this.
It checks out fresh, has no `.build/`, and therefore builds from the pin every time.
The two environments diverge silently and only on the developer's side.

## Timeline

**2026-08-09.** `.build/mdreflow` is built from the then-current pin.

**2026-08-13.** [#1462](https://github.com/actions-gateway/github-actions-gateway/pull/1462) adopts mdreflow v0.1.7.
`tools/go.mod` changes; the binary does not.

**2026-08-13.** A release-readiness session runs `make check`.
`md-reflow-check` reports four files as needing reflow.
The tree is clean; the binary is four days stale.

The reading taken is that `main` is broken.
It is a reasonable inference: the four files are byte-identical to `origin/main`, which does rule out the working branch as the cause, and that check is performed and passes.
What is not considered is a third possibility, that the instrument is wrong.

**2026-08-13 to 08-14.** The claim is restated in three pull request descriptions as a note about repository state, each time alongside the observation that CI cannot see the gate because it is in `CHECK_FAST_GATES` and no workflow runs it.
That observation is true and is filed as a second instance of [Q831](https://github.com/actions-gateway/github-actions-gateway/blob/main/docs/STATUS.md).
It also supplies a ready explanation for why nobody else has noticed, which removes the one prompt that might have triggered a re-check.

**2026-08-14.** A worker session is dispatched with "unbreak `main`" first and the Q831 wiring second.
The Q831 half is real and lands, wiring five previously unwired gates.
The first half has nothing to fix.

**2026-08-14.** Q831's fix wires `md-reflow-check` into `doc-links.yml`.
A run on main's tip now reports the `md-reflow` job as **passed**, while the local run still fails.

**2026-08-14.** The contradiction forces the comparison.
`.build/mdreflow` is dated 2026-08-09, the pin is v0.1.7, the binary is deleted, make rebuilds it, and `make md-reflow-check` passes.
`make check` passes whole for the first time in the session.

## Contributing factors

**A tool binary outlives its pin, by construction.** The Makefile rule has no prerequisites, so nothing connects a change in `tools/go.mod` to a rebuild.
Recorded as Q842 before this session, from the *loud* failure mode: a stale binary meeting a flag it does not know.
The silent mode, a working invocation returning a wrong answer, is strictly worse and was not on the row.

**Nothing reconciles a local verdict against CI's.** Both signals existed the entire time and were never placed side by side.
There is no habit, and was no written rule, that treats a local-versus-CI disagreement as a statement about the checkout rather than about the repository.

**A true adjacent finding made the false one look explained.** `md-reflow-check` genuinely did not run in CI.
That fact answered "why has nobody else hit this?", which is the question that would otherwise have prompted a second look.
A correct observation supplied cover for an incorrect one.

**Restatement substituted for verification.** The claim was written three times.
Each writing treated the previous one as established rather than as a claim needing its own support, so the cost of being wrong compounded while the evidence stayed at one unexamined reading.

**The verdict was silent, not loud.** A crash or an unknown-flag error would have pointed at the tool.
A clean exit with a plausible file list pointed at the tree, which is where the reader was already looking.

## Action items

**Mitigative**, shipped: Q842 updated with the silent failure mode and moved up the Queue in [#1493](https://github.com/actions-gateway/github-actions-gateway/pull/1493), so the row now carries both modes and the cost case that distinguishes them.

**Preventative**, the class rather than the instance:

- A rule under [The status you report is a claim too](../development/testing.md#the-status-you-report-is-a-claim-too) that a local gate disagreeing with CI indicts the toolchain first, since CI builds its tools from the pin and a developer checkout does not.
  It also names the compounding part, that a repeated claim gains authority without gaining evidence.
- The build rule itself, which is Q842's own fix and the only change here that removes the failure rather than teaching people to expect it.
  Shipped: every tool rule now depends on the file carrying its version pin (`tools/go.mod` and `tools/vendor/modules.txt` for the vendored eight, `cmd/gmc/go.{mod,sum}` for ginkgo), so a bump invalidates the binary the way a source change does.

## What this is not

Not a CI gap.
CI was correct at every point, including before `md-reflow-check` was wired into a workflow, because the tree it was reading was clean the whole time.

Not an argument for distrusting local gates.
The local gate is the fast feedback loop and works; what failed was the assumption that a green or red verdict from it is a statement about the repository, when it is a statement about the repository *and* the binary that read it.
The cheap defence is to notice when the two environments disagree and to fix the difference before reporting the tree.
