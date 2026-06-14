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
- [Response playbooks](#response-playbooks)
  - [Suspected compromised AGC (tenant-scoped)](#suspected-compromised-agc-tenant-scoped)
  - [Suspected compromised GMC (cluster-scoped, Tier-0)](#suspected-compromised-gmc-cluster-scoped-tier-0)
  - [Proxy saturation / slowloris](#proxy-saturation--slowloris)
- [Posture scanning (preventive)](#posture-scanning-preventive)
  - [Manifest posture — polaris (automated, in CI)](#manifest-posture--polaris-automated-in-ci)
  - [CIS-benchmark posture — kube-bench (manual, pre-production)](#cis-benchmark-posture--kube-bench-manual-pre-production)
- [Tenant egress posture & deliberate widening](#tenant-egress-posture--deliberate-widening)
- [License attribution in images](#license-attribution-in-images)
- [Image provenance: signature & SBOM verification](#image-provenance-signature--sbom-verification)
  - [Verify a signature](#verify-a-signature)
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
backend. A sample policy and its wiring are tracked as a
separate deliverable; this runbook specifies *what to alert on* once that
policy is in place.

| Detection | Audit predicate | Why it matters | Response |
|---|---|---|---|
| **AGC full-body Secret list** | `verb=list resource=secrets` by the AGC ServiceAccount (`system:serviceaccount:<tenant-ns>:actions-gateway-controller`) returning object bodies | Legit AGC code lists Secret *metadata* only ([H-2 residual](../plan/security.md)). A body `list` means out-of-band enumeration of user-managed Secrets. | Treat the AGC as compromised: cordon the tenant namespace, rotate the GitHub App key (runbook.md § GitHub App Key Compromise), inspect the AGC image. |
| **AGC Secret access outside its label scope** | `verb=get resource=secrets` by the AGC SA for Secret names not matching `actions-gateway/runner-group=*` or the AGC's `gitHubAppRef` | The AGC only needs its agent-pool and payload Secrets. A `get` on a developer's `ghcr-pull-token` is exfiltration. | As above. |
| **GMC Secret reads beyond reconcile cadence** | `verb=get resource=secrets` by the GMC SA (`system:serviceaccount:gmc-system:gmc-controller-manager`) at a rate far above the reconcile/requeue cadence | The GMC reads each `gitHubAppRef` Secret only during reconcile (cache-bypassed `Get`). A high `get` rate is credential harvesting. | Treat the GMC as a Tier-0 compromise: isolate the GMC pod, rotate **all** tenant GitHub App keys, audit which Secrets were read. |
| **GMC namespace-PSA escalation attempt** | `namespace-psa-guard` ValidatingAdmissionPolicy `deny` events for the GMC SA | The guard ([§5.3](../design/05-security.md#53-security-profiles-and-the-privileged-opt-in)) blocks the GMC relabelling non-tenant namespaces (e.g. `kube-system` → `privileged`). A denial means the GMC tried. | A denial is a successful block, but a *signal* of compromise. Isolate the GMC and investigate. |
| **GMC out-of-tenant resource write** | `gmc-tenant-resource-guard` ValidatingAdmissionPolicy `deny` events for the GMC SA | The guard blocks the GMC creating/updating/deleting Deployments, RoleBindings, Secrets, NetworkPolicies, etc. in any namespace not marked `actions-gateway.github.com/tenant=true` (e.g. a Deployment or Secret into `kube-system`). A denial means the GMC tried (Q121/Q122). | A denial is a successful block but a *signal* of compromise. Isolate the GMC and investigate. |
| **GMC workload creation outside reconcile** | `verb=create resource=deployments|roles|rolebindings` by the GMC SA in a *marked tenant* namespace with no corresponding `ActionsGateway` CR change | The `gmc-tenant-resource-guard` VAP already blocks writes into *unmarked* namespaces; this catches the residual — provisioning inside a legitimate tenant namespace without a triggering CR edit, which is lateral movement within the GMC's confined scope. | Isolate the GMC; diff provisioned resources against live `ActionsGateway` CRs. |

Until the audit policy lands, these threats are mitigated structurally
(RBAC scope, cache-bypass, the `namespace-psa-guard` and
`gmc-tenant-resource-guard` VAPs, no Secret informer) but write-confinement
denials aside are **not observable** — there is no alert that fires if a
compromised binary exercises its standing *read* permissions. Closing that
gap is the value of the audit policy. Note the two VAPs confine GMC *writes*
(create/update/delete) only; Secret *reads* (`get`/`list`/`watch`) cannot be
gated at admission and remain cluster-wide at the RBAC layer — the audit
policy is the only detective control for them (see
[§5.1](../design/05-security.md#51-gmc-level-threats-cluster-scoped)).

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

If instead you want to take over the **proxy's** own egress policy (for example
to express GitHub egress as FQDN rules under Cilium/Calico), set
`spec.proxy.managedNetworkPolicy: false` on the `ActionsGateway` — the GMC then
stops managing the proxy GitHub-CIDR rule and you own keeping it current. That is
the supported, explicit hand-off; the managed path remains the default.

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
