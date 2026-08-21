#!/usr/bin/env bash
#
# doc-blocks-test.sh — behavioural tests for doc-blocks.sh.
#
# The gate's own failure mode is the one it exists to catch. A parser that
# stopped matching the annotation would report every page clean, and a floor that
# counted nothing would report every page covered — both green, both silent,
# both indistinguishable from a page that is genuinely fine. So every assertion
# here is a KNOWN-BAD document that must go red, paired with the good document
# that must stay green: asserting only the green half proves the gate runs, not
# that it can fail.
#
# Fixtures are written to a $$-suffixed scratch dir under tmp/ and removed on
# exit, so concurrent suites under `make scripts-test` cannot collide.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

GATE="scripts/docs/doc-blocks.sh"
WORK="tmp/doc-blocks-test.$$"
mkdir -p "$WORK"
trap 'rm -rf "$WORK"' EXIT

pass=0
fail=0

# expect_red NAME FIXTURE_BODY — the gate must reject this document.
expect_red() {
	local name="$1" body="$2" out rc
	printf '%s\n' "$body" >"$WORK/case.md"
	set +e
	out="$("$GATE" --check "$WORK/case.md" 2>&1)"
	rc=$?
	set -e
	if ((rc == 0)); then
		printf 'FAIL: %s — the gate accepted a document it must reject\n' "$name" >&2
		printf '%s\n' "$out" >&2
		fail=$((fail + 1))
		return
	fi
	pass=$((pass + 1))
}

# expect_green NAME FIXTURE_BODY — the gate must accept this document.
expect_green() {
	local name="$1" body="$2" out rc
	printf '%s\n' "$body" >"$WORK/case.md"
	set +e
	out="$("$GATE" --check "$WORK/case.md" 2>&1)"
	rc=$?
	set -e
	if ((rc != 0)); then
		printf 'FAIL: %s — the gate rejected a document it must accept\n' "$name" >&2
		printf '%s\n' "$out" >&2
		fail=$((fail + 1))
		return
	fi
	pass=$((pass + 1))
}

# expect_red_output NAME FIXTURE_BODY SUBSTRING — red, and the message says why.
# A gate that fails with the wrong reason sends the reader to the wrong block,
# which is the failure mode Q958 set out to avoid.
expect_red_output() {
	local name="$1" body="$2" want="$3" out rc
	printf '%s\n' "$body" >"$WORK/case.md"
	set +e
	out="$("$GATE" --check "$WORK/case.md" 2>&1)"
	rc=$?
	set -e
	if ((rc == 0)); then
		printf 'FAIL: %s — the gate accepted a document it must reject\n' "$name" >&2
		fail=$((fail + 1))
		return
	fi
	if [[ "$out" != *"$want"* ]]; then
		printf 'FAIL: %s — red for the wrong reason; wanted %q in:\n%s\n' "$name" "$want" "$out" >&2
		fail=$((fail + 1))
		return
	fi
	pass=$((pass + 1))
}

GOOD='# Doc

<!-- gag:verify id=first mode=run teardown=namespace -->
```sh
kubectl create namespace team-a
```

<!-- gag:verify id=second mode=apply needs=first teardown=object -->
```yaml
apiVersion: v1
kind: ConfigMap
```
'

expect_green 'a well-formed document' "$GOOD"

# --- the annotation grammar ---------------------------------------------------

expect_red_output 'a duplicate id' '# Doc

<!-- gag:verify id=dup mode=run -->
```sh
true
```

<!-- gag:verify id=dup mode=run -->
```sh
true
```
' 'is already used at line'

expect_red_output 'a needs= naming nothing' '# Doc

<!-- gag:verify id=only mode=apply needs=ghost -->
```yaml
kind: ConfigMap
```
' 'needs "ghost", which is not declared earlier'

# Ordering is the whole prerequisite mechanism: the executor applies in file
# order, so a needs= pointing FORWARD would be applied before its prerequisite.
expect_red_output 'a needs= naming a later block' '# Doc

<!-- gag:verify id=early mode=apply needs=late -->
```yaml
kind: ConfigMap
```

<!-- gag:verify id=late mode=apply -->
```yaml
kind: ConfigMap
```
' 'not declared earlier'

expect_red_output 'an unknown mode' '# Doc

<!-- gag:verify id=only mode=execute -->
```sh
true
```
' 'expected one of'

expect_red_output 'an unknown key' '# Doc

<!-- gag:verify id=only mode=run whenever=tuesday -->
```sh
true
```
' 'unknown key'

expect_red_output 'a missing id' '# Doc

<!-- gag:verify mode=run -->
```sh
true
```
' 'id= is required'

expect_red_output 'a missing mode' '# Doc

<!-- gag:verify id=only -->
```sh
true
```
' 'mode= is required'

expect_red_output 'an id that is not a slug' '# Doc

<!-- gag:verify id=Not_A_Slug mode=run -->
```sh
true
```
' 'is not a lowercase slug'

expect_red_output 'an unknown teardown' '# Doc

<!-- gag:verify id=only mode=run teardown=incinerate -->
```sh
true
```
' 'expected one of'

# --- the annotation must reach a block ----------------------------------------

expect_red_output 'an annotation with no fence under it' '# Doc

<!-- gag:verify id=orphan mode=run -->

Just prose, no fence.
' 'is not followed by a fenced block'

expect_red_output 'an annotation at the end of the file' '# Doc

<!-- gag:verify id=trailing mode=run -->
' 'is not followed by a fenced block'

# The first annotation is orphaned by the second, which is what the message says.
expect_red_output 'two annotations in a row' '# Doc

<!-- gag:verify id=one mode=run -->
<!-- gag:verify id=two mode=run -->
```sh
true
```
' 'id=one is not followed by a fenced block'

# --- the language must match the mode -----------------------------------------

expect_red_output 'a yaml block marked mode=run' '# Doc

<!-- gag:verify id=only mode=run -->
```yaml
kind: ConfigMap
```
' 'A manifest block is mode=apply'

expect_red_output 'an sh block marked mode=apply' '# Doc

<!-- gag:verify id=only mode=apply -->
```sh
true
```
' 'A command block is mode=run'

# --- a declared gap must state its reason -------------------------------------

expect_red_output 'a skip with no reason' '# Doc

<!-- gag:verify id=only mode=skip -->
```sh
true
```
' 'no reason='

expect_green 'a skip that states its reason' '# Doc

<!-- gag:verify id=only mode=skip reason=needs-a-kubelet -->
```sh
true
```
'

# --- an unannotated block is inert, not an error ------------------------------
#
# Coverage is opt-in by design: half the fences on an install page are
# illustrative, and failing on them would make the gate noise. This asserts the
# design rather than merely leaving it untested.
expect_green 'an unannotated block' '# Doc

```yaml
kind: ConfigMap
```
'

# --- the floor ----------------------------------------------------------------
#
# The floor is what catches a block quietly dropping out of coverage, so it has
# to be exercised against the registry rather than an ad-hoc file argument (a
# file named on the command line carries no floor). Both directions: the real
# page must meet its floor, and a page with a block demoted to mode=skip must not.

floor_case() {
	local name="$1" body="$2" want_rc="$3" out rc
	local doc="$WORK/floored.md"
	printf '%s\n' "$body" >"$doc"
	set +e
	# A file named on the command line carries no registry floor, so --floor
	# supplies one; it is the same comparison the registry path runs.
	out="$("$GATE" --check --floor 2 "$doc" 2>&1)"
	rc=$?
	set -e
	if ((rc != want_rc)); then
		printf 'FAIL: %s — wanted rc=%d, got rc=%d:\n%s\n' "$name" "$want_rc" "$rc" "$out" >&2
		fail=$((fail + 1))
		return
	fi
	pass=$((pass + 1))
}

floor_case 'two executed blocks meets a floor of two' "$GOOD" 0

floor_case 'one block demoted to skip breaks the floor' '# Doc

<!-- gag:verify id=first mode=run teardown=namespace -->
```sh
kubectl create namespace team-a
```

<!-- gag:verify id=second mode=skip reason=demoted -->
```yaml
apiVersion: v1
kind: ConfigMap
```
' 1

# --- the shipped page ---------------------------------------------------------
#
# The registry default, end to end. This is what would catch the annotations and
# the parser drifting apart on the page that actually ships.
if ! "$GATE" --check >/dev/null 2>&1; then
	printf 'FAIL: the shipped registry does not pass its own gate\n' >&2
	fail=$((fail + 1))
else
	pass=$((pass + 1))
fi

# The page must still parse to the record count the executor's floor expects. A
# parser that silently stopped matching would return zero records and every
# per-block assertion in the Go walk would pass vacuously.
executed="$("$GATE" --emit docs/getting-started.md | awk -F'\t' '$2 != "skip"' | wc -l | tr -d ' ')"
if ((executed < 10)); then
	printf 'FAIL: docs/getting-started.md emits %s executable blocks, expected at least 10\n' "$executed" >&2
	fail=$((fail + 1))
else
	pass=$((pass + 1))
fi

printf 'doc-blocks-test: %d passed, %d failed\n' "$pass" "$fail"
((fail == 0))
