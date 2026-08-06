# 8. Glossary

← [Test Plan](07-test-plan.md) | [Back to index](README.md) | Next: [Appendix A — Capacity Targets & SLOs →](appendix-a-capacity-slos.md)

---

| Term | Definition |
| --- | --- |
| **GMC** | Gateway Manager Controller. Cluster-scoped operator that watches `ActionsGateway` CRs and provisions per-tenant gateway resources. See [§2.1](02-architecture.md#21-tier-1--gateway-manager-controller-gmc). |
| **AGC** | Actions Gateway Controller. Namespace-scoped operator, one per tenant, that owns the runtime session loop and job-acquisition pipeline. See [§2.2](02-architecture.md#22-tier-2--actions-gateway-controller-agc). |
| **ActionsGateway** | Namespace-scoped CRD that declares a tenant gateway instance. Created by the tenant in their own namespace; reconciled by the GMC. See [§3.1](03-api-contracts.md#31-kubernetes-crd-schemas). |
| **RunnerGroup** | Namespace-scoped CRD that declares a pool of virtual runner sessions sharing a label set and pod template. Reconciled by the AGC. See [§3.1](03-api-contracts.md#31-kubernetes-crd-schemas). |
| **Broker** | The GitHub Actions service that brokers runner sessions and dispatches jobs. Accessed via `broker_url` for session and message endpoints. |
| **Run Service** | The GitHub service that owns a specific job's lifecycle (acquire, renew, complete). Accessed per-job via `run_service_url` extracted from each `GetMessage` response. |
| **Twirp Results Service** | GitHub's protobuf-over-HTTP log and step-summary ingestion endpoint. Worker pods stream live job output to this service over a long-lived HTTP/2 connection. |
| **`sessionId`** | Identifier returned by `POST /sessions`, used as the long-poll session key for `GET /message` calls. Bound to a virtual runner registration. |
| **`runner_request_id`** | Identifier in a `RunnerJobRequest` message that the AGC passes as `jobMessageId` to `AcquireJob` and as `jobId` to `RenewJob`. Treated opaquely. |
| **`planId`** | Identifier in the `AcquireJob` response (header `x-plan-id` or body `.plan.planId`). Required for every `RenewJob` call for that job. See [§3.4](03-api-contracts.md#34-broker-payload-blueprints-go-structs). |
| **`RunnerJobRequest`** | Message type returned by `GET /message` when a job is available. Body contains `run_service_url`, `runner_request_id`, and `billing_owner_id`. |
| **`ACTIONS_RUNTIME_TOKEN`** | Single-use token in the decrypted job payload that the worker uses to authenticate to the Twirp Results Service for log streaming. |
| **Installation token** | Short-lived (1 hour) GitHub App access token derived from a signed JWT and exchanged at `POST /app/installations/{id}/access_tokens`. Held in memory by the Token Manager. |
| **RuntimeClass** | Kubernetes mechanism for selecting an alternative container runtime per pod (e.g. `gvisor`, `kata-containers`). Optional — see [Appendix B](appendix-b-worker-isolation.md). |

---

← [Test Plan](07-test-plan.md) | [Back to index](README.md) | Next: [Appendix A — Capacity Targets & SLOs →](appendix-a-capacity-slos.md)
