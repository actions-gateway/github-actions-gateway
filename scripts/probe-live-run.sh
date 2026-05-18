#!/usr/bin/env bash
# probe-live-run.sh — end-to-end setup and execution of the Milestone 1 wire
# protocol probe against a real GitHub App installation.
#
# What this script does:
#   1. Validates prerequisites (go, jq, curl, openssl)
#   2. Mints a GitHub App JWT and exchanges it for an installation access token
#   3. Gets a runner registration token and downloads the official runner binary
#   4. Runs config.sh (unattended) to register a temporary virtual runner and
#      extract the broker URL written to the runner's .runner config file
#   5. Creates a minimal workflow in the target repo (if not already present)
#   6. Triggers a workflow_dispatch to queue a job
#   7. Builds and runs cmd/probe — payload (stdout) captured, logs to terminal
#   8. Redacts sensitive tokens and writes testdata/job_payload.json
#   9. Deregisters the temporary runner from GitHub
#
# Required environment variables:
#   GITHUB_APP_ID              — numeric App ID from the App settings page
#   GITHUB_APP_PRIVATE_KEY     — path to the .pem file downloaded from the App
#   GITHUB_APP_INSTALLATION_ID — installation ID (from URL after installing App)
#   GITHUB_OWNER_REPO          — "owner/repo" to register the runner against
#
# Optional:
#   GITHUB_RUNNER_VERSION      — pin a runner version (default: latest)
#   PROBE_RAW_OUTPUT           — path for raw payload (default: /tmp/probe-raw-payload.json)
#
set -euo pipefail

# ── Helpers ───────────────────────────────────────────────────────────────────

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RUNNER_TMP=$(mktemp -d /tmp/actions-runner-XXXXXX)
RAW_PAYLOAD="${PROBE_RAW_OUTPUT:-/tmp/probe-raw-payload.json}"
REG_TOKEN=""  # set later; used by cleanup

step() { echo; echo "==> $*"; }
die()  { echo; echo "ERROR: $*" >&2; exit 1; }

# gh_curl DESCRIPTION METHOD URL [EXTRA_CURL_ARGS...]
# Makes a GitHub API call. Prints the response body and exits with a clear
# error message if the HTTP status is not 2xx. Does NOT use curl -f so that
# error responses are always captured and shown.
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

cleanup() {
    if [[ -d "${RUNNER_TMP:-}" ]]; then
        if [[ -f "$RUNNER_TMP/.runner" && -n "$REG_TOKEN" ]]; then
            echo
            echo "==> Deregistering temporary runner"
            (cd "$RUNNER_TMP" && ./config.sh remove --token "$REG_TOKEN" 2>/dev/null || true)
        fi
        rm -rf "$RUNNER_TMP"
    fi
}
trap cleanup EXIT

# ── Prerequisites ─────────────────────────────────────────────────────────────

step "Checking prerequisites"
for cmd in go jq curl openssl; do
    command -v "$cmd" &>/dev/null || die "'$cmd' not found in PATH"
done
echo "  go, jq, curl, openssl: OK"

[[ -n "${GITHUB_APP_ID:-}"              ]] || die "GITHUB_APP_ID is not set"
[[ -n "${GITHUB_APP_PRIVATE_KEY:-}"     ]] || die "GITHUB_APP_PRIVATE_KEY is not set"
[[ -n "${GITHUB_APP_INSTALLATION_ID:-}" ]] || die "GITHUB_APP_INSTALLATION_ID is not set"
[[ -n "${GITHUB_OWNER_REPO:-}"          ]] || die "GITHUB_OWNER_REPO is not set"
[[ -f "$GITHUB_APP_PRIVATE_KEY"         ]] || die "PEM file not found: $GITHUB_APP_PRIVATE_KEY"

OWNER="${GITHUB_OWNER_REPO%%/*}"
REPO="${GITHUB_OWNER_REPO##*/}"
[[ "$OWNER" != "$REPO" ]] || die "GITHUB_OWNER_REPO must be 'owner/repo', got: $GITHUB_OWNER_REPO"

echo "  app_id=$GITHUB_APP_ID  installation_id=$GITHUB_APP_INSTALLATION_ID  repo=$GITHUB_OWNER_REPO"

# ── JWT ───────────────────────────────────────────────────────────────────────

step "Minting GitHub App JWT (RS256 via openssl)"

# RFC 7515 base64url: standard base64, strip padding, swap + and / chars.
b64url() { base64 | tr -d '=' | tr '+/' '-_' | tr -d '\n'; }

NOW=$(date +%s)
HEADER=$(printf '{"alg":"RS256","typ":"JWT"}' | b64url)
CLAIMS=$(printf '{"iat":%d,"exp":%d,"iss":"%s"}' $((NOW - 60)) $((NOW + 600)) "$GITHUB_APP_ID" | b64url)

# openssl dgst -sha256 -sign produces a PKCS#1 v1.5 RS256 signature.
SIG=$(printf '%s.%s' "$HEADER" "$CLAIMS" \
    | openssl dgst -sha256 -sign "$GITHUB_APP_PRIVATE_KEY" -binary 2>/dev/null \
    | b64url) \
    || die "openssl signing failed — check that GITHUB_APP_PRIVATE_KEY is a valid RSA PEM file"

JWT="$HEADER.$CLAIMS.$SIG"
echo "  JWT header : $(echo "$HEADER" | base64 -d 2>/dev/null || true)"
echo "  JWT claims : $(echo "$CLAIMS" | base64 -d 2>/dev/null || true)"
echo "  JWT length : ${#JWT} chars"

# ── Installation token ────────────────────────────────────────────────────────

step "Exchanging JWT for installation access token"
INSTALL_TOKEN_RESP=$(gh_curl \
    "exchange JWT for installation token" POST \
    "https://api.github.com/app/installations/${GITHUB_APP_INSTALLATION_ID}/access_tokens" \
    -H "Authorization: Bearer $JWT" \
    -H "Accept: application/vnd.github+json" \
    -H "X-GitHub-Api-Version: 2022-11-28")

INSTALL_TOKEN=$(echo "$INSTALL_TOKEN_RESP" | jq -r .token)
EXPIRES_AT=$(echo "$INSTALL_TOKEN_RESP" | jq -r .expires_at)
echo "  token obtained (expires $EXPIRES_AT)"

# ── Runner registration token ─────────────────────────────────────────────────

step "Getting runner registration token"
REG_TOKEN_RESP=$(gh_curl \
    "get runner registration token" POST \
    "https://api.github.com/repos/$OWNER/$REPO/actions/runners/registration-token" \
    -H "Authorization: Bearer $INSTALL_TOKEN" \
    -H "Accept: application/vnd.github+json" \
    -H "X-GitHub-Api-Version: 2022-11-28")

REG_TOKEN=$(echo "$REG_TOKEN_RESP" | jq -r .token)
echo "  registration token obtained (expires $(echo "$REG_TOKEN_RESP" | jq -r .expires_at))"

# ── Download official runner ──────────────────────────────────────────────────

step "Detecting runner platform and version"
if [[ -n "${GITHUB_RUNNER_VERSION:-}" ]]; then
    RUNNER_VERSION="$GITHUB_RUNNER_VERSION"
else
    RUNNER_VERSION=$(curl -s \
        -H "Accept: application/vnd.github+json" \
        "https://api.github.com/repos/actions/runner/releases/latest" \
        | jq -r '.tag_name | ltrimstr("v")')
    [[ -n "$RUNNER_VERSION" ]] || die "Could not determine latest runner version"
fi

RAW_OS=$(uname -s)
RAW_ARCH=$(uname -m)
case "$RAW_OS" in
    Linux)  OS="linux" ;;
    Darwin) OS="osx"   ;;  # runner releases use "osx", not "darwin"
    *) die "Unsupported OS: $RAW_OS" ;;
esac
case "$RAW_ARCH" in
    x86_64)        ARCH="x64"   ;;
    aarch64|arm64) ARCH="arm64" ;;
    *) die "Unsupported architecture: $RAW_ARCH" ;;
esac
echo "  version=$RUNNER_VERSION  os=$OS  arch=$ARCH"

RUNNER_ARCHIVE="actions-runner-${OS}-${ARCH}-${RUNNER_VERSION}.tar.gz"
RUNNER_URL="https://github.com/actions/runner/releases/download/v${RUNNER_VERSION}/${RUNNER_ARCHIVE}"

step "Downloading actions/runner $RUNNER_VERSION"
curl -fL --progress-bar "$RUNNER_URL" -o "$RUNNER_TMP/$RUNNER_ARCHIVE" \
    || die "Failed to download runner from $RUNNER_URL"
tar -C "$RUNNER_TMP" -xzf "$RUNNER_TMP/$RUNNER_ARCHIVE"
echo "  extracted to $RUNNER_TMP"

# ── Configure runner (broker URL discovery) ───────────────────────────────────

step "Configuring temporary runner to discover broker URL"
RUNNER_NAME="probe-$(date +%s)"
(cd "$RUNNER_TMP" && ./config.sh \
    --unattended \
    --url    "https://github.com/$GITHUB_OWNER_REPO" \
    --token  "$REG_TOKEN" \
    --name   "$RUNNER_NAME" \
    --labels "probe,self-hosted" \
    --no-default-labels)

RUNNER_CONFIG="$RUNNER_TMP/.runner"
[[ -f "$RUNNER_CONFIG" ]] || die "config.sh did not create .runner — check output above"

echo "  .runner contents:"
jq . "$RUNNER_CONFIG"

BROKER_URL=$(jq -r '.serverUrl // .brokerUrl // empty' "$RUNNER_CONFIG")
[[ -n "$BROKER_URL" ]] || die "Could not find serverUrl/brokerUrl in .runner — see contents above"
echo "  broker URL: $BROKER_URL"

# ── Ensure probe-test workflow exists ─────────────────────────────────────────

step "Ensuring probe-test workflow exists in $GITHUB_OWNER_REPO"
WORKFLOW_PATH=".github/workflows/probe-test.yml"

# Check locally — the file must be committed and pushed so GitHub can
# index it for workflow_dispatch. No API call needed; avoids contents:read
# permission requirement on the installation token.
if [[ ! -f "$REPO_ROOT/$WORKFLOW_PATH" ]]; then
    echo >&2
    echo "  ERROR: $WORKFLOW_PATH not found locally." >&2
    echo "  The file must be committed and pushed before running this script." >&2
    echo "  It should already exist at .github/workflows/probe-test.yml in the repo." >&2
    die "workflow file missing — check that you are running from the repo root and have pulled latest"
else
    echo "  found at $REPO_ROOT/$WORKFLOW_PATH"
fi

# ── Build probe ───────────────────────────────────────────────────────────────

step "Building cmd/probe"
cd "$REPO_ROOT"
go build -o /tmp/probe ./cmd/probe/
echo "  binary: /tmp/probe"

# ── Trigger workflow ──────────────────────────────────────────────────────────

step "Triggering workflow_dispatch"
# Try the API first. If it returns 403 (App lacks actions:write), fall back to
# the gh CLI (uses the user's personal token). If neither works, print manual
# instructions and continue — the probe will poll until a job appears.
DISPATCH_RESP=$(curl -s -w '\n__HTTP_STATUS__%{http_code}' -X POST \
    "https://api.github.com/repos/$OWNER/$REPO/actions/workflows/probe-test.yml/dispatches" \
    -H "Authorization: Bearer $INSTALL_TOKEN" \
    -H "Accept: application/vnd.github+json" \
    -H "Content-Type: application/json" \
    -d '{"ref":"main"}')
DISPATCH_STATUS=$(echo "$DISPATCH_RESP" | grep '__HTTP_STATUS__' | sed 's/.*__HTTP_STATUS__//')

if [[ "$DISPATCH_STATUS" =~ ^2 ]]; then
    echo "  dispatched via API — job will appear in the queue within a few seconds"
elif command -v gh &>/dev/null; then
    echo "  API dispatch returned HTTP $DISPATCH_STATUS — trying gh CLI"
    gh workflow run probe-test.yml --repo "$GITHUB_OWNER_REPO" --ref main \
        && echo "  dispatched via gh CLI" \
        || echo "  WARNING: gh CLI dispatch also failed — trigger manually (see below)"
else
    echo
    echo "  WARNING: workflow_dispatch failed (HTTP $DISPATCH_STATUS)" >&2
    echo "  The GitHub App installation likely lacks 'actions: write' permission." >&2
    echo >&2
    echo "  Fix A: GitHub.com → Settings → Developer settings → GitHub Apps → your app" >&2
    echo "         → Permissions & events → Actions → Read and write → Save → re-install." >&2
    echo >&2
    echo "  Fix B (manual): open the Actions tab in the repo, run 'probe-test' workflow" >&2
    echo "         manually, then the probe below will pick up the queued job." >&2
    echo
fi

# ── Run probe ─────────────────────────────────────────────────────────────────

step "Running probe  (Ctrl-C to stop after ≥3 renewals)"
echo "  logs  → stderr (terminal)"
echo "  payload → $RAW_PAYLOAD"
echo
echo "  NOTE: watch the CreateSession log line for a session key field."
echo "  If the decrypted body looks garbled, set GITHUB_SESSION_KEY=<value>"
echo "  and re-run with the probe binary directly."
echo

export GITHUB_APP_ID
export GITHUB_APP_PRIVATE_KEY
export GITHUB_APP_INSTALLATION_ID
export GITHUB_BROKER_URL="$BROKER_URL"
export GITHUB_RUNNER_VERSION="$RUNNER_VERSION"

/tmp/probe 2>&1 1>"$RAW_PAYLOAD" | tee /tmp/probe.log || true

# ── Redact and save fixture ───────────────────────────────────────────────────

step "Saving redacted fixture"
FIXTURE="$REPO_ROOT/testdata/job_payload.json"

if [[ ! -s "$RAW_PAYLOAD" ]]; then
    echo "  WARNING: payload file is empty — probe may not have acquired a job."
    echo "  Probe logs: /tmp/probe.log"
    exit 0
fi

# Redact strings that look like bearer tokens or base64 secrets (≥40 chars).
jq 'walk(
    if type == "string" and (
        test("^v[0-9]") or
        test("^ghs_") or
        test("^ghp_") or
        (length > 40 and test("^[A-Za-z0-9+/=_-]+$"))
    )
    then "REDACTED"
    else .
    end
)' "$RAW_PAYLOAD" > "$FIXTURE" 2>/dev/null \
    || { echo "  jq redaction failed, copying raw"; cp "$RAW_PAYLOAD" "$FIXTURE"; }

echo "  written: $FIXTURE"
echo
echo "Next steps:"
echo "  1. Review $FIXTURE and redact any remaining secrets"
echo "  2. git add testdata/job_payload.json && git commit"
echo "  3. Document Investigation A + B findings in docs/plan/milestone-1.md §8"
