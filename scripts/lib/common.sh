# Shared helpers for the scripts/ tree. Source, don't execute:
#
#   REPO_ROOT="$(git rev-parse --show-toplevel)"
#   # shellcheck source=scripts/lib/common.sh
#   source "$REPO_ROOT/scripts/lib/common.sh"
#
# Callers must set REPO_ROOT before sourcing and have `set -euo pipefail`
# active (every script in this repo does, per the bash conventions).
# shellcheck shell=bash

# require_cmd NAME INSTALL_URL — fail fast with an install hint when a tool a
# gate needs is missing from PATH.
require_cmd() {
	local name="$1" url="$2"
	command -v "$name" >/dev/null 2>&1 || {
		echo "$name not found on PATH — install: $url" >&2
		exit 1
	}
}

# step MESSAGE — print a blank line then a "==> MESSAGE" progress header to
# stdout. die MESSAGE — print a blank line then "ERROR: MESSAGE" to stderr and
# exit 1. Both are the plain progress/abort idiom the live-probe scripts share
# (probe-live-run.sh, probe-investigations-cd.sh); kept here so they stay one
# implementation rather than a per-script copy (Q370 / F10).
step() { echo; echo "==> $*"; }
die() { echo; echo "ERROR: $*" >&2; exit 1; }

# git_candidates PATHSPEC... — emit the git-known paths matching PATHSPEC, one
# per line: tracked (--cached) PLUS untracked-and-not-gitignored (--others
# --exclude-standard). With no PATHSPEC, the whole worktree.
#
# The untracked half is load-bearing for any gate that scans the tree. Listing
# `--cached` alone makes a brand-new file invisible to its own first `make
# check` — a false green until the commit that tracks it (Q432 in the shellcheck
# gate; Q619 swept the doc-link, conflict-marker and plan-ref gates). The
# consequence for callers' users: a scratch file is opted out by *gitignoring*
# it, not by leaving it untracked — write it under the gitignored tmp/ at the
# repo root, per the repo temp-file convention.
#
# core.quotePath=false keeps a non-ASCII path literal rather than C-quoted, so
# it survives to the reader instead of being silently dropped by
# select_present_files. Impure (queries git) and cwd-sensitive: call it with the
# repo root as the working directory. Filtering lives in select_present_files.
git_candidates() {
	git -c core.quotePath=false ls-files --cached --others --exclude-standard -- "$@"
}

# select_present_files — read candidate paths on stdin, emit the ones a reader
# can actually open, in input order. Two classes have to go:
#   * a deleted-but-tracked path — `--cached` still lists a file removed from
#     the worktree, and a reader exits non-zero on a path it cannot open;
#   * duplicates — `--cached` lists an unmerged path once per merge stage, and
#     reading the same file three times triples its findings.
# Pure (reads stdin, stats paths). Drains all of stdin, so a `git_candidates |
# select_present_files` pipeline under `set -o pipefail` reports a failing
# candidate query instead of degrading to an empty — silently green — file set.
# Asserted by scripts/ci/shellcheck-scripts-test.sh.
#
# The seen-set is an associative array, not a string scanned with a glob: the
# doc-links oracle feeds this the whole tree (13.8k paths, 90% of them vendor/),
# and a substring test against an accumulating string costs O(N^2) — 38s there,
# against 0.5s for the keyed lookup.
select_present_files() {
	local path
	local -A seen=()
	while IFS= read -r path; do
		[[ -n "$path" ]] || continue
		[[ -f "$path" ]] || continue
		[[ -n "${seen[$path]:-}" ]] && continue
		seen[$path]=1
		printf '%s\n' "$path"
	done
}

# gh_curl DESCRIPTION METHOD URL [EXTRA_CURL_ARGS...] — make a GitHub API call,
# print the response body, and die() with the status and body when the HTTP code
# is not 2xx. Deliberately does NOT pass curl -f: error responses must be
# captured and shown, not swallowed. Extra args (auth headers, -d payloads) are
# forwarded verbatim after the URL. Shared by the live-probe scripts (Q370 / F10).
gh_curl() {
	local description="$1" method="$2" url="$3"
	shift 3
	local resp http_code body
	resp=$(curl -s -w '\n__HTTP_STATUS__%{http_code}' -X "$method" "$url" "$@")
	http_code=$(echo "$resp" | grep '__HTTP_STATUS__' | sed 's/.*__HTTP_STATUS__//')
	body=$(echo "$resp" | grep -v '__HTTP_STATUS__')
	if [[ ! "$http_code" =~ ^2 ]]; then
		echo >&2
		echo "ERROR: $description failed (HTTP $http_code)" >&2
		echo "  URL: $method $url" >&2
		echo "  Response: $body" >&2
		exit 1
	fi
	echo "$body"
}

# confirm_or_exit MESSAGE — print MESSAGE then require an interactive y/yes
# before continuing; exit non-zero on anything else. ASSUME_YES=1 skips the
# prompt (automation). Gate billable or destructive operations with this so a
# fat-finger is a no-op rather than a cloud spend.
confirm_or_exit() {
	local message="$1" reply
	printf '%s\n' "$message"
	if [[ "${ASSUME_YES:-}" == "1" ]]; then
		echo "ASSUME_YES=1 set — skipping confirmation."
		return 0
	fi
	read -r -p "Proceed? [y/N] " reply
	if [[ "$reply" != "y" && "$reply" != "Y" && "$reply" != "yes" ]]; then
		echo "Aborted — no changes made." >&2
		exit 1
	fi
}

# gke_get_credentials_and_verify PROJECT ZONE CLUSTER — fetch kubeconfig for the
# named GKE cluster, then fail closed unless it became the active kubectl
# context. Every later kubectl/helm call runs against the current context, so
# this one assertion guards them all from landing on the wrong cluster (e.g. a
# production context that happened to be selected). Callers must require_cmd
# gcloud, kubectl, and gke-gcloud-auth-plugin (GKE kubeconfigs authenticate
# through that external plugin).
gke_get_credentials_and_verify() {
	# Names are gke_-prefixed (not project/zone/cluster) so shellcheck's SC2153
	# does not flag callers' ${PROJECT}/${ZONE}/${CLUSTER} as case-typos of them.
	local gke_project="$1" gke_zone="$2" gke_cluster="$3"
	echo "Fetching cluster credentials for ${gke_cluster}..."
	gcloud container clusters get-credentials "$gke_cluster" \
		--project="$gke_project" --zone="$gke_zone"
	local expected current
	expected="gke_${gke_project}_${gke_zone}_${gke_cluster}"
	current="$(kubectl config current-context)"
	if [[ "$current" != "$expected" ]]; then
		echo "Refusing to continue: kubectl context is '${current}'," >&2
		echo "expected '${expected}'. Aborting before any cluster writes." >&2
		exit 1
	fi
	echo "Active kubectl context: ${current}"
}

# workspace_modules — print the disk path of every module in go.work, one per
# line. The repo is a Go workspace, so go tooling runs per module (a repo-root
# `go test ./...` does not work — see docs/development/go-workspaces.md).
workspace_modules() {
	go work edit -json | jq -r '.Use[].DiskPath'
}

# nonworkspace_modules — print the disk path of every tracked Go module kept OUT
# of go.work, one per line, sorted. Vendored trees are the sole exclusion: they
# are third-party source, not modules this repo maintains.
#
# DISCOVERED, not enumerated — every tracked go.mod minus the go.work members.
# As a hand-maintained list this was a gate that covers a new module only if
# someone remembers to widen it, and the cost of forgetting is silence: nothing
# tests, lints, scans, or tidies the module, and every gate stays green.
# Discovery makes a new module opt OUT rather than opt IN.
#
# This is the whole-repo view — the one the tidy flow needs, because `go mod
# tidy` normalises a module's go.mod/go.sum whoever wrote its imports.
# firstparty_nonworkspace_modules is the narrower view for gates that only
# reason about first-party code.
#
# Impure (queries git and go) and cwd-sensitive: call it with the repo root as
# the working directory. Asserted by scripts/go/go-lint-scope-test.sh.
nonworkspace_modules() {
	local workspace=$'\n' m dir
	while IFS= read -r m; do
		workspace+="${m#./}"$'\n'
	done < <(workspace_modules)
	while IFS= read -r m; do
		[[ "$m" == */go.mod ]] || continue
		[[ "$m" == */vendor/* ]] && continue
		dir="${m%/go.mod}"
		[[ "$workspace" == *$'\n'"$dir"$'\n'* ]] && continue
		printf '%s\n' "$dir"
	done < <(git ls-files --cached --others --exclude-standard -- '*go.mod' | sort)
}

# firstparty_nonworkspace_modules — nonworkspace_modules minus `tools/`, which
# pins third-party build tools via blank imports and holds no first-party code.
# These are the modules deliberately kept out of go.work
# (docs/development/go-workspaces.md § First-party Go tooling stays outside the
# workspace): a workspace module would drag every change to it through an image
# bake and an e2e cluster, because check-path-filters.sh requires the
# workspace-covering filters to match every go.work module.
#
# The cost of that choice is that go-test.sh, go-lint.sh and go-vulncheck.sh
# each need a separate GOWORK=off pass over these: they run one workspace-wide
# invocation, which cannot reach a module go.work does not list.
#
# coverage.sh is a partial exception: its ratchet derives one profile from the
# workspace build list and filters it per module, so these modules carry no
# baseline row and no floor. It does still run their tests, unmeasured — `make
# check` calls cover-check in place of `make test`, so skipping them there would
# leave the fast gate never executing them at all.
firstparty_nonworkspace_modules() {
	local dir
	while IFS= read -r dir; do
		[[ "$dir" == tools ]] && continue
		printf '%s\n' "$dir"
	done < <(nonworkspace_modules)
}

# init_throttle — populate THROTTLE_JOBS / THROTTLE_PREFIX from
# scripts/agent/local-throttle.sh: a parallelism cap (physical cores − 2) and a
# low-priority QoS command prefix on an interactive GUI dev shell, both empty
# on CI/headless/SSH so heavy phases run at full speed there. See that
# script's header for the detection rules and rationale (an unthrottled run
# can trip the macOS WindowServer watchdog and freeze the GUI).
#
# THROTTLE_PREFIX is a command prefix ("nice -n 10 taskpolicy -d throttle",
# "nice -n 19 ionice -c 3")
# that callers expand UNQUOTED so it word-splits into command + args; when
# empty it disappears entirely.
init_throttle() {
	# shellcheck disable=SC2034  # consumed by the sourcing scripts
	THROTTLE_JOBS="$("$REPO_ROOT/scripts/agent/local-throttle.sh" jobs)"
	# shellcheck disable=SC2034  # consumed by the sourcing scripts
	THROTTLE_PREFIX="$("$REPO_ROOT/scripts/agent/local-throttle.sh" prefix)"
}

# serialize_heavy_build — bound how many of the calling script's heavy phases run
# at once across concurrent worktrees/sessions on one dev machine, so several
# `make check` runs don't collectively saturate a small core count and blow the
# linter/test timeouts. The parallelism cap (init_throttle) bounds ONE run's
# fan-out but is blind to siblings; this holds one of N machine-wide slots
# (`local-throttle.sh slots`, default 2) so the N+1st run queues instead of
# piling on. Idle servers and CI are NOT bounded (local-throttle.sh reports no
# lock there) — those SHOULD run fully parallel.
#
# N used to be 1 — strictly one heavy run machine-wide — which made the gate set
# the pace for a machine running several sessions: one run used `jobs` threads
# while every sibling blocked for its whole duration. GAG_HEAVY_BUILD_SLOTS=1
# restores that behaviour; see scripts/agent/local-throttle.sh for why the
# default moved.
#
# It re-execs the calling script once holding an advisory lock for the script's
# whole lifetime, then proceeds normally in the re-exec'd child. Call it AFTER
# `cd "$REPO_ROOT"` and sourcing this file, passing the script's own "$@":
#
#   serialize_heavy_build "$@"
#
# No-op when there is no lock file (CI/headless/SSH/non-GUI), when perl is
# absent, or when a slot is already held (the sentinel env var), so a locked
# script may invoke other locked scripts without deadlocking on itself.
#
# Implemented with perl's flock: an advisory lock available on both macOS (which
# ships no flock(1)) and Linux, and — crucially — released automatically when the
# holding process dies, so a Ctrl-C'd or killed build never strands a stale lock
# that wedges every later run. With N > 1 the acquire is a non-blocking sweep
# over the slots plus a 1 s retry, since flock cannot block on "any of N".
#
# The wait is REPORTED, not just endured: a heartbeat every 30 s while queued and
# a one-line total on acquire. The recommended way to run the gate under
# contention is in the background while you do the docs/PR work the gate's
# verdict doesn't gate (docs/development/parallel-dispatch.md), and then the run's
# log is the only signal there is — a single line followed by hours of silence is
# indistinguishable from a hang, which is why the queue depth this semaphore
# exists to bound was only ever anecdotal ("waits up to 5 h were observed").
# stderr only: the lock paths, their count, and the acquire protocol are
# unchanged, so a worktree still on older code contends here exactly as before.
serialize_heavy_build() {
	[[ -n "${GAG_HEAVY_BUILD_LOCK_HELD:-}" ]] && return 0
	local throttle="$REPO_ROOT/scripts/agent/local-throttle.sh"
	local slots i lock locks=()
	slots="$("$throttle" slots)"
	# Empty (CI/headless/non-GUI) or malformed: run unbounded, as before.
	[[ "$slots" =~ ^[0-9]+$ ]] || return 0
	for (( i = 1; i <= slots; i++ )); do
		lock="$("$throttle" lockfile "$i")"
		[[ -n "$lock" ]] && locks+=("$lock")
	done
	(( ${#locks[@]} > 0 )) || return 0
	command -v perl >/dev/null 2>&1 || return 0
	export GAG_HEAVY_BUILD_LOCK_HELD=1
	# perl takes a slot, runs the script as a child, and exits with its status;
	# the lock fd lives in perl and releases when perl exits. Locks that cannot be
	# opened at all degrade to running unserialized rather than failing the build.
	exec perl -MFcntl=:flock -e '
		my $n = shift @ARGV;
		my @paths = splice(@ARGV, 0, $n);
		my ($fh, $start, $next_report) = (undef, time, 0);
		while (1) {
			my $openable = 0;
			for my $p (@paths) {
				open(my $h, ">", $p) or next;
				$openable++;
				if (flock($h, LOCK_EX|LOCK_NB)) { $fh = $h; last; }
			}
			last if $fh;
			exec @ARGV if !$openable;
			my $queued = time - $start;
			if ($queued >= $next_report) {
				printf STDERR "==> waiting for a heavy-build slot (%d in use, queued %ds)...\n", scalar(@paths), $queued;
				$next_report = $queued + 30;
			}
			select(undef, undef, undef, 1);
		}
		my $queued = time - $start;
		printf STDERR "==> heavy-build slot acquired after %ds queued\n", $queued if $queued >= 5;
		my $rc = system @ARGV;
		exit 255 if $rc == -1;
		exit($rc & 127 ? 128 + ($rc & 127) : $rc >> 8);
	' "${#locks[@]}" "${locks[@]}" bash "$0" "$@"
}

# release_identity_regexp — print the cosign --certificate-identity-regexp that
# a legitimate release signature must match. Releases are cut by pushing a v*
# tag, so the keyless Fulcio cert records publish.yml running from
# `refs/tags/vX.Y.Z`. The pattern is anchored and TAGS-ONLY: it deliberately
# rejects a signature minted from a branch ref (`refs/heads/...`) — e.g. a
# workflow_dispatch run from a scratch branch that overwrote a released GHCR tag
# (Q124). Shared by verify-release.sh (the verifier) and verify-release-test.sh
# (the assertion that the tags-only property holds). Single arg: the
# `owner/repo` slug, default actions-gateway/github-actions-gateway.
release_identity_regexp() {
	local slug="${1:-actions-gateway/github-actions-gateway}"
	printf '^https://github.com/%s/\\.github/workflows/publish\\.yml@refs/tags/v.*$' "$slug"
}

# resolve_release_tag [repo_root] — print the release an adopter is running, as
# `<tag>\t<source>`, or nothing at all when no release exists yet (a fresh fork).
# The caller decides whether that is a skip or an error.
#
# "The release" is the highest stable vX.Y.Z tag: 0.x and any `-rc`/`-alpha`/
# `-beta` suffix are prereleases nobody installs. That is the same test
# hooks/release_version.py applies to the docs-site announce bar, and publish.yml
# and pages.yml to a tag they are handed — one definition, three readers.
#
# Tags are read locally first, then off origin: a shallow CI checkout carries no
# tags, and an unfetched tag list is indistinguishable from a tagless repo
# without asking. $GAG_RELEASE_TAG overrides both, for exercising a gate against
# a version that does not exist yet.
resolve_release_tag() {
	local tree="${1:-.}" tag
	if [[ -n "${GAG_RELEASE_TAG:-}" ]]; then
		# shellcheck disable=SC2016 # the source label names the variable, literally
		printf '%s\t%s\n' "$GAG_RELEASE_TAG" '$GAG_RELEASE_TAG'
		return
	fi
	tag="$(git -C "$tree" tag --list 'v*' | _stable_release_tag)"
	if [[ -n "$tag" ]]; then
		printf '%s\tlocal tags\n' "$tag"
		return
	fi
	# No `origin`, or no network, is "cannot tell" rather than an error here.
	tag="$({ git -C "$tree" ls-remote --tags --refs origin 'v*' 2>/dev/null || true; } |
		awk -F/ '{ print $NF }' | _stable_release_tag)"
	[[ -n "$tag" ]] && printf '%s\torigin remote\n' "$tag"
	return 0
}

# _stable_release_tag — filter a stream of tag names to the highest stable one.
_stable_release_tag() {
	awk '/^v[0-9]+\.[0-9]+\.[0-9]+$/ && !/^v0\./' | sort -V | tail -1
}

# resolve_prepared_release [repo_root] — print the release a candidate is being
# cut for, as `vX.Y.Z`, or nothing when no candidate is outstanding.
#
# A release is "prepared" when a prerelease tag exists for a version that has no
# stable tag yet: `v1.5.0-rc.3` present and `v1.5.0` absent means 1.5.0 is being
# cut. It is derived from tags alone, so nothing declares or clears it, and it
# stops answering the moment the stable tag lands.
#
# It exists so the docs can pin the release they are about to publish *before*
# the tag. The site builds each version from its own tag, so pins bumped after
# the tag never reach the release's published page: three of the four releases
# cut since 1.0.0 shipped the previous version's install command as their landing
# page, and v1.3.0 avoided it only because a hand-fix happened to land first.
#
# Deliberately NOT folded into resolve_release_tag, whose answer means "the
# release an adopter is running" and is read by publish.yml, pages.yml and the
# announce bar. A candidate is not something anybody runs.
resolve_prepared_release() {
	local tree="${1:-.}" tags newest_pre stable
	tags="$(git -C "$tree" tag --list 'v*')"
	# Highest prerelease of a non-0.x version, by version order.
	newest_pre="$(printf '%s\n' "$tags" |
		awk '/^v[0-9]+\.[0-9]+\.[0-9]+-/ && !/^v0\./' | sort -V | tail -1)"
	[[ -n "$newest_pre" ]] || return 0
	stable="${newest_pre%%-*}"
	# Already released: the candidate is spent and the stable tag is the answer.
	# Herestring, not a pipe: `grep -q` exits on the match, the writer takes
	# SIGPIPE, and under `set -o pipefail` the dead pipeline reads as no-match —
	# so a pipeline falsifies a tag that IS present once the list outgrows the
	# 64 KiB pipe buffer, and only then (Q982).
	grep -qxF -- "$stable" <<<"$tags" && return 0
	printf '%s\n' "$stable"
}

# release_pin_exempt_versions_regexp — print the pattern matching a release
# version that appears in the pin-bearing pages on purpose and must not be
# bumped. Currently only v2.0.0, the announced v1alpha1/v2alpha1 removal
# release. The pattern is v-prefixed; chart pins are written without the `v`, so
# a bare `2.0.0` still fails. Shared by the two gates below.
release_pin_exempt_versions_regexp() {
	printf '%s' '^v2\.0\.0$'
}

# release_version_literals FILE — emit one `<line>\t<literal>\t<kind>` record per
# release-version literal in FILE.
#
# `kind` is `semver` for a full X.Y.Z (optionally v-prefixed) or `patchline` for
# the `X.Y.z` shorthand the install callout uses for "newer patch releases".
# A match flanked by a digit — or by a dot followed by a digit — is part of a
# longer dotted run (a four-part version, a dotted-quad address) and is not a
# release version. A line beginning `Measured on kind ` records what was actually
# installed for a measurement, so its versions are skipped: bumping one would
# falsify the record.
#
# Shared by check-release-pins.sh, which reads the working tree, and
# verify-published-docs.sh, which reads the pages that tree published (Q784) —
# one extractor, so a pin shape the source gate sees cannot be invisible to the
# published-site gate.
release_version_literals() {
	awk '
		function flanked(before, after, after2) {
			if (before ~ /[0-9]/) return 1
			if (before == "." ) return 1
			if (after ~ /[0-9]/) return 1
			if (after == "." && after2 ~ /[0-9]/) return 1
			return 0
		}
		/^Measured on kind / { next }
		{
			rest = $0
			offset = 0
			while (match(rest, /v?[0-9]+\.[0-9]+\.([0-9]+|z)/)) {
				tok = substr(rest, RSTART, RLENGTH)
				before = (RSTART + offset > 1) ? substr($0, RSTART + offset - 1, 1) : ""
				end = RSTART + RLENGTH
				after  = substr(rest, end, 1)
				after2 = substr(rest, end + 1, 1)
				offset += end - 1
				rest = substr(rest, end)
				if (flanked(before, after, after2)) continue
				printf "%d\t%s\t%s\n", NR, tok, (tok ~ /z$/ ? "patchline" : "semver")
			}
		}
	' "$1"
}

# Placeholder sha256 digest used only to render the Helm chart for scanning
# and validation: production installs pin the image digests, so auditing the
# digest-pinned form reflects the SHIPPED posture. A digest is also REQUIRED
# to render at all — the chart fails closed when any of the four image digests
# (gmc/agc/proxy/wrapper) is empty (Q96, Q307), and scripts/manifest/manifest-validate.sh
# asserts each rejection. The value must satisfy values.schema.json's
# sha256:[a-f0-9]{64} pattern. Shared by polaris-scan.sh and manifest-validate.sh.
# shellcheck disable=SC2034  # consumed by the sourcing scripts
POLARIS_RENDER_DIGEST="${POLARIS_RENDER_DIGEST:-sha256:1111111111111111111111111111111111111111111111111111111111111111}"

# --set-string flags pinning ALL FOUR image digests, the minimum required to
# render the chart since Q307 (agc/proxy/wrapper now fail closed at render like
# gmc did). Use in place of a single --set-string gmc.image.digest=… whenever a
# render must succeed. shellcheck disable=SC2034 — consumed by sourcing scripts.
# shellcheck disable=SC2034
RENDER_DIGEST_ARGS=(
	--set-string "gmc.image.digest=$POLARIS_RENDER_DIGEST"
	--set-string "agc.image.digest=$POLARIS_RENDER_DIGEST"
	--set-string "proxy.image.digest=$POLARIS_RENDER_DIGEST"
	--set-string "wrapper.image.digest=$POLARIS_RENDER_DIGEST"
)
