# Security Policy

GitHub Actions Gateway (GAG) runs untrusted CI workloads in a shared, multi-tenant
Kubernetes cluster. Tenant isolation, GitHub egress containment, and least-privilege
RBAC are load-bearing properties of the design, so we take security reports seriously.
The full threat model lives in [`docs/design/05-security.md`](docs/design/05-security.md).

## Supported versions

The project is pre-1.0 and under active development. Security fixes land on the
`main` branch; there is no backport policy for older commits yet. Always track the
latest `main` until a tagged release line exists.

| Version | Supported          |
| ------- | ------------------ |
| `main`  | :white_check_mark: |
| older   | :x:                |

## Reporting a vulnerability

**Please do not open a public issue, pull request, or discussion for a security
vulnerability.** Public disclosure before a fix is available puts every operator at
risk.

Report privately through GitHub's [Private Vulnerability Reporting](https://docs.github.com/en/code-security/security-advisories/guidance-on-reporting-and-writing-information-about-vulnerabilities/privately-reporting-a-security-vulnerability):

1. Go to the repository's **Security** tab.
2. Click **Report a vulnerability** (under "Advisories").
3. Fill in the advisory form with the details below.

This opens a private channel between you and the maintainers and lets us collaborate
on a fix and a coordinated disclosure.

A good report includes:

- The component affected (GMC, AGC, proxy, worker, broker, or the wire protocol).
- The version or commit SHA you tested against.
- A description of the impact (what isolation, egress, or privilege boundary is
  crossed) and which threat in [`docs/design/05-security.md`](docs/design/05-security.md)
  it relates to, if any.
- Reproduction steps or a proof of concept.
- Any suggested remediation, if you have one.

## What to expect

- **Acknowledgement** within 5 business days.
- **Initial assessment** (severity, affected components, whether it reproduces)
  within 10 business days.
- Coordinated disclosure once a fix is available. We will credit reporters who wish
  to be named in the resulting advisory.

## Scope

In scope:

- The GAG control-plane components: Gateway Manager Controller (GMC), Actions Gateway
  Controller (AGC), egress proxy, worker, and broker.
- The wire protocol and the runner-session multiplexing logic.
- The Kubernetes manifests, RBAC, NetworkPolicies, and pod security profiles shipped
  under `config/`.
- The container images built from this repository.

Out of scope:

- Vulnerabilities in upstream dependencies that are already publicly disclosed —
  report those upstream. (We track dependency updates via Dependabot.)
- GitHub Actions, the GitHub API, or the self-hosted runner agent itself.
- Findings that require an already-compromised cluster-admin or node-root attacker,
  unless they cross a boundary the design explicitly defends (see the threat model).
- Denial of service from a tenant exhausting its own `ResourceQuota`.
