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

# A metric registration and a flag declaration in the shapes the two readers
# match. Both are already published at the seed tag, so neither reads as new.
operator_src() {
	local extra="${1:-}"
	cat <<EOF
package controller

const SeedMetric = "actions_gateway_seed_total"
$extra

func flags(fs *FlagSet) {
	fs.StringVar(&opts.Seed, "seed-mode", "", "help")
}
EOF
}

controller_src() {
	local reason="$1"
	cat <<EOF
package controller

import (
	corev1 "k8s.io/api/core/v1"

	"github.com/actions-gateway/github-actions-gateway/api/apiconditions"
)

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

# The GMC half: its own recorder, and a reason reached through an import alias.
# The real GMC imports the shared vocabulary as gmcv2alpha1, which is what left
# every one of its recorder calls unplaceable until Q925.
gmc_src() {
	local reason="$1"
	cat <<EOF
package controller

import (
	corev1 "k8s.io/api/core/v1"

	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/apiconditions"
)

type GMCRecorder interface {
	Event(namespace, name, eventtype, reason, action, note string)
}

func (r *G) recordEvent(ag *AG, eventtype, reason, action, note string) {
	r.Recorder.Event(ag.Namespace, ag.Name, eventtype, reason, action, note)
}

func (r *G) provision(ag *AG) {
	setCondition(ag, gmcv2alpha1.ReasonListenerActive)
	r.recordEvent(ag, corev1.EventTypeNormal, "$reason", "Reconcile", "n")
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
	mkdir -p "$d/api/apiconditions" "$d/cmd/agc/internal/controller" \
		"$d/cmd/gmc/internal/controller" "$d/charts/actions-gateway"
	(
		cd "$d"
		printf 'devtools\n' >.gitignore
		ln -s "$REPO_ROOT/devtools" devtools
		api_src >api/apiconditions/conditions.go
		controller_src WorkerPodStuckPending >cmd/agc/internal/controller/shared.go
		gmc_src ProxyCertificateIssued >cmd/gmc/internal/controller/gateway.go
		# The operator surfaces the fixture has to carry for a window over it to
		# mean anything: a section refuses when its set is empty at the older
		# tag, so a fixture with no metric and no flag would refuse in every
		# test rather than report.
		operator_src >cmd/agc/internal/controller/operator.go
		printf 'replicas: 1\nmetrics:\n  enabled: true\n' >charts/actions-gateway/values.yaml
		git init -q -b main
		# Q820: no detached maintenance racing the next command in a fixture repo.
		git config maintenance.auto false
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

# The GMC is scanned too, and its reasons reach the section through the same
# import alias the real one uses (Q925). Scoping REASON_TREES back to the AGC, or
# keying reason resolution on the identifier again, both fail here.
test_gmc_reason_is_listed() {
	local d out
	d="$(build_repo gmc-reason)"
	(
		cd "$d"
		gmc_src WorkerDrainTimeout >cmd/gmc/internal/controller/gateway.go
		git commit -q -am "feat: a new GMC Event reason"
	)
	out="$(run_script "$d")"
	check gmc-reason-listed "$(event_section "$out")" "WorkerDrainTimeout"
}

# section_body OUTPUT TITLE — the body of any section, indentation stripped.
section_body() {
	printf '%s\n' "$1" | awk -v want="$2" '
		/^== / { inside = (index($0, want) > 0); next }
		inside { sub(/^  /, ""); print }
	'
}

# The operator-facing surfaces added by Q1037. Each one is asserted in both
# directions, because the defect being fixed was a query that reported an empty
# diff while matching nothing at all: "none new" is only worth reading if the
# reader can be shown to find something when there IS something.
test_new_metric_is_listed() {
	local d out
	d="$(build_repo new-metric)"
	(
		cd "$d"
		operator_src 'const Widgets = "actions_gateway_widgets_total"' \
			>cmd/agc/internal/controller/operator.go
		git commit -q -am "feat: a new metric"
	)
	out="$(run_script "$d")"
	check new-metric-listed "$(section_body "$out" "New metric names")" \
		"actions_gateway_widgets_total"
}

test_unchanged_metrics_report_none() {
	local d out
	d="$(build_repo same-metric)"
	(
		cd "$d"
		operator_src 'const Widgets = "actions_gateway_widgets_total"' \
			>cmd/agc/internal/controller/operator.go
		git commit -q -am "feat: a metric"
		git tag v0.2.0
		operator_src 'const Widgets = "actions_gateway_widgets_total" // a comment' \
			>cmd/agc/internal/controller/operator.go
		# Something unrelated must change in the window, or the run exits early
		# with nothing to review and prints no sections at all — which would let
		# this assertion pass on an absent section rather than an empty one.
		api_src '	ReasonListenerPaused = "ListenerPaused"' >api/apiconditions/conditions.go
		git commit -q -am "feat: a condition reason, and a comment on a metric"
	)
	out="$(run_script "$d" v0.2.0)"
	check unchanged-metrics-none "$(section_body "$out" "New metric names")" "(none)"
}

test_new_flag_is_listed() {
	local d out
	d="$(build_repo new-flag)"
	(
		cd "$d"
		printf 'package controller\n\nfunc more(fs *FlagSet) {\n\tfs.StringVar(&opts.W, "widget-mode", "", "help")\n}\n' \
			>cmd/agc/internal/controller/moreflags.go
		git add -A && git commit -q -m "feat: a new flag"
	)
	out="$(run_script "$d")"
	check new-flag-listed "$(section_body "$out" "New CLI flags")" "widget-mode"
}

# The refusal, and the reason both sections exist. Drafting the v1.7.0 notes a
# hand-rolled flag query matched the wrong declaration shape and returned zero
# at BOTH ends; an empty diff of two empty sets is indistinguishable from a real
# "nothing changed". A tree carrying no flag at the older tag must say it could
# not enumerate rather than print an empty section.
test_unenumerable_surface_refuses() {
	local d out body
	d="$(build_repo enumerable)"
	(
		cd "$d"
		# Strip the flag declaration from the seed while leaving the metric, so
		# one section refuses and the other reports in the same run. Both ends
		# lose it, which is exactly the shape that produced the defect: two
		# empty sets diff to an empty set and read as "nothing changed".
		printf 'package controller\n\nconst SeedMetric = "actions_gateway_seed_total"\n' \
			>cmd/agc/internal/controller/operator.go
		git commit -q -am "chore: a tree with no flags"
		git tag v0.2.0
		printf 'package controller\n\nconst SeedMetric = "actions_gateway_seed_total"\nconst Extra = "actions_gateway_extra_total"\n' \
			>cmd/agc/internal/controller/operator.go
		git commit -q -am "feat: another metric"
	)
	out="$(run_script "$d" v0.2.0)"
	body="$(section_body "$out" "New CLI flags")"
	check_contains flags-refuse-when-empty-at-ref "$body" "COULD NOT ENUMERATE"
	check_contains flags-refusal-blocks-the-claim "$body" "not a report of none-new"
	# The control that makes the refusal mean something rather than being the
	# script's only answer: the metric section reads the same tree and reports.
	check metrics-do-not-refuse-in-the-same-tree \
		"$(section_body "$out" "New metric names")" "actions_gateway_extra_total"
}

test_new_reason_is_the_only_surface
test_unchanged_reasons_report_none
test_gmc_reason_is_listed
test_empty_window_is_quiet
test_unscannable_window_says_so
test_new_metric_is_listed
test_unchanged_metrics_report_none
test_new_flag_is_listed
test_unenumerable_surface_refuses

if ((fails > 0)); then
	printf '\n%d test(s) failed\n' "$fails" >&2
	exit 1
fi
printf '\nall api-surface-since tests passed\n'
