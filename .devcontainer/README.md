# Agent sandbox

A container for running Claude Code autonomously (`--dangerously-skip-permissions`
or default mode with a broad allowlist) without the agent being able to damage
your host or exfiltrate credentials.

> **Status: draft.** Review and adapt before wiring into a fleet. Nothing here
> is committed or active until you build and run it.

## Why a container changes the security model

In full bypass mode (`--dangerously-skip-permissions`), Claude Code consults
**none** of the `allow`/`ask`/`deny` rules. The container becomes the only
boundary. That bounds damage to your *host*, but inside the container the agent
still holds whatever you mounted — push credentials, tokens. So "the container
is the sandbox" is only half true: it protects the laptop, not the repo or the
secrets.

This setup adds three layers so broad autonomy stays safe:

| Layer | File | Stops |
|---|---|---|
| Network egress allowlist | `init-firewall.sh` | Exfiltration / C2 to any host except the Anthropic API and GitHub |
| Credential-read deny rules | `claude-settings.json` | The agent reading a key/token and leaking it through an *allowed* channel (e.g. a PR body on GitHub) |
| Scoped credentials | _operational, see below_ | Catastrophic git ops surviving even if a token leaks |

No single layer is sufficient; they cover each other's gaps.

## Egress allowlist (`init-firewall.sh`)

Default-drops all outbound traffic, then allows DNS, established return flows,
the Anthropic API, and GitHub's published CIDR ranges (pulled from
`api.github.com/meta`). It self-verifies at the end: GitHub must be reachable
and `example.com` must be blocked, or it exits non-zero.

Because the repo **vendors** its Go dependencies, `go build`/`go test` run
offline — the allowlist deliberately does *not* include the Go module proxy. If
you drop vendoring, add `proxy.golang.org`/`sum.golang.org` to `ALLOWED_DOMAINS`,
but note their `storage.googleapis.com` backend is CDN-backed and resolves to
shifting IPs; for CDN-heavy egress prefer a filtering HTTP CONNECT proxy
(tinyproxy/squid with a hostname allowlist, `HTTPS_PROXY=...`) over IP rules.

Runs once at container start via `devcontainer.json`'s `postStartCommand`.
Requires `NET_ADMIN`/`NET_RAW` (granted in `runArgs`).

## Credential-read deny rules (`claude-settings.json`)

Baked into the agent user's `~/.claude/settings.json`, so repo-level settings
can't relax them (`deny` always wins). Blocks reads of `*.pem`, `*.key`, SSH
keys, `.env`, `secrets/`, `/run/secrets`, plus `sudo` and Keychain access.

**Known gap:** denying the `Read` *tool* doesn't stop `git` from using a
credential file (different process), which is what you want — but it also
doesn't stop the agent from `cat`/`head`-ing that file via Bash, and it can't
hide an env-var token from `printenv`. Don't rely on secrecy. The robust fix is
to never put readable long-lived secrets in the container at all — see below.

## Scoped credentials (operational — do this, don't skip it)

The token in the container is the real risk surface. Make a leak survivable:

### One-time: create the agent's GitHub App

Do **not** reuse the `actions-gateway-test` App — that one is the runner
control plane (`actions:write`, `administration:write`, …) and has no
`contents`/`pull_requests`. The agent needs its own least-privilege identity.

1. github.com → org `actions-gateway` → Settings → Developer settings → GitHub
   Apps → **New GitHub App**.
2. **Repository permissions:** Contents → *Read and write*; Pull requests →
   *Read and write*; Metadata → *Read* (auto). Add Workflows → *Read and write*
   **only** if agents edit `.github/workflows/`. Leave everything else *No access*.
3. **Webhook:** uncheck *Active* (not needed). **Where can this be installed:**
   *Only on this account*.
4. Create it → note the **Client ID** (`Iv23…`) → **Generate a private key**
   (downloads a `.pem`) → **Install** the App on the `github-actions-gateway`
   repo only.
5. Store the key in Keychain under a *distinct account* (hex-encoded, matching
   how the mint script reads it), then shred the download:
   ```bash
   security add-generic-password -U -a actions-gateway-agent \
     -s github-app-private-key \
     -w "$(xxd -p < ~/Downloads/agent-app.*.private-key.pem | tr -d '\n')"
   rm -P ~/Downloads/agent-app.*.private-key.pem
   ```

The mint script then targets this App via env overrides — no code change:

```bash
GITHUB_APP_CLIENT_ID=Iv23…theNewAppId \
KEYCHAIN_ACCOUNT=actions-gateway-agent \
  scripts/mint-installation-token.sh
```

### Operational rules

1. **Short-lived, repo-scoped token, not the App PEM.** The script above mints
   an *installation* token (expires in ≤1h), scoped to this one repo with only
   `contents:write`+`pull_requests:write`. Pass it as `AGENT_GH_TOKEN`. The App
   private key never leaves the host Keychain.
2. **Protect `main` server-side.** Enable branch protection requiring PR +
   green CI and disallowing force-push. This is the only reliable guard against
   a destructive push — client-side `deny` globs can't reliably tell which
   branch a `git push` targets. With protection on, even a fully compromised
   agent can't rewrite `main`.
3. **Prefer SSH deploy-key-over-agent-socket** if you want no token on disk or
   in env at all: forward an `ssh-agent` socket holding a deploy key that lacks
   force-push rights. The key bytes never enter the container.

## Build & run

```bash
# Build the image
docker build -t actions-gateway-agent .devcontainer

# Run one autonomous agent (token minted fresh on the host)
docker run --rm -it \
  --cap-add=NET_ADMIN --cap-add=NET_RAW \
  -e AGENT_GH_TOKEN="$(./scripts/mint-installation-token.sh)" \
  -e ANTHROPIC_API_KEY="$ANTHROPIC_API_KEY" \
  -v "$PWD:/workspace" \
  actions-gateway-agent \
  bash -lc 'sudo /usr/local/bin/init-firewall.sh && claude --dangerously-skip-permissions -p "next"'
```

(`mint-installation-token.sh` is a stub you still need — it reads the App PEM
*on the host* and exchanges it for a short-lived installation token. The PEM
stays on the host; only the token crosses into the container.)

Or open the folder in an editor/CLI that understands `devcontainer.json`.

## Residual risks (be honest about these)

- The agent can still do anything *within* its token's scope: open junk PRs,
  push to non-protected branches, burn CI minutes. Scope + branch protection
  cap the damage; they don't eliminate it.
- The allowlist trusts GitHub wholesale — anything reachable under GitHub's
  CIDRs (gists, any repo the token can touch) is a possible exfil sink. This is
  why the token must be narrowly scoped.
- `api.github.com/meta` ranges are fetched at start; if GitHub changes ranges
  mid-run, new IPs aren't picked up until the next container start.
