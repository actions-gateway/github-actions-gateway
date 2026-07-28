#!/usr/bin/env bash
#
# Unit tests for the make-recipe parsing in scripts/check-codegen-drift.sh (Q457):
# manifests_recipe (tab folding, line continuations, comment stripping, where a
# recipe ends), recipe_generators (which tokens count as controller-gen
# generators), and assert_registry_fidelity, the consumer that turns a parse into
# a pass or a failure.
#
# The gate reads `manifests:` recipes straight out of Makefiles, and make recipes
# are tab-sensitive — a class of bug that is invisible to review and obvious to a
# fixture. #886, the PR that added the gate, nearly shipped a parser that folded
# continuation lines without converting their tabs, which hides every generator
# that begins a wrapped line; see the tab-wrapped-generator cases below. Both
# real recipes happen to put every generator on the first line today, so that bug
# would have lain dormant until the first rewrap.
#
# The gate is only worth having if it fails when it should, so these assertions
# are the permanent form of the invert-the-fix verification
# (docs/development/testing.md § Diagnosing failures). None of them need
# controller-gen: they stop at the parse and the registry assertions. Runs under
# `make check` (via `make scripts-test`) and the CI shellcheck job.
#
# File-wide: every fixture body is make source text, so $(CONTROLLER_GEN) and
# $(CURDIR) are make variable references that must reach the fixture Makefile
# unexpanded — single quotes are the point, not an oversight.
# shellcheck disable=SC2016
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"
# Source the script under test for its helpers; the BASH_SOURCE guard there keeps
# main() from running (and from demanding a built controller-gen).
# shellcheck source=scripts/check-codegen-drift.sh
source "$REPO_ROOT/scripts/check-codegen-drift.sh"

FIXTURE_ROOT="$REPO_ROOT/tmp/codegen-drift-test.$$"
mkdir -p "$FIXTURE_ROOT"
trap 'rm -rf "$FIXTURE_ROOT"' EXIT INT TERM

fails=0

# fixture NAME BODY — write BODY as $FIXTURE_ROOT/NAME/Makefile and echo the dir.
# BODY goes through printf '%b', so every tab is spelled as a literal \t escape:
# this suite is about tab handling, and a real tab in the source would be exactly
# the invisible character under test.
fixture() {
	local name="$1" body="$2"
	local dir="$FIXTURE_ROOT/$name"
	mkdir -p "$dir"
	printf '%b' "$body" >"$dir/Makefile"
	printf '%s' "$dir"
}

# expect_recipe NAME BODY WANT — assert manifests_recipe folds BODY to exactly
# WANT. WANT carries the folded line's leading and trailing spaces; the report
# brackets both sides so a whitespace-only mismatch is legible.
expect_recipe() {
	local name="$1" body="$2" want="$3" dir got
	dir="$(fixture "$name" "$body")"
	got="$(manifests_recipe "$dir")"
	if [[ "$got" == "$want" ]]; then
		printf 'ok   recipe %-30s\n' "$name"
	else
		printf 'FAIL recipe %-30s\n  want=[%s]\n  got =[%s]\n' "$name" "$want" "$got" >&2
		fails=$((fails + 1))
	fi
}

# expect_generators NAME RECIPE WANT — assert recipe_generators reduces RECIPE to
# WANT, a newline-separated generator list.
expect_generators() {
	local name="$1" recipe="$2" want="$3" got
	got="$(recipe_generators "$recipe")"
	if [[ "$got" == "$want" ]]; then
		printf 'ok   gens   %-30s -> [%s]\n' "$name" "${got//$'\n'/ }"
	else
		printf 'FAIL gens   %-30s want=[%s] got=[%s]\n' \
			"$name" "${want//$'\n'/ }" "${got//$'\n'/ }" >&2
		fails=$((fails + 1))
	fi
}

# expect_fidelity NAME WANT DIR GENERATORS OUTPUTS — run assert_registry_fidelity
# over a fixture module and assert it passed or failed. WANT is 'pass' or 'fail'.
# The call is NOT wrapped in $(...): it reports through the global RC, which a
# subshell would discard. Its stderr lands in a file so expect_message can grep
# the operator-facing text.
expect_fidelity() {
	local name="$1" want="$2" dir="$3" generators="$4" outputs="$5" got
	LAST_ERR="$FIXTURE_ROOT/$name.err"
	RC=0
	assert_registry_fidelity "$dir" "$generators" "$outputs" 2>"$LAST_ERR"
	if ((RC == 0)); then got=pass; else got=fail; fi
	if [[ "$got" == "$want" ]]; then
		printf 'ok   fidelity %-28s -> %s\n' "$name" "$got"
	else
		printf 'FAIL fidelity %-28s want=%s got=%s\n%s\n' \
			"$name" "$want" "$got" "$(cat "$LAST_ERR")" >&2
		fails=$((fails + 1))
	fi
}

# expect_message NAME PATTERN — assert the last fidelity run explained itself.
expect_message() {
	local name="$1" pattern="$2"
	if grep -q -- "$pattern" "$LAST_ERR"; then
		printf 'ok   message  %-28s reported %q\n' "$name" "$pattern"
	else
		printf 'FAIL message  %-28s did not mention %q\n%s\n' \
			"$name" "$pattern" "$(cat "$LAST_ERR")" >&2
		fails=$((fails + 1))
	fi
}

# --- manifests_recipe: folding ------------------------------------------------

# The simplest shape: one tab-indented line. The leading tab becomes a space, and
# the fold appends one, so a token is matchable as " token " at either end.
expect_recipe single-line \
	'manifests:\n\t$(CONTROLLER_GEN) crd paths="./..." output:crd:artifacts:config=config/crd\n' \
	' $(CONTROLLER_GEN) crd paths="./..." output:crd:artifacts:config=config/crd '

# REGRESSION (#886): a generator that begins a wrapped continuation line. The
# parser this suite guards converts the continuation's leading tabs to spaces; the
# one #886 nearly shipped did not, leaving 'rbac:roleName=agc-role' preceded by a
# literal tab. assert_registry_fidelity matches generators as " $gen " with
# grep -F, so a tab-prefixed generator is simply not found and the row is called
# unfaithful — a gate that fails every build the moment someone rewraps a recipe.
# Drop the gsub(/\t/, " ", line) from manifests_recipe and this case fails.
expect_recipe tab-wrapped-generator \
	'manifests:\n\t$(CONTROLLER_GEN) crd \\\n\t\trbac:roleName=agc-role paths="./..." \\\n\t\toutput:crd:artifacts:config=config/crd\n' \
	' $(CONTROLLER_GEN) crd    rbac:roleName=agc-role paths="./..."    output:crd:artifacts:config=config/crd '

# Quoting is preserved verbatim — the fold is textual, not a shell parse. This is
# cmd/gmc's real shape, where the binary is quoted and paths= carries quotes.
expect_recipe quoted-binary-and-paths \
	'manifests:\n\tGOWORK=$(CURDIR)/go.work.gen "$(CONTROLLER_GEN)" rbac:roleName=manager-role crd webhook paths="./..." \\\n\t\toutput:crd:artifacts:config=config/crd/bases\n' \
	' GOWORK=$(CURDIR)/go.work.gen "$(CONTROLLER_GEN)" rbac:roleName=manager-role crd webhook paths="./..."    output:crd:artifacts:config=config/crd/bases '

# The recipe ends at the next target — a following rule is never folded in.
expect_recipe stops-at-next-target \
	'manifests:\n\t$(CONTROLLER_GEN) crd paths="./..."\nother:\n\t@echo nope\n' \
	' $(CONTROLLER_GEN) crd paths="./..." '

# A blank line also ends it. make itself tolerates blank lines inside a recipe, so
# this truncates early — but it truncates toward a LOUD failure: the registered
# generators on the dropped lines go missing and assert_registry_fidelity says so,
# rather than quietly regenerating less than `make manifests` does.
expect_recipe blank-line-ends-recipe \
	'manifests:\n\t$(CONTROLLER_GEN) crd paths="./..."\n\n\t$(CONTROLLER_GEN) webhook paths="./..."\n' \
	' $(CONTROLLER_GEN) crd paths="./..." '

# A module with no manifests: target parses to nothing. assert_registry_fidelity
# turns that into the "drop the row, or restore the target" failure below.
expect_recipe no-manifests-target \
	'generate:\n\t$(CONTROLLER_GEN) object paths="./..."\n' \
	''

# A prerequisite list on the target line is skipped with it, not folded in.
expect_recipe target-with-prerequisites \
	'manifests: $(CONTROLLER_GEN) ## regenerate\n\t$(CONTROLLER_GEN) crd paths="./..."\n' \
	' $(CONTROLLER_GEN) crd paths="./..." '

# --- manifests_recipe: comment stripping (Q464) -------------------------------
#
# A recipe line is handed to the shell, so the shell's comment rule is the one
# that decides what make actually runs: an unquoted '#' at the start of a word
# begins a comment. manifests_recipe strips exactly that much. Before Q464 it
# stripped nothing, so a commented-out call contributed its '#' and its
# generators to the fold as live tokens.

# The motivating shape: a call commented out in favour of a hand-maintained
# manifest. The whole line is a comment, so it contributes nothing at all —
# folding it in made assert_registry_fidelity reject the module for running a
# generator named '#'.
expect_recipe commented-out-call \
	'manifests:\n\t# $(CONTROLLER_GEN) crd paths="./..."\n\t@echo "hand-maintained here"\n' \
	' @echo "hand-maintained here" '

# A trailing comment on a live call: the call survives, the note does not.
expect_recipe trailing-comment \
	'manifests:\n\t$(CONTROLLER_GEN) crd paths="./..." # webhook is hand-maintained\n' \
	' $(CONTROLLER_GEN) crd paths="./..."  '

# A quoted '#' is ordinary text, in either quote style — over-stripping would
# truncate a live call at the first '#' inside an echo.
expect_recipe hash-in-double-quotes \
	'manifests:\n\t@echo "roleName=agc-role # not a comment" && $(CONTROLLER_GEN) crd\n' \
	' @echo "roleName=agc-role # not a comment" && $(CONTROLLER_GEN) crd '
# Single quotes in a single-quoted bash string: '\'' closes, escapes, reopens.
expect_recipe hash-in-single-quotes \
	'manifests:\n\t@echo '\''sharp # sign'\'' && $(CONTROLLER_GEN) crd\n' \
	' @echo '\''sharp # sign'\'' && $(CONTROLLER_GEN) crd '

# A backslash-escaped '#' is ordinary text too. The body spells it '\\#' because
# printf '%b' collapses the pair; the wanted output carries the single backslash
# the fixture Makefile actually holds.
expect_recipe escaped-hash \
	'manifests:\n\t@echo \\# && $(CONTROLLER_GEN) crd\n' \
	' @echo \# && $(CONTROLLER_GEN) crd '

# A '#' mid-word starts no comment — the shell only takes one at a word start.
expect_recipe hash-mid-word \
	'manifests:\n\t@echo id#42 && $(CONTROLLER_GEN) crd\n' \
	' @echo id#42 && $(CONTROLLER_GEN) crd '

# A comment that ends in a backslash does NOT continue: the shell discards
# everything to the newline, backslash included, and reads the next line as a
# command. So the commented call vanishes and the live one below it survives.
expect_recipe comment-swallows-its-continuation \
	'manifests:\n\t# $(CONTROLLER_GEN) webhook \\\n\t$(CONTROLLER_GEN) crd paths="./..."\n' \
	' $(CONTROLLER_GEN) crd paths="./..." '

# A make comment at column 0 — no tab — ends the recipe here, though make itself
# would ignore it and keep reading. Same trade as blank-line-ends-recipe above,
# and the same reason it is tolerable: it truncates toward a LOUD failure, since
# the generators on the dropped lines then read as unregistered.
expect_recipe make-comment-line-ends-recipe \
	'manifests:\n\t$(CONTROLLER_GEN) crd paths="./..."\n# a make comment\n\t$(CONTROLLER_GEN) webhook paths="./..."\n' \
	' $(CONTROLLER_GEN) crd paths="./..." '

# --- recipe_generators: token classification ----------------------------------

# The binary, the GOWORK assignment, paths= and every output: rule drop out;
# what remains is the generator list, quoting and all.
expect_generators gmc-shape \
	' GOWORK=$(CURDIR)/go.work.gen "$(CONTROLLER_GEN)" rbac:roleName=manager-role crd webhook paths="./..."    output:crd:artifacts:config=config/crd/bases ' \
	$'rbac:roleName=manager-role\ncrd\nwebhook'

# The unquoted binary is filtered by the same *CONTROLLER_GEN* glob.
expect_generators bare-binary ' $(CONTROLLER_GEN) crd ' 'crd'

# Splitting is whitespace-general, so a stray tab still separates tokens. The tab
# bug above was never in this split — it was in the " $gen " grep that consumes
# this function's input, which is why manifests_recipe is where tabs must die.
expect_generators tabs-split-like-spaces $' $(CONTROLLER_GEN)\tcrd\twebhook ' $'crd\nwebhook'

# An empty recipe yields no generators at all.
expect_generators empty-recipe '' ''

# This classifier is deliberately dumb about comments: by the time it runs,
# manifests_recipe has already stripped them (Q464), so a '#' can only reach here
# from a caller passing raw make source. Pinned so the responsibility stays in
# one place — comment syntax is a line-level concern, and duplicating it in a
# whitespace-split token filter is where the two would drift apart.
expect_generators raw-comment-not-filtered-here \
	' # $(CONTROLLER_GEN) crd paths="./..." ' \
	$'#\ncrd'

# --- assert_registry_fidelity: the parse in service of the gate ---------------

# REGRESSION (#886), end to end: a row that faithfully describes a tab-wrapped
# recipe must pass. This is the assertion that would have caught the dropped
# gsub — with it, 'rbac:roleName=agc-role' is invisible and the row is rejected.
wrapped="$(fixture fidelity-tab-wrapped \
	'manifests:\n\t$(CONTROLLER_GEN) crd \\\n\t\trbac:roleName=agc-role paths="./..." \\\n\t\toutput:crd:artifacts:config=config/crd \\\n\t\toutput:rbac:artifacts:config=config/rbac\n')"
expect_fidelity tab-wrapped-row-is-faithful pass "$wrapped" \
	'crd rbac:roleName=agc-role' 'crd=config/crd rbac=config/rbac'

# A generator registered in MODULES but absent from the recipe: the row claims
# output this gate would regenerate and `make manifests` would not.
expect_fidelity registered-generator-absent fail "$wrapped" \
	'crd rbac:roleName=agc-role webhook' 'crd=config/crd rbac=config/rbac'
expect_message registered-generator-absent 'is registered in'

# The reverse hole, and the dangerous one: the recipe runs a generator the row
# omits, so the gate would never regenerate its output and its drift would go
# unseen.
expect_fidelity recipe-generator-unregistered fail "$wrapped" \
	'crd' 'crd=config/crd rbac=config/rbac'
expect_message recipe-generator-unregistered 'omits'

# An output rule pointing somewhere the row does not name: the gate would diff
# against the wrong committed dir.
expect_fidelity output-dir-mismatch fail "$wrapped" \
	'crd rbac:roleName=agc-role' 'crd=config/crd/bases rbac=config/rbac'
expect_message output-dir-mismatch 'names a different dir'

# A kind with no explicit output: rule in the recipe (gmc's webhook, which takes
# controller-gen's default) is still allowed to name its committed dir.
defaulted="$(fixture fidelity-defaulted-output \
	'manifests:\n\t$(CONTROLLER_GEN) crd webhook paths="./..." \\\n\t\toutput:crd:artifacts:config=config/crd\n')"
expect_fidelity defaulted-output-kind-allowed pass "$defaulted" \
	'crd webhook' 'crd=config/crd webhook=config/webhook'

# REGRESSION (Q464), end to end: a commented-out call must contribute nothing.
# Before the fix the fold made this row unfaithful for omitting the generators
# '#' and 'webhook' — a gate failing over a call make never runs, and naming a
# generator that does not exist.
commented="$(fixture fidelity-commented-out-call \
	'manifests:\n\t# $(CONTROLLER_GEN) webhook paths="./..."\n\t$(CONTROLLER_GEN) crd paths="./..." \\\n\t\toutput:crd:artifacts:config=config/crd\n')"
expect_fidelity commented-out-call-ignored pass "$commented" \
	'crd' 'crd=config/crd'

# The over-coverage direction, and the one the fold silently allowed: the row
# registers a generator that survives only as a comment. The " $gen " presence
# grep matched the commented copy, so the gate regenerated webhook output and
# diffed it against a committed dir `make manifests` no longer writes.
expect_fidelity commented-generator-is-absent fail "$commented" \
	'crd webhook' 'crd=config/crd webhook=config/webhook'
expect_message commented-generator-is-absent 'is registered in'

# The same fold hid a real fault in the other half of the assertion: a
# commented-out output: rule satisfied the row's dir match, so a row pointing at
# the wrong committed dir passed and the gate would have diffed against it. The
# live rule is the only one that counts, so this must fail — and fail with the
# dir message, not with the '#'-as-generator noise it produced before.
stale_dir_note="$(fixture fidelity-commented-output-rule \
	'manifests:\n\t$(CONTROLLER_GEN) crd paths="./..." \\\n\t\toutput:crd:artifacts:config=config/crd/bases # was output:crd:artifacts:config=config/crd\n')"
expect_fidelity commented-output-rule-ignored fail "$stale_dir_note" \
	'crd' 'crd=config/crd'
expect_message commented-output-rule-ignored 'names a different dir'

# A registered module whose manifests: target has gone away.
gone="$(fixture fidelity-no-target 'generate:\n\t$(CONTROLLER_GEN) object paths="./..."\n')"
expect_fidelity no-manifests-recipe fail "$gone" 'crd' 'crd=config/crd'
expect_message no-manifests-recipe "no 'manifests:' recipe"

# --- the tracked tree ---------------------------------------------------------

# Every real MODULES row still describes its module's real recipe, and every
# first-party Makefile with a manifests: target is registered. This is the gate's
# own assertions 1 and 2 over the committed tree — assertion 3 (drift) needs
# controller-gen and stays in `make codegen-check`.
RC=0
assert_registry_complete 2>"$FIXTURE_ROOT/tree.err"
for row in "${MODULES[@]}"; do
	IFS='|' read -r module generators outputs <<<"$row"
	assert_registry_fidelity "$module" "$generators" "$outputs" 2>>"$FIXTURE_ROOT/tree.err"
done
if ((RC == 0)); then
	printf 'ok   tree     %-28s %s\n' registry-matches-makefiles "$(module_dirs | tr '\n' ' ')"
else
	printf 'FAIL tree     %-28s registry and Makefiles disagree\n%s\n' \
		registry-matches-makefiles "$(cat "$FIXTURE_ROOT/tree.err")" >&2
	fails=$((fails + 1))
fi

if ((fails > 0)); then
	printf '\ncheck-codegen-drift-test: %d assertion(s) failed\n' "$fails" >&2
	exit 1
fi
printf '\ncheck-codegen-drift-test: all assertions passed\n'
