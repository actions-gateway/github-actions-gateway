# Security Operations: Abuse Detection & Response

> **Audience:** SRE, Security

This runbook turns the abuse
heuristics in the [threat model](../design/05-security.md) into concrete,
operator-actionable detections. It complements — does not replace — the
availability/SLO alerting in [observability.md](observability.md) and the
incident-response procedures in [runbook.md](runbook.md).

The signals here detect **abuse or compromise** (a misbehaving tenant, a
compromised AGC/GMC, a saturation attack), not ordinary capacity
degradation. Each row of [§5.1](../design/05-security.md#51-gmc-level-threats-cluster-scoped)
and [§5.2](../design/05-security.md#52-agc--proxy-level-threats-namespace-scoped)
of the threat model that says "operators should monitor X" is mapped below
to the metric or audit-log query that surfaces it.

Two detection substrates are used:

- **Prometheus metrics** — emitted by the controllers and proxy today.
  See [observability.md](observability.md) for the full reference and how
  to scrape them. Alert rules are in [§ Prometheus abuse alerts](#prometheus-abuse-alerts)
  below.
- **API-server audit log** — the only substrate that can see a compromised
  AGC/GMC issuing RBAC-permitted-but-anomalous calls (e.g. a full-body
  Secret `list`). These detections require an audit policy that captures
  the relevant verbs; the controllers cannot self-report calls made
  out-of-band by a compromised binary. A sample audit policy is tracked
  separately (see [§ Audit-log abuse detections](#audit-log-abuse-detections)).

---

## Table of Contents

- [Threat → signal map](#threat--signal-map)
- [Prometheus abuse alerts](#prometheus-abuse-alerts)
- [Audit-log abuse detections](#audit-log-abuse-detections)
  - [API server audit policy (sample)](#api-server-audit-policy-sample)
- [Response playbooks](#response-playbooks)
  - [Suspected compromised AGC (tenant-scoped)](#suspected-compromised-agc-tenant-scoped)
  - [Suspected compromised GMC (cluster-scoped, Tier-0)](#suspected-compromised-gmc-cluster-scoped-tier-0)
  - [Proxy saturation / slowloris](#proxy-saturation--slowloris)
- [Posture scanning (preventive)](#posture-scanning-preventive)
  - [Manifest posture — polaris (automated, in CI)](#manifest-posture--polaris-automated-in-ci)
  - [CIS-benchmark posture — kube-bench (manual, pre-production)](#cis-benchmark-posture--kube-bench-manual-pre-production)
- [Tenant egress posture & deliberate widening](#tenant-egress-posture--deliberate-widening)
  - [Managing egress at scale](#managing-egress-at-scale)
  - [Expressing GitHub egress by FQDN: the `egressPolicyMode` opt-in](#expressing-github-egress-by-fqdn-the-egresspolicymode-opt-in)
- [Tightening AGC apiserver egress: the `apiserver-cidrs` allowlist](#tightening-agc-apiserver-egress-the-apiserver-cidrs-allowlist)
- [GitHub API base URL must be HTTPS](#github-api-base-url-must-be-https)
- [Priority classes: the `allowed-priority-classes` allowlist](#priority-classes-the-allowed-priority-classes-allowlist)
- [License attribution in images](#license-attribution-in-images)
- [Image provenance: signature & SBOM verification](#image-provenance-signature--sbom-verification)
  - [Verify a signature](#verify-a-signature)
  - [Verify build provenance](#verify-build-provenance)
  - [Retrieve and inspect the SBOM](#retrieve-and-inspect-the-sbom)
- [Reference Links](#reference-links)

## Threat → signal map

| Threat (from [05-security.md](../design/05-security.md)) | Abuse signal | Detection substrate | Severity |
|---|---|---|---|
| **Eviction-Retry API Misuse** ([§5.2](../design/05-security.md#52-agc--proxy-level-threats-namespace-scoped)) — compromised AGC looping `rerun-failed-jobs` | `eviction_retries_total` rate climbs without matching node pressure; `eviction_retries_exhausted_total` increments | Metric | Ticket → Page on sustained climb |
| **Proxy Pool Exhaustion / slowloris** ([§5.2](../design/05-security.md#52-agc--proxy-level-threats-namespace-scoped), M-17/M-18) | `proxy_connections_active` pinned near capacity; `proxy_tunnel_duration_seconds` mass in the 6h bucket | Metric | Page |
| **Server-Side Request Forgery (SSRF) / destination probing via proxy** ([§5.2](../design/05-security.md#52-agc--proxy-level-threats-namespace-scoped), M-2/M-12) | `proxy_dial_errors_total` spike (workers repeatedly dialing blocked destinations) | Metric | Ticket |
| **DoS via Resource Exhaustion** ([§5.2](../design/05-security.md#52-agc--proxy-level-threats-namespace-scoped)) — rogue workflow exhausting tenant quota | `kube_resourcequota` used/hard ratio sustained at 1.0 | Metric (kube-state-metrics) | Ticket |
| **`ActionsGateway` CR in reserved namespace / spec probing** ([§5.1](../design/05-security.md#51-gmc-level-threats-cluster-scoped)) | Admission webhook `403` rejection rate | Metric (controller-runtime) | Ticket |
| **Cross-Tenant GitHub App Credential Leakage / key compromise** ([§5.1](../design/05-security.md#51-gmc-level-threats-cluster-scoped)) | `token_refresh_errors_total` spike (key revoked out-of-band, or a forged token rejected) | Metric | Page |
| **Mass tenant provisioning** ([§5.1](../design/05-security.md#51-gmc-level-threats-cluster-scoped)) — compromised GMC deploying workloads | `managed_gateways` jumps unexpectedly | Metric | Page |
| **AGC overpermissioned Secret access** ([§5.2](../design/05-security.md#52-agc--proxy-level-threats-namespace-scoped), H-2 residual) — compromised AGC binary issuing a full-body Secret `list` | AGC ServiceAccount `list secrets` in audit log (legit code path is metadata-only — see [security.md H-2](../plan/security.md)) | Audit log | Page |
| **GMC privilege escalation** ([§5.1](../design/05-security.md#51-gmc-level-threats-cluster-scoped)) — compromised GMC reading Secrets / writing out-of-tenant resources | GMC ServiceAccount `get secrets` beyond reconcile cadence; `namespaces patch` denied by `namespace-psa-guard`; any write denied by `gmc-tenant-resource-guard` | Audit log | Page |

---

## Prometheus abuse alerts

These rules reference metrics that are emitted today
([observability.md § Full Metrics Reference](observability.md#full-metrics-reference)).
Drop them into the same `PrometheusRule` group as the SLO alerts, or a
dedicated `actions-gateway-security` group. Tune thresholds to your fleet.

```yaml
groups:
  - name: actions-gateway-security
    rules:

      # Page: eviction-retry loop — sustained re-queue rate without a
      # matching node-pressure event suggests rerun-failed-jobs abuse
      # (compromised AGC) rather than genuine eviction churn.
      - alert: ActionsGatewayEvictionRetryAbuse
        expr: |
          sum by (namespace, runner_group) (
            rate(actions_gateway_eviction_retries_total[15m])
          ) > 0.05
        for: 30m
        labels:
          severity: critical
        annotations:
          summary: "Sustained eviction-retry rate in {{ $labels.namespace }}/{{ $labels.runner_group }}"
          description: "Eviction retries have run >0.05/s for 30m. Correlate with node pressure; if nodes are healthy, suspect a rerun loop and inspect the AGC."

      # Page: proxy connection pool saturation (slowloris / tunnel flood).
      # Pair with HPA: if replicas are already at maxReplicas this is a
      # ceiling, not headroom.
      - alert: ActionsGatewayProxyConnectionsSaturated
        expr: |
          actions_gateway_proxy_connections_active > 500
        for: 5m
        labels:
          severity: critical
        annotations:
          summary: "Proxy CONNECT tunnels saturated in {{ $labels.namespace }}"
          description: "Active tunnels > 500 for 5m. Check for slowloris (many long-lived tunnels) via the tunnel-duration histogram."

      # Page: tunnels accumulating in the top (6h) duration bucket means
      # connections are riding the absolute lifetime cap — the M-18
      # slowloris signature.
      - alert: ActionsGatewayProxyLongLivedTunnels
        expr: |
          increase(
            actions_gateway_proxy_tunnel_duration_seconds_bucket{le="3600"}[1h]
          ) -
          increase(
            actions_gateway_proxy_tunnel_duration_seconds_bucket{le="1800"}[1h]
          ) > 20
        for: 15m
        labels:
          severity: warning
        annotations:
          summary: "Unusually long proxy tunnels in {{ $labels.namespace }}"
          description: ">20 tunnels lasted 30m–1h in the last hour. GitHub long-polls are sticky but minutes-long; hour-long tunnels warrant inspection."

      # Ticket: dial-error spike — workers repeatedly hitting blocked
      # destinations (SSRF probing, or a misconfigured workload).
      - alert: ActionsGatewayProxyDialErrorSpike
        expr: |
          rate(actions_gateway_proxy_dial_errors_total[5m]) > 1
        for: 10m
        labels:
          severity: warning
        annotations:
          summary: "Proxy upstream dial errors spiking in {{ $labels.namespace }}"
          description: "Dial errors >1/s for 10m. The proxy only reaches GitHub CIDRs + DNS; a spike suggests a workload probing blocked destinations."

      # Page: token-refresh error spike can mean the GitHub App key was
      # revoked out-of-band — the expected first symptom of key compromise
      # response, or of an attacker's forged token being rejected.
      - alert: ActionsGatewayTokenRefreshAbuse
        expr: |
          increase(actions_gateway_token_refresh_errors_total[10m]) > 3
        for: 10m
        labels:
          severity: critical
        annotations:
          summary: "Token refresh failures in {{ $labels.namespace }}"
          description: "If no operator rotated the key, treat as possible key compromise. See runbook.md § GitHub App Key Compromise."

      # Page: unexpected jump in managed gateways — a compromised GMC
      # provisioning workloads, or runaway CR creation.
      - alert: ActionsGatewayManagedGatewaysJump
        expr: |
          increase(actions_gateway_managed_gateways[10m]) > 5
        labels:
          severity: critical
        annotations:
          summary: "Managed ActionsGateway count jumped"
          description: "More than 5 new ActionsGateway CRs in 10m. Confirm this matches an expected onboarding; otherwise inspect the GMC and CR audit trail."

      # Ticket: tenant quota pinned at 100% — resource-exhaustion DoS or a
      # genuinely undersized quota. ResourceQuota is the hard cap, so this
      # is contained, but sustained saturation is worth a look.
      - alert: ActionsGatewayQuotaExhausted
        expr: |
          kube_resourcequota{type="used"}
            / ignoring(type) kube_resourcequota{type="hard"} >= 1
        for: 30m
        labels:
          severity: warning
        annotations:
          summary: "Tenant ResourceQuota saturated in {{ $labels.namespace }}"
          description: "Quota at 100% for 30m. Distinguish legitimate demand (raise the platform-owned ResourceQuota on the namespace) from a job-flood (inspect workflow sources)."

      # Ticket: admission webhook rejecting CRs — a tenant repeatedly
      # probing reserved namespaces or invalid specs.
      - alert: ActionsGatewayWebhookRejections
        expr: |
          rate(controller_runtime_webhook_requests_total{code="403"}[10m]) > 0.1
        for: 15m
        labels:
          severity: warning
        annotations:
          summary: "Admission webhook rejecting ActionsGateway requests"
          description: "Sustained 403s from the validating webhook. Check which principal is submitting CRs to reserved namespaces or with invalid specs."
```

> **Note on labels.** The proxy metrics (`actions_gateway_proxy_*`) carry no
> intrinsic `namespace` label — each per-tenant proxy is a separate scrape
> target. The `{{ $labels.namespace }}` interpolation above resolves from
> the `namespace` label your `ServiceMonitor`/scrape config attaches to the
> target, not from the metric itself. If your scrape config does not add it,
> drop the interpolation.

---

## Audit-log abuse detections

The most dangerous abuse signals — a compromised AGC or GMC issuing RBAC
calls that are *permitted* but *anomalous* — are invisible to Prometheus.
The legitimate code paths avoid them (the AGC enumerates its Secrets
metadata-only per [H-2](../plan/security.md); the GMC reads Secret bodies
only during a reconcile via a cache-bypassing `Get`), so any of the calls
below originating from a controller ServiceAccount indicates the binary is
doing something its source does not.

Detecting these requires an **API-server audit policy** that logs the
relevant verbs at `Metadata` level or higher, shipped to a security
information and event management (SIEM) system or log-based alerting
backend. A ready-to-adapt sample policy ships at
[`examples/apiserver-audit-policy.yaml`](examples/apiserver-audit-policy.yaml);
[§ API server audit policy (sample)](#api-server-audit-policy-sample) below
covers installing it and reading the events. The table specifies *what to
alert on* once that policy is in place.

| Detection | Audit predicate | Why it matters | Response |
|---|---|---|---|
| **AGC full-body Secret list** | `verb=list resource=secrets` by the AGC ServiceAccount (`system:serviceaccount:<tenant-ns>:actions-gateway-controller`) returning object bodies | Legit AGC code lists Secret *metadata* only ([H-2 residual](../plan/security.md)). A body `list` means out-of-band enumeration of user-managed Secrets. | Treat the AGC as compromised: cordon the tenant namespace, rotate the GitHub App key (runbook.md § GitHub App Key Compromise), inspect the AGC image. |
| **AGC Secret access outside its label scope** | `verb=get resource=secrets` by the AGC SA for Secret names not matching `actions-gateway/runner-group=*` or the AGC's `gitHubAppRef` | The AGC only needs its agent-pool and payload Secrets. A `get` on a developer's `ghcr-pull-token` is exfiltration. | As above. |
| **GMC Secret reads beyond reconcile cadence** | `verb=get resource=secrets` by the GMC SA (`system:serviceaccount:gmc-system:gmc-controller-manager`) at a rate far above the reconcile/requeue cadence | The GMC reads each `gitHubAppRef` Secret only during reconcile (cache-bypassed `Get`). A high `get` rate is credential harvesting. | Treat the GMC as a Tier-0 compromise: isolate the GMC pod, rotate **all** tenant GitHub App keys, audit which Secrets were read. |
| **GMC namespace-PSA escalation attempt** | `namespace-psa-guard` ValidatingAdmissionPolicy `deny` events for the GMC SA | The guard ([§5.3](../design/05-security.md#53-security-profiles-and-the-privileged-opt-in)) blocks the GMC relabelling non-tenant namespaces (e.g. `kube-system` → `privileged`). A denial means the GMC tried. | A denial is a successful block, but a *signal* of compromise. Isolate the GMC and investigate. |
| **GMC out-of-tenant resource write** | `gmc-tenant-resource-guard` ValidatingAdmissionPolicy `deny` events for the GMC SA | The guard blocks the GMC creating/updating/deleting Deployments, RoleBindings, Secrets, NetworkPolicies, etc. in any namespace not marked `actions-gateway.github.com/tenant=true` (e.g. a Deployment or Secret into `kube-system`). A denial means the GMC tried (Q121/Q122). | A denial is a successful block but a *signal* of compromise. Isolate the GMC and investigate. |
| **GMC workload creation outside reconcile** | `verb=create resource=deployments|roles|rolebindings` by the GMC SA in a *marked tenant* namespace with no corresponding `ActionsGateway` CR change | The `gmc-tenant-resource-guard` VAP already blocks writes into *unmarked* namespaces; this catches the residual — provisioning inside a legitimate tenant namespace without a triggering CR edit, which is lateral movement within the GMC's confined scope. | Isolate the GMC; diff provisioned resources against live `ActionsGateway` CRs. |

Without the audit policy, these threats are mitigated structurally
(RBAC scope, cache-bypass, the `namespace-psa-guard` and
`gmc-tenant-resource-guard` VAPs, no Secret informer) but write-confinement
denials aside are **not observable** — there is no alert that fires if a
compromised binary exercises its standing *read* permissions. Closing that
gap is the value of the audit policy. Note the two VAPs confine GMC *writes*
(create/update/delete) only; Secret *reads* (`get`/`list`/`watch`) cannot be
gated at admission and remain cluster-wide at the RBAC layer — the audit
policy is the only detective control for them (see
[§5.1](../design/05-security.md#51-gmc-level-threats-cluster-scoped)).

### API server audit policy (sample)

A sample audit policy that captures exactly the verbs the table above alerts
on — and nothing else — ships at
[`examples/apiserver-audit-policy.yaml`](examples/apiserver-audit-policy.yaml).

**What it detects.** Three focused rule groups, all at `Metadata` level:

1. **GMC Secret reads** (`get`/`list`/`watch secrets`) by the GMC
   ServiceAccount, cluster-wide — surfaces credential harvesting and any
   read outside the GMC's tenant namespaces.
2. **GMC out-of-tenant write attempts** (`create`/`update`/`patch`/`delete`)
   on the kinds the `gmc-tenant-resource-guard` and `namespace-psa-guard`
   VAPs confine — a `403` `responseStatus` is a successful block but a signal
   the binary tried.
3. **AGC Secret reads** (`get`/`list`/`watch secrets`) by each AGC
   ServiceAccount — surfaces the full-body `list` and out-of-scope `get` the
   legitimate metadata-only code path never makes.

**Why `Metadata`, not `RequestResponse`.** A Secret `get`/`list` *response*
body contains `.data` — the GitHub App private keys this control protects.
Logging at `RequestResponse` would copy that key material into the audit
backend, creating a second exfiltration surface. `Metadata` records the
requester, verb, resource, name, namespace, timestamp, and response code —
enough to detect an anomalous read without duplicating the secret. Keep the
Secret rules at `Metadata`.

**Before you install,** edit the placeholders (the file's header comment lists
them): the GMC ServiceAccount user string if you overrode the install
namespace or `namePrefix`, and one `users:` entry per tenant namespace for the
AGC rule (the audit `users:` field is an exact match with no wildcard).

#### Where auto-install is — and isn't — possible

The policy is a **static file `kube-apiserver` reads at startup**, not a cluster
object: there is no `kubectl apply` for it, and the Helm chart cannot install it
(it deploys workloads, not control-plane node files). Full installation is
therefore only possible where **you control the API-server flags** — a cluster
you provision (kind, kubeadm). On a managed control plane (EKS / GKE / AKS) the
provider owns those flags and ships a *fixed* audit configuration to its own log
sink; you cannot supply this file, so the path is to enable the provider's audit
logging and translate the same predicates against the managed stream. Both are
covered below.

#### Self-managed: cluster you provision (auto)

If you are creating the cluster, bake the policy into `kube-apiserver` from the
start — no static-pod surgery. The
[`examples/kind-cluster-audit.yaml`](examples/kind-cluster-audit.yaml) kind
config does this via `extraMounts` + a `ClusterConfiguration` audit patch; the
same `apiServer.extraArgs` / `extraVolumes` block works in any kubeadm
`ClusterConfiguration` (`kubeadm init --config`).

#### Self-managed: existing kubeadm cluster (scripted)

For a cluster already running, the policy file must be placed on each
control-plane node and the `kube-apiserver` static-pod manifest patched to add
the audit flags and mounts.
[`examples/install-apiserver-audit-policy.sh`](examples/install-apiserver-audit-policy.sh)
automates exactly that — run it **once per control-plane node, as root, on the
node**:

```bash
sudo ./install-apiserver-audit-policy.sh        # --dry-run to preview first
```

It validates the policy, installs it to `/etc/kubernetes/audit/policy.yaml`, and
idempotently patches `/etc/kubernetes/manifests/kube-apiserver.yaml` (timestamped
backup; `yq` for the structured edit so the manifest cannot be corrupted). The
kubelet restarts the API server automatically. To do it by hand instead, add the
`--audit-policy-file` / `--audit-log-*` flags and the `audit-policy` +
`audit-log` `volumeMounts`/`volumes` to the manifest — the script's
[kind config](examples/kind-cluster-audit.yaml) shows the exact shape. Use
`--audit-log-path=-` to emit to stdout (e.g. to ship via a log agent) instead of
a file. Forward the log to your SIEM and translate the table's predicates into
alert rules there.

#### Managed control planes (EKS / GKE / AKS)

You **cannot** supply a custom `--audit-policy-file` on a managed control plane —
the provider owns the API-server flags. Instead, enable the provider's
control-plane audit logging and apply the **same predicates** (requester
`user.username` / principal, `verb`, Secret resource, VAP `403` denials) as
filters/alerts against the managed log stream. The detection logic is identical;
only the substrate differs. The provider's default policy is broader than this
sample (it logs more than the controller ServiceAccounts) — scope your queries to
the controller identities below.

- **Amazon EKS.** Enable the **`audit`** control-plane log type on the cluster
  (`aws eks update-cluster-config --logging`, or the console/IaC equivalent).
  Events land in CloudWatch log group `/aws/eks/<cluster>/cluster`, streams
  `kube-apiserver-audit-*`; EKS's fixed policy logs Secret access at `Metadata`.
  Query with CloudWatch Logs Insights:

  ```
  fields @timestamp, user.username, verb, objectRef.namespace, objectRef.name, responseStatus.code
  | filter @logStream like /kube-apiserver-audit/
  | filter objectRef.resource = "secrets"
      and user.username = "system:serviceaccount:gmc-system:gmc-controller-manager"
  | filter verb in ["list","watch"] or objectRef.namespace != "<a-tenant-namespace>"
  ```

- **Google GKE.** API-server **write** events are Admin Activity audit logs
  (always on). Secret **reads** are **Data Access** logs, which are **off by
  default** — enable `DATA_READ`/`ADMIN_READ` for the Kubernetes Engine API
  (Cloud Console → IAM → Audit Logs, or IaC). Query in Logs Explorer; the
  Kubernetes verb is encoded in `protoPayload.methodName`
  (`io.k8s.core.v1.secrets.get` / `.list` / `.watch`) and the caller in
  `protoPayload.authenticationInfo.principalEmail`:

  ```
  resource.type="k8s_cluster"
  protoPayload.methodName=~"io.k8s.core.v1.secrets.(get|list|watch)"
  protoPayload.authenticationInfo.principalEmail="system:serviceaccount:gmc-system:gmc-controller-manager"
  ```

- **Azure AKS.** Add a cluster **diagnostic setting** forwarding the
  **`kube-audit`** category (to Log Analytics, a storage account, or Event Hub).
  Use `kube-audit`, **not** `kube-audit-admin`: the `-admin` variant drops
  `get`/`list` events, so it cannot see Secret reads — the very signal this
  policy exists for. Query with KQL (the event is JSON in the `log_s` column):

  ```kusto
  AzureDiagnostics
  | where Category == "kube-audit"
  | extend e = parse_json(log_s)
  | where e.objectRef.resource == "secrets"
      and e.user.username == "system:serviceaccount:gmc-system:gmc-controller-manager"
  | where e.verb in ("list","watch") or e.objectRef.namespace != "<a-tenant-namespace>"
  ```

  Repeat the per-provider queries for each AGC ServiceAccount
  (`system:serviceaccount:<tenant-ns>:actions-gateway-controller`), alerting on
  `verb == "list"` or a `get` on a Secret the AGC does not own.

**Read the events.** Audit events are one JSON object per line. To see GMC
Secret reads (substitute your GMC user string):

```bash
jq -c 'select(.user.username == "system:serviceaccount:gmc-system:gmc-controller-manager"
        and .objectRef.resource == "secrets")
       | {stage, verb, ns: .objectRef.namespace, name: .objectRef.name,
          code: .responseStatus.code, t: .requestReceivedTimestamp}' \
  /var/log/kubernetes/audit/audit.log
```

A baseline is one `get` per `gitHubAppRef` Secret per reconcile, only in
tenant namespaces. Alert on: any `list`/`watch`; a `get` rate well above the
reconcile cadence; or any `get` outside a tenant namespace (e.g.
`kube-system`). For the GMC write rule, a `responseStatus.code` of `403`
is a VAP block — investigate the binary that attempted it. For AGC reads,
filter by each AGC user string and alert on `verb == "list"` (the legit path
is metadata-only) or a `get` on a Secret name the AGC does not own.

---

## Response playbooks

For the full credential-rotation procedure see
[runbook.md § GitHub App Key Compromise](runbook.md#github-app-key-compromise).
The abuse-specific first moves:

### Suspected compromised AGC (tenant-scoped)

1. **Contain.** Scale the AGC to zero so it stops acting:
   `kubectl scale deploy/actions-gateway-controller -n <namespace> --replicas=0`.
   In-flight jobs will be cancelled by GitHub when `renewjob` lapses; this
   is acceptable during a suspected breach.
2. **Rotate.** Rotate the tenant's GitHub App key
   ([runbook.md § GitHub App Key Compromise](runbook.md#github-app-key-compromise)) —
   the AGC held it in memory.
3. **Scope.** Check the API-server audit log for Secret `get`/`list` calls
   by the AGC ServiceAccount; enumerate which tenant Secrets may have been
   read.
4. **Verify the image.** Confirm the running AGC image digest matches the
   GMC-pinned `AGC_IMAGE` (digest pinning is enforced — see
   [§5.2 Supply-Chain](../design/05-security.md#52-agc--proxy-level-threats-namespace-scoped)).

### Suspected compromised GMC (cluster-scoped, Tier-0)

1. **Contain.** Scale the GMC to zero:
   `kubectl scale deploy/gmc-controller-manager -n gmc-system --replicas=0`.
   Existing tenant gateways keep running (the GMC is not in the data path —
   see [runbook.md § GMC Total Failure](runbook.md#gmc-total-failure)); only
   provisioning and reconcile pause.
2. **Rotate everything.** A compromised GMC can read every tenant's
   `gitHubAppRef` Secret. Rotate **all** tenant GitHub App keys.
3. **Scope.** Audit GMC ServiceAccount `get secrets` and
   `create`/`patch` calls; reconcile provisioned resources against live
   `ActionsGateway` CRs to find anything created off-CR.
4. **Verify the image.** Confirm the GMC image digest against the deployed
   manifest before scaling back up.

### Proxy saturation / slowloris

1. **Confirm the shape.** Long-lived tunnels in the
   `proxy_tunnel_duration_seconds` top bucket + pinned
   `proxy_connections_active` ⇒ slowloris. Spread across many short tunnels
   ⇒ genuine burst (let the HPA absorb it).
2. **Identify the source.** The proxy serves one tenant; the offending
   tunnels originate from worker pods in that tenant namespace. Inspect
   recent worker pods for the responsible workflow.
3. **Mitigate.** The proxy enforces a per-read idle deadline (5m) and a 6h
   absolute lifetime cap (M-18), so hung tunnels self-terminate. If a single
   workflow is the culprit, cancel its run in the GitHub Actions UI; the
   worker pod and its tunnels are released on job completion.

---

## Posture scanning (preventive)

The detections above catch abuse at runtime. Two scanners catch posture
regressions *before* they reach a cluster — one in CI on every chart change,
one a pre-production manual step against the live cluster.

If your cluster runs a policy engine (Kyverno / OPA Gatekeeper), see
[admission-policies.md](admission-policies.md) for whether GAG pods comply with
common admission policies and for sample policies that *enforce* GAG's posture
at admission time.

### Manifest posture — polaris (automated, in CI)

[polaris](https://polaris.docs.fairwinds.com/) audits the Kubernetes
security/best-practice posture of the **shipped install artifact**: the CI
`polaris` job (in [`.github/workflows/security-scan.yml`](../../.github/workflows/security-scan.yml))
renders the [Helm chart](../../charts/actions-gateway) and checks the rendered
manifests. It runs on every PR that touches the chart or the `Makefile`, and on
every push to `main`.

- **What it gates.** The scan fails the PR on any `danger` finding — a
  privileged container, a host namespace, dangerous capabilities, a missing
  `securityContext`, a floating `:latest` image tag, and similar real
  regressions. A change that weakens the chart's hardened defaults cannot merge.
- **What it reports but does not block.** `warning`-level findings are printed
  for visibility. The handful that are false positives against a Helm-packaged
  operator chart (the controller's required ServiceAccount-token automount, the
  cross-document NetworkPolicy match polaris can't resolve statically, the
  `IfNotPresent` pull policy that is correct for a digest-pinned image, and
  Helm's `app.kubernetes.io/instance` labelling) are tuned to `ignore` in
  [`charts/actions-gateway/polaris.yaml`](../../charts/actions-gateway/polaris.yaml),
  each with a justifying comment. **Never relax a `danger` check to silence a
  finding — fix the chart instead** (secure-by-default).
- **Run it yourself.** `make polaris-scan` (needs `helm` and `polaris` on
  `PATH`) runs the exact CI gate locally. It renders with a placeholder image
  digest so the audit reflects the production, digest-pinned posture — a digest
  is also required for the chart to render at all (an empty `gmc.image.digest`
  fails the render; `make manifest-validate` asserts that rejection), so the
  placeholder cannot mask a fail-open default.

> The scan audits *workload posture in the generated manifests*. It does not
> replace pinning real image digests (`gmc.image.digest`,
> `agc.image.digest`, `proxy.image.digest` in `values.yaml`) at install time —
> see [tenant-onboarding.md](tenant-onboarding.md) and the chart README.

### CIS-benchmark posture — kube-bench (manual, pre-production)

polaris scans our *manifests*; it cannot see how the *cluster itself* is
configured (kubelet flags, API-server settings, etcd permissions, control-plane
file modes). Those are the province of the
[CIS Kubernetes Benchmark](https://www.cisecurity.org/benchmark/kubernetes),
which [kube-bench](https://github.com/aquasecurity/kube-bench) checks against a
**live node** — so it cannot run in our manifest-only CI and is instead a
pre-production checklist item the cluster operator runs once per cluster (and
after any control-plane upgrade).

Run it as a Job on the cluster you are about to onboard tenants onto:

```bash
# Runs kube-bench on every node via the upstream Job manifest, then collects
# the report. Requires cluster-admin. Pin to a released tag, not main.
kubectl apply -f https://raw.githubusercontent.com/aquasecurity/kube-bench/v0.10.7/job.yaml
kubectl wait --for=condition=complete job/kube-bench --timeout=120s
kubectl logs job/kube-bench
kubectl delete job kube-bench
```

Triage the report against this operator's needs:

- **`[FAIL]` on control-plane / kubelet hardening** (e.g. `--anonymous-auth=false`,
  `--authorization-mode` not `AlwaysAllow`, read-only etcd data dir,
  `--protect-kernel-defaults=true`) — fix at the cluster layer before
  onboarding. These are cluster-admin remediations, not chart settings; managed
  control planes (EKS/GKE/AKS) pass most of them by default and expose the rest
  as cluster config.
- **NetworkPolicy / PodSecurity benchmark items** — this operator already
  satisfies the workload half: the chart ships GMC NetworkPolicies
  (`networkPolicy.enabled=true`) and the GMC stamps Pod Security Admission
  labels per tenant `securityProfile`. Confirm the cluster has a
  NetworkPolicy-enforcing CNI (Calico/Cilium; kindnet does **not** enforce) and
  the `PodSecurity` admission plugin enabled, or those controls are inert. To
  prove enforcement on a live cluster, run the negative probes in
  [network-architecture.md § How to Validate Network Isolation](../design/network-architecture.md#how-to-validate-network-isolation) —
  the "blocked" probes must actually time out (validated under Calico on a
  kind cluster, Q7b 2026-06-11).
- **Cluster DNS must be labelled `k8s-app=kube-dns` in `kube-system`.** Tenant
  NetworkPolicies confine port-53 egress to the cluster DNS service rather than
  leaving DNS open to any resolver (Q105 — an open DNS path is an unattributed
  exfiltration side-channel). The selector matches the conventional CoreDNS
  deployment: pods labelled `k8s-app: kube-dns` in the `kube-system` namespace
  (matched via the immutable `kubernetes.io/metadata.name` namespace label). This
  is the default on every mainstream distribution and managed control plane. If
  your cluster runs DNS under a different label or namespace **and** uses an
  enforcing CNI, tenant pods will fail to resolve any name until you either
  relabel the DNS pods or set `spec.proxy.managedNetworkPolicy: false` and supply
  your own DNS egress rule. Symptom: tenant workloads time out on every lookup
  while non-DNS connectivity is unaffected.
- **NodeLocal DNSCache (`node-local-dns`) is supported.** With node-local-dns,
  pods send queries to a link-local IP (default `169.254.20.10`) served by a
  `hostNetwork` `node-local-dns` pod, not to a `k8s-app: kube-dns` CoreDNS pod —
  which the kube-dns podSelector cannot match. The tenant NetworkPolicies
  therefore allow port-53 egress to the link-local block `169.254.0.0/16` as a
  second peer alongside the kube-dns selector (Q136), so both topologies resolve
  out of the box with no operator action. Link-local is non-routable and
  node-scoped, so this preserves the no-arbitrary-resolver property of Q105 —
  the link-local block cannot reach an external resolver. If your node-local-dns
  cache listens on a non-default address *outside* `169.254.0.0/16`, set
  `spec.proxy.managedNetworkPolicy: false` and supply your own DNS egress rule,
  or add an additive NetworkPolicy — see
  [Tenant egress posture & deliberate widening](#tenant-egress-posture--deliberate-widening).
- **Findings that don't apply** (managed control plane hides the file, a check
  for a component you don't run) — record the justification alongside the
  cluster's onboarding ticket.

The goal is **zero critical (`[FAIL]`) findings that this stack depends on**
before the first production tenant (per
[milestone-5.md](../plan/milestone-5.md) §3).

## Tenant egress posture & deliberate widening

**The secure default is controller-managed and not opt-in.** For every tenant,
the GMC reconciles three NetworkPolicies that confine worker (and AGC) egress to
exactly what the design requires: DNS to the cluster DNS service only, and all
GitHub-bound traffic through the per-tenant egress proxy (whose source IPs are
attributable). Worker pods cannot reach arbitrary destinations directly — that is
the per-tenant egress-IP isolation property, and it is present automatically the
moment a tenant is provisioned. Do **not** hand-edit the GMC-managed policies
(`actions-gateway-workload`, `actions-gateway-controller`, `actions-gateway-proxy`):
the controller reconciles them back, and the proxy policy's GitHub-CIDR rule is
refreshed from `api.github.com/meta` every 24h, so a hand-edit would be reverted
or go stale. See [network-architecture.md](../design/network-architecture.md#networkpolicy-rules)
for the full policy set.

> **Running a service mesh?** A mesh sidecar transparently intercepts the
> worker's outbound TCP and can re-route GitHub-bound traffic through a mesh
> egress gateway, silently bypassing the per-tenant proxy and dropping the
> egress-IP attribution this isolation property rests on. See
> [Running GAG Alongside a Service Mesh](service-mesh-coexistence.md) for the
> injection opt-out and egress exclusions that keep GAG's proxy path authoritative.

Some jobs legitimately need egress the proxy cannot carry — the CONNECT proxy
tunnels HTTP/HTTPS to GitHub CIDRs only, so a non-HTTP protocol (a database, SSH,
a raw TCP/UDP service), an internal artifact store or package mirror, or a
specific custom DNS resolver is unreachable by default. **Grant that egress with
an *additional*, additive NetworkPolicy in the tenant namespace — not by relaxing
the managed defaults.** NetworkPolicies are additive (a union of allows), so an
extra policy widens egress for the pods it selects without touching the floor.

Worker pods carry two selectable labels, so you can target all workers or a single
runner type:

- `actions-gateway/component: workload` — every worker (and the AGC) in the tenant
- `actions-gateway/runner-group: <name>` — workers of one specific RunnerGroup

```yaml
# Applied by a platform admin (requires NetworkPolicy write in the tenant
# namespace) — grants ONE runner type extra egress. CIDR + port + protocol.
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: gpu-builders-extra-egress
  namespace: team-a
spec:
  podSelector:
    matchLabels:
      actions-gateway/component: workload
      actions-gateway/runner-group: gpu-builders   # omit this line for tenant-wide
  policyTypes: [Egress]
  egress:
    - to:
        - ipBlock: {cidr: 10.50.0.0/24}            # internal registry / artifact store
      ports:
        - {protocol: TCP, port: 443}
        - {protocol: TCP, port: 5432}              # e.g. Postgres
    - to:
        - ipBlock: {cidr: 10.50.0.53/32}           # custom DNS resolver
      ports:
        - {protocol: UDP, port: 53}
        - {protocol: TCP, port: 53}
```

**This is a deliberate, documented trade-off, not a routine knob.** Egress to the
listed destinations leaves with the worker's own pod IP and therefore **bypasses
the per-tenant proxy egress-IP attribution** for those flows. Untrusted job code
(e.g. fork-PR workflows) can use any hole you open, so:

- Keep the allowlist as narrow as the use case requires — specific CIDRs and
  ports, never a `0.0.0.0/0` catch-all.
- Authoring it requires namespace NetworkPolicy-write, so it is inherently a
  platform/admin decision — which is the correct authority for relaxing
  attribution. Track each grant in the tenant's onboarding ticket.
- **For a custom DNS resolver specifically, prefer a cluster-level CoreDNS
  `forward` zone** over reopening worker DNS: that keeps resolution on the
  attributable in-cluster path while still resolving the names you need.

If instead you want to express the **proxy's own** GitHub egress as FQDN rules under
a DNS-aware CNI, you have two supported options:

- **First-class, GMC-managed (recommended on a v2 `EgressProxy`)** — set
  `spec.egressPolicyMode: CiliumFQDN` (or `CalicoFQDN`) and the GMC emits the
  CNI-native FQDN policy for you, keeping it owned and reconciled. See
  [Expressing GitHub egress by FQDN](#expressing-github-egress-by-fqdn-the-egresspolicymode-opt-in)
  below.
- **Full hand-off** — set `spec.proxy.managedNetworkPolicy: false` (v1
  `ActionsGateway`) or `spec.managedNetworkPolicy: false` (v2 `EgressProxy`): the GMC
  stops managing the proxy egress policy entirely and you own it. Use this when you
  need a shape the managed FQDN mode does not emit.

The managed CIDR path remains the default in all cases.

### Managing egress at scale

This project deliberately does **not** ship tooling to manage the *widening*
policies — that is a cluster/platform concern with a mature ecosystem, and owning
it here would re-create the coupling the managed-floor split avoids. What the
project commits to instead is a stable **integration surface**: every worker pod
carries two labels you can target from any policy engine, and these are a
supported contract (they will not be renamed without a migration note):

- `actions-gateway/component: workload` — all worker (and AGC) pods in the tenant
- `actions-gateway/runner-group: <name>` — workers of one specific RunnerGroup

For anything beyond a handful of static CIDRs, prefer the ecosystem over
hand-written `NetworkPolicy`:

- **Your CNI's richer egress** — `CiliumNetworkPolicy` `toFQDNs` (DNS-aware,
  hostname allowlists), Calico `NetworkSet` (reusable CIDR groups) / DNS policy,
  and policy tiers. This is the right tool for "let `gpu-builders` reach
  `*.internal.corp` and a database." It pairs with the
  `spec.proxy.managedNetworkPolicy: false` hand-off above.
- **`AdminNetworkPolicy`** ([sig-network `network-policy-api`](https://network-policy-api.sigs.k8s.io/))
  — cluster-admin-level, cross-namespace egress baselines (`AdminNetworkPolicy` /
  `BaselineAdminNetworkPolicy`), implemented by Cilium/Calico/OVN-Kubernetes. The
  most direct fit for "platform admin governs egress across all tenant
  namespaces" — maturing (alpha→beta), so confirm your CNI's support level.
- **Kyverno / OPA Gatekeeper** — policy-as-code to *generate* per-namespace NPs
  (e.g. a templated default-deny or egress allowance keyed off a namespace label)
  and to *validate* that any admin-added egress conforms to your guardrails.

The labels above are what make all of these targetable; the secure floor stays
GMC-managed regardless.

### Expressing GitHub egress by FQDN: the `egressPolicyMode` opt-in

By default the GMC expresses the proxy pool's GitHub allowlist as **IP CIDR ranges**,
refreshed from `api.github.com/meta` every 24h (`egressPolicyMode: CIDR`). This works
on every NetworkPolicy-enforcing CNI and needs no DNS awareness. If you run a CNI that
enforces DNS-based egress, you can instead have the GMC emit a **CNI-native FQDN
policy** scoped to the GitHub hostnames — no CIDR feed to keep current. This is an
opt-in on the v2 `EgressProxy`:

```yaml
apiVersion: actions-gateway.com/v2alpha1
kind: EgressProxy
metadata:
  name: shared
  namespace: team-a
spec:
  egressPolicyMode: CiliumFQDN   # or CalicoFQDN; default is CIDR
```

When set to `CiliumFQDN` the GMC reconciles a `CiliumNetworkPolicy` (`cilium.io/v2`)
with `toFQDNs` rules; `CalicoFQDN` reconciles a Calico `NetworkPolicy`
(`projectcalico.org/v3`) with destination `domains`. Both are named `<proxy>-proxy-fqdn`,
owned by the `EgressProxy` (garbage-collected with it), and cover the GitHub Actions
runner endpoint families: `api.github.com`, `github.com`, `codeload.github.com`,
`objects.githubusercontent.com`, `*.actions.githubusercontent.com`, and
`*.blob.core.windows.net`. In an FQDN mode the standard `NetworkPolicy` drops its
GitHub-CIDR rule (DNS + ingress are unchanged) and the 24h IP-range reconcile skips
this proxy.

**Prerequisites — this is the operator's responsibility:**

| Mode | Requires | If the CNI cannot enforce it |
| --- | --- | --- |
| `CiliumFQDN` | Cilium with DNS-aware policy (`toFQDNs`), and the `CiliumNetworkPolicy` CRD installed | The GMC's apply fails loudly and the `EgressProxy` goes `Degraded`. GitHub egress stays **denied** (the standard NetworkPolicy default-denies it), never silently opened. |
| `CalicoFQDN` | Calico with DNS-based policy enabled, and the `projectcalico.org/v3` `NetworkPolicy` CRD installed | As above — fail-closed. |

**Secure-by-default guarantee.** Selecting an FQDN mode can never weaken egress: the
standard NetworkPolicy still default-denies GitHub egress, so if the CNI-native policy
is absent or unenforced, GitHub egress is denied rather than wide-open. Confirm your
CNI actually enforces DNS-based egress before relying on it (see the
[network isolation validation](../design/network-architecture.md#how-to-validate-network-isolation)
procedure). `egressPolicyMode` has no effect when `managedNetworkPolicy: false`.

> FQDN mode is currently a v2 `EgressProxy` feature (the shared-egress surface). The v1
> `ActionsGateway` proxy and v2 direct-egress (proxy-less) gateways stay on the CIDR
> path; if you need FQDN egress there, use the `managedNetworkPolicy: false` hand-off
> above.

---

## Tightening AGC apiserver egress: the `apiserver-cidrs` allowlist

The AGC pod holds the tenant's GitHub App private key and is the only workload
that needs the Kubernetes API server. The GMC therefore reconciles a NetworkPolicy
(`actions-gateway-controller`) admitting AGC egress on the apiserver ports **443
and 6443**. By default that rule has **no destination restriction** (any-dest):
kube-proxy DNATs the `kubernetes` Service ClusterIP to a provider-specific
apiserver IP *before* NetworkPolicy is evaluated, so a precise `ipBlock` is not
portable and a wrong one silently severs the AGC's apiserver access (the PR #59
post-DNAT trap). That breadth is a documented residual
([§5.2](../design/05-security.md#52-agc--proxy-level-threats-namespace-scoped)):
a compromised AGC could in principle reach an arbitrary external HTTPS endpoint
on 443, not just the apiserver.

**If your platform exposes the apiserver on a *stable* CIDR**, you can close that
residual by scoping the rule to it. Set the chart value `apiServerCIDRs` (passed
to the GMC as `--apiserver-cidrs`) to the apiserver CIDR set:

```yaml
# values.yaml — opt-in tightening of the AGC apiserver-egress rule.
apiServerCIDRs:
  - "10.0.0.1/32"        # the apiserver's post-DNAT endpoint (single IP)
  - "172.16.0.0/12"      # or a CIDR covering the control-plane node pool
```

When set, the GMC scopes every tenant's AGC NetworkPolicy 443/6443 rule to these
CIDRs via `ipBlock` peers (ports unchanged); when **empty (the default)** the rule
stays any-destination — no behavior change. This is an **opt-in tightening, never
a loosening**, applied cluster-wide (the apiserver is the same endpoint for every
tenant). The GMC **validates each entry as a CIDR at startup and refuses to start
on a malformed one**, so a typo fails fast rather than reconciling a NetworkPolicy
the apiserver rejects.

### The one verification every platform shares

The value must cover the destination the AGC's apiserver traffic carries
**after** kube-proxy DNAT — **not** the `kubernetes` Service ClusterIP (that
virtual IP is DNAT'd away *before* NetworkPolicy is evaluated, so an `ipBlock`
naming the ClusterIP matches nothing and severs apiserver access). The post-DNAT
destinations are exactly the **endpoints** of the `kubernetes` Service in the
`default` namespace. On every platform, confirm them before you scope:

```bash
kubectl get endpoints kubernetes -n default -o wide
# NAME         ENDPOINTS                          AGE
# kubernetes   10.0.12.34:443,10.0.34.56:443      90d
```

Those `ENDPOINTS` IP:port pairs are the literal targets the policy evaluator
matches against. Your `apiServerCIDRs` must contain a CIDR covering **all** of
them, and **stay** covering them as the platform rotates. Below is how to find a
*stable* covering CIDR per platform — and where no stable CIDR exists, so you
must leave the default.

### Finding the apiserver CIDR, by platform

| Platform | Where the apiserver lives after DNAT | Stable CIDR to scope to | Safe to scope? |
|---|---|---|---|
| **kind** | The control-plane container's IP on the kind Docker network, on **6443**. | The kind network subnet, e.g. `172.18.0.0/16` (`docker network inspect kind -f '{{(index .IPAM.Config 0).Subnet}}'`), or a `/32` per control-plane node. | **Yes** — local, you own the IPs. (Note kindnet does not *enforce* NetworkPolicy; scope it on a Calico/Cilium kind.) |
| **kubeadm / self-managed** | Control-plane node IP(s), on **6443**. | A `/32` per control-plane node, or the control-plane node subnet CIDR. For HA, list **every** control-plane node IP — a missed one strands the AGC whenever it lands on that apiserver. | **Yes**, if control-plane node IPs are static (the usual case). Re-confirm after any control-plane add/replace. |
| **Amazon EKS** | With **private endpoint access**, managed elastic network interfaces (ENIs) in *your* VPC subnets — private IPs you control. With **public-only** access, an AWS-managed public IP that AWS does **not** publish and may change. | The VPC CIDR, or the specific subnet CIDRs that host the cluster ENIs (`aws ec2 describe-subnets`). Verify against `kubectl get endpoints kubernetes`. | **Yes** for private-endpoint clusters. **No** for public-only — leave the default. |
| **Google GKE** | For **private clusters**, the control-plane private endpoint in the Google-managed master CIDR block (a `/28` you set at cluster creation). Public clusters use a Google-managed public IP that can change. | The master `/28`: `gcloud container clusters describe <c> --format='value(privateClusterConfig.masterIpv4CidrBlock)'`. | **Yes** for private clusters. **No** for public endpoint — leave the default. |
| **Azure AKS** | For **private clusters**, a private IP in your VNet (resolved via the cluster's private DNS zone). Public AKS uses an Azure-managed public IP behind the apiserver FQDN that may change. | The AKS node subnet CIDR (or the resolved private-endpoint IP as a `/32`); confirm with `kubectl get endpoints kubernetes`. | **Yes** for private clusters. **No** for public endpoint — leave the default. |

Rule of thumb: **a managed control plane is only safely scopable when you have
put its endpoint inside a network range you own** (private/VPC/VNet access). A
public managed endpoint is a moving, provider-owned IP — scoping to a guessed
range will eventually break, so keep the any-destination default there.

**The cost of getting it wrong.** A CIDR that is too narrow (or that the
platform rotates out from under you) **breaks the AGC's apiserver access** — the
AGC can no longer acquire jobs or manage worker pods for that tenant. Symptom:
AGC logs show apiserver dial timeouts after a rollout that introduced or changed
`apiServerCIDRs`. **Remedy: widen the CIDR or clear `apiServerCIDRs` to restore
the any-destination default.** Treat this as any egress tightening — validate on
one cluster before fleet-wide rollout, and re-confirm after control-plane
scaling, upgrades, or IP changes.

Leave `apiServerCIDRs` unset unless you have a confirmed, stable apiserver CIDR —
the any-destination default is bounded by the §5.2 compensating controls (key
mounted read-only, never an env var; workers carry no apiserver egress at all;
digest-pinned non-root AGC; all GitHub-bound traffic still through the proxy).

### Why GAG can't discover and tighten this for you (feasibility verdict)

A natural question is whether the GMC should just **read the `kubernetes`
endpoints itself and scope every AGC NetworkPolicy automatically**, making the
tightening the default. We reviewed this for Q183 and **deliberately did not**,
because an auto-tightened default would silently regress apiserver reachability
on common platforms:

- **The IPs rotate, and a snapshot goes stale.** On managed control planes the
  endpoint IPs change on scaling, upgrades, and maintenance — without notice.
  A one-time discovery at provisioning time would tighten to a set that later
  stops matching, breaking the AGC.
- **Keeping it live needs a watch — with a lockout failure mode.** Staying
  correct means watching `endpoints/kubernetes` and re-reconciling every
  tenant's AGC policy on each change. There is always a race window where the
  policy lags a real IP rotation; during it the AGC (and potentially the GMC,
  which reaches the apiserver over the same path) is locked out of the very
  apiserver it needs to *repair* the policy — a self-inflicted control-plane
  deadlock. A tightening that can strand the controller maintaining it is not a
  safe default.
- **CNI rewrites can move the target again.** Some CNIs apply further
  SNAT/encapsulation, so even the endpoint IPs are not guaranteed to be what the
  policy evaluator ultimately matches — another reason a portable automatic
  value does not exist.

**Verdict:** automatic, default-on tightening is **not safe or portable**, so
the any-destination rule stays the secure default and narrowing remains an
operator-confirmed, per-cluster opt-in (the `apiServerCIDRs` allowlist above).
This honours secure-by-default without silently regressing reachability: the
operator who *can* verify a stable CIDR closes the residual, and everyone else
keeps a working AGC. A controller-driven endpoint-watch that auto-narrows is
recorded as a future enhancement gated on solving the rotation/lockout failure
mode — see [appendix-g §G.10](../design/appendix-g-future-enhancements.md#g10-controller-discovered-apiserver-cidr-auto-narrowing).

---

## GitHub API base URL must be HTTPS

The AGC mints GitHub App installation access tokens by POSTing a signed App-JWT
to the GitHub REST API and reading back a short-lived installation token. Both
the JWT (signed with the tenant's private key) and the returned token are
credential material, so this exchange must never traverse a plaintext channel.

The endpoint host is taken from the **`GITHUB_API_BASE_URL`** environment
variable, defaulting to `https://api.github.com` when unset. The token provider
**rejects a non-HTTPS `GITHUB_API_BASE_URL` at startup** — the AGC (and the
`probe`) will refuse to start with a clear error rather than leak credentials on
the first token mint:

```
githubapp: refusing non-HTTPS GITHUB_API_BASE_URL "http://…":
GitHub App token exchange must use HTTPS to protect credentials in transit;
plaintext is permitted only under an explicit dev/test opt-in
```

This is **secure-by-default**: HTTPS is required with no configuration, an
HTTPS value (including a GitHub Enterprise Server base such as
`https://ghe.example.com/api/v3`) and the unset default both work, and the error
names the offending URL but never any token or JWT material.

**Operator action:** if you set `GITHUB_API_BASE_URL` (e.g. for GitHub
Enterprise Server), it must begin with `https://`. A plaintext value will block
startup. Do not work around this by editing the deployment to inject a stub
signal — the plaintext path exists only for the project's own in-cluster test
fixtures.

**Documented dev/test trade-off.** The e2e suite points the AGC at an in-cluster
`fakegithub` over plaintext (`http://<svc>.<ns>.svc.cluster.local:<port>`). That
path is permitted only by an explicit opt-in that production never carries: the
AGC allows a plaintext base URL **only when the stub env `STUB_AUTH_URL` is
set**, which a GMC-provisioned AGC receives solely via `AGC_EXTRA_*` under the
testing-only `--allow-agc-extra-env` GMC flag. When the opt-in is active the AGC
logs `dev/test mode: allowing non-HTTPS GITHUB_API_BASE_URL for token exchange`
at startup, so the relaxation is visible in the logs. A production AGC has no
stub env and therefore always enforces HTTPS.

---

## Priority classes: the `allowed-priority-classes` allowlist

A tenant `RunnerGroup` can request scheduling priority for its worker pods via
`priorityTiers[].priorityClassName`, which the AGC stamps onto the pods as
`spec.priorityClassName`. `PriorityClass` is a **cluster-scoped** object carrying
a priority *value* and a `preemptionPolicy` (Kubernetes default
`PreemptLowerPriority`). Left unvalidated, a tenant could name a high-priority,
preempting class and have the scheduler **evict other tenants' running worker
pods** to schedule its own — a cross-tenant isolation break (Q132).

The platform owns which classes a tenant may use:

1. **The platform pre-creates the `PriorityClass` objects.** The GMC never
   creates cluster-scoped objects (same platform-ownership model as the
   `ResourceQuota`, Q130). Create allowlisted classes with
   **`preemptionPolicy: Never`** unless cross-tenant preemption is genuinely
   intended for that tier — `PriorityClass` is global, so a `PreemptLowerPriority`
   class lets *any* tenant that uses it evict *any* lower-priority pod cluster-wide,
   across tenant boundaries.

   ```yaml
   apiVersion: scheduling.k8s.io/v1
   kind: PriorityClass
   metadata:
     name: runner-standard
   value: 100000
   preemptionPolicy: Never   # orders ahead in scheduling without evicting others
   description: "Standard self-hosted runner worker pods."
   ```

2. **The platform allowlists the names on the GMC.** Set the
   `--allowed-priority-classes` flag (comma-separated) on the GMC controller. The
   validating webhook rejects any `ActionsGateway` whose
   `runnerGroups[].priorityTiers[].priorityClassName` is not on the list, naming
   both the offending class and the permitted set.

   ```yaml
   # GMC Deployment / Helm values — args on the controller-manager container
   args:
     - --allowed-priority-classes=runner-standard,runner-opportunistic
   ```

   **An empty/unset allowlist forbids every `priorityTiers` PriorityClass
   reference** (secure default): out of the box no tenant can set a
   `PriorityClass`. Tenants that only need a soft concurrency ceiling can use
   `maxWorkers` instead, which requires no `PriorityClass`.

There is deliberately no tenant-settable per-tier `preemptionPolicy` field;
preemption is governed entirely by the platform-created `PriorityClass` object.
See [§5.2 — Cross-Tenant Pod Preemption via PriorityClass](../design/05-security.md#52-agc--proxy-level-threats-namespace-scoped).

### Self-service additions via a watched ConfigMap (Q188)

Editing `--allowed-priority-classes` and rolling out the GMC for every new class
is slow. The GMC can **also** source the allowlist from a ConfigMap it watches in
its own install namespace, so a platform admin can add an allowed class without a
flag edit or restart — the change takes effect on the next watch event.

The ConfigMap source is **additive** and **off by default**. When unset, behavior
is exactly the flag-only behavior above.

1. **Enable the watch.** Set the ConfigMap name on the GMC, either via the flag
   or the Helm value:

   ```yaml
   # GMC Deployment / Helm values
   priorityClassAllowlist:
     configMapName: gmc-priority-class-allowlist   # renders --priority-class-allowlist-configmap
   ```

   Setting it renders the flag **and** a namespaced `Role`/`RoleBinding` granting
   the GMC `get`/`list`/`watch` on that one ConfigMap in its own namespace — the
   GMC is never granted cluster-wide ConfigMap read.

2. **Create the ConfigMap in the GMC's namespace.** It must live in the GMC
   install namespace (e.g. `gmc-system`) so only a platform admin — not a tenant —
   can write it. The `allowedPriorityClasses` key lists class names, separated by
   commas or newlines:

   ```yaml
   apiVersion: v1
   kind: ConfigMap
   metadata:
     name: gmc-priority-class-allowlist
     namespace: gmc-system
   data:
     allowedPriorityClasses: |
       runner-bursty
       runner-batch
   ```

   The **effective allowlist is the union** of the static `--allowed-priority-classes`
   flag and the ConfigMap entries: the ConfigMap can only *widen* the allowlist,
   never remove a flag-pinned class. You still pre-create each `PriorityClass`
   object first (step 1 of the previous section) — the allowlist only governs which
   *names* a tenant may reference.

**Fail-safe behavior.** The allowlist is a cross-tenant-isolation guardrail, so a
broken ConfigMap must never silently widen it:

- A **missing or deleted** ConfigMap, a **missing `allowedPriorityClasses` key**,
  or **any invalid entry** (a name that is not a valid DNS-1123 subdomain) causes
  the GMC to fall back to the **static flag allowlist only** and log a warning.
- A malformed ConfigMap is rejected **wholesale** — a valid name sitting next to a
  typo is *not* partially applied — so a mistake can never smuggle a class in.
- An **empty** `allowedPriorityClasses` value is valid and simply adds nothing.

Because the dynamic set is additive and resets to the static base on any error,
the worst case of a bad ConfigMap is that recently-added self-service classes stop
being accepted until it is fixed — never that an unintended class becomes allowed.

---

## License attribution in images

The compiled binaries statically link third-party Go modules (MIT/BSD/Apache/ISC/…),
whose licenses require reproducing their copyright/notice text wherever the
binaries are redistributed — and a container image is a redistribution
(Apache-2.0 §4(d), the MIT/BSD reproduce-the-notice clauses). Each of the four
production images therefore ships its license attribution under **`/licenses/`**,
the [Red Hat/OpenShift container-certification](https://github.com/redhat-openshift-ecosystem/openshift-preflight)
convention, which pairs with the `org.opencontainers.image.licenses="Apache-2.0"`
label every image already carries.

- **What is bundled.** `/licenses/` in the `agc`, `gmc`, `proxy`, and `worker`
  images contains three files:
  - `LICENSE` — the project's own Apache-2.0 license.
  - `NOTICE` — the project's Apache-style copyright/attribution notice.
  - `THIRD-PARTY-NOTICES` — the aggregated license and notice texts of every
    vendored module statically linked into the binary.

  The `worker` image is built on the upstream `actions-runner` base, which
  carries its own license files for its components; the `/licenses/` files we add
  cover only the wrapper binary and its dependencies.

- **Inspect it on a running pod.** The files are plain text owned root-readable,
  so any container can read them:

  ```bash
  kubectl exec deploy/gmc -- cat /licenses/LICENSE
  # distroless images (agc/gmc/proxy) have no shell; use the worker base's shell,
  # or copy a file out of any of them:
  kubectl cp <namespace>/<pod>:/licenses/THIRD-PARTY-NOTICES ./THIRD-PARTY-NOTICES
  ```

- **How it is kept current.** `THIRD-PARTY-NOTICES` lives at the repo root and is
  **generated and committed** by `make third-party-notices`
  ([`scripts/gen-third-party-notices.sh`](../../scripts/gen-third-party-notices.sh)),
  which concatenates the `LICENSE`/`NOTICE`/`COPYING` files of every module under
  the committed, version-pinned `vendor/` tree — offline, no network or module
  cache. The CI `license-notices` workflow runs `make third-party-notices-check`
  on every change to `vendor/**` (or the generator) and fails the PR if the
  committed file is stale, so a dependency add/remove/bump cannot ship without
  refreshed attribution.

---

## Image provenance: signature & SBOM verification

The four first-party images (`gmc`, `agc`, `proxy`, `worker`) are published to
GHCR by the [`publish.yml`](../../.github/workflows/publish.yml) workflow on every
`v*` release tag (the maintainer-facing cut-a-release procedure is in
[release.md](release.md)). Each one is:

- **Multi-arch** (`linux/amd64` + `linux/arm64`): the published ref is an OCI
  image **index**; the digest you pin at install time is the index digest, and
  the kubelet resolves the node's per-arch manifest from it at pull time.
- **Signed keyless with [cosign](https://docs.sigstore.dev/).** There is no
  signing key to distribute or rotate — the signature is bound to a short-lived
  [Fulcio](https://docs.sigstore.dev/certificate_authority/overview/) certificate
  issued against the GitHub Actions OIDC identity of the publish workflow and
  recorded in the public [Rekor](https://docs.sigstore.dev/logging/overview/)
  transparency log. You verify *who signed it* (the workflow identity), not a key
  you have to trust out-of-band. Signing is **recursive**: the index *and* each
  per-arch manifest carry a signature, so verification succeeds against the
  pinned index digest and also against a per-arch manifest digest (e.g. an image
  mirrored or referenced by platform-specific manifest).
- **Carrying a signed SLSA build-provenance attestation** (from
  [`actions/attest-build-provenance`](https://github.com/actions/attest-build-provenance)),
  attached to the index digest as an OCI referrer through the same keyless
  Fulcio/Rekor path. It records *how* the image was built — the workflow, repo,
  commit, and trigger — and is authenticated, so a pusher can't forge it. This
  meets **SLSA Build L2**; buildx's unsigned default provenance is disabled in
  favour of it (full SLSA Build L3 would need an isolated reusable-workflow
  builder, not yet adopted).
- **Accompanied by an SPDX-JSON SBOM per architecture** (generated with
  [syft](https://github.com/anchore/syft)) attached as a cosign attestation to
  that architecture's manifest, so you can enumerate exactly what shipped in the
  image your nodes actually run.

### Verify a signature

Before deploying — or as a forensic step when investigating a suspected image
swap (the "Verify the image" step in the
[compromised-AGC](#suspected-compromised-agc-tenant-scoped) and
[compromised-GMC](#suspected-compromised-gmc-cluster-scoped-tier-0) playbooks) —
confirm the image was signed by *this project's* publish workflow:

```bash
# Pin the identity to the publish workflow on a release tag, and the issuer to
# GitHub's OIDC provider. A signature from any other identity (or none) fails.
cosign verify \
  --certificate-identity-regexp '^https://github.com/actions-gateway/github-actions-gateway/\.github/workflows/publish\.yml@refs/tags/v.*$' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
  ghcr.io/actions-gateway/gmc:<tag-or-digest>
```

- **A `cosign verify` failure is a stop-ship / incident signal.** It means the
  image was not signed by the publish workflow — a locally built, tampered, or
  third-party image. Do not deploy it; if it is already running, treat it as a
  suspected supply-chain compromise (isolate per the playbooks above).
- Always verify by **digest** (`@sha256:…`) for the running workload — a tag is
  mutable; the digest is the bytes. `kubectl get pod <p> -o jsonpath='{.status.containerStatuses[*].imageID}'`
  gives the digest actually pulled.
- The same `--certificate-identity-regexp` / `--certificate-oidc-issuer` pair is
  what a cluster-admission policy engine (Kyverno `verifyImages`, Sigstore policy
  controller) should enforce so unsigned images can't run at all — that
  cluster-wide enforcement is the operator's to configure (the gateway does not
  ship it, mirroring the registry-allowlist split in
  [§5.2 Supply-Chain](../design/05-security.md#52-agc--proxy-level-threats-namespace-scoped)).

### Verify build provenance

The build-provenance attestation answers *how and where* the image was built —
the complement to the signature's *who signed it*. It is bound to the **index**
digest (unlike the per-arch SBOM attestations), so a tag or index-digest
reference verifies directly. The one-command check uses the GitHub CLI:

```bash
# Confirms the signed SLSA provenance was minted by this project's publish
# workflow. Resolve the tag to the index digest first for a workload check.
gh attestation verify oci://ghcr.io/actions-gateway/gmc:<tag-or-digest> \
  --repo actions-gateway/github-actions-gateway \
  --signer-workflow actions-gateway/github-actions-gateway/.github/workflows/publish.yml
```

`cosign` verifies the same attestation against the keyless identity, matching the
signature/SBOM commands above (the predicate type is the SLSA provenance v1
in-toto type):

```bash
cosign verify-attestation --type slsaprovenance1 \
  --certificate-identity-regexp '^https://github.com/actions-gateway/github-actions-gateway/\.github/workflows/publish\.yml@refs/tags/v.*$' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
  ghcr.io/actions-gateway/gmc:<tag-or-digest> \
  | jq -r '.payload | @base64d | fromjson | .predicate.buildDefinition.externalParameters'
```

- **A provenance verification failure is a stop-ship / incident signal**, exactly
  like a `cosign verify` failure: the image was not built-and-attested by the
  publish workflow. The same identity pair is what an admission policy engine can
  enforce so only images with valid provenance run.
- The provenance is **authenticated** (signed via Fulcio, logged in Rekor), which
  is the property buildx's unsigned default provenance lacks — that is why the
  pipeline disables the default and emits this signed one instead.

### Retrieve and inspect the SBOM

The SBOMs ride with the image as signed attestations — **one per architecture,
attached to that architecture's manifest digest** (not to the index, so the
SBOM you audit is exactly what your nodes run) — and are also uploaded as build
artifacts on each publish run. To pull and inspect one from the registry,
resolve the per-arch digest from the index first:

```bash
# Resolve the manifest digest for the architecture you are auditing.
digest="$(docker buildx imagetools inspect ghcr.io/actions-gateway/gmc:<tag-or-digest> --raw \
  | jq -r '.manifests[] | select(.platform.os == "linux" and .platform.architecture == "amd64") | .digest')"

# Download that arch's SPDX-JSON SBOM attestation, verifying its keyless signature first.
cosign verify-attestation --type spdxjson \
  --certificate-identity-regexp '^https://github.com/actions-gateway/github-actions-gateway/\.github/workflows/publish\.yml@refs/tags/v.*$' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
  "ghcr.io/actions-gateway/gmc@${digest}" \
  | jq -r '.payload | @base64d | fromjson | .predicate' > gmc.spdx.json

# Then audit packages, e.g. grep for a CVE-affected library, or feed to a scanner:
jq -r '.packages[].name' gmc.spdx.json | sort -u
```

PR CI ([`security-scan.yml`](../../.github/workflows/security-scan.yml)) builds
each image and generates the same SBOM as a build artifact, so SBOM generation is
exercised on every code PR — but **signing and attestation run only on
publish** (they need a registry push and the publish workflow's OIDC identity).
A green PR therefore proves the image builds and the SBOM generates; it does not
exercise the cosign sign/attest path.

---

## Reference Links

- [Threat model (05-security.md)](../design/05-security.md) — the abuse heuristics this runbook operationalises
- [observability.md](observability.md) — full metrics reference and SLO alerting
- [runbook.md](runbook.md) — incident response and day-2 operations
- [troubleshooting.md](troubleshooting.md) — symptom → diagnosis → remediation
- [Security review findings (plan/security.md)](../plan/security.md) — per-finding status, including H-2 and the audit-policy follow-on
