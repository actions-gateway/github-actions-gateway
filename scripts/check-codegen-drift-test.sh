#!/usr/bin/env bash
#
# Unit tests for the make-recipe parsing in scripts/check-codegen-drift.sh (Q457):
# manifests_recipe (tab folding, line continuations, where a recipe ends),
# recipe_generators (which tokens count as controller-gen generators), and
# assert_registry_fidelity, the consumer that turns a parse into a pass or a
# failure.
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

# A commented-out call is folded in like any other recipe line — the parser is
# textual and does not know '#' starts a shell comment. Pinned, not endorsed: see
# the commented-out-call-leaks-tokens case below for what it costs.
expect_recipe commented-out-call \
	'manifests:\n\t# $(CONTROLLER_GEN) crd paths="./..."\n\t@echo "hand-maintained here"\n' \
	' # $(CONTROLLER_GEN) crd paths="./..."  @echo "hand-maintained here" '

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

# A commented-out call leaks its '#' and its generator as live tokens. The '#'
# then reads as an unregistered generator, so the gate fails loudly rather than
# silently skipping the commented generator — but the message names '#', which is
# not the real problem. Queue: Q464.
expect_generators commented-out-call-leaks-tokens \
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
