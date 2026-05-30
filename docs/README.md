# Documentation

Documentation for the GitHub Actions Gateway — a four-tier system for running
GitHub Actions self-hosted runners at scale on Kubernetes. For the project
overview see the top-level [README](../README.md); for the condensed design entry
point see [DESIGN.md](../DESIGN.md).

## Start here

- [getting-started.md](getting-started.md) — deploy the system, wire up the GitHub
  App, validate, and rotate credentials.
- [STATUS.md](STATUS.md) — single source of truth for progress and priorities.

## Sections

| Area | What's in it |
|---|---|
| [design/](design/README.md) | Full system design — architecture, API/CRD contracts, security, test plan, glossary, appendices. |
| [development/](development/README.md) | Developer workflow — building, testing, kind iteration, Go workspaces, code generation. |
| [operations/](operations/README.md) | Operator guides — runbook, troubleshooting, observability, upgrades, tenant onboarding. |
| [plan/](plan/README.md) | Implementation plans and audits. Authoritative ordering lives in [STATUS.md](STATUS.md). |

## Find your path

Each section's `README.md` is its index. For role-based reading orders (architect,
platform engineer, security engineer, tenant team) see
[design/README.md § Reading Paths by Role](design/README.md#reading-paths-by-role).
New to the vocabulary? Start with the [glossary](design/08-glossary.md).
