# 2026-08-06: a paragraph split changed what a sentence referred to

A documentation change that was supposed to move paragraph boundaries moved a
meaning instead. Two verification instruments were run and both came back clean,
because neither could see the class of defect that had been introduced. A
reviewer's question caught it, not a tool.

Kept because the interesting part is not the wrong sentence. It is that the
change was verified, the verification was sound, and the verification did not
cover the claim it was cited for.

## Impact

- One incorrect technical statement in [`docs/design/01-executive-summary.md`](../design/01-executive-summary.md),
  live on an open pull request for roughly an hour. It did not reach `main`.
- Three additional commits to correct it, plus two structural edits made on a
  bad measurement and reverted.
- One round of reviewer attention that the tooling should have absorbed.

## What the defect was

The source paragraph ran: field → tier behaviour → concurrency cap → who owns
the guardrails → what a platform team can therefore express. Its closing
sentence began "**This lets** a platform team express 'GPU runners always get at
least 5 slots, can burst to 20, and are capped at 30'", where "This" reached
back across the whole paragraph to the `priorityTiers` field.

The split moved the guardrail sentence into a paragraph of its own. That put a
second candidate antecedent between the pronoun and its referent, and the
reopened sentence was rewritten to "**Together these** let a platform team
express…". As published, "these" reads as the two platform-owned settings named
in the intervening paragraph. Those settings are not what expresses 5/20/30.

## Timeline

| # | What happened |
|---|---|
| 1 | Paragraph split; closing sentence reworded to reopen the new paragraph. |
| 2 | Token-multiset reconciliation run before and after: clean. |
| 3 | Local gate green. Pushed, PR opened, describing the reconciliation as proof that "a dropped qualifier cannot hide". |
| 4 | Reviewer asked whether the edits were actually good. |
| 5 | Re-read found the referent defect and three smaller ones. |
| 6 | First repair kept the split and wrote connective text to hold the seam, asserting a purpose the source never stated. Second defect, same class. |
| 7 | Reviewer noted a revert had been promised, not a repair. Paragraph reverted to byte-identical. |
| 8 | A scan for a related problem reported 22 affected sites; the real figure was 8. Two edits already made on the 22 were reverted. |

## Contributing factors

All systemic. Each one is a thing the repository did not say, or said in a form
that did not reach the moment it was needed.

**Reconciliation was documented as sufficient.** [`testing.md`](../development/testing.md#a-bulk-mechanical-change-proves-itself-by-reconciliation-not-by-an-empty-leftover-query)
presented before/after token reconciliation as the proof for a bulk change,
without noting that it is one-directional. It detects removals. The defect was
an addition, and additions are invisible to it. The check was correct and was
cited for a claim it does not support.

**A differential claim was measured with a point-in-time scan.** The follow-up
probe answered "which labels have unlabelled paragraphs after them" and the
result was reported as "which of those this change caused". Those are different
questions and the numbers differ by a factor of three. Nothing required a probe
making a claim about a change to be run against the base tree as well.

**No rule covered split-induced referent breakage.** A pronoun whose antecedent
moves is the characteristic failure of paragraph splitting, and it was
undocumented, so nothing prompted a re-read of the reopened paragraphs.

**The repair reflex was to write prose.** Damage from a split was addressed by
inventing connective text, which is how the second defect entered. Nothing said
that a seam needing invented words is a signal the split was wrong.

**The general principle existed and did not fire.** [`testing.md`](../development/testing.md#the-status-you-report-is-a-claim-too)
already closes with "name the signal the claim actually depends on, confirm it
could have shown you the opposite, and read that one." Applied to either probe
it gives the right answer. Every worked example under it was a *corrupted or
absent* signal, so a sound instrument with a narrow scope did not pattern-match
to the rule.

## Action items

**Mitigative**, shipped in the change itself: the paragraph reverted, the
correcting edits made, and nine split rules recorded in
[`q710-overlong-paragraphs.md`](../plan/q710-overlong-paragraphs.md), including
reconciliation's blindness to additions and the requirement that a differential
claim be measured differentially.

**Preventative**, the class rather than the instance:

- A worked example under
  [The status you report is a claim too](../development/testing.md#the-status-you-report-is-a-claim-too)
  for a *sound* instrument whose question is narrower than the claim, so the
  section's closing principle has an instance of this shape to match against.
- A statement in the reconciliation section that the check is one-directional,
  and that a prose edit needs more than it.

## What this is not

Not a case of a missing gate. No repository gate reads prose: link checks
resolve links, the punctuation gate counts punctuation. A gate that could have
caught this would have to understand what a pronoun refers to. The realistic
defence is the documented discipline, applied, which is why both action items
are rules rather than tooling.
