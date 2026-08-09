#!/usr/bin/env bash
#
# Unit tests for scripts/docs/check-roadmap.sh — the roadmap/backlog coherence gate.
#
# The rule that earns the gate is "a Q-ID the roadmap names but STATUS.md no
# longer has": because done rows are deleted, that is an exact signal the work
# shipped. These fixtures pin that case plus the section-placement rules, so a
# future change to either file's format fails here rather than silently
# passing everything. Runs under `make check` (via `make scripts-test`).
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
CHECK="$REPO_ROOT/scripts/docs/check-roadmap.sh"

WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT

fails=0

# status QUEUE_IDS DEFERRED_IDS — write a STATUS.md holding the given
# space-separated IDs in each table. Echoes the file path.
#
# An entry may carry labels as `ID:label,label` (`Q1:1.5-gate`), which the
# fixture renders backticked the way the real table does — rules 7 and 8 read
# the backticks. A bare ID gets the neutral `infra` label.
status() {
    local queue="$1" deferred="$2" file="$WORKDIR/STATUS.md" entry
    {
        printf '# Project Status\n\n## Queue\n\n'
        printf '| ID | Item | Labels | St | Sz | Notes |\n|---|---|---|---|---|---|\n'
        for entry in $queue; do
            printf '| <a id="%s"></a>%s | Thing | %s | 🔲 | S | note |\n' \
                "${entry%%:*}" "${entry%%:*}" "$(status_labels "$entry")"
        done
        printf '\n## Deferred\n\n'
        printf '| ID | Item | Labels | Sz | Trigger to revive |\n|---|---|---|---|---|\n'
        for entry in $deferred; do
            printf '| <a id="%s"></a>%s | Thing | %s | S | **Demand:** someone asks. |\n' \
                "${entry%%:*}" "${entry%%:*}" "$(status_labels "$entry")"
        done
    } >"$file"
    printf '%s\n' "$file"
}

# status_labels ENTRY — render an `ID:a,b` entry's labels as `` `a` `b` ``.
status_labels() {
    local entry="$1" labels
    # shellcheck disable=SC2016 # backticks are Markdown here, not substitution
    [[ "$entry" == *:* ]] || { printf '`infra`\n'; return; }
    labels="${entry#*:}"
    # shellcheck disable=SC2016 # ditto: the label cell is backticked in STATUS.md
    printf '`%s`\n' "${labels//,/\` \`}"
}

# roadmap NEAR_BULLETS -- EXPLORING_BULLETS — write a roadmap.md with the given
# raw bullet lines in each gated section. Echoes the file path.
roadmap() {
    local file="$WORKDIR/roadmap.md" line in_exploring=0
    {
        printf '# Roadmap\n\n## How to read this page\n\n'
        printf -- '- **An ungated section.** No annotation needed here.\n'
        printf '\n## In progress / near-term\n\n'
        for line in "$@"; do
            if [[ "$line" == "--" ]]; then
                printf '\n## Exploring / longer-term\n\n'
                in_exploring=1
                continue
            fi
            printf '%s\n' "$line"
        done
        (( in_exploring )) || printf '\n## Exploring / longer-term\n\n'
        printf '\n## How priorities are set\n\nFeedback drives the ordering.\n'
    } >"$file"
    printf '%s\n' "$file"
}

# features NAME BULLETS… — write features-NAME.md with the given raw bullet
# lines under one capability heading. Echoes the file path. NAME keeps each
# fixture on its own path, since $FEATURES_FILE outlives the call that made it.
features() {
    local file="$WORKDIR/features-$1.md" line
    shift
    {
        printf '# Features\n\nIntro prose, not a bullet.\n\n## Job intake\n\n'
        for line in "$@"; do printf '%s\n' "$line"; done
    } >"$file"
    printf '%s\n' "$file"
}

# The features fixture every assertion runs against unless it overrides this.
FEATURES_FILE="$(features clean '- **[A linked capability](operations/runbook.md)** — one clause.')"

# expect NAME EXPECTED_RC ROADMAP STATUS [SUBSTRING] — run the gate and assert
# its exit code, and optionally that its stderr mentions SUBSTRING. The features
# page comes from $FEATURES_FILE so the roadmap cases need not name it.
expect() {
    local name="$1" want_rc="$2" rm_file="$3" st_file="$4" needle="${5:-}"
    local out rc=0
    out="$("$CHECK" "$rm_file" "$st_file" "$FEATURES_FILE" 2>&1)" || rc=$?
    if (( rc != want_rc )); then
        printf 'FAIL %-34s rc=%d, want %d\n' "$name" "$rc" "$want_rc"
        printf '%s\n' "$out" | awk '{ print "       " $0 }'
        fails=$((fails + 1))
        return
    fi
    if [[ -n "$needle" ]] && ! grep -qF -- "$needle" <<<"$out"; then
        printf 'FAIL %-34s missing %q in output\n' "$name" "$needle"
        printf '%s\n' "$out" | awk '{ print "       " $0 }'
        fails=$((fails + 1))
        return
    fi
    printf 'ok   %-34s rc=%d\n' "$name" "$rc"
}

# Rule 6 applies to these too, so the baseline fixtures carry a link. Without
# one, every roadmap assertion below would fail on the link check instead of the
# rule it means to pin.
NEAR='- **Near thing.** <!-- q:Q1 --> Body text. [detail](plan/thing.md)'
EXPL='- **Far thing.** <!-- q:Q2 --> Body text. [detail](plan/thing.md)'

# The happy path: near-term backed by a Queue row, exploring by a Deferred row.
expect "clean: both sections backed" 0 \
    "$(roadmap "$NEAR" -- "$EXPL")" "$(status "Q1" "Q2")"

# A bullet outside the two gated sections carries no annotation and isn't gated.
expect "clean: other sections ungated" 0 \
    "$(roadmap "$NEAR" -- "$EXPL")" "$(status "Q1" "Q2")"

# The drift this gate exists for: the row was deleted because the work shipped.
expect "dangling ID (row deleted)" 1 \
    "$(roadmap "$NEAR" -- "$EXPL")" "$(status "" "Q2")" \
    'no longer exists in STATUS.md'

# A bullet nobody annotated cannot be checked at all.
expect "missing annotation" 1 \
    "$(roadmap '- **Near thing.** Body text. [d](plan/t.md)' -- "$EXPL")" "$(status "Q1" "Q2")" \
    'has no <!-- q:QN --> annotation'

# Parked: the row moved to Deferred, so the bullet belongs under Exploring.
expect "near-term names only Deferred" 1 \
    "$(roadmap "$NEAR" -- "$EXPL")" "$(status "" "Q1 Q2")" \
    'Move it to "Exploring / longer-term"'

# Revived: the row moved into the Queue, so it is active work.
expect "exploring names only Queue" 1 \
    "$(roadmap "$NEAR" -- "$EXPL")" "$(status "Q1 Q2" "")" \
    'Move it to "In progress / near-term"'

# A multi-item bullet stays green while any one of its rows is still open...
expect "multi-ID, all live" 0 \
    "$(roadmap '- **Near thing.** <!-- q:Q1,Q3 --> Body. [d](plan/t.md)' -- "$EXPL")" \
    "$(status "Q1 Q3" "Q2")"

# ...but a deleted row is still reported, so a half-shipped bullet gets edited.
expect "multi-ID, one shipped" 1 \
    "$(roadmap '- **Near thing.** <!-- q:Q1,Q3 --> Body. [d](plan/t.md)' -- "$EXPL")" \
    "$(status "Q1" "Q2")" 'names Q3'

# The annotation may sit on any line of the bullet, not just the first.
expect "annotation on continuation line" 0 \
    "$(roadmap '- **Near thing.** Body text runs on' '  and ends here. <!-- q:Q1 --> [d](plan/t.md)' -- "$EXPL")" \
    "$(status "Q1" "Q2")"

# A typo'd annotation must not silently pass as "unbacked but unchecked".
expect "non-Q-ID annotation" 1 \
    "$(roadmap '- **Near thing.** <!-- q:TODO --> Body. [d](plan/t.md)' -- "$EXPL")" \
    "$(status "Q1" "Q2")" 'is not a Q-ID'

# A fenced block documenting the annotation format is prose about it, not an
# annotation — the shape a line-matching gate cannot tell apart, because the
# fence is invisible to it. This bullet has no real annotation and must fail.
expect "annotation inside a code fence does not count" 1 \
    "$(roadmap '- **Near thing.** Body text. [d](plan/t.md)' '' '  ```' '  - **Example.** <!-- q:Q1 --> How to annotate.' '  ```' -- "$EXPL")" \
    "$(status "Q1" "Q2")" 'has no <!-- q:QN --> annotation'

# The word cap measures the bullet, not the page. Prose between two bullets
# belongs to neither, and counting it against the one above inflates every
# multi-paragraph section: docs/roadmap.md:43 carried 19 words of a following
# paragraph that way. 55 words of bullet plus 20 of paragraph is under the
# 60-word cap only if the paragraph is left out of it.
long_body="$(printf 'word %.0s' $(seq 1 51))"
trailing_prose="$(printf 'para %.0s' $(seq 1 20))"
expect "prose after a bullet is not counted against it" 0 \
    "$(roadmap "- **Near thing.** <!-- q:Q1 --> [d](plan/t.md) $long_body" '' "$trailing_prose" -- "$EXPL")" \
    "$(status "Q1" "Q2")"

# The positive control for that one: a bullet whose own body wraps across lines
# is still measured whole, so excluding the paragraph after it is not a way of
# excluding continuation lines too.
wrapped_head="$(printf 'word %.0s' $(seq 1 30))"
wrapped_tail="$(printf 'word %.0s' $(seq 1 35))"
expect "a bullet spanning line breaks is counted whole" 1 \
    "$(roadmap "- **Near thing.** <!-- q:Q1 --> [d](plan/t.md) $wrapped_head" "  $wrapped_tail" -- "$EXPL")" \
    "$(status "Q1" "Q2")" 'the rest belongs in the linked doc'

# Format drift on either side is a hard error (rc 2), never a silent pass.
printf '# Roadmap\n\n## Something Else\n\n- **Orphan.** Body. [d](plan/t.md)\n' >"$WORKDIR/no-sections.md"
expect "roadmap headings renamed" 2 \
    "$WORKDIR/no-sections.md" "$(status "Q1" "Q2")" 'found no bullets'

printf '# Project Status\n\nNo tables here.\n' >"$WORKDIR/no-rows.md"
expect "STATUS tables unparseable" 2 \
    "$(roadmap "$NEAR" -- "$EXPL")" "$WORKDIR/no-rows.md" 'parsed no Q-IDs'

expect "missing file" 2 "$WORKDIR/nope.md" "$(status "Q1" "Q2")" 'file not found'

# --- Rule 5: the feature index stays a link index, not prose. ---------------
#
# The failure this pins: docs/features.md was extracted from a roadmap section
# whose bullets had grown to 126 words each *because* nothing made them link
# out. Both halves of the rule are load-bearing — a word cap alone still allows
# an unlinked stub, and a link alone still allows a paragraph.

RM_OK="$(roadmap "$NEAR" -- "$EXPL")"
ST_OK="$(status "Q1" "Q2")"

FEATURES_FILE="$(features nolink '- **A capability with no doc behind it** — nothing to click.')"
expect "feature bullet without a link" 1 "$RM_OK" "$ST_OK" 'has no link'

# 60 words past a valid link: the shape the extraction removed.
long_bullet="- **[A capability that will not stop talking](operations/runbook.md)** —$(printf ' word%.0s' $(seq 1 60))"
FEATURES_FILE="$(features toolong "$long_bullet")"
expect "feature bullet over the word cap" 1 "$RM_OK" "$ST_OK" 'Move the detail into the linked doc'

# A badge span is presentation, not prose, and must not spend the word budget.
# Sized to the boundary so this actually discriminates: the title, the two badge
# labels, the dash and 39 body words are exactly the 45-word cap, so any tag or
# attribute leaking into the count fails it. A shorter bullet would pass either
# way and pin nothing.
badge_bullet="- **[A v2 capability](operations/runbook.md)** <span class=\"gag-v2-badge\">v2</span> <span class=\"gag-maturity-badge\">beta</span> —$(printf ' word%.0s' $(seq 1 39))"
FEATURES_FILE="$(features badges "$badge_bullet")"
expect "badge markup is not counted" 0 "$RM_OK" "$ST_OK"

# Format drift on the features page is a hard error, not a silent pass — the
# same stance the two roadmap-side rc=2 cases above take.
printf '# Features\n\nProse only, no bullets.\n' >"$WORKDIR/features-empty.md"
FEATURES_FILE="$WORKDIR/features-empty.md"
expect "features page has no bullets" 2 "$RM_OK" "$ST_OK" 'found no capability bullets'

FEATURES_FILE="$WORKDIR/features-clean.md"

# --- Rule 6: roadmap bullets link out too, under a looser cap. --------------
#
# Same failure as rule 5 in the other section: at extraction the five worst
# roadmap bullets ran 74-123 words by explaining the whole approach inline,
# while the plan doc or Appendix G section holding that detail went unlinked.

expect "roadmap bullet without a link" 1 \
    "$(roadmap '- **Near thing.** <!-- q:Q1 --> No link anywhere.' -- "$EXPL")" \
    "$ST_OK" 'Point at the plan doc or Appendix G section'

# 70 words past a valid link — over the 60-word roadmap cap but well under
# nothing, so this pins the cap itself rather than the presence of prose.
long_near="- **Near thing.** <!-- q:Q1 --> [d](plan/t.md)$(printf ' word%.0s' $(seq 1 70))"
expect "roadmap bullet over the word cap" 1 \
    "$(roadmap "$long_near" -- "$EXPL")" "$ST_OK" \
    'the rest belongs in the linked doc'

# The roadmap cap is deliberately looser than the feature cap: a bullet that
# would fail rule 5 still passes rule 6, because it must also name its gate.
mid_near="- **Near thing.** <!-- q:Q1 --> [d](plan/t.md)$(printf ' word%.0s' $(seq 1 50))"
expect "roadmap cap is looser than features" 0 \
    "$(roadmap "$mid_near" -- "$EXPL")" "$ST_OK"

# --- Rules 7 and 8: the release commitment and the gate label agree. --------
#
# The one promise this page makes with a date on it. Rule 7 is the load-bearing
# half and reads only the `<!-- q:QN -->` binding and the label, both
# machine-readable, so it survives the commitment being re-rendered from prose
# into a derived chip (Q770). Rule 8 is the narrower guard on a version typed
# by hand; it is expected to find nothing once the chip is derived, which is why
# it is not the half the reconciliation rests on.

GATED_NEAR='- **Near thing.** <!-- q:Q1 --> Body text. [detail](plan/thing.md) Gating the 1.5 release.'

expect "gate label bound to a bullet" 0 \
    "$(roadmap "$GATED_NEAR" -- "$EXPL")" "$(status "Q1:feature,1.5-gate" "Q2")"

# Rule 7: the label has nowhere to render, so the release is committed nowhere
# an adopter reads. A gated row no bullet names is invisible to every other rule
# here, all of which start from a bullet and look outward.
expect "gated row no bullet names" 1 \
    "$(roadmap "$NEAR" -- "$EXPL")" "$(status "Q1 Q3:feature,1.5-gate" "Q2")" \
    'no roadmap.md bullet names it'

# ...and it is indifferent to how the bullet states the release. A bullet with
# no prose claim at all still covers the label, because the binding is what
# rule 7 reads. Without this, rule 7 would fail the page the moment Q770
# replaces the sentence with a chip.
expect "coverage does not require prose" 0 \
    "$(roadmap "$NEAR" -- "$EXPL")" "$(status "Q1:feature,1.5-gate" "Q2")"

# A gated Deferred row needs a bullet just the same; the label means the tag
# waits for it either way.
# shellcheck disable=SC2016 # the backticks are the finding's Markdown, not substitution
DEFERRED_GATE_NEEDLE='Q4 is labelled `2.0-gate`'
expect "gated Deferred row no bullet names" 1 \
    "$(roadmap "$NEAR" -- "$EXPL")" "$(status "Q1" "Q2 Q4:feature,2.0-gate")" \
    "$DEFERRED_GATE_NEEDLE"

# Rule 8: the row was re-gated to a later release (or ungated entirely) and the
# hand-typed version still promises the old one.
expect "hand-typed version outlives the label" 1 \
    "$(roadmap "$GATED_NEAR" -- "$EXPL")" "$(status "Q1:feature" "Q2")" \
    'writes the 1.5 release into its prose'

expect "hand-typed version names the wrong release" 1 \
    "$(roadmap "$GATED_NEAR" -- "$EXPL")" "$(status "Q1:feature,2.0-gate" "Q2")" \
    'writes the 1.5 release into its prose'

# A patch component is discarded, because `2.0-gate` blocks the `v2.0.0` tag
# and that is how docs/roadmap.md writes it. Without the normalization this
# bullet reads as a claim on release 2.0.0 and fails against its own label.
# shellcheck disable=SC2016 # backticks are a Markdown code span in the fixture
V_PATCH_NEAR='- **Near thing.** <!-- q:Q1 --> [d](plan/t.md) Gating the `v2.0.0` release.'
expect "a v-prefixed patch version normalizes" 0 \
    "$(roadmap "$V_PATCH_NEAR" -- "$EXPL")" "$(status "Q1:feature,2.0-gate" "Q2")"

# The Q273 trap, and the reason rule 8 keys on a gating verb rather than on a
# version appearing anywhere. Q273's bullet names `v1.3.0` and `v2.0.0` to say
# where the removal lands; it is a Deferred row event-gated on v2-ga Phase 3,
# not a row blocking that tag, and it carries no gate label. A rule reading
# "a bullet naming a version must carry the matching label" fails this page as
# written, which is the false fire that would have forced a wrong edit.
# shellcheck disable=SC2016 # backticks are Markdown code spans in the fixture
Q273_SHAPE='- **[Retiring the old APIs](operations/v1alpha1-deprecation.md)** <!-- q:Q2 --> Committed, but not yet started. `v1.3.0` is the one-release-ahead announcement; **`v2.0.0`** is the named release that removes all three together. Gated on the `v2` GA API being validated, not on a date.'
expect "naming a release is not claiming one" 0 \
    "$(roadmap "$NEAR" -- "$Q273_SHAPE")" "$(status "Q1" "Q2")"

# ...and the positive control for that: the same bullet with a gating verb in
# front of the version is a claim, so the exemption is about the verb and not
# about `v2.0.0` being unreadable.
expect "the same bullet with a gating verb claims" 1 \
    "$(roadmap "$NEAR" -- "${Q273_SHAPE/is the named release/gates the \`v2.0.0\` release and is the named release}")" \
    "$(status "Q1" "Q2")" 'writes the 2.0 release into its prose'

# "Gateway" contains "gate". The page says it constantly, so an unanchored verb
# match would read every bullet mentioning the product as a release claim.
# Sized so the word boundary is the only thing refusing this: the version sits
# 25 characters past "Gate", well inside the window the verb may reach across.
expect "Gateway is not a gating verb" 0 \
    "$(roadmap '- **Near thing.** <!-- q:Q1 --> [d](plan/t.md) The Gateway is validated for the 1.5 release.' -- "$EXPL")" \
    "$(status "Q1" "Q2")"

# A claim wraps across a line break in the real page, and the parser fuses the
# words either side of one, so "Gating the" + "1.5 release." reads as
# "Gating the1.5 release." The rule must not depend on where an author wrapped,
# and both seams inside the claim are separate cases: docs/roadmap.md wraps at
# the first for Q712 and at the second for Q713.
expect "a claim wrapped before the version" 0 \
    "$(roadmap '- **Near thing.** <!-- q:Q1 --> [d](plan/t.md) Gating the' '  1.5 release.' -- "$EXPL")" \
    "$(status "Q1:feature,1.5-gate" "Q2")"

expect "a claim wrapped after the version" 0 \
    "$(roadmap '- **Near thing.** <!-- q:Q1 --> [d](plan/t.md) Gating the 1.5' '  release.' -- "$EXPL")" \
    "$(status "Q1:feature,1.5-gate" "Q2")"

# A multi-ID bullet is gated if any row it names is, and the claim covers it.
expect "gate on one of several rows" 0 \
    "$(roadmap '- **Near thing.** <!-- q:Q1,Q3 --> [d](plan/t.md) Gating the 1.5 release.' -- "$EXPL")" \
    "$(status "Q1:feature Q3:feature,1.5-gate" "Q2")"

# A gate on a row the bullet no longer names is rule 2's finding, not a missing
# claim: reporting both would bury the dangling ID under a rule 7 message about
# a row that is gone.
expect "a deleted row contributes no gate" 1 \
    "$(roadmap '- **Near thing.** <!-- q:Q1,Q3 --> [d](plan/t.md)' -- "$EXPL")" \
    "$(status "Q1" "Q2")" 'names Q3'

# The label column moving would leave rules 7 and 8 reading nothing while every
# other rule still passed. That is the silent-pass shape, so it is rc 2.
{
    printf '# Project Status\n\n## Queue\n\n'
    printf '| ID | Item | St | Sz | Notes |\n|---|---|---|---|---|\n'
    printf '| <a id="Q1"></a>Q1 | Thing | 🔲 | S | note |\n'
    printf '\n## Deferred\n\n| ID | Item | Sz | Trigger to revive |\n|---|---|---|---|\n'
    printf '| <a id="Q2"></a>Q2 | Thing | S | **Demand:** someone asks. |\n'
} >"$WORKDIR/no-labels.md"
expect "STATUS.md lost its Labels column" 2 \
    "$(roadmap "$NEAR" -- "$EXPL")" "$WORKDIR/no-labels.md" 'no "Labels" column'

if (( fails )); then
    printf '\ncheck-roadmap-test: %d assertion(s) failed\n' "$fails" >&2
    exit 1
fi

printf '\ncheck-roadmap-test: all assertions passed\n'
