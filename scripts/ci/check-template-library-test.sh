#!/usr/bin/env bash
#
# Unit tests for scripts/ci/check-template-library.sh (Q554).
#
# The gate exists to fail when the shipped runner template library and the set CI
# exercises stop agreeing, so every rule is asserted in BOTH directions: a
# fixture that breaks it must go red, and the sound fixture must stay green. A
# rule that quietly stops matching fails exactly as silently as one that matches
# everything, and this gate's whole value is that it notices.
#
# Two cases earn their place beyond the obvious ones:
#
#   - the RunnerSet strategic-merge patch carries `templateRef.kind:
#     ClusterRunnerTemplate` as a nested VALUE. A naive search for that string
#     inside a patch item flags it as a strategic merge against the template,
#     which is a false positive that would make the correct overlay unfixable.
#     It was the first thing this gate got wrong; it is pinned here.
#   - a JSON 6902 `op: replace` on a whole list passes rule 5 and still destroys
#     the base. Only the render comparison catches that, which is why the render
#     half is not decoration.
#
# Runs under `make check` (via `make scripts-test`) and the CI shellcheck job.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"
cd "$REPO_ROOT"

GATE="$REPO_ROOT/scripts/ci/check-template-library.sh"
FIXTURE_ROOT="$REPO_ROOT/tmp/check-template-library-test.$$"
trap 'rm -rf "$FIXTURE_ROOT"' EXIT INT TERM

fails=0
ok() { printf 'ok   %-46s %s\n' "$1" "$2"; }
bad() {
	printf 'FAIL %-46s %s\n' "$1" "$2" >&2
	fails=$((fails + 1))
}

# build_fixture DIR — a minimally sound library + overlays tree: two exercised
# entries, one inert entry, two selectable overlays. Every red case below is this
# tree with exactly one thing broken.
build_fixture() {
	local root="$1"
	rm -rf "$root"
	mkdir -p "$root/templates/plain" "$root/templates/kata-dind" \
		"$root/templates/privileged-dind" \
		"$root/overlays/kata" "$root/overlays/dind"

	local entry
	for entry in plain kata-dind privileged-dind; do
		cat >"$root/templates/$entry/kustomization.yaml" <<-EOF
			apiVersion: kustomize.config.k8s.io/v1beta1
			kind: Kustomization
			resources:
			  - template.yaml
		EOF
		cat >"$root/templates/$entry/template.yaml" <<-EOF
			apiVersion: actions-gateway.com/v2beta1
			kind: ClusterRunnerTemplate
			metadata:
			  name: $entry
			spec:
			  # No workerImage, as the shipped library entries have none: the AGC
			  # gap-fills its default. The overlay patch below replaces that
			  # absent member, which kustomize creates rather than rejecting, so
			  # this fixture exercises the shape the real overlays depend on
			  # instead of asserting it holds.
			  podTemplate:
			    spec:
			      initContainers:
			        - name: dind
			          image: docker:28-dind
			          restartPolicy: Always
			      containers:
			        - name: runner
		EOF
	done

	cat >"$root/README.md" <<-'EOF'
		# fixture library
		Entries: plain, kata-dind, privileged-dind.
	EOF

	local overlay tmpl
	for overlay in kata dind; do
		tmpl=$([[ "$overlay" == kata ]] && echo kata-dind || echo privileged-dind)
		# The RunnerSet is here rather than in a shared base only to keep the
		# fixture one directory deep; the tracked tree keeps it in ../../base.
		# It must exist for the strategic-merge patch below to have a target,
		# which is what lets the render assertions run against this fixture.
		cat >"$root/overlays/$overlay/resources.yaml" <<-EOF
			apiVersion: actions-gateway.com/v2beta1
			kind: RunnerSet
			metadata:
			  name: ci
			  namespace: fixture
			spec:
			  gatewayRef:
			    name: fixture
			---
			apiVersion: networking.k8s.io/v1
			kind: NetworkPolicy
			metadata:
			  name: open-egress
			  namespace: fixture
			spec:
			  podSelector: {}
			  policyTypes: [Egress]
			  egress:
			    - {}
		EOF
		cat >"$root/overlays/$overlay/kustomization.yaml" <<-EOF
			apiVersion: kustomize.config.k8s.io/v1beta1
			kind: Kustomization
			resources:
			  - ../../templates/$tmpl
			  - resources.yaml
			patches:
			  # A strategic merge whose body names a DIFFERENT kind, and which
			  # mentions ClusterRunnerTemplate only as a nested value. Must not trip
			  # the JSON 6902 rule.
			  - patch: |
			      apiVersion: actions-gateway.com/v2beta1
			      kind: RunnerSet
			      metadata:
			        name: ci
			        namespace: fixture
			      spec:
			        templateRef:
			          name: $tmpl
			          kind: ClusterRunnerTemplate
			  - target:
			      group: actions-gateway.com
			      version: v2beta1
			      kind: ClusterRunnerTemplate
			      name: $tmpl
			    patch: |
			      # a comment ahead of the ops
			      - op: replace
			        path: /spec/workerImage
			        value: ghcr.io/example/runner:1
		EOF
	done

	cat >"$root/e2e-start.sh" <<-'EOF'
		#!/usr/bin/env bash
		main() {
			case "${E2E_VARIANT}" in
				dind|kata) ;;
				*)
					echo "error" >&2
					exit 1
					;;
			esac
		}
	EOF
}

# run_gate DIR — the gate against a fixture tree. Sets RC and GATE_OUT in the
# CALLER's shell, deliberately not via `out="$(run_gate …)"`: a command
# substitution runs in a subshell, so an RC assigned inside one never reaches
# the caller and every assertion reads a stale 0. That is how the first draft of
# this suite reported thirteen passes against thirteen broken fixtures.
RC=0
GATE_OUT=""
run_gate() {
	local root="$1"
	shift
	local log="$FIXTURE_ROOT/gate.log"
	set +e
	"$GATE" --templates-dir "$root/templates" --overlays-dir "$root/overlays" \
		--start-script "$root/e2e-start.sh" --readme "$root/README.md" "$@" >"$log" 2>&1
	RC=$?
	set -e
	GATE_OUT="$(<"$log")"
}

# expect_green NAME ROOT — the fixture as built must pass.
expect_green() {
	local name="$1" root="$2"
	shift 2
	run_gate "$root" --no-render "$@"
	die_if_killed "$name" "$RC"
	if ((RC == 0)); then
		ok "$name" "gate passes the sound fixture"
	else
		bad "$name" "gate failed a sound fixture (rc=$RC): $GATE_OUT"
	fi
}

# expect_red NAME ROOT FRAGMENT — the broken fixture must fail, naming FRAGMENT.
expect_red() {
	local name="$1" root="$2" fragment="$3"
	shift 3
	run_gate "$root" --no-render "$@"
	die_if_killed "$name" "$RC"
	if ((RC == 0)); then
		bad "$name" "gate PASSED a broken fixture; the rule is disarmed"
	elif [[ "$GATE_OUT" != *"$fragment"* ]]; then
		bad "$name" "failed, but not for the expected reason (want '$fragment'): $GATE_OUT"
	else
		ok "$name" "caught, naming '$fragment'"
	fi
}

F="$FIXTURE_ROOT/tree"

# --- the sound tree, and the false positive that shaped the parser -----------

build_fixture "$F"
expect_green "sound fixture" "$F"

# The same assertion stated as its own case: the nested templateRef.kind value in
# the RunnerSet patch is present in the sound tree above, so a regression that
# starts reading it as the patch's own kind turns that green red. Restate it
# explicitly so the reason survives a future edit to build_fixture.
if grep -q 'kind: ClusterRunnerTemplate' "$F/overlays/kata/kustomization.yaml"; then
	ok "nested templateRef.kind is present" "the sound fixture really does exercise the false positive"
else
	bad "nested templateRef.kind is present" "build_fixture no longer contains it; the green above proves nothing"
fi

# --- rule 1: entry shape -----------------------------------------------------

build_fixture "$F"
sed 's/^  name: kata-dind$/  name: kata-dind-oops/' "$F/templates/kata-dind/template.yaml" >"$F/t" &&
	mv "$F/t" "$F/templates/kata-dind/template.yaml"
expect_red "name must match directory" "$F" "templateRef names the object"

build_fixture "$F"
rm "$F/templates/plain/kustomization.yaml"
expect_red "entry needs a kustomization" "$F" "has no kustomization.yaml"

build_fixture "$F"
printf 'kind: ConfigMap\n' >>"$F/templates/plain/template.yaml"
expect_red "entry is one document" "$F" "want exactly 1"

# --- rule 2: nothing ships as the cluster default ----------------------------

build_fixture "$F"
sed 's|^metadata:$|metadata:\n  annotations:\n    actions-gateway.com/is-default-template: "true"|' \
	"$F/templates/plain/template.yaml" >"$F/t" && mv "$F/t" "$F/templates/plain/template.yaml"
expect_red "no shipped cluster default" "$F" "none may ship as the cluster default"

# --- rule 3: shipped == exercised, both directions ---------------------------

build_fixture "$F"
mkdir -p "$F/templates/gvisor"
cp "$F/templates/plain/kustomization.yaml" "$F/templates/gvisor/kustomization.yaml"
sed 's/^  name: plain$/  name: gvisor/' "$F/templates/plain/template.yaml" \
	>"$F/templates/gvisor/template.yaml"
printf 'gvisor\n' >>"$F/README.md"
expect_red "an unexercised entry cannot ship" "$F" "no reachable dogfood e2e overlay exercises them"

build_fixture "$F"
# Point the dind overlay at the inert entry: `plain` becomes exercised, so its
# exemption is stale and must be surrendered rather than kept.
sed 's|templates/privileged-dind|templates/plain|' "$F/overlays/dind/kustomization.yaml" >"$F/t" &&
	mv "$F/t" "$F/overlays/dind/kustomization.yaml"
expect_red "a stale inert exemption is caught" "$F" "declared inert but an overlay exercises it"

build_fixture "$F"
# An overlay directory the start script will not select is not evidence.
cp -R "$F/overlays/kata" "$F/overlays/gvisor"
expect_red "an unreachable overlay is not evidence" "$F" "have no E2E_VARIANT arm"

build_fixture "$F"
rm -rf "$F/overlays/dind"
expect_red "a selectable variant needs its overlay" "$F" "with no matching directory"

build_fixture "$F"
sed '/templates\/privileged-dind/d' "$F/overlays/dind/kustomization.yaml" >"$F/t" &&
	mv "$F/t" "$F/overlays/dind/kustomization.yaml"
expect_red "an overlay must consume the library" "$F" "consumes no"

# --- rule 4: no overlay-local template ---------------------------------------

build_fixture "$F"
cat >>"$F/overlays/kata/resources.yaml" <<-'EOF'
	---
	apiVersion: actions-gateway.com/v2beta1
	kind: ClusterRunnerTemplate
	metadata:
	  name: sneaky-copy
EOF
expect_red "no parallel copy in an overlay" "$F" "declares a ClusterRunnerTemplate"

# --- rule 5: JSON 6902 only, against a template ------------------------------

build_fixture "$F"
cat >>"$F/overlays/kata/kustomization.yaml" <<-'EOF'
	  - patch: |
	      apiVersion: actions-gateway.com/v2beta1
	      kind: ClusterRunnerTemplate
	      metadata:
	        name: kata-dind
	      spec:
	        podTemplate:
	          spec:
	            initContainers:
	              - name: dind
	                resources:
	                  requests:
	                    cpu: "3"
EOF
expect_red "a strategic merge on a template is caught" "$F" "REPLACES lists wholesale"

build_fixture "$F"
# A targeted patch whose body is a strategic merge rather than an op list.
cat >>"$F/overlays/kata/kustomization.yaml" <<-'EOF'
	  - target:
	      group: actions-gateway.com
	      version: v2beta1
	      kind: ClusterRunnerTemplate
	      name: kata-dind
	    patch: |
	      spec:
	        podTemplate:
	          spec:
	            nodeSelector:
	              disk: ssd
EOF
expect_red "a targeted non-op body is caught" "$F" "is not a JSON 6902 op list"

# --- rule 6: the README names every entry ------------------------------------

build_fixture "$F"
printf '# fixture library\nEntries: plain, kata-dind.\n' >"$F/README.md"
expect_red "an undocumented entry is caught" "$F" "does not name library entry"

build_fixture "$F"
# The substring trap: an entry named `dind` must not count as documented on the
# strength of a sentence about `kata-dind`. Same defect class #1343 found in the
# scripts/README.md gate (`start.sh` matched by `e2e-start.sh`).
mkdir -p "$F/templates/dind"
cp "$F/templates/plain/kustomization.yaml" "$F/templates/dind/kustomization.yaml"
sed 's/^  name: plain$/  name: dind/' "$F/templates/plain/template.yaml" >"$F/templates/dind/template.yaml"
expect_red "a name-boundary match, not a substring" "$F" "does not name library entry 'dind'"

# --- rule 7: the render catches what rule 5 cannot ---------------------------

if command -v kubectl >/dev/null 2>&1; then
	build_fixture "$F"
	run_gate "$F"
	die_if_killed "sound fixture renders" "$RC"
	if ((RC == 0)); then
		ok "sound fixture renders" "overlays keep every load-bearing line of their base"
	else
		bad "sound fixture renders" "rc=$RC: $GATE_OUT"
	fi

	build_fixture "$F"
	# A JSON 6902 `replace` on the whole list. Rule 5 is satisfied (it IS an op
	# list) and the base's dind image and restartPolicy are gone anyway — the
	# failure mode the render half exists for.
	cat >>"$F/overlays/kata/kustomization.yaml" <<-'EOF'
		  - target:
		      group: actions-gateway.com
		      version: v2beta1
		      kind: ClusterRunnerTemplate
		      name: kata-dind
		    patch: |
		      - op: replace
		        path: /spec/podTemplate/spec/initContainers
		        value:
		          - name: dind
	EOF
	run_gate "$F"
	die_if_killed "render catches a list replacement" "$RC"
	if ((RC == 0)); then
		bad "render catches a list replacement" "gate PASSED an overlay that erased its base's init container"
	elif [[ "$GATE_OUT" != *"renders WITHOUT load-bearing lines"* ]]; then
		bad "render catches a list replacement" "failed for another reason: $GATE_OUT"
	else
		ok "render catches a list replacement" "reported the lost base lines"
	fi
else
	printf 'skip %-46s %s\n' "render assertions" "kubectl not on PATH"
fi

# --- the tracked tree ---------------------------------------------------------

set +e
tracked="$("$GATE" 2>&1)"
tracked_rc=$?
die_if_killed "tracked tree passes" "$tracked_rc"
set -e
if ((tracked_rc == 0)); then
	ok "tracked tree passes" "$(tail -1 <<<"$tracked")"
else
	bad "tracked tree passes" "$tracked"
fi

if ((fails > 0)); then
	printf '\n%d check-template-library assertion(s) failed\n' "$fails" >&2
	exit 1
fi
printf '\nall check-template-library assertions passed\n'
