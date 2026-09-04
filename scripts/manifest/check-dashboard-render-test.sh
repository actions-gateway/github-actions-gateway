#!/usr/bin/env bash
#
# Unit tests for scripts/manifest/check-dashboard-render.py.
#
# The gate's whole job is to go red, so every case that must fail is paired with
# a control that must pass. Without the controls, a checker that refused every
# dashboard change — or one that had stopped reading the diff and passed
# everything — would look identical to a working gate from the outside.
#
# Three of the cases are the ones a naive gate gets wrong, and each is here
# because the row named it: a change split across commits is judged as one diff,
# a description-only edit is exempt, and the baseline is the merge base rather
# than origin/main's tip.
#
# The fixtures set refs/remotes/origin/main with update-ref rather than pushing:
# the checker keys on the merge base, and a push to a fixture remote is denied by
# this workstation's branch guard. The PNGs are text — the gate compares blob
# ids, never pixels, so a real render would only make the fixtures slower.
set -euo pipefail
shopt -s inherit_errexit

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"
CHECKER="$HERE/check-dashboard-render.py"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

fail=0
ok()  { printf 'ok   %s\n' "$1"; }
bad() { printf 'FAIL %s\n' "$1"; fail=1; }

# A dashboard small enough to read, carrying the two shapes the gate tells apart:
# a panel description, and a targets list a render would show.
dashboard() {  # dashboard <repo> <name> <description> <extra-target-or-empty>
    local extra=""
    if [[ -n "${4:-}" ]]; then
        extra=",
        { \"expr\": \"$4\", \"refId\": \"B\" }"
    fi
    mkdir -p "$1/deploy/monitoring"
    cat > "$1/deploy/monitoring/grafana-dashboard-$2.json" <<JSON
{
  "title": "$2",
  "panels": [
    {
      "id": 1,
      "description": "$3",
      "targets": [
        { "expr": "up", "refId": "A" }$extra
      ]
    }
  ]
}
JSON
}

render() {  # render <repo> <name> <content>
    mkdir -p "$1/docs/assets"
    printf '%s\n' "$3" > "$1/docs/assets/grafana-dashboard-$2.png"
}

newrepo() {  # newrepo <dir> -> a repo with one dashboard and its render
    local r="$1"
    git init -q -b main "$r"
    # Q820: no detached maintenance racing the next command in a fixture repo.
    git -C "$r" config maintenance.auto false
    git -C "$r" config user.email t@e.com
    git -C "$r" config user.name T
    dashboard "$r" platform "A panel." ""
    render "$r" platform "PNG-v1"
}

seal() {  # seal <repo> — commit the base and make it the merge base
    git -C "$1" add -A
    git -C "$1" commit -qm base
    git -C "$1" update-ref refs/remotes/origin/main HEAD
    git -C "$1" checkout -q -b claude/work
}

run() {  # run <repo> [args...] -> rc, output in $TMP/out
    local repo="$1" rc=0
    shift
    (cd "$repo" && python3 "$CHECKER" "$@") > "$TMP/out" 2>&1 || rc=$?
    return "$rc"
}

expect() {  # expect <want-rc> <repo> <name> [pattern]
    local want="$1" repo="$2" name="$3" pat="${4:-}" rc=0
    run "$repo" || rc=$?
    die_if_killed "$name" "$rc" "$want"
    if [[ "$rc" != "$want" ]]; then
        bad "$name (rc=$rc want=$want)"
        sed 's/^/       /' "$TMP/out" | head -3
        return
    fi
    if [[ -n "$pat" ]] && ! grep -q "$pat" "$TMP/out"; then
        bad "$name (rc matched but output lacks '$pat')"
        sed 's/^/       /' "$TMP/out" | head -3
        return
    fi
    ok "$name"
}

# --- the defect the gate exists to catch (#1526) ---------------------------

R="$TMP/series"; newrepo "$R"; seal "$R"
dashboard "$R" platform "A panel." "actions_gateway_scale_set_name_collision"
git -C "$R" commit -qam "add a series to the panel"
expect 1 "$R" "a new panel series with no render fails" "did not"

# The control: the same JSON change, rendered. Without it the case above is
# equally consistent with a gate that refuses every dashboard change.
render "$R" platform "PNG-v2"
git -C "$R" commit -qam "re-render the platform dashboard"
expect 0 "$R" "the same change with a fresh render passes"

# And the other direction: a branch that touches no dashboard is not asked for a
# render, so the gate is not simply demanding a PNG on every branch.
R="$TMP/unrelated"; newrepo "$R"; seal "$R"
printf 'a note\n' > "$R/notes.md"
git -C "$R" add notes.md
git -C "$R" commit -qm "an unrelated change"
expect 0 "$R" "a branch touching no dashboard passes" "0 dashboard"

# --- the whole diff, not a commit ------------------------------------------

# A legitimate split: the JSON lands in one commit and the render three commits
# later. Judged per commit this is red; judged as the branch's diff it is green,
# which is the whole point of keying on the merge base.
R="$TMP/split"; newrepo "$R"; seal "$R"
dashboard "$R" platform "A panel." "actions_gateway_up"
git -C "$R" commit -qam "change the dashboard"
printf 'interim\n' > "$R/notes.md"
git -C "$R" add notes.md
git -C "$R" commit -qm "unrelated work in between"
render "$R" platform "PNG-v2"
git -C "$R" commit -qam "render it"
expect 0 "$R" "a JSON commit and a render commit three apart pass as one diff"

# The same branch with its render commit missing must still be red, or the case
# above proves only that the gate stopped looking after the first commit.
git -C "$R" checkout -q -b claude/no-render "HEAD~2"
expect 1 "$R" "the same split without its render commit fails"

# The other ordering, and the one that separates a whole-diff gate from a gate
# reading only the tip commit: the render is committed first and the JSON
# follows. Both are in the branch's diff, so this is green; a gate judging
# HEAD~1..HEAD sees a dashboard change with no render beside it and goes red.
R="$TMP/split2"; newrepo "$R"; seal "$R"
render "$R" platform "PNG-v2"
git -C "$R" commit -qam "re-render ahead of the change"
dashboard "$R" platform "A panel." "actions_gateway_up"
git -C "$R" commit -qam "change the dashboard"
expect 0 "$R" "a render committed before its JSON change passes as one diff"

# --- a description-only edit is exempt (#1531) ------------------------------

R="$TMP/desc"; newrepo "$R"; seal "$R"
dashboard "$R" platform "A panel, now explained at length." ""
git -C "$R" commit -qam "reword the panel description"
expect 0 "$R" "a reworded description needs no re-render"

# Reformatting the file around a description edit must not defeat the exemption:
# the comparison is over parsed JSON, not over the diff's lines.
R="$TMP/reflow"; newrepo "$R"; seal "$R"
python3 - "$R" <<'PY'
import json, pathlib, sys
p = pathlib.Path(sys.argv[1]) / "deploy/monitoring/grafana-dashboard-platform.json"
doc = json.loads(p.read_text())
doc["panels"][0]["description"] = "Reworded."
p.write_text(json.dumps(doc, indent=8, sort_keys=True))
PY
git -C "$R" commit -qam "reword and reformat"
expect 0 "$R" "a description edit survives the file being reformatted"

# A description added where a panel had none is the same exemption: the info
# icon it adds is not what the screenshot is read for, and demanding a
# fifteen-minute render for a doc string is friction with nothing behind it.
R="$TMP/descnew"; newrepo "$R"; seal "$R"
python3 - "$R" <<'PY'
import json, pathlib, sys
p = pathlib.Path(sys.argv[1]) / "deploy/monitoring/grafana-dashboard-platform.json"
doc = json.loads(p.read_text())
del doc["panels"][0]["description"]
p.write_text(json.dumps(doc))
PY
git -C "$R" commit -qam "drop the description"
expect 0 "$R" "removing a description needs no re-render"

# The exemption must not widen past descriptions: a title renders.
R="$TMP/title"; newrepo "$R"; seal "$R"
python3 - "$R" <<'PY'
import json, pathlib, sys
p = pathlib.Path(sys.argv[1]) / "deploy/monitoring/grafana-dashboard-platform.json"
doc = json.loads(p.read_text())
doc["panels"][0]["description"] = "Reworded."
doc["title"] = "Platform, renamed"
p.write_text(json.dumps(doc))
PY
git -C "$R" commit -qam "reword the description and rename the dashboard"
expect 1 "$R" "a title change beside a description edit still fails"

# --- a dashboard with no render at all --------------------------------------

R="$TMP/new"; newrepo "$R"; seal "$R"
dashboard "$R" tenant "A tenant panel." ""
git -C "$R" add -A
git -C "$R" commit -qm "add a second dashboard"
expect 1 "$R" "a new dashboard with no screenshot fails" "does not exist"

render "$R" tenant "PNG-v1"
git -C "$R" add -A
git -C "$R" commit -qm "render the new dashboard"
expect 0 "$R" "the new dashboard passes once rendered"

# --- the merge base, not origin/main's tip ----------------------------------

# main changes a dashboard while this branch is behind. Keyed on the tip that
# reads as a change this branch made and demands a render for someone else's
# work; keyed on the merge base it is not this branch's diff at all.
R="$TMP/behind"; newrepo "$R"; seal "$R"
printf 'branch work\n' > "$R/notes.md"
git -C "$R" add notes.md
git -C "$R" commit -qm "work on the branch"
git -C "$R" checkout -q main
dashboard "$R" platform "A panel." "someone_elses_series"
git -C "$R" commit -qam "a dashboard change on main"
git -C "$R" update-ref refs/remotes/origin/main HEAD
git -C "$R" checkout -q claude/work
expect 0 "$R" "a dashboard main changed while the branch was behind is not the branch's"

# --- the override -----------------------------------------------------------

R="$TMP/override"; newrepo "$R"; seal "$R"
dashboard "$R" platform "A panel." "actions_gateway_up"
git -C "$R" commit -qam "change the dashboard"
expect 1 "$R" "the override case is red without the override"
rc=0
(cd "$R" && DASHBOARD_ALLOW_STALE_RENDER=grafana-dashboard-platform.json \
    python3 "$CHECKER") > "$TMP/out" 2>&1 || rc=$?
die_if_killed "the override clears it and says so" "$rc"
if [[ "$rc" == 0 ]] && grep -q "excused" "$TMP/out"; then
    ok "the override clears it and says so"
else
    bad "the override clears it and says so (rc=$rc)"
    sed 's/^/       /' "$TMP/out" | head -3
fi

# --- a read that could not be taken is not a verdict ------------------------

R="$TMP/badjson"; newrepo "$R"; seal "$R"
printf '{ "panels": [ ' > "$R/deploy/monitoring/grafana-dashboard-platform.json"
git -C "$R" commit -qam "truncate the dashboard"
expect 2 "$R" "unparseable JSON exits 2 rather than passing" "refusing to guess"

# A selection matching nothing is the one failure the gate cannot report on
# itself: every dashboard would pass in silence. The dashboards are shipped
# artifacts, so an empty match means the glob lost them.
R="$TMP/noglob"; newrepo "$R"; seal "$R"
git -C "$R" mv deploy/monitoring/grafana-dashboard-platform.json \
    deploy/monitoring/platform-dashboard.json
git -C "$R" commit -qm "rename the dashboard out from under the glob"
expect 2 "$R" "a selection matching no dashboard exits 2 rather than passing" "matched nothing"

R="$TMP/nobase"; newrepo "$R"
git -C "$R" add -A
git -C "$R" commit -qm base
git -C "$R" checkout -q -b claude/work
expect 2 "$R" "no origin/main exits 2 rather than passing" "refusing to guess"

# --- explicit refs ----------------------------------------------------------

# The gate is run against arbitrary refs to reproduce a historical PR, so the
# option has to select the same diff the default path would.
R="$TMP/refs"; newrepo "$R"; seal "$R"
dashboard "$R" platform "A panel." "actions_gateway_up"
git -C "$R" commit -qam "change the dashboard"
rc=0
(cd "$R" && python3 "$CHECKER" --base HEAD~1 --head HEAD) > "$TMP/out" 2>&1 || rc=$?
die_if_killed "--base/--head select the diff to judge" "$rc"
if [[ "$rc" == 1 ]]; then
    ok "--base/--head select the diff to judge"
else
    bad "--base/--head select the diff to judge (rc=$rc want=1)"
    sed 's/^/       /' "$TMP/out" | head -3
fi

exit "$fail"
