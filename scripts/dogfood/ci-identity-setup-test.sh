#!/usr/bin/env bash
# ci-identity-setup-test.sh — asserts the keyless CI identity bootstrap against a
# scripted `gcloud` and `gh`.
#
# The subject writes IAM on the dogfood project and protection rules on the
# repository, and its whole value is the trust boundary those writes draw. So
# the boundary is asserted as written — the provider's condition, the principal
# allowed to impersonate, the refs the environment admits — along with the
# idempotency that makes a re-run safe: nothing is re-created, and a drifted
# condition is converged rather than left standing.
#
# Both CLIs are stubbed on PATH, so the real script runs end to end.
set -euo pipefail
shopt -s inherit_errexit

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SUBJECT="$SCRIPT_DIR/ci-identity-setup.sh"

pass=0
fail=0
ok() {
	printf '[ci-identity-setup-test] ok   %s\n' "$1"
	pass=$((pass + 1))
}
bad() {
	printf '[ci-identity-setup-test] FAIL %s\n' "$1" >&2
	fail=$((fail + 1))
}

WORK="$(mktemp -d)"
BIN="$WORK/bin"
LOG="$WORK/calls.log"
ENV_BODY="$WORK/env-body.json"
cleanup() { rm -rf "$WORK"; }
trap cleanup EXIT
export LOG ENV_BODY

mkdir -p "$BIN"
# STUB_EXISTS      1 => every describe succeeds (a re-run), else 404
# STUB_CONDITION   the provider's current attributeCondition on a re-run
# STUB_POLICIES    "type name" lines the environment already carries
# STUB_UID         the id `gh api users/<login>` answers
cat >"$BIN/gcloud" <<'STUB_BODY'
#!/usr/bin/env bash
set -uo pipefail
printf 'gcloud %s\n' "$*" >>"${LOG}"
case "$*" in
"projects describe"*) echo 424242 ;;
*"providers describe"*)
	[[ "${STUB_EXISTS:-}" == 1 ]] || exit 1
	printf '%s\n' "${STUB_CONDITION}"
	;;
*" describe "*) [[ "${STUB_EXISTS:-}" == 1 ]] || exit 1 ;;
esac
exit 0
STUB_BODY
cat >"$BIN/gh" <<'STUB_BODY'
#!/usr/bin/env bash
set -uo pipefail
printf 'gh %s\n' "$*" >>"${LOG}"
case "$*" in
"api repos/octo/repo --jq"*) echo "111 222" ;;
"api users/"*) printf '%s\n' "${STUB_UID}" ;;
"api -X PUT"*) cat >"${ENV_BODY}" ;;
*"deployment-branch-policies --jq"*) printf '%s\n' "${STUB_POLICIES:-}" ;;
esac
exit 0
STUB_BODY
chmod +x "$BIN/gcloud" "$BIN/gh"

OUT=""
RC=0
# run_case [VAR=VALUE...] — run the subject with the defaults below overridden.
run_case() {
	: >"$LOG"
	: >"$ENV_BODY"
	set +e
	OUT="$(env PATH="$BIN:$PATH" PROJECT=dogfood-proj CLUSTER=gag-dogfood ZONE=us-east1-b \
		REPO=octo/repo REVIEWERS=alice ASSUME_YES=1 STUB_UID=7 STUB_EXISTS= \
		STUB_CONDITION= STUB_POLICIES= "$@" "$SUBJECT" 2>&1 </dev/null)"
	RC=$?
	set -e
}

want_rc() {
	if [[ "$RC" == "$2" ]]; then ok "$1"; else bad "$1: want exit $2, got $RC"$'\n'"$OUT"; fi
}
want_call() {
	if grep -qF -- "$2" "$LOG"; then ok "$1"; else bad "$1: no call containing: $2"; fi
}
no_call() {
	if grep -qF -- "$2" "$LOG"; then bad "$1: unexpected call containing: $2"; else ok "$1"; fi
}
want_out() {
	if [[ "$OUT" == *"$2"* ]]; then ok "$1"; else bad "$1: output lacks: $2"; fi
}
# want_body NAME NEEDLE — the environment body the subject PUT.
want_body() {
	if grep -qF -- "$2" "$ENV_BODY"; then ok "$1"; else bad "$1: $(cat "$ENV_BODY")"; fi
}

CONDITION="assertion.repository_id == '111' && assertion.repository_owner_id == '222' && assertion.environment == 'dogfood-validation'"
PRINCIPAL="principalSet://iam.googleapis.com/projects/424242/locations/global/workloadIdentityPools/github-actions/attribute.environment/dogfood-validation"

# --- a first run creates the whole boundary ----------------------------------
run_case
want_rc "a first run converges" 0
want_call "creates the pool" "workload-identity-pools create github-actions"
want_call "creates the provider" "providers create-oidc github-oidc"
want_call "the provider trusts only GitHub's issuer" "--issuer-uri=https://token.actions.githubusercontent.com"
want_call "the provider accepts only this repo, by id, inside the environment" "--attribute-condition=${CONDITION}"
want_call "maps the environment claim the principal set keys on" "attribute.environment=assertion.environment"
want_call "creates the service account" "service-accounts create gag-release-validator"
want_call "only the environment's principals may impersonate" "--member=${PRINCIPAL}"
want_call "grants milestone 1's read-only role" "--role=roles/container.viewer --condition=None"
no_call "grants nothing wider by default" "roles/container.admin"
want_body "requires the named reviewer" '"reviewers": [{"type":"User","id":7}]'
want_body "restricts the refs that may deploy" '"custom_branch_policies": true'
want_call "admits main" "-f name=main -f type=branch"
want_call "admits candidate tags" "-f name=v*-rc.* -f type=tag"
want_call "publishes the provider name" "variable set GCP_WORKLOAD_IDENTITY_PROVIDER --env dogfood-validation --repo octo/repo --body projects/424242/locations/global/workloadIdentityPools/github-actions/providers/github-oidc"
want_call "publishes the service account" "--body gag-release-validator@dogfood-proj.iam.gserviceaccount.com"

# --- a re-run creates nothing ------------------------------------------------
run_case STUB_EXISTS=1 "STUB_CONDITION=${CONDITION}" $'STUB_POLICIES=branch main\ntag v*-rc.*'
want_rc "a re-run converges" 0
no_call "does not re-create the pool" "workload-identity-pools create"
no_call "does not re-create the provider" "create-oidc"
no_call "leaves a matching condition alone" "update-oidc"
no_call "does not re-create the service account" "service-accounts create"
no_call "does not re-add ref policies" "-X POST"

# --- a drifted condition is converged ----------------------------------------
run_case STUB_EXISTS=1 "STUB_CONDITION=assertion.repository_owner == 'octo'"
want_rc "a drifted provider converges" 0
want_call "rewrites a drifted condition" "update-oidc github-oidc"
want_call "rewrites it to the id-keyed one" "--attribute-condition=${CONDITION}"

# --- nothing is written before the inputs resolve ----------------------------
run_case STUB_UID=
want_rc "an unresolvable reviewer fails" 1
want_out "names the reviewer it could not resolve" "could not resolve reviewer alice"
no_call "writes no IAM before the inputs resolve" "add-iam-policy-binding"
no_call "writes no environment before the inputs resolve" "-X PUT"

run_case ASSUME_YES=
want_rc "a declined confirmation aborts" 1
no_call "writes nothing when declined" "services enable"

run_case REVIEWERS=
if [[ "$RC" != 0 ]]; then ok "requires REVIEWERS"; else bad "requires REVIEWERS: exit 0"; fi

printf '[ci-identity-setup-test] %d passed, %d failed\n' "$pass" "$fail"
((fail == 0))
