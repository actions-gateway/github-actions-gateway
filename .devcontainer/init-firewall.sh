#!/usr/bin/env bash
#
# init-firewall.sh — lock down container egress to an allowlist.
#
# Description:
#   Runs once at container start (as root, before the agent user starts doing
#   work). Default-drops all outbound traffic except DNS, established return
#   flows, and a small allowlist: the Anthropic API (so Claude Code works) and
#   GitHub (so git/gh work). Everything else — including any attempt to POST a
#   leaked secret to an arbitrary host — is dropped at the kernel.
#
#   Go builds do NOT need egress: this repo vendors its dependencies, so
#   `go build` / `go test` run fully offline. If you ever drop vendoring, add
#   the Go module proxy hosts (proxy.golang.org, sum.golang.org, and their
#   storage.googleapis.com backend) to ALLOWED_DOMAINS — note that CDN-backed
#   hosts resolve to changing IPs, so a filtering HTTP proxy is more robust
#   than IP allowlisting for those (see .devcontainer/README.md).
#
# Requires: iptables, ipset, dig (dnsutils), jq, curl. Must run with NET_ADMIN.
#
# Usage:
#   sudo ./init-firewall.sh

# --- Strict mode ---
set -euo pipefail

# Hostnames the agent is permitted to reach (resolved to IPs below).
readonly ALLOWED_DOMAINS=(
  "api.anthropic.com"
  "statsig.anthropic.com"
)

readonly IPSET_NAME="agent-allow"

log() { printf '[init-firewall] %s\n' "$*" >&2; }

die() { log "ERROR: $*"; exit 1; }

# Remove any pre-existing rules so re-runs are idempotent.
flush_existing() {
  local table
  for table in filter nat mangle; do
    iptables -t "$table" -F
    iptables -t "$table" -X
  done
  ipset destroy "$IPSET_NAME" 2>/dev/null || true
}

# Build the set of allowed destination CIDRs/IPs.
build_allowset() {
  ipset create "$IPSET_NAME" hash:net

  # GitHub publishes its CIDR ranges via the meta API. Pull web/api/git/
  # packages so git clone, gh, and HTTPS to github.com all work.
  local meta
  meta="$(curl -fsSL --max-time 20 https://api.github.com/meta)" \
    || die "could not fetch GitHub meta ranges"

  local cidr
  while IFS= read -r cidr; do
    [[ -n "$cidr" ]] || continue
    ipset add "$IPSET_NAME" "$cidr" -exist
  done < <(jq -r '(.web + .api + .git + .packages)[]' <<<"$meta" | sort -u)

  # Resolve each allowlisted hostname to its current A records.
  local domain ip count
  for domain in "${ALLOWED_DOMAINS[@]}"; do
    count=0
    while IFS= read -r ip; do
      [[ -n "$ip" ]] || continue
      ipset add "$IPSET_NAME" "$ip" -exist
      (( count += 1 ))
    done < <(dig +short A "$domain" | grep -E '^[0-9.]+$' || true)
    (( count > 0 )) || die "could not resolve allowlisted host: $domain"
    log "allowed ${domain} (${count} addrs)"
  done
}

# Apply the default-deny policy with the allowlist carved out.
apply_rules() {
  # Loopback is always fine.
  iptables -A INPUT  -i lo -j ACCEPT
  iptables -A OUTPUT -o lo -j ACCEPT

  # Keep established/related flows (return traffic for allowed connections).
  iptables -A INPUT  -m state --state ESTABLISHED,RELATED -j ACCEPT
  iptables -A OUTPUT -m state --state ESTABLISHED,RELATED -j ACCEPT

  # DNS resolution must work (for the resolver and for `dig` above).
  iptables -A OUTPUT -p udp --dport 53 -j ACCEPT
  iptables -A OUTPUT -p tcp --dport 53 -j ACCEPT

  # Outbound to the allowlisted IP set only.
  iptables -A OUTPUT -m set --match-set "$IPSET_NAME" dst -j ACCEPT

  # Default deny everything else.
  iptables -P INPUT   DROP
  iptables -P FORWARD DROP
  iptables -P OUTPUT  DROP
}

# Prove the policy is actually live before handing control to the agent.
verify() {
  curl -fsSL --max-time 10 -o /dev/null https://api.github.com/zen \
    || die "verification failed: GitHub unreachable through allowlist"
  if curl -fsSL --max-time 5 -o /dev/null https://example.com 2>/dev/null; then
    die "verification failed: egress to example.com succeeded but must be blocked"
  fi
  log "egress lockdown verified (GitHub reachable, example.com blocked)"
}

main() {
  local tool
  for tool in iptables ipset dig jq curl; do
    command -v "$tool" >/dev/null 2>&1 || die "missing required tool: $tool"
  done
  flush_existing
  build_allowset
  apply_rules
  verify
  log "firewall initialized"
}

main "$@"
