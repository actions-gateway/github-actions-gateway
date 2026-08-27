#!/usr/bin/env bash
#
# check-template-library.sh - Reconcile the SHIPPED runner template library with
# the set CI actually exercises, and with the rules that keep the two attached.
#
# deploy/templates/ ships golden ClusterRunnerTemplates an operator applies
# directly. A shipped golden template is an implicit claim that it works, so the
# library's admission rule is "only what CI exercises" (Q554). That rule is worth
# nothing if nothing enforces it: an entry added with no CI touching it looks
# identical to a validated one, and the library rots one plausible-looking
# addition at a time.
#
# What this asserts, and why each one is a way the library rots:
#
#   1. Every entry directory is a well-formed library entry: template.yaml +
#      kustomization.yaml, a single v2beta1 ClusterRunnerTemplate whose
#      metadata.name IS the directory name. The name is the operator's whole
#      interface (templateRef points at it), so a directory and a name that
#      disagree make the README's instructions wrong.
#   2. No entry carries the cluster-default annotation. Every entry is opt-in;
#      choosing a cluster default is an operator decision, and a shipped default
#      would silently apply a privileged pod shape to sets that named no
#      template.
#   3. Shipped == exercised, modulo INERT_ENTRIES below. An entry is exercised
#      when a dogfood e2e overlay consumes it as a kustomize base AND that
#      overlay is a variant scripts/dogfood/e2e-start.sh will actually select.
#      An overlay CI cannot reach is not evidence.
#   4. The other direction: no dogfood e2e overlay may declare its own
#      ClusterRunnerTemplate. That is exactly how the parallel copy this change
#      removed comes back, and a copy drifts silently because both render fine.
#   5. A patch against a ClusterRunnerTemplate must be JSON 6902, never a
#      strategic merge. kustomize has no schema for a CRD, so it degrades a
#      strategic merge to an RFC 7386 JSON merge patch, which REPLACES a list
#      wholesale instead of merging it by key: a patch naming only
#      initContainers[0].resources drops that container's image, restartPolicy,
#      capability set and startup probe, and renders at exit 0. Measured on
#      kustomize v5.8.1 (kubectl 1.36).
#   6. The library README names every entry, so a new entry cannot ship
#      undocumented.
#   7. With kubectl present: every entry and every overlay renders, and each
#      overlay's render is a SUPERSET of its base's load-bearing lines. This is
#      the direct catch for rule 5's failure mode rather than its proxy — a
#      clobbered list shows up as a base marker missing from the render.
#      Skipped with a notice when kubectl is absent (it is `e2e`-tier in
#      scripts/ci/check-tools.sh, so `make check` cannot require it).
#
# Backs `make template-library-check`; assertions in check-template-library-test.sh
# under `make scripts-test`.
#
# Options (for the test suite; all default to the real tree):
#   --templates-dir PATH   the shipped library
#   --overlays-dir PATH    the dogfood e2e overlays
#   --start-script PATH    the script whose accepted variants define reachability
#   --readme PATH          the library README that must name every entry
#   --no-render            skip rule 7 even when kubectl is present
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

TEMPLATES_DIR="deploy/templates"
OVERLAYS_DIR="deploy/dogfood-e2e/overlays"
START_SCRIPT="scripts/dogfood/e2e-start.sh"
README="deploy/templates/README.md"
RENDER=1

# Library entries admitted WITHOUT a dogfood e2e overlay exercising them, each
# with the reason it is nonetheless not an unvalidated claim. Adding to this list
# is the deliberate act the rule exists to make visible; it is not a shortcut for
# an entry whose e2e wiring is merely unfinished.
#
#   plain — carries no daemon, no capability adds, no RuntimeClass and no
#           volumes. There is no pod shape here to validate beyond schema and
#           CEL admission, which every entry gets on every integration run
#           (TestTemplateLibrary_Admitted). It is also the shape the AGC's own
#           security gap-fill is written against, so a dedicated e2e variant
#           would exercise the provisioner rather than the template.
INERT_ENTRIES=(plain)

while (($# > 0)); do
	case "$1" in
	--templates-dir)
		TEMPLATES_DIR="$2"
		shift
		;;
	--overlays-dir)
		OVERLAYS_DIR="$2"
		shift
		;;
	--start-script)
		START_SCRIPT="$2"
		shift
		;;
	--readme)
		README="$2"
		shift
		;;
	--no-render)
		RENDER=0
		;;
	*)
		printf 'check-template-library.sh: unknown argument: %s\n' "$1" >&2
		exit 2
		;;
	esac
	shift
done

fails=0
fail() {
	printf 'FAIL %s\n' "$1" >&2
	fails=$((fails + 1))
}

# contains ITEM ELEMENT... — true when ITEM equals one of the remaining args.
# Deliberately not `printf '%s\n' "${arr[@]}" | grep -qx`: `grep -q` exits on the
# match, the writer takes SIGPIPE, and under `set -o pipefail` the dead pipeline
# reads as no-match — falsifying a value that IS present once the list outgrows
# the 64 KiB pipe buffer, and only then (Q982). Deliberately literal equality
# too, where `-x` anchored a regex: an entry name is a directory name, so a `.`
# in one was never meant to match any character.
contains() {
	local item="$1" element
	shift
	for element in "$@"; do
		[[ "$element" == "$item" ]] && return 0
	done
	return 1
}

# The `kind:` of a manifest's single document, and the metadata.name under it.
# Anchored at column 0 / two spaces so a `kind:` inside a comment, a patch body
# or a nested podTemplate cannot be mistaken for the document's own.
doc_kind() { awk '/^kind: / { print $2; exit }' "$1"; }
doc_name() {
	awk '
		/^metadata:$/ { in_meta = 1; next }
		in_meta && /^[^[:space:]]/ { in_meta = 0 }
		in_meta && /^  name: / { print $2; exit }
	' "$1"
}
doc_api_version() { awk '/^apiVersion: / { print $2; exit }' "$1"; }

# The `resources:` list of a kustomization, one entry per line, unquoted.
kustomize_resources() {
	awk '
		/^resources:[[:space:]]*$/ { in_list = 1; next }
		in_list && /^[^[:space:]#]/ { in_list = 0 }
		in_list && /^[[:space:]]*-[[:space:]]/ {
			sub(/^[[:space:]]*-[[:space:]]*/, "")
			gsub(/["\047]/, "")
			print
		}
	' "$1"
}

# ---------------------------------------------------------------- entries ----

entries=()
while IFS= read -r dir; do
	entries+=("$(basename "$dir")")
done < <(find "$TEMPLATES_DIR" -mindepth 1 -maxdepth 1 -type d | LC_ALL=C sort)

if ((${#entries[@]} == 0)); then
	fail "$TEMPLATES_DIR contains no entry directories — this gate is no longer checking anything"
fi

for entry in "${entries[@]}"; do
	dir="$TEMPLATES_DIR/$entry"
	tmpl="$dir/template.yaml"
	kust="$dir/kustomization.yaml"

	if [[ ! -f "$tmpl" ]]; then
		fail "library entry '$entry' has no template.yaml"
		continue
	fi
	[[ -f "$kust" ]] || fail "library entry '$entry' has no kustomization.yaml, so \`kubectl apply -k $dir\` fails"

	kind="$(doc_kind "$tmpl")"
	[[ "$kind" == "ClusterRunnerTemplate" ]] ||
		fail "$tmpl declares kind '$kind', want ClusterRunnerTemplate — a namespaced RunnerTemplate may not carry a privileged or near-privileged shape"

	api="$(doc_api_version "$tmpl")"
	[[ "$api" == actions-gateway.com/* ]] ||
		fail "$tmpl declares apiVersion '$api', want the actions-gateway.com group"

	name="$(doc_name "$tmpl")"
	[[ "$name" == "$entry" ]] ||
		fail "$tmpl is named '$name' but lives in '$entry/'; templateRef names the object, so the two must agree"

	# Rule 2. Present tense on purpose: the annotation must not appear at all,
	# including commented-out, because a commented default is one uncomment from
	# shipping one.
	if grep -q 'is-default-template' "$tmpl"; then
		fail "$tmpl mentions the is-default-template annotation; every library entry is opt-in and none may ship as the cluster default"
	fi

	# Rule 1, second half: exactly one document. A second document in an entry
	# would ship alongside the template with nothing naming it.
	docs=$(grep -c '^kind: ' "$tmpl" || true)
	((docs == 1)) ||
		fail "$tmpl contains $docs top-level documents, want exactly 1 (the ClusterRunnerTemplate)"

	# Rule 6. On a name boundary, not a substring: `dind` is not `kata-dind`,
	# and a plain match would report a new entry as documented on the strength
	# of a sentence about a different one.
	grep -qE "(^|[^[:alnum:]_-])$entry([^[:alnum:]_-]|$)" "$README" ||
		fail "$README does not name library entry '$entry'; a shipped entry no doc mentions is one nobody can adopt"
done

# --------------------------------------------------------------- overlays ----

# Variants scripts/dogfood/e2e-start.sh will select. Read from the case arm
# rather than from the directory listing: the question is what CI can reach, and
# an overlay the script rejects is not evidence for anything.
selectable="$(awk '
	/^[[:space:]]*case "\$\{E2E_VARIANT\}" in/ { in_case = 1; next }
	in_case && /^[[:space:]]*esac/ { in_case = 0 }
	in_case && /\)[[:space:]]*;;/ {
		arm = $0
		sub(/\).*/, "", arm)
		gsub(/[[:space:]]/, "", arm)
		if (arm == "*") next
		n = split(arm, parts, "|")
		for (i = 1; i <= n; i++) print parts[i]
	}
' "$START_SCRIPT" | LC_ALL=C sort -u)"

if [[ -z "$selectable" ]]; then
	fail "no E2E_VARIANT arms parsed out of $START_SCRIPT; reachability is unverifiable, so no entry can be called exercised"
fi

overlays=()
while IFS= read -r dir; do
	overlays+=("$(basename "$dir")")
done < <(find "$OVERLAYS_DIR" -mindepth 1 -maxdepth 1 -type d | LC_ALL=C sort)

# check_patch_form KUSTOMIZATION — rule 5. Fails when a strategic-merge patch
# item names a ClusterRunnerTemplate as its own kind, or when an item targeting
# one carries a body that is not an op list.
#
# Indent-aware on purpose. A plain search for `kind: ClusterRunnerTemplate`
# inside an item matches three different things: the item's own `target.kind`,
# the root `kind:` of a strategic-merge body, and a nested VALUE such as the
# RunnerSet patch's `templateRef.kind`. Only the first two are this rule's
# business, and reading the third as either is a false positive that would make
# the correct overlay unfixable. So: items split at the two-space bullet,
# leading `- ` normalised to spaces so an item's own keys share one indent, and
# a body's root keys taken at the body's own minimum indent.
check_patch_form() {
	local kust="$1" report line
	report="$(awk '
		function value(s,   v) { v = s; sub(/^[^:]+:[[:space:]]*/, "", v); return v }
		function flush(   i, k, body_root, target_kind, merge_kind, is_op) {
			if (n == 0) return
			# target.kind — only lines inside the target block itself.
			if (target_line > 0) {
				for (i = target_line + 1; i <= n && ind[i] > ind[target_line]; i++) {
					k = key[i]
					if (k ~ /^kind:[[:space:]]/) target_kind = value(k)
				}
			}
			# The patch body: everything indented past `patch: |`. Its root keys
			# sit at the first such lines indent.
			if (patch_line > 0) {
				for (i = patch_line + 1; i <= n; i++) {
					if (key[i] == "") continue
					if (ind[i] <= ind[patch_line]) break
					if (body_root == 0) body_root = ind[i]
					if (ind[i] != body_root || key[i] ~ /^#/) continue
					k = key[i]
					if (k ~ /^kind:[[:space:]]/ && merge_kind == "") merge_kind = value(k)
					if (k ~ /^- op:[[:space:]]/) is_op = 1
				}
			}
			if (target_line > 0) {
				if (target_kind == "ClusterRunnerTemplate" && !is_op) print "NOOPS"
			} else if (merge_kind == "ClusterRunnerTemplate") {
				print "MERGE"
			}
			n = 0; patch_line = 0; target_line = 0
		}
		/^patches:[[:space:]]*$/ { in_block = 1; next }
		in_block && /^[^[:space:]#]/ { flush(); in_block = 0 }
		!in_block { next }
		/^  - / { flush(); item = 1 }
		!item { next }
		{
			# Normalise the item bullet only (two spaces, dash, space), so an
			# items own keys share one indent. A `- op:` bullet deeper in a body
			# is content and must keep its indent.
			raw = ($0 ~ /^  - /) ? "    " substr($0, 5) : $0
			match(raw, /^[[:space:]]*/)
			n++
			ind[n] = RLENGTH
			key[n] = substr(raw, RLENGTH + 1)
			if (key[n] ~ /^patch:[[:space:]]*\|/) patch_line = n
			if (key[n] ~ /^target:[[:space:]]*$/) target_line = n
		}
		END { flush() }
	' "$kust")"

	while IFS= read -r line; do
		case "$line" in
		MERGE)
			fail "$kust patches a ClusterRunnerTemplate with a strategic merge. kustomize has no CRD schema, so it degrades to a JSON merge patch and REPLACES lists wholesale: the dind container silently loses its image, restartPolicy, capabilities and probe, at exit 0. Use the target: + JSON 6902 form."
			;;
		NOOPS)
			fail "$kust has a patch targeting a ClusterRunnerTemplate whose body is not a JSON 6902 op list. See the note in $TEMPLATES_DIR/kata-dind/kustomization.yaml."
			;;
		esac
	done <<<"$report"
}

# An overlay directory the start script cannot select, or a selectable variant
# with no overlay, breaks the equivalence rule 3 rests on.
have_overlays="$(printf '%s\n' "${overlays[@]}" | LC_ALL=C sort)"
unreachable="$(comm -13 <(printf '%s\n' "$selectable") <(printf '%s\n' "$have_overlays"))"
if [[ -n "$unreachable" ]]; then
	fail "overlay(s) $(tr '\n' ' ' <<<"$unreachable")have no E2E_VARIANT arm in $START_SCRIPT, so CI never applies them; they cannot count as exercising a library entry"
fi
missing_overlay="$(comm -23 <(printf '%s\n' "$selectable") <(printf '%s\n' "$have_overlays"))"
if [[ -n "$missing_overlay" ]]; then
	fail "$START_SCRIPT accepts E2E_VARIANT $(tr '\n' ' ' <<<"$missing_overlay")with no matching directory under $OVERLAYS_DIR"
fi

# entry -> the overlay that consumes it, built from each overlay's resources list.
declare -a exercised=()
for overlay in "${overlays[@]}"; do
	kust="$OVERLAYS_DIR/$overlay/kustomization.yaml"
	if [[ ! -f "$kust" ]]; then
		fail "overlay '$overlay' has no kustomization.yaml"
		continue
	fi

	consumed=()
	while IFS= read -r res; do
		[[ "$res" == */templates/* ]] || continue
		consumed+=("$(basename "$res")")
	done < <(kustomize_resources "$kust")

	if ((${#consumed[@]} == 0)); then
		fail "overlay '$overlay' consumes no $TEMPLATES_DIR entry; the shipped artifact and the exercised one have separated"
	elif ((${#consumed[@]} > 1)); then
		fail "overlay '$overlay' consumes ${#consumed[@]} library entries (${consumed[*]}); one overlay validates one worker shape"
	fi

	for name in "${consumed[@]}"; do
		if ! contains "$name" "${entries[@]}"; then
			fail "overlay '$overlay' bases on '$name', which is not an entry under $TEMPLATES_DIR"
			continue
		fi
		if grep -qx -- "$overlay" <<<"$selectable"; then
			exercised+=("$name")
		fi
	done

	# Rule 4: the overlay's own resources must not re-declare a template.
	for res in $(kustomize_resources "$kust"); do
		[[ "$res" == *.yaml ]] || continue
		f="$OVERLAYS_DIR/$overlay/$res"
		[[ -f "$f" ]] || continue
		if grep -q '^kind: ClusterRunnerTemplate' "$f"; then
			fail "$f declares a ClusterRunnerTemplate; the worker shape belongs in $TEMPLATES_DIR so the shipped artifact and the exercised one cannot drift"
		fi
	done

	# Rule 5.
	check_patch_form "$kust"
done

# ------------------------------------------------------------ rule 3, both ----

want_exercised="$(printf '%s\n' "${entries[@]}" | grep -vxF -f <(printf '%s\n' "${INERT_ENTRIES[@]}") || true)"
# `|| true`: grep -v exits 1 when nothing survives the filter, which is the
# ordinary "no entry is exercised yet" case rather than an error.
have_exercised="$(printf '%s\n' "${exercised[@]+"${exercised[@]}"}" | grep -v '^$' | LC_ALL=C sort -u || true)"

unexercised="$(comm -23 <(printf '%s\n' "$want_exercised" | grep -v '^$' | LC_ALL=C sort) <(printf '%s\n' "$have_exercised"))"
if [[ -n "$unexercised" ]]; then
	fail "library entries $(tr '\n' ' ' <<<"$unexercised")ship but no reachable dogfood e2e overlay exercises them.
       Only what CI exercises may ship (Q554). Wire an overlay onto the entry, or
       add it to INERT_ENTRIES in this script with the reason it needs none."
fi

for inert in "${INERT_ENTRIES[@]}"; do
	if ! contains "$inert" "${entries[@]}"; then
		fail "INERT_ENTRIES names '$inert', which is not an entry under $TEMPLATES_DIR"
	elif grep -qx -- "$inert" <<<"$have_exercised"; then
		fail "'$inert' is declared inert but an overlay exercises it; drop it from INERT_ENTRIES rather than keeping an exemption that no longer applies"
	fi
done

# ----------------------------------------------------------------- render ----

if ((RENDER)) && command -v kubectl >/dev/null 2>&1; then
	# The lines a wholesale list replacement destroys: container images, the
	# native-sidecar marker, the isolation mechanism, the block-device wiring,
	# and the capability adds. Compared as kustomize-normalized output, so
	# indentation in the sources cannot make the two sides disagree.
	markers() {
		grep -E '^[[:space:]]+((image|restartPolicy|privileged|runtimeClassName|volumeMode|devicePath|failureThreshold|allowPrivilegeEscalation):|- [A-Z][A-Z_]+$)' |
			sed 's/^[[:space:]]*//' | LC_ALL=C sort
	}

	for entry in "${entries[@]}"; do
		if ! kubectl kustomize "$TEMPLATES_DIR/$entry" >/dev/null 2>&1; then
			fail "\`kubectl kustomize $TEMPLATES_DIR/$entry\` does not render; an operator cannot apply this entry"
		fi
	done

	for overlay in "${overlays[@]}"; do
		kust="$OVERLAYS_DIR/$overlay/kustomization.yaml"
		[[ -f "$kust" ]] || continue

		render="$(kubectl kustomize "$OVERLAYS_DIR/$overlay" 2>&1)" || {
			fail "\`kubectl kustomize $OVERLAYS_DIR/$overlay\` does not render:
$render"
			continue
		}

		base=""
		while IFS= read -r res; do
			[[ "$res" == */templates/* ]] && base="$TEMPLATES_DIR/$(basename "$res")"
		done < <(kustomize_resources "$kust")
		[[ -n "$base" && -d "$base" ]] || continue

		base_render="$(kubectl kustomize "$base" 2>/dev/null)" || continue
		lost="$(comm -23 <(markers <<<"$base_render") <(markers <<<"$render"))"
		if [[ -n "$lost" ]]; then
			fail "overlay '$overlay' renders WITHOUT load-bearing lines its base $base declares:
$(awk '{ print "         " $0 }' <<<"$lost")
       This is the signature of a strategic-merge patch replacing a list wholesale."
		fi
	done
else
	printf 'note: kubectl not on PATH (it is e2e-tier in scripts/ci/check-tools.sh), so the render assertions were skipped\n'
fi

# ----------------------------------------------------------------- verdict ----

if ((fails > 0)); then
	printf '\n%d template-library check(s) failed. The rule the library rests on is that\n' "$fails" >&2
	printf 'only what CI exercises may ship: %s/README.md documents it, and this\n' "$TEMPLATES_DIR" >&2
	printf 'gate is what makes it true rather than aspirational.\n' >&2
	exit 1
fi

printf 'template library agrees with what CI exercises: %d entries (%d exercised by a dogfood e2e overlay, %d declared inert), %d overlays, all patches JSON 6902\n' \
	"${#entries[@]}" "$(grep -c . <<<"$have_exercised" || true)" "${#INERT_ENTRIES[@]}" "${#overlays[@]}"
