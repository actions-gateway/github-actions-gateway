#!/usr/bin/env bash
# mint-installation-token.sh — mint a short-lived, repo-scoped GitHub App
# installation access token for the agent sandbox container.
#
# Runs on the HOST. Reads the App private key from the macOS Keychain (never
# touching disk), signs an App JWT, and exchanges it for an installation token
# scoped to a single repository with the minimum permissions an autonomous
# coding agent needs. Prints ONLY the token to stdout so it can be captured:
#
#   AGENT_GH_TOKEN="$(scripts/mint-installation-token.sh)"
#
# The App private key stays on the host; only this ≤1h token crosses into the
# container. See .devcontainer/README.md "Scoped credentials".
#
# Configuration (env vars; defaults target the actions-gateway-test App):
#   GITHUB_APP_CLIENT_ID   — App client ID (Iv23…/Iv1.…), used as the JWT `iss`.
#                            REQUIRED: GitHub deprecated the numeric App ID as
#                            `iss` in late 2024. Find it on the App settings page.
#   GITHUB_OWNER_REPO      — "owner/repo" to scope the token to.
#                            Default: derived from the `origin` git remote.
#   KEYCHAIN_ACCOUNT       — Keychain account holding the hex-encoded PEM.
#                            Default: actions-gateway-test
#   KEYCHAIN_SERVICE       — Keychain service name. Default: github-app-private-key
#   GITHUB_APP_PRIVATE_KEY — path to a PEM file; overrides Keychain (for CI/Linux).
#   TOKEN_PERMISSIONS      — JSON object of installation permissions.
#                            Default: {"contents":"write","pull_requests":"write"}
#                            Add "workflows":"write" only if the agent must edit
#                            files under .github/workflows.
#
# Requires: jq, curl, openssl (+ `security` & `xxd` when reading from Keychain).

set -euo pipefail

readonly GITHUB_API="https://api.github.com"
readonly DEFAULT_PERMISSIONS='{"contents":"write","pull_requests":"write"}'

log()  { printf '[mint-token] %s\n' "$*" >&2; }
die()  { printf '[mint-token] ERROR: %s\n' "$*" >&2; exit 1; }

# Emit the App private key (PEM) to stdout without writing it to disk.
read_pem() {
  local key_path="${GITHUB_APP_PRIVATE_KEY:-}"
  if [[ -n "$key_path" ]]; then
    [[ -f "$key_path" ]] || die "GITHUB_APP_PRIVATE_KEY set but file not found: $key_path"
    cat "$key_path"
    return
  fi
  command -v security >/dev/null 2>&1 || die "macOS 'security' not found; set GITHUB_APP_PRIVATE_KEY to a PEM path instead"
  command -v xxd >/dev/null 2>&1 || die "'xxd' not found (needed to decode the hex-encoded Keychain entry)"
  security find-generic-password -a "$KEYCHAIN_ACCOUNT" -s "$KEYCHAIN_SERVICE" -w 2>/dev/null \
    | xxd -r -p \
    || die "could not read App key from Keychain (account=$KEYCHAIN_ACCOUNT service=$KEYCHAIN_SERVICE)"
}

# RFC 7515 base64url: base64, strip padding, swap +/ for -_, drop newlines.
b64url() { base64 | tr -d '=' | tr '+/' '-_' | tr -d '\n'; }

# Sign "$1" (the JWT signing input) with the App key, emit base64url signature.
sign_rs256() {
  local input="$1"
  # Process substitution keeps the PEM off disk: openssl reads it via /dev/fd.
  printf '%s' "$input" \
    | openssl dgst -sha256 -sign <(read_pem) -binary 2>/dev/null \
    | b64url \
    || die "openssl signing failed — is the Keychain entry a valid RSA PEM?"
}

# curl wrapper: capture body + HTTP code, fail loudly on non-2xx. Body -> stdout.
api() {
  local description="$1" method="$2" url="$3"; shift 3
  local response http_code body
  response=$(curl -sS -w $'\n%{http_code}' --max-time 30 -X "$method" "$url" \
    -H "Accept: application/vnd.github+json" \
    -H "X-GitHub-Api-Version: 2022-11-28" \
    "$@") || die "$description: curl failed"
  http_code="${response##*$'\n'}"
  body="${response%$'\n'*}"
  if [[ "$http_code" != 2* ]]; then
    die "$description failed (HTTP $http_code): $body"
  fi
  printf '%s' "$body"
}

main() {
  local tool
  for tool in jq curl openssl; do
    command -v "$tool" >/dev/null 2>&1 || die "missing required tool: $tool"
  done

  : "${KEYCHAIN_ACCOUNT:=actions-gateway-test}"
  : "${KEYCHAIN_SERVICE:=github-app-private-key}"
  local permissions="${TOKEN_PERMISSIONS:-$DEFAULT_PERMISSIONS}"
  jq -e . >/dev/null 2>&1 <<<"$permissions" || die "TOKEN_PERMISSIONS is not valid JSON: $permissions"

  local client_id="${GITHUB_APP_CLIENT_ID:-}"
  [[ -n "$client_id" ]] || die "GITHUB_APP_CLIENT_ID is required (App client ID, Iv23…/Iv1.…; numeric App ID no longer works as JWT iss)"

  # Resolve owner/repo from env or the origin remote.
  local owner_repo="${GITHUB_OWNER_REPO:-}"
  if [[ -z "$owner_repo" ]]; then
    local origin
    origin="$(git config --get remote.origin.url 2>/dev/null)" \
      || die "GITHUB_OWNER_REPO unset and no origin remote to derive it from"
    # Strip protocol/host and the trailing .git: git@github.com:o/r.git | https://github.com/o/r.git
    owner_repo="$(printf '%s' "$origin" | awk -F'[:/]' '{print $(NF-1)"/"$NF}')"
    owner_repo="${owner_repo%.git}"
  fi
  [[ "$owner_repo" == */* ]] || die "could not resolve owner/repo (got: '$owner_repo')"
  local owner="${owner_repo%%/*}" repo="${owner_repo##*/}"
  log "scoping token to ${owner}/${repo} with permissions ${permissions}"

  # --- Mint the App JWT (10-minute lifetime, GitHub's max). ---
  local now header claims jwt
  now="$(date +%s)"
  header="$(printf '{"alg":"RS256","typ":"JWT"}' | b64url)"
  claims="$(printf '{"iat":%d,"exp":%d,"iss":"%s"}' "$((now - 60))" "$((now + 540))" "$client_id" | b64url)"
  jwt="${header}.${claims}.$(sign_rs256 "${header}.${claims}")"

  # --- Discover the installation for this repo. ---
  local install_id
  install_id="$(api "look up installation" GET "${GITHUB_API}/repos/${owner}/${repo}/installation" \
    -H "Authorization: Bearer ${jwt}" | jq -r '.id')"
  [[ "$install_id" =~ ^[0-9]+$ ]] || die "could not resolve installation id for ${owner}/${repo} (is the App installed there?)"

  # --- Exchange the JWT for a scoped installation access token. ---
  local body token expires
  body="$(jq -nc --arg repo "$repo" --argjson perms "$permissions" \
    '{repositories: [$repo], permissions: $perms}')"
  local resp
  resp="$(api "exchange JWT for installation token" POST \
    "${GITHUB_API}/app/installations/${install_id}/access_tokens" \
    -H "Authorization: Bearer ${jwt}" \
    -d "$body")"
  token="$(jq -r '.token' <<<"$resp")"
  expires="$(jq -r '.expires_at' <<<"$resp")"
  [[ -n "$token" && "$token" != "null" ]] || die "no token in response: $resp"
  log "token minted, expires ${expires}"

  # ONLY the token on stdout.
  printf '%s\n' "$token"
}

main "$@"
