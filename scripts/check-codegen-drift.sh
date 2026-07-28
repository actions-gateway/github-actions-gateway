#!/usr/bin/env bash
#
# Fail if a committed controller-gen manifest is stale relative to the Go types it
# is generated from (Q440).
#
# Each module's `make manifests` regenerates its CRD/RBAC/webhook YAML from the
# kubebuilder markers, but nothing ran that on a contributor's behalf — the
# committed YAML was only ever as fresh as the last person who remembered. The
# gap is worst ACROSS modules: cmd/gmc's ActionsGateway CRD embeds AGC types
# (RunnerGroupSpec), so a doc comment edited in cmd/agc/api changes the GMC
# manifest, and only `make -C cmd/gmc manifests` propagates it. #793 edited a
# quotaRetryDelay doc comment in the AGC type and the GMC CRD never caught up, so
# every later GMC contributor got that hunk as unrelated diff noise the moment
# they regenerated (Q440).
#
# This regenerates every registered module's manifests into a scratch tree and
# diffs them against the committed copies. It NEVER writes into the working tree,
# so it detects drift in the committed manifests (and any uncommitted hand-edit),
# not merely whether a regen-in-place produced a git diff.
#
# Three assertions, cheapest first:
#
#   1. Registry completeness. Every first-party module whose Makefile defines a
#      `manifests:` target is registered below. A new module generating manifests
#      fails this gate until someone registers it, so the hole cannot reopen.
#   2. Registry fidelity. Each row's generator list and explicit output rules
#      match that module's own `manifests:` recipe, so this gate regenerates
#      exactly what `make manifests` would rather than a stale approximation.
#   3. Drift. Every regenerated file matches its committed counterpart, and every
#      committed file under a generated output dir is either produced by that
#      module's controller-gen run or listed in EXEMPT with a reason.
#
# Scope is the MANIFESTS half of codegen. DeepCopy (`make generate`, the
# controller-gen `object` generator) writes zz_generated.deepcopy.go beside its
# source rather than into a redirectable output dir, and its drift is
# intra-module — a type change that needs new DeepCopy code fails to compile.
#
# Costs about two seconds (three controller-gen runs over already-parsed
# packages) plus the one-time .build/controller-gen build. Backs
# `make codegen-check` (part of `make check`) and the `lint` job in
# .github/workflows/unit-test.yml.
#
#   scripts/check-codegen-drift.sh              # run the three assertions
#   CONTROLLER_GEN=/path/to/controller-gen ...  # override the binary (make passes it)
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

CONTROLLER_GEN="${CONTROLLER_GEN:-$REPO_ROOT/.build/controller-gen}"

# One row per module that generates manifests, as three '|'-separated fields:
#
#   <module dir>|<controller-gen generators>|<kind>=<committed output dir> ...
#
# The generators and output dirs mirror the module's `manifests:` target;
# assertion 2 holds them there. A kind with no explicit `output:` rule in the
# recipe (gmc's webhook, which uses controller-gen's default) still needs its
# committed dir named here so this gate can redirect it away from the tree.
MODULES=(
	"api|crd|crd=config/crd"
	"cmd/agc|crd rbac:roleName=agc-role|crd=config/crd rbac=config/rbac"
	"cmd/gmc|rbac:roleName=manager-role crd webhook|crd=config/crd/bases rbac=config/rbac webhook=config/webhook"
)

# Committed files that live under a generated output dir but are NOT produced by
# that module's controller-gen run. Each entry needs a reason: an unexplained one
# is indistinguishable from a manifest whose type was deleted.
EXEMPT=(
	# A bundled copy of the AGC-owned RunnerGroup CRD, not GMC controller-gen
	# output — controller-gen walks only the GMC module's own packages. It is held
	# byte-identical to cmd/agc/config/crd/ by `make chart-crds-check` (Q73).
	"cmd/gmc/config/crd/bases/actions-gateway.github.com_runnergroups.yaml"
)

# Scratch tree for the regenerated manifests. Created by main() rather than at
# load time so check-codegen-drift-test.sh can source this file for its parsing
# helpers without inheriting a temp dir or an EXIT trap of its own.
GEN_TMP=""

RC=0

fail() {
	echo "ERROR: $*" >&2
	RC=1
}

# module_dirs — print the disk path of every registered module, one per line.
module_dirs() {
	local row
	for row in "${MODULES[@]}"; do
		printf '%s\n' "${row%%|*}"
	done
}

# manifests_recipe MODULE — print MODULE's `manifests:` recipe as one line with
# the shell comments stripped, the line continuations folded away, and every tab
# turned into a space, so a token can be matched as " token " regardless of how
# the Makefile wraps and indents it.
#
# Comments go by the shell's rule, because the shell is what runs a recipe line:
# an unquoted '#' at the start of a word begins a comment, and a '#' that is
# quoted, escaped, or mid-word is ordinary text. Everything from that '#' to the
# end of the physical line is dropped — including a trailing backslash, which
# continues nothing once it is inside a comment. A line that is nothing but a
# comment contributes nothing to the fold.
#
# Without this, a commented-out call folded in as live text: its '#' and its
# generators read as generators this gate does not regenerate, so the module was
# reported unfaithful for running a generator named '#', and a commented-out
# output: rule satisfied the dir match that the live rule should have (Q464).
manifests_recipe() {
	local module="$1"
	awk '
		# strip_comment LINE — LINE truncated at its first shell comment. Quote
		# state is tracked within the line only: a quote left open across a
		# backslash continuation is not a shape make recipes take here.
		function strip_comment(line,   i, c, n, quote, prev) {
			n = length(line)
			quote = ""
			prev = " " # start of line is a word boundary
			for (i = 1; i <= n; i++) {
				c = substr(line, i, 1)
				if (quote == "\047") {
					# Single quotes take everything literally, backslash included.
					if (c == "\047") { quote = "" }
				} else if (quote == "\"") {
					if (c == "\\") { i++; prev = "x"; continue }
					if (c == "\"") { quote = "" }
				} else if (c == "\\") {
					# The escaped character is literal, so it cannot open a comment
					# and it is not whitespace for the word-start test.
					i++
					prev = "x"
					continue
				} else if (c == "\047" || c == "\"") {
					quote = c
				} else if (c == "#" && (prev == " " || prev == "\t")) {
					return substr(line, 1, i - 1)
				}
				prev = c
			}
			return line
		}
		/^manifests:/ { inrecipe = 1; next }
		inrecipe && /^\t/ {
			line = strip_comment($0)
			if (line ~ /^[ \t]*$/) { next }
			sub(/\\$/, "", line)
			gsub(/\t/, " ", line)
			printf "%s ", line
			next
		}
		inrecipe { exit }
	' "$module/Makefile"
}

# recipe_generators RECIPE — print the controller-gen generator tokens in RECIPE,
# one per line: everything that is not the binary, the GOWORK assignment, a
# paths= argument, or an output: rule.
recipe_generators() {
	local token
	# Unquoted on purpose: the recipe is a command line, and splitting it on
	# whitespace is exactly how make hands it to the shell.
	# shellcheck disable=SC2086 # deliberate split: the argument is a command line
	for token in $1; do
		case "$token" in
		GOWORK=* | *CONTROLLER_GEN* | paths=* | output:*) ;;
		*) printf '%s\n' "$token" ;;
		esac
	done
}

# assert_registry_complete — every first-party Makefile defining a `manifests:`
# target must be registered in MODULES.
assert_registry_complete() {
	local makefile module registered
	while IFS= read -r makefile; do
		module="$(dirname "$makefile")"
		case "$module" in . | vendor/* | */vendor/*) continue ;; esac
		grep -qE '^manifests:' "$makefile" || continue
		registered="$(module_dirs | grep -Fx "$module" || true)"
		if [[ -z "$registered" ]]; then
			fail "$module/Makefile defines a 'manifests:' target but $module is not registered in
  scripts/check-codegen-drift.sh — its generated manifests are unguarded. Add a MODULES row
  naming its controller-gen generators and output dirs."
		fi
	done < <(git ls-files '*Makefile')
}

# assert_registry_fidelity MODULE GENERATORS OUTPUTS — the row must describe the
# same controller-gen invocation the module's `manifests:` target runs.
assert_registry_fidelity() {
	local module="$1" generators="$2" outputs="$3"
	local recipe gen pair kind dir rule
	recipe="$(manifests_recipe "$module")"
	if [[ -z "$recipe" ]]; then
		fail "$module/Makefile has no 'manifests:' recipe, but $module is registered in
  scripts/check-codegen-drift.sh. Drop the MODULES row, or restore the target."
		return
	fi
	# Registered generators must all appear in the recipe, and vice versa.
	# shellcheck disable=SC2086 # deliberate split: the field is a generator list
	for gen in $generators; do
		if ! grep -qF -- " $gen " <<<" $recipe "; then
			fail "$module: generator '$gen' is registered in scripts/check-codegen-drift.sh but
  absent from '$module/Makefile' manifests:. Re-sync the MODULES row with the recipe."
		fi
	done
	while IFS= read -r gen; do
		[[ -n "$gen" ]] || continue
		if ! grep -qF -- " $gen " <<<" $generators "; then
			fail "$module: '$module/Makefile' manifests: runs generator '$gen', which the MODULES row
  in scripts/check-codegen-drift.sh omits — this gate would not regenerate its output.
  Add it to the row."
		fi
	done < <(recipe_generators "$recipe")
	# Every explicit output rule in the recipe must match the registered dir.
	# shellcheck disable=SC2086 # deliberate split: the field is a kind=dir list
	for pair in $outputs; do
		kind="${pair%%=*}"
		dir="${pair#*=}"
		rule="output:$kind:artifacts:config="
		if grep -qF -- "$rule" <<<"$recipe" && ! grep -qF -- "$rule$dir " <<<"$recipe "; then
			fail "$module: the MODULES row in scripts/check-codegen-drift.sh sends '$kind' output to
  '$dir', but '$module/Makefile' manifests: names a different dir. Re-sync the row."
		fi
	done
}

# is_exempt PATH — true when PATH is a registered non-generated committed file.
is_exempt() {
	local path="$1" entry
	for entry in "${EXEMPT[@]}"; do
		[[ "$entry" == "$path" ]] && return 0
	done
	return 1
}

# regenerate MODULE GENERATORS OUTPUTS — run the module's controller-gen
# invocation with every output kind redirected into the scratch tree.
regenerate() {
	local module="$1" generators="$2" outputs="$3"
	local pair kind
	local -a args=()
	# shellcheck disable=SC2206 # deliberate split: the field is a generator list
	args=($generators)
	# shellcheck disable=SC2086 # deliberate split: the field is a kind=dir list
	for pair in $outputs; do
		kind="${pair%%=*}"
		mkdir -p "$GEN_TMP/$module/$kind"
		args+=("output:$kind:artifacts:config=$GEN_TMP/$module/$kind")
	done
	args+=('paths=./...')
	# GOWORK points at the module's go.work.gen, matching its manifests: target —
	# controller-gen loads packages, so it needs the generation workspace, not the
	# ambient one (docs/development/code-generation.md).
	(cd "$module" && GOWORK="$PWD/go.work.gen" "$CONTROLLER_GEN" "${args[@]}")
}

# assert_no_drift MODULE OUTPUTS — diff each regenerated file against its
# committed counterpart, both directions.
assert_no_drift() {
	local module="$1" outputs="$2"
	local pair kind dir produced committed base
	# shellcheck disable=SC2086 # deliberate split: the field is a kind=dir list
	for pair in $outputs; do
		kind="${pair%%=*}"
		dir="${pair#*=}"
		for produced in "$GEN_TMP/$module/$kind"/*; do
			[[ -e "$produced" ]] || continue
			committed="$module/$dir/$(basename "$produced")"
			if [[ ! -f "$committed" ]]; then
				fail "$module: controller-gen produces $(basename "$produced") but $committed is not
  committed. Run 'make -C $module manifests' and commit the result."
				continue
			fi
			if ! diff -u "$committed" "$produced"; then
				fail "$committed is stale — it does not match what controller-gen generates from
  today's Go types. This is expected after a type or doc-comment change in ANY module whose
  types $module embeds. Fix: run 'make -C $module manifests', then 'make chart-crds' to carry
  a CRD change into the Helm chart, and commit both."
			fi
		done
		for committed in "$module/$dir"/*; do
			[[ -e "$committed" ]] || continue
			base="$(basename "$committed")"
			[[ -e "$GEN_TMP/$module/$kind/$base" ]] && continue
			is_exempt "$committed" && continue
			fail "$committed is committed under a generated output dir but controller-gen no longer
  produces it — likely a deleted type or a renamed kind. Delete it, or add it to EXEMPT in
  scripts/check-codegen-drift.sh with the reason it is not generated here."
		done
	done
}

main() {
	if [[ ! -x "$CONTROLLER_GEN" ]]; then
		echo "controller-gen not found at $CONTROLLER_GEN — build it with 'make tools'." >&2
		exit 1
	fi

	GEN_TMP="$(mktemp -d)"
	trap 'rm -rf "$GEN_TMP"' EXIT INT TERM

	assert_registry_complete

	local row module generators outputs
	for row in "${MODULES[@]}"; do
		IFS='|' read -r module generators outputs <<<"$row"
		assert_registry_fidelity "$module" "$generators" "$outputs"
		regenerate "$module" "$generators" "$outputs"
		assert_no_drift "$module" "$outputs"
	done

	if [[ "$RC" -ne 0 ]]; then
		exit 1
	fi
	echo "committed CRD/RBAC/webhook manifests match controller-gen output for: $(module_dirs | tr '\n' ' ')"
}

# Run main only when executed directly, so check-codegen-drift-test.sh can source
# this file to exercise the recipe parsing and the registry assertions against
# fixtures without building controller-gen or regenerating anything.
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
	main "$@"
fi
