# Accessing Metrics (scraping setup)

> **Audience:** Platform engineer

Part of the [Observability](observability.md) guide.
See also: [Metrics reference](observability-metrics.md) · [Alerting & SLOs](observability-alerting.md) · [Grafana dashboards](observability-dashboards.md) · [Logging & tracing](observability-logging.md).

## How to Access Metrics

**Port forward (ad-hoc):** the per-tenant AGC/proxy metrics ports require a client certificate (mTLS), so a plain `curl` is rejected at the TLS handshake.
Present the published scraper bundle (see [the per-tenant section](#scraping-per-tenant-agc-and-proxy-metrics-mtls) for the Secret name and an end-to-end `curl --cert/--key/--cacert` example).

**Prometheus operator (production):** the chart wires scraping automatically when `metrics.serviceMonitor.enabled=true` — both the GMC manager `ServiceMonitor` and a per-tenant `ServiceMonitor` for each provisioned tenant's AGC/proxy.
The metrics port is named `metrics` (`:8443`) on every Service.
You do not hand-author these `ServiceMonitor`s; the sections below describe what they wire and the prerequisites.

**Google Managed Prometheus (GKE):** managed collection does not read `ServiceMonitor`s, so you write a `PodMonitoring` per component yourself.
See [Scraping with Google Managed Prometheus](#scraping-with-google-managed-prometheus-gke).

### Install-time scraping prerequisites (GMC manager)

The default GMC install ships the manager NetworkPolicy **enabled by default** (`networkPolicy.enabled=true`).
Selecting the manager pod flips it to default-deny on ingress, so its `/metrics` endpoint admits traffic only from namespaces carrying the right label:

- **Scraping the GMC manager metrics:** label your Prometheus namespace `metrics: enabled`, or no scrape will reach the manager:
  ```bash
  kubectl label namespace <prometheus-namespace> metrics=enabled
  ```

The validating-webhook port (container `9443`) is intentionally re-allowed from **any source** — the kube-apiserver that calls it is not a pod in a labeled namespace, so a source restriction there would silently break every `ActionsGateway` admission (`failurePolicy: Fail`).
The webhook is TLS + caBundle authenticated, so the sensitive surface stays the `metrics: enabled` restriction above.
No namespace label is required for CR admission.

This applies to the **GMC manager** only.
The per-tenant AGC and proxy metrics are governed by the per-tenant NetworkPolicies the GMC generates (the AGC and proxy NPs already admit monitoring-namespace scrapes of the metrics port) and use mutual TLS — see [Scraping per-tenant AGC and proxy metrics (mTLS)](#scraping-per-tenant-agc-and-proxy-metrics-mtls).

> Runtime enforcement of these policies depends on the CNI; kindnet's `kube-network-policies` does not drop all egress negatives (see the worker egress limitation in [troubleshooting.md](troubleshooting.md)).
> The manager NP is verified by manifest review and is pending a cluster-only runtime check.

The `ServiceMonitor` integration stays **opt-in**, behind the `metrics.serviceMonitor.enabled` chart value (default `false`): out-of-box Prometheus Operator scraping.
It is left off by default because the `ServiceMonitor` CRD only exists once the Prometheus Operator is installed, so rendering it unconditionally would break `helm install` on clusters without it.
The toggle does nothing for Google Managed Prometheus managed collection, which reads `PodMonitoring` instead.

### Verifying the metrics scrape TLS (GMC manager)

The GMC metrics endpoint (`:8443`) is served over TLS.
How the scrape verifies that certificate is controlled by `metrics.tls.certManager.enabled`:

- **`true` (default, secure):** cert-manager issues a dedicated metrics serving cert — the `metrics-serving-cert` `Certificate`, minted from the same `selfsigned-issuer` as the webhook into the `metrics-server-cert` Secret.
  The GMC serves it (`--metrics-cert-path`), and the rendered `ServiceMonitor` verifies it against the issuing CA:

  ```yaml
  tlsConfig:
    serverName: <namePrefix>-controller-manager-metrics-service.<namespace>.svc
    ca:
      secret:
        name: metrics-server-cert
        key: ca.crt
  ```

  The scrape is authenticated end-to-end and **not** MITM-able.
  This path requires cert-manager (it reuses the webhook's Issuer) and is automatically inert when `certManager.enabled=false`.

- **`false`, or `certManager.enabled=false`:** the GMC falls back to controller-runtime's auto-generated self-signed metrics cert, and the `ServiceMonitor` scrapes with `tlsConfig.insecureSkipVerify: true`.
  Prometheus cannot verify the server, so an in-cluster attacker who can intercept the scrape connection could impersonate the metrics endpoint (the bearer token still authenticates the *scraper* to the server, but not the server to the scraper).
  Use this only on clusters without cert-manager, accepting the weaker posture.

The metrics-server-cert Secret is read from the **ServiceMonitor's namespace** (the GMC release namespace), where the chart creates it — no extra copying is needed.
Because verification follows `certManager.enabled` by default, an install that already uses cert-manager for the webhook (the default) gets verified metrics scraping automatically once the `ServiceMonitor` is enabled.

### Scraping per-tenant AGC and proxy metrics (mTLS)

Each provisioned tenant runs its own AGC and egress-proxy pods, which serve `/metrics` over **mutual TLS** on `:8443`.
Unlike the GMC manager, the listener **requires a client certificate** signed by that tenant's metrics CA — there is no bearer-token or `insecureSkipVerify` fallback.
Three things are wired per tenant so Prometheus can scrape them:

1. **Metrics Services (always created).** The GMC creates a `metrics`-named `:8443` port on the proxy `Service` and a dedicated AGC `Service`, both in the tenant namespace.
   The AGC Service is named for the gateway, as `<gateway>-agc`, and the proxy Service for the `EgressProxy`, as `<ep>-proxy`.
   These exist regardless of the scrape toggle.
2. **Per-tenant `ServiceMonitor`s (opt-in).** When `metrics.serviceMonitor.enabled=true`, the GMC creates one `ServiceMonitor` per component in the tenant namespace (`<ep>-proxy-metrics`, `<gateway>-agc-metrics`).
   Each selects only its own component's Service via the tenant's owner labels, so one tenant's monitor never selects another tenant's pods.
   Under v2 the monitors are named per resource instead: `<gateway>-agc-metrics` for each `ActionsGateway`'s AGC, and `<ep>-proxy-metrics` for each `EgressProxy`.
3. **The scraper client bundle (mTLS).** Each `ServiceMonitor` presents the per-tenant scraper client bundle from the `<ep>-metrics-client` Secret in the tenant namespace (the AGC's is `<gateway>-agc-metrics-client`) — `tls.crt`/`tls.key` authenticate the scraper to the listener and `ca.crt` verifies the listener's server cert.
   `serverName` is the component's `<service>.<namespace>.svc` DNS name (a SAN on the server cert), so the scrape is verified end-to-end and **not** MITM-able:

   ```yaml
   tlsConfig:
     serverName: <ep>-proxy.<tenant-namespace>.svc
     ca:        { secret: { name: <ep>-metrics-client, key: ca.crt } }
     cert:      { secret: { name: <ep>-metrics-client, key: tls.crt } }
     keySecret: { name: <ep>-metrics-client, key: tls.key }
   ```

#### `EgressProxy` proxy metrics

The egress proxy is a standalone `EgressProxy` resource (multiple per namespace are allowed), so its proxy-metrics objects are keyed on the `EgressProxy` name `<ep>`:

| Object | Name |
|---|---|
| Proxy `Service` (metrics port `:8443`) | `<ep>-proxy` |
| Server bundle Secret (mounted into the proxy) | `<ep>-metrics-tls` |
| Scraper client bundle Secret (published) | `<ep>-metrics-client` |
| `ServiceMonitor` | `<ep>-proxy-metrics` |

Each `EgressProxy` owns its **own** metrics CA — distinct from the AGC's and from every other `EgressProxy` — so Prometheus must read that proxy's own `<ep>-metrics-client` bundle, with `serverName: <ep>-proxy.<namespace>.svc`.
The same toggle (`metrics.serviceMonitor.enabled` → `--enable-tenant-service-monitors`), the same `metrics: enabled` NetworkPolicy prerequisite, and the same graceful `ServiceMonitorCRDMissing` handling apply.

The AGC follows the same pattern keyed on the gateway name `<gateway>`, with its own CA, distinct from every `EgressProxy`'s:

| Object | Name |
|---|---|
| AGC `Service` (metrics port `:8443`) | `<gateway>-agc` |
| Scraper client bundle Secret (published) | `<gateway>-agc-metrics-client` |
| `ServiceMonitor` | `<gateway>-agc-metrics` |

Its `serverName` is `<gateway>-agc.<namespace>.svc`.

**Prerequisites:**

- **Prometheus Operator** must be installed (the `monitoring.coreos.com` `ServiceMonitor` CRD must exist).
  The toggle is off by default precisely because the CRD is not present on every cluster.
  If the CRD is absent when the toggle is on, the GMC logs a warning and emits a `ServiceMonitorCRDMissing` Event on the `ActionsGateway` and continues — a missing scrape prerequisite never blocks tenant provisioning.
- **Prometheus reads the client bundle from the `ServiceMonitor`'s namespace** (the tenant namespace), so the scraping Prometheus must be configured to select `ServiceMonitor`s across tenant namespaces (`serviceMonitorNamespaceSelector`) and granted read access to the per-tenant `<gateway>-agc-metrics-client` and `<ep>-metrics-client` Secrets.
  Each tenant has a distinct CA and client cert, which is why a single cluster-wide `ServiceMonitor` cannot scrape them — the wiring is necessarily per tenant.
- **NetworkPolicy:** label the Prometheus namespace `metrics: enabled` so the per-tenant NetworkPolicy admits the scrape (the AGC and proxy policies admit the `:8443` metrics port only from `metrics=enabled` namespaces).

**Ad-hoc verification** (mounting the published bundle locally).
The Service and Secret are named per **gateway**, so set `svc`/`secret` for the tenant you are scraping:

```sh
ns=<tenant-namespace>

# Per ActionsGateway, e.g. gateway "dogfood" -> dogfood-agc:
svc=<gateway>-agc
secret=<gateway>-agc-metrics-client

kubectl get secret "$secret" -n "$ns" \
  -o jsonpath='{.data.tls\.crt}' | base64 -d > client.crt
kubectl get secret "$secret" -n "$ns" \
  -o jsonpath='{.data.tls\.key}' | base64 -d > client.key
kubectl get secret "$secret" -n "$ns" \
  -o jsonpath='{.data.ca\.crt}'  | base64 -d > ca.crt
kubectl port-forward -n "$ns" "svc/$svc" 8443:8443 &
curl --cert client.crt --key client.key --cacert ca.crt \
  "https://$svc.$ns.svc:8443/metrics" --resolve \
  "$svc.$ns.svc:8443:127.0.0.1"
rm -f client.crt client.key ca.crt   # delete the cert material when done
```

The `--resolve` host must match `$svc.$ns.svc` exactly: that name is what the AGC's serving certificate is issued for, so substituting `localhost` fails the TLS handshake rather than the authorization check.

(The bundle is a client *certificate*, not a long-lived account credential; still remove the files when finished.)

### Scraping with Google Managed Prometheus (GKE)

Google Managed Prometheus (GMP) managed collection reads its own `PodMonitoring` resource.
It does not read `ServiceMonitor`s: Google's [setup page](https://docs.cloud.google.com/stackdriver/docs/managed-prometheus/setup-managed) (read 2026-11-26) has you convert them, or run self-deployed collectors instead.
The GMC does not create `PodMonitoring`s either, so `metrics.serviceMonitor.enabled` has no effect on what GMP scrapes.
This section covers the per-tenant AGC and egress proxy, not the GMC manager.

You need:

- **Managed collection turned on.** The same page says it is on by default for GKE Standard 1.27 and later and for Autopilot 1.25 and later.
- **A `PodMonitoring` schema with Secret references for `tls.ca`, `tls.cert` and `tls.key`.** The [API reference](https://github.com/GoogleCloudPlatform/prometheus-engine/blob/main/doc/api.md) (read 2026-11-26) lists them.
  Neither page names the first GKE or GMP version that has them.
- **The collector's namespace labeled `metrics=enabled`,** or the per-tenant NetworkPolicy drops the scrape.
  The collector runs in `gmp-system` on Standard clusters and `gke-gmp-system` on Autopilot:

  ```bash
  kubectl label namespace <collector-namespace> metrics=enabled
  ```

  The label admits every pod in that namespace to every tenant's metrics port and to the GMC manager's, not only the collector.
  Each listener still demands its own credential.

Then apply one `PodMonitoring` per component, with the `Role` and `RoleBinding` that let the collector read that component's client bundle.
For a gateway's AGC:

```yaml
apiVersion: monitoring.googleapis.com/v1
kind: PodMonitoring
metadata:
  name: <gateway>-agc
  namespace: <namespace>
spec:
  selector:
    matchLabels:
      app: <gateway>-agc
  endpoints:
    - port: metrics
      path: /metrics
      scheme: https
      interval: 60s
      tls:
        serverName: <gateway>-agc.<namespace>.svc
        ca:
          secret:
            name: <gateway>-agc-metrics-client
            key: ca.crt
        cert:
          secret:
            name: <gateway>-agc-metrics-client
            key: tls.crt
        key:
          secret:
            name: <gateway>-agc-metrics-client
            key: tls.key
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: gmp-collector-read-<gateway>-agc-metrics-client
  namespace: <namespace>
rules:
  - apiGroups: [""]
    resources: ["secrets"]
    resourceNames: ["<gateway>-agc-metrics-client"]
    verbs: ["get", "list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: gmp-collector-read-<gateway>-agc-metrics-client
  namespace: <namespace>
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: gmp-collector-read-<gateway>-agc-metrics-client
subjects:
  - kind: ServiceAccount
    name: collector
    namespace: <collector-namespace>
```

For an `EgressProxy`, write the same three objects in the `EgressProxy`'s namespace with these values, naming the `Role` and `RoleBinding` after its client bundle:

| Field | AGC | `EgressProxy` |
|---|---|---|
| `PodMonitoring` name | `<gateway>-agc` | `<ep>-proxy` |
| Pod selector | `app: <gateway>-agc` | `actions-gateway.com/egress-proxy: <ep>` |
| `serverName` | `<gateway>-agc.<namespace>.svc` | `<ep>-proxy.<namespace>.svc` |
| Client bundle Secret | `<gateway>-agc-metrics-client` | `<ep>-metrics-client` |

The AGC manifest is adapted from the project's [dogfood scrape](https://github.com/actions-gateway/github-actions-gateway/tree/main/deploy/dogfood-metrics), which landed every scrape over a 15-minute window on 2026-11-02.
The `EgressProxy` variant has not been tried, nor has anything on Autopilot, including whether it lets you label `gke-gmp-system`.

To check a target, query `up{namespace="<namespace>"}` with PromQL; `1` means the last scrape succeeded.
Where the NetworkPolicy is enforced, a missing namespace label shows up as a scrape that times out before the TLS handshake.

---

← Back to [Observability](observability.md)
