# Q168 — v2 direct egress (optional-proxy behavior)

**Goal:** make the v2 egress proxy genuinely optional — no `defaultProxyRef`/`proxyRef`
⇒ **direct egress** (still NetworkPolicy-restricted), not `Degraded`/`ProxyNotFound`.

Design: appendix-h §H.10 + §H.4. Plan slice: `docs/plan/v2-api.md` § "Direct egress".

## Secure-by-default guardrail (the whole point)
Dropping the proxy drops egress **identity** (per-tenant IP attribution), NOT egress
**restriction**. Direct egress is still default-deny egress + allow only **DNS + GitHub
CIDRs** (+ **kube API** for the AGC control plane). There is no proxy-less *unrestricted*
mode. Identity becomes the opt-in (add a proxy); restriction stays mandatory/default-on.

## Behavior matrix
| gateway.defaultProxyRef | rs.proxyRef | mode |
|---|---|---|
| nil | nil | **Direct** (this work) |
| nil | X | Proxied via X |
| Y | nil | Proxied via Y |
| Y | X | Proxied via X |

A Direct RunnerSet always sits under a Direct gateway (defaultProxyRef nil), so the
GMC-provisioned workload NetworkPolicy (gateway-scoped) already carries the GitHub-CIDR
egress the direct worker needs — the two are consistent by construction. Mixed case
(Direct gateway + a RunnerSet with its own proxyRef): the workload NP allows GitHub CIDRs,
so that proxied worker *could* reach GitHub directly — a best-effort attribution caveat,
documented; restriction is still preserved.

## Work items
1. **API (`api/v2alpha1`)**
   - `conditions.go`: `ProxyModeProxied`/`ProxyModeDirect`; `ConditionEgressUnattributed`
     (advisory, abnormal-is-True, does NOT gate Ready); `ReasonDirectEgress`/`ReasonProxiedEgress`.
   - `actionsgateway_types.go` + `runnerset_types.go`: `ProxyMode string` in status + printcolumn.
   - Regenerate deepcopy + CRDs (`make generate manifests` per code-generation.md).
2. **GMC `ActionsGatewayV2Reconciler`**
   - Inject `IPCache *IPRangeCache`.
   - `defaultProxyRef == nil` ⇒ direct (don't fail closed). Set ⇒ proxied; missing set ref still fail-closed ProxyNotFound.
   - `reconcileResources(proxy *EgressProxy)` (nil = direct):
     - AGC NP + workload NP gain GitHub-CIDR egress in direct mode (from IPCache snapshot).
     - AGC Deployment: direct ⇒ no HTTP(S)_PROXY/PROXY_TLS_SECRET_NAME env, no proxy-CA mount.
   - `updateStatus`: set `ProxyMode` + `EgressUnattributed` (direct ⇒ True/DirectEgress, proxied ⇒ False/ProxiedEgress).
   - Wire `IPCache` in `cmd/gmc/cmd/main.go`.
3. **`buildAGCDeploymentFrom`**: skip proxy-CA volume/mount when `proxyTLSSecret == ""`.
4. **AGC `runnerset_target` / `runnerset_controller`**
   - `resolveRunnerSetRefs`: proxyName "" ⇒ resolved with `proxy == nil` (direct), not ProxyNotFound.
   - `Resolve`: nil proxy ⇒ empty HTTP(S)Proxy/ProxyTLSSecretName ⇒ direct worker egress.
   - Reconcile: set `ProxyMode` + `EgressUnattributed` on the RunnerSet status.
5. **IP refresh relocation (`ipranges.go`)**: in `reconcileAll`, also patch each Direct v2
   `ActionsGateway`'s AGC NP + workload NP GitHub-CIDR egress (new `patchDirectEgressNetworkPolicies`).
   Proxied gateways/EgressProxy path unchanged.
6. **Docs**: appendix-h (drop "deferred", direct egress shipped); `docs/operations/tenant-onboarding.md`
   (proxy-less option + IP-attribution trade + egress still restricted); `05-security.md`
   (secure-by-default line); troubleshooting if warranted. STATUS.md: remove Q168 deferred row (own commit).

## Tests
- GMC envtest (`integration/`): proxy-less gateway → Ready, AGC NP + workload NP have GitHub-CIDR
  egress, `proxyMode: Direct` + EgressUnattributed; proxied regression; IP-refresh patches direct NPs.
- AGC envtest (`integration/`): proxy-less RunnerSet → Ready, `proxyMode: Direct` + EgressUnattributed;
  proxied regression. Unit tests for builders + resolve.
- e2e (proxy-less job→pod→direct→GitHub, non-GitHub blocked): **shipped (Q178)**.
  `E2E_V2_DirectEgress` (`cmd/gmc/test/e2e/direct_egress_test.go`) provisions a
  proxy-less gateway + RunnerSet on kind and proves both halves of the §H.10 contract
  live on a CNI:
  - **Positive (both CNI legs):** a workload-labelled pod reaches `api.github.com`
    directly — no proxy in the path — confirming the workload NP's GitHub-CIDR
    allowance lets proxy-less workers egress to GitHub.
  - **Negative (Calico leg only):** from the same workload network context a
    connection to a non-GitHub destination (fakegithub in `e2e-infra`) is dropped by
    the default-deny egress NetworkPolicy. Self-skips on kindnet via
    `egressEnforcingCNI()` — kindnet accepts NetworkPolicy but does not enforce egress
    drops (Q7b/Q119), so the block can only be proven on the Calico lane
    (`e2e-calico.yml`, triggered per-PR on `cmd/gmc/**`).
