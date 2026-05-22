# 7. Test Plan

← [Implementation Phases](06-implementation-phases.md) | [Back to index](README.md) | Next: [Glossary →](08-glossary.md)

---

Testing is structured in three layers. Each layer has a distinct scope, speed contract, and failure signal. All three layers run in CI; only unit and integration tests gate PRs. End-to-end tests run nightly against a staging cluster. Multi-tenant scenarios are explicitly covered at the integration and end-to-end layers, since tenant isolation is a correctness property of the system, not just a deployment concern.

---

## 7.1. Unit Tests

**Scope:** Pure logic within a single package — no network, no Kubernetes API, no real file I/O.

**Speed contract:** Full suite runs in under 30 seconds. Any test requiring a sleep or external call does not belong here.

**Tooling:** Standard `go test ./...` with `testify/assert`. Use `go test -race` in CI to catch goroutine data races.

**What to cover:**

* **Broker API client** — Request construction, header injection, and response parsing for `sessions`, `message`, `acquirejob`, and `renewjob`. Use `httptest.NewServer` to serve static JSON fixtures without hitting GitHub. Assert that `acquirejob` and `renewjob` use the `run_service_url` from the message body, not the broker URL.
* **RenewJob loop** — Verify the per-job goroutine calls `renewjob` at the correct 60-second interval, stops cleanly when the job completes, and handles a non-200 response from `renewjob` by surfacing an error without panicking.
* **Rate-limit (429) backoff** — Drive the broker API client against an `httptest` server that returns `429 Too Many Requests` with a `Retry-After: 30` header. Assert the client honors `Retry-After`, increments `actions_gateway_message_poll_errors_total{reason="rate_limited"}`, and falls back to exponential backoff capped at 5 minutes when the header is absent. Assert that sustained 429s for >10 minutes surface a `RateLimited` condition on the corresponding `RunnerGroup`.
* **Token Manager** — Use a fake clock to advance time to T-5 minutes before token expiry and assert the Token Manager fetches a new token before the old one expires. Assert that session goroutines reading the token during a refresh window get a valid (old or new) token and are never blocked. Assert that `actions_gateway_token_refresh_errors_total` increments on each failed refresh attempt; the alerting threshold for this metric is defined in [docs/operations/observability.md](../../docs/operations/observability.md#recommended-alert-rules) (> 0 for 5 minutes triggers a page).
* **Payload decryption** — AES-256 decryption of the `TaskAgentMessage.Body` field. Test against a pre-generated key/ciphertext pair committed as a fixture. Test failure modes: wrong key, truncated payload, invalid base64.
* **Session registry** — Goroutine lifecycle management: spawn N sessions, verify N goroutines are running, scale down to M, verify M remain with no leaks. Use `goleak.VerifyNone(t)` (from `go.uber.org/goleak`) as a test cleanup hook — it identifies leaked goroutines by stack trace, making failures actionable. Do not use `runtime.NumGoroutine()` deltas, which include Go runtime goroutines and produce unreliable counts.
* **Label-to-pod mapping** — The logic that translates `RunnerGroup` runner labels to a target pod spec. Table-driven tests covering label matches, mismatches, defaults, and invalid configurations.
* **AGC reconciler state machine** — Unit-test the AGC reconciler's desired-vs-actual diffing logic with a fake `client.Client` (provided by `controller-runtime/pkg/client/fake`). Cover create, update, scale-up, scale-down, and delete transitions.
* **GMC reconciler state machine** — Unit-test the GMC reconciler with a fake client. For a given `ActionsGateway` spec, assert that the reconciler produces exactly the expected set of Kubernetes objects (ServiceAccount, Role, RoleBinding, NetworkPolicy, ResourceQuota, proxy Deployment, proxy Service, proxy PodDisruptionBudget, HPA, AGC Deployment) all within the CR's own namespace. Table-driven tests covering spec creation, proxy scaling bound changes, and deletion. For credential rotation specifically: assert that updating `gitHubAppRef.Name` causes the GMC to update the AGC Deployment's volume reference to the new Secret name (triggering a rollout) and does not mutate or delete the old Secret.
* **HPA spec generation** — For a range of `ProxyConfig` inputs (explicit values, all defaults, boundary values), assert the generated `HorizontalPodAutoscaler` has the correct `minReplicas`, `maxReplicas`, and `targetCPUUtilizationPercentage`. Assert that `minReplicas` is always ≥ 1 and ≤ `maxReplicas`. Assert proxy pods always have `resources.requests.cpu` set (required for HPA metric computation).
* **Proxy env injection** — Assert that the AGC Deployment spec produced by the reconciler contains `HTTP_PROXY`, `HTTPS_PROXY`, and `NO_PROXY` env vars. Assert `NO_PROXY` includes `kubernetes.default.svc.cluster.local` and the configured `noProxyCIDRs`. Assert the same three vars appear in the worker pod template.
* **Status Conditions** — Assert that the GMC sets the `Ready`, `ProxyAvailable`, and `AGCAvailable` conditions on `ActionsGatewayStatus` correctly as components become healthy or degrade. Assert conditions use standard `metav1.Condition` types compatible with `kubectl wait --for=condition=Ready`.
* **Runner version rejection** — Unit-test the session goroutine's handling of a `400 Bad Request` from `POST /sessions` containing a version-too-old message. Assert the goroutine surfaces the error as a `RunnerGroup` condition rather than silently retrying in a tight loop.
* **GMC RBAC boundary assertions** — Enumerate the generated ClusterRole rules and assert that no rule grants `*` verbs on `secrets`, `pods`, or `nodes` at the cluster level. This is a regression guard against accidental privilege escalation during development.
* **`gitHubAppRef` namespace defaulting** — Unit-test the defaulting logic: when `Namespace` is omitted, assert it resolves to the `ActionsGateway` CR's own namespace; when set explicitly, assert that value is used instead.
* **Reserved namespace blocklist validation** — Unit-test the admission webhook logic that rejects `ActionsGateway` CRs created in reserved namespaces (`kube-system`, `kube-public`, `actions-gateway-system`, etc.).

---

## 7.2. Integration Tests

**Scope:** Multiple components interacting with real infrastructure dependencies — a live Kubernetes API server and a stubbed GitHub backend. No actual GitHub network calls. No container image builds and no real Kubernetes scheduling — pods are created in the API server but never actually scheduled.

**Speed contract:** Full suite runs in under 5 minutes. Each test must complete in under 30 seconds. Tests run against a local `envtest` API server (from `controller-runtime`). `kind` is not used for this layer — it requires container builds and is slower than envtest.

**Tooling:** `controller-runtime/pkg/envtest` for the Kubernetes API surface. A shared stateful `httptest` fake broker (`internal/brokertest`) for the GitHub broker — tests control it by enqueuing job messages on demand and asserting which sessions were deleted, rather than replaying static fixtures. Standard `go test` with `testify` and `gomega.Eventually` for eventually-consistent assertions. Ginkgo is not used; the integration tests follow the same `testing`-package style as the unit tests in this repo.

> **Detailed test cases** are specified in [docs/plan/integration-tests.md](../plan/integration-tests.md), including the file layout, suite setup, and CI workflow.

**What to cover:**

* **CRD install and validation** — Install both `ActionsGateway` and `RunnerGroup` CRD schemas into `envtest`. Verify valid manifests are accepted. Verify invalid specs are rejected at admission: the namespace blocklist webhook rejects reserved names; CRD CEL rules reject `priorityTiers` in non-ascending threshold order; CRD CEL rules reject `maxWorkers` values that conflict with the last `priorityTiers` threshold.
* **GMC tenant provisioning** — Create a namespace, then apply an `ActionsGateway` CR into it. Verify the GMC creates all expected child resources within that same namespace: ServiceAccount (AGC + worker), Role, RoleBinding, NetworkPolicy, ResourceQuota, proxy Deployment (with `resources.requests.cpu` set), proxy Service, proxy PodDisruptionBudget, HPA, AGC Deployment (with `HTTP_PROXY`, `HTTPS_PROXY`, and `NO_PROXY` set), and bootstrap RunnerGroups. Verify the GMC does not create or modify the namespace itself. Assert `ActionsGatewayStatus.Conditions` includes the `ProxyAvailable` and `AGCAvailable` condition types. Note: because envtest does not schedule pods, `Deployment.status.readyReplicas` stays at 0 — the `Ready` condition will not become `True` and tests assert the non-Ready state is reported correctly rather than asserting `Ready=True`. Additional provisioning cases: `gitHubAppRef.namespace` omitted defaults to the CR's own namespace; `spec.proxy.noProxyCIDRs` is merged with (not replaced by) the mandatory cluster-internal exclusions; updating `gitHubAppRef.name` causes the AGC Deployment to reference the new Secret without deleting the old one.
* **GMC tenant teardown** — Delete an `ActionsGateway` CR and verify the GMC removes all associated resources, including the proxy Deployment, Service, PodDisruptionBudget, and HPA, without affecting any other tenant namespace. Assert that a second `ActionsGateway` CR remains fully intact. Also verify that re-applying the same CR after teardown brings all resources back cleanly.
* **HPA bounds update** — Update `spec.proxy.maxReplicas` on a live `ActionsGateway` CR and verify the GMC patches the HPA to reflect the new bound within one reconcile cycle.
* **Proxy NetworkPolicy content** — Verify the content of the generated `NetworkPolicy` in the API server: proxy pod egress includes the GitHub CIDR rules; AGC and worker pods have egress rules only to the proxy ClusterIP; DNS egress is always present. Verify that `spec.proxy.managedNetworkPolicy: false` suppresses the GitHub CIDR egress rules. Verify the `IPRangeReconciler` patches an existing policy when the fetched CIDR set changes. Note: envtest does not run a CNI plugin — these tests verify the `NetworkPolicy` spec content, not actual packet filtering.
* **AGC RBAC scope enforcement** — Provision an `ActionsGateway` CR so the GMC creates the `actions-gateway-agc` ServiceAccount and its Role/RoleBinding. Impersonate that ServiceAccount via `rest.ImpersonateConfig` and attempt to list resources in a different tenant's namespace. Assert the API server returns 403. Assert that listing in the same namespace returns 200.
* **AGC reconciler end-to-end** — Deploy a `RunnerGroup`, verify the AGC starts exactly one listener goroutine at rest (the permanent baseline). Enqueue jobs via the fake broker and verify additional goroutines spawn up to `.spec.maxListeners`. Verify idle goroutines shut down once the queue empties, leaving exactly one active listener. Update `.spec.maxListeners`, verify the new ceiling takes effect without restarting in-flight goroutines. Delete the resource, verify all goroutines exit and no agent Secrets or worker Pods are orphaned.
* **Secret lifecycle** — Verify that a Secret is created with the correct payload labels when a job is intercepted, scoped to the correct tenant namespace, and deleted after the pod terminates. In envtest, pod phase must be advanced manually (no kubelet) — tests set the pod status to `Succeeded` via the status subresource client.
* **Pod provisioning** — Verify that the AGC creates a Pod with the correct image, resource limits, volume mounts, and security context when a job payload is received from the fake broker. Verify controller-enforced invariants are applied unconditionally: `automountServiceAccountToken: false`, `serviceAccountName: actions-gateway-worker`, `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY` env vars injected with the provisioner's values. Verify `priorityTiers` tier assignment by pod count. Verify `maxWorkers` ceiling holds the third pod until an active pod completes.
* **Failure recovery** — Simulate a non-eviction pod failure (set pod status to `Failed` without `reason: Evicted`) and verify the AGC cleans up the associated Secret without leaking it and without triggering an automatic rerun. Simulate an eviction (set `reason: Evicted`) and verify the AGC calls the rerun API, increments `actions_gateway_eviction_retries_total`, and cleans up the Secret. Verify `maxEvictionRetries: 0` suppresses the rerun call and increments `actions_gateway_eviction_retries_exhausted_total` instead.
* **SIGTERM session cleanup** — Start an AGC against the fake broker, burst the listener count to N sessions. Cancel the reconciler's context (simulating SIGTERM). Assert the AGC issues `DELETE /sessions/{id}` for every registered session before the context fully unwinds, confirmed by the fake broker. Assert no goroutine leak via `goleak.VerifyNone`.

---

## 7.3. End-to-End Tests

**Scope:** A real workflow job dispatched from GitHub, executed by the gateway against a staging Kubernetes cluster, with results confirmed in the GitHub Actions UI.

**Speed contract:** Individual tests take 2–5 minutes each. The full suite runs nightly; a focused smoke test (single job, single runner) runs on every merge to `main`.

**Tooling:** A dedicated test GitHub repository with a small set of workflow files. The staging cluster runs the gateway operator in a locked-down namespace. A test harness script (under `test/e2e/`) triggers workflow dispatches via the GitHub REST API and polls the run to completion, asserting on final status and log content.

**What to cover:**

* **Smoke test — single job, single tenant** — Create a namespace, apply one `ActionsGateway` CR into it, dispatch a minimal workflow (`echo "hello"`), and assert the run completes green with correct log output in the GitHub UI. This is the merge gate.
* **Parallel job execution** — Dispatch a matrix workflow with 10 parallel jobs against a single tenant and assert all 10 complete successfully, verifying the session multiplexer handles concurrent polling without message collisions.
* **Multi-tenant isolation** — Provision two `ActionsGateway` CRs pointing to different GitHub repositories and different namespaces. Dispatch simultaneous jobs to both. Assert that each job runs in its own namespace, that no Secrets are visible across namespaces, and that one tenant's resource consumption does not affect the other's job throughput.
* **Proxy egress isolation** — Confirm via network observation that GitHub API calls and log stream traffic from both the AGC and worker pods exit through the tenant's proxy Service address, not directly through the cluster NAT. Assert no direct egress to GitHub IPs is observed from AGC or worker pods.
* **Proxy HA under disruption** — Cordon one node hosting a proxy pod and drain it. Assert the PodDisruptionBudget prevents eviction until another proxy pod is scheduled, and that in-flight jobs are not interrupted during the disruption.
* **Tenant provisioning and deprovisioning** — Create a namespace, apply an `ActionsGateway` CR, run a job successfully, then delete the CR. Assert all GMC-owned resources (proxy Deployment, proxy Service, HPA, PodDisruptionBudget, AGC Deployment, RBAC, NetworkPolicy, ResourceQuota) are removed but the namespace itself remains intact. Re-apply the CR and verify a fresh gateway and proxy pool come up cleanly and can run jobs again.
* **Job failure propagation** — Dispatch a workflow with a deliberately failing step (`exit 1`) and assert the GitHub UI correctly reflects the failure status. Verify the worker pod exits non-zero and is still cleaned up within the tenant namespace.
* **Cross-tenant Secret opacity** — After two tenants have completed jobs, assert via namespace inspection that neither tenant's namespace contains Secrets belonging to the other. Assert that the AGC ServiceAccount for tenant A cannot read Secrets in tenant B's namespace.
* **Resource cleanup under load** — Dispatch 50 sequential jobs across 5 tenants and assert zero pod or Secret leaks remain after all runs complete. Checked by polling all tenant namespaces for residual resources 60 seconds post-completion.
* **Proxy HPA scaling** — Dispatch a sustained burst of 50 concurrent jobs against a single tenant with `spec.proxy.maxReplicas: 5` and `spec.proxy.minReplicas: 2`. Assert the HPA scales the proxy pool above `minReplicas` during the burst, and that replica count returns to `minReplicas` within 5 minutes of load subsiding. Assert no jobs are dropped during scale-up or scale-down.
* **GMC restart resilience** — Delete and re-create the GMC pod while tenants are active. Assert that in-flight jobs are not interrupted, the GMC correctly re-derives tenant state on restart, no duplicate resources are created during reconciliation, and the proxy HPAs remain intact.
* **AGC restart resilience** — Mid-run on a single tenant, delete and re-create the AGC pod. Assert in-flight jobs are not double-acquired, the AGC converges back to the correct goroutine count within one reconcile cycle, and all traffic continues routing through the proxy pool.
* **RenewJob under long-running job** — Dispatch a workflow that sleeps for 15 minutes. Assert the job completes successfully, confirming the RenewJob loop correctly kept the lock alive across multiple renewal cycles without GitHub cancelling the job.
* **Rolling AGC upgrade** — Start a steady stream of dispatched workflows against a single tenant, then patch the AGC Deployment image to a new tag mid-flight. Assert the upgrade completes the rolling update successfully, in-flight long polls drop and reconnect (with no duplicated job acquires observed via the broker's audit log), per-job RenewJob loops resume after the new pod starts, and total workflow success rate over the upgrade window stays above 95%. Assert that jobs whose lock expired during the blackout are redelivered and complete on retry.
* **GitHub IP range reconciliation** — Simulate a GitHub meta API response with a new IP range not present in the existing NetworkPolicy. Trigger a GMC reconcile cycle and assert the proxy pod NetworkPolicy egress rules are updated to include the new range. Assert that `spec.proxy.managedNetworkPolicy: false` suppresses the update.

---

← [Implementation Phases](06-implementation-phases.md) | [Back to index](README.md) | Next: [Glossary →](08-glossary.md)
