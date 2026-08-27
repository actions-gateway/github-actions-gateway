# Security Operations: Abuse Detection & Response

> **Audience:** Platform engineer, Security

This runbook turns the abuse heuristics in the [threat model](../design/05-security.md) into concrete, operator-actionable detections.
It complements — does not replace — the availability/SLO alerting in [observability.md](observability.md) and the incident-response procedures in [runbook.md](runbook.md).

The signals here detect **abuse or compromise** (a misbehaving tenant, a compromised AGC/GMC, a saturation attack), not ordinary capacity degradation.
Each row of [§5.1](../design/05-security.md#51-gmc-level-threats-cluster-scoped) and [§5.2](../design/05-security.md#52-agc--proxy-level-threats-namespace-scoped) of the threat model that says "operators should monitor X" is mapped below to the metric or audit-log query that surfaces it.

Two detection substrates are used:

- **Prometheus metrics** — emitted by the controllers and proxy today.
  See [observability.md](observability.md) for the full reference and how to scrape them.
  Alert rules are in [§ Prometheus abuse alerts](#prometheus-abuse-alerts) below.
- **API-server audit log** — the only substrate that can see a compromised AGC/GMC issuing RBAC-permitted-but-anomalous calls (e.g. a full-body Secret `list`).
  These detections require an audit policy that captures the relevant verbs; the controllers cannot self-report calls made out-of-band by a compromised binary.
  A sample audit policy is tracked separately (see [§ Audit-log abuse detections](#audit-log-abuse-detections)).

---

## Table of Contents

- [Threat → signal map](#threat--signal-map)
- [Prometheus abuse alerts](#prometheus-abuse-alerts)
- [Audit-log abuse detections](#audit-log-abuse-detections)
  - [API server audit policy (sample)](#api-server-audit-policy-sample)
- [Per-connection egress audit](#per-connection-egress-audit)
- [Response playbooks](#response-playbooks)
  - [Suspected compromised AGC (tenant-scoped)](#suspected-compromised-agc-tenant-scoped)
  - [Suspected compromised GMC (cluster-scoped, Tier-0)](#suspected-compromised-gmc-cluster-scoped-tier-0)
  - [Proxy saturation / slowloris](#proxy-saturation--slowloris)
- [Posture scanning (preventive)](#posture-scanning-preventive)
  - [Manifest posture — polaris (automated, in CI)](#manifest-posture--polaris-automated-in-ci)
  - [CIS-benchmark posture — kube-bench (manual, pre-production)](#cis-benchmark-posture--kube-bench-manual-pre-production)
- [Job intake: bind every tenant to a GitHub runner group](#job-intake-bind-every-tenant-to-a-github-runner-group)
- [Tenant egress posture & deliberate widening](#tenant-egress-posture--deliberate-widening)
  - [Managing egress at scale](#managing-egress-at-scale)
  - [Expressing GitHub egress by FQDN: the `egressPolicyMode` opt-in](#expressing-github-egress-by-fqdn-the-egresspolicymode-opt-in)
- [Worker egress destinations: the egress allowlist](#worker-egress-destinations-the-egress-allowlist)
- [Sharing an egress proxy across namespaces](#sharing-an-egress-proxy-across-namespaces)
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
| **Eviction-Retry API Misuse** ([§5.2](../design/05-security.md#52-agc--proxy-level-threats-namespace-scoped)) — compromised AGC looping `rerun-failed-jobs` | `eviction_retries_total{cause="eviction"}` rate climbs without matching node pressure; `eviction_retries_exhausted_total` increments. Split by the `tier` label to see which acquisition path is issuing the re-runs (Q417); the alert below aggregates over it, so it fires either way. **Scope the query to `cause="eviction"`** — the same counter records legitimate `preemption` recoveries, which are the expected steady state under a preempting `priorityTiers` floor and would otherwise read as abuse (Q497) | Metric | Ticket → Page on sustained climb |
| **Proxy Pool Exhaustion / slowloris** ([§5.2](../design/05-security.md#52-agc--proxy-level-threats-namespace-scoped), M-17/M-18) | `proxy_connections_active` pinned near capacity; `proxy_tunnel_duration_seconds` mass in the 6h bucket | Metric | Page |
| **Server-Side Request Forgery (SSRF) / destination probing via proxy** ([§5.2](../design/05-security.md#52-agc--proxy-level-threats-namespace-scoped), M-2/M-12) | `proxy_connect_denied_total` rate rising — every increment is an explicit allowlist denial (a workload reaching for an off-allowlist destination), so this is the precise signal; corroborate with a `proxy_dial_errors_total` spike. The counter says a probe happened, not what it reached: [per-connection egress audit](#per-connection-egress-audit) names the destination, but only on a pool that had it enabled before the incident | Metric (+ optional log) | Ticket |
| **DoS via Resource Exhaustion** ([§5.2](../design/05-security.md#52-agc--proxy-level-threats-namespace-scoped)) — rogue workflow exhausting tenant quota | `kube_resourcequota` used/hard ratio sustained at 1.0 | Metric (kube-state-metrics) | Ticket |
| **`ActionsGateway` CR in reserved namespace / spec probing** ([§5.1](../design/05-security.md#51-gmc-level-threats-cluster-scoped)) | Admission webhook `403` rejection rate | Metric (controller-runtime) | Ticket |
| **Cross-Tenant GitHub App Credential Leakage / key compromise** ([§5.1](../design/05-security.md#51-gmc-level-threats-cluster-scoped)) | `token_refresh_errors_total` spike (key revoked out-of-band, or a forged token rejected) | Metric | Page |
| **Mass tenant provisioning** ([§5.1](../design/05-security.md#51-gmc-level-threats-cluster-scoped)) — compromised GMC deploying workloads | `managed_gateways` jumps unexpectedly | Metric | Page |
| **AGC overpermissioned Secret access** ([§5.2](../design/05-security.md#52-agc--proxy-level-threats-namespace-scoped), H-2 residual) — compromised AGC binary issuing a full-body Secret `list` | AGC ServiceAccount `list secrets` in audit log (legit code path is metadata-only — see [security.md H-2](../plan/security.md)) | Audit log | Page |
| **GMC privilege escalation** ([§5.1](../design/05-security.md#51-gmc-level-threats-cluster-scoped)) — compromised GMC reading Secrets / writing out-of-tenant resources | GMC ServiceAccount `get secrets` beyond reconcile cadence; `namespaces patch` denied by `namespace-psa-guard`; any write denied by `gmc-tenant-resource-guard` | Audit log | Page |

---

## Prometheus abuse alerts

These rules reference metrics that are emitted today ([observability.md § Full Metrics Reference](observability-metrics.md#full-metrics-reference)).
Drop them into the same `PrometheusRule` group as the SLO alerts, or a dedicated `actions-gateway-security` group.
Tune thresholds to your fleet.

```yaml
groups:
  - name: actions-gateway-security
    rules:

      # Page: eviction-retry loop — sustained re-queue rate without a
      # matching node-pressure event suggests rerun-failed-jobs abuse
      # (compromised AGC) rather than genuine eviction churn.
      #
      # Scoped to cause="eviction" deliberately (Q497). The same counter also
      # records preemption recoveries, which are the EXPECTED steady state for a
      # tenant running a preempting priorityTiers floor — including them would
      # page on a supported configuration working correctly, and an alert that
      # cries wolf on normal operation stops being read.
      - alert: ActionsGatewayEvictionRetryAbuse
        expr: |
          sum by (namespace, runner_group) (
            rate(actions_gateway_eviction_retries_total{cause="eviction"}[15m])
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

      # Ticket: allowlist-denied CONNECTs — the precise SSRF signal. Every
      # increment is an explicit egress-allowlist denial, so this fires even
      # when the blocked destinations are unreachable (no dial attempted).
      # Also shipped in the reference PrometheusRule with a runbook_url.
      - alert: ActionsGatewayProxyConnectDenied
        expr: |
          rate(actions_gateway_proxy_connect_denied_total[5m]) > 0.1
        for: 10m
        labels:
          severity: warning
        annotations:
          runbook_url: "https://actions-gateway.com/operations/runbook/#actionsgatewayproxyconnectdenied"
          summary: "Egress proxy denying CONNECTs in {{ $labels.namespace }}"
          description: "The egress proxy is refusing CONNECT requests to off-allowlist destinations at >0.1/s for 10m — a workload probing blocked destinations, or a misconfigured egress target. Sharper than dial errors: every denial here is an explicit allowlist rejection."

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

> **Note on labels.** The proxy metrics (`actions_gateway_proxy_*`) carry no intrinsic `namespace` label — each per-tenant proxy is a separate scrape target.
> The `{{ $labels.namespace }}` interpolation above resolves from the `namespace` label your `ServiceMonitor`/scrape config attaches to the target, not from the metric itself.
> If your scrape config does not add it, drop the interpolation.

---

## Audit-log abuse detections

The most dangerous abuse signals — a compromised AGC or GMC issuing RBAC calls that are *permitted* but *anomalous* — are invisible to Prometheus.
The legitimate code paths avoid them (the AGC enumerates its Secrets metadata-only per [H-2](../plan/security.md); the GMC reads Secret bodies only during a reconcile via a cache-bypassing `Get`), so any of the calls below originating from a controller ServiceAccount indicates the binary is doing something its source does not.

Detecting these requires an **API-server audit policy** that logs the relevant verbs at `Metadata` level or higher, shipped to a security information and event management (SIEM) system or log-based alerting backend.
A ready-to-adapt sample policy ships at [`examples/apiserver-audit-policy.yaml`](examples/apiserver-audit-policy.yaml); [§ API server audit policy (sample)](#api-server-audit-policy-sample) below covers installing it and reading the events.
The table specifies *what to alert on* once that policy is in place.

| Detection | Audit predicate | Why it matters | Response |
|---|---|---|---|
| **AGC full-body Secret list** | `verb=list resource=secrets` by the AGC ServiceAccount (`system:serviceaccount:<tenant-ns>:actions-gateway-controller`) returning object bodies | Legit AGC code lists Secret *metadata* only ([H-2 residual](../plan/security.md)). A body `list` means out-of-band enumeration of user-managed Secrets. | Treat the AGC as compromised: cordon the tenant namespace, rotate the GitHub App key (runbook.md § GitHub App Key Compromise), inspect the AGC image. |
| **AGC Secret access outside its label scope** | `verb=get resource=secrets` by the AGC SA for Secret names not matching `actions-gateway/runner-group=*` or the AGC's `gitHubAppRef` | The AGC only needs its agent-pool and payload Secrets. A `get` on a developer's `ghcr-pull-token` is exfiltration. | As above. |
| **GMC Secret reads beyond reconcile cadence** | `verb=get resource=secrets` by the GMC SA (`system:serviceaccount:gmc-system:gmc-controller-manager`) at a rate far above the reconcile/requeue cadence | The GMC reads each `gitHubAppRef` Secret only during reconcile (cache-bypassed `Get`). A high `get` rate is credential harvesting. | Treat the GMC as a Tier-0 compromise: isolate the GMC pod, rotate **all** tenant GitHub App keys, audit which Secrets were read. |
| **GMC namespace-PSA escalation attempt** | `namespace-psa-guard` ValidatingAdmissionPolicy `deny` events for the GMC SA | The guard ([§5.3](../design/05-security.md#53-security-profiles-and-the-privileged-opt-in)) blocks the GMC relabelling non-tenant namespaces (e.g. `kube-system` → `privileged`). A denial means the GMC tried. | A denial is a successful block, but a *signal* of compromise. Isolate the GMC and investigate. |
| **GMC out-of-tenant resource write** | `gmc-tenant-resource-guard` ValidatingAdmissionPolicy `deny` events for the GMC SA | The guard blocks the GMC creating/updating/deleting Deployments, RoleBindings, Secrets, NetworkPolicies, etc. in any namespace not marked `actions-gateway.github.com/tenant=true` (e.g. a Deployment or Secret into `kube-system`). A denial means the GMC tried (Q121/Q122). | A denial is a successful block but a *signal* of compromise. Isolate the GMC and investigate. |
| **GMC workload creation outside reconcile** | `verb=create resource=deployments|roles|rolebindings` by the GMC SA in a *marked tenant* namespace with no corresponding `ActionsGateway` CR change | The `gmc-tenant-resource-guard` VAP already blocks writes into *unmarked* namespaces; this catches the residual — provisioning inside a legitimate tenant namespace without a triggering CR edit, which is lateral movement within the GMC's confined scope. | Isolate the GMC; diff provisioned resources against live `ActionsGateway` CRs. |

Without the audit policy, these threats are mitigated structurally (RBAC scope, cache-bypass, the `namespace-psa-guard` and `gmc-tenant-resource-guard` VAPs, no Secret informer) but write-confinement denials aside are **not observable** — there is no alert that fires if a compromised binary exercises its standing *read* permissions.
Closing that gap is the value of the audit policy.
Note the two VAPs confine GMC *writes* (create/update/delete) only; Secret *reads* (`get`/`list`/`watch`) cannot be gated at admission and remain cluster-wide at the RBAC layer — the audit policy is the only detective control for them (see [§5.1](../design/05-security.md#51-gmc-level-threats-cluster-scoped)).

### API server audit policy (sample)

A sample audit policy that captures exactly the verbs the table above alerts on — and nothing else — ships at [`examples/apiserver-audit-policy.yaml`](examples/apiserver-audit-policy.yaml).

**What it detects.** Three focused rule groups, all at `Metadata` level:

1. **GMC Secret reads** (`get`/`list`/`watch secrets`) by the GMC ServiceAccount, cluster-wide — surfaces credential harvesting and any read outside the GMC's tenant namespaces.
2. **GMC out-of-tenant write attempts** (`create`/`update`/`patch`/`delete`) on the kinds the `gmc-tenant-resource-guard` and `namespace-psa-guard` VAPs confine — a `403` `responseStatus` is a successful block but a signal the binary tried.
3. **AGC Secret reads** (`get`/`list`/`watch secrets`) by each AGC ServiceAccount — surfaces the full-body `list` and out-of-scope `get` the legitimate metadata-only code path never makes.

**Why `Metadata`, not `RequestResponse`.** A Secret `get`/`list` *response* body contains `.data` — the GitHub App private keys this control protects.
Logging at `RequestResponse` would copy that key material into the audit backend, creating a second exfiltration surface.
`Metadata` records the requester, verb, resource, name, namespace, timestamp, and response code — enough to detect an anomalous read without duplicating the secret.
Keep the Secret rules at `Metadata`.

**Before you install,** edit the placeholders (the file's header comment lists them): the GMC ServiceAccount user string if you overrode the install namespace or `namePrefix`, and one `users:` entry per tenant namespace for the AGC rule (the audit `users:` field is an exact match with no wildcard).

#### Where auto-install is — and isn't — possible

The policy is a **static file `kube-apiserver` reads at startup**, not a cluster object: there is no `kubectl apply` for it, and the Helm chart cannot install it (it deploys workloads, not control-plane node files).
Full installation is therefore only possible where **you control the API-server flags** — a cluster you provision (kind, kubeadm).
On a managed control plane (EKS / GKE / AKS) the provider owns those flags and ships a *fixed* audit configuration to its own log sink; you cannot supply this file, so the path is to enable the provider's audit logging and translate the same predicates against the managed stream.
Both are covered below.

#### Self-managed: cluster you provision (auto)

If you are creating the cluster, bake the policy into `kube-apiserver` from the start — no static-pod surgery.
The [`examples/kind-cluster-audit.yaml`](examples/kind-cluster-audit.yaml) kind config does this via `extraMounts` + a `ClusterConfiguration` audit patch; the same `apiServer.extraArgs` / `extraVolumes` block works in any kubeadm `ClusterConfiguration` (`kubeadm init --config`).

#### Self-managed: existing kubeadm cluster (scripted)

For a cluster already running, the policy file must be placed on each control-plane node and the `kube-apiserver` static-pod manifest patched to add the audit flags and mounts.
[`examples/install-apiserver-audit-policy.sh`](examples/install-apiserver-audit-policy.sh) automates exactly that — run it **once per control-plane node, as root, on the node**:

```bash
sudo ./install-apiserver-audit-policy.sh        # --dry-run to preview first
```

It validates the policy, installs it to `/etc/kubernetes/audit/policy.yaml`, and idempotently patches `/etc/kubernetes/manifests/kube-apiserver.yaml` (timestamped backup; `yq` for the structured edit so the manifest cannot be corrupted).
The kubelet restarts the API server automatically.
To do it by hand instead, add the `--audit-policy-file` / `--audit-log-*` flags and the `audit-policy` + `audit-log` `volumeMounts`/`volumes` to the manifest — the script's [kind config](examples/kind-cluster-audit.yaml) shows the exact shape.
Use `--audit-log-path=-` to emit to stdout (e.g. to ship via a log agent) instead of a file.
Forward the log to your SIEM and translate the table's predicates into alert rules there.

#### Managed control planes (EKS / GKE / AKS)

You **cannot** supply a custom `--audit-policy-file` on a managed control plane — the provider owns the API-server flags.
Instead, enable the provider's control-plane audit logging and apply the **same predicates** (requester `user.username` / principal, `verb`, Secret resource, VAP `403` denials) as filters/alerts against the managed log stream.
The detection logic is identical; only the substrate differs.
The provider's default policy is broader than this sample (it logs more than the controller ServiceAccounts) — scope your queries to the controller identities below.

- **Amazon EKS.** Enable the **`audit`** control-plane log type on the cluster (`aws eks update-cluster-config --logging`, or the console/IaC equivalent).
  Events land in CloudWatch log group `/aws/eks/<cluster>/cluster`, streams `kube-apiserver-audit-*`; EKS's fixed policy logs Secret access at `Metadata`.
  Query with CloudWatch Logs Insights:

  ```
  fields @timestamp, user.username, verb, objectRef.namespace, objectRef.name, responseStatus.code
  | filter @logStream like /kube-apiserver-audit/
  | filter objectRef.resource = "secrets"
      and user.username = "system:serviceaccount:gmc-system:gmc-controller-manager"
  | filter verb in ["list","watch"] or objectRef.namespace != "<a-tenant-namespace>"
  ```

- **Google GKE.** API-server **write** events are Admin Activity audit logs (always on).
  Secret **reads** are **Data Access** logs, which are **off by default** — enable `DATA_READ`/`ADMIN_READ` for the Kubernetes Engine API (Cloud Console → IAM → Audit Logs, or IaC).
  Query in Logs Explorer; the Kubernetes verb is encoded in `protoPayload.methodName` (`io.k8s.core.v1.secrets.get` / `.list` / `.watch`) and the caller in `protoPayload.authenticationInfo.principalEmail`:

  ```
  resource.type="k8s_cluster"
  protoPayload.methodName=~"io.k8s.core.v1.secrets.(get|list|watch)"
  protoPayload.authenticationInfo.principalEmail="system:serviceaccount:gmc-system:gmc-controller-manager"
  ```

- **Azure AKS.** Add a cluster **diagnostic setting** forwarding the **`kube-audit`** category (to Log Analytics, a storage account, or Event Hub).
  Use `kube-audit`, **not** `kube-audit-admin`: the `-admin` variant drops `get`/`list` events, so it cannot see Secret reads — the very signal this policy exists for.
  Query with KQL (the event is JSON in the `log_s` column):

  ```kusto
  AzureDiagnostics
  | where Category == "kube-audit"
  | extend e = parse_json(log_s)
  | where e.objectRef.resource == "secrets"
      and e.user.username == "system:serviceaccount:gmc-system:gmc-controller-manager"
  | where e.verb in ("list","watch") or e.objectRef.namespace != "<a-tenant-namespace>"
  ```

  Repeat the per-provider queries for each AGC ServiceAccount (`system:serviceaccount:<tenant-ns>:actions-gateway-controller`), alerting on `verb == "list"` or a `get` on a Secret the AGC does not own.

**Read the events.** Audit events are one JSON object per line.
To see GMC Secret reads (substitute your GMC user string):

```bash
jq -c 'select(.user.username == "system:serviceaccount:gmc-system:gmc-controller-manager"
        and .objectRef.resource == "secrets")
       | {stage, verb, ns: .objectRef.namespace, name: .objectRef.name,
          code: .responseStatus.code, t: .requestReceivedTimestamp}' \
  /var/log/kubernetes/audit/audit.log
```

A baseline is one `get` per `gitHubAppRef` Secret per reconcile, only in tenant namespaces.
Alert on: any `list`/`watch`; a `get` rate well above the reconcile cadence; or any `get` outside a tenant namespace (e.g.
`kube-system`).
For the GMC write rule, a `responseStatus.code` of `403` is a VAP block — investigate the binary that attempted it.
For AGC reads, filter by each AGC user string and alert on `verb == "list"` (the legit path is metadata-only) or a `get` on a Secret name the AGC does not own.

---

## Per-connection egress audit

The audit policy above is a detective control on what a controller asks the **apiserver**.
It says nothing about where a tenant's workers went, which is a different question with a different substrate: the proxy's per-connection egress record (`EgressProxy.spec.auditLogging: Connections`).

**It is off by default and records forward, never backward.** Enabling it during an incident tells you nothing about the traffic that caused the incident, so the decision to enable is one you make before you need it: the same shape as the apiserver audit policy, and worth taking at the same time.

Weigh four things before turning it on across a fleet:

| | |
|---|---|
| **What it retains** | One line per accepted CONNECT naming the destination host and port. That is a record of where a tenant's traffic went, so retention and access are a policy decision, not a default. |
| **What it costs** | One line per connection. Under real CI load it becomes the pool's dominant log volume; size the collector before enabling, not after. |
| **What it cannot tell you** | On a pool shared via `spec.sharing.allowedNamespaces` the record names the **pool**, not the consuming tenant. See [what you must not assume](#what-you-must-not-assume). |
| **What it never carries** | No request header, no tunneled byte, nothing from the TLS session. The proxy does not terminate or inspect it. Detail: [security design](../design/05-security.md#proxy-egress-audit-record). |
| **What it does not change** | Enforcement. This is a logging setting: no value alters what the proxy forwards. Destination enforcement stays `destinationFQDNs`/`destinationCIDRs` plus the pod-egress policy. Unlike Pod Security Admission, `audit` here does **not** mean report-only-instead-of-enforce, and enabling it relaxes nothing. |

Turning it on, and the field-by-field shape, are in [tenant onboarding](tenant-onboarding.md#per-pool-egress-audit-record) and the [logging reference](observability-logging.md#proxy-egress-audit-record).

---

## Response playbooks

For the full credential-rotation procedure see [runbook.md § GitHub App Key Compromise](runbook.md#github-app-key-compromise).
The abuse-specific first moves:

### Suspected compromised AGC (tenant-scoped)

1. **Contain.** Scale the AGC to zero so it stops acting: `kubectl scale deploy/actions-gateway-controller -n <namespace> --replicas=0`.
   In-flight jobs will be cancelled by GitHub when `renewjob` lapses; this is acceptable during a suspected breach.
2. **Rotate.** Rotate the tenant's GitHub App key ([runbook.md § GitHub App Key Compromise](runbook.md#github-app-key-compromise)) — the AGC held it in memory.
3. **Scope.** Check the API-server audit log for Secret `get`/`list` calls by the AGC ServiceAccount; enumerate which tenant Secrets may have been read.
4. **Verify the image.** Confirm the running AGC image digest matches the GMC-pinned `AGC_IMAGE` (digest pinning is enforced — see [§5.2 Supply-Chain](../design/05-security.md#52-agc--proxy-level-threats-namespace-scoped)).

### Suspected compromised GMC (cluster-scoped, Tier-0)

1. **Contain.** Scale the GMC to zero: `kubectl scale deploy/gmc-controller-manager -n gmc-system --replicas=0`.
   Existing tenant gateways keep running (the GMC is not in the data path — see [runbook.md § GMC Total Failure](runbook.md#gmc-total-failure)); only provisioning and reconcile pause.
2. **Rotate everything.** A compromised GMC can read every tenant's `gitHubAppRef` Secret.
   Rotate **all** tenant GitHub App keys.
3. **Scope.** Audit GMC ServiceAccount `get secrets` and `create`/`patch` calls; reconcile provisioned resources against live `ActionsGateway` CRs to find anything created off-CR.
4. **Verify the image.** Confirm the GMC image digest against the deployed manifest before scaling back up.

### Proxy saturation / slowloris

1. **Confirm the shape.** Long-lived tunnels in the `proxy_tunnel_duration_seconds` top bucket + pinned `proxy_connections_active` ⇒ slowloris.
   Spread across many short tunnels ⇒ genuine burst (let the HPA absorb it).
2. **Identify the source.** The proxy serves one tenant; the offending tunnels originate from worker pods in that tenant namespace.
   Inspect recent worker pods for the responsible workflow.
3. **Mitigate.** The proxy enforces a per-read idle deadline (5m) and a 6h absolute lifetime cap (M-18), so hung tunnels self-terminate.
   If a single workflow is the culprit, cancel its run in the GitHub Actions UI; the worker pod and its tunnels are released on job completion.

---

## Posture scanning (preventive)

The detections above catch abuse at runtime.
Two scanners catch posture regressions *before* they reach a cluster — one in CI on every chart change, one a pre-production manual step against the live cluster.

If your cluster runs a policy engine (Kyverno / OPA Gatekeeper), see [admission-policies.md](admission-policies.md) for whether GAG pods comply with common admission policies and for sample policies that *enforce* GAG's posture at admission time.

### Manifest posture — polaris (automated, in CI)

[polaris](https://polaris.docs.fairwinds.com/) audits the Kubernetes security/best-practice posture of the **shipped install artifact**: the CI `polaris` job (in [`.github/workflows/security-scan.yml`](../../.github/workflows/security-scan.yml)) renders the [Helm chart](../../charts/actions-gateway) and checks the rendered manifests.
It runs on every PR that touches the chart or the `Makefile`, and on every push to `main`.

- **What it gates.** The scan fails the PR on any `danger` finding — a privileged container, a host namespace, dangerous capabilities, a missing `securityContext`, a floating `:latest` image tag, and similar real regressions.
  A change that weakens the chart's hardened defaults cannot merge.
- **What it reports but does not block.** `warning`-level findings are printed for visibility.
  The handful that are false positives against a Helm-packaged operator chart (the controller's required ServiceAccount-token automount, the cross-document NetworkPolicy match polaris can't resolve statically, the `IfNotPresent` pull policy that is correct for a digest-pinned image, and Helm's `app.kubernetes.io/instance` labelling) are tuned to `ignore` in [`charts/actions-gateway/polaris.yaml`](../../charts/actions-gateway/polaris.yaml), each with a justifying comment.
  **Never relax a `danger` check to silence a finding — fix the chart instead** (secure-by-default).
- **Run it yourself.** `make polaris-scan` (needs `helm` and `polaris` on `PATH`) runs the exact CI gate locally.
  It renders with a placeholder image digest so the audit reflects the production, digest-pinned posture — a digest is also required for the chart to render at all (an empty digest on any of the four images — `gmc`/`agc`/`proxy`/`wrapper` — fails the render; `make manifest-validate` asserts each rejection), so the placeholder cannot mask a fail-open default.

> The scan audits *workload posture in the generated manifests*.
> It does not replace pinning real image digests (`gmc.image.digest`, `agc.image.digest`, `proxy.image.digest`, `wrapper.image.digest` in `values.yaml`) at install time — see [tenant-onboarding.md](tenant-onboarding.md) and the chart README.

### CIS-benchmark posture — kube-bench (manual, pre-production)

polaris scans our *manifests*; it cannot see how the *cluster itself* is configured (kubelet flags, API-server settings, etcd permissions, control-plane file modes).
Those are the province of the [CIS Kubernetes Benchmark](https://www.cisecurity.org/benchmark/kubernetes), which [kube-bench](https://github.com/aquasecurity/kube-bench) checks against a **live node** — so it cannot run in our manifest-only CI and is instead a pre-production checklist item the cluster operator runs once per cluster (and after any control-plane upgrade).

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

- **`[FAIL]` on control-plane / kubelet hardening** (e.g.
  `--anonymous-auth=false`, `--authorization-mode` not `AlwaysAllow`, read-only etcd data dir, `--protect-kernel-defaults=true`) — fix at the cluster layer before onboarding.
  These are cluster-admin remediations, not chart settings; managed control planes (EKS/GKE/AKS) pass most of them by default and expose the rest as cluster config.
- **NetworkPolicy / PodSecurity benchmark items** — this operator already satisfies the workload half: the chart ships GMC NetworkPolicies (`networkPolicy.enabled=true`) and the GMC stamps Pod Security Admission labels per tenant `securityProfile`.
  Confirm the cluster has a NetworkPolicy-enforcing Container Network Interface (CNI) (Calico/Cilium; kindnet does **not** enforce) and the `PodSecurity` admission plugin enabled, or those controls are inert.
  To prove enforcement on a live cluster, run the negative probes in [network-architecture.md § How to Validate Network Isolation](../design/network-architecture.md#how-to-validate-network-isolation) — the "blocked" probes must actually time out (validated under Calico on a kind cluster, Q7b 2026-06-11).
- **Cluster DNS must be labelled `k8s-app=kube-dns` in `kube-system`.** Tenant NetworkPolicies confine port-53 egress to the cluster DNS service rather than leaving DNS open to any resolver (Q105 — an open DNS path is an unattributed exfiltration side-channel).
  The selector matches the conventional CoreDNS deployment: pods labelled `k8s-app: kube-dns` in the `kube-system` namespace (matched via the immutable `kubernetes.io/metadata.name` namespace label).
  This is the default on every mainstream distribution and managed control plane.
  If your cluster runs DNS under a different label or namespace **and** uses an enforcing CNI, tenant pods will fail to resolve any name until you either relabel the DNS pods or set `spec.proxy.managedNetworkPolicy: false` and supply your own DNS egress rule.
  Symptom: tenant workloads time out on every lookup while non-DNS connectivity is unaffected.
- **NodeLocal DNSCache (`node-local-dns`) is supported.** With node-local-dns, pods send queries to a link-local IP (default `169.254.20.10`) served by a `hostNetwork` `node-local-dns` pod, not to a `k8s-app: kube-dns` CoreDNS pod — which the kube-dns podSelector cannot match.
  The tenant NetworkPolicies therefore allow port-53 egress to the link-local block `169.254.0.0/16` as a second peer alongside the kube-dns selector (Q136), so both topologies resolve out of the box with no operator action.
  Link-local is non-routable and node-scoped, so this preserves the no-arbitrary-resolver property of Q105 — the link-local block cannot reach an external resolver.
  If your node-local-dns cache listens on a non-default address *outside* `169.254.0.0/16`, set `spec.proxy.managedNetworkPolicy: false` and supply your own DNS egress rule, or add an additive NetworkPolicy — see [Tenant egress posture & deliberate widening](#tenant-egress-posture--deliberate-widening).
- **Findings that don't apply** (managed control plane hides the file, a check for a component you don't run) — record the justification alongside the cluster's onboarding ticket.

The goal is **zero critical (`[FAIL]`) findings that this stack depends on** before the first production tenant (per [milestone-5.md § 3](../plan/milestone-5.md#3-posture-audit-kube-bench--polaris)).

## Job intake: bind every tenant to a GitHub runner group

Everything else on this page bounds what a tenant's workers may reach.
This bounds who may reach *them*, and it is the one control that lives at GitHub, outside anything the cluster can enforce.

A runner set that names no GitHub runner group registers into the installation's **default** group, which in most organizations admits every repository.
On a shared cluster that means any repository in the org can put a tenant's runner label in `runs-on` and have its job run in that tenant's namespace, against that tenant's quota, egressing from that tenant's proxy IPs.
The pod isolation described above is untouched; what is unbounded is the intake.

Two things to do, in this order, per tenant:

1. **At GitHub**, create a runner group for the tenant and scope its repository access to that tenant's repositories.
   GAG never creates a runner group and never edits one's access policy, so a group whose access is *All repositories* buys nothing over the default group.
2. **In the cluster**, set `spec.defaultRunnerGroup` on the tenant's `ActionsGateway`.
   Every `RunnerSet` under it inherits the group; a set may override with its own `spec.runnerGroup` to narrow further.

A group name the installation does not have leaves the set `Ready=False`/`RunnerGroupNotFound` and registers no scale set, rather than falling back to the default group.
That is deliberate: the fallback would widen the boundary at the moment the name was mistyped.

Onboarding walkthrough and failure modes: [tenant onboarding](tenant-onboarding.md#bind-a-runner-set-to-a-github-runner-group).
Threat-model rationale: [design 5.2](../design/05-security.md).

## Tenant egress posture & deliberate widening

**The secure default is controller-managed and not opt-in.** For every tenant, the GMC reconciles three NetworkPolicies that confine worker (and AGC) egress to exactly what the design requires: DNS to the cluster DNS service only, and all GitHub-bound traffic through the per-tenant egress proxy (whose source IPs are attributable).
Worker pods cannot reach arbitrary destinations directly — that is the per-tenant egress-IP isolation property, and it is present automatically the moment a tenant is provisioned.
Do **not** hand-edit the GMC-managed policies (`actions-gateway-workload`, `actions-gateway-controller`, `actions-gateway-proxy`): the controller reconciles them back, and the proxy policy's GitHub-CIDR rule is refreshed from `api.github.com/meta` every 24h, so a hand-edit would be reverted or go stale.
See [network-architecture.md](../design/network-architecture.md#networkpolicy-rules) for the full policy set.

> **Running a service mesh?** A mesh sidecar transparently intercepts the worker's outbound TCP and can re-route GitHub-bound traffic through a mesh egress gateway, silently bypassing the per-tenant proxy and dropping the egress-IP attribution this isolation property rests on.
> See [Running GAG Alongside a Service Mesh](service-mesh-coexistence.md) for the injection opt-out and egress exclusions that keep GAG's proxy path authoritative.

Some jobs legitimately need egress the proxy cannot carry — the CONNECT proxy tunnels HTTP/HTTPS to GitHub CIDRs only, so a non-HTTP protocol (a database, SSH, a raw TCP/UDP service), an internal artifact store or package mirror, or a specific custom DNS resolver is unreachable by default.
**Grant that egress with an *additional*, additive NetworkPolicy in the tenant namespace — not by relaxing the managed defaults.** NetworkPolicies are additive (a union of allows), so an extra policy widens egress for the pods it selects without touching the floor.

Worker pods carry two selectable labels, so you can target all workers or a single runner type:

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

**This is a deliberate, documented trade-off, not a routine knob.** Egress to the listed destinations leaves with the worker's own pod IP and therefore **bypasses the per-tenant proxy egress-IP attribution** for those flows.
Untrusted job code (e.g. fork-PR workflows) can use any hole you open, so:

- Keep the allowlist as narrow as the use case requires — specific CIDRs and ports, never a `0.0.0.0/0` catch-all.
- Authoring it requires namespace NetworkPolicy-write, so it is inherently a platform/admin decision — which is the correct authority for relaxing attribution.
  Track each grant in the tenant's onboarding ticket.
- **For a custom DNS resolver specifically, prefer a cluster-level CoreDNS `forward` zone** over reopening worker DNS: that keeps resolution on the attributable in-cluster path while still resolving the names you need.

If instead you want to express the **proxy's own** GitHub egress as FQDN rules under a DNS-aware CNI, you have two supported options:

- **First-class, GMC-managed (recommended on a v2 `EgressProxy`)** — set `spec.egressPolicyMode: FQDN` and have the operator select `--fqdn-policy-backend`; the GMC emits the CNI-native FQDN policy for you, keeping it owned and reconciled.
  See [Expressing GitHub egress by FQDN](#expressing-github-egress-by-fqdn-the-egresspolicymode-opt-in) below.
- **Full hand-off** — set `spec.proxy.managedNetworkPolicy: false` (v1 `ActionsGateway`) or `spec.managedNetworkPolicy: false` (v2 `EgressProxy`): the GMC stops managing the proxy egress policy entirely and you own it.
  Use this when you need a shape the managed FQDN mode does not emit.

The managed CIDR path remains the default in all cases.

### Managing egress at scale

This project deliberately does **not** ship tooling to manage the *widening* policies — that is a cluster/platform concern with a mature ecosystem, and owning it here would re-create the coupling the managed-floor split avoids.
What the project commits to instead is a stable **integration surface**: every worker pod carries two labels you can target from any policy engine, and these are a supported contract (they will not be renamed without a migration note):

- `actions-gateway/component: workload` — all worker (and AGC) pods in the tenant
- `actions-gateway/runner-group: <name>` — workers of one specific RunnerGroup

For anything beyond a handful of static CIDRs, prefer the ecosystem over hand-written `NetworkPolicy`:

- **Your CNI's richer egress** — `CiliumNetworkPolicy` `toFQDNs` (DNS-aware, hostname allowlists), Calico `NetworkSet` (reusable CIDR groups) / DNS policy, and policy tiers.
  This is the right tool for "let `gpu-builders` reach `*.internal.corp` and a database."
  It pairs with the `spec.proxy.managedNetworkPolicy: false` hand-off above.
- **`AdminNetworkPolicy`** ([sig-network `network-policy-api`](https://network-policy-api.sigs.k8s.io/)) — cluster-admin-level, cross-namespace egress baselines (`AdminNetworkPolicy` / `BaselineAdminNetworkPolicy`), implemented by Cilium/Calico/OVN-Kubernetes.
  The most direct fit for "platform admin governs egress across all tenant namespaces" — maturing (alpha→beta), so confirm your CNI's support level.
- **Kyverno / OPA Gatekeeper** — policy-as-code to *generate* per-namespace NPs (e.g. a templated default-deny or egress allowance keyed off a namespace label) and to *validate* that any admin-added egress conforms to your guardrails.

The labels above are what make all of these targetable; the secure floor stays GMC-managed regardless.

### Expressing GitHub egress by FQDN: the `egressPolicyMode` opt-in

By default the GMC expresses the proxy pool's GitHub allowlist as **IP CIDR ranges**, refreshed from `api.github.com/meta` every 24h (`egressPolicyMode: CIDR`).
This works on every NetworkPolicy-enforcing CNI and needs no DNS awareness.
If your cluster enforces DNS-based egress, you can instead have the GMC express the allowlist by **hostname** — no CIDR feed to keep current.

The choice is split across **two roles** (Q245), so the tenant API stays stable as CNI/platform FQDN mechanisms proliferate:

- **The tenant expresses intent** on the v2 `EgressProxy` — `egressPolicyMode: FQDN` means "allow my GitHub (and any `destinationFQDNs`) egress by hostname."
  The tenant does **not** name Cilium/Calico/GKE.
- **The operator picks the mechanism**, once per cluster, via the GMC `--fqdn-policy-backend` flag (chart: `fqdnPolicyBackend`).

```yaml
apiVersion: actions-gateway.com/v2alpha1
kind: EgressProxy
metadata:
  name: shared
  namespace: team-a
spec:
  egressPolicyMode: FQDN   # intent only; default is CIDR
```

```yaml
# values.yaml — the platform operator picks how FQDN intent is enforced, cluster-wide.
fqdnPolicyBackend: cilium   # none (default) | cilium | calico | gke
```

**Operator backend selector (`--fqdn-policy-backend`):**

| Backend | GMC emits | Requires |
| --- | --- | --- |
| `none` (default) | *(nothing)* — FQDN intent is **rejected at admission** | — the cluster declares no FQDN backend |
| `cilium` | `CiliumNetworkPolicy` (`cilium.io/v2`) with `toFQDNs` | A **self-managed** Cilium with DNS-aware policy **and the `cilium.io/v2 CiliumNetworkPolicy` CRD installed** |
| `calico` | Calico `NetworkPolicy` (`projectcalico.org/v3`) with destination `domains` | Calico with DNS-based policy enabled, and the `projectcalico.org/v3 NetworkPolicy` CRD installed |
| `gke` | GKE `FQDNNetworkPolicy` (`networking.gke.io/v1alpha1`) | GKE Dataplane V2 with `--enable-fqdn-network-policy` |

The emitted object is named `<proxy>-proxy-fqdn`, owned by the `EgressProxy` (garbage-collected with it), and covers the GitHub Actions runner endpoint families: `api.github.com`, `github.com`, `codeload.github.com`, `objects.githubusercontent.com`, `*.actions.githubusercontent.com`, and `*.blob.core.windows.net` — **plus the `gitHubURL` host of every gateway that references this proxy**, directly via `defaultProxyRef` or through a `RunnerSet`'s `proxyRef` (Q506).
That is what makes an FQDN mode work for GitHub Enterprise Server without any extra configuration: the appliance's hostname is on the referring `ActionsGateway`, and a gateway applied after the proxy re-triggers the policy.
A public-GitHub-only pool emits exactly what it did before.
In an FQDN mode the standard `NetworkPolicy` drops its GitHub-CIDR rule (DNS + ingress are unchanged) and the 24h IP-range reconcile skips this proxy.
The GMC also re-checks the emitted policy on a bounded cadence (a fraction of the egress-staleness window, ~6h at the default; Q326), so an out-of-band edit or delete is repaired within that window even when nothing else touches the `EgressProxy`.

> **The default `none` denies FQDN intent — loudly, at apply time.** A tenant that sets `egressPolicyMode: FQDN` on a cluster with `--fqdn-policy-backend=none` is **rejected by the admission webhook** ("this cluster has no FQDN egress backend configured; ask the platform operator, or use egressPolicyMode: CIDR"), not left with a silently `Degraded` proxy pool.
> The default never guesses a mechanism or auto-detects.

> **GHES on the CIDR default needs the appliance's ranges.** CIDR mode allows only the ranges `api.github.com/meta` publishes, and a GitHub Enterprise Server appliance on your own address space is in none of them — so the pool's `NetworkPolicy` denies the traffic it exists to carry.
> The GMC cannot know those ranges, so it names the gap: `GitHubEgressIncomplete=True` / `ApplianceRangesRequired` on the `EgressProxy`, with the unreachable host in the message.
> Supply the ranges in `spec.destinationCIDRs` — allowlisting them under `--allowed-egress-cidrs` first, since that field is platform-gated — or move the pool to an FQDN mode, which carries the host for you.
> See [troubleshooting](troubleshooting.md#a-ghes-tenants-traffic-never-reaches-the-appliance).

**Deprecated `CiliumFQDN` / `CalicoFQDN`.** The earlier per-CNI enum values still work: each pins its namesake backend regardless of `--fqdn-policy-backend`, so existing `EgressProxy` objects keep behaving exactly as before.
They are **deprecated** — the admission webhook attaches a warning steering you to `FQDN` + `--fqdn-policy-backend`.
Migrate by changing the tenant field to `FQDN` and setting the matching operator backend.

> **They are not removed at `v2.0.0`.
> The earliest release that may remove them is `v3.0.0`.** The two values are enum members of `egressPolicyMode` in the **beta** version `actions-gateway.com/v2beta1`, and `v2.0.0` keeps serving `v2beta1` — it adds the General Availability (GA) `v2` version beside it rather than taking it away.
> An API element can only be removed by incrementing the version, never deleted from a version that is still served, so these two live exactly as long as `v2beta1` does.
> Retiring a served version is a breaking change and lands on a major tag, which puts the earliest possible removal at `v3.0.0` — one major beyond the `v2.0.0` that removes `v1alpha1`, `v2alpha1`, and classic acquisition.
> Full reasoning and the coupling to the other clocks: [the deprecation and removal notice](v1alpha1-deprecation.md#a-fourth-deprecation-on-a-different-clock-ciliumfqdn--calicofqdn).

> **Managed "Cilium" platforms usually do NOT accept the `cilium` backend.** The CRD test is literal: `kubectl get crd ciliumnetworkpolicies.cilium.io` must succeed.
> **GKE Dataplane V2's managed Cilium does not install it** (dropped since GKE 1.21.5-gke.1300) — use `--fqdn-policy-backend=gke` there, not `cilium`.
> If a selected backend's CRD is absent, the GMC's apply fails loudly and the `EgressProxy` goes `Degraded`; GitHub egress stays **denied** (the base NetworkPolicy default-denies it), never silently opened.
> The still-deferred cluster-scoped backends (AKS `CiliumClusterwideNetworkPolicy`, EKS `ClusterNetworkPolicy`) and OpenShift OVN `EgressFirewall` are tracked in the [Q245 plan](../plan/q245-fqdn-intent-backend-split.md).

> **The `gke` backend is additive-allow, so the base NetworkPolicy is load-bearing.** Unlike Cilium/Calico policies (which are self-default-denying), a GKE `FQDNNetworkPolicy` only *adds* an allow — it is a union with any NetworkPolicy on the same pod, not a default-deny.
> GAG's fail-closed guarantee therefore depends on the base standard NetworkPolicy (always emitted, GitHub-CIDR rule dropped in FQDN mode) staying present: the FQDN object widens the union to permit GitHub, and if it is absent or unenforced, GitHub egress stays denied by the base NP.
> **`gke`-backend enforcement is not yet live-validated on a real GKE cluster** (see the Q245 plan's live-validation follow-up); also note GKE's ~50-IP-per-FQDN resolution ceiling can intermittently drop egress for a wide wildcard like `*.blob.core.windows.net` under load — reserve FQDN egress for what an in-cluster caching mirror can't proxy.

**Secure-by-default guarantee.** Selecting an FQDN mode can never weaken egress: the standard NetworkPolicy still default-denies GitHub egress, so if the CNI-native policy is absent or unenforced, GitHub egress is denied rather than wide-open.
Confirm your CNI actually enforces DNS-based egress before relying on it (see the [network isolation validation](../design/network-architecture.md#how-to-validate-network-isolation) procedure).
`egressPolicyMode` has no effect when `managedNetworkPolicy: false`.

> FQDN mode is currently a v2 `EgressProxy` feature (the shared-egress surface).
> The v1 `ActionsGateway` proxy and v2 direct-egress (proxy-less) gateways stay on the CIDR path; if you need FQDN egress there, use the `managedNetworkPolicy: false` hand-off above.

### Pinning a tenant's proxy pool to its egress IP: `spec.scheduling`

A per-tenant egress IP is realized *below* Kubernetes: a node pool whose egress path (a per-range cloud NAT gateway, a dedicated subnet, a SNAT-ing gateway node) owns that IP.
So the pod's **node** decides which IP its traffic leaves by, and pinning the tenant's proxy pool to the tenant's node pool is what binds the two.

`EgressProxy.spec.scheduling` carries the standard `nodeSelector`, `tolerations`, `affinity`, `topologySpreadConstraints`, and `priorityClassName`; `ActionsGateway.spec.scheduling` does the same for the AGC control-plane pod.
(Worker pods have always had the full surface via `RunnerTemplate.spec.podTemplate`.)

```yaml
apiVersion: actions-gateway.com/v2beta1
kind: EgressProxy
metadata:
  name: shared
  namespace: tenant-a
spec:
  minReplicas: 2
  scheduling:
    # Pin to tenant-a's node pool -> tenant-a's Cloud NAT -> tenant-a's egress IP.
    nodeSelector:
      cloud.google.com/gke-nodepool: pool-tenant-a
    # Tolerate the taint that keeps other workloads off that pool.
    tolerations:
      - key: dedicated
        operator: Equal
        value: tenant-a
        effect: NoSchedule
```

**Anti-affinity precedence.** The GMC stamps a **required** cross-node `podAntiAffinity` on every proxy pool, so replicas land on distinct nodes and one node failure cannot take the pool down.
Composition with `spec.scheduling.affinity`:

| You set | Result |
|---|---|
| nothing | Built-in required cross-node spread (unchanged behaviour) |
| `nodeAffinity` and/or `podAffinity` | Applied **alongside** the built-in spread, which is preserved |
| `podAntiAffinity` (any non-nil value) | **Replaces** the built-in term entirely — set it and you own it |

That last row is the escape hatch a **single-node tenant pool** needs: the required built-in term would otherwise strand every replica after the first in `Pending`.
Either opt out of spreading explicitly —

```yaml
spec:
  scheduling:
    affinity:
      podAntiAffinity: {}   # explicit empty: no spreading at all
```

— or set `minReplicas: 1`.
A soft spread (`preferredDuringScheduling…`) is the middle ground: it prefers distinct nodes but still schedules on a one-node pool.

**Spreading across zones: `topologySpreadConstraints`.** For zonal (or any failure-domain) spread, use `spec.scheduling.topologySpreadConstraints` — the modern successor to the cross-node `podAntiAffinity`, able to express "spread across zones, tolerate a skew of 1" that anti-affinity cannot:

```yaml
spec:
  scheduling:
    topologySpreadConstraints:
      - maxSkew: 1
        topologyKey: topology.kubernetes.io/zone
        whenUnsatisfiable: ScheduleAnyway
        labelSelector:
          matchLabels: { app: proxy }
```

Unlike `affinity`, this field **composes** with the built-in cross-node anti-affinity — it never replaces it.
`podAntiAffinity: {}` stays the single opt-out for the built-in cross-node spread; `topologySpreadConstraints` adds a second axis on top.
**The `Pending` trap:** a *soft* zonal spread (`whenUnsatisfiable: ScheduleAnyway`) still inherits the *required* built-in cross-node anti-affinity, so replicas beyond the pinned pool's node count strand in `Pending` — the same escapes apply (`podAntiAffinity: {}`, or lower `minReplicas`).

> **These placement fields are CR-author-settable, by design.** `nodeSelector`, `tolerations`, `affinity`, and `topologySpreadConstraints` are not gated by a platform allowlist.
> Choosing an egress path is a feature — tenants should not have to share a rate limit or a block radius — and worker pods have always been able to do it.
> The consequence: if you attribute traffic by source IP and do not fully trust your tenant CR authors, **you** must constrain placement, at the pod admission layer.
> A validating allowlist of `nodeSelector` values is *unsound* (`nodeAffinity` supports `NotIn`/`DoesNotExist`, so "any pool but mine" is expressible); pinning `nodeSelector` by **mutation** is sound, because Kubernetes ANDs it with `nodeAffinity`.
> Ready-to-apply Kyverno and Gatekeeper samples: [admission-policies.md § Governing where GAG pods schedule](admission-policies.md#governing-where-gag-pods-schedule).
>
> **`priorityClassName` is the exception — it *is* gated** (see below).
> It is a cluster-wide, cross-tenant preemption lever, not a choice about the tenant's own traffic, so it sits behind its own allowlist.

**Prioritizing infra pods: `priorityClassName` (gated).** An evicted proxy pod takes that tenant's *entire egress path* down; an evicted AGC pod takes that tenant's control plane down.
`spec.scheduling.priorityClassName` raises these infra pods above best-effort workloads under node pressure.
Because a `PriorityClass` is a cluster-wide preemption lever — and `system-*` classes are nameable from *any* namespace, not just `kube-system` — this field is gated against a **separate, infra-only** allowlist (`--allowed-infra-priority-classes`) that must stay **disjoint** from the worker allowlist.
See [Priority classes: the two allowlists](#priority-classes-the-allowed-priority-classes-allowlist).

The full per-tenant egress-IP reference architecture — cloud-by-cloud primitives, cost model, and the live-validated GKE recipe — is in the [Q243 plan](../plan/q243-egress-ip-reference-arch.md).

---

## Worker egress destinations: the egress allowlist

By default a worker can reach **only GitHub** through its per-tenant egress proxy — the proxy pool's NetworkPolicy permits the GitHub endpoints and nothing else.
Jobs that fetch off-platform build dependencies (Go modules from `proxy.golang.org`, an internal artifact host, a cloud private-API endpoint) therefore fail on egress, not toolchain.
The egress allowlist (Q242 G.1) lets a **platform admin** open a small, explicit set of non-GitHub destinations while keeping per-tenant egress-IP attribution and the DNS-exfil containment intact.

### Prefer an in-cluster caching mirror first

For **remote third-party dependencies** (Go modules, npm, PyPI, crates, container layers), reach for an **in-cluster caching mirror** before the allowlist:

- **More secure** — workers egress only to a stable in-cluster pod; the worker NetworkPolicy never names an external destination.
  The mirror (not every worker) holds the narrow, auditable outbound path, and it can be pre-populated and run air-gapped.
- **Lower-maintenance** — upstream IPs and hostnames churn; a public-host allowlist rots and breaks builds when a registry shifts ranges.
  A mirror's address is stable forever.
- **Better behavior** — caching cuts repeat fetches, survives upstream outages, and gives a single audit point.

Per-ecosystem mirrors: **Athens** (Go module proxy), **Verdaccio** (npm), a registry **pull-through cache** (container images).
Reserve the destination allowlist for what a mirror genuinely cannot proxy: a *specific* live cloud-provider API (`kms.<region>.amazonaws.com`, a Private-Google-Access CIDR like `199.36.153.8/30`), internal services reachable only by IP, and one-off stable endpoints.
**Never** a wildcard like `*.googleapis.com` (it covers `storage.googleapis.com/<any-bucket>` and reopens broad exfil), and **not** the metadata/IMDS endpoint.

### How the allowlist works

The destinations are governed by two **platform-owned** GMC allowlists, mirroring the [`allowed-priority-classes`](#priority-classes-the-allowed-priority-classes-allowlist) model.
The `EgressProxy` is a tenant-authorable CR, so a tenant may *request* a destination, but only the platform decides what is permitted — a request outside the allowlist is rejected at admission:

- `--allowed-egress-fqdns` (chart: `allowedEgressFQDNs`) — permitted FQDN **suffixes**.
  A request matches if it equals or is a subdomain of an entry (allowing `golang.org` permits `proxy.golang.org`).
  Gates `EgressProxy.spec.destinationFQDNs`.
- `--allowed-egress-cidrs` (chart: `allowedEgressCIDRs`) — permitted IP ranges.
  A request matches by **subnet containment** (allowing `10.0.0.0/8` permits a requested `10.1.0.0/16`).
  Gates `EgressProxy.spec.destinationCIDRs`.

**Both empty (the default) forbids every non-GitHub destination** — the secure default.
GitHub is always allowed implicitly; the lists only add.

```yaml
# values.yaml — platform admin opens proxy.golang.org/sum.golang.org for Go builds.
allowedEgressFQDNs:
  - golang.org          # covers proxy.golang.org, sum.golang.org (subdomain match)
allowedEgressCIDRs: []  # none permitted
```

A tenant then requests the destination on their `EgressProxy`.
FQDN destinations require an FQDN `egressPolicyMode` (the CRD rejects `destinationFQDNs` in CIDR mode, since the pod-egress layer expresses them as `toFQDNs` rules); CIDR destinations work in any mode:

```yaml
apiVersion: actions-gateway.com/v2alpha1
kind: EgressProxy
metadata:
  name: shared
  namespace: team-a
spec:
  egressPolicyMode: FQDN   # (or the deprecated CiliumFQDN/CalicoFQDN); requires an operator --fqdn-policy-backend
  destinationFQDNs: [proxy.golang.org, sum.golang.org]   # rejected unless covered by --allowed-egress-fqdns
  destinationCIDRs: []                                   # rejected unless contained in --allowed-egress-cidrs
```

The GMC then (1) adds the destinations to the proxy pool's egress policy — FQDNs to the CNI-native `toFQDNs`/`domains` set, CIDRs as `ipBlock` peers on the standard NetworkPolicy — and (2) injects them into the proxy's CONNECT allowlist so the proxy permits exactly the GitHub set plus these destinations (an `EgressProxy` with no extra destinations stays transport-only, unchanged).
A denied CONNECT increments `actions_gateway_proxy_connect_denied_total`.

> **Allowlisting opens the *policy*, not the *route*.** A `destinationCIDRs` entry lifts the egress-policy block; it does not make the destination reachable.
> The Private-Google-Access VIPs in particular require subnet-level Private Google Access plus Cloud DNS and a route — cluster networking G.1 does not configure.

### Self-service additions via a watched ConfigMap

To add a permitted destination without editing the flags and rolling out the GMC, point the GMC at a ConfigMap in its own namespace whose `fqdns`/`cidrs` keys **augment** the static flags (additive and fail-safe — a missing or malformed ConfigMap leaves the flag allowlists in force):

```yaml
# values.yaml
egressDestinationAllowlist:
  configMapName: gmc-egress-destination-allowlist   # renders --egress-destination-allowlist-configmap
```

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: gmc-egress-destination-allowlist
  namespace: <gmc-namespace>
data:
  fqdns: |
    proxy.golang.org
    sum.golang.org
  cidrs: |
    199.36.153.8/30
```

Setting `configMapName` also renders a namespaced Role/RoleBinding granting the GMC get/list/watch on that one ConfigMap (no cluster-wide ConfigMap read).
The effective allowlist is the union of the flags and the ConfigMap, enforced by the EgressProxy validating webhook on every create/update — a destination outside it is rejected with a message naming the effective allowlist.

---

## Sharing an egress proxy across namespaces

By default an `EgressProxy` serves only its own namespace.
One proxy pool can serve several namespaces (a platform-operated central pool, or a set of cooperating teams), but only if the **proxy's owner** says so.

### Consent lives on the provider

Naming a proxy from the consumer side authorizes nothing.
The proxy owner lists the namespaces permitted to use it:

```yaml
apiVersion: actions-gateway.com/v2beta1
kind: EgressProxy
metadata:
  name: shared-pool
  namespace: platform-egress
spec:
  sharing:
    allowedNamespaces:
      - team-a
      - team-b
```

A consumer then names the proxy and its namespace:

```yaml
apiVersion: actions-gateway.com/v2beta1
kind: ActionsGateway
metadata:
  name: gateway
  namespace: team-a
spec:
  defaultProxyRef:
    name: shared-pool
    namespace: platform-egress
```

The same `namespace` field is available on `RunnerSet.spec.proxyRef`.
Omitting it means the referrer's own namespace, which is what every pre-existing manifest does, so nothing changes for them.

### What you must not assume

**A shared proxy is a shared egress identity.** Every namespace using the pool leaves GitHub from the same addresses, so per-tenant egress attribution no longer holds between them.
Share a proxy between tenants you are willing to treat as one for attribution purposes; give mutually-distrusting tenants their own pools.
The same limit reaches the [per-connection egress audit](#per-connection-egress-audit) record, which carries the pool's namespace rather than the consumer's: a CONNECT names no namespace, so on a shared pool the record says which destination was reached but not by whom.

**Only the public certificate crosses the boundary.** The GMC copies the proxy's certificate into each granted namespace as a ConfigMap named `proxy-share-<provider-namespace>-<proxy-name>`.
The private key stays in the provider namespace.
That ConfigMap is GMC-owned: do not edit or delete it, and do not create one by hand expecting it to grant access, since the GMC reconciles it back.

### Verifying a grant took effect

```bash
kubectl get configmap -n team-a -l actions-gateway/proxy-share=true
```

One entry per proxy `team-a` is entitled to.
If it is missing, the grant has not been accepted; check the consumer's condition:

```bash
kubectl get actionsgateway -n team-a gateway -o jsonpath='{.status.conditions}'
```

`Degraded` with reason `ProxyShareNotGranted` means the proxy exists but does not list this namespace.
Reason `ProxyNotFound` means the name or namespace is wrong.
Both fail closed: no worker pods run and no traffic is permitted while either is set.

Confirm the provider side admits the consumer:

```bash
kubectl get networkpolicy -n platform-egress shared-pool-proxy -o yaml
```

The ingress rules should carry one peer per granted namespace, each with **both** a `namespaceSelector` and a `podSelector` inside the same list entry.
Two separate entries would mean "any pod in that namespace, or any workload pod anywhere".
If you ever see that shape, treat it as a defect and report it.

### Revoking

Remove the namespace from `allowedNamespaces`.
The GMC deletes the projected ConfigMap and drops the ingress peer, and the consumer's references go `ProxyShareNotGranted`.

**Enforcement is immediate; one of the three status signals is bounded rather than instant.** The GMC watches the `EgressProxy`, so the projection and the provider's ingress peer both go on the reconcile that follows your edit.
From that moment no new connection from the consumer's workers is admitted, whatever their CRs still say.
An `ActionsGateway`'s `defaultProxyRef` reports `Degraded`/`ProxyShareNotGranted` on the same reconcile, because the GMC owns that resolution.
A `RunnerSet`'s `proxyRef` is resolved by the AGC instead, which may read the projection but not watch it (its Role grants `get` on ConfigMaps and not `list`/`watch`), so its `Ready` condition follows within **one minute**, on a re-check cadence rather than a watch.
Until it does the set keeps acquiring jobs, and their worker pods fail to reach the proxy rather than reaching it without a grant.

In-flight connections through the proxy end when their pods do, so drain the consumer's RunnerSets first if you need a clean cut.

---

## Tightening AGC apiserver egress: the `apiserver-cidrs` allowlist

The AGC pod holds the tenant's GitHub App private key and is the only workload that needs the Kubernetes API server.
The GMC therefore reconciles a NetworkPolicy (`actions-gateway-controller`) admitting AGC egress on the apiserver ports **443 and 6443**.
By default that rule has **no destination restriction** (any-dest): kube-proxy DNATs the `kubernetes` Service ClusterIP to a provider-specific apiserver IP *before* NetworkPolicy is evaluated, so a precise `ipBlock` is not portable and a wrong one silently severs the AGC's apiserver access (the PR #59 post-DNAT trap).
That breadth is a documented residual ([§5.2](../design/05-security.md#52-agc--proxy-level-threats-namespace-scoped)): a compromised AGC could in principle reach an arbitrary external HTTPS endpoint on 443, not just the apiserver.

**If your platform exposes the apiserver on a *stable* CIDR**, you can close that residual by scoping the rule to it.
Set the chart value `apiServerCIDRs` (passed to the GMC as `--apiserver-cidrs`) to the apiserver CIDR set:

```yaml
# values.yaml — opt-in tightening of the AGC apiserver-egress rule.
apiServerCIDRs:
  - "10.0.0.1/32"        # the apiserver's post-DNAT endpoint (single IP)
  - "172.16.0.0/12"      # or a CIDR covering the control-plane node pool
```

When set, the GMC scopes every tenant's AGC NetworkPolicy 443/6443 rule to these CIDRs via `ipBlock` peers (ports unchanged); when **empty (the default)** the rule stays any-destination — no behavior change.
This is an **opt-in tightening, never a loosening**, applied cluster-wide (the apiserver is the same endpoint for every tenant).
The GMC **validates each entry as a CIDR at startup and refuses to start on a malformed one**, so a typo fails fast rather than reconciling a NetworkPolicy the apiserver rejects.

### The one verification every platform shares

The value must cover the destination the AGC's apiserver traffic carries **after** kube-proxy DNAT — **not** the `kubernetes` Service ClusterIP (that virtual IP is DNAT'd away *before* NetworkPolicy is evaluated, so an `ipBlock` naming the ClusterIP matches nothing and severs apiserver access).
The post-DNAT destinations are exactly the **endpoints** of the `kubernetes` Service in the `default` namespace.
On every platform, confirm them before you scope:

```bash
kubectl get endpoints kubernetes -n default -o wide
# NAME         ENDPOINTS                          AGE
# kubernetes   10.0.12.34:443,10.0.34.56:443      90d
```

Those `ENDPOINTS` IP:port pairs are the literal targets the policy evaluator matches against.
Your `apiServerCIDRs` must contain a CIDR covering **all** of them, and **stay** covering them as the platform rotates.
Below is how to find a *stable* covering CIDR per platform — and where no stable CIDR exists, so you must leave the default.

### Finding the apiserver CIDR, by platform

| Platform | Where the apiserver lives after DNAT | Stable CIDR to scope to | Safe to scope? |
|---|---|---|---|
| **kind** | The control-plane container's IP on the kind Docker network, on **6443**. | The kind network subnet, e.g. `172.18.0.0/16` (`docker network inspect kind -f '{{(index .IPAM.Config 0).Subnet}}'`), or a `/32` per control-plane node. | **Yes** — local, you own the IPs. (Note kindnet does not *enforce* NetworkPolicy; scope it on a Calico/Cilium kind.) |
| **kubeadm / self-managed** | Control-plane node IP(s), on **6443**. | A `/32` per control-plane node, or the control-plane node subnet CIDR. For HA, list **every** control-plane node IP — a missed one strands the AGC whenever it lands on that apiserver. | **Yes**, if control-plane node IPs are static (the usual case). Re-confirm after any control-plane add/replace. |
| **Amazon EKS** | With **private endpoint access**, managed elastic network interfaces (ENIs) in *your* VPC subnets — private IPs you control. With **public-only** access, an AWS-managed public IP that AWS does **not** publish and may change. | The VPC CIDR, or the specific subnet CIDRs that host the cluster ENIs (`aws ec2 describe-subnets`). Verify against `kubectl get endpoints kubernetes`. | **Yes** for private-endpoint clusters. **No** for public-only — leave the default. |
| **Google GKE** | For **private clusters**, the control-plane private endpoint in the Google-managed master CIDR block (a `/28` you set at cluster creation). Public clusters use a Google-managed public IP that can change. | The master `/28`: `gcloud container clusters describe <c> --format='value(privateClusterConfig.masterIpv4CidrBlock)'`. | **Yes** for private clusters. **No** for public endpoint — leave the default. |
| **Azure AKS** | For **private clusters**, a private IP in your VNet (resolved via the cluster's private DNS zone). Public AKS uses an Azure-managed public IP behind the apiserver FQDN that may change. | The AKS node subnet CIDR (or the resolved private-endpoint IP as a `/32`); confirm with `kubectl get endpoints kubernetes`. | **Yes** for private clusters. **No** for public endpoint — leave the default. |

Rule of thumb: **a managed control plane is only safely scopable when you have put its endpoint inside a network range you own** (private/VPC/VNet access).
A public managed endpoint is a moving, provider-owned IP — scoping to a guessed range will eventually break, so keep the any-destination default there.

**The cost of getting it wrong.** A CIDR that is too narrow (or that the platform rotates out from under you) **breaks the AGC's apiserver access** — the AGC can no longer acquire jobs or manage worker pods for that tenant.
Symptom: AGC logs show apiserver dial timeouts after a rollout that introduced or changed `apiServerCIDRs`.
**Remedy: widen the CIDR or clear `apiServerCIDRs` to restore the any-destination default.** Treat this as any egress tightening — validate on one cluster before fleet-wide rollout, and re-confirm after control-plane scaling, upgrades, or IP changes.

Leave `apiServerCIDRs` unset unless you have a confirmed, stable apiserver CIDR — the any-destination default is bounded by the §5.2 compensating controls (key mounted read-only, never an env var; workers carry no apiserver egress at all; digest-pinned non-root AGC; all GitHub-bound traffic still through the proxy).

### Why GAG can't discover and tighten this for you (feasibility verdict)

A natural question is whether the GMC should just **read the `kubernetes` endpoints itself and scope every AGC NetworkPolicy automatically**, making the tightening the default.
We reviewed this for Q183 and **deliberately did not**, because an auto-tightened default would silently regress apiserver reachability on common platforms:

- **The IPs rotate, and a snapshot goes stale.** On managed control planes the endpoint IPs change on scaling, upgrades, and maintenance — without notice.
  A one-time discovery at provisioning time would tighten to a set that later stops matching, breaking the AGC.
- **Keeping it live needs a watch — with a lockout failure mode.** Staying correct means watching `endpoints/kubernetes` and re-reconciling every tenant's AGC policy on each change.
  There is always a race window where the policy lags a real IP rotation; during it the AGC (and potentially the GMC, which reaches the apiserver over the same path) is locked out of the very apiserver it needs to *repair* the policy — a self-inflicted control-plane deadlock.
  A tightening that can strand the controller maintaining it is not a safe default.
- **CNI rewrites can move the target again.** Some CNIs apply further SNAT/encapsulation, so even the endpoint IPs are not guaranteed to be what the policy evaluator ultimately matches — another reason a portable automatic value does not exist.

**Verdict:** automatic, default-on tightening is **not safe or portable**, so the any-destination rule stays the secure default and narrowing remains an operator-confirmed, per-cluster opt-in (the `apiServerCIDRs` allowlist above).
This honours secure-by-default without silently regressing reachability: the operator who *can* verify a stable CIDR closes the residual, and everyone else keeps a working AGC.
A controller-driven endpoint-watch that auto-narrows is recorded as a future enhancement gated on solving the rotation/lockout failure mode — see [appendix-g §G.10](../design/appendix-g-future-enhancements.md#g10-controller-discovered-apiserver-cidr-auto-narrowing).

---

## GitHub API base URL must be HTTPS

The AGC mints GitHub App installation access tokens by POSTing a signed App-JWT to the GitHub REST API and reading back a short-lived installation token.
Both the JWT (signed with the tenant's private key) and the returned token are credential material, so this exchange must never traverse a plaintext channel.

The endpoint host is taken from the **`GITHUB_API_BASE_URL`** environment variable, defaulting to `https://api.github.com` when unset.
On a GMC-provisioned AGC you do not set it: the GMC derives it from the gateway's `spec.gitHubURL` and injects it on the AGC `Deployment` — `https://api.github.com` for a `github.com` gateway, `https://<host>/api/v3` for GitHub Enterprise Server.
The token provider **rejects a non-HTTPS `GITHUB_API_BASE_URL` at startup** — the AGC (and the `probe`) will refuse to start with a clear error rather than leak credentials on the first token mint:

```
githubapp: refusing non-HTTPS GITHUB_API_BASE_URL "http://…":
GitHub App token exchange must use HTTPS to protect credentials in transit;
plaintext is permitted only under an explicit dev/test opt-in
```

This is **secure-by-default**: HTTPS is required with no configuration, an HTTPS value (including a GitHub Enterprise Server base such as `https://ghe.example.com/api/v3`) and the unset default both work, and the error names the offending URL but never any token or JWT material.

**Operator action:** on a GMC-provisioned gateway there is nothing to set — `spec.gitHubURL` is validated `https://` at admission, so the derived base is always HTTPS.
If you run the AGC or `probe` standalone and set `GITHUB_API_BASE_URL` yourself (e.g. for GitHub Enterprise Server), it must begin with `https://`.
A plaintext value will block startup.
Do not work around this by editing the deployment to inject a stub signal — the plaintext path exists only for the project's own in-cluster test fixtures.

**Documented dev/test trade-off.** The e2e suite points the AGC at an in-cluster `fakegithub` over plaintext (`http://<svc>.<ns>.svc.cluster.local:<port>`).
That path is permitted only by an explicit opt-in that production never carries: the AGC allows a plaintext base URL **only when the stub env `STUB_AUTH_URL` is set**, which a GMC-provisioned AGC receives solely via `AGC_EXTRA_*` under the testing-only `--allow-agc-extra-env` GMC flag.
When the opt-in is active the AGC logs `dev/test mode: allowing non-HTTPS GITHUB_API_BASE_URL for token exchange` at startup, so the relaxation is visible in the logs.
A production AGC has no stub env and therefore always enforces HTTPS.

---

## Priority classes: the `allowed-priority-classes` allowlist

A tenant can request scheduling priority for its worker pods in two places, and both land on the pod as `spec.priorityClassName`:

| Where a tenant sets it | On which kind |
|---|---|
| `priorityTiers[].priorityClassName` | v1 `ActionsGateway` / `RunnerGroup`, v2 `RunnerSet` |
| `podTemplate.spec.priorityClassName` | v1 `ActionsGateway.runnerGroups[]`, v2 `RunnerTemplate` |

`PriorityClass` is a **cluster-scoped** object carrying a priority *value* and a `preemptionPolicy` (Kubernetes default `PreemptLowerPriority`).
Left unvalidated, a tenant could name a high-priority, preempting class and have the scheduler **evict other tenants' running worker pods — and their egress proxies** — to schedule its own: a cross-tenant isolation break (Q132, Q289).

> **`system-cluster-critical` is not reserved for `kube-system`.** Kubernetes ships `system-cluster-critical` (value `2000000000`, `preemptionPolicy: PreemptLowerPriority`) and `system-node-critical` in every cluster, and **nothing restricts them to the `kube-system` namespace** — a pod in any namespace may name one.
> The allowlist is what stands between a tenant and cluster-wide preemption.
> Leave it unset (the default) unless you mean to grant it.

The platform owns which classes a tenant may use:

1. **The platform pre-creates the `PriorityClass` objects.** The GMC never creates cluster-scoped objects (same platform-ownership model as the `ResourceQuota`, Q130).
   Create allowlisted classes with **`preemptionPolicy: Never`** unless cross-tenant preemption is genuinely intended for that tier — `PriorityClass` is global, so a `PreemptLowerPriority` class lets *any* tenant that uses it evict *any* lower-priority pod cluster-wide, across tenant boundaries.

   ```yaml
   apiVersion: scheduling.k8s.io/v1
   kind: PriorityClass
   metadata:
     name: runner-standard
   value: 100000
   preemptionPolicy: Never   # orders ahead in scheduling without evicting others
   description: "Standard self-hosted runner worker pods."
   ```

2. **The platform allowlists the names on the GMC.** Set the `--allowed-priority-classes` flag (comma-separated) on the GMC controller.
   The validating webhook rejects — naming both the offending class and the permitted set — any `ActionsGateway` whose `runnerGroups[].priorityTiers[].priorityClassName` or `runnerGroups[].podTemplate.spec.priorityClassName` is off the list, any `RunnerTemplate` whose `podTemplate.spec.priorityClassName` is off the list, and any v2 `RunnerSet` whose `priorityTiers[].priorityClassName` is off the list.

   ```yaml
   # GMC Deployment / Helm values — args on the controller-manager container
   args:
     - --allowed-priority-classes=runner-standard,runner-opportunistic
   ```

   **An empty/unset allowlist forbids every named PriorityClass** (secure default): out of the box no tenant can set one.
   An *unset* `podTemplate.spec.priorityClassName` is always admitted — the default forbids named classes, not ordinary unprioritized worker pods.
   Tenants that only need a soft concurrency ceiling can use `maxWorkers` instead, which requires no `PriorityClass`.

The cluster-scoped `ClusterRunnerTemplate` is **exempt**: only the platform can create cluster-scoped objects, so it may name any class — the same reasoning that lets it declare privileged containers.

There is deliberately no tenant-settable per-tier `preemptionPolicy` field; preemption is governed entirely by the platform-created `PriorityClass` object.
See [§5.2 — Cross-Tenant Pod Preemption via PriorityClass](../design/05-security.md#52-agc--proxy-level-threats-namespace-scoped).

### Infra pods: the separate `allowed-infra-priority-classes` allowlist

`EgressProxy.spec.scheduling.priorityClassName` and `ActionsGateway.spec.scheduling.priorityClassName` prioritize the **infra** pods — the proxy pool and the AGC control plane — and are gated by a **second, infra-only** allowlist, `--allowed-infra-priority-classes` (chart: `allowedInfraPriorityClasses`).
It behaves exactly like the worker allowlist (comma-separated names, empty-forbids-all secure default, unset name always admitted), but it is a **distinct set that must never intersect the worker allowlist**.

**Why not one allowlist.** Infra pods need to sit *above* workers — that is the whole point of prioritizing them.
If you reused `--allowed-priority-classes` and added a high, preempting class so an `EgressProxy` could name it, that same class would become nameable from a **worker** pod (`RunnerTemplate.podTemplate.spec.priorityClassName`).
Any tenant could then lift its *workers* to infra priority and **preempt other tenants' proxy pods** — the gate meant to protect the proxy becomes the mechanism for evicting it.
The two allowlists exist precisely so an infra class is unreachable from any worker surface.

**The GMC enforces disjointness at startup.** If a class appears in both `--allowed-priority-classes` and `--allowed-infra-priority-classes`, the GMC **refuses to boot** — converting a silent priority inversion into a loud configuration error.
Keep the two sets separate; a good convention is an `infra-` name prefix.
The flags are not the only route to an overlap once the watched CR is in play; see [Disjointness is enforced on every edit](#disjointness-is-enforced-on-every-edit-not-only-at-startup).

```yaml
# GMC Deployment / Helm values — args on the controller-manager container
args:
  - --allowed-priority-classes=runner-standard,runner-opportunistic
  - --allowed-infra-priority-classes=gag-infra-critical   # DISJOINT from the above
```

Create the infra class with a value **above** the worker classes so the scheduler keeps infra pods when a node is contended:

```yaml
apiVersion: scheduling.k8s.io/v1
kind: PriorityClass
metadata:
  name: gag-infra-critical
value: 1000000000            # well above the runner-* classes
preemptionPolicy: Never      # order ahead without evicting others, unless you mean to
description: "GAG per-tenant egress proxy and AGC control-plane pods."
```

### Upgrading: previously ungated `priorityClassName` fields are now gated

Two of the tenant-reachable routes shipped before their gate did: `podTemplate.spec.priorityClassName` was accepted unvalidated on `RunnerTemplate` and `ActionsGateway.runnerGroups[]`, and `priorityTiers[].priorityClassName` was accepted unvalidated on the v2 `RunnerSet`.
Both are now rejected unless allowlisted, so **an existing CR that names an off-allowlist class will fail its next `create`/`update`** (admission webhooks do not re-validate already-stored objects, so nothing breaks until the CR is edited).

Before upgrading, find the affected objects and either add their class to `--allowed-priority-classes` or remove the field:

```bash
# v2 RunnerTemplates
kubectl --context "$CTX" get runnertemplates -A -o json | jq -r '
  .items[]
  | select(.spec.podTemplate.spec.priorityClassName != null)
  | "\(.metadata.namespace)/\(.metadata.name) -> \(.spec.podTemplate.spec.priorityClassName)"'

# v2 RunnerSets (each priorityTiers[] entry names a class)
kubectl --context "$CTX" get runnersets.actions-gateway.com -A -o json | jq -r '
  .items[]
  | .metadata.namespace as $ns | .metadata.name as $name
  | (.spec.priorityTiers // [])[]
  | "\($ns)/\($name) -> \(.priorityClassName)"'

# v1 ActionsGateways (the class lives on each runnerGroups[] entry)
kubectl --context "$CTX" get actionsgateways.actions-gateway.github.com -A -o json | jq -r '
  .items[]
  | .metadata.namespace as $ns | .metadata.name as $name
  | (.spec.runnerGroups // [])[]
  | select(.podTemplate.spec.priorityClassName != null)
  | "\($ns)/\($name) -> \(.podTemplate.spec.priorityClassName)"'
```

Any class these print that is **not** in your `--allowed-priority-classes` was an open cross-tenant preemption lever; treat a `system-cluster-critical` or `system-node-critical` result as an incident, not a config change.

### Self-service additions via the `PriorityClassAllowlist` CR (Q188, Q298)

Editing `--allowed-priority-classes` and rolling out the GMC for every new class is slow.
The GMC **also** sources both allowlists from a cluster-scoped `PriorityClassAllowlist` CR it watches, so a platform admin can add an allowed class without a flag edit or restart — the change takes effect on the next watch event.

One object carries both sets, in two lists that mirror the two flags:

| CR field | Augments the flag | Gates |
|---|---|---|
| `spec.allowedPriorityClasses` | `--allowed-priority-classes` | worker pods (`priorityTiers[]`, `podTemplate.spec`) |
| `spec.allowedInfraPriorityClasses` | `--allowed-infra-priority-classes` | infra pods (`EgressProxy` / `ActionsGateway` `spec.scheduling`) |

The chart always renders this object (named `<release>-priorityclass-allowlist`) from `allowedPriorityClasses` and `allowedInfraPriorityClasses`, and always points the GMC at it.
The **same object is the [`priorityclass-allowlist-guard`](#defense-in-depth-the-priorityclass-allowlist-guard-policy-q289) policy's parameter**, so the webhook and the policy read one source and cannot drift — there is no superset discipline to remember.
The policy reads only `allowedPriorityClasses`: its job is the direct-write path to `runnergroups`, which carry no infra scheduling block, and the two infra kinds have `failurePolicy: fail` webhooks of their own.

!!! note "This replaced a watched ConfigMap (Q492)"
    `priorityClassAllowlist.configMapName` is removed; setting it now fails the Helm render with a migration message.
    A ConfigMap could not remain the policy's `paramKind` — a kube-apiserver defect permanently breaks param resolution for any *core-type* `paramKind` (see [below](#param-resolution-used-to-break-cluster-wide-q444-fixed-in-q492)).
    Migration steps: [upgrade.md](upgrade.md#priorityclass-allowlist-configmap-to-cr).

**Add a class without a restart.** Edit the object in place:

```bash
kubectl edit priorityclassallowlist gmc-priorityclass-allowlist
```

```yaml
apiVersion: actions-gateway.com/v2beta1
kind: PriorityClassAllowlist
metadata:
  name: gmc-priorityclass-allowlist
spec:
  allowedPriorityClasses:
    - runner-bursty
    - runner-batch
  allowedInfraPriorityClasses:      # DISJOINT from the list above
    - gag-infra-high
```

The **effective allowlist for each webhook is the union** of its static flag and the matching list here: the CR can only *widen* an allowlist, never remove a flag-pinned class.
You still pre-create each `PriorityClass` object first (step 1 of the previous section) — the allowlist only governs which *names* a tenant may reference.

#### Disjointness is enforced on every edit, not only at startup

The startup check reads the two flags.
Once either list can also be edited in a CR, that check alone is not enough: an edit could put one class on both surfaces long after the GMC booted.
Three gates hold the invariant, and the first one you hit depends on how the overlap was written.

| Where the overlap comes from | What stops it | What you see |
|---|---|---|
| Both flags name the class | Startup check | The GMC **refuses to boot** with the shared names |
| Both CR lists name the class | CRD CEL rule | `kubectl apply` is **rejected**: `allowedPriorityClasses (worker) and allowedInfraPriorityClasses must be disjoint` |
| One CR list collides with the *other* flag | GMC reconciler | The write is admitted (CEL cannot read a controller flag), then **both dynamic sets are dropped** and the GMC logs `WARNING: PriorityClassAllowlist would make the worker and infra allowlists intersect` with `sharedClasses` |
| Anything that got past all three | Admission read path | A class on both allowlists is admitted on **neither** surface |

The third row is the one to recognize in practice, because the `kubectl apply` **succeeds**.
The GMC then falls back to the two flag allowlists — so the symptom is that *every* recently self-serviced class, worker and infra alike, stops being accepted at once.
That is deliberate: a partially applied pair is how an overlap becomes real, so the object is refused whole.
Grep the GMC log for `sharedClasses`, remove the named class from one of the two lists, and the next watch event restores both dynamic sets.

!!! warning "A chart upgrade reasserts both lists over this object"
    An in-place edit is the fast path, not the durable one.
    Persist any class you intend to keep in the chart's `allowedPriorityClasses` / `allowedInfraPriorityClasses` values.

!!! note "Setting `allowedInfraPriorityClasses` for the first time needs the CRD step"
    The field is new in Q298.
    Helm never reapplies the chart-root `crds/` dir on upgrade, so a release upgrading from before it existed still has a CRD whose schema lacks the field.
    The chart omits the key while the value is empty, so a default upgrade is unaffected; set it, and apply the CRDs first — [upgrade.md](upgrade.md#gmc-install-and-upgrade-via-helm-recommended).

**One caveat the flag does not share.** The *policy* sees only this object, never the flag.
A class listed in `--allowed-priority-classes` but absent here is admitted by the webhook and denied by the policy on the direct-write path.
The chart renders both from one value, so this only arises if you edit the object down by hand.

**Fail-safe behavior.** The allowlist is a cross-tenant-isolation guardrail, so a broken object must never silently widen it:

- The CRD schema constrains every entry in **both** lists to a valid DNS-1123 subdomain and each list to a set, so a malformed name is **rejected at write time** — it never reaches the GMC or the policy.
- A **missing or deleted** object, or any invalid entry that predates the schema, causes the GMC to fall back to the **static flag allowlists only** and log a warning.
- An invalid list is rejected **wholesale**, and across both lists — a valid name sitting next to a typo is *not* partially applied, and a good infra addition does not ride in on an object whose worker list is malformed.
- An **absent or empty** list is valid and simply adds nothing.

Because the dynamic sets are additive and reset to the static bases on any error, the worst case is that recently-added self-service classes stop being accepted until it is fixed — never that an unintended class becomes allowed.

**RBAC.** The chart grants the GMC a `ClusterRole` with `get`/`list`/`watch` on `priorityclassallowlists` and nothing else.
The kind is cluster-scoped so the role must be too, but it carries **no write verb**: the GMC can never widen its own allowlist.

### Narrowing the allowlist: drain stored references first

Adding a class is safe in any order; **removing one is not**.
The [guard policy](#defense-in-depth-the-priorityclass-allowlist-guard-policy-q289) re-validates the whole stored object on every update — and the GMC webhooks do the same on the tenant-facing kinds.
Once a class leaves the allowlist, **every spec-changing write to a stored object still naming it is denied**: the GMC's reconcile writes freeze, and the object cannot be edited except to move off the class.
This holds for every narrowing route — the chart's `allowedPriorityClasses` value, the `--allowed-priority-classes` flag, or an in-place edit of the `PriorityClassAllowlist` CR.

**Teardown is exempt** (Q518): both the guard policy and the webhooks admit a *deletion-only* update — one whose object already carries a `deletionTimestamp` and whose spec is unchanged, which is what the AGC's finalizer-removal write looks like — so deleting a tenant that still names a removed class completes normally.
On chart versions without this exemption, that same teardown write was denied and the namespace hung in `Terminating` ([recovery](troubleshooting.md#tenant-namespace-stuck-terminating-after-narrowing-the-priorityclass-allowlist)).
The exemption admits nothing new: the write it admits stores a spec byte-identical to the one already stored, and a spec *change* on a deleting object is still validated (design rationale: [05-security.md § Deletion-only updates](../design/05-security.md#deletion-only-updates-are-exempt-from-re-validation-q518)).

Narrow in this order:

1. **Enumerate stored references** to the class you are removing.
   The three commands under [Upgrading](#upgrading-previously-ungated-priorityclassname-fields-are-now-gated) cover the tenant-authored kinds; also check the GMC-authored `RunnerGroup`s, which store the class too:

   ```bash
   kubectl --context "$CTX" get runnergroups.actions-gateway.github.com -A -o json | jq -r '
     .items[]
     | .metadata.namespace as $ns | .metadata.name as $name
     | ((.spec.priorityTiers // [])[].priorityClassName),
       (.spec.podTemplate.spec.priorityClassName // empty)
     | "\($ns)/\($name) -> \(.)"'
   ```

2. **Move every referencing object off the class** — switch it to a still-allowed class or drop the field.
   Edit the tenant-facing CR (`ActionsGateway`, `RunnerSet`, `RunnerTemplate`) and let the GMC reconcile the `RunnerGroup`s.
   Admission validates the *incoming* object, so an update that removes the reference always passes — this step also works as the un-wedge path after a premature narrowing.
   Re-run step 1 until it prints nothing.

3. **Remove the class from the allowlist** — take it out of the chart's `allowedPriorityClasses` value (which renders both the `--allowed-priority-classes` flag and the `PriorityClassAllowlist` CR) and upgrade the release.

4. **Delete the `PriorityClass` object last**, if you are retiring it entirely.
   Deleting it earlier does not tighten admission (the allowlist gates names, not objects) but does break scheduling for any worker pod still created with the name — the apiserver rejects pods naming a nonexistent class.

### Defense-in-depth: the `priorityclass-allowlist-guard` policy (Q289)

The webhooks above gate every *tenant-facing* CR, but `RunnerGroup` CRs have no webhook of their own — they are normally authored only by the GMC from an already-validated `ActionsGateway`.
A principal granted **direct `runnergroups` write RBAC** would therefore bypass the allowlist entirely.
The chart ships a `ValidatingAdmissionPolicy` (`<release>-priorityclass-allowlist-guard`, on by default under `admissionPolicy.enabled`, Kubernetes ≥ 1.30) that rejects any `runnergroups` create/update — from **any** writer, the GMC included — whose `priorityTiers[].priorityClassName` or `podTemplate.spec.priorityClassName` is off the allowlist.
Unlike a webhook, the policy also **re-validates every write to an existing object**, so a pre-gate stored RunnerGroup naming an off-allowlist class is caught on its next update, not just its next re-create.
The flip side: removing a class that stored objects still name freezes every later spec-changing write to them.
Deletion-only updates (deletionTimestamp set, spec unchanged — the finalizer-removal write teardown depends on) are exempt via the policy's `exclude-deletion-only-updates` match condition (Q518), so narrowing can freeze a live object but can no longer wedge its deletion — see [Narrowing the allowlist](#narrowing-the-allowlist-drain-stored-references-first).

The policy matches the v2 kinds too (Q323): `runnersets` (`priorityTiers[].priorityClassName`) and `runnertemplates` (`podTemplate.spec.priorityClassName`), across v2alpha1 and v2beta1.
Those kinds are already gated by `failurePolicy: Fail` GMC webhooks, so for them the policy is defense-in-depth — coverage while the webhook is unavailable or bypassed, plus the stored-object re-validation a webhook cannot provide.
`ClusterRunnerTemplate` is exempt (cluster-scoped, platform-authored — its writers can create `PriorityClass` objects anyway).

The policy reads its allowlist from the cluster-scoped **`PriorityClassAllowlist` CR** `<release>-priorityclass-allowlist` (the apiserver cannot read the GMC flag).
The chart renders that object from `allowedPriorityClasses` — the same value that feeds `--allowed-priority-classes` — and the GMC watches the very same object, so the webhook and the policy cannot drift.
See [Self-service additions](#self-service-additions-via-the-priorityclassallowlist-cr-q188-q298).

**Why a CRD and not a ConfigMap (Q492).** A `paramKind` on a *core* type is destroyed by a kube-apiserver defect the moment the set of bindings naming it goes empty — which `helm uninstall` does.
See [below](#param-resolution-used-to-break-cluster-wide-q444-fixed-in-q492).

**Failure mode.** The binding uses `parameterNotFoundAction: Deny`: if the parameter object is **deleted**, every matched write — `runnergroups`, `runnersets`, and `runnertemplates` alike — is denied (`no params found for policy binding`) until it is recreated — loud and fail-closed rather than silently off.
The GMC surfaces this as provisioning errors on affected gateways; recreate the object (or re-run the chart upgrade) to recover.
Deleting it does not affect running workloads.

#### Param resolution used to break cluster-wide (Q444, fixed in Q492)

Releases before the `PriorityClassAllowlist` migration used a **ConfigMap** as the policy's `paramKind`, and were exposed to a kube-apiserver defect that denied every matched write cluster-wide:

```text
denied request: failed to configure binding: no params found for policy binding
with `Deny` parameterNotFoundAction
```

…while the ConfigMap plainly existed at the referenced name and namespace.
Because `parameterNotFoundAction: Deny` resolves parameters **before** any per-object matching, that denied *every* matched write — `runnergroups`, `runnersets`, `runnertemplates`, class-naming or not.
It could also fail **open**, silently enforcing a stale allowlist from a frozen informer cache.

The trigger is deleting the policy's **binding**: the apiserver keys one *shared* parameter informer per core-type `paramKind`, tears it down once no binding names that kind, and never restarts it.
`helm uninstall` deletes the guard's only binding, so a later reinstall could not repair it — the only recovery was a kube-apiserver restart, unavailable on EKS/GKE/AKS.
Observed on Kubernetes 1.35.5 and 1.36.1.

**What fixes it.** For a *CRD* `paramKind` the apiserver allocates a fresh dynamic informer per context, so the same transition is survivable.
Moving the parameter to the cluster-scoped `PriorityClassAllowlist` CR (Q492) therefore removes the exposure at its root rather than mitigating it.
The contrast is measured directly — the same empty-binding-set transition, one GVK apart — by [`scripts/e2e/vap-param-informer-check.sh`](https://github.com/actions-gateway/github-actions-gateway/blob/main/scripts/e2e/vap-param-informer-check.sh), and the uninstall/reinstall cycle is now a CI gate (`scripts/e2e/chart-reinstall-check.sh`).
Mechanism and measurements: [`q444-vap-param-resolution.md`](../plan/archive/q444-vap-param-resolution.md).

**If you are still on an affected release**, upgrade in place — `helm upgrade` never removes the binding and is safe — and see [upgrade.md](upgrade.md#priorityclass-allowlist-configmap-to-cr).
If you are already in the broken state, restart kube-apiserver where you can; otherwise set `admissionPolicy.enabled=false` to restore writes (you lose the backstop until the apiserver recycles), then upgrade.

**The benign window is not this defect.** A reinstall removes the parameter object and recreates it, and for a second or two in between the guard correctly fails closed with the same message.
That clears on its own.


---


## License attribution in images

The compiled binaries statically link third-party Go modules (MIT/BSD/Apache/ISC/…), whose licenses require reproducing their copyright/notice text wherever the binaries are redistributed — and a container image is a redistribution (Apache-2.0 §4(d), the MIT/BSD reproduce-the-notice clauses).
Each of the four production images therefore ships its license attribution under **`/licenses/`**, the [Red Hat/OpenShift container-certification](https://github.com/redhat-openshift-ecosystem/openshift-preflight) convention, which pairs with the `org.opencontainers.image.licenses="Apache-2.0"` label every image already carries.

- **What is bundled.** `/licenses/` in the `agc`, `gmc`, `proxy`, and `worker` images contains three files:
  - `LICENSE` — the project's own Apache-2.0 license.
  - `NOTICE` — the project's Apache-style copyright/attribution notice.
  - `THIRD-PARTY-NOTICES` — the aggregated license and notice texts of every vendored module statically linked into the binary.

  The `worker` image is built on the upstream `actions-runner` base, which carries its own license files for its components; the `/licenses/` files we add cover only the wrapper binary and its dependencies.

- **Inspect it on a running pod.** The files are plain text at `/licenses/`.
  The worker image ships a shell + coreutils + tar, so `exec`-cat and `kubectl cp` work on a worker pod:

  ```bash
  kubectl exec -n <tenant-ns> <worker-pod> -- cat /licenses/LICENSE
  kubectl cp <tenant-ns>/<worker-pod>:/licenses/THIRD-PARTY-NOTICES ./THIRD-PARTY-NOTICES
  # The agc/gmc/proxy images are distroless (no shell, cat, or tar), so neither
  # exec-cat nor kubectl cp works on them — read their /licenses from the repo
  # root (THIRD-PARTY-NOTICES) or from the image with an OCI tool (crane/skopeo).
  ```

- **How it is kept current.** `THIRD-PARTY-NOTICES` lives at the repo root and is **generated and committed** by `make third-party-notices` ([`scripts/release/gen-third-party-notices.sh`](../../scripts/release/gen-third-party-notices.sh)), which concatenates the `LICENSE`/`NOTICE`/`COPYING` files of every module under the committed, version-pinned `vendor/` tree — offline, no network or module cache.
  The CI `license-notices` workflow runs `make third-party-notices-check` on every change to `vendor/**` (or the generator) and fails the PR if the committed file is stale, so a dependency add/remove/bump cannot ship without refreshed attribution.

---

## Image provenance: signature & SBOM verification

The six first-party images (`gmc`, `agc`, `proxy`, `worker`, `wrapper`, `build-runner`) are published to GHCR by the [`publish.yml`](../../.github/workflows/publish.yml) workflow on every `v*` release tag (the maintainer-facing cut-a-release procedure is in [release.md](release.md)).
Each one is:

- **Multi-arch** (`linux/amd64` + `linux/arm64`): the published ref is an OCI image **index**; the digest you pin at install time is the index digest, and the kubelet resolves the node's per-arch manifest from it at pull time.
- **Signed keyless with [cosign](https://docs.sigstore.dev/).** There is no signing key to distribute or rotate — the signature is bound to a short-lived [Fulcio](https://docs.sigstore.dev/certificate_authority/overview/) certificate issued against the GitHub Actions OIDC identity of the publish workflow and recorded in the public [Rekor](https://docs.sigstore.dev/logging/overview/) transparency log.
  You verify *who signed it* (the workflow identity), not a key you have to trust out-of-band.
  Signing is **recursive**: the index *and* each per-arch manifest carry a signature, so verification succeeds against the pinned index digest and also against a per-arch manifest digest (e.g. an image mirrored or referenced by platform-specific manifest).
- **Carrying a signed SLSA build-provenance attestation** (from [`actions/attest-build-provenance`](https://github.com/actions/attest-build-provenance)), attached to the index digest as an OCI referrer through the same keyless Fulcio/Rekor path.
  It records *how* the image was built — the workflow, repo, commit, and trigger — and is authenticated, so a pusher can't forge it.
  This meets **SLSA Build L2**; buildx's unsigned default provenance is disabled in favour of it (full SLSA Build L3 would need an isolated reusable-workflow builder, not yet adopted).
- **Accompanied by an SPDX-JSON SBOM per architecture** (generated with [syft](https://github.com/anchore/syft)) attached as a cosign attestation to that architecture's manifest, so you can enumerate exactly what shipped in the image your nodes actually run.

### Verify a signature

Before deploying — or as a forensic step when investigating a suspected image swap (the "Verify the image" step in the [compromised-AGC](#suspected-compromised-agc-tenant-scoped) and [compromised-GMC](#suspected-compromised-gmc-cluster-scoped-tier-0) playbooks) — confirm the image was signed by *this project's* publish workflow:

```bash
# Pin the identity to the publish workflow on a release tag, and the issuer to
# GitHub's OIDC provider. A signature from any other identity (or none) fails.
cosign verify \
  --certificate-identity-regexp '^https://github.com/actions-gateway/github-actions-gateway/\.github/workflows/publish\.yml@refs/tags/v.*$' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
  ghcr.io/actions-gateway/gmc:<tag-or-digest>
```

- **A `cosign verify` failure is a stop-ship / incident signal.** It means the image was not signed by the publish workflow — a locally built, tampered, or third-party image.
  Do not deploy it; if it is already running, treat it as a suspected supply-chain compromise (isolate per the playbooks above).
- Always verify by **digest** (`@sha256:…`) for the running workload — a tag is mutable; the digest is the bytes.
  `kubectl get pod <p> -o jsonpath='{.status.containerStatuses[*].imageID}'` gives the digest actually pulled.
- The same `--certificate-identity-regexp` / `--certificate-oidc-issuer` pair is what a cluster-admission policy engine (Kyverno `verifyImages`, Sigstore policy controller) should enforce so unsigned images can't run at all — that cluster-wide enforcement is the operator's to configure (the gateway does not ship it, mirroring the registry-allowlist split in [§5.2 Supply-Chain](../design/05-security.md#52-agc--proxy-level-threats-namespace-scoped)).

### Verify build provenance

The build-provenance attestation answers *how and where* the image was built — the complement to the signature's *who signed it*.
It is bound to the **index** digest (unlike the per-arch SBOM attestations), so a tag or index-digest reference verifies directly.
The one-command check uses the GitHub CLI:

```bash
# Confirms the signed SLSA provenance was minted by this project's publish
# workflow. Resolve the tag to the index digest first for a workload check.
gh attestation verify oci://ghcr.io/actions-gateway/gmc:<tag-or-digest> \
  --repo actions-gateway/github-actions-gateway \
  --signer-workflow actions-gateway/github-actions-gateway/.github/workflows/publish.yml
```

> **Scripting this check?** `gh attestation verify` prints its summary only to a terminal.
> Redirected or captured it emits nothing while still exiting 0, so a passing verification is indistinguishable from one that checked nothing.
> In a pipeline, add `--format json` and assert on the result — `.[0].verificationResult.signature.certificate.buildSignerURI` must name this repo's `publish.yml` at a `refs/tags/v*` ref.

`cosign` verifies the same attestation against the keyless identity, matching the signature/SBOM commands above (the predicate type is the SLSA provenance v1 in-toto type):

```bash
cosign verify-attestation --type slsaprovenance1 \
  --certificate-identity-regexp '^https://github.com/actions-gateway/github-actions-gateway/\.github/workflows/publish\.yml@refs/tags/v.*$' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
  ghcr.io/actions-gateway/gmc:<tag-or-digest> \
  | jq -r '.payload | @base64d | fromjson | .predicate.buildDefinition.externalParameters'
```

- **A provenance verification failure is a stop-ship / incident signal**, exactly like a `cosign verify` failure: the image was not built-and-attested by the publish workflow.
  The same identity pair is what an admission policy engine can enforce so only images with valid provenance run.
- The provenance is **authenticated** (signed via Fulcio, logged in Rekor), which is the property buildx's unsigned default provenance lacks — that is why the pipeline disables the default and emits this signed one instead.

### Retrieve and inspect the SBOM

The SBOMs ride with the image as signed attestations — **one per architecture, attached to that architecture's manifest digest** (not to the index, so the SBOM you audit is exactly what your nodes run) — and are also uploaded as build artifacts on each publish run.
To pull and inspect one from the registry, resolve the per-arch digest from the index first:

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

PR CI ([`security-scan.yml`](../../.github/workflows/security-scan.yml)) builds each image and generates the same SBOM as a build artifact, so SBOM generation is exercised on every code PR — but **signing and attestation run only on publish** (they need a registry push and the publish workflow's OIDC identity).
A green PR therefore proves the image builds and the SBOM generates; it does not exercise the cosign sign/attest path.

---

## Reference Links

- [Threat model (05-security.md)](../design/05-security.md) — the abuse heuristics this runbook operationalises
- [observability.md](observability.md) — full metrics reference and SLO alerting
- [runbook.md](runbook.md) — incident response and day-2 operations
- [troubleshooting.md](troubleshooting.md) — symptom → diagnosis → remediation
- [Security review findings (plan/security.md)](../plan/security.md) — per-finding status, including H-2 and the audit-policy follow-on
