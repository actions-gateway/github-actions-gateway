#!/usr/bin/env bash
#
# Unit tests for the Event-reason enumeration in scripts/release/api-surface-since.sh.
#
# An Event reason is an argument at the recording site rather than a declaration,
# so the section reporting it is only worth reading if an empty one means "none
# new" and nothing else. These fixtures pin the three ways that could stop being
# true: a reason added with no other API surface in the window must still be
# reported, a scan that could not run must say so rather than print an empty
# section, and the section must be empty when the sets at the two ends agree.
# Runs under `make check` (via `make scripts-test`).
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
SCRIPT="$REPO_ROOT/scripts/release/api-surface-since.sh"
EVENT_SECTION="New Event reasons"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

fails=0

# The AGC shapes the scanner has to read through: a recorder declared as an
# interface method, a wrapper forwarding to it, and one literal reason of its
# own. devtools/docs/reasontiers/main_test.go owns the scanner's own cases;
# these only have to be scannable.
api_src() {
	local extra="${1:-}"
	cat <<EOF
package apiconditions

const (
	ReasonListenerActive = "ListenerActive"
$extra
)
EOF
}

controller_src() {
	local reason="$1"
	cat <<EOF
package controller

import corev1 "k8s.io/api/core/v1"

type EventRecorder interface {
	Event(namespace, name, eventtype, reason, action, note string)
}

func (r *R) recordEvent(rs *RS, eventtype, reason, action, note string) {
	r.Recorder.Event(rs.Namespace, rs.Name, eventtype, reason, action, note)
}

func (r *R) ready(rs *RS) {
	setCondition(rs, apiconditions.ReasonListenerActive)
	r.recordEvent(rs, corev1.EventTypeWarning, "$reason", "ReapWorkerPods", "n")
}
EOF
}

# build_repo NAME — a fixture repo tagged v0.1.0 at a tree emitting
# WorkerPodStuckPending. Echoes its path. devtools is symlinked rather than
# copied so the script's `go build` finds the real scanner; it is gitignored, and
# the script archives only cmd/agc and api, so it never reaches a fixture ref.
build_repo() {
	local d="$WORK/$1"
	rm -rf "$d"
	mkdir -p "$d/api/apiconditions" "$d/cmd/agc/internal/controller"
	(
		cd "$d"
		printf 'devtools\n' >.gitignore
		ln -s "$REPO_ROOT/devtools" devtools
		api_src >api/apiconditions/conditions.go
		controller_src WorkerPodStuckPending >cmd/agc/internal/controller/shared.go
		git init -q -b main
		git config user.email t@t.t
		git config user.name t
		git add -A
		git commit -q -m "chore: seed"
		git tag v0.1.0
	)
	echo "$d"
}

# run_script DIR [ARGS…] — the script's combined output, whatever it exits with.
run_script() {
	local dir="$1"
	shift
	(cd "$dir" && "$SCRIPT" "$@") 2>&1 || true
}

# event_section OUTPUT — the body of the Event reasons section, indentation
# stripped, so a test asserts on what that section said and not on where the
# string happened to appear.
event_section() {
	printf '%s\n' "$1" | awk -v want="$EVENT_SECTION" '
		/^== / { inside = (index($0, want) > 0); next }
		inside { sub(/^  /, ""); print }
	'
}

check() {
	local name="$1" got="$2" want="$3"
	if [[ "$got" == "$want" ]]; then
		printf 'ok   %s\n' "$name"
		return
	fi
	printf 'FAIL %s\n  want: %q\n  got:  %q\n' "$name" "$want" "$got" >&2
	fails=$((fails + 1))
}

check_contains() {
	local name="$1" got="$2" want="$3"
	if [[ "$got" == *"$want"* ]]; then
		printf 'ok   %s\n' "$name"
		return
	fi
	printf 'FAIL %s\n  want substring: %q\n  got: %q\n' "$name" "$want" "$got" >&2
	fails=$((fails + 1))
}

# A reason added since the tag is listed, and it is the only one listed: the
# unchanged WorkerPodStuckPending is already published and must not read as new.
# Nothing under the API paths changed in this window, so an early exit keyed on
# those alone would swallow the whole report.
test_new_reason_is_the_only_surface() {
	local d out
	d="$(build_repo new-reason)"
	(
		cd "$d"
		controller_src JobProvisionStalled >cmd/agc/internal/controller/shared.go
		git commit -q -am "feat: a new Event reason"
	)
	out="$(run_script "$d")"
	check new-reason-listed "$(event_section "$out")" "JobProvisionStalled"
	check_contains new-reason-window-not-swallowed "$out" "API surface between v0.1.0"
}

# The complement: AGC source changed, no reason did. Without this case an
# always-empty section would pass the one above just as well.
test_unchanged_reasons_report_none() {
	local d out
	d="$(build_repo same-reasons)"
	(
		cd "$d"
		printf '\n// a comment, and no new reason\n' >>cmd/agc/internal/controller/shared.go
		api_src '	ReasonPodsNotStarting = "PodsNotStarting"' >api/apiconditions/conditions.go
		git commit -q -am "chore: touch the AGC and add a condition reason"
	)
	out="$(run_script "$d")"
	check unchanged-reasons-none "$(event_section "$out")" "(none)"
	check_contains unchanged-reasons-condition-still-seen "$out" "PodsNotStarting"
}

# Nothing in the window at all: --quiet still reports nothing to review.
test_empty_window_is_quiet() {
	local d
	d="$(build_repo empty-window)"
	if (cd "$d" && "$SCRIPT" --quiet >/dev/null 2>&1); then
		printf 'FAIL empty-window-quiet: expected exit 1 when there is nothing to review\n' >&2
		fails=$((fails + 1))
	else
		printf 'ok   empty-window-quiet\n'
	fi
}

# A scan that could not run must say so. An empty section here would report the
# Event surface as unchanged, which is the failure the section exists to prevent.
test_unscannable_window_says_so() {
	local d out
	d="$(build_repo unscannable)"
	(
		cd "$d"
		rm -f devtools
		controller_src JobProvisionStalled >cmd/agc/internal/controller/shared.go
		git commit -q -am "feat: a new Event reason"
	)
	out="$(run_script "$d")"
	check_contains unscannable-reports-failure "$(event_section "$out")" "COULD NOT ENUMERATE"
	check_contains unscannable-is-not-none "$(event_section "$out")" "not a report of none-new"
}

test_new_reason_is_the_only_surface
test_unchanged_reasons_report_none
test_empty_window_is_quiet
test_unscannable_window_says_so

if ((fails > 0)); then
	printf '\n%d test(s) failed\n' "$fails" >&2
	exit 1
fi
printf '\nall api-surface-since tests passed\n'
