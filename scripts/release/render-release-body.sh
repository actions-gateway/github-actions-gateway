#!/usr/bin/env bash
# render-release-body.sh — turn a release note into the body GitHub should publish.
#
#   scripts/release/render-release-body.sh docs/releases/vX.Y.Z.md > tmp/body.md
#
# Writes to stdout; exit 2 on a usage or read error.
#
# Two rules govern release-note prose and they disagree in this one directory.
# `md-reflow-check` keeps every tracked markdown file at sentence-per-line, so a
# changed sentence is a changed line and the most-reviewed prose the project ships
# stays reviewable. But GitHub renders a release body with comment-flavour GFM,
# where a single newline is a hard break: publishing the file verbatim turns each
# sentence into its own rendered line. Measured 2026-08-15 on the live v1.5.0
# body, 59 <br> across 31 paragraphs, and the count has grown every release
# (20 -> 22 -> 33 in-paragraph breaks at v1.3.0 -> v1.4.0 -> v1.5.0).
#
# Rendering at publish time keeps both properties instead of trading one away: the
# repository keeps its per-sentence diffs, and the published body reads as
# paragraphs.
#
# Paragraphs, list-item continuations and blockquote bodies are joined, which is
# the runbook's rule verbatim: keep every paragraph, blockquote, and list
# continuation on one line. Structure is never touched — fences, tables, headings,
# HTML and the bullets themselves survive byte for byte, and a `> [!WARNING]`
# marker stays on its own line or the alert stops rendering as one.
#
# Verify against the renderer, never the source: `gh release view --json body`
# returns raw Markdown, which contains no <br> however badly it is wrapped, so
# grepping that is a check that cannot fail. The oracle is GitHub's own comment
# flavour, which is what release-notes-check and this script's test both use:
#
#   gh api -X POST /markdown -f mode=gfm -f "text=$(cat FILE)" > out.html
set -euo pipefail
shopt -s inherit_errexit

usage() {
	cat >&2 <<-EOF
		usage: $(basename "$0") <release-note.md>

		  Joins prose paragraphs onto one line, leaving lists, tables, headings,
		  quotes, fenced code and HTML blocks untouched. Writes to stdout.
	EOF
	exit 2
}

[[ $# -eq 1 ]] || usage
[[ "$1" == -h || "$1" == --help ]] && usage
[[ -f "$1" ]] || {
	echo "render-release-body: not a file: $1" >&2
	exit 2
}

# The join is a single awk pass so the file is never rewritten in place: this is a
# renderer, and the source of truth stays the file in docs/releases/.
#
# A "block" is a run of non-blank lines. Its FIRST line decides the whole block,
# which is what keeps a list item's continuation lines with their item rather than
# folding them into the preceding paragraph.
awk '
	# pending holds the line being assembled; mode says what may join it.
	function flush() {
		if (pending != "") print pending
		pending = ""
		mode = ""
	}
	function emit(s) { flush(); print s }
	function strip(s) { sub(/^[ \t]+/, "", s); return s }

	# A fenced block runs to its closing fence and is never reflowed, blank lines
	# inside it included — those blanks are content, not block separators.
	/^[ \t]*(```|~~~)/ { flush(); infence = !infence; print; next }
	infence { print; next }

	/^[ \t]*$/ { flush(); print; next }

	# Structure, always verbatim: headings, tables, HTML, thematic breaks, link
	# reference definitions.
	/^[ \t]*(#|\||<|\[[^]]*\]:)/ { emit($0); next }
	/^[ \t]*([-*_][ \t]*){3,}$/  { emit($0); next }

	# Blockquotes. An alert marker must stay alone on its line; the rest of the
	# quote is one paragraph and joins, keeping the "> " prefix.
	/^[ \t]*>/ {
		body = $0
		sub(/^[ \t]*>[ \t]?/, "", body)
		if (body ~ /^\[!/) { emit($0); mode = "quote"; next }
		if (mode == "quote" && pending != "") { pending = pending " " body; next }
		flush()
		pending = "> " body
		mode = "quote"
		next
	}

	# A list bullet opens a new item. Its own text starts the line; indented
	# continuations below fold into it.
	/^[ \t]*([-*+][ \t]|[0-9]+[.)][ \t])/ { flush(); pending = $0; mode = "list"; next }

	# Indented and not continuing a list: a code block, left alone.
	/^(    |\t)/ { if (mode != "list") { emit($0); next } }

	{
		if (mode == "list" || mode == "prose") pending = pending " " strip($0)
		else { flush(); pending = $0; mode = "prose" }
		next
	}

	END { flush() }
' "$1"
