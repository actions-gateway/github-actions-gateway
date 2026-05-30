# cmd/

Entrypoints for every binary built from this repo. Each subdirectory is its own Go module (see [go-workspaces.md](../docs/development/go-workspaces.md)).

| Binary | Role | Tier |
|---|---|---|
| [agc](agc/) | Actions Gateway Controller — reconciles `RunnerGroup` CRDs into adaptive listener goroutine pools that long-poll the GitHub Actions broker for jobs and provision ephemeral worker pods. | Per-tenant |
| [gmc](gmc/) | Gateway Manager Controller — reconciles a single `ActionsGateway` CR into per-tenant AGC instances and their supporting resources (Namespace, RBAC, Secret, Service, NetworkPolicy, egress proxy pool). | Cluster |
| [worker](worker/) | Entrypoint wrapper for ephemeral runner pods. Materializes job payload + JIT runner config from a mounted Secret, then exec's `Runner.Worker` over anonymous pipes. | Per-job |
| [proxy](proxy/) | Stateless HTTPS CONNECT proxy that gives each tenant an isolated egress IP for GitHub traffic. | Per-tenant pool |
| [probe](probe/) | Standalone broker wire-protocol probe — authenticates via a GitHub App, registers a session, polls for a job, and renews the lock. Used for Milestone 1 investigations and protocol regression checks against real GitHub. | Dev tool |

See [DESIGN.md](../DESIGN.md) and [docs/design/02-architecture.md](../docs/design/02-architecture.md) for how these tiers fit together.
