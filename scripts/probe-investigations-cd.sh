#!/usr/bin/env bash
# probe-investigations-cd.sh — runs Milestone 1 Investigations C and D.
#
# Investigation C (Session Reuse): tests whether a broker session remains valid
# for GetMessage polling immediately after AcquireJob. Runs the probe with
# PROBE_SESSION_REUSE_TEST=true, then fires a second workflow_dispatch the
# moment "job acquired" appears in the probe log so there is a job ready when
# the probe re-enters GetMessage.
#
# Investigation D (Job Delivery Throttling): tests whether GitHub delivers a
# queued job to a session registered after the job was queued (opportunistic
# delivery). Runs the probe with PROBE_JOB_DELIVERY_TEST=true; the probe
# creates a second internal session immediately after AcquireJob.
#
# After both runs, findings are extracted from the probe logs and written into
# docs/plan/milestone-1.md §8.C and §8.D automatically.
#
# Required env vars (prompted interactively if not set):
#   GITHUB_APP_ID              — numeric App ID from App settings
#   GITHUB_APP_CLIENT_ID       — client ID (Iv1.xxx) — used as JWT iss claim
#   GITHUB_APP_PRIVATE_KEY     — path to .pem file downloaded from App settings
#   GITHUB_APP_INSTALLATION_ID — installation ID (from URL after installing App)
#   GITHUB_OWNER_REPO          — "owner/repo" to register the runner against
#
# Optional:
#   GITHUB_RUNNER_VERSION      — pin a runner version (default: latest)
#   PROBE_SKIP_INVESTIGATION   — "C" or "D" to skip one (default: run both)
#   PROBE_RUNNER_CACHE         — runner binary cache dir
#                                (default: ~/.cache/github-actions-runner)
#
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RUNNER_TMP=$(mktemp -d /tmp/actions-runner-XXXXXX)
RUNNER_CACHE_DIR="${PROBE_RUNNER_CACHE:-$HOME/.cache/github-actions-runner}"
REG_TOKEN=""  # set later; used by cleanup

LOG_C=/tmp/probe-inv-c.log
LOG_D=/tmp/probe-inv-d.log
PAYLOAD_C=/tmp/probe-inv-c-payload.json
PAYLOAD_D=/tmp/probe-inv-d-payload.json

step()  { echo; echo "==> $*"; }
die()   { echo; echo "ERROR: $*" >&2; exit 1; }
info()  { echo "  $*"; }
warn()  { echo "  WARNING: $*" >&2; }

# ── Interactive input collection ──────────────────────────────────────────────

prompt_if_missing() {
    local var="$1" label="$2"
    if [[ -z "${!var:-}" ]]; then
        printf "  %s: " "$label"
        # $var holds the NAME of the target variable; reading/exporting into the
        # variable named by $var is the intended indirection here. SC2229/SC2163
        # assume the literal-name mistake, which does not apply.
        # shellcheck disable=SC2229
        read -r "$var"
        # shellcheck disable=SC2163
        export "$var"
    fi
}

step "Collecting required inputs (press Enter to skip if already exported)"
prompt_if_missing GITHUB_APP_ID              "GitHub App ID (numeric)"
prompt_if_missing GITHUB_APP_CLIENT_ID       "GitHub App Client ID (Iv1.xxx from App settings)"
prompt_if_missing GITHUB_APP_PRIVATE_KEY     "Path to App private key (.pem file)"
prompt_if_missing GITHUB_APP_INSTALLATION_ID "GitHub App Installation ID"
prompt_if_missing GITHUB_OWNER_REPO          "Target repo (owner/repo)"

# ── Prerequisites ─────────────────────────────────────────────────────────────

step "Checking prerequisites"
for cmd in go jq curl openssl python3; do
    command -v "$cmd" &>/dev/null || die "'$cmd' not found in PATH"
done
info "go, jq, curl, openssl, python3: OK"

[[ -n "${GITHUB_APP_ID:-}"              ]] || die "GITHUB_APP_ID is not set"
[[ -n "${GITHUB_APP_CLIENT_ID:-}"       ]] || die "GITHUB_APP_CLIENT_ID is not set (Iv1.xxx from App settings)"
[[ -n "${GITHUB_APP_PRIVATE_KEY:-}"     ]] || die "GITHUB_APP_PRIVATE_KEY is not set"
[[ -n "${GITHUB_APP_INSTALLATION_ID:-}" ]] || die "GITHUB_APP_INSTALLATION_ID is not set"
[[ -n "${GITHUB_OWNER_REPO:-}"          ]] || die "GITHUB_OWNER_REPO is not set"
[[ -f "$GITHUB_APP_PRIVATE_KEY"         ]] || die "PEM file not found: $GITHUB_APP_PRIVATE_KEY"

OWNER="${GITHUB_OWNER_REPO%%/*}"
REPO="${GITHUB_OWNER_REPO##*/}"
[[ "$OWNER" != "$REPO" ]] || die "GITHUB_OWNER_REPO must be 'owner/repo', got: $GITHUB_OWNER_REPO"

info "app_id=$GITHUB_APP_ID  installation_id=$GITHUB_APP_INSTALLATION_ID  repo=$GITHUB_OWNER_REPO"

# ── Helpers ───────────────────────────────────────────────────────────────────

# gh_curl DESCRIPTION METHOD URL [EXTRA_CURL_ARGS...]
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

# trigger_dispatch — fires a workflow_dispatch for probe-test.yml on main.
trigger_dispatch() {
    local resp http_code
    resp=$(curl -s -w '\n__HTTP_STATUS__%{http_code}' -X POST \
        "https://api.github.com/repos/$OWNER/$REPO/actions/workflows/probe-test.yml/dispatches" \
        -H "Authorization: Bearer $INSTALL_TOKEN" \
        -H "Accept: application/vnd.github+json" \
        -H "Content-Type: application/json" \
        -d '{"ref":"main"}')
    http_code=$(echo "$resp" | grep '__HTTP_STATUS__' | sed 's/.*__HTTP_STATUS__//')
    if [[ "$http_code" =~ ^2 ]]; then
        info "workflow_dispatch succeeded (HTTP $http_code)"
        return 0
    fi
    warn "workflow_dispatch returned HTTP $http_code"
    if command -v gh &>/dev/null; then
        info "falling back to gh CLI dispatch"
        if gh workflow run probe-test.yml --repo "$GITHUB_OWNER_REPO" --ref main; then
            info "dispatched via gh CLI"
        else
            warn "gh CLI dispatch also failed — trigger manually via Actions tab"
        fi
    else
        warn "App may lack 'actions: write' permission."
        warn "Trigger the 'probe-test' workflow manually in the Actions tab NOW."
    fi
}

# wait_for_log LOG_FILE GREP_PATTERN TIMEOUT_SECS
# Returns 0 when the pattern appears, 1 on timeout.
wait_for_log() {
    local log="$1" pattern="$2" timeout="$3"
    local elapsed=0
    while [[ $elapsed -lt $timeout ]]; do
        if grep -qE "$pattern" "$log" 2>/dev/null; then
            return 0
        fi
        sleep 1
        elapsed=$((elapsed + 1))
    done
    return 1
}

cleanup() {
    # Kill any background probe or tail processes.
    kill "${PROBE_PID:-}" "${TAIL_PID:-}" 2>/dev/null || true
    wait "${PROBE_PID:-}" "${TAIL_PID:-}" 2>/dev/null || true

    if [[ -d "${RUNNER_TMP:-}" ]]; then
        if [[ -f "$RUNNER_TMP/.runner" && -n "$REG_TOKEN" ]]; then
            echo
            echo "==> Deregistering temporary runner"
            # Best-effort deregistration: ignore any failure (cd or remove).
            (cd "$RUNNER_TMP" && ./config.sh remove --token "$REG_TOKEN" 2>/dev/null) || true
        fi
        rm -rf "$RUNNER_TMP"
    fi
}
trap cleanup EXIT

# ── JWT ───────────────────────────────────────────────────────────────────────

step "Minting GitHub App JWT (RS256 via openssl)"

b64url() { base64 | tr -d '=' | tr '+/' '-_' | tr -d '\n'; }

NOW=$(date +%s)
HEADER=$(printf '{"alg":"RS256","typ":"JWT"}' | b64url)
CLAIMS=$(printf '{"iat":%d,"exp":%d,"iss":"%s"}' $((NOW - 60)) $((NOW + 600)) "$GITHUB_APP_CLIENT_ID" | b64url)
SIG=$(printf '%s.%s' "$HEADER" "$CLAIMS" \
    | openssl dgst -sha256 -sign "$GITHUB_APP_PRIVATE_KEY" -binary 2>/dev/null \
    | b64url) \
    || die "openssl signing failed — check that GITHUB_APP_PRIVATE_KEY is a valid RSA PEM file"
JWT="$HEADER.$CLAIMS.$SIG"
info "JWT length: ${#JWT} chars"

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
info "token obtained (expires $EXPIRES_AT)"

# ── Runner registration token ─────────────────────────────────────────────────

step "Getting runner registration token"
REG_TOKEN_RESP=$(gh_curl \
    "get runner registration token" POST \
    "https://api.github.com/repos/$OWNER/$REPO/actions/runners/registration-token" \
    -H "Authorization: Bearer $INSTALL_TOKEN" \
    -H "Accept: application/vnd.github+json" \
    -H "X-GitHub-Api-Version: 2022-11-28")
REG_TOKEN=$(echo "$REG_TOKEN_RESP" | jq -r .token)
info "registration token obtained (expires $(echo "$REG_TOKEN_RESP" | jq -r .expires_at))"

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
    Darwin) OS="osx"   ;;
    *) die "Unsupported OS: $RAW_OS" ;;
esac
case "$RAW_ARCH" in
    x86_64)        ARCH="x64"   ;;
    aarch64|arm64) ARCH="arm64" ;;
    *) die "Unsupported architecture: $RAW_ARCH" ;;
esac
info "version=$RUNNER_VERSION  os=$OS  arch=$ARCH"

RUNNER_ARCHIVE="actions-runner-${OS}-${ARCH}-${RUNNER_VERSION}.tar.gz"
RUNNER_URL="https://github.com/actions/runner/releases/download/v${RUNNER_VERSION}/${RUNNER_ARCHIVE}"

step "Fetching actions/runner $RUNNER_VERSION"
mkdir -p "$RUNNER_CACHE_DIR"
CACHED_ARCHIVE="$RUNNER_CACHE_DIR/$RUNNER_ARCHIVE"
if [[ -f "$CACHED_ARCHIVE" ]]; then
    info "cache hit: $CACHED_ARCHIVE"
else
    info "downloading $RUNNER_URL"
    curl -fL --progress-bar "$RUNNER_URL" -o "$CACHED_ARCHIVE" \
        || { rm -f "$CACHED_ARCHIVE"; die "Failed to download runner from $RUNNER_URL"; }
    info "cached to $CACHED_ARCHIVE"
fi
tar -C "$RUNNER_TMP" -xzf "$CACHED_ARCHIVE"
info "extracted to $RUNNER_TMP"

# ── Configure runner ──────────────────────────────────────────────────────────

step "Configuring temporary runner"
RUNNER_NAME="probe-inv-$(date +%s)"
(cd "$RUNNER_TMP" && ./config.sh \
    --unattended \
    --url    "https://github.com/$GITHUB_OWNER_REPO" \
    --token  "$REG_TOKEN" \
    --name   "$RUNNER_NAME" \
    --labels "probe,self-hosted" \
    --no-default-labels)

RUNNER_CONFIG="$RUNNER_TMP/.runner"
[[ -f "$RUNNER_CONFIG" ]] || die "config.sh did not create .runner"
info ".runner contents:"; jq . "$RUNNER_CONFIG"

BROKER_URL=$(jq -r '.serverUrl // .brokerUrl // empty' "$RUNNER_CONFIG")
[[ -n "$BROKER_URL" ]] || die "Could not find serverUrl/brokerUrl in .runner"
BROKER_URL_V2=$(jq -r '.serverUrlV2 // empty' "$RUNNER_CONFIG")
POOL_ID=$(jq -r '.poolId // 1' "$RUNNER_CONFIG")
AGENT_ID=$(jq -r '.agentId' "$RUNNER_CONFIG")
AGENT_NAME=$(jq -r '.agentName' "$RUNNER_CONFIG")
USE_V2_FLOW=$(jq -r '.useV2Flow // false' "$RUNNER_CONFIG")

[[ -n "$AGENT_ID"   && "$AGENT_ID"   != "null" ]] || die "Could not find agentId in .runner"
[[ -n "$AGENT_NAME" && "$AGENT_NAME" != "null" ]] || die "Could not find agentName in .runner"
info "agentId=$AGENT_ID  agentName=$AGENT_NAME  useV2Flow=$USE_V2_FLOW  poolId=$POOL_ID"

RUNNER_CREDENTIALS="$RUNNER_TMP/.credentials"
RUNNER_RSA_PARAMS="$RUNNER_TMP/.credentials_rsaparams"
[[ -f "$RUNNER_CREDENTIALS" ]] || die ".credentials not found after config.sh"
[[ -f "$RUNNER_RSA_PARAMS"  ]] || die ".credentials_rsaparams not found after config.sh"

# ── Verify probe-test workflow exists ─────────────────────────────────────────

step "Verifying probe-test workflow"
WORKFLOW_PATH=".github/workflows/probe-test.yml"
[[ -f "$REPO_ROOT/$WORKFLOW_PATH" ]] \
    || die "$WORKFLOW_PATH not found — commit and push it before running this script"
info "found at $REPO_ROOT/$WORKFLOW_PATH"

# ── Build probe ───────────────────────────────────────────────────────────────

step "Building cmd/probe"
cd "$REPO_ROOT"
go build -o /tmp/probe ./cmd/probe/
info "binary: /tmp/probe"

# ── Export probe environment (shared across both investigations) ───────────────

export GITHUB_APP_ID
export GITHUB_APP_PRIVATE_KEY
export GITHUB_APP_INSTALLATION_ID
export GITHUB_BROKER_URL="$BROKER_URL"
export GITHUB_BROKER_URL_V2="${BROKER_URL_V2:-}"
export GITHUB_RUNNER_VERSION="$RUNNER_VERSION"
export GITHUB_RUNNER_OS="$OS"
export GITHUB_RUNNER_ARCH="$ARCH"
export GITHUB_POOL_ID="$POOL_ID"
export GITHUB_AGENT_ID="$AGENT_ID"
export GITHUB_AGENT_NAME="$AGENT_NAME"
export GITHUB_USE_V2_FLOW="$USE_V2_FLOW"
export GITHUB_RUNNER_CREDENTIALS_FILE="$RUNNER_CREDENTIALS"
export GITHUB_RUNNER_RSA_PARAMS_FILE="$RUNNER_RSA_PARAMS"

# ── Investigation C ───────────────────────────────────────────────────────────

if [[ "${PROBE_SKIP_INVESTIGATION:-}" != "C" ]]; then
    step "Investigation C: Session Reuse After acquirejob"
    info "Triggering first workflow dispatch (job for primary acquisition)..."
    trigger_dispatch

    info "Starting probe with PROBE_SESSION_REUSE_TEST=true"
    info "Logs → $LOG_C   Payload → $PAYLOAD_C"
    : > "$LOG_C"
    PROBE_SESSION_REUSE_TEST=true /tmp/probe >"$PAYLOAD_C" 2>"$LOG_C" &
    PROBE_PID=$!
    tail -f "$LOG_C" &
    TAIL_PID=$!

    info "Waiting for primary job acquisition (up to 5 min)..."
    if wait_for_log "$LOG_C" 'msg="job acquired"' 300; then
        info "'job acquired' detected — triggering second dispatch immediately"
        # Brief pause so the probe re-enters GetMessage before the dispatch lands.
        sleep 2
        trigger_dispatch || true
    else
        warn "'job acquired' not seen within 5 min — second dispatch skipped"
        warn "The investigation result will be inconclusive; re-run after queuing a job manually."
    fi

    info "Waiting for Investigation C result (up to 3 min)..."
    if wait_for_log "$LOG_C" 'INVESTIGATION-C:.*CONFIRMED|INVESTIGATION-C:.*error|INVESTIGATION-C:.*timeout|INVESTIGATION-C:.*live throughout' 180; then
        info "Investigation C result recorded in $LOG_C"
    else
        warn "No INVESTIGATION-C result line found within 3 min"
    fi

    # Graceful shutdown: SIGINT triggers DeleteSession defer in the probe.
    kill -INT "$PROBE_PID" 2>/dev/null || true
    sleep 5
    kill "$TAIL_PID" 2>/dev/null || true
    wait "$PROBE_PID" 2>/dev/null || true
    wait "$TAIL_PID" 2>/dev/null || true
    unset PROBE_PID TAIL_PID

    info "Investigation C complete. Full log: $LOG_C"
fi

# ── Investigation D ───────────────────────────────────────────────────────────

if [[ "${PROBE_SKIP_INVESTIGATION:-}" != "D" ]]; then
    step "Investigation D: Job Delivery Throttling by Session Count"
    info "Triggering first workflow dispatch (job for primary acquisition)..."
    trigger_dispatch

    info "Starting probe with PROBE_JOB_DELIVERY_TEST=true"
    info "Logs → $LOG_D   Payload → $PAYLOAD_D"
    : > "$LOG_D"
    PROBE_JOB_DELIVERY_TEST=true /tmp/probe >"$PAYLOAD_D" 2>"$LOG_D" &
    PROBE_PID=$!
    tail -f "$LOG_D" &
    TAIL_PID=$!

    info "Waiting for primary job acquisition (up to 5 min)..."
    if wait_for_log "$LOG_D" 'msg="job acquired"' 300; then
        info "'job acquired' detected — triggering second dispatch for second-session test"
        sleep 2
        trigger_dispatch || true
    else
        warn "'job acquired' not seen within 5 min — second dispatch skipped"
    fi

    info "Waiting for Investigation D result (up to 3 min)..."
    if wait_for_log "$LOG_D" 'INVESTIGATION-D:.*CONFIRMED|INVESTIGATION-D:.*error|INVESTIGATION-D:.*timeout' 180; then
        info "Investigation D result recorded in $LOG_D"
    else
        warn "No INVESTIGATION-D result line found within 3 min"
    fi

    kill -INT "$PROBE_PID" 2>/dev/null || true
    sleep 5
    kill "$TAIL_PID" 2>/dev/null || true
    wait "$PROBE_PID" 2>/dev/null || true
    wait "$TAIL_PID" 2>/dev/null || true
    unset PROBE_PID TAIL_PID

    info "Investigation D complete. Full log: $LOG_D"
fi

# ── Extract findings and update milestone-1.md ────────────────────────────────

step "Extracting findings and updating docs/plan/milestone-1.md"

MILESTONE="$REPO_ROOT/docs/plan/milestone-1.md"

# Extract the most informative INVESTIGATION-C log line for the finding.
extract_finding() {
    local log="$1" prefix="$2"
    # Prefer lines with CONFIRMED, error, or timeout; fall back to any result line.
    grep -oE "${prefix}[^\"]*\"[^\"]*\"[^\"]*" "$log" 2>/dev/null \
        | grep -E 'CONFIRMED|error|timeout|inconclusive' \
        | tail -1 \
        | sed 's/.*msg="\([^"]*\)".*/\1/' \
        || grep "${prefix}" "$log" 2>/dev/null | tail -1 \
        || echo "No result recorded — check $log"
}

if [[ "${PROBE_SKIP_INVESTIGATION:-}" != "C" ]]; then
    if grep -qE 'SESSION REUSE CONFIRMED' "$LOG_C" 2>/dev/null; then
        FINDING_C="Session reuse confirmed. A second \`RunnerJobRequest\` was received on the same \`sessionId\` immediately after \`AcquireJob\`. The Session Multiplexer design proceeds as written — goroutines loop without a delete+create cycle between jobs."
    elif grep -qE 'session was live throughout' "$LOG_C" 2>/dev/null; then
        FINDING_C="Session reuse supported. The session continued returning 202 (no-job) responses on the same \`sessionId\` after \`AcquireJob\` with no protocol error — the session was not invalidated. The polling window expired before a second job arrived; the second dispatch was either queued too late or cancelled. The Session Multiplexer design proceeds as written."
    elif grep -qE "session invalidated after AcquireJob" "$LOG_C" 2>/dev/null; then
        ERR=$(grep -oE 'error=[^ ]+' "$LOG_C" | head -1 || echo "")
        FINDING_C="Session reuse NOT supported. \`GetMessage\` returned a protocol error after \`AcquireJob\` ($ERR). The AGC goroutine must call \`DeleteSession\` + \`CreateSession\` between each job. Note the added latency in the Milestone 2 plan."
    elif grep -qE "inconclusive" "$LOG_C" 2>/dev/null; then
        FINDING_C="Inconclusive — no second job arrived within the 3-minute polling window. Re-run with a second job pre-queued before the probe re-enters \`GetMessage\`."
    else
        FINDING_C="Investigation C did not produce a conclusive result. Review \`$LOG_C\` manually and record the finding here."
    fi
    info "Finding C: $FINDING_C"
else
    FINDING_C="Skipped (PROBE_SKIP_INVESTIGATION=C)."
fi

if [[ "${PROBE_SKIP_INVESTIGATION:-}" != "D" ]]; then
    if grep -qE 'OPPORTUNISTIC DELIVERY CONFIRMED' "$LOG_D" 2>/dev/null; then
        FINDING_D="Opportunistic delivery confirmed. The second session — registered *after* the first job was acquired — received the second queued job. GitHub delivers to any ready session regardless of when it was registered. The adaptive listener model is safe; no standby pool is needed."
    elif grep -qE "throttling" "$LOG_D" 2>/dev/null; then
        FINDING_D="Possible throttling observed. The second session did not receive the second job within 3 minutes. GitHub may bind delivery to the set of sessions present at queue time. Evaluate pre-spawning 2–3 warm standby sessions per RunnerGroup and update Appendix E before Milestone 2."
    elif grep -qE "CreateSession.*failed" "$LOG_D" 2>/dev/null; then
        FINDING_D="Investigation D aborted: second-session CreateSession failed. Check \`$LOG_D\` for the error; the runner may only allow one active session at a time."
    else
        FINDING_D="Investigation D did not produce a conclusive result. Review \`$LOG_D\` manually and record the finding here."
    fi
    info "Finding D: $FINDING_D"
else
    FINDING_D="Skipped (PROBE_SKIP_INVESTIGATION=D)."
fi

# Write findings into the two **Finding:** placeholders in §8.C and §8.D using
# Python to avoid sed portability issues with multi-line replacements.
python3 - "$MILESTONE" "$FINDING_C" "$FINDING_D" <<'PYEOF'
import sys

path, finding_c, finding_d = sys.argv[1], sys.argv[2], sys.argv[3]
placeholder = '**Finding:** *Record here after running the probe.*'

with open(path, 'r') as f:
    content = f.read()

if placeholder not in content:
    print(f"  WARNING: placeholder not found in {path} — findings not written")
    sys.exit(0)

# Replace first occurrence (§8.C).
content = content.replace(placeholder, f'**Finding:** {finding_c}', 1)
# Replace second occurrence (§8.D).
content = content.replace(placeholder, f'**Finding:** {finding_d}', 1)

with open(path, 'w') as f:
    f.write(content)

print(f"  Updated {path}")
PYEOF

# ── Summary ───────────────────────────────────────────────────────────────────

step "Done"
echo
echo "  Investigation C log : $LOG_C"
echo "  Investigation D log : $LOG_D"
echo "  milestone-1.md      : $MILESTONE"
echo
echo "Next steps:"
echo "  1. Review docs/plan/milestone-1.md §8.C and §8.D — adjust findings if needed"
echo "  2. git add docs/plan/milestone-1.md && git commit"
echo "  3. Mark the two checklist items done in §5 and close Milestone 1"
