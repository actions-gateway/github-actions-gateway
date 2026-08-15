#!/usr/bin/env bash
# render-release-body-test.sh — asserts the renderer joins prose and nothing else.
#
# A renderer that silently reflowed a table or a fenced block would corrupt the
# most-read document the project publishes, and the corruption would only be
# visible after the release was sealed. Every structure the notes actually use
# therefore gets a case asserting it survives byte for byte, alongside the cases
# asserting prose really is joined — a renderer that changed nothing would pass a
# one-sided suite while fixing none of the 59 hard breaks it exists to remove.
# Fixtures below are markdown, so single-quoted strings are full of backticks
# and `$` that must stay literal. Disabled file-wide rather than per case.
# shellcheck disable=SC2016
set -euo pipefail
shopt -s inherit_errexit

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SUBJECT="$SCRIPT_DIR/render-release-body.sh"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

pass=0
fail=0
ok() {
	printf '[render-release-body-test] ok   %s\n' "$1"
	pass=$((pass + 1))
}
bad() {
	printf '[render-release-body-test] FAIL %s\n' "$1" >&2
	fail=$((fail + 1))
}

# render INPUT — run the subject over a heredoc body, print the result.
render() {
	printf '%s' "$1" >"$WORK/in.md"
	"$SUBJECT" "$WORK/in.md"
}

expect() {
	local desc="$1" want="$2" got="$3"
	if [[ "$got" == "$want" ]]; then
		ok "$desc"
	else
		bad "$desc"
		printf '       want: %q\n       got:  %q\n' "$want" "$got" >&2
	fi
}

# unchanged DESC INPUT — the block must survive byte for byte.
unchanged() {
	local desc="$1" input="$2"
	expect "$desc" "$(printf '%s' "$input")" "$(render "$input")"
}

# --- prose is joined ---------------------------------------------------------
expect "a sentence-per-line paragraph becomes one line" \
	"One. Two. Three." \
	"$(render 'One.
Two.
Three.
')"

expect "blank lines still separate paragraphs" \
	"A one. A two.

B one." \
	"$(render 'A one.
A two.

B one.
')"

expect "a single-line paragraph is unchanged" \
	"Already one line." \
	"$(render 'Already one line.
')"

# Idempotence: publishing twice must not keep folding text together differently.
once="$(render 'One.
Two.
')"
printf '%s\n' "$once" >"$WORK/once.md"
expect "rendering is idempotent" "$once" "$("$SUBJECT" "$WORK/once.md")"

# --- list and quote continuations join, structure does not -------------------
#
# The runbook's rule is "keep every paragraph, blockquote, and list continuation
# on one line", so a continuation folds into its bullet while the bullet itself
# and its siblings stay exactly where they were.
expect "a list item absorbs its continuation line" \
	'- `RunnerLabelsIncomplete`, on a `RunnerSet`: labels went missing. Reasons `LabelsRegistered` / `LabelsNotRegistered` (#1393)
- A second item.' \
	"$(render '- `RunnerLabelsIncomplete`, on a `RunnerSet`: labels went missing.
  Reasons `LabelsRegistered` / `LabelsNotRegistered` (#1393)
- A second item.
')"

unchanged "an ordered list without continuations is untouched" '1. Apply the CRDs.
2. Upgrade the chart.'

expect "an ordered item absorbs its continuation too" \
	'1. Apply the CRDs. Skipping this stops the GMC.
2. Upgrade the chart.' \
	"$(render '1. Apply the CRDs.
   Skipping this stops the GMC.
2. Upgrade the chart.
')"

unchanged "a table is untouched" '| Image | Digest |
| --- | --- |
| gmc | `sha256:abc` |'

unchanged "a heading is untouched" '## Upgrading
### Details'

# The alert marker must stay alone on its line or GitHub stops rendering the block
# as an alert at all; only the quote's body joins.
expect "a GFM alert keeps its marker and joins its body" \
	'> [!WARNING]
> Apply the CRDs first. Skipping it stops the GMC.' \
	"$(render '> [!WARNING]
> Apply the CRDs first.
> Skipping it stops the GMC.
')"

expect "a plain blockquote joins" \
	'> One. Two.' \
	"$(render '> One.
> Two.
')"

unchanged "an HTML fold is untouched" '<details><summary><b>Features (6)</b></summary>
<p>'

unchanged "indented code is untouched" '    helm upgrade gag ./charts/actions-gateway
    kubectl get pods'

# A fenced block is content, blank lines included — reflowing inside one would
# corrupt the install command operators copy.
expect "a fenced block survives, blank line and all" \
	'```bash
helm upgrade gag \
  --set gmc.image.digest=sha256:abc

kubectl get pods
```' \
	"$(render '```bash
helm upgrade gag \
  --set gmc.image.digest=sha256:abc

kubectl get pods
```
')"

expect "prose after a fence is still joined" \
	'```
code
```

After one. After two.' \
	"$(render '```
code
```

After one.
After two.
')"

# --- the real notes ----------------------------------------------------------
#
# Reconciliation against the shipped file, not just fixtures: the renderer must
# actually remove the in-paragraph breaks, and must not change the file'"'"'s
# structural line count.
note="$REPO_ROOT/docs/releases/v1.5.0.md"
if [[ -f "$note" ]]; then
	rendered="$("$SUBJECT" "$note")"
	before="$(grep -c '^' "$note")"
	after="$(printf '%s\n' "$rendered" | grep -c '^')"
	if ((after < before)); then
		ok "the shipped notes lose lines to joining (${before} -> ${after})"
	else
		bad "the shipped notes were not joined at all (${before} -> ${after})"
	fi

	# Fences, list items and table rows must come through at the same count.
	for pat in '^```' '^- ' '^| '; do
		b="$(grep -c "$pat" "$note" || true)"
		a="$(printf '%s\n' "$rendered" | grep -c "$pat" || true)"
		expect "shipped notes keep every '${pat}' line (${b})" "$b" "$a"
	done

	# Reconciliation on content: joining must move words together, never lose or
	# reorder them. Quote markers are excluded because a joined blockquote keeps
	# one `>` where the source had several.
	src_words="$(tr -s '[:space:]' '\n' <"$note" | grep -v '^>\?$' | grep -c . || true)"
	out_words="$(printf '%s\n' "$rendered" | tr -s '[:space:]' '\n' | grep -v '^>\?$' | grep -c . || true)"
	expect "no word is lost joining the shipped notes (${src_words})" "$src_words" "$out_words"
else
	printf '[render-release-body-test] SKIP shipped-notes case (no docs/releases/v1.5.0.md)\n'
fi

# --- usage -------------------------------------------------------------------
rc=0
"$SUBJECT" >/dev/null 2>&1 || rc=$?
expect "no argument is a usage error" 2 "$rc"
rc=0
"$SUBJECT" "$WORK/nope.md" >/dev/null 2>&1 || rc=$?
expect "a missing file is exit 2" 2 "$rc"

printf '[render-release-body-test] %d passed, %d failed\n' "$pass" "$fail"
[[ "$fail" -eq 0 ]]
