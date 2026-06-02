# Agent sandbox image: Go toolchain + Claude Code + egress-lockdown tooling.
# Matches the repo's builder base (golang:1.26, see cmd/*/Dockerfile).
FROM golang:1.26

# Tools the agent and the firewall need.
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates git curl jq ripgrep \
      iptables ipset dnsutils sudo gnupg \
    && rm -rf /var/lib/apt/lists/*

# GitHub CLI (from the official apt repo).
RUN curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
      | dd of=/usr/share/keyrings/githubcli-archive-keyring.gpg \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
      > /etc/apt/sources.list.d/github-cli.list \
    && apt-get update && apt-get install -y --no-install-recommends gh \
    && rm -rf /var/lib/apt/lists/*

# Node + Claude Code CLI.
RUN curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && npm install -g @anthropic-ai/claude-code \
    && rm -rf /var/lib/apt/lists/*

# Unprivileged agent user. It may run ONLY the firewall script as root (so the
# lockdown can program iptables at start); it has no general sudo.
RUN useradd -m -s /bin/bash agent \
    && echo 'agent ALL=(root) NOPASSWD: /usr/local/bin/init-firewall.sh' \
      > /etc/sudoers.d/agent-firewall \
    && chmod 0440 /etc/sudoers.d/agent-firewall

COPY init-firewall.sh /usr/local/bin/init-firewall.sh
RUN chmod 0755 /usr/local/bin/init-firewall.sh

# Container-level deny rules at the user-settings layer (repo settings can't
# relax these).
COPY claude-settings.json /home/agent/.claude/settings.json
RUN chown -R agent:agent /home/agent/.claude

USER agent
WORKDIR /workspace
