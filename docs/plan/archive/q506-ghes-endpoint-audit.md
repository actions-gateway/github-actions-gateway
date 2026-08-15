# Q506 — Audit `github.com` hardcoded where a GHES endpoint belongs

**Status: DONE — Option A shipped 2026-07-31.** The audit below stands as written; this header records what was decided and built on top of it.
The decision was **Option A, fix the class**, taken by the maintainer.

| Finding | Shipped |
|---|---|
| #1 API base URL | `githubapp.DeriveAPIBaseURL` derives the base from `gitHubURL`; both GMC builders inject `GITHUB_API_BASE_URL` on the AGC Deployment, before `extraEnv` so the testing override still wins |
| #2 FQDN allowlist | The EgressProxy reconciler resolves the referrer graph and feeds every referrer's GHES host into the CNI policy and `PROXY_ALLOWED_HOST_SUFFIXES`; ActionsGateway + RunnerSet watch edges requeue the proxy |
| #3 CIDR ranges | No code answer, as predicted. The new advisory `GitHubEgressIncomplete` condition names the operator obligation instead of leaving a connect timeout |
| #4 two detectors | Collapsed — `GithubRegistrar.apiBase` and `scaleSetAPIBase` both call the shared resolver; `isHostedServer` and its substring test are gone |
| #5 noproxy claim | Retired from the Queue row, not carried forward |

Two things the build learned that the audit could not:

- **The #3 remedy is platform-gated, not tenant-self-serve.** `destinationCIDRs` is bounded by the GMC's `--allowed-egress-cidrs` allowlist, so clearing `GitHubEgressIncomplete` needs a platform admin's entry.
  The integration test had to pick a CIDR inside the suite's allowlist to clear the condition — the same constraint the audit noted for `destinationFQDNs` under #2 applies here.
- **An envtest watch assertion is vacuous unless the pool is driven Ready first.** A not-ready proxy short-requeues every 15 s, and that alone carried the new gateway into the reconcile — the first draft of the #2 integration test passed with both watch edges deleted.
  Disabling the suite's 2 s resync was not enough; the Q326 quota-watch test had already solved this by writing the Deployment status.

**Every finding below is from source inspection only.** Nothing was run against a GHES appliance.
Read the severities as "what the code says will happen", and see [What this audit did not verify](#what-this-audit-did-not-verify) — the #1 measurement named there is now covered by the integration tests, but the end-to-end GHES behaviour is still unobserved.

## What Q504 fixed, and what it implied

[Q504](../../operations/upgrade.md#non-breaking-eviction-auto-retry-now-honours-github_api_base_url-it-never-did-on-ghes) fixed one call: the eviction/preemption `rerun-failed-jobs` request read a provisioner field nothing assigned and fell back to `https://api.github.com`, so on GHES it posted a valid installation token to a host that had never issued it.
The fix routes that call through `githubapp.ResolveAPIBaseURL`, the same helper the token exchange uses.

Q506 was filed on the suspicion that the same shape recurs elsewhere.
It does — but not where the Queue row guessed.
The row named `githubEgressFQDNs` and "the noproxy guard list".
`githubEgressFQDNs` is a real defect; the noproxy guard is not (its callers already resolve the GHES host).
Meanwhile the resolution Q504 leaned on — `GITHUB_API_BASE_URL` — has **no supported way to be set** on a GMC-provisioned Actions Gateway Controller (AGC).

## The inventory

Every `github.com` / `api.github.com` / `codeload.github.com` occurrence outside `vendor/`, classified.
Three real defects, two ambiguities, the rest legitimate.

| # | Site | Class | Operator-visible? |
|---|---|---|---|
| 1 | [`githubapp/auth.go:56`](../../../githubapp/auth.go) `defaultAPIBaseURL`, with no GMC injection of `GITHUB_API_BASE_URL` | **Defect** | No — no config surface exists at all |
| 2 | [`egressproxy_fqdn.go:123`](../../../cmd/gmc/internal/controller/egressproxy_fqdn.go) `githubEgressFQDNs` → FQDN policy + proxy CONNECT allowlist | **Defect** | Yes — emitted CNI policy, `PROXY_ALLOWED_HOST_SUFFIXES` |
| 3 | [`ipranges.go:133`](../../../cmd/gmc/internal/controller/ipranges.go) `https://api.github.com/meta` → egress `ipBlock` CIDRs | **Defect** | Yes — emitted NetworkPolicy |
| 4 | [`github_registrar.go:390`](../../../cmd/agc/internal/agentpool/github_registrar.go) `isHostedServer` substring test | Ambiguous | No |
| 5 | [`noproxy.go:19`](../../../cmd/gmc/internal/webhook/noproxy/noproxy.go) `GitHubHosts` | Not a defect | Yes — admission rejection |
| 6 | Chart `actions-gateway.github.com` API group; `ghcr.io/actions/actions-runner`; `cmd/probe`; `test/fakegithub`; e2e specs; CRD/godoc examples; code comments | Legitimate | n/a |

### 1. The AGC's GitHub API base URL is never derived from `gitHubURL`

**The largest finding, and the one Q506 did not anticipate.** The AGC resolves its REST API base from the `GITHUB_API_BASE_URL` env var, defaulting to `https://api.github.com` ([`githubapp/auth.go:202`](../../../githubapp/auth.go)).
The Gateway Manager Controller (GMC) never sets it.
Both builders inject `GITHUB_ORG_URL` from `spec.gitHubURL` and stop there — [`builder.go:642`](../../../cmd/gmc/internal/controller/builder.go) (v1) and [`actionsgateway_v2_builder.go:383`](../../../cmd/gmc/internal/controller/actionsgateway_v2_builder.go) (v2).
There is no CRD field for it, no Helm value, and no other injection site.

The only route to that variable is `AGC_EXTRA_GITHUB_API_BASE_URL` behind the GMC's `--allow-agc-extra-env` flag, whose own help text reads *"Intended for testing only"* ([`flags.go:95`](../../../cmd/gmc/cmd/flags.go)) and which [security-operations.md](../../operations/security-operations.md#github-api-base-url-must-be-https) tells operators not to use in production.

So on a GHES gateway the App JWT is POSTed to `https://api.github.com/app/installations/<id>/access_tokens` — an App that host has never heard of.
Every token mint fails, on both acquisition tiers, before any job is acquired.

Two consequences worth separating:

- **The runner-registration path already gets this right and disagrees.** `GithubRegistrar.apiBase()` derives `https://<ghes-host>/api/v3` from `OrgURL` ([`github_registrar.go:383`](../../../cmd/agc/internal/agentpool/github_registrar.go)), and the scale-set tier derives the same base from `gitHubURL` ([`runnerset_scaleset.go:260`](../../../cmd/agc/internal/controller/runnerset_scaleset.go)).
  Three components answer "which GitHub API?" from two different sources.
  Q504 collapsed two of them; the env-var source is still unbridged to `gitHubURL`.
- **Shipped docs already assume the variable is set.** [upgrade.md](../../operations/upgrade.md#non-breaking-eviction-auto-retry-now-honours-github_api_base_url-it-never-did-on-ghes) scopes Q504's fix to *"any deployment that sets `GITHUB_API_BASE_URL` — that is, every GitHub Enterprise Server install"*, and [tenant-onboarding.md](../../operations/tenant-onboarding.md) lists a GHES URL as a supported `spec.gitHubURL`.
  If this finding holds, Q504's fix cannot reach a GMC-provisioned GHES tenant, because that tenant cannot get past token exchange.

**Blast radius: total, for every GHES tenant.** Not a degraded mode — no job is ever acquired.
Nothing the operator can configure changes it.

### 2. `githubEgressFQDNs` omits the GHES host, on two surfaces

[`githubEgressFQDNs`](../../../cmd/gmc/internal/controller/egressproxy_fqdn.go) is a package-level `[]string` of six public hostnames.
It feeds:

- the FQDN-mode CNI policy (Cilium / Calico / GKE), via `egressFQDNs`; and
- the proxy CONNECT allowlist `PROXY_ALLOWED_HOST_SUFFIXES`, via [`proxyAllowlistEnv`](../../../cmd/gmc/internal/controller/egressproxy_builder.go).

Neither can see a GHES host.
The `EgressProxy` carries no `gitHubURL` of its own — that lives on the referring `ActionsGateway` — and the controller never resolves the referrers, so a GHES tenant in an FQDN egress mode gets an allowlist naming six hosts none of its traffic uses.

**Blast radius: total for GHES tenants that use an `EgressProxy` in an FQDN mode, zero otherwise.** The proxy is optional (`spec.proxy` on v1, `defaultProxyRef` on v2 are both `+optional`), and FQDN is not the default mode, so this is the narrowest of the three.

**The workaround is real but not tenant-self-serve.** `spec.destinationFQDNs` would carry the GHES host, but the field is documented as *"EXTRA, **non-GitHub** DNS host suffixes"* and is gated by the platform-owned `--allowed-egress-fqdns` allowlist ([Q242 G.1](q242-g1-proxy-destination-allowlist.md)).
A tenant cannot add its own GHES host; a platform admin must allowlist it, into a field whose stated purpose it contradicts.

### 3. CIDR mode programs public GitHub's ranges, which never contain GHES

The default `egressPolicyMode` is `CIDR`.
In it, the `IPRangeReconciler` fetches `https://api.github.com/meta` ([`ipranges.go:133`](../../../cmd/gmc/internal/controller/ipranges.go)) and programs the merged `api`/`actions`/`web` ranges as the proxy pool's egress `ipBlock`.
A GHES appliance sits on the customer's own address space and appears in none of them, so the NetworkPolicy denies the proxy's traffic to the one host it exists to reach.

**Blast radius: total for GHES tenants that use an `EgressProxy`, which in the default mode is the likeliest GHES-plus-proxy combination.** Same platform-gated workaround as #2, via `destinationCIDRs`.

This one has **no clean code fix**.
A GHES appliance's address is knowable only to the operator.
Pointing the fetcher at a GHES `/meta` is not a mechanical swap: GHES serves that endpoint, but it describes that appliance's own configuration, and whether those ranges are the right allowlist is a design question, not a substitution.
Treat #3 as a documentation-and-validation problem regardless of which path is chosen for #1 and #2.

### 4. Two different detectors answer "is this public GitHub?"

`isHostedServer` tests `strings.Contains(githubURL, "github.com")` over the **whole URL including its path** ([`github_registrar.go:390`](../../../cmd/agc/internal/agentpool/github_registrar.go)), so a GHES org path literally named `github.com` — `https://ghes.corp/github.com` — misclassifies as public SaaS.
`scaleSetAPIBase` answers the same question by parsing the URL and switching on `u.Host` ([`runnerset_scaleset.go:260`](../../../cmd/agc/internal/controller/runnerset_scaleset.go)), which has no such hole.

Low severity on its own — the triggering org name is contrived.
It matters as a symptom: the repo has no single "resolve this gateway's GitHub API base" helper, so each caller invented one.
Any fix for #1 should collapse all three.

### 5. The noproxy guard is not a defect — the Queue row's claim doesn't hold

`noproxy.GitHubHosts` is a public-only list, but it is never the whole protected set.
Both callers add the tenant's GHES host:

- v1 parses `spec.gitHubURL` and appends its hostname ([`actionsgateway_webhook.go:402`](../../../cmd/gmc/internal/webhook/v1alpha1/actionsgateway_webhook.go)).
- v2 resolves it from every referrer, in both directions, through the uncached API reader ([`noproxy_referrers.go`](../../../cmd/gmc/internal/webhook/v2alpha1/noproxy_referrers.go)) — this was Q322, filed precisely as the GHES residual of the original guard.

The package doc says so explicitly.
The residual is the opposite of the row's claim: a GHES tenant is *also* barred from listing `github.com` in `noProxyCIDRs`, which is over-strict in the fail-safe direction.
Retire the claim rather than carrying it forward.

### 6. Legitimately `github.com`

- **Chart and CRD `actions-gateway.github.com`** — the v1 API group name.
  A DNS-shaped identifier, not an endpoint.
- **`cmd/agc/names/names.go:49` `ghcr.io/actions/actions-runner`** — the worker image default, overridable via `WorkerImage`.
  An image registry, not an API endpoint.
  Adjacent: an air-gapped GHES install cannot pull it.
  Out of scope here.
- **`cmd/probe/main.go:79,100`** — investigation probes deliberately aimed at live public GitHub.
- **Test fixtures and specs** — `test/fakegithub` and the `cmd/gmc/test/e2e` suite.
- **CRD field docs and godoc examples** (`https://github.com/my-org`) — documentation, and each already names the GHES form alongside.
- **Comments** in `cmd/agc/main.go:525` and `transport/trustpool.go:31`.

One adjacent gap worth naming so it is not rediscovered as part of this class: `BuildProxyTrustPool` builds system roots plus the proxy CA ([`trustpool.go`](../../../cmd/agc/internal/transport/trustpool.go)).
A GHES appliance fronted by a private CA is not trusted by that pool.
That is a certificate-trust gap, not a hardcoded endpoint, and needs its own item if the project commits to GHES.

## The open decision

The three defects do not share a fix, so the choice is not one lever.

| | A — fix the class | B — document the limit | C — split by decidability |
|---|---|---|---|
| **#1 API base URL** | Derive from `gitHubURL` in the GMC | Leave; state GHES unsupported | Derive from `gitHubURL` |
| **#2 FQDN allowlist** | Resolve referrer hosts in the controller | Leave | Resolve referrer hosts |
| **#3 CIDR ranges** | No code answer exists | Leave | Document + validate |
| **Doc consequence** | GHES claims become true | Retract GHES from onboarding, architecture, upgrade notes | Narrow the claim to "GHES minus operator-supplied egress ranges" |

**Option A — fix the class.** #1 is genuinely small: the derivation already exists twice (`scaleSetAPIBase`, `apiBase`), so this is one shared helper plus two builder call sites, and it collapses #4 on the way through. #2 is moderate — the resolution logic exists as `referrerGitHubHosts` in the v2 webhook, but moving it into a reconcile adds a watch edge (a gateway's `gitHubURL` change must requeue the proxy). #3 has no code answer at all, so even Option A ends with a documented operator obligation.
Note that #1 is an operator-visible behaviour change: a new env var on every AGC Deployment means every AGC pod restarts on the rollout.

**Option B — document GHES as a bounded limitation.** Cheap to write, and honest about today's code.
The cost is not the page — it is the retraction.
GHES is currently presented as supported in [tenant-onboarding.md](../../operations/tenant-onboarding.md), in [02-architecture.md](../../design/02-architecture.md) ("Both hosted github.com and GitHub Enterprise Server (GHES) endpoints are supported"), and implicitly in Q504's upgrade note, which addresses GHES operators as an existing audience.
B means walking all three back, and doing so for a platform whose registration path was deliberately built to handle it.

**Option C — split by decidability, and recommended.** Fix what the code can determine (#1, #2, #4 — all from `gitHubURL`, which the operator already supplies), document what it cannot (#3 — the appliance's address space), and add a validation signal so the undecidable part fails loudly rather than as a timeout.
This is not a compromise between A and B: it is the observation that #3 is a different kind of problem from #1 and #2, and grouping all three under one verb is what makes the decision look harder than it is.

**The judgement C rests on, stated so it can be disagreed with:** #1 is a small, well-precedented change with a large payoff, and shipping it alone converts GHES from "cannot start" to "starts, and needs egress ranges configured".
If the project is willing to say that sentence, C is right.
If it is not willing to support GHES at all, B is right and #1 should not be built — but then the retraction is the work, and it should be scoped as such.

**Sequencing, whichever path wins:** #1 is a prerequisite for the others being observable.
Until token exchange reaches the GHES host, no GHES tenant gets far enough to hit an egress-allowlist problem, so fixing #2 or #3 first is unverifiable.

## Acceptance

### If Option A or C

- One helper resolves a gateway's GitHub API base from `spec.gitHubURL`, and `GithubRegistrar.apiBase`, `scaleSetAPIBase`, and the GMC's env injection all call it.
  No second detector remains (closes #4).
- The GMC sets `GITHUB_API_BASE_URL` on the AGC Deployment on both the v1 and v2 paths, derived from `gitHubURL`.
  A `github.com` gateway keeps the value it has today, so no public-SaaS deployment changes behaviour.
- The testing-only `AGC_EXTRA_*` override still wins on conflict, matching how `GITHUB_ORG_URL` and the tracing vars already behave — the e2e suite points the AGC at `fakegithub` through it.
- An integration test asserts a GHES `gitHubURL` produces an AGC Deployment whose `GITHUB_API_BASE_URL` addresses that host and not `api.github.com`.
  It must fail if the injection is removed — a test that passes on a missing env var is the negative-assertion trap this repo already has a rule about.
- **Option A only:** the FQDN allowlist and CONNECT suffix list carry every referrer's `gitHubURL` host, and a change to a referrer's `gitHubURL` requeues the `EgressProxy`.
- Operator docs updated per the doc-update-matrix: onboarding states what a GHES tenant must supply, and the upgrade notes describe the new env var on AGC Deployments (a rollout restart).

### If Option B

- One operator-facing page states plainly that GHES is not supported, names which of the three surfaces breaks and how each presents, and is linked from onboarding.
- The GHES `gitHubURL` example is removed from [tenant-onboarding.md](../../operations/tenant-onboarding.md), and the GHES support sentence in [02-architecture.md](../../design/02-architecture.md) is narrowed to the registration path it actually describes.
- Q504's upgrade note is amended — it currently addresses GHES operators as a supported audience.
- Admission rejects a GHES `gitHubURL` outright, or the docs say why it is admitted and then fails at runtime.
  Admitting a configuration documented as unsupported, silently, is the worse of the two.

### Common to every path

- The Q506 Queue row's noproxy claim is retired, not carried forward (#5).
- #3's operator obligation is documented wherever it lands: a GHES tenant using an `EgressProxy` must have its appliance's ranges allowlisted, whichever egress mode it runs.

## What this audit did not verify

Source inspection only — no GHES appliance was involved, and this repo's rule is to treat an investigation finding as unverified until it is confirmed end to end.

The specific claims that carry the most weight and the least evidence:

- **#1 fails at token exchange on GHES.** Inferred from the env-var default plus the absence of any injection site.
  Not observed.
- **#2 and #3 deny GHES traffic.** Inferred from the emitted policy contents.
  Not observed against a live CNI.

**Cheapest confirming measurement for #1**, and the one to run before committing to any option: apply a `gitHubURL` pointing at a non-`github.com` host on a kind cluster and read the resulting AGC Deployment's env.
If `GITHUB_API_BASE_URL` is absent, #1 holds without needing a real GHES appliance — the defect is the missing injection, not anything the appliance does.
An envtest against the existing `cmd/gmc/internal/controller/integration/` suites reaches the same evidence without a cluster.

**What the fix did and did not verify.** That envtest measurement is now `TestGMC_GHESGateway_AGCAddressesTheAppliance` and its v2 twin, so the *injection* is proven against a real apiserver, and the #4 detector defect was confirmed end-to-end (restoring the substring test made a GHES-org registration escape to real `api.github.com` and come back `401 Bad credentials`).
Still unobserved: any call against an actual GHES appliance — token exchange, registration, or egress — and the CIDR-mode denial of #3, which is inferred from the emitted policy contents rather than watched on a live CNI.
