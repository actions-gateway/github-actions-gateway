#!/usr/bin/env bash
#
# workflow-step-body.sh — print one workflow step's `run:` body, dedented, so a
# driver can execute it (Q1006).
#
# `make check` runs targets over the tree and no target enters a workflow step's
# shell, so a `run:` body is first executed by the runner on the event that
# triggers it. For the eleven workflows with no `pull_request` trigger — measured
# by parsing each `on:` block, not by grepping, since release-freeze-watch.yml
# names `pull_request` only in a comment — that first execution is after merge,
# or at a `v*` tag for publish.yml. Neither actionlint nor shellcheck's
# integration executes a body, so a defect in what the script DECIDES is
# invisible to both: release-freeze-watch.yml reached review classifying its
# delegate's crash codes with `-eq 2` where they are 2 and above, and 7, 126 and
# 137 each fell through to the report step and opened an issue retiring a
# candidate nothing had measured.
#
# This is the extraction half. It reads the body out of the YAML through the
# block-scalar dedent so a driver can run it under `bash -e -o pipefail`, the
# shell Actions uses for a `run:` step, with stubs for whatever the body shells
# out to. scripts/ci/workflow-acting-steps-test.sh is that driver.
#
# Usage:
#   workflow-step-body.sh WORKFLOW SELECTOR
#
# SELECTOR is a step's `id:` or `name:` value, matched exactly. Every refusal
# below exits 2 rather than printing nothing, because an empty body drives
# clean and would make every case built on it vacuous:
#
#   * the workflow does not exist, or names no step matching SELECTOR;
#   * two steps match it, so which body ran would depend on file order;
#   * the step has no `run:` (a `uses:` step has no shell to drive);
#   * the body is a folded scalar (`>`), whose line joining this does not model;
#   * the body carries a `${{ }}` expression, which the runner substitutes
#     BEFORE bash sees it — so what a driver ran would not be what runs in CI.
#     Steps that take their inputs through `env:` are drivable; ones that
#     interpolate into the script text are not, and saying so is the point.
set -euo pipefail
shopt -s inherit_errexit

if (($# != 2)); then
	printf 'usage: workflow-step-body.sh WORKFLOW SELECTOR\n' >&2
	exit 2
fi

WORKFLOW="$1"
SELECTOR="$2"

if [[ ! -f "$WORKFLOW" ]]; then
	printf 'workflow-step-body: %s does not exist, so there is no body to extract\n' "$WORKFLOW" >&2
	exit 2
fi

# Indentation-driven rather than regex-driven: a step's keys sit two columns
# right of the `-` that opened it, and the body sits right of `run:`. Comparing
# measured indents avoids building a regex per nesting depth, and avoids the
# false match a bare /run:/ makes on a body line that mentions one.
awk -v selector="$SELECTOR" -v workflow="$WORKFLOW" '
function indent_of(s) { match(s, /^ */); return RLENGTH }

function trim(s) { gsub(/^[ \t]+|[ \t]+$/, "", s); return s }

function unquote(s) {
	if (s ~ /^".*"$/ || s ~ /^'"'"'.*'"'"'$/) return substr(s, 2, length(s) - 2)
	return s
}

function fail(msg) {
	printf "workflow-step-body: %s: %s\n", workflow, msg > "/dev/stderr"
	# awk runs END after exit, and END flushes the buffer again. Mark the
	# failure so it refuses there rather than reporting itself twice.
	failed = 1
	exit 2
}

# Decide whether the buffered step is the wanted one and, if so, capture its
# run: body. Deliberately does not print: a second match has to be a refusal,
# and that is only knowable once every step has been read.
function flush_step(   i, keyind, rest, val, runidx, runrest, bodyind, ind, line, matched) {
	if (nbuf == 0) return
	keyind = dashind + 2
	matched = 0
	runidx = 0
	for (i = 1; i <= nbuf; i++) {
		line = buf[i]
		if (line ~ /^[ \t]*$/) continue
		if (indent_of(line) != keyind) continue
		rest = substr(line, keyind + 1)
		if (rest ~ /^id:/) {
			val = unquote(trim(substr(rest, 4)))
			if (val == selector) matched = 1
		} else if (rest ~ /^name:/) {
			val = unquote(trim(substr(rest, 6)))
			if (val == selector) matched = 1
		} else if (rest ~ /^run:/) {
			runidx = i
			runrest = trim(substr(rest, 5))
		}
	}
	if (!matched) { nbuf = 0; return }
	if (++hits > 1) fail("two steps are named \"" selector "\", so which body runs depends on file order")
	if (runidx == 0) fail("step \"" selector "\" has no run: — a uses: step has no shell to drive")
	if (runrest ~ /^>/) fail("step \"" selector "\" uses a folded scalar (>), whose line joining this does not model")

	nbody = 0
	if (runrest !~ /^\|/) {
		# A plain one-line `run: cmd`. Nothing to dedent.
		if (runrest == "") fail("step \"" selector "\" has an empty run:")
		body[++nbody] = runrest
	} else {
		bodyind = -1
		for (i = runidx + 1; i <= nbuf; i++) {
			line = buf[i]
			if (line ~ /^[ \t]*$/) { if (bodyind >= 0) body[++nbody] = ""; continue }
			ind = indent_of(line)
			# A sibling key of run: ends the block scalar.
			if (ind <= keyind) break
			if (bodyind < 0) bodyind = ind
			if (ind < bodyind) fail("step \"" selector "\" has a run: line indented less than the block scalar opened")
			body[++nbody] = substr(line, bodyind + 1)
		}
		# A clipped block scalar (`|`) keeps one trailing newline and drops the
		# rest, so the blank lines separating this step from the next are not
		# part of the body the runner writes to disk.
		while (nbody > 0 && body[nbody] == "") nbody--
		if (nbody == 0) fail("step \"" selector "\" opened a block scalar with no body")
	}
	nbuf = 0
}

# The `steps:` key of any job. Only sequences directly under one are steps; a
# `with:` block carrying its own list must not be mistaken for them.
/^[ ]*steps:[ ]*$/ { insteps = 1; stepsind = indent_of($0); dashind = -1; next }

insteps && /^[ ]*[^ #]/ && indent_of($0) <= stepsind { flush_step(); insteps = 0 }

insteps {
	if ($0 ~ /^[ ]*-[ ]/) {
		ind = indent_of($0)
		if (dashind < 0) dashind = ind
		if (ind == dashind) {
			flush_step()
			# Normalise "- key: v" to "  key: v" so every key of the step
			# sits at one column and the loop above can compare indents.
			buf[++nbuf] = substr($0, 1, ind) "  " substr($0, ind + 3)
			next
		}
	}
	buf[++nbuf] = $0
}

END {
	if (failed) exit 2
	flush_step()
	if (hits == 0) fail("no step has id or name \"" selector "\"")
	for (i = 1; i <= nbody; i++) {
		if (body[i] ~ /\$\{\{/) fail("step \"" selector "\" interpolates a ${{ }} expression into its script, so a driven body is not the body CI runs — pass it through env: instead")
		print body[i]
	}
}
' "$WORKFLOW"
