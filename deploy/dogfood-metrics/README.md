# GAG dogfood metrics: keep the AGC's series in Managed Prometheus

The dogfood cluster runs Google Managed Prometheus (GMP), but nothing told it to scrape the AGC, so every `actions_gateway_*` counter (the eviction retries, the scale-set job results, the startup verdicts) died with the pod.
This tree adds that scrape: one `PodMonitoring` for the `dogfood` gateway's AGC in `gag-dogfood`, plus the RBAC its collector needs.

**This is a dogfood config, not a shipped product install.** It hardcodes the dogfood gateway's names.
GMP's managed collector reads `PodMonitoring`, not the Prometheus Operator's `ServiceMonitor`, so the GMC's `metrics.serviceMonitor.enabled` toggle does nothing here.

## How the scrape authenticates

The AGC serves `/metrics` on `:8443` and requires a client certificate signed by that gateway's metrics CA.
The `PodMonitoring` endpoint presents the scraper bundle the GMC already publishes, the `dogfood-agc-metrics-client` Secret, the same one [the runbook](../../docs/plan/gke-dogfood.md#sampling-agc-metrics-during-a-measurement-run) uses to sample `/metrics` by hand:

| Secret key | Used as |
|---|---|
| `tls.crt`, `tls.key` | the client certificate the collector presents |
| `ca.crt` | the CA the collector verifies the AGC's server certificate against |

`serverName` is `dogfood-agc.gag-dogfood.svc`, a SAN on the AGC's server certificate, so the scrape is verified end to end with no `insecureSkipVerify`.
GMP's collector has no secret access of its own; the `Role` and `RoleBinding` grant its `gmp-system/collector` ServiceAccount read on this one Secret, which is the setup GMP's managed-collection docs give for an mTLS endpoint.

## Deploy

Prerequisites not expressible in kustomize:

1. **GMP managed collection is enabled.** It is on by default on a GKE Standard cluster at 1.27 or later.
   The `PodMonitoring` uses the `tls.ca`/`cert`/`key` Secret references, which the dogfood cluster's GMP version has not been checked for; `kubectl apply` validates fields strictly by default (`--validate=strict`), so a version without them rejects the apply rather than dropping the fields.
2. **`gmp-system` carries `metrics=enabled`.** The GMC's per-tenant NetworkPolicy admits `:8443` only from namespaces with that label, so without it the scrape times out before the TLS handshake:

   ```bash
   kubectl --context "gke_${PROJECT}_${ZONE}_${CLUSTER}" \
     label namespace gmp-system metrics=enabled --overwrite
   ```

   This admits every pod in `gmp-system` to every tenant's metrics port and to the GMC manager's, not only the collector.
   Each listener still demands its own client certificate, so the label opens the network path and grants no access.
   `gmp-system` is a GKE-managed namespace; whether GKE keeps a label it did not set has not been checked, so re-check it after a cluster upgrade.

Then apply, pinned to the dogfood context:

```bash
kubectl --context "gke_${PROJECT}_${ZONE}_${CLUSTER}" apply -k deploy/dogfood-metrics
```

## What is still owed

- **A series count.** Nobody has counted the AGC's live series; take one `/metrics` sample through the runbook's port-forward and count the non-comment lines before relying on any cost estimate.
- **The egress proxy.** The dogfood tenant runs without an `EgressProxy`, so there is no proxy to scrape.
  Attaching one needs a second `PodMonitoring`, selecting that proxy's pods and presenting its `<ep>-metrics-client` Secret, with `serverName` set to `<ep>-proxy.gag-dogfood.svc`.
- **A confirmed scrape.** In Cloud Monitoring, `up{namespace="gag-dogfood"}` should read `1` within a couple of scrape intervals of the apply.
