#!/usr/bin/env bash
#
# claude-piped-gate-hook.sh — Claude Code PreToolUse hook that warns when a
# command whose exit status IS the answer is piped into a filter, so the Bash
# call reports the filter's status instead of the gate's (Q625).
#
# Why this exists: the Bash tool runs zsh, and a pipeline's status is its LAST
# stage's. `make check 2>&1 | tail -30; echo "EXIT=$?"` prints `EXIT=0` for a
# failing gate — a false green that reads identical to a real one. zsh has no
# `PIPESTATUS` to recover it (that is bash; in zsh the name expands to empty,
# which also reads as success), and the zsh spelling `$pipestatus` is rarely
# reached for. The rule is written in CLAUDE.md and in testing.md § The status
# you report is a claim too, and sessions still trip it — most recently a
# `git pull --ff-only 2>&1 | tail -5; echo "EXIT=$?"` that reported success for
# a pull that had failed. Prose has had its chance; this is the mechanical check.
#
# Severity is `ask`, never `deny`: piping a gate into a filter is sometimes
# exactly right (you want the output and do not care about the status), and a
# stateless hook cannot tell those apart. A false positive costs one keystroke.
#
# WHAT IT DETECTS — a registered gate segment (.claude/piped-gate-guard.json)
# whose FOLLOWING shell operator is a pipe:
#   * `make check | tail -30`, `make check 2>&1 | tail -30; echo "EXIT=$?"`
#   * `git pull --ff-only 2>&1 | tail -5`, `git push … | tail`
#   * `go test ./... | grep FAIL`, `scripts/ci/check-tools.sh | head`
#   * inside a command substitution: `f=$(make check | tail -1)`
#   * through a subshell's closing paren: `(cd cmd/agc && go test ./...) | tail`
#   * a `$PIPESTATUS` / `${PIPESTATUS[0]}` expansion anywhere in the command —
#     that name does not exist in zsh, so it is itself the bug, pipe or no pipe.
#
# WHAT IT DELIBERATELY DOES NOT DETECT (each would cost more false positives
# than it is worth, or cannot be seen from the command string at all):
#   * Quoted text and heredoc bodies are blanked before analysis. A commit
#     message or a `grep` pattern that merely NAMES `make check | tail` must not
#     prompt — that is the trap Q624 records against the sibling throttle hook.
#     The cost is that `bash -c 'make check | tail'` is invisible too.
#   * A gate piped inside a script file, or reached through a variable or
#     `eval` (`$CMD | tail`) — the hook sees one command string, not a program.
#   * A gate behind a throttle wrapper (`taskpolicy … go test … | tail`): the
#     wrapper's own flags make the head unparseable without a real parser.
#   * A pipeline whose status genuinely does not matter. Indistinguishable from
#     the bug, which is exactly why the decision is `ask`.
#   * `cmd > file 2>&1; echo $?` (correct), `set -o pipefail` in the same command
#     string, and the zsh `$pipestatus` array — all three are mitigations and
#     suppress the warning.
#   * Any command carrying a heavy `go … -race`, which the sibling
#     claude-go-throttle-hook.sh rewrites. Two hooks answering one Bash call is
#     undefined territory in Claude Code, and clobbering that rewrite would let
#     an unthrottled `-race` run freeze the GUI (Q92) — a worse failure than a
#     missed warning, so this hook stands down.
#
# Shell rather than a devtools/ Go program despite the parsing (bash-style.md
# § When not to write the script in shell): a PreToolUse hook runs on every Bash
# call from a fresh clone, before `make tools` has built anything.
#
# Wired up by .claude/settings.json as a PreToolUse hook on the Bash matcher.
# Requires jq; if jq is missing the hook is a no-op (fail-open).
set -euo pipefail

REGISTRY_REL=".claude/piped-gate-guard.json"
DOC_ANCHOR="docs/development/testing.md#the-status-you-report-is-a-claim-too"

# Populated by split_segments: SEG[i] is a shell segment, OP[i] the operator
# that FOLLOWS it ("|", "&&", ";", "close", "sub", "end", …).
SEG=()
OP=()

# emit_allow_unchanged exits 0 with no output, which lets the original command
# proceed through Claude Code's normal permission flow untouched.
emit_allow_unchanged() {
	exit 0
}

# emit_ask REASON returns an `ask` decision carrying REASON. The command string
# is never written anywhere persistent — the reason is the only place any of it
# is echoed, and that goes straight back to the session.
emit_ask() {
	jq -cn --arg reason "$1" '{
		hookSpecificOutput: {
			hookEventName: "PreToolUse",
			permissionDecision: "ask",
			permissionDecisionReason: $reason
		}
	}'
	exit 0
}

# strip_heredocs drops heredoc bodies, keeping the line that opens them. A body
# is free text — a commit message quoting a piped gate must not read as one.
# Herestrings (`<<<`) are masked first so their word is not taken for a
# delimiter.
strip_heredocs() {
	local input="$1" out="" line scan delim=""
	local hd_re="[<][<]-?[[:space:]]*['\"]?([A-Za-z_][A-Za-z0-9_]*)"
	while IFS= read -r line; do
		if [[ -n "$delim" ]]; then
			# `<<-` strips leading tabs from the terminator, so compare trimmed.
			if [[ "${line//[[:space:]]/}" == "$delim" ]]; then
				delim=""
			fi
			continue
		fi
		out+="$line"$'\n'
		scan="${line//<<</ }"
		if [[ "$scan" =~ $hd_re ]]; then
			delim="${BASH_REMATCH[1]}"
		fi
	done <<<"$input"
	printf '%s' "$out"
}

# blank_quotes S [KEEP_DOUBLE] replaces the CONTENTS of quoted spans with
# spaces, preserving length and the quote characters. Blanking only removes
# text, so it can never manufacture a gate head that was not typed; an
# unbalanced quote blanks the tail of the command, which fails quiet.
#
# A non-empty KEEP_DOUBLE preserves double-quoted content: the shell expands
# there, so `echo "EXIT=${PIPESTATUS[0]}"` really does read that variable, while
# the single-quoted `grep '$PIPESTATUS'` does not. Segmentation uses the
# blank-everything form; the expansion checks use this one.
blank_quotes() {
	local s="$1"
	local keep_double="${2:-}"
	local out="" i=0 n=${#s} c q=""
	# ANSI-C quoting so the single literal backslash reads unambiguously.
	local bs=$'\\'
	while ((i < n)); do
		c="${s:i:1}"
		if [[ -z "$q" ]]; then
			case "$c" in
			"$bs")
				out+="  "
				i=$((i + 2))
				continue
				;;
			"'" | '"')
				q="$c"
				out+="$c"
				;;
			*)
				out+="$c"
				;;
			esac
		elif [[ "$c" == "$q" ]]; then
			q=""
			out+="$c"
		elif [[ "$q" == '"' && "$c" == "$bs" ]]; then
			out+="  "
			i=$((i + 2))
			continue
		elif [[ "$q" == '"' && -n "$keep_double" ]]; then
			out+="$c"
		else
			out+=" "
		fi
		i=$((i + 1))
	done
	printf '%s' "$out"
}

# split_segments fills SEG/OP by scanning for the shell operators that end one
# command and start another. Grouping characters get their own operator so a
# gate inside a subshell or a command substitution still has a reachable
# successor.
split_segments() {
	local s="$1"
	local cur="" i=0 n=${#s} c c2
	SEG=()
	OP=()
	while ((i < n)); do
		c="${s:i:1}"
		c2="${s:i:2}"
		# fd-duplication and the merge-all redirect, not backgrounding. `2>&1` is
		# on nearly every gate invocation worth warning about, so reading its `&`
		# as an operator would split the gate off from its own pipe.
		case "$c2" in
		'>&' | '<&' | '&>')
			cur+="$c2"
			i=$((i + 2))
			continue
			;;
		esac
		# shellcheck disable=SC2016 # `$(` below is a literal two characters in
		# the command text, not an expansion to perform.
		case "$c2" in
		'&&' | '||')
			SEG+=("$cur")
			OP+=("$c2")
			cur=""
			i=$((i + 2))
			continue
			;;
		';;')
			SEG+=("$cur")
			OP+=(";")
			cur=""
			i=$((i + 2))
			continue
			;;
		'|&')
			SEG+=("$cur")
			OP+=("|")
			cur=""
			i=$((i + 2))
			continue
			;;
		'$(')
			SEG+=("$cur")
			OP+=("sub")
			cur=""
			i=$((i + 2))
			continue
			;;
		esac
		case "$c" in
		'|')
			SEG+=("$cur")
			OP+=("|")
			cur=""
			;;
		'&')
			SEG+=("$cur")
			OP+=("&")
			cur=""
			;;
		';' | $'\n')
			SEG+=("$cur")
			OP+=(";")
			cur=""
			;;
		'(' | '{')
			SEG+=("$cur")
			OP+=("open")
			cur=""
			;;
		')' | '}')
			SEG+=("$cur")
			OP+=("close")
			cur=""
			;;
		'`')
			SEG+=("$cur")
			OP+=("sub")
			cur=""
			;;
		*)
			cur+="$c"
			;;
		esac
		i=$((i + 1))
	done
	SEG+=("$cur")
	OP+=("end")
}

# effective_op INDEX prints the operator a segment's exit status actually flows
# into, seeing through the closing brackets of a group whose remainder is blank:
# in `(cd x && go test ./...) | tail` the `go test` segment ends at `)`, but its
# status is what the pipe consumes. Anything else between the group and the
# operator means the status does not reach it, so nothing is printed.
effective_op() {
	local i="$1" total="${#OP[@]}"
	while ((i < total)) && [[ "${OP[i]}" == "close" ]]; do
		i=$((i + 1))
		((i < total)) || return 0
		[[ -z "${SEG[i]//[[:space:]]/}" ]] || return 0
	done
	((i < total)) || return 0
	printf '%s' "${OP[i]}"
}

# segment_head prints a segment stripped of leading whitespace, `VAR=val`
# assignments, and the wrappers that precede a real command word — so the
# registry patterns can anchor at command position instead of searching the raw
# string, which is what makes them fire on the invocation and not on a `git
# show` or a commit message that names it.
segment_head() {
	local rest="${1#"${1%%[![:space:]]*}"}"
	local changed=1
	while ((changed)); do
		changed=0
		if [[ "$rest" =~ ^[A-Za-z_][A-Za-z0-9_]*=[^[:space:]]*[[:space:]]+ ]]; then
			rest="${rest#"${BASH_REMATCH[0]}"}"
			changed=1
			continue
		fi
		if [[ "$rest" =~ ^(time|sudo|nohup|command|exec|bash|sh|zsh)[[:space:]]+ ]]; then
			rest="${rest#"${BASH_REMATCH[0]}"}"
			changed=1
		fi
	done
	printf '%s' "$rest"
}

# matches_any HEAD PATTERNS — success when HEAD matches one of the newline-
# separated ERE patterns.
matches_any() {
	local head="$1" patterns="$2" pat
	while IFS= read -r pat; do
		[[ -n "$pat" ]] || continue
		if [[ "$head" =~ $pat ]]; then
			return 0
		fi
	done <<<"$patterns"
	return 1
}

# defers_to_throttle_hook returns success for a command the sibling
# claude-go-throttle-hook.sh will rewrite: an un-throttled heavy `go … -race`.
# Only one hook may answer a Bash call, and losing that rewrite is worse than
# losing this warning.
defers_to_throttle_hook() {
	local cmd="$1"
	[[ "$cmd" == *-race* ]] || return 1
	[[ "$cmd" =~ (^|[^[:alnum:]_-])go[[:space:]]+(build|test)([[:space:]]|$) ]] || return 1
	case "$cmd" in
	*local-throttle.sh* | *"taskpolicy "* | *"nice -n"*) return 1 ;;
	*) return 0 ;;
	esac
}

main() {
	command -v jq >/dev/null 2>&1 || emit_allow_unchanged

	local input
	input="$(cat)"
	[[ -n "$input" ]] || emit_allow_unchanged

	local tool_name command
	tool_name="$(printf '%s' "$input" | jq -r '.tool_name // empty' 2>/dev/null || true)"
	command="$(printf '%s' "$input" | jq -r '.tool_input.command // empty' 2>/dev/null || true)"
	[[ "$tool_name" == "Bash" ]] || emit_allow_unchanged
	[[ -n "$command" ]] || emit_allow_unchanged

	if defers_to_throttle_hook "$command"; then
		emit_allow_unchanged
	fi

	local stripped sanitized expansions
	stripped="$(strip_heredocs "$command")"
	sanitized="$(blank_quotes "$stripped")"
	expansions="$(blank_quotes "$stripped" keep-double)"

	# `PIPESTATUS` is bash's; the Bash tool runs zsh, where the name expands to
	# empty and every test against it reads as success. Flag the expansion
	# wherever it appears — this one is a bug on its own, not only after a pipe.
	# shellcheck disable=SC2016 # matching the literal text of someone else's
	# command; expanding it here is exactly what must not happen.
	case "$expansions" in
	*'$PIPESTATUS'* | *'${PIPESTATUS'*)
		emit_ask "This reads \$PIPESTATUS, which does not exist in zsh — the shell the Bash tool runs. It expands to empty, so the test against it reads as success whatever the pipeline did. zsh spells it \$pipestatus (lowercase, 1-indexed); better still, redirect and read the status directly: cmd > tmp/out.log 2>&1; echo \"EXIT=\$?\". See $DOC_ANCHOR."
		;;
	esac

	# Mitigated: pipefail propagates the failure, and zsh's $pipestatus recovers
	# each stage's status. `pipefail` is read off the quote-blanked form (it is
	# a shell word, never inside a string); $pipestatus off the expanding one.
	case "$sanitized" in
	*pipefail*) emit_allow_unchanged ;;
	esac
	# shellcheck disable=SC2016 # literal command text, as above.
	case "$expansions" in
	*'$pipestatus'* | *'${pipestatus'*) emit_allow_unchanged ;;
	esac

	local script_dir registry gates exempt
	script_dir="$(cd "$(dirname "$0")" && pwd)"
	registry="$script_dir/../../$REGISTRY_REL"
	[[ -r "$registry" ]] || emit_allow_unchanged
	gates="$(jq -r '.gates[]? // empty' "$registry" 2>/dev/null || true)"
	exempt="$(jq -r '.exempt[]? // empty' "$registry" 2>/dev/null || true)"
	[[ -n "$gates" ]] || emit_allow_unchanged

	split_segments "$sanitized"

	local i head
	for ((i = 0; i < ${#SEG[@]}; i++)); do
		[[ "$(effective_op "$i")" == "|" ]] || continue
		head="$(segment_head "${SEG[i]}")"
		[[ -n "$head" ]] || continue
		if matches_any "$head" "$exempt"; then
			continue
		fi
		if ! matches_any "$head" "$gates"; then
			continue
		fi
		emit_ask "\`${head:0:70}\` is piped into a filter, so this call's exit status is the filter's, not the gate's — a failure reads exactly like a pass, and zsh (the shell the Bash tool runs) has no PIPESTATUS to recover it. Redirect instead, then reconcile status against output: cmd > tmp/out.log 2>&1; echo \"EXIT=\$?\"; grep -E 'FAILED|Error [0-9]|^make:' tmp/out.log. Continue only if you want the output and not the status. See $DOC_ANCHOR."
	done

	emit_allow_unchanged
}

main "$@"
