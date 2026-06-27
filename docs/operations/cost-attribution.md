# Live per-tenant cost attribution (OpenCost / Kubecost)

> **Audience:** SRE, Platform engineer, budget owner

[Appendix F — Cost model](../design/appendix-f-cost-model.md) gives you a *model*: list-price rates, the per-job formula, and the [savings calculator](../design/appendix-f-cost-model.md#f5-savings-calculator-this-system-vs-arc) that estimates what you save over Actions Runner Controller (ARC). This page is the *measurement* counterpart — how to turn that model into **real dollars per tenant** from your live cluster using [OpenCost](https://www.opencost.io/) or [Kubecost](https://www.kubecost.com/) (Kubecost is built on OpenCost and shares its allocation engine and query surface).

You get this nearly for free because GAG (GitHub Actions Gateway) already maps cleanly onto how these tools allocate cost:

- **Each tenant is a dedicated namespace.** OpenCost/Kubecost aggregate cost by namespace natively, so `aggregate=namespace` *is* per-tenant cost with no extra wiring.
- **Every object GAG creates carries the Kubernetes recommended (`app.kubernetes.io/*`) labels.** That lets you split a tenant's namespace cost into its worker / proxy / controller components without learning any project-specific keys.

This page assumes OpenCost or Kubecost is already installed and scraping your cluster (their install docs cover that). It does not require any GAG-specific configuration.

---

## Table of Contents

- [The cost shape you are attributing](#the-cost-shape-you-are-attributing)
- [The labels and namespaces to query](#the-labels-and-namespaces-to-query)
- [OpenCost allocation queries](#opencost-allocation-queries)
- [Kubecost allocation queries](#kubecost-allocation-queries)
- [How zero-idle-compute shows up in the reports](#how-zero-idle-compute-shows-up-in-the-reports)
- [From allocation to showback / chargeback](#from-allocation-to-showback--chargeback)

---

## The cost shape you are attributing

Per [Appendix F §F.1](../design/appendix-f-cost-model.md#f1-per-job-cost-breakdown), a tenant's spend has three parts, and they behave very differently in an allocation report:

| Component | `app.kubernetes.io/component` | Cost behaviour | Share of tenant cost |
|---|---|---|---|
| **Worker pods** | `runner` | One pod per job, alive only for the job's duration, then deleted | **The bulk** — especially GPU jobs |
| **Egress proxy pool** | `proxy` | Always-on at `minReplicas`, HPA-managed | Small per-tenant fixed floor |
| **AGC** (Actions Gateway Controller) | `controller` | Single always-on pod per tenant | Small per-tenant fixed floor |

The defining property — **zero idle compute** — is what makes the worker-pod line *track job volume*: there is no always-on runner pool to allocate, so between jobs the worker line drops to zero and only the proxy + AGC floor remains. That is the live, measured version of the idle-floor saving the calculator estimates. [How zero-idle shows up](#how-zero-idle-compute-shows-up-in-the-reports) below shows what to look for.

---

## The labels and namespaces to query

These are the labels GAG actually stamps (verified against the code in [`api/apilabels/labels.go`](../../api/apilabels/labels.go) and the AGC/GMC builders). They are **additive metadata** — never build an allocation *filter you depend on for billing* around the controllers' functional selectors (`app:`, `actions-gateway/component: workload`); those exist for NetworkPolicy/Service wiring and may change. Use the `app.kubernetes.io/*` set below, which is the stable contract for tooling.

| Label | Worker pod | Egress proxy pod | AGC pod |
|---|---|---|---|
| `app.kubernetes.io/part-of` | `actions-gateway` | `actions-gateway` | `actions-gateway` |
| `app.kubernetes.io/component` | `runner` | `proxy` | `controller` |
| `app.kubernetes.io/name` | `actions-runner` | `actions-gateway-proxy` | `actions-gateway-controller` |
| `app.kubernetes.io/instance` | owning `RunnerGroup` / `RunnerSet` name | owning `EgressProxy` / `ActionsGateway` name | owning `ActionsGateway` name |
| `app.kubernetes.io/managed-by` | `actions-gateway-controller` | `actions-gateway-gmc` | `actions-gateway-gmc` |
| `app.kubernetes.io/version` | runner image version | *(omitted)* | *(omitted)* |

**Namespace = tenant.** GAG places a tenant's worker pods, proxy pool, and AGC all in that tenant's namespace (the `ActionsGateway` CR's namespace). The namespace name is operator-chosen — GAG does not derive or rename it — so use whatever namespace your team assigned the tenant. There is no separate proxy or worker namespace to reconcile.

**Per-RunnerGroup / per-RunnerSet drill-down.** To split a tenant's worker cost by runner shape (e.g. `gpu-a100` vs `cpu-standard`), aggregate by `app.kubernetes.io/instance`, which carries the owning RunnerGroup/RunnerSet name. (GAG also sets project-specific `actions-gateway/runner-group` and `actions-gateway.com/runner-set` labels for `kubectl` selection — see [observability.md](observability.md#drilling-down-to-individual-runner-pods) — but for cost queries prefer the recommended `instance` label.)

> **Cardinality caution.** Do not aggregate cost by a label whose value is per-job or per-run (there is none in the recommended set, but worker pods *also* carry per-job annotations like `actions-gateway.com/run-id`). Aggregating allocation by a unique-per-job key explodes the result set the same way it would explode Prometheus — see the [label cardinality warning](observability.md#label-cardinality-warning).

---

## OpenCost allocation queries

OpenCost exposes an [Allocation API](https://www.opencost.io/docs/integrations/api) and a Prometheus-friendly metrics surface. The exact filter syntax has evolved across releases — treat the queries below as the shape to adapt, and check your version's API docs for the precise parameter names.

**Per-tenant total** (the headline number — one row per tenant namespace):

```sh
# Last 7 days, one cost row per namespace = one row per tenant.
curl -G 'http://opencost:9003/allocation/compute' \
  --data-urlencode 'window=7d' \
  --data-urlencode 'aggregate=namespace'
```

**One tenant, split into worker / proxy / controller:**

```sh
# Scope to one tenant namespace, then break it down by component label.
curl -G 'http://opencost:9003/allocation/compute' \
  --data-urlencode 'window=7d' \
  --data-urlencode 'filter=namespace:"team-a"' \
  --data-urlencode 'aggregate=label:app.kubernetes.io/component'
```

This returns three rows — `runner`, `proxy`, `controller` — letting you confirm that worker pods dominate and see the fixed proxy + AGC floor explicitly.

**One tenant, worker cost by runner shape:**

```sh
curl -G 'http://opencost:9003/allocation/compute' \
  --data-urlencode 'window=30d' \
  --data-urlencode 'filter=namespace:"team-a" + label[app_kubernetes_io_component]:"runner"' \
  --data-urlencode 'aggregate=label:app.kubernetes.io/instance'
```

**Whole-fleet GAG spend** (every tenant, every component):

```sh
curl -G 'http://opencost:9003/allocation/compute' \
  --data-urlencode 'window=30d' \
  --data-urlencode 'filter=label[app_kubernetes_io_part_of]:"actions-gateway"' \
  --data-urlencode 'aggregate=namespace'
```

> OpenCost flattens label keys to a Prometheus-safe form in some filter contexts — `app.kubernetes.io/component` becomes `app_kubernetes_io_component`. Use the dotted form for `aggregate=label:…` and the underscored form inside `filter=label[…]` as your version's docs specify.

---

## Kubecost allocation queries

Kubecost's [Allocation API](https://docs.kubecost.com/apis/apis-overview/api-allocation) takes the same shape. From the Kubecost UI, the equivalent is the **Allocations** report with *Aggregate by → Namespace* (or *→ Label* for the component split).

**Per-tenant total:**

```sh
curl -G 'http://kubecost:9090/model/allocation' \
  --data-urlencode 'window=7d' \
  --data-urlencode 'aggregate=namespace' \
  --data-urlencode 'accumulate=true'
```

**One tenant, split by component:**

```sh
curl -G 'http://kubecost:9090/model/allocation' \
  --data-urlencode 'window=7d' \
  --data-urlencode 'filterNamespaces=team-a' \
  --data-urlencode 'aggregate=label:app.kubernetes.io/component'
```

**Whole-fleet GAG spend by tenant:**

```sh
curl -G 'http://kubecost:9090/model/allocation' \
  --data-urlencode 'window=30d' \
  --data-urlencode 'filterLabels=app.kubernetes.io/part-of:actions-gateway' \
  --data-urlencode 'aggregate=namespace'
```

For automated chargeback, Kubecost also serves these as scheduled CSV/JSON exports and as a [SQL/cloud-cost integration](https://docs.kubecost.com/) — wire the per-namespace allocation into the same export you use for the rest of the cluster.

---

## How zero-idle-compute shows up in the reports

The single most useful thing an allocation report *proves* about GAG is the property the cost model claims: **you pay for jobs, not for idle runners.** Look for these signatures:

- **The `runner` component line tracks job volume, not the clock.** Aggregate one tenant by `app.kubernetes.io/component` over a window that spans a quiet period (overnight, weekend). The `runner` cost collapses toward zero when no jobs run and rises with throughput — because there is no `minRunners`-style always-on pool to allocate. Contrast this with an ARC scale set running `minRunners: N > 0`, whose runner pods accrue cost 24/7 in the same report (see [Appendix F §F.2](../design/appendix-f-cost-model.md#f2-cost-comparison-this-system-vs-arc)).
- **The idle floor is just `proxy` + `controller`.** A tenant that ran zero jobs in the window still shows a small, flat cost: its always-on egress proxy pool (`minReplicas`) and its single AGC pod. That floor is the per-tenant fixed overhead from [§F.1](../design/appendix-f-cost-model.md#f1-per-job-cost-breakdown) — typically cents to low dollars per day, dominated by the proxy.
- **GPU spend is bursty and short-lived.** Because a worker pod is deleted the instant its job completes (`completedPodTTL`), expensive GPU nodes appear in allocation only for the job's actual duration. A 30-minute GPU job is 30 minutes of GPU allocation, not a day of held capacity — the [§F.1 worker-pod formula](../design/appendix-f-cost-model.md#worker-pod-dominant-cost) made measurable.

> **Allocation needs a node price book.** OpenCost/Kubecost compute pod cost from the *node's* hourly rate × the pod's resource share. On a managed cloud they read real instance pricing automatically; on-prem or with committed-use/Spot discounts, configure a [custom pricing model](https://www.opencost.io/docs/configuration/) so the per-tenant dollars reflect your contracted rates rather than list price — the same caveat Appendix F makes about its list-price figures.

---

## From allocation to showback / chargeback

This live data is the input to the **showback vs chargeback** choice in [Appendix F §F.3](../design/appendix-f-cost-model.md#f3-tenant-cost-allocation):

- **Showback:** point a Grafana panel (or the Kubecost UI) at the per-namespace allocation so each team sees its own real spend next to the [tenant dashboard](observability.md#tenant-dashboard) throughput metrics. No billing integration required.
- **Chargeback:** export the per-namespace allocation on a schedule and feed it into your billing system, optionally applying a discount/overhead factor. The `app.kubernetes.io/instance` breakdown lets you bill by runner shape if a tenant runs both cheap CPU and expensive GPU groups.

Cross-check the allocation against GAG's own metrics for a sanity test: `actions_gateway_job_duration_seconds` (per `namespace`, `runner_group`) × the runner shape's GPU/CPU node fraction should land in the same ballpark as the `runner`-component allocation for that tenant. A large divergence usually means oversized resource requests — the [worker right-sizing lever](../design/appendix-f-cost-model.md#worker-resource-right-sizing) in Appendix F.

---

← Back to [Operations overview](README.md) · related: [Observability](observability.md), [Appendix F — Cost model](../design/appendix-f-cost-model.md)
