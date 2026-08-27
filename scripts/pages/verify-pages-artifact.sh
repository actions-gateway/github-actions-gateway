#!/usr/bin/env bash
#
# verify-pages-artifact.sh — fail when the assembled Pages artifact does not
# contain the version the run is deploying (Q1000).
#
# The publish job builds a version with mike, pushes the gh-pages tree, then
# assembles the artifact from that branch with `git archive`. Every one of those
# steps can report success while the tree that reaches _site is the PREVIOUS
# one: a stale archive still has a root index.html and a CNAME, which is all the
# check here used to assert, so a deploy of the wrong tree passed its own
# verification and reported green.
#
# Reads the assembled directory, not the site. The published half is
# verify-pages-live.sh, which runs after the deploy.
#
# Usage:
#   verify-pages-artifact.sh --site DIR --version V [--alias A]
#
#   --site DIR   Assembled artifact root (the workflow's _site).
#   --version V  Version id this run deployed (e.g. 1.6.0, or dev).
#   --alias A    Alias this run claimed (e.g. stable). Blank = none claimed,
#                which is a backport that is not the highest release, and every
#                `dev` deploy.
#   --stable-tag This run is a stable release tag push. Resolved from the git
#                ref alone, so it shares no derivation with the alias above.
#
# Assertions: verify-pages-artifact-test.sh, under `make scripts-test`.
set -euo pipefail
shopt -s inherit_errexit

SITE=""
VERSION=""
ALIAS=""
stable_tag=0

while (($# > 0)); do
	case "$1" in
	--site)
		SITE="${2:-}"
		shift
		;;
	--version)
		VERSION="${2:-}"
		shift
		;;
	--alias)
		ALIAS="${2:-}"
		shift
		;;
	--stable-tag)
		stable_tag=1
		;;
	*)
		printf 'verify-pages-artifact: unknown argument: %s\n' "$1" >&2
		exit 2
		;;
	esac
	shift
done

if [[ -z "$SITE" || -z "$VERSION" ]]; then
	echo "usage: $0 --site DIR --version V [--alias A]" >&2
	exit 2
fi

SITE="${SITE%/}"
rc=0

# err MESSAGE — report a failed assertion as a workflow annotation. `::error::`
# is inert outside Actions, where the same line still reads as the failure.
err() {
	printf '::error::verify-pages-artifact: %s\n' "$1" >&2
	rc=1
}

# The root of a mike tree is the redirect stub `mike set-default` writes, not a
# built page: until some version claims the default the apex domain 404s while
# this workflow still reports success on every push. That is what happened
# between the Q238 cutover and the first seed.
if [[ ! -f "$SITE/index.html" ]]; then
	err "artifact has no root index.html, so the apex domain will 404. No version owns the default; run 'mike set-default' via a workflow_dispatch seed (docs/development/website.md, Seeding already-released versions)."
fi

if [[ ! -f "$SITE/CNAME" ]]; then
	err "artifact has no root CNAME; deploying would clear the custom domain."
fi

# The version tree itself. A stale archive has every version EXCEPT the one this
# run just built, which is the only shape the two assertions above cannot see.
if [[ ! -f "$SITE/$VERSION/index.html" ]]; then
	err "artifact has no $VERSION/index.html, so this run would deploy a tree without the version it just built. The archive did not pick up the mike commit; re-run the publish job."
fi

versions="$SITE/versions.json"
if [[ ! -f "$versions" ]]; then
	err "artifact has no versions.json, so the version selector would serve nothing."
elif ! jq -e . "$versions" > /dev/null 2>&1; then
	err "artifact versions.json is not valid JSON."
else
	# `jq -e` exits 1 on a false/null result, which is the "not listed" answer
	# rather than an error; anything else is jq failing and must not read as a
	# clean pass.
	if ! jq -e --arg v "$VERSION" 'any(.[]; .version == $v)' "$versions" > /dev/null; then
		err "artifact versions.json does not list $VERSION, so the version selector would not offer it. Listed: $(jq -r 'map(.version) | join(", ")' "$versions")"
	elif [[ -n "$ALIAS" ]]; then
		if ! jq -e --arg v "$VERSION" --arg a "$ALIAS" \
			'any(.[]; .version == $v and ((.aliases // []) | index($a) != null))' "$versions" > /dev/null; then
			err "artifact versions.json does not put the '$ALIAS' alias on $VERSION, so /$ALIAS/ would keep serving the previous release. Alias currently on: $(jq -r --arg a "$ALIAS" '[.[] | select((.aliases // []) | index($a) != null) | .version] | join(", ") | if . == "" then "<no version>" else . end' "$versions")"
		fi
		# --alias-type=copy makes an alias a real directory: Pages artifact
		# deploys do not follow symlinks, so a symlinked alias deep-links to 404.
		if [[ ! -f "$SITE/$ALIAS/index.html" ]]; then
			err "artifact has no $ALIAS/index.html, so /$ALIAS/ would 404."
		fi
	fi
fi

# The alias assertion above compares against what the mike step REPORTED it
# claimed, and that report comes from the same `mike list` comparison the check
# exists to audit. Taking it at face value is fail-open in the one property that
# matters: a `mike list` that errors or changes shape leaves `highest` empty, so
# the step takes its backport branch, `stable` is never moved, and the claim
# arrives empty — which switches the assertion off rather than failing it. The
# run then goes green with /stable/ on the previous release, which is the
# user-visible symptom of the incident this check was written for, reached by a
# different cause.
#
# So derive the rule again here, from the artifact's own versions.json, and
# require the alias independently. `--stable-tag` comes from the ref, and the
# version list is read as `.version` fields rather than scraped as text, so
# neither input can fail the way the step's does.
if ((stable_tag)) && [[ -f "$versions" ]] && jq -e . "$versions" > /dev/null 2>&1; then
	highest="$(jq -r '.[].version' "$versions" |
		grep -E '^[0-9]+\.[0-9]+\.[0-9]+$' | sort -V | tail -1 || true)"
	if [[ -n "$highest" && "$highest" == "$VERSION" ]]; then
		if ! jq -e --arg v "$VERSION" \
			'any(.[]; .version == $v and ((.aliases // []) | index("stable") != null))' \
			"$versions" > /dev/null; then
			err "$VERSION is the highest release in this artifact but does not carry the 'stable' alias, so /stable/ would keep serving the previous release. The mike step reported claiming '${ALIAS:-<none>}'; check its 'mike list' step 3 output for a swallowed failure."
		fi
	fi
fi

if ((rc == 0)); then
	printf 'verify-pages-artifact: %s carries %s%s, root and CNAME present\n' \
		"$SITE" "$VERSION" "${ALIAS:+ (alias $ALIAS)}"
fi
exit "$rc"
