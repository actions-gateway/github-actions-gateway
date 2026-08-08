#!/usr/bin/env bash
#
# Fail if committed controller-gen output is stale relative to the Go types it is
# generated from (Q440, Q477).
#
# Each module's `make generate` regenerates its CRD/RBAC/webhook YAML and its
# zz_generated.deepcopy.go from the kubebuilder markers, but nothing ran that on a
# contributor's behalf — the committed output was only ever as fresh as the last
# person who remembered. For manifests the gap is worst ACROSS modules: cmd/gmc's
# ActionsGateway CRD embeds AGC types (RunnerGroupSpec), so a doc comment edited
# in cmd/agc/api changes the GMC manifest, and only `make -C cmd/gmc manifests`
# propagates it. #793 edited a quotaRetryDelay doc comment in the AGC type and the
# GMC CRD never caught up, so every later GMC contributor got that hunk as
# unrelated diff noise the moment they regenerated (Q440).
#
# This regenerates every registered module's output into a scratch tree and diffs
# it against the committed copies. It NEVER writes into the working tree, so it
# detects drift in the committed output (and any uncommitted hand-edit), not
# merely whether a regen-in-place produced a git diff.
#
# Three assertions, cheapest first, each over both halves of codegen:
#
#   1. Registry completeness. Every first-party module whose Makefile defines a
#      `manifests:` target is registered in MODULES, and every one defining a
#      `deepcopy:` target is registered in DEEPCOPY_MODULES. A new module
#      generating either fails this gate until someone registers it, so the hole
#      cannot reopen.
#   2. Registry fidelity. Each registration's generator list and explicit output
#      rules match that module's own recipe, so this gate regenerates exactly what
#      `make manifests` / `make deepcopy` would rather than a stale approximation.
#   3. Drift. Every regenerated file matches its committed counterpart; every
#      committed file under a generated output dir is either produced by that
#      module's controller-gen run or listed in EXEMPT with a reason; and every
#      committed zz_generated.deepcopy.go is still produced, with the same bytes.
#
# The DeepCopy half was unguarded until Q477, on the reasoning that a type needing
# new DeepCopy code fails to compile without it. That is false for an ADDED type:
# ClusterCapacity (Q470, #917) shipped with no DeepCopy methods at all and an
# ActionsGatewaySpec.DeepCopyInto that never copied the field, so
# ActionsGateway.DeepCopy() returned an object aliasing the caller's pointer into
# the shared informer cache. Nothing failed to compile, and nothing failed CI.
#
# Costs about four seconds (six controller-gen runs over already-parsed packages,
# plus one ~30 MB copy of the working tree) and the one-time .build/controller-gen
# build. Backs `make codegen-check` (part of `make check`) and the `lint` job in
# .github/workflows/unit-test.yml.
#
#   scripts/go/check-codegen-drift.sh              # run the three assertions
#   CONTROLLER_GEN=/path/to/controller-gen ...  # override the binary (make passes it)
set -euo pipefail
shopt -s inherit_errexit

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

# One entry per module whose `deepcopy:` target regenerates zz_generated.deepcopy.go.
# There is no output dir to register: the controller-gen `object` generator writes
# beside its source, which is also why this half needs a tree copy rather than the
# output redirection the manifests half uses (see copy_tree).
#
# DEPENDENCY ORDER IS LOAD-BEARING. regenerate_deepcopy deletes a module's
# committed DeepCopy from the copy before regenerating it, so every module must be
# regenerated before any module that imports it — cmd/agc and cmd/gmc both resolve
# `api` through a relative `replace` and would otherwise load it mid-deletion.
DEEPCOPY_MODULES=(api cmd/agc cmd/gmc)

# The controller-gen call every module's `deepcopy:` recipe runs, spelled exactly
# as the Makefiles spell it: $(YEAR) unexpanded and the shell quoting intact, so
# assertion 2 can match it against the recipe text. deepcopy_generator() turns it
# into the argument controller-gen actually receives.
#
# One shared value because all three modules run the identical call by contract —
# a module that needs its own would make this a per-module field, not a reason to
# leave the difference unregistered.
# shellcheck disable=SC2016 # $(YEAR) is make source text; expanding it here is the bug
DEEPCOPY_GENERATOR='object:headerFile="hack/boilerplate.go.txt",year=$(YEAR)'

# The file the `object` generator writes into each API package.
DEEPCOPY_FILE='zz_generated.deepcopy.go'

# Working-tree entries copy_tree leaves out: git's own state, build output, the
# gitignored scratch dir, and two trees no module's DeepCopy run reads. The
# vendored trees are safe to drop because each module's go.work.gen is a
# single-module workspace ('use .'), so the repo-root workspace vendor/ never
# applies, and tools/ is built with GOWORK=off against its own vendor/. Dropping
# them takes the copy from ~330 MB to ~30 MB.
COPY_SKIP=(.git .build .claude tmp vendor tools)

# Committed files that live under a generated output dir but are NOT produced by
# that module's controller-gen run. Each entry needs a reason: an unexplained one
# is indistinguishable from a manifest whose type was deleted.
EXEMPT=(
	# A bundled copy of the AGC-owned RunnerGroup CRD, not GMC controller-gen
	# output — controller-gen walks only the GMC module's own packages. It is held
	# byte-identical to cmd/agc/config/crd/ by `make chart-crds-check` (Q73).
	"cmd/gmc/config/crd/bases/actions-gateway.github.com_runnergroups.yaml"
)

# Scratch tree for the regenerated manifests, and — under $GEN_TMP/tree — the
# working-tree copy the DeepCopy half regenerates into. Created by main() rather
# than at load time so check-codegen-drift-test.sh can source this file for its
# parsing helpers without inheriting a temp dir or an EXIT trap of its own.
GEN_TMP=""
TREE_TMP=""

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

# deepcopy_generator — DEEPCOPY_GENERATOR as the shell hands it to controller-gen:
# make expands YEAR (`YEAR ?= $(shell date +%Y)` in all three Makefiles) and the
# shell removes the quotes.
#
# The year is resolved at run time, not hardcoded: a literal would make this gate
# fail every January. It reaches no output today — the boilerplate files are
# empty by design (docs/development/code-generation.md § No per-file license
# headers) so there is no {{.Year}} to substitute — but the gate's job is to run
# the module's own call, not a simplification of it.
deepcopy_generator() {
	# shellcheck disable=SC2016 # '$(YEAR)' is the literal pattern being replaced
	local gen="${DEEPCOPY_GENERATOR//'$(YEAR)'/$(date +%Y)}"
	printf '%s\n' "${gen//\"/}"
}

# module_recipe MODULE TARGET — print MODULE's `TARGET:` recipe as one line with
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
module_recipe() {
	local module="$1" target="$2"
	awk -v target="$target" '
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
		$0 ~ "^" target ":" { inrecipe = 1; next }
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
# target must be registered in MODULES, and every one defining a `deepcopy:`
# target in DEEPCOPY_MODULES.
assert_registry_complete() {
	local makefile module registered
	while IFS= read -r makefile; do
		module="$(dirname "$makefile")"
		case "$module" in . | vendor/* | */vendor/*) continue ;; esac
		if grep -qE '^manifests:' "$makefile"; then
			registered="$(module_dirs | grep -Fx "$module" || true)"
			if [[ -z "$registered" ]]; then
				fail "$module/Makefile defines a 'manifests:' target but $module is not registered in
  scripts/go/check-codegen-drift.sh — its generated manifests are unguarded. Add a MODULES row
  naming its controller-gen generators and output dirs."
			fi
		fi
		if grep -qE '^deepcopy:' "$makefile"; then
			registered="$(printf '%s\n' "${DEEPCOPY_MODULES[@]}" | grep -Fx "$module" || true)"
			if [[ -z "$registered" ]]; then
				fail "$module/Makefile defines a 'deepcopy:' target but $module is not registered in
  scripts/go/check-codegen-drift.sh — its $DEEPCOPY_FILE files are unguarded. Add it to
  DEEPCOPY_MODULES, after every module whose types it imports."
			fi
		fi
	done < <(git ls-files '*Makefile')
}

# assert_registry_fidelity MODULE TARGET REGISTRY GENERATORS OUTPUTS — the
# registration must describe the same controller-gen invocation the module's
# `TARGET:` recipe runs. REGISTRY names the array to fix when it does not.
# OUTPUTS is empty for the DeepCopy half, which has no output: rules to check.
assert_registry_fidelity() {
	local module="$1" target="$2" registry="$3" generators="$4" outputs="$5"
	local recipe gen pair kind dir rule
	recipe="$(module_recipe "$module" "$target")"
	if [[ -z "$recipe" ]]; then
		fail "$module/Makefile has no '$target:' recipe, but $module is registered in
  $registry in scripts/go/check-codegen-drift.sh. Drop the registration, or restore the target."
		return
	fi
	# Registered generators must all appear in the recipe, and vice versa.
	# shellcheck disable=SC2086 # deliberate split: the field is a generator list
	for gen in $generators; do
		if ! grep -qF -- " $gen " <<<" $recipe "; then
			fail "$module: generator '$gen' is registered in scripts/go/check-codegen-drift.sh but
  absent from '$module/Makefile' $target:. Re-sync $registry with the recipe."
		fi
	done
	while IFS= read -r gen; do
		[[ -n "$gen" ]] || continue
		if ! grep -qF -- " $gen " <<<" $generators "; then
			fail "$module: '$module/Makefile' $target: runs generator '$gen', which $registry
  in scripts/go/check-codegen-drift.sh omits — this gate would not regenerate its output.
  Add it to the registration."
		fi
	done < <(recipe_generators "$recipe")
	# Every explicit output rule in the recipe must match the registered dir.
	# shellcheck disable=SC2086 # deliberate split: the field is a kind=dir list
	for pair in $outputs; do
		kind="${pair%%=*}"
		dir="${pair#*=}"
		rule="output:$kind:artifacts:config="
		if grep -qF -- "$rule" <<<"$recipe" && ! grep -qF -- "$rule$dir " <<<"$recipe "; then
			fail "$module: $registry in scripts/go/check-codegen-drift.sh sends '$kind' output to
  '$dir', but '$module/Makefile' $target: names a different dir. Re-sync the registration."
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
  scripts/go/check-codegen-drift.sh with the reason it is not generated here."
		done
	done
}

# is_skipped NAME — true when NAME is a top-level entry copy_tree leaves out.
is_skipped() {
	local name="$1" entry
	for entry in "${COPY_SKIP[@]}"; do
		[[ "$entry" == "$name" ]] && return 0
	done
	return 1
}

# copy_tree — populate $TREE_TMP with the working tree, minus COPY_SKIP.
#
# The DeepCopy half cannot borrow the manifests half's trick of redirecting
# controller-gen's output away from the tree: the `object` generator's output rule
# joins every package's file onto one path, so api/v2alpha1 and api/v2beta1 would
# both land on <dir>/zz_generated.deepcopy.go and the second would win. Copying
# the tree and regenerating in the copy is what keeps the working tree untouched.
#
# It has to be the whole tree, not one module per copy: cmd/agc and cmd/gmc reach
# their first-party dependencies through relative `replace … => ../../api`
# directives, which only resolve when the siblings sit where the go.mod says.
copy_tree() {
	local entry base
	TREE_TMP="$GEN_TMP/tree"
	mkdir -p "$TREE_TMP"
	for entry in "$REPO_ROOT"/* "$REPO_ROOT"/.[!.]*; do
		[[ -e "$entry" ]] || continue
		base="$(basename "$entry")"
		is_skipped "$base" && continue
		cp -R "$entry" "$TREE_TMP/"
	done
}

# deepcopy_files ROOT — print every first-party zz_generated.deepcopy.go under
# ROOT. Vendored trees are pruned: they carry their upstream's generated DeepCopy,
# which controller-gen never rewrites and this gate must not diff.
deepcopy_files() {
	find "$1" -path '*/vendor/*' -prune -o -name "$DEEPCOPY_FILE" -print | sort
}

# regenerate_deepcopy MODULE — run MODULE's `deepcopy:` invocation inside the tree
# copy, after clearing the copies that came in with it so a surviving file is one
# controller-gen no longer writes rather than a leftover.
#
# Deleting them first is safe even though the package stops satisfying
# runtime.Object: controller-gen tolerates the resulting type errors, which is the
# same thing that lets `make generate` work on a freshly scaffolded API type.
regenerate_deepcopy() {
	local module="$1" produced
	while IFS= read -r produced; do
		rm -f "$produced"
	done < <(deepcopy_files "$TREE_TMP/$module")
	# GOWORK points at the module's go.work.gen, matching its deepcopy: target.
	(cd "$TREE_TMP/$module" && GOWORK="$PWD/go.work.gen" "$CONTROLLER_GEN" "$(deepcopy_generator)" 'paths=./...')
}

# assert_no_deepcopy_drift MODULE — diff each regenerated zz_generated.deepcopy.go
# against its committed counterpart, both directions.
assert_no_deepcopy_drift() {
	local module="$1" produced committed rel
	while IFS= read -r produced; do
		rel="${produced#"$TREE_TMP"/}"
		if [[ ! -f "$rel" ]]; then
			fail "$module: controller-gen generates $rel but it is not committed — a new API package
  whose DeepCopy was never generated. Run 'make -C $module deepcopy' (or 'make generate' from
  the repo root) and commit the result."
			continue
		fi
		if ! diff -u "$rel" "$produced"; then
			fail "$rel is stale — it does not match what controller-gen generates from today's Go
  types. An ADDED type is the usual cause: nothing fails to compile when a new type simply has
  no DeepCopy, but a struct field holding it is then shallow-copied into the shared informer
  cache. Fix: run 'make -C $module deepcopy' (or 'make generate' from the repo root) and commit
  the result."
		fi
	done < <(deepcopy_files "$TREE_TMP/$module")
	while IFS= read -r committed; do
		[[ -f "$TREE_TMP/$committed" ]] && continue
		fail "$committed is committed but controller-gen no longer generates it — likely a deleted
  type or a package that no longer holds an API kind. Run 'make -C $module deepcopy' and delete
  the file."
	done < <(deepcopy_files "$module")
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
		assert_registry_fidelity "$module" manifests "the MODULES row" "$generators" "$outputs"
		regenerate "$module" "$generators" "$outputs"
		assert_no_drift "$module" "$outputs"
	done

	copy_tree
	for module in "${DEEPCOPY_MODULES[@]}"; do
		assert_registry_fidelity "$module" deepcopy "DEEPCOPY_MODULES" "$DEEPCOPY_GENERATOR" ""
		regenerate_deepcopy "$module"
		assert_no_deepcopy_drift "$module"
	done

	if [[ "$RC" -ne 0 ]]; then
		exit 1
	fi
	echo "committed CRD/RBAC/webhook manifests match controller-gen output for: $(module_dirs | tr '\n' ' ')"
	echo "committed $DEEPCOPY_FILE matches controller-gen output for: ${DEEPCOPY_MODULES[*]}"
}

# Run main only when executed directly, so check-codegen-drift-test.sh can source
# this file to exercise the recipe parsing and the registry assertions against
# fixtures without building controller-gen or regenerating anything.
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
	main "$@"
fi
