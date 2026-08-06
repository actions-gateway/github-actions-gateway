#!/usr/bin/env bash
#
# claude-go-throttle-hook.sh — Claude Code PreToolUse hook that auto-throttles
# raw `go build` / `go test` commands run outside `make`.
#
# Why this exists (Q92): the Makefile auto-throttles its own recipes
# (background-QoS prefix + I/O throttle + parallelism cap via
# scripts/agent/local-throttle.sh), but a bare `go build` / `go test` that Claude or
# the user runs *directly* through the Bash tool gets none of that — full
# priority, uncapped, no I/O throttle. On a small Mac a heavy run (especially
# `-race`, a ~5–10× CPU/memory/I/O amplifier), *especially alongside other
# concurrent sessions*, can saturate the machine and trip the WindowServer
# watchdog: the GUI freezes/restarts. This was observed for real — an
# unthrottled `go test -race` in a parallel worktree session crashed
# WindowServer during the session that filed Q92.
#
# This hook automates the manual workaround documented in CLAUDE.md
# (`$(scripts/agent/local-throttle.sh prefix) go test ...`): it transparently prepends
# the same platform QoS prefix that `make` uses, so a forgotten prefix no longer
# means an unthrottled run.
#
# Behaviour (fail-open everywhere — a parse/tool error never blocks a command):
#   * Non-Bash tool, or not a `go build`/`go test` command  -> allow unchanged.
#   * Throttle inactive (CI / headless / SSH / non-GUI, per local-throttle.sh)
#     or already throttled (command already carries the prefix or calls
#     local-throttle.sh)                                      -> allow unchanged.
#   * Simple `go build`/`go test` (no shell chaining)         -> rewrite the
#     command to prepend the QoS prefix and auto-allow it. Auto-allow is safe:
#     the command is strictly a bare `go build`/`go test`, the same boundary the
#     repo's `Bash(go build *)` / `Bash(go test *)` allowlist already trusts. It
#     is also necessary — without it the rewritten `taskpolicy … go test …` form
#     would no longer match that allowlist and would trigger a *new* prompt.
#   * A compound command (contains `&&`, `|`, `;`, `$()`, …) or one with a
#     redirect (`>`, `<`) carrying `-race` (the dangerous amplifier) -> rewrite
#     it to insert the QoS prefix before its single `go build`/`go test` token
#     and return an `ask` decision (not `allow`). `ask` still throttles the run
#     while keeping the permission system, the user, and the security-guard hooks
#     (branch-guard, workspace-guard) in the loop — an `allow` would ride the
#     whole command (its other segments, an outside-workspace redirect) past
#     them, which is why the bare form uses `allow` but this one must not.
#   * The same compound/redirected `-race` case with more than one `go build`/
#     `go test` invocation to throttle and one prefix to place -> `deny` with the
#     specific reason. We deny rather than throttle one invocation and leave the
#     other running at full tilt.
#   * Any other such `go build`/`go test` (no `-race`)         -> allow unchanged.
#
# Why redirects/chaining disqualify the *auto-allow* path (but not throttling):
# an outside-workspace redirect target (`go test … > /tmp/out`) is exactly what
# the workspace-guard hook prompts on, and a chained segment could be any guarded
# op. Claude Code does not document how a PreToolUse `allow` composes with another
# hook's `ask`/`deny`, so — secure by default — we never emit `allow` for a
# command a guard might care about. The auto-*allowed* class stays a bare
# `go build`/`go test`: no guarded file command, no git op, no redirect. But the
# throttle itself must still reach the dangerous `-race` forms, so for those we
# rewrite and emit `ask` — which, unlike `allow`, leaves the command in the
# permission flow for the user and the guard hooks to act on however Claude Code
# composes them — instead of blocking the caller outright.
#
# What counts as an invocation (Q624): only a `go` token in *command position* —
# the start of the command, or after `;`/`&&`/`||`/`|`/`(`/a newline, past any
# leading `VAR=val` assignments. Quoted strings and heredoc bodies are text to
# the receiving command, not commands, so they are skipped. Without that a
# `git commit -F -` heredoc whose message quotes `go test -race` read as a go
# invocation: two mentions denied the commit outright, one silently rewrote the
# prefix *into the message*. Known limit: a heredoc fed to a shell
# (`bash <<EOF … go test -race … EOF`) is likewise skipped and runs unthrottled;
# the body is opaque here, and telling a shell heredoc from a message one means
# special-casing command names, which is the failure mode this replaced.
#
# Wired up by .claude/settings.json as a PreToolUse hook on the Bash matcher.
# Requires jq; if jq is missing the hook is a no-op (fail-open).
set -euo pipefail

# emit_allow_unchanged exits 0 with no output, which lets the original command
# proceed through Claude Code's normal permission flow untouched.
emit_allow_unchanged() {
	exit 0
}

# scan_go_invocations prints one `START END` line per command-position
# `go build`/`go test` invocation in the command: START is the offset of the `go`
# token, END the offset just past its last argument (its terminating operator,
# redirect, or end of string). It prints nothing when there is none.
#
# The scan tracks shell quoting, heredoc bodies and command position, so a `go`
# that is merely *named* — in a `git commit` message, a heredoc body, a `grep`
# pattern — is not an invocation. It also means a wrapper form reports nothing:
# in `taskpolicy -d throttle go test`, `go` is an argument to `taskpolicy`, not
# a command.
scan_go_invocations() {
	local cmd="$1"
	local n=${#cmd}
	local i=0 next_i ch state=plain
	local cmdpos=1 in_word=0 wstart=-1 word=""
	local pending_go=-1 go_start=-1
	local flush op endinv
	local heredocs="" j q dash delim close
	local consumed spec hdash hdelim line probe
	# A lone backslash, as a variable so it can be a `case` pattern without
	# reading as a botched quote escape (shellcheck SC1003).
	local bslash=$'\\'

	while ((i < n)); do
		ch="${cmd:i:1}"

		if [[ "$state" == sq ]]; then
			if [[ "$ch" == "'" ]]; then state=plain; else word+="$ch"; fi
			i=$((i + 1))
			continue
		fi
		if [[ "$state" == dq ]]; then
			if [[ "$ch" == '"' ]]; then
				state=plain
			elif [[ "$ch" == "$bslash" ]] && ((i + 1 < n)); then
				i=$((i + 1))
				word+="${cmd:i:1}"
			else
				word+="$ch"
			fi
			i=$((i + 1))
			continue
		fi

		flush=0 op=0 endinv=0 next_i=-1

		case "$ch" in
		"'" | '"')
			if ((in_word == 0)); then
				in_word=1
				wstart=$i
			fi
			if [[ "$ch" == "'" ]]; then state=sq; else state=dq; fi
			;;
		"$bslash")
			if ((in_word == 0)); then
				in_word=1
				wstart=$i
			fi
			if ((i + 1 < n)); then
				i=$((i + 1))
				word+="${cmd:i:1}"
			fi
			;;
		' ' | $'\t')
			flush=1
			;;
		$'\n')
			flush=1 op=1 endinv=1
			# A newline ends the line that opened any pending heredocs, so their
			# bodies start here. Consume each one through its delimiter line.
			if [[ -n "$heredocs" ]]; then
				consumed=$((i + 1))
				while [[ -n "$heredocs" ]]; do
					spec="${heredocs%%$'\n'*}"
					heredocs="${heredocs#*$'\n'}"
					hdash="${spec:0:1}"
					hdelim="${spec:1}"
					while ((consumed < n)); do
						line="${cmd:consumed}"
						line="${line%%$'\n'*}"
						probe="$line"
						# `<<-` strips leading tabs from the delimiter line.
						if [[ "$hdash" == '-' ]]; then
							probe="${probe#"${probe%%[!$'\t']*}"}"
						fi
						consumed=$((consumed + ${#line} + 1))
						if [[ "$probe" == "$hdelim" ]]; then break; fi
					done
				done
				next_i=$consumed
			fi
			;;
		';' | '&' | '|' | '(' | ')' | '{' | '}' | '`')
			flush=1 op=1 endinv=1
			;;
		'<' | '>')
			# A redirect ends the invocation's argument list but does not start a
			# new command, so cmdpos is left alone.
			flush=1 endinv=1
			if [[ "${cmd:i:2}" == '<<' && "${cmd:i:3}" != '<<<' ]]; then
				# `<<` / `<<-` opens a heredoc: record its delimiter (the body is
				# skipped when the current line ends) and resume after it.
				j=$((i + 2))
				dash='='
				delim=''
				if [[ "${cmd:j:1}" == '-' ]]; then
					dash='-'
					j=$((j + 1))
				fi
				while [[ "${cmd:j:1}" == ' ' || "${cmd:j:1}" == $'\t' ]]; do j=$((j + 1)); done
				close="${cmd:j:1}"
				if [[ "$close" == "'" || "$close" == '"' ]]; then
					j=$((j + 1))
					while ((j < n)) && [[ "${cmd:j:1}" != "$close" ]]; do
						delim+="${cmd:j:1}"
						j=$((j + 1))
					done
					j=$((j + 1))
				else
					while ((j < n)); do
						q="${cmd:j:1}"
						case "$q" in
						' ' | $'\t' | $'\n' | ';' | '&' | '|' | '<' | '>' | '(' | ')') break ;;
						"$bslash")
							j=$((j + 1))
							delim+="${cmd:j:1}"
							j=$((j + 1))
							;;
						*)
							delim+="$q"
							j=$((j + 1))
							;;
						esac
					done
				fi
				heredocs+="$dash$delim"$'\n'
				next_i=$j
			fi
			;;
		*)
			if ((in_word == 0)); then
				in_word=1
				wstart=$i
			fi
			word+="$ch"
			;;
		esac

		if ((flush)) && ((in_word)); then
			if ((cmdpos)); then
				if [[ "$word" =~ ^[A-Za-z_][A-Za-z0-9_]*= ]]; then
					: # a `VAR=val` assignment keeps the next word in command position
				elif [[ "$word" == go ]]; then
					pending_go=$wstart
					cmdpos=0
				else
					cmdpos=0
				fi
			elif ((pending_go >= 0)); then
				if [[ "$word" == build || "$word" == test ]]; then
					go_start=$pending_go
				fi
				pending_go=-1
			fi
			in_word=0 wstart=-1 word=""
		fi
		if ((endinv)) && ((go_start >= 0)); then
			printf '%s %s\n' "$go_start" "$i"
			go_start=-1
		fi
		if ((op)); then
			cmdpos=1 pending_go=-1
		fi

		if ((next_i >= 0)); then i=$next_i; else i=$((i + 1)); fi
	done

	# End of string: flush the trailing word and close any open invocation.
	if ((in_word)); then
		if ((cmdpos == 0)) && ((pending_go >= 0)) && [[ "$word" == build || "$word" == test ]]; then
			go_start=$pending_go
		fi
	fi
	if ((go_start >= 0)); then
		printf '%s %s\n' "$go_start" "$n"
	fi
}

# already_throttled returns success when the text preceding a go invocation
# already carries a throttle prefix (taskpolicy / nice) or computes one via
# local-throttle.sh — i.e. the documented manual workaround, or a previous wrap,
# is already in place. It is passed only the pre-invocation text so a commit
# message naming `taskpolicy` cannot suppress a real throttle.
#
# The literal `taskpolicy`/`nice` forms are the case where the wrapper's own
# `go` argument is not in command position and so is invisible to
# scan_go_invocations anyway; what this still has to catch is
# `$(scripts/agent/local-throttle.sh prefix) go test …`, whose `go` follows the
# substitution's `)` and *is* in command position.
already_throttled() {
	local cmd="$1"
	case "$cmd" in
	*local-throttle.sh* | *"taskpolicy "* | *"nice -n"*) return 0 ;;
	*) return 1 ;;
	esac
}

# is_compound returns success when the command contains shell control operators
# that introduce additional commands (chaining, pipes, command substitution,
# backgrounding, newlines). Such a command cannot be auto-allowed: its other
# segments would ride past the permission system on our `allow`.
is_compound() {
	local cmd="$1"
	# Single quotes are intentional: these are literal substrings to match
	# (`$(` is command substitution), not expressions to expand.
	# shellcheck disable=SC2016
	case "$cmd" in
	*'|'* | *'&'* | *';'* | *'$('* | *'`'* | *$'\n'*) return 0 ;;
	*) return 1 ;;
	esac
}

# has_redirect returns success when the command contains a redirection (`>`,
# `>>`, `<`). Any redirect disqualifies the transparent auto-allow path: a
# redirect to an outside-workspace path is exactly what workspace-guard prompts
# on, and we must never auto-allow past that guard. fd-dups like `2>&1` are
# swept up too — harmless to exclude (they fall back to the normal flow), and
# not worth the fragile parsing it would take to tell them from file redirects.
has_redirect() {
	local cmd="$1"
	case "$cmd" in
	*'>'* | *'<'*) return 0 ;;
	*) return 1 ;;
	esac
}

# rewrite_at prints the command with the QoS prefix inserted immediately before
# the `go` token at OFFSET, preserving everything around it — a subshell
# wrapper, a leading `cd`, redirects, `VAR=val` assignments.
rewrite_at() {
	local cmd="$1" offset="$2" prefix="$3"
	printf '%s%s %s' "${cmd:0:offset}" "$prefix" "${cmd:offset}"
}

main() {
	# jq is required to parse the hook payload and emit a rewrite safely.
	command -v jq >/dev/null 2>&1 || emit_allow_unchanged

	local input
	input="$(cat)"
	[[ -n "$input" ]] || emit_allow_unchanged

	local tool_name command
	tool_name="$(printf '%s' "$input" | jq -r '.tool_name // empty' 2>/dev/null || true)"
	command="$(printf '%s' "$input" | jq -r '.tool_input.command // empty' 2>/dev/null || true)"
	[[ "$tool_name" == "Bash" ]] || emit_allow_unchanged
	[[ -n "$command" ]] || emit_allow_unchanged

	# Locate the command-position go invocations. None (including a command that
	# only *names* one) means there is nothing to throttle.
	local invocations
	invocations="$(scan_go_invocations "$command")"
	[[ -n "$invocations" ]] || emit_allow_unchanged

	# Count them, note where the first one starts, and decide `-race` from the
	# invocations' own argument text rather than the whole string — a `-race` in
	# a commit message is not a race run.
	local count=0 first_start=-1 has_race=0 start end
	while read -r start end; do
		count=$((count + 1))
		if ((count == 1)); then first_start=$start; fi
		if [[ "${command:start:end - start}" == *-race* ]]; then has_race=1; fi
	done <<<"$invocations"

	if already_throttled "${command:0:first_start}"; then
		emit_allow_unchanged
	fi

	# Resolve the platform throttle prefix from the sibling script. An empty
	# prefix means throttling is off (CI / headless / SSH / unsupported OS), so
	# there is nothing to do.
	local script_dir throttle prefix
	script_dir="$(cd "$(dirname "$0")" && pwd)"
	throttle="$script_dir/local-throttle.sh"
	[[ -x "$throttle" ]] || emit_allow_unchanged
	prefix="$("$throttle" prefix 2>/dev/null || true)"
	[[ -n "$prefix" ]] || emit_allow_unchanged

	if is_compound "$command" || has_redirect "$command"; then
		# We can only safely auto-*allow* a bare, single go build/test. A compound
		# command (its other segments would ride past the permission system) or one
		# with a redirect (an outside-workspace target is what workspace-guard
		# prompts on) must not be auto-allowed. For the genuinely dangerous case —
		# one carrying `-race` — we still MUST throttle it, so instead of blocking
		# we rewrite it and return `ask`: the throttle is applied and the prompt
		# keeps the user and the guard hooks in the loop. A non-`-race` form stays
		# on the normal permission flow (and the guards) unchanged.
		if ((has_race)); then
			if ((count == 1)); then
				local racecmd
				racecmd="$(rewrite_at "$command" "$first_start" "$prefix")"
				jq -cn --arg cmd "$racecmd" '{
					hookSpecificOutput: {
						hookEventName: "PreToolUse",
						permissionDecision: "ask",
						permissionDecisionReason: "Auto-throttled a heavy `go ... -race` (I/O and CPU demoted below the desktop) — confirm to run the throttled form. An unthrottled -race run can saturate the machine and freeze the local GUI; see CLAUDE.md.",
						updatedInput: { command: $cmd }
					}
				}'
				return 0
			fi
			# More than one go build/test to throttle and only one prefix to place.
			# Deny with the specific reason rather than emit a command that
			# throttles one invocation and leaves the other running at full tilt.
			jq -cn '{
				hookSpecificOutput: {
					hookEventName: "PreToolUse",
					permissionDecision: "deny",
					permissionDecisionReason: "Blocked: this `go ... -race` has more than one go build/test invocation, so the hook cannot insert the throttle prefix unambiguously. Give each `go ... -race` its own throttle prefix ($(scripts/agent/local-throttle.sh prefix)), run each go line on its own, or use the matching `make` target (it throttles itself). See CLAUDE.md."
				}
			}'
			return 0
		fi
		emit_allow_unchanged
	fi

	# Simple command: prepend the prefix and auto-allow the throttled form. A
	# non-compound command without a redirect holds exactly one invocation —
	# a second would need an operator between them.
	local newcmd
	newcmd="$(rewrite_at "$command" "$first_start" "$prefix")"

	jq -cn --arg cmd "$newcmd" '{
		hookSpecificOutput: {
			hookEventName: "PreToolUse",
			permissionDecision: "allow",
			permissionDecisionReason: "Auto-throttled heavy go build/test (I/O and CPU demoted below the desktop) to keep the local GUI responsive — see CLAUDE.md.",
			updatedInput: { command: $cmd }
		}
	}'
}

main "$@"
