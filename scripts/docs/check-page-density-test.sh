#!/usr/bin/env bash
#
# check-page-density-test.sh — both directions for each of the gate's two
# checks, plus the shapes that must stay green.
#
# The must-stay-green cases are the point: an admonition run broken by a
# heading, and a run quoted inside a fenced code block, are both legitimate and
# a naive scanner fails them.

set -euo pipefail
shopt -s inherit_errexit

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GATE="$SCRIPT_DIR/check-page-density.sh"
WORK="$(cd "$SCRIPT_DIR/../.." && pwd)/tmp/page-density-test"

rm -rf "$WORK"
mkdir -p "$WORK"
trap 'rm -rf "$WORK"' EXIT

pass=0
fail=0

check() {
    local name="$1" want="$2"
    shift 2
    local got=0
    "$GATE" "$@" >"$WORK/out.log" 2>&1 || got=$?
    if [[ "$got" == "$want" ]]; then
        pass=$((pass + 1))
        printf 'ok   %s\n' "$name"
    else
        fail=$((fail + 1))
        printf 'FAIL %s (want exit %s, got %s)\n' "$name" "$want" "$got"
        sed 's/^/       /' "$WORK/out.log"
    fi
}

# --- admonition runs -------------------------------------------------------

cat >"$WORK/wall.md" <<'EOF'
# Title

!!! note "one"

    body

!!! info "two"

    body

!!! info "three"

    body

!!! warning "four"

    body
EOF
check "four consecutive admonitions fail" 1 "$WORK/wall.md"

cat >"$WORK/atlimit.md" <<'EOF'
# Title

!!! note "one"

    body

!!! info "two"

    body

!!! info "three"

    body
EOF
check "three consecutive admonitions pass" 0 "$WORK/atlimit.md"

cat >"$WORK/broken.md" <<'EOF'
# Title

!!! note "one"

    body

!!! info "two"

    body

## A heading resets the run

!!! info "three"

    body

!!! warning "four"

    body
EOF
check "a heading between them resets the run" 0 "$WORK/broken.md"

cat >"$WORK/fenced.md" <<'EOF'
# Title

Documenting the syntax, not using it:

```markdown
!!! note "one"
!!! note "two"
!!! note "three"
!!! note "four"
!!! note "five"
```
EOF
check "admonitions inside a code fence are not counted" 0 "$WORK/fenced.md"

check "the limit is configurable" 1 --limit 1 "$WORK/atlimit.md"

# --- duplicate stat-tile leads ---------------------------------------------

cat >"$WORK/page-a.md" <<'EOF'
# A

<div class="gag-stats" markdown="0">
  <div class="gag-stat">
    <span class="gag-stat__num">1</span>
    <span class="gag-stat__label"><strong class="gag-stat__lead">Listener footprint at rest</strong>: shared</span>
  </div>
</div>
EOF

cat >"$WORK/page-b.md" <<'EOF'
# B

<div class="gag-stats" markdown="0">
  <div class="gag-stat">
    <span class="gag-stat__num">1</span>
    <span class="gag-stat__label"><strong class="gag-stat__lead">Listener footprint at rest</strong>: shared</span>
  </div>
</div>
EOF

cat >"$WORK/page-c.md" <<'EOF'
# C

<div class="gag-stats" markdown="0">
  <div class="gag-stat">
    <span class="gag-stat__num">0</span>
    <span class="gag-stat__label"><strong class="gag-stat__lead">Idle GPU pods between jobs</strong>: none</span>
  </div>
</div>
EOF

check "the same stat lead on two pages fails" 1 "$WORK/page-a.md" "$WORK/page-b.md"
check "distinct stat leads pass" 0 "$WORK/page-a.md" "$WORK/page-c.md"
check "one page using a lead once passes" 0 "$WORK/page-a.md"

# --- the real tree ---------------------------------------------------------

check "the committed docs tree passes" 0

printf '\ncheck-page-density-test: %d passed, %d failed\n' "$pass" "$fail"
((fail == 0))
