# Q311 — Scale-set tier monitoring surface

**Goal:** Give the scale-set acquisition tier (the default protocol since Q264 P5) the four monitoring surfaces the classic tier already has, so a scale-set-only deploy is not silently unobservable.

**Problem.** The scale-set listener emits four counters (`actions_gateway_scaleset_jobs_assigned_total`, `…_jobs_provisioned_total`, `…_provision_errors_total`, `…_jobs_completed_total`) that are documented in the Full Metrics Reference but wired to **no alert, no SLO recording rule, no dashboard panel, and no preview series**.
Critically, the classic throughput alert `ActionsGatewayNoActiveSessions` keys off `actions_gateway_active_sessions`, a gauge **only the classic listener emits** — a ScaleSet-protocol RunnerSet never creates that series, so on a scale-set-only deploy the alert never evaluates and a wedged tier pages no one.
The dashboards' `$namespace` template variable is also derived from `active_sessions`, so both dashboards render blank on a scale-set-only deploy.

## Surfaces to add

1. **Alerts** (`deploy/monitoring/prometheusrule.yaml`, mirrored in `observability.md` + `runbook.md`):
   - `ActionsGatewayScaleSetProvisioningStalled` (critical) — the scale-set analog of `NoActiveSessions`: jobs are being *assigned* but none are being *provisioned* (a wedged tier). `assigned rate > 0 unless provisioned rate > 0`.
   - `ActionsGatewayScaleSetProvisionErrors` (warning) — sustained provision-attempt failures (JIT-config mint / pod create).
2. **SLO recording rule** (`prometheusrule.yaml` `actions-gateway-slos` group):
   - `actions_gateway:scaleset_provision_success_rate:rate5m` — provisioned / (provisioned + errors) per namespace, mirroring `job_acquisition_success_rate`.
3. **Dashboard** — new "Scale-set Acquisition Tier" row on `grafana-dashboard-tenant.json` (assigned/provisioned throughput, provision success-rate gauge, provision errors, completions by result); a scale-set throughput panel on the platform dashboard's cross-tenant row.
   Both dashboards' `$namespace` variable unioned to also populate from the scale-set series so they render on a scale-set-only deploy.
4. **Preview series** — synthetic `scaleset_*` series in `deploy/monitoring/preview/exporter.py` so the new panels render in the screenshot harness.

## Out of scope
- No new metrics: the four surfaces are built on the existing counters.
- Scale-set failure conditions/events were Q325 (PR #657), separate.

## Status
- **DONE.** All four surfaces shipped:
  - Alerts `ActionsGatewayScaleSetProvisioningStalled` (critical, the throughput wedge) + `ActionsGatewayScaleSetProvisionErrors` (warning) in `prometheusrule.yaml`, mirrored in `observability.md`, with runbook entries.
  - SLO rule `actions_gateway:scaleset_provision_success_rate:rate5m`.
  - Dashboard: "Scale-set Acquisition Tier" row on the tenant dashboard + cross-tenant scale-set panel on the platform dashboard; both `$namespace` variables unioned so a scale-set-only deploy still populates.
  - Preview: synthetic `scaleset_*` series in `exporter.py` (verified emitting end-to-end via a local exporter run).
- Follow-up (mechanical): regenerate the dashboard PNG screenshots via `deploy/monitoring/preview/render.sh` — not run here (kind + kube-prometheus stack); doc tables/captions already describe the new row.
