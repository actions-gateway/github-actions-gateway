# Security Audit 3: November 2026

Third full security audit, performed 2026-11-09 against `7b3f016ad`, ahead of `v2.0.0`.
The first audit (2026-05) is [security.md](security.md) and the second (2026-06) is [archive/security-audit-2026-06.md](archive/security-audit-2026-06.md); this doc records only the third audit's findings and where each one is tracked.

> **Status: findings filed, the medium one fixed, the eight stricter admission rules landed (Q1322), the four egress proxy sharing leads confirmed and fixed (Q1324), and six of the seven release and CI leads settled (Q1326).** 9 findings are confirmed from source (one medium, eight low) and 30 were leads that source alone cannot settle, 16 of which still are, one of them the release and CI lead that waits on Q1330.
> Nothing reached high or critical.
> Two rows gated `v2.0.0`, Q1321 and Q1322, and both have landed.
> The rest are [Q1323](../queue/Q1323.md) to [Q1328](../queue/Q1328.md).

## Method and limits

The audit read source and ran nothing.
No agent built, tested or executed anything from the repository, and nothing was sent to a cluster, to GitHub or to any other live system.
That sets what each verdict means:

- **Confirmed** means static text fully determines the result: a manifest, a policy expression, a constant or a pure function.
  No confirmed result was observed on a cluster.
- **Needs validation** means source supports the path and something outside the repository decides the outcome: a Container Network Interface (CNI) plugin, the apiserver, GitHub, tenant role-based access control (RBAC) or a repository setting.
  These carry no severity.

So the confirmed set is a floor, and the weight of the audit is in the leads.

Coverage was planned as 58 units, each a surface, a trust boundary and an attack class, across two hunting waves.
A reviewer that had not seen the hunting pass checked each candidate, and a second fresh reviewer checked each surviving record against source line by line.
That second pass corrected wording or a citation on 15 of the 39 retained records and changed no verdict, root cause or severity.
Three candidates were rejected.
A coverage critic after the first wave accepted eight gaps, which became the second wave; the critic after the second wave and a separate final critic both accepted none.

What the audit did not do:

- **Dependency vulnerability status was not assessed.** No vulnerability database was consulted, and `make vulncheck` and `make trivy-scan` were not run.
- **The trust model is the documented one.** Where a lead says "a tenant who can write X", whether tenants hold that permission in a real install was not established.
- **Three commits landed on `main` during the audit** (#2286, #2288, #2289) and were not reviewed. #2286 adds an admission rule on `spec.tracing`, which no finding here concerns.

The recurring theme matches the second audit's: a documented isolation property that the enforcing layer does not deliver, or delivers for fewer kinds than the doc names.

## Disposition

| Finding | Verdict | Tracked by |
|---|---|---|
| [GMC ServiceAccount can write ConfigMaps (and, where the CRDs exist, CNI-native network policies and ServiceMonitors) in any namespace; tenant-resource-guard does not match those kinds](#confinement-claim) | confirmed, medium | Q1321 (`2.0-gate`), fixed |
| [Tenant-authored EgressPeer accepts 0.0.0.0/0 on any port, so the AGC NetworkPolicy can be widened past the operator's apiServerCIDRs narrowing, contrary to the documented 'single scoped peer'](#stricter-admission-rules) | confirmed, low | Q1322 (`2.0-gate`), fixed |
| [Namespaced RunnerTemplate privilege gate checks only securityContext.privileged, so the documented 'platform-owned template pins the pod shape' property is not enforced in a privileged-profile namespace](#stricter-admission-rules) | confirmed, low | Q1322 (`2.0-gate`), fixed |
| [FQDN-mode EgressProxy policy admits any hostname a tenant puts in ActionsGateway.spec.githubURL, without consulting the platform egress allowlist](#stricter-admission-rules) | confirmed, low | Q1322 (`2.0-gate`), fixed, see the finding's Resolution |
| [noProxyCIDRs admission guard does not model comma-carrying, wildcard, port-qualified or IP-literal NO_PROXY forms](#stricter-admission-rules) | validated at admission, rule added | Q1322 (`2.0-gate`), fixed |
| [noProxyCIDRs/githubURL bypass guard ignores proxyRef.namespace, so a shared EgressProxy and its cross-namespace referrers are never compared at admission](#stricter-admission-rules) | validated at admission, rule added | Q1322 (`2.0-gate`), fixed |
| [www.github.com and api.github.com spellings of githubURL yield a different scope key for the same org, so the cross-tenant scale-set name guard does not compare them](#stricter-admission-rules) | needs validation | Q1322 (`2.0-gate`), guard fixed, see the finding's Resolution; the GitHub-side question stays open |
| [Tenant RunnerTemplate volume sources are unfiltered, so a node-mounted network volume (nfs, iscsi) may give workers a data path outside the worker NetworkPolicy and egress proxy](#stricter-admission-rules) | rule gap confirmed in envtest; node mount not observed | Q1322 (`2.0-gate`), fixed |
| [Template projected serviceAccountToken volume survives buildPod despite the documented 'no Kubernetes API credentials in the worker pod' floor invariant](#stricter-admission-rules) | rule gap confirmed in envtest; token minting not observed | Q1322 (`2.0-gate`), fixed |
| [Share and attribution projection names collide for (ns 'a-b', proxy 'c') and (ns 'a', proxy 'b-c')](#confirmed-low-findings) | confirmed, low | [Q1323](../queue/Q1323.md) |
| [ActionsGateway defaultProxyRef status tells a tenant whether a named EgressProxy exists in another namespace](#confirmed-low-findings) | confirmed, low | [Q1323](../queue/Q1323.md) |
| [An attributed namespace named 'ca' overwrites the attribution CA in the clients Secret and forces a bundle regeneration on every reconcile](#confirmed-low-findings) | confirmed, low | [Q1323](../queue/Q1323.md) |
| [A direct-egress gateway's workload NetworkPolicy permits direct GitHub egress for proxied workers in the same namespace, while their status reports Proxied and attributed](#confirmed-low-findings) | confirmed, low | [Q1323](../queue/Q1323.md) |
| [A tenant can pre-claim scale-set names in a GitHub org it holds no credentials for, and another tenant's RunnerSet or gateway is then rejected at admission](#confirmed-low-findings) | confirmed, low | [Q1323](../queue/Q1323.md) |
| [Provider-writable proxy TLS Secret is projected unvalidated into consumer namespaces and installed as an unconstrained trust root by consumer AGCs and workers](#egress-proxy-sharing-leads) | confirmed by a local test, fixed | Q1324 |
| [A tenant can stall another tenant's EgressProxy reconcile with a labelled ConfigMap and an unblankable Secret in its own namespace](#egress-proxy-sharing-leads) | confirmed by a local test, fixed | Q1324 |
| [Bring-your-own-autoscaled EgressProxy lets the live Deployment replica count drive an unbounded repeated-addition loop on the GMC's single EgressProxy worker](#egress-proxy-sharing-leads) | confirmed by a local test, fixed | Q1324 |
| [Transport-only proxy tunnels to its own loopback listeners, so a worker can replace its source address and arrival port with the proxy's](#egress-proxy-sharing-leads) | confirmed by a local test, fixed | Q1324 |
| [Worker-pod reaper deletes whichever GitHub runner the pod's runner-name annotation names, with no ownership test](#scale-set-identity-leads) | needs validation | [Q1325](../queue/Q1325.md) |
| [Editing runnerLabels[0] releases the scale-set name claim while the AGC keeps its live session on the old name](#scale-set-identity-leads) | needs validation | [Q1325](../queue/Q1325.md) |
| [Scale-set name uniqueness guard is check-then-act across two objects, so one tenant creating its RunnerSet and ActionsGateway concurrently may claim another tenant's scale-set name](#scale-set-identity-leads) | needs validation | [Q1325](../queue/Q1325.md) |
| [AGC adopts the first scale set a server-side name filter returns without comparing its name, while admission compares first labels as exact case-sensitive strings](#scale-set-identity-leads) | needs validation | [Q1325](../queue/Q1325.md) |
| [publish.yml dispatch `tag` input is never compared with the dispatch ref, so one tag's source can be published and signed under another version name while the stop-ship gates evaluate the named version](#release-and-ci-leads) | same-principal today, accepted residual | Q1326, in [05-security.md](../design/05-security.md#who-can-start-a-release-is-held-by-a-headcount-not-a-control) |
| [Release verification identity pattern accepts a publish.yml signature from any v* tag for any version](#release-and-ci-leads) | same-principal today, accepted residual | Q1326, in [05-security.md](../design/05-security.md#who-can-start-a-release-is-held-by-a-headcount-not-a-control) |
| [Historical publish.yml copies at 28 v* tags have no validated-candidate gate and accept a free-form version name, under a signing identity verify-release accepts for any version](#release-and-ci-leads) | same-principal today, accepted residual | Q1326, in [05-security.md](../design/05-security.md#who-can-start-a-release-is-held-by-a-headcount-not-a-control) |
| [pages.yml publish job deploys dispatch-chosen content from any ref and serves the whole gh-pages branch](#release-and-ci-leads) | same-principal today, accepted residual | Q1326, in [05-security.md](../design/05-security.md#who-can-start-a-release-is-held-by-a-headcount-not-a-control) |
| [Dependabot sync and rebase writers compile and run a freshly bumped build tool in a job holding a contents:write token](#release-and-ci-leads) | confirmed from a run log; dispatch refuted, tag path an accepted residual | Q1326, in [05-security.md](../design/05-security.md#who-can-start-a-release-is-held-by-a-headcount-not-a-control); hardening [Q1341](../queue/Q1341.md) |
| [No source-visible control stops a fork pull request from naming a dogfood scale-set label in runs-on](#release-and-ci-leads) | closed by Q1330's default refusal once the dogfood cluster runs it; GitHub's approval setting holds it until then | Q1330 |
| [go-throttle hook returns `allow` for single `go build`/`go test` commands whose flags or GOFLAGS run a caller-named program or write to a caller-named path](#release-and-ci-leads) | confirmed against go1.27.1, fixed | Q1326 |
| [AGC re-runs and force-cancels whatever run its namespace ConfigMaps and pod annotations name; docs/design/05-security.md line 200 describes only the pod annotation and says a run ID cannot be substituted](#agc-run-and-recovery-leads) | needs validation | [Q1327](../queue/Q1327.md) |
| [One repository's OOM-killed jobs can fill the per-set failure ledger and withhold automatic re-runs for every repository on the RunnerSet](#agc-run-and-recovery-leads) | needs validation | [Q1327](../queue/Q1327.md) |
| [Throughput sizing profile derives the worker memory limit from job-controlled peak usage with no maxRequests clamp, so the limit can exceed the operator's envelope and may ratchet](#agc-run-and-recovery-leads) | needs validation | [Q1327](../queue/Q1327.md) |
| [Tenant-chosen workerImage host (and registry redirects) steer AGC GETs to in-cluster 443/6443 endpoints off the egress proxy, with the outcome reported in RunnerSet status](#agc-run-and-recovery-leads) | needs validation | [Q1327](../queue/Q1327.md) |
| [AGC run-status, re-run and force-cancel errors carry the raw upstream body into logs, and run-status and re-run errors into a tenant-namespace Event, bypassing SanitizeBody](#agc-run-and-recovery-leads) | needs validation | [Q1327](../queue/Q1327.md) |
| [Gateway teardown deletes the workload NetworkPolicy while worker pods running job code can still be alive](#worker-pod-and-networkpolicy-leads) | needs validation | [Q1328](../queue/Q1328.md) |
| [Unmarking a tenant namespace revokes only the one canonical reader RoleBinding, so other GMC-written bindings keep granting Secret access there](#worker-pod-and-networkpolicy-leads) | needs validation | [Q1328](../queue/Q1328.md) |
| [namespace-psa-guard admits the GMC ServiceAccount setting Pod Security enforce=privileged on a marked tenant namespace that lacks the platform eligibility label](#worker-pod-and-networkpolicy-leads) | needs validation | [Q1328](../queue/Q1328.md) |
| [DNS egress rule confines which resolver a pod may contact, not which names it resolves; the design states the DNS exfiltration channel is closed](#worker-pod-and-networkpolicy-leads) | needs validation | [Q1328](../queue/Q1328.md) |
| [GitHub CIDR allowlist admits /meta `actions` ranges that serve Azure blob storage, so direct-mode worker 443 egress may not be GitHub-only](#worker-pod-and-networkpolicy-leads) | needs validation | [Q1328](../queue/Q1328.md) |

## Confinement claim

The one medium finding, tracked as Q1321, which gated `v2.0.0` and has landed.
The design doc claimed a confinement the shipped policy did not deliver, and a General Availability release should not carry a security claim that source refutes.

**Fixed by both routes.** `gmc-tenant-resource-guard` now matches ConfigMaps, the three CNI-native policy kinds and ServiceMonitors, in the chart template and the config copy, and [05-security.md](../design/05-security.md#gmc-privilege-escalation-blast-radius-and-compensating-controls) states what stays cluster-wide: ConfigMap reads, Events, and the GMC's own API kinds.
A unit test fails when the manager ClusterRole grants a write that no shipped policy matches, and a second holds each config copy to its chart template.
Two ConfigMap writes are admitted outside a tenant namespace, which the finding's own fix did not anticipate in full: any in the install namespace, as it proposed, and the delete of a proxy-share or failure-attribution projection, without which neither could be withdrawn from a namespace that had lost its marker and the provider proxy's reconcile would fail there.
The role was not narrowed: the GMC lists its share projections by label across namespaces, so ConfigMap `get`/`list` stays cluster-wide and is recorded as a residual.
The finding below is kept as written on 2026-11-09.

### GMC ServiceAccount can write ConfigMaps (and, where the CRDs exist, CNI-native network policies and ServiceMonitors) in any namespace; tenant-resource-guard does not match those kinds

`gmc-tenant-resource-guard:ungated-configmap-cni-servicemonitor-writes`: confirmed, medium.
Starts at `charts/actions-gateway/templates/rbac.yaml:17`.

The guard's resourceRules cover the original provisioning kinds and were not extended when the role gained write verbs on ConfigMaps (the egressproxy_controller.go marker widened configmaps from get to the full write set for cross-namespace proxy-share projection; ipranges_store.go adds create/update), the CNI-native FQDN policy kinds, and ServiceMonitors.
Nothing compares the role's namespaced write grants against the guard's matched resources, and the envtest guard suite exercises only kinds the guard already names.

**Established by source.** Derived by reading source, not by running anything.
The ClusterRole authorizes configmaps create/update/patch/delete cluster-wide.
No resourceRule in tenant-resource-guard lists configmaps, so its matchConstraints do not select the request and its validations are not evaluated; no other shipped policy or webhook matches the resource.
A GMC-identity ConfigMap write to an unmarked namespace is therefore admitted, contradicting 05-security.md:52-54.
By the same reading the guard does not select writes to ciliumnetworkpolicies, projectcalico.org networkpolicies, fqdnnetworkpolicies or servicemonitors on clusters that serve those APIs.
The verifier also found the envtest copy of the guard lacks the verticalpodautoscalers rule the chart template has (chart lines 45-48), a separate drift that does not affect this result.

**Fix.** Add the five kinds to tenant-resource-guard in both the chart template and the cmd/gmc/config/admission-policy copy, and admit ConfigMap writes outside a tenant namespace only in the GMC's own install namespace (the IP-range store and leader election live there).
Before shipping, confirm each controller's writes of these kinds land only in marked tenant namespaces or the install namespace, since a proxy-share projection into an unmarked consumer namespace would start being denied.
Add envtest cases: a GMC-identity ConfigMap create in an unmarked namespace is denied, and the IP-range store and a share projection into a marked namespace still succeed.
Add a test that derives the namespaced write kinds from cmd/gmc/config/rbac/role.yaml and fails when one is missing from the guard's resourceRules.
Correct docs/design/05-security.md lines 23 and 52-54 to state what is and is not gated, including that ConfigMap get/list is cluster-wide and cannot be gated by admission; narrowing the role (dropping list and patch on configmaps if the controllers allow) reduces that read.
The code change below is a sketch and was not rendered or tested.

**Evidence.** `docs/design/05-security.md:54`, `docs/design/05-security.md:23`, `cmd/gmc/internal/controller/egressproxy_controller.go:27`, `cmd/gmc/internal/controller/actionsgateway_v2_controller.go:40`, `cmd/gmc/config/admission-policy/tenant-resource-guard.yaml:41`, `cmd/gmc/internal/controller/integration/tenant_resource_guard_test.go:107`, `charts/actions-gateway/templates/namespace-psa-guard.yaml:18`.

## Stricter admission rules

Eight findings whose fix is a stricter admission rule, tracked together as Q1322 and gating `v2.0.0` for the reason Q1283 and Q1312 were: a manifest that applies today would be refused afterwards, so only a major can take the rule, and the next major is `v3`.
All eight have landed for `v2.0.0`.
Three are confirmed and five are leads.
For the leads the gap in the rule is visible in source; what is unsettled is whether anything gets through the layer behind it.
Each rule must pass an unchanged stored value, as `spec.tracing.endpoint`'s does.

### Tenant-authored EgressPeer accepts 0.0.0.0/0 on any port, so the AGC NetworkPolicy can be widened past the operator's apiServerCIDRs narrowing, contrary to the documented 'single scoped peer'

`egresspeer-unbounded-peer-agc-networkpolicy`: confirmed, low.
Starts at `api/v2/shared_types.go:304`.

**Fixed by a rule on the CIDR, with the docs corrected for what it leaves.** Two CEL rules on `EgressPeer.cidr` refuse a value that is not a CIDR or is broader than `/16` for IPv4 or `/48` for IPv6, and one that is link-local or covers one of four metadata and platform addresses that clouds serve outside link-local space (Alibaba Cloud, Azure, and the IPv6 addresses of AWS and Google Cloud); each passes an unchanged stored value.
That list is not complete and the docs no longer say the peer cannot name a metadata server: IPv6 forms that embed `169.254.169.254` for a translator (`64:ff9b::a9fe:a9fe`, `2002:a9fe:a9fe::/48`, `::a9fe:a9fe`) are admitted, because a NAT64 prefix is also how an IPv6-only cluster reaches a legitimate IPv4 peer, and whether any of them reaches a node's metadata server was not tested.
The floor is a policy number with no measurement behind it, set at the largest network a single CIDR has to cover: an AWS VPC is at most a `/16`, and `/48` is one IPv6 site.
Three parts of the proposed fix were decided against, and [05-security.md](../design/05-security.md#57-workload-identity-the-no-pem-delegation-model) says what each leaves open.
A selector peer is not bounded, because a rule refusing `{}` would leave an `Exists` expression that selects the same pods, and a selector reaches pods only, never a node address.
No operator allowlist gates the peer, since an opt-in one is additive and needs no major.
The builder does not narrow a stored value, so a `0.0.0.0/0` stored before `v2.0.0` keeps its rule until an operator narrows it, which `upgrade.md` tells them how to find.
The finding below is kept as written on 2026-11-09.

EgressPeer validation is structural only: the CEL rule at api/v2/actionsgateway_types.go:537 checks exactly-one-of, the pattern at :558 matches any prefix length including /0, the ActionsGateway webhook (actionsgateway_webhook.go:155-200) never inspects the peer, and egressPeerTo (actionsgateway_v2_builder.go:440-448) passes the CIDR straight into an IPBlock. buildAGCNetworkPolicyV2 appends the rule regardless of apiServerCIDRs, so nothing subordinates the tenant-delegated hole to the operator's scope.

**Established by source.** Derived by reading source, not by running: the built AGC policy contains the egress rule {Ports:[80], To:[{IPBlock:{CIDR:"0.0.0.0/0"}}]} in addition to the apiserver rule scoped to 10.0.0.0/24, so the authored policy admits TCP/80 from the AGC pod to every IPv4 address, including 169.254.169.254, while the same gateway without the field does not.
No traffic, CNI enforcement or data read was observed.

**Fix.** Bound the tenant-delegated peer at admission and make it subordinate to operator scoping: reject a cidr shorter than a minimum prefix length (and ranges overlapping link-local) in the ActionsGateway webhook or with a CEL cidr() rule, decide whether an empty selector is acceptable, and optionally gate CIDR peers behind an operator allowlist when --apiserver-cidrs is set.
Otherwise correct the godoc and 05-security.md so they stop claiming a single scoped peer and that apiServerCIDRs closes everything.
Add an AGC-with-peer case to TestBuildNetworkPolicy_DeniesCloudMetadataServer and a webhook rejection test.

**Evidence.** `api/v2/actionsgateway_types.go:529`, `docs/design/05-security.md:979`, `docs/design/05-security.md:361`, `docs/design/05-security.md:985`, `charts/actions-gateway-crds-v2/templates/crds/actionsgateway-crd.yaml:2110`, `cmd/gmc/internal/controller/metadata_egress_test.go:124`.

### Namespaced RunnerTemplate privilege gate checks only securityContext.privileged, so the documented 'platform-owned template pins the pod shape' property is not enforced in a privileged-profile namespace

`runnertemplate-privileged-equivalent-shape`: confirmed, low.
Starts at `cmd/gmc/internal/webhook/v2/runnertemplate_webhook.go:194`.

validateReservedPodFields equates 'a privileged worker shape' with the single boolean securityContext.privileged (isPrivileged).
It reads no other host-reaching field (capabilities.add, hostPath and other volume sources, procMount, seccomp or AppArmor profile, runtimeClassName), buildPod passes those fields through, applySecurityDefaults is a no-op under the privileged profile, and nothing binds a RunnerSet in a privileged-profile namespace to a ClusterRunnerTemplate.

**Established by source.** Derived by reading, not running: the RunnerTemplate admission function returns no error for this spec, and buildPod returns a Pod whose spec still carries the hostPath volume, the added capabilities, the Unconfined seccomp profile and uid 0 with no runtimeClassName.
Source therefore establishes that no GAG control distinguishes this tenant-authored shape from the platform's kata-dind shape, contrary to kata-dind-workloads.md:45, :176, :293 and 05-security.md:884.
Whether the apiserver admits the pod is decided only by the namespace Pod Security level; admission in a privileged namespace and any node effect were not observed.

**Fix.** Decide which statement is authoritative.
To deliver the documented property, make the namespaced-kind gate evaluate the Pod Security Standards baseline level rather than one boolean: when rejectPrivileged is true in validateReservedPodFields, reject capabilities.add outside the baseline set, hostPath volumes, hostPort, procMount other than Default, Unconfined seccomp or AppArmor, and non-baseline sysctls on the pod and every container and init container, and add a table case per field to the existing validateReservedPodFields tests in cmd/gmc/internal/webhook/v2/runnertemplate_webhook_test.go.
If the narrower check is kept by design, correct docs/design/05-security.md:884, docs/operations/kata-dind-workloads.md:45, :176 and :293 and deploy/templates/kata-dind/template.yaml:33-35 to say that a tenant in a privileged-profile namespace can author any pod shape short of privileged: true and the reserved host-namespace fields, so platform ownership of the template does not pin the worker shape there.

**Resolved in `v2.0.0` by enforcing, not by correcting the docs.** The namespaced-kind webhook now evaluates the whole `podTemplate` against the Pod Security Standards `baseline` level on every profile, through the upstream evaluator (`k8s.io/pod-security-admission/policy` v0.37.2, vendored) and not the hand-written field list sketched above, so the webhook and a `baseline` namespace cannot drift on what `baseline` means.
The evaluation is pinned to Pod Security Standards 1.37 and does not use the library's latest version: at latest, a Dependabot bump of `k8s.io/pod-security-admission` that carried a stricter check would refuse, in a minor release, a manifest the previous one admitted.
The cost is that a later upstream tightening reaches a `privileged`-profile namespace only when the pin is raised, which is a breaking change to schedule; `TestPodShapeVersion_IsPinned` fails if the pin is replaced by latest.
Every `baseline` check forbids a set value and none requires one, so evaluating the partial template gives the verdict the built pod would get.
None of the three shipped `deploy/templates/` entries is refused, because all three are `ClusterRunnerTemplate`s; `TestTemplateLibrary_PodShape` pins that, and that `kata-dind` and `privileged-dind` are refused as the namespaced kind.
A stored template passes while its `podTemplate` is unchanged.
`runtimeClassName` is not a `baseline` field and stays tenant-settable.
The rule and the volume-type rule have one install-wide opt-out, the GMC flag `--allow-unchecked-runner-template-pod-shape`, default off.
It exists because admission is conjunctive: a platform's own `ValidatingAdmissionPolicy` or policy engine cannot admit a template this webhook refuses, so without the flag a platform that already decides tenant pod shape, more finely than baseline-or-nothing, would lose that decision to a rule it cannot configure.
With the flag set and no such policy, the finding stands as audited, and so does the volume-source finding below, which the same flag covers; the flag does not reach the API-credential rule.

**Evidence.** `docs/operations/kata-dind-workloads.md:45`, `docs/operations/kata-dind-workloads.md:176`, `docs/operations/kata-dind-workloads.md:293`, `deploy/templates/kata-dind/template.yaml:33`, `docs/design/05-security.md:884`, `docs/design/appendix-h-v2-api-decomposition.md:449`, `cmd/gmc/internal/webhook/v2/runnertemplate_webhook.go:81`, `docs/design/05-security.md:754`, `docs/operations/runner-template-library.md:126`.

### FQDN-mode EgressProxy policy admits any hostname a tenant puts in ActionsGateway.spec.githubURL, without consulting the platform egress allowlist

`gmc/egressproxy-fqdn-referrer-githuburl-host-not-allowlisted`: confirmed, low.
Starts at `api/v2/actionsgateway_types.go:41`.

resolveReferrerGitHubHosts returns each bound referrer's githubURL hostname after dropping only empty values and hosts already in the built-in public GitHub list (cmd/gmc/internal/controller/egressproxy_referrers.go:102-105). egressFQDNs (egressproxy_fqdn.go:133-138) and proxyAllowlistEnv (egressproxy_builder.go:194) append those hosts beside the allowlist-gated destinationFQDNs with no call to EgressDestinationAllowlist.CoversFQDN.
The only allowlist check on any admission path is validateEgressDestinations, which iterates spec.destinationFQDNs and spec.destinationCIDRs alone; the ActionsGateway webhook holds no reference to the allowlist.

**Established by source.** Derived by reading the source, not by running it: both objects are admitted with an empty platform allowlist, and the GMC emits a CiliumNetworkPolicy (or the Calico/GKE equivalent) for the pool whose TCP/443 egress rule lists exfil.example.net beside the six GitHub hosts, with a proxy Deployment carrying no CONNECT destination allowlist.
No step on the path calls EgressDestinationAllowlist.
This contradicts docs/design/05-security.md:159 and :329.
Whether packets then reach the host depends on the CNI and DNS and was not observed.

**Fix.** Gate referrer GitHub hosts on the same platform allowlist as destinationFQDNs.
Give the EgressProxy reconciler the shared EgressDestinationAllowlist and drop any referrer host that is not in the built-in GitHub set and not covered by CoversFQDN, reporting it on the GitHubEgressIncomplete condition with a new reason, so a GitHub Enterprise Server host needs one platform allowlist entry in FQDN mode exactly as its ranges do in CIDR mode.
Optionally also reject at ActionsGateway/RunnerSet admission when the bound proxy is FQDN-mode and the host is uncovered.
Validate the extracted host as an RFC 1123 name before emitting it, so no wildcard or pattern character can reach a matchPattern/pattern rule.
If self-serve GHES in FQDN mode is the intended design instead, correct docs/design/05-security.md:156-159 and :329 and the threat row at :203 to say githubURL is a tenant-controlled egress destination there.
Regression cases: extend TestResolveReferrerGitHubHosts in cmd/gmc/internal/controller/egressproxy_referrers_test.go with an off-allowlist GHES referrer expecting no host, an allowlisted one expecting the host, and a wildcard host expecting none; add an FQDN-mode case to cmd/gmc/internal/controller/integration/v2_egressproxy_ghes_egress_test.go asserting the emitted policy omits an unallowlisted githubURL host.

**Evidence.** `cmd/gmc/internal/webhook/v2/egressproxy_webhook.go:48`, `cmd/gmc/internal/webhook/validation/validation.go:69`, `cmd/gmc/internal/controller/egressproxy_referrers.go:103`, `cmd/gmc/internal/controller/egressproxy_builder.go:187`, `cmd/proxy/main.go:16`, `docs/design/05-security.md:156`, `docs/design/05-security.md:329`, `docs/design/network-architecture.md:309`, `docs/operations/security-operations.md:656`, `docs/plan/archive/q506-ghes-endpoint-audit.md:85`, `cmd/gmc/internal/controller/egressproxy_referrers.go:62`, `cmd/gmc/internal/controller/egressproxy_builder.go:527`.

**Resolution.** The EgressProxy reconciler now holds the shared `EgressDestinationAllowlist`, and `resolveReferrerGitHubHosts` splits referrer hosts into allowed and refused.
Only an allowed host reaches the FQDN policy and the proxy's CONNECT allowlist, in every egress mode.
A host is allowed when it is an RFC 1123 name, lowercased and with one trailing dot dropped, that `CoversFQDN` accepts, so `*.corp.example` is refused even where `corp.example` is allowlisted.
Public GitHub needs no entry: the built-in host list is matched without regard to case, and the `www.github.com` spelling of public GitHub is allowed as before.
A refused host sets `GitHubEgressIncomplete=True` with the new reason `GitHubHostNotAllowlisted` wherever the pool cannot reach it by address: managed FQDN mode, or an unmanaged pool with `destinationFQDNs` and no `destinationCIDRs`.

The allowlist's ConfigMap half loads after the GMC starts and changes under it, which the gate has to survive in both directions.
A reconcile that would refuse a host before the first ConfigMap read holds and requeues, so a GMC restart does not narrow a live policy.
The ConfigMap reconciler reads once at start whether or not the object exists, and the hold ends 30 seconds after the GMC starts regardless, so a named ConfigMap that was never created cannot keep a pool from reconciling.
Each ConfigMap read then requeues every EgressProxy, so an added entry reaches the policy at once and not at the six-hour re-check.

This changes what a stored GHES gateway on an FQDN-mode pool can reach on upgrade, and `docs/operations/upgrade.md` says how to find one and which allowlist entry restores it.
The optional admission-time rejection is declined.
The allowlist and the proxy binding both change after admission, so the reconciler has to enforce the rule whatever admission says, and a rejection would add a second stricter rule for no added protection.
An admission warning gives the same early signal without refusing anything and can ship in a minor ([Q1334](../queue/Q1334.md)).
Whether packets reach a covered host still depends on the CNI and DNS and was not observed: the envtest asserts the emitted policy and the proxy's environment, not traffic.

### noProxyCIDRs admission guard does not model comma-carrying, wildcard, port-qualified or IP-literal NO_PROXY forms

`gmc-webhook-noproxy:matcher-misses-no-proxy-forms`: needs validation.
Starts at `api/v2/egressproxy_types.go:208`.

**Resolved for `v2.0.0`: the admission gap is real and the rule is added.** The first blocker below is settled by running the consumer, Go's `net/http` on go1.27.3, in a child process per `NO_PROXY` value.
The comma, `*`, `*.domain` and `host:port` forms each exempt the GitHub host.
So does a hostname spelled in fullwidth letters, which the audit did not list: the client compares a non-ASCII name in its IDNA form.
An address or CIDR entry exempts only a host that is itself an IP literal, so it matters for a `githubURL` written as an IP and never for a GitHub hostname.
The client compares a trailing dot literally: a dot-only entry (`.`, `*.`) is the suffix `.` and exempts every host written with a trailing dot, which `githubURL` admits, while `github.com.` exempts `github.com.` and not `github.com`.
The client ignores `*domain` with no dot and a bracketed IPv6 address with no port, and the rule does not model them.

`noproxy.ValidateEntries` now reads an entry the way that client does, and `TestMatchesAgreesWithNetHTTP` fails if the two disagree in either direction.
It is stricter in four declared places.
A port-qualified entry is refused whatever the port, because the guard is handed a host and not the port its traffic uses.
The other three follow a second consumer, libcurl, which `git` and `curl` in a worker pod read the same `NO_PROXY` through: a leading-dot entry also matches the bare domain, a space or a tab separates entries as a comma does, and one trailing dot on the entry or the host is ignored.
Those three were measured by hand against curl 8.7.1 on macOS on 2026-11-10, with an unresolvable host and a closed proxy port; no test pins them, because a test that runs `curl` would assert on the build host's tooling.
An `EgressProxy` update re-judges the entries only when the list changes, so a stored value passes an unrelated edit.
The second blocker stays open and the rule does not depend on it: whether exempted traffic leaves the pod was not measured, and neither was the libcurl in a worker image or the Actions runner's own parser.

noproxy.hostnameMatches (cmd/gmc/internal/webhook/noproxy/noproxy.go:89-96) validates each array element as one hostname using equality or dot-suffix comparison, while the builders treat the array as fragments of a comma-separated NO_PROXY string (strings.Join with ',').
An element is never split on commas before validation, a '*' or '*.' prefix and a ':port' suffix are compared literally, ValidateEntries skips CIDR and bare-IP entries unconditionally (noproxy.go:54), and the CRD puts no pattern on the items.

**Blocked on:**

- The NO_PROXY parser the AGC applies is the Go standard library's (not vendored in the repository; no httpproxy directory exists in the tree) and the worker's is the Actions runner and job tooling.
  That '*', '*.domain' and 'domain:port' are honoured as GitHub matches is not readable from target source.
  For the comma form only the splitting of NO_PROXY on commas is assumed, which the repository's own builders and comments rely on.
- Whether exempted GitHub traffic leaves the pod depends on the CNI enforcing the workload NetworkPolicy, on spec.managedNetworkPolicy, and on the gateway being direct with a RunnerSet-level proxyRef (the one source-visible configuration where the policy also allows GitHub CIDRs); none is a source fact.
- No sandbox was available in this run, so ValidateEntries could not be executed; the admission result for the four entry shapes is derived by reading hostnameMatches.

**Local test.** In cmd/gmc/internal/webhook/noproxy/noproxy_test.go add TestValidateEntries_RejectsCommaWildcardAndPortForms asserting an error from ValidateEntries("spec.noProxyCIDRs", []string{e}, GitHubHosts) for each of "x,github.com", "*", "*.github.com", "github.com:443", and for "10.0.0.0/8" and "10.1.2.3" against protected host "10.1.2.3"; it is expected to fail today.
Beside it add a test that passes the string built the way buildNoProxy builds it for each entry to httpproxy.Config{HTTPSProxy: "https://p:8080", NoProxy: s}.ProxyFunc() and asserts a nil proxy URL for https://api.github.com/, which pins which admitted forms the Go consumer treats as a GitHub exemption.
Add the same entries to the EgressProxy cases in cmd/gmc/internal/controller/integration/v2_egressproxy_admission_test.go.

**Owner check.** On a test cluster the owner applies an EgressProxy with noProxyCIDRs ["x,github.com"] in a tenant namespace, confirms admission succeeds, reads the AGC Deployment's NO_PROXY env, and checks status.proxyMode.
Then, with a RunnerSet proxyRef under a gateway that has no defaultProxyRef, runs one job and checks whether the worker's GitHub connections appear in the proxy pool's connection log or leave from the node address.

**Evidence.** `cmd/gmc/internal/webhook/noproxy/noproxy.go:32`, `cmd/gmc/internal/webhook/noproxy/noproxy.go:54`, `cmd/gmc/internal/webhook/noproxy/noproxy_test.go:14`, `charts/actions-gateway-crds-v2/templates/crds/egressproxy-crd.yaml:264`, `docs/design/05-security.md:165`, `cmd/gmc/internal/controller/actionsgateway_v2_builder.go:290`, `cmd/agc/internal/controller/runnerset_controller.go:628`.

### noProxyCIDRs/githubURL bypass guard ignores proxyRef.namespace, so a shared EgressProxy and its cross-namespace referrers are never compared at admission

`gmc-webhook-noproxy:cross-namespace-proxyref-unchecked`: needs validation.
Starts at `api/v2/shared_types.go:202`.

**Resolved for `v2.0.0`: the admission gap is real and the rule is added.** The first blocker below is settled by the webhook unit tests and the envtest suite: with the namespace handling removed, the provider-side and consumer-side cases for a shared proxy are admitted, as the finding reads from source.
Both directions now resolve `ProxyObjectRef.Namespace` and count a cross-namespace pair only where the proxy grants the referrer's namespace, the binding `resolveReferrerGitHubHosts` applies.
A referrer naming a proxy that does not grant it is admitted without the proxy's entries being judged: the reference wires nothing, and a rejection would tell a tenant what another namespace's proxy excludes.
The `EgressProxy` write that adds the grant is checked against the newly granted namespace's referrers, so the pair cannot be assembled grant-last.
The proxy side lists gateways and `RunnerSet`s cluster-wide only for a proxy that grants a namespace, and on update only when `noProxyCIDRs` changed or a namespace was added.
Each side's update judges only the pairs that write can newly bind, so a stored pair passes an unrelated edit.
The other two blockers stay open and the rule does not depend on them; the envtest assertion on the projected share ConfigMap proposed below was not added, because it tests the downstream effect and not the rule.

validateGitHubHostAgainstProxy takes a single namespace argument and its three callers (actionsgateway_webhook.go:79 and :96, runnerset_webhook.go:201) pass the referrer's namespace with ref.Name only; referrerGitHubHosts matches on ref.Name alone and scopes both Lists to the proxy's namespace.
Neither was updated when ProxyObjectRef gained Namespace for cross-namespace sharing, unlike the reconcile-side resolveReferrerGitHubHosts.

**Blocked on:**

- No sandbox on this host: the webhook unit and envtest suites could not be run, so the admission outcome (admit on the same-namespace NotFound) is read from source rather than observed.
- Whether GitHub Enterprise Server traffic exempted by NO_PROXY actually leaves around the shared proxy, rather than being dropped, depends on the consumer namespace's proxied-mode NetworkPolicy being enforced by the cluster CNI, which is a deployment fact not in source.
- The NO_PROXY suffix match is performed by the Go standard library in the AGC and by the runner and job tooling in the worker, none of which is repository code.

**Local test.** In cmd/gmc/internal/webhook/v2/egressproxy_webhook_test.go add a case with a fake reader holding EgressProxy provider/shared (sharing.allowedNamespaces [consumer], noProxyCIDRs ["corp.example"]) and ActionsGateway consumer/gw (githubURL https://ghes.corp.example/org, defaultProxyRef {name: shared, namespace: provider}); assert ValidateCreate and ValidateUpdate of the proxy reject.
In actionsgateway_webhook_test.go and runnerset_webhook_test.go add the mirror cases asserting the consumer-side create is rejected (and that a same-named proxy in the consumer namespace is not the one consulted).
All are expected to admit today.
Then in the GMC envtest suite under cmd/gmc/internal/controller/integration assert the projected share ConfigMap carries no-proxy corp.example and the consumer AGC Deployment's NO_PROXY contains it.

**Owner check.** On a disposable test cluster the owner creates a sharing EgressProxy whose noProxyCIDRs holds a suffix covering a consumer gateway's GitHub Enterprise Server host, confirms both objects are admitted, reads NO_PROXY on the consumer's AGC Deployment and on a worker pod, and observes whether a connection from the worker to that host is dropped by NetworkPolicy or leaves from a non-proxy source IP.

**Evidence.** `docs/design/05-security.md:166`, `api/v2/egressproxy_types.go:194`, `cmd/gmc/internal/controller/egressproxy_referrers.go:35`, `cmd/gmc/internal/webhook/v2/runnerset_webhook.go:201`, `cmd/gmc/internal/webhook/v2/egressproxy_webhook.go:96`.

### www.github.com and api.github.com spellings of githubURL yield a different scope key for the same org, so the cross-tenant scale-set name guard does not compare them

`scalesetscope-github-host-alias-splits-scope`: needs validation.
Starts at `cmd/gmc/internal/webhook/validation/validation.go:69`.

scalesetscope.GitHubScope normalizes case, port and trailing slash but not the host aliases that githubapp.DeriveAPIBaseURL collapses to public GitHub, so the guard's identity for a scope is finer than the identity the AGC addresses.

**Blocked on:**

- Whether GitHub's POST /actions/runner-registration accepts url=https://www.github.com/<org> (or https://api.github.com/<org>) and returns the same Actions service tenant as https://github.com/<org> is a GitHub-side fact not determinable from source; if it rejects the alias the second listener fails to start and nothing crosses tenants.
- The actor needs a GitHub App installation with runner-admin access on the same org; whether two tenants sharing one org are distinct principals in a given deployment is an owner fact.

**Local test.** In cmd/gmc/internal/scalesetscope/scalesetscope_test.go add a GitHubScope table case asserting https://github.com/acme, https://www.github.com/acme and https://api.github.com/acme produce one key, and in the RunnerSet webhook tests under cmd/gmc/internal/webhook/v2 add a case with two gateways in different namespaces using the two spellings and one first runnerLabel, expecting rejection.
Both are expected to fail on the current code.
Candidate fix: fold the three DeriveAPIBaseURL public hosts to github.com in GitHubScope (or reject the aliases in validation.GitHubURL).

**Owner check.** Owner-observed, in a scratch org: register one ScaleSet RunnerSet under a gateway with https://github.com/<org>, then a second in another namespace under https://www.github.com/<org> with the same first label, and observe whether the second AGC's RunnerSet reaches Ready reporting the same scale set ID, or fails at runner-registration.

**Evidence.** `cmd/gmc/internal/scalesetscope/scalesetscope.go:52`, `githubapp/auth.go:230`, `scaleset/client.go:271`, `docs/design/05-security.md:668`, `cmd/gmc/internal/controller/actionsgateway_scaleset_collision.go:60`, `api/v2/actionsgateway_types.go:39`.

**Resolution.** `GitHubScope` now folds `www.github.com` and `api.github.com` to `github.com`, and a unit test holds its alias list to the hosts `DeriveAPIBaseURL` treats as public GitHub.
It also drops a trailing dot on the host, since `github.com.` resolves as `github.com` does.
A new gateway under either alias is also rejected: on create, and on an update that changes `githubURL`, which the CRD's immutability rule refuses first.
The fold is what keeps two tenants apart, and the rejection keeps new gateways off spellings whose registration behaviour is unmeasured.
A rule that rejects a value earlier releases accepted is a breaking change, so it went into `v2.0.0`.
It costs one more breaking change, and a stored alias gateway cannot be re-created from a backup until its host is written `github.com`.
The rejection is not in `validation.GitHubURL`, which re-runs on every update: `githubURL` is immutable, so there it would refuse every later edit to a stored alias gateway.
The fold has the same problem one step removed, because both scope guards also re-run on update.
Two tenants stored on different spellings with one scale-set name would each be refused every unrelated edit after the upgrade, and either could hold the other there.
So an update that leaves the claim as stored is compared by the scope as written, which is the comparison made before the fold: a RunnerSet keeping its first `runnerLabel` and `gatewayRef`, or a gateway keeping its `githubURL`.
Any new or changed claim is compared folded, and the reconciler reports a stored alias pair on both gateways as `ScaleSetNameCollision`.

Both Blocked-on items stay open.
Whether GitHub's runner registration accepts the alias spellings, and so whether a stored pair ever shared a scale set, was not measured, and the Owner check above is still the way to find out.
A related defect in the same host comparison is tracked separately: `DeriveAPIBaseURL` matches the three hosts by exact case and with no trailing dot ([Q1333](../queue/Q1333.md)).

### Tenant RunnerTemplate volume sources are unfiltered, so a node-mounted network volume (nfs, iscsi) may give workers a data path outside the worker NetworkPolicy and egress proxy

`worker-pod-node-mounted-network-volume`: needs validation.
Starts at `api/v2/runnertemplate_types.go:38`.

Neither the RunnerTemplate admission path (CRD CEL rules, GMC validating webhook) nor Provisioner.buildPod limits volume source types on a tenant-authored pod template, and the default baseline Pod Security level the design names as the pod-spec backstop is not documented or tested here as restricting network-backed volume types, while the egress guarantees in the design are stated without that exception.

**Blocked on:**

- Source-only run with no sandbox: whether Pod Security Admission at the baseline level admits a pod with an nfs or iscsi volume source at the supported Kubernetes version (>= 1.35) is apiserver behaviour; the pod-security-admission policy source is not vendored in this repository, so it could not be read or exercised.
- Whether worker nodes carry an NFS or iSCSI client, and whether node-originated traffic may reach a tenant-chosen server, are node-image, node-firewall and CNI host-policy facts outside source.
- Tenant RBAC is a deployment fact: if a tenant may create pods directly in its namespace, the same volume is reachable without a RunnerTemplate and the template path adds no authority; and a cluster admission policy may already restrict pod volume types in tenant namespaces.

**Local test.** Add a case to TestValidateReservedPodFields in cmd/gmc/internal/webhook/v2/runnertemplate_webhook_test.go giving the namespaced kind a podTemplate with an nfs volume and asserting rejection (expected to fail today, which pins the missing check).
In the GMC envtest suite under cmd/gmc/internal/controller/integration, create a pod with an nfs volume and one with an iscsi volume in a namespace labelled pod-security.kubernetes.io/enforce=baseline and again with enforce=restricted, and record for each whether the apiserver admits it.
Envtest has no kubelet, so the mount itself stays an owner-observed fact.

**Owner check.** Platform owner confirms, without mounting anything against a real external server: whether worker node pools ship NFS or iSCSI client tooling; whether node egress and any CNI host-endpoint policy permit a node to reach a tenant-chosen address; whether a cluster admission policy already limits pod volume types in namespaces labelled actions-gateway.com/tenant=managed; and whether tenant roles allow direct pod creation there.

**Resolved in `v2.0.0`.** The first blocker is measured: against the envtest apiserver at 1.35.0, Pod Security Admission at `baseline` admits a pod with an `nfs` volume and one with an `iscsi` volume and forbids `hostPath`, and `restricted` forbids all three (`TestUpstream_PodSecurity_BaselineAdmitsNetworkVolumes`, which stays in the suite).
The namespaced-kind webhook now holds `podTemplate.spec.volumes` to the types the `restricted` level allows, by running the upstream `restrictedVolumes` check alone, so the list is `configMap`, `csi`, `downwardAPI`, `emptyDir`, `ephemeral`, `image`, `persistentVolumeClaim`, `projected` and `secret`, as Pod Security Standards 1.37 defines it; the evaluation is pinned to that version, so the list moves only when the pin is raised.
The GMC flag `--allow-unchecked-runner-template-pod-shape`, default off, turns this rule off together with the `baseline` one, and with it set and no platform policy in its place this finding is open again on every profile but `restricted`, the default `baseline` one included: a tenant can name an `nfs` or `iscsi` server and the node mounts it.
That is the larger of the opt-out's two costs, because it does not need a `privileged`-profile namespace.
`ClusterRunnerTemplate` is exempt, and a stored template passes while its `podTemplate` is unchanged.
The other two blockers are deployment facts and stay unobserved: whether a node could complete the mount, and whether a tenant who may create pods directly has the same path without a template.
The rule was added anyway because it costs a tenant only the move to a `PersistentVolumeClaim`, and no later minor can add it.

**Evidence.** `docs/design/05-security.md:120`, `docs/design/05-security.md:329`, `docs/design/05-security.md:752`, `cmd/agc/internal/provisioner/pod.go:314`, `docs/operations/admission-policies.md:14`.

### Template projected serviceAccountToken volume survives buildPod despite the documented 'no Kubernetes API credentials in the worker pod' floor invariant

`worker-pod-projected-sa-token-passthrough`: needs validation.
Starts at `api/v2/runnertemplate_types.go:38`.

buildPod enforces the credential invariant only through the automountServiceAccountToken boolean and the ServiceAccountName overwrite; it neither removes nor rejects projected serviceAccountToken volume sources carried by the tenant podTemplate, and no CEL rule or webhook inspects podTemplate.spec.volumes.

**Blocked on:**

- Source-only run with no sandbox: whether the apiserver admits and the kubelet mints a token for an explicit projected serviceAccountToken volume when automountServiceAccountToken is false is Kubernetes behaviour, not determined by this repository.
- The authority of the '<gateway>-worker' ServiceAccount token is a deployment fact: cluster bindings on system:serviceaccounts or system:authenticated, and any cloud workload-identity or Vault federation that trusts the cluster issuer for that account or a tenant-chosen audience.
- Tenant RBAC is a deployment fact: a tenant who may create pods or serviceaccounts/token in its namespace can obtain the same token without the template, which would make this same-principal.
- Reachability of the apiserver from a worker pod depends on a policy-enforcing CNI honouring the workload NetworkPolicy and on whether the apiserver endpoint is reachable through the tenant egress proxy.

**Local test.** Add a case to TestProvisioner_ReservedFieldsOverwritten (cmd/agc/internal/provisioner/provisioner_test.go:637) whose template carries a projected volume with a serviceAccountToken source mounted in the runner container, and assert the built pod has no serviceAccountToken projection; it is expected to fail today.
Then add an envtest to cmd/agc/internal/controller/integration/v2_runnerset_scaleset_provisioning_test.go (beside TestV2_RunnerSet_ScaleSet_TemplateEditTakesEffectWithoutRestart, which already drives a worker pod build from an edited RunnerTemplate) that provisions a worker from such a template and asserts the apiserver accepts the pod with automountServiceAccountToken false and the projection intact; the earlier envtest for worker-pod invariants, TestAGC_PodProvisioning_CorrectSpec, was deleted and survives only as a mention in docs/queue/Q1259.md.
Token minting needs a kubelet, so the final step is a kind e2e case, using a dummy gateway, that asserts the projected token file is present in the runner container.

**Owner check.** Platform owner checks, read-only: whether any RoleBinding, ClusterRoleBinding or workload-identity/Vault federation references the '<gateway>-worker' ServiceAccounts or the system:serviceaccounts groups; whether tenants hold pod create or serviceaccounts/token create in their namespace; and whether the apiserver endpoint is reachable from outside the cluster or through the tenant egress proxy.

**Resolved in `v2.0.0`, in two places.** The apiserver half of the first blocker is measured: the envtest apiserver at 1.35.0 stores a pod with `automountServiceAccountToken: false` and its own projected `serviceAccountToken` source as written (`TestUpstream_ProjectedServiceAccountTokenSurvivesAutomountFalse`).
Whether the kubelet then mints the token was not run; envtest has no kubelet, and the kind case sketched above was not written.
The webhook refuses the source on `RunnerTemplate` and on `ClusterRunnerTemplate`: the invariant is a floor on every profile, so platform authorship is no exemption, unlike the two pod-shape rules.
`buildPod` also removes the source from every worker pod, because the stored-value pass would otherwise leave a template from before the rule projecting a token indefinitely; the volume stays, so a pod mounting it still starts.
Review of the fix found a second projected source of the same kind, and it is closed with the token: a `podCertificate` source addressed to a signer under `kubernetes.io` is refused on both kinds and removed by `buildPod`.
`k8s.io/api` v0.37.2 documents `kubernetes.io/kube-apiserver-client-pod` as issuing client certificates the apiserver accepts and as unimplemented, so nothing was reachable; the refusal goes in now because only a major release can add one.
The whole reserved prefix is refused, not the one named signer, so a signer Kubernetes adds later starts refused and a minor release can admit it.
Left open on purpose: a `podCertificate` from any other signer (a workload certificate, an API credential only if the cluster administrator configures the apiserver to trust that CA), and a token a mutating admission webhook injects at pod creation (IRSA, EKS Pod Identity, Azure Workload Identity), which `buildPod` runs before and cannot see; the second is read from the order of operations and was not run on a cluster.
The remaining blockers (what the worker ServiceAccount's token can reach, tenant RBAC) are deployment facts and stay unobserved.

**Evidence.** `docs/design/05-security.md:907`, `docs/design/05-security.md:869`, `cmd/agc/internal/provisioner/pod.go:349`, `cmd/gmc/internal/controller/actionsgateway_v2_builder.go:137`, `cmd/gmc/internal/controller/shared_agc_deployment.go:219`.

## Confirmed low findings

Five confirmed findings whose fix needs no major, tracked as [Q1323](../queue/Q1323.md).

### Share and attribution projection names collide for (ns 'a-b', proxy 'c') and (ns 'a', proxy 'b-c')

`apinames/join-ambiguous-proxy-projection-name`: confirmed, low.
Starts at `cmd/gmc/internal/controller/egressproxy_sharing.go:73`.

The name is a '-'-joined concatenation of two fields that may each contain '-', with no escaping, length prefix or hash of the pair; the GMC apply path does not check that an existing object belongs to the same pool before replacing its data; and the AGC read path does not verify the proxy-namespace/proxy-name keys the projection carries.

**Established by source.** Derived by reading source, not by running: both inputs produce the string "proxy-share-a-b-c" (and "proxy-attribution-a-b-c"), the GMC apply path replaces the Data of an existing object of that name unconditionally, and the AGC consumes it without an identity check.
Which provider's data is present at a given moment depends on reconcile order, which was not observed; since one object cannot hold both pools' facts, at least one of the consumer's two references resolves to the other pool in every ordering.

**Fix.** Make the derived name injective: add a short hash of the pair joined with a separator that cannot appear in either field (for example ShortHash(namespace + "/" + name)) at the GMC share and attribution derivations and at both AGC derivation sites (runnerset_target.go:93 and :626), migrating existing objects by their labels.
Independently, have applyProxyShare and applyProjectedSecret refuse to overwrite an object whose proxy-namespace/proxy-name labels name a different pool, and have resolveSharedProxy reject a projection whose proxy-namespace/proxy-name data keys do not match the reference.
Add a table case to egressproxy_sharing_test.go asserting distinct names for ("a-b","c") versus ("a","b-c"), and update the name documented at docs/operations/security-operations.md:922.

**Evidence.** `cmd/agc/internal/controller/runnerset_target.go:93`, `cmd/gmc/internal/controller/egressproxy_attribution.go:730`, `cmd/gmc/internal/controller/egressproxy_sharing.go:67`, `cmd/gmc/internal/controller/egressproxy_sharing.go:293`, `cmd/gmc/internal/controller/egressproxy_sharing_test.go:77`, `api/v2/egressproxy_types.go:307`.

### ActionsGateway defaultProxyRef status tells a tenant whether a named EgressProxy exists in another namespace

`gmc-defaultproxyref-existence-oracle`: confirmed, low.
Starts at `api/v2/shared_types.go:202`.

ActionsGatewayV2Reconciler.Reconcile tests apierrors.IsNotFound on a cross-namespace EgressProxy read and returns ProxyNotFound before it evaluates provider consent, then reports an existing but ungranted proxy with a different reason and message.
Both outcomes are written by setNotReady to an object and an Event the unconsented referrer can read.

**Established by source.** Derived by reading, not by running: the first input sets Ready=False and Degraded=True with reason ProxyNotFound, the second sets them with reason ProxyShareNotGranted, both on the attacker's own ActionsGateway.
The reason alone tells the tenant that the named EgressProxy exists in the other namespace.
No other property of that proxy is exposed.

**Fix.** For a cross-namespace reference (proxyNS differs from the gateway's namespace), report ProxyShareNotGranted with one neutral message for both a missing proxy and an ungranted one, and log the real cause at the GMC.
Keep ProxyNotFound for same-namespace references.
Reword security-operations.md:939-940 so it says a consumer cannot tell the two apart for a cross-namespace reference and should ask the provider.
Add an envtest beside TestV2_ProxySharing_UngrantedCrossNamespaceRefIsDenied in cmd/gmc/internal/controller/integration/v2_egressproxy_sharing_test.go asserting that a missing cross-namespace proxy and an ungranted one yield identical reason and message.

**Evidence.** `cmd/gmc/internal/controller/actionsgateway_v2_controller.go:278`, `cmd/agc/internal/controller/runnerset_target.go:676`, `cmd/gmc/internal/webhook/v2/scaleset_scope.go:21`, `docs/design/05-security.md:669`, `docs/design/05-security.md:181`, `docs/operations/security-operations.md:939`, `cmd/gmc/internal/controller/integration/v2_egressproxy_sharing_test.go:69`, `cmd/gmc/internal/controller/integration/v2_actionsgateway_test.go:569`.

### An attributed namespace named 'ca' overwrites the attribution CA in the clients Secret and forces a bundle regeneration on every reconcile

`gmc/egressproxy-attribution/clients-secret-ca-key-collision`: confirmed, low.
Starts at `cmd/gmc/internal/controller/egressproxy_attribution.go:137`.

One flat Secret key space holds both a fixed key (ca.crt) and keys derived from tenant namespace names (<ns>.crt), with no prefix and no reserved-name check; the reader that recovers the namespace set special-cases the fixed key, so the collision is also invisible to the stability check.

**Established by source.** Derived by source reading, not by running: with consumers {ca, team-b} the clients Secret's ca.crt holds the 'ca' namespace's client leaf; the recovered namespace set is {team-b} against a desired {ca, team-b}, so a new bundle is generated on every reconcile; and each projected Secret's ca.crt is that client leaf, not the issuer of the read listener's certificate.
The resulting TLS verification failure in consumer AGCs and the pool rollout loop are the expected consequences and were not observed.

**Fix.** Separate the key spaces: store client pairs under a prefix a DNS label cannot collide with the fixed key (for example 'client.<ns>.crt' / 'client.<ns>.key', with attributionClientNamespaces and buildProxyAttributionSecret reading the same prefix), or take the projected CA from the server Secret / the returned caPEM instead of the clients map.
Add a unit test in cmd/gmc/internal/controller/egressproxy_attribution_test.go with a consumer named 'ca' asserting that the clients Secret's CA key equals bundle.caPEM, that attributionClientNamespaces round-trips the namespace set, and that a second ensureAttributionCerts call issues nothing.

**Evidence.** `cmd/gmc/internal/controller/shared_pki.go:41`, `cmd/gmc/internal/controller/egressproxy_attribution.go:549`, `cmd/gmc/internal/controller/egressproxy_attribution.go:298`, `cmd/gmc/internal/controller/egressproxy_controller.go:727`, `cmd/agc/internal/proxyattribution/poller.go:302`, `cmd/gmc/internal/webhook/validation/validation.go:21`.

### A direct-egress gateway's workload NetworkPolicy permits direct GitHub egress for proxied workers in the same namespace, while their status reports Proxied and attributed

`gmc/workload-netpol/shared-selector-unions-direct-github-egress-onto-proxied-workers`: confirmed, low.
Starts at `cmd/gmc/internal/controller/actionsgateway_v2_controller.go:429`.

buildWorkloadNetworkPolicyV2 attaches a per-gateway decision (direct adds the GitHub CIDR rule) to a namespace-wide podSelector (component=workload), and buildPod stamps no selectable gateway or egress-mode label on workers. setEgressMode and the gateway's updateStatus each derive Proxied/EgressUnattributed from that one object's own proxy reference and never consider another workload policy selecting the same pods.

**Established by source.** Established by reading source, not by running: the GMC emits, for a direct gateway, a workload NetworkPolicy whose podSelector matches proxied sets' worker pods and whose egress list allows TCP/443 to the GitHub CIDR set, while the proxied RunnerSet and any proxied sibling gateway report proxyMode Proxied with EgressUnattributed=False.
No admission check or other emitted policy narrows the selector.
Whether a given CNI then passes the packet was not observed; it follows from the additive NetworkPolicy semantics the product's own egress lockdown already depends on.

**Fix.** Scope the direct-egress allowance to the pods it is for: have the AGC stamp workers with a selectable egress-mode (or gateway) label, and move the GitHub CIDR rule into a policy that selects only direct-mode pods, leaving the shared default-deny policy with DNS and proxy peers.
If the shared posture is kept by design, report it instead: set EgressUnattributed=True (or a dedicated condition) on every proxied RunnerSet and gateway whose namespace holds a direct gateway, state the caveat in 05-security.md, network-architecture.md and tenant-onboarding.md, and correct troubleshooting.md:3936-3937.
Regression case in cmd/gmc/internal/controller: build the workload policies for a direct and a proxied gateway in one namespace and assert that no policy whose selector matches a proxied set's worker labels carries an ipBlock egress peer.

**Evidence.** `cmd/gmc/internal/controller/actionsgateway_v2_builder.go:92`, `cmd/gmc/internal/controller/actionsgateway_v2_builder.go:273`, `cmd/agc/internal/controller/runnerset_controller.go:637`, `cmd/gmc/internal/controller/actionsgateway_v2_controller.go:811`, `cmd/gmc/internal/controller/ipranges.go:565`, `docs/design/05-security.md:147`, `docs/operations/troubleshooting.md:3936`, `docs/plan/archive/q168-direct-egress.md:23`, `docs/design/network-architecture.md:96`, `cmd/gmc/internal/webhook/v2/actionsgateway_webhook.go:77`.

### A tenant can pre-claim scale-set names in a GitHub org it holds no credentials for, and another tenant's RunnerSet or gateway is then rejected at admission

`scalesetscope-unauthenticated-scope-claim`: confirmed, low.
Starts at `cmd/gmc/internal/webhook/v2/actionsgateway_webhook.go:155`.

scalesetscope.Of counts every stored (githubURL, runnerLabels[0]) pair as a first-come claim on the GitHub-side name, and nothing binds the claiming gateway to authority over the GitHub scope it names.

**Established by source.** Established by reading, not by running: validateScaleSetLabelUniqueness returns scaleSetConflictError for the tenant-a RunnerSet, so ValidateCreate denies it.
No code on the path reads credentials, a gateway condition or a namespace-to-org binding before counting the tenant-b claim.
The admission of the two tenant-b objects themselves follows from the same functions finding no prior claim on that label in that scope.

**Fix.** Bind a cross-namespace claim to authority over the scope before it can block another tenant: either count cross-namespace claims only from gateways whose AGC has authenticated to the scope (a status condition set after a successful installation-token exchange), or add a platform-owned allowlist of GitHub scopes per tenant namespace enforced in the ActionsGateway webhook.
Keep same-namespace and same-gateway collisions unconditional.
Whichever is chosen, state the remaining first-come behaviour between tenants that both hold the org in docs/design/05-security.md, and correct the label-usage wording at line 670.

**Evidence.** `cmd/gmc/internal/scalesetscope/scalesetscope.go:144`, `cmd/gmc/internal/webhook/validation/validation.go:69`, `cmd/gmc/internal/webhook/v2/actionsgateway_webhook.go:128`, `cmd/gmc/cmd/wiring.go:309`, `docs/design/05-security.md:669`, `docs/operations/troubleshooting.md:3657`.

## Egress proxy sharing leads

Four leads on the egress proxy and cross-namespace sharing, each confirmed by a local test and fixed under Q1324; the **Outcome** line under each says what was measured.
The owner checks below were not run, and none is needed to close the lead: each fix holds whatever tenant RBAC turns out to grant.
The first is the most consequential lead in the audit and was reported independently by five of the hunting passes.

### Provider-writable proxy TLS Secret is projected unvalidated into consumer namespaces and installed as an unconstrained trust root by consumer AGCs and workers

`gmc/egressproxy-share/unvalidated-ca-projection`: needs validation.
Starts at `cmd/gmc/internal/controller/egressproxy_controller.go:310`.

ensureProxyCert (egressproxy_controller.go:308-313) accepts an existing proxy TLS Secret on first-block parse and expiry alone, and reconcileProxyShares (egressproxy_sharing.go:249-254) distributes its tls.crt verbatim across the namespace boundary as trust material.
The consumers (trustpool.go:46, cmd/worker/main.go:814) install every certificate in those bytes as a general-purpose root rather than pinning one non-CA leaf to the proxy Service name.

**Blocked on:**

- Deployment fact: whether a provider-namespace tenant principal may update the GMC-owned '<ep>-proxy-tls' Secret.
  Tenant RBAC is not in source; the chart's tenant-resource-guard policy constrains the GMC's writes, not tenants', and no shipped policy protects this Secret.
- Deployment and CNI fact: converting the planted root into interception needs a provider-run pod carrying the pool identity label behind the Service (or a changed proxy image), admitted by pod RBAC, Pod Security admission and the proxy NetworkPolicy.
- Runtime behaviour not observed in a source-only run: the GMC reconcile leaving the replaced Secret in place and projecting it, and Go x509 plus the runner's OpenSSL/.NET stack accepting a chain to the appended CA for a GitHub host.
- Timing: the consumer AGC reads the proxy CA once at startup (cmd/agc/main.go:239), so the AGC half takes effect only on its next restart; whether anything rolls the AGC on a projection change was not traced.
  Worker pods mount the ConfigMap per job.

**Local test.** 1) In cmd/gmc/internal/controller/egressproxy_sharing_test.go add a test that seeds '<ep>-proxy-tls' with tls.crt = a serving leaf for the Service names followed by a locally generated IsCA certificate, runs ensureProxyCert then reconcileProxyShares against the fake client with one granted, referencing consumer namespace, and asserts on the projected ca.crt (expected today: equal to the seeded bytes, no ProxyCertificateIssued Event).
2\) In cmd/agc/internal/transport/trustpool_test.go add a test that passes those bytes to BuildTrustPool and verifies a leaf for api.github.com signed by the second certificate (expected today: verifies), with the control that the GMC-issued certificate from generateEgressProxyCert does not verify such a leaf.
3\) In cmd/worker/worker_test.go assert installCATrust writes both certificates into ca-bundle.crt.
Locally generated keys only; a fix is proven when (1) regenerates or refuses the Secret, or projects only the validated leaf, and (2) fails verification.

**Owner check.** On a non-production cluster with two tenant namespaces and a granted share, the owner, using the provider tenant's own RBAC identity, runs `kubectl auth can-i update secret/<ep>-proxy-tls -n <provider-ns>` and `kubectl auth can-i create pods -n <provider-ns>`; if the Secret update is allowed, replace tls.crt with leaf plus a test CA and read the consumer namespace's proxy-share-<provider-ns>-<name> ConfigMap and a new consumer worker pod's SSL_CERT_FILE bundle for the test CA.
Do not attempt interception.

**Outcome.** Confirmed and fixed.
A unit test seeding `tls.crt` with the GMC-issued leaf followed by a locally generated CA confirmed that the pair verifies a leaf for `api.github.com` signed by that CA.
The GMC now accepts only the shape `generateEgressProxyCert` issues (one non-CA leaf, exactly the Service's DNS names, paired with `tls.key`), re-issues anything else, and projects only that re-encoded leaf.
The projected bytes then fail to verify the `api.github.com` leaf (`TestReconcileProxyShares_PlantedCAIsNeverProjected`).
Whether tenants can write the Secret was not established, and the fix does not depend on it.

**Evidence.** `cmd/gmc/internal/controller/shared_pki.go:107`, `cmd/gmc/internal/controller/egressproxy_cert.go:50`, `charts/actions-gateway/files/agc-tenant-role-rules.yaml:61`, `cmd/gmc/internal/controller/egressproxy_builder.go:95`, `docs/design/05-security.md:184`, `docs/design/05-security.md:204`, `docs/operations/security-operations.md:922`.

### A tenant can stall another tenant's EgressProxy reconcile with a labelled ConfigMap and an unblankable Secret in its own namespace

`gmc/egressproxy-attribution/label-claimed-projection-wedges-reconcile`: needs validation.
Starts at `cmd/gmc/internal/controller/egressproxy_attribution.go:789`.

reconcileAttributionProjections attributes a ConfigMap to a pool from tenant-settable labels alone, across all namespaces, and treats a failure to blank the same-named Secret in that foreign namespace as fatal to the pool's whole reconcile, ahead of every child apply and the share prune.

**Blocked on:**

- Apiserver behaviour, not observed in this source-only run: the response to an Update that changes data on a Secret with immutable: true.
  Any rejection wedges the reconcile because the Update error is unfiltered, but that a rejection occurs on every attempt is a Kubernetes apiserver fact, not repository source.
- Deployment fact: the attacker's namespace must carry the tenant marker with the gmc-tenant-secret-reader RoleBinding in place; otherwise the Get is Forbidden, the blank is skipped, the ConfigMap is deleted and no stall forms.
- Deployment fact: tenant RBAC must allow creating ConfigMaps with arbitrary labels and Secrets with immutable: true in the tenant's own namespace.
- Runtime fact for the revocation claim only: whether the consumer-side egress NetworkPolicy loses its peer for the pool after revocation while the provider reconcile is stalled (ActionsGateway reconcile trigger on an EgressProxy spec change, the early return at actionsgateway_v2_controller.go:282 ahead of the rebuild at line 435, and the IP-range refresh rebuild at ipranges.go:561).
  If it does, the revoked consumer loses access despite the stale provider ingress peer.

**Local test.** Add an envtest case to cmd/gmc/internal/controller/integration/v2_egressproxy_attribution_rbac_test.go, beside TestV2_EgressProxy_Attribution_RunsUnderTheShippedRBAC so it runs under the shipped RBAC and admission policies.
Case 1 (stall): create tenant namespaces 'victim' and 'attacker'; in 'attacker' create ConfigMap 'x' labelled actions-gateway.com/proxy-attribution=true plus the two proxy-namespace and proxy-name labels for victim/pool, and Secret 'x' with a non-empty tls.key and immutable: true; create EgressProxy victim/pool with failureAttribution unset.
Assert the pool reports Degraded with the message naming step 'reconcile failure attribution' and that its NetworkPolicy and Deployment are never created; record the exact apiserver status code of the rejected Update.
Case 2 (revocation): start with a healthy pool sharing to 'attacker' with a RunnerSet and ActionsGateway referencing it, plant the two objects, remove 'attacker' from allowedNamespaces, and assert separately (a) whether the provider ingress peer and the share ConfigMap persist and (b) whether the consumer namespace's workload and AGC NetworkPolicies still carry the egress peer for the pool after the ActionsGateway reconciles.
Only (a) and (b) both persisting establishes retained access.
A unit variant in cmd/gmc/internal/controller/egressproxy_attribution_test.go can use an interceptor client returning apierrors.NewInvalid on the Secret Update to pin the error propagation without an apiserver.

**Owner check.** Owner-observed, non-destructive: on a non-production cluster with the chart installed, confirm whether tenant roles permit creating a Secret with immutable: true and a ConfigMap carrying the three projection labels, and whether a policy-enforcing CNI is in place so the stale ingress peer would matter.

**Outcome.** Confirmed and fixed.
Envtest on Kubernetes 1.35 returns `Invalid` for an Update that blanks an immutable Secret, and the unit variant shows such an error aborted the pool's reconcile.
A withdrawal now acts only on a ConfigMap at the projection's own name and leaves a Secret it cannot blank, so the planted pair no longer stalls the victim's pool (`TestV2_EgressProxy_Attribution_ForeignUnblankableSecretDoesNotStallThePool`, `TestEgressProxyReconcile_ForeignProjectionsDoNotStallThePool`).
The revocation half has no stall left to retain access through, so it was not tested separately.

**Evidence.** `cmd/gmc/internal/controller/egressproxy_attribution.go:787`, `cmd/gmc/internal/controller/egressproxy_attribution.go:842`, `cmd/gmc/internal/controller/egressproxy_controller.go:178`, `cmd/gmc/cmd/wiring.go:212`, `cmd/gmc/internal/controller/egressproxy_controller.go:27`, `cmd/gmc/internal/controller/tenant_secret_access.go:225`, `cmd/gmc/internal/controller/egressproxy_sharing.go:210`, `docs/design/05-security.md:191`.

### Bring-your-own-autoscaled EgressProxy lets the live Deployment replica count drive an unbounded repeated-addition loop on the GMC's single EgressProxy worker

`gmc/egressproxy-quota/byo-replicas-unbounded-mulquantity-loop`: needs validation.
Starts at `cmd/gmc/internal/controller/hpa_target_deployment.go:56`.

mulQuantity (cmd/gmc/internal/controller/shared_quota.go:73-79) is O(n) and its comment (lines 69-72) assumes n <= 100. evalEgressProxyQuota (cmd/gmc/internal/controller/egressproxy_quota.go:23-33) breaks the assumption for managedAutoscaling=false by substituting the live Deployment's spec.replicas, an int32 that assignExternallyScaledDeploymentSpec (hpa_target_deployment.go:53-59) leaves untouched whenever it is non-nil.

**Blocked on:**

- Tenant RBAC is a deployment fact not in source: the path needs a principal other than the platform operator who can write the <name>-proxy Deployment's spec.replicas (apps/deployments, deployments/scale) or create an HPA or KEDA ScaledObject targeting it.
  The architecture summary treats ConfigMap/Secret/pod write as normal for a tenant namespace admin and does not state Deployment or autoscaler write.
  If only the platform operator can scale the Deployment, the actor is trusted and this is hardening.
- Per-reconcile wall time is unmeasured (no sandbox on this host, nothing was run).
  The iteration count is exact from source; the duration depends on Quantity.Add cost on the int64 and inf.Dec paths and on CFS throttling under the deployed CPU limit, so whether other tenants' EgressProxy reconciles wait seconds or indefinitely is not established.
- Effects beyond the stalled egressproxy work queue (CPU contention with the ActionsGateway controller, webhook latency, leader-election renewal) depend on Go scheduling and the deployed CPU limit.
- Requires at least one ResourceQuota in the tenant namespace (shared_quota.go:137 skips the math otherwise); whether platform operators set one is a deployment fact.
- That the apiserver accepts spec.replicas up to the int32 maximum on a Deployment or scale write is third-party behaviour not shown in this repository's source.

**Local test.** In cmd/gmc/internal/controller/egressproxy_quota_test.go, beside TestEvalEgressProxyQuota_PressureFromHeadroom (the file has no bring-your-own case yet), add a test with a fake client holding one ResourceQuota, an EgressProxy with ManagedAutoscaling=false, and a Deployment with Status.Replicas=0 and Spec.Replicas stepped through 1e5, 1e6, 1e7; time evalEgressProxyQuota at each step under a short go test -timeout, confirm linear growth, and extrapolate to 2147483647 rather than running it.
Repeat with spec.resources.limits.memory large enough to overflow int64 early to time the inf.Dec path.
After a fix (clamp additional, or compute the product with MilliValue/inf.Dec multiplication), add a case in shared_quota_test.go beside TestFootprintFromResources asserting footprintFromResources returns promptly and correctly for replicas = math.MaxInt32.
An envtest case in cmd/gmc/internal/controller/integration can scale a bring-your-own proxy Deployment in a quota-bearing namespace to a large count and assert a new EgressProxy in a second namespace still reaches Ready in the normal window.

**Owner check.** Owner checks, read-only: for a representative tenant subject, `kubectl auth can-i patch deployments/scale -n <tenant-ns> --as <tenant-subject>`, the same for `update deployments` and `create horizontalpodautoscalers` (and KEDA scaledobjects where installed); whether marked tenant namespaces carry a ResourceQuota; the GMC container's CPU limit; and the controller_runtime_reconcile_time_seconds histogram for controller=egressproxy.

**Outcome.** Confirmed and fixed.
With the repeated addition restored, one `footprintFromResources` call at `math.MaxInt32` replicas did not finish within a 30-second test timeout on the development host.
`mulQuantity` now calls `Quantity.Mul`, which apimachinery provides and which stays exact past int64, so the same call returns immediately with the exact product (`TestFootprintFromResources_MaxInt32ReplicasIsExact`).
Tenant scale RBAC was not established, and the fix does not depend on it.

**Evidence.** `cmd/gmc/internal/controller/shared_quota.go:69`, `cmd/gmc/internal/controller/egressproxy_quota.go:24`, `api/v2/egressproxy_types.go:65`, `cmd/gmc/internal/controller/egressproxy_controller.go:725`, `cmd/gmc/internal/controller/egressproxy_controller.go:581`, `cmd/gmc/internal/controller/egressproxy_builder.go:147`, `vendor/k8s.io/apimachinery/pkg/api/resource/quantity.go:601`, `charts/actions-gateway/values.yaml:284`, `docs/operations/tenant-onboarding.md:279`.

### Transport-only proxy tunnels to its own loopback listeners, so a worker can replace its source address and arrival port with the proxy's

`cmd/proxy/proxy.go:checkDestination:transport-only-loopback-self-dial`: needs validation.
Starts at `cmd/proxy/proxy.go:830`.

checkDestination returns every authority as permitted when no allowlist is configured, and neither it nor handleConnect excludes loopback, unspecified or the pod's own addresses, while the proxy's two identity inputs (RemoteAddr and the local port) assume every accepted connection crossed the pod's ingress NetworkPolicy.

**Blocked on:**

- Source-only run with no sandbox: the nested CONNECT was not executed, so the loopback RemoteAddr on the inner request and the resulting audit and failure records are derived from reading net/http and kernel loopback behaviour, not observed.
- Whether the deployed CNI leaves pod-internal loopback traffic outside NetworkPolicy enforcement is a dataplane fact not visible in source (expected, since the traffic never leaves the pod network namespace).
- Whether the platform's audit pipeline already correlates the outer record (worker address to 127.0.0.1:<proxy port>) with the inner one, which would reduce the audit half to detectable evasion, is an operator fact outside the repository.

**Local test.** In cmd/proxy/audit_test.go, next to TestAudit_ConnectionsWithSourceRecordsClientAddress, add a test that starts a Server with AuditLogging set to AuditConnectionsWithSource and no allowlist, sends CONNECT <the server's own 127.0.0.1 listener address> to it, then sends a second CONNECT for a local echo listener through that tunnel, and asserts the audit record for the echo destination carries the test client's address; it is expected to carry 127.0.0.1 today.
In cmd/proxy/attribution_test.go, next to TestAttribution_RecordsByListenerAndServesOnlyTheCertsNamespace, add a test with two attributed namespaces that connects on namespace A's listener, tunnels to namespace B's listener address on loopback, issues a CONNECT to a closed port, and asserts namespace B's /v1/failures stays empty; it is expected to return one record with sourceIP 127.0.0.1.
Both use only loopback fixtures and the existing in-package helpers.

**Owner check.** The pool owner observes, for each EgressProxy: (1) whether spec.destinationFQDNs and spec.destinationCIDRs are both empty, i.e. the proxy container has no PROXY_ALLOWED_HOST_SUFFIXES or PROXY_ALLOWED_CIDRS env and is transport-only; (2) whether spec.auditLogging is ConnectionsWithSource or spec.failureAttribution is PerConsumerPort, the two features whose identity inputs this path affects; (3) whether the cluster's CNI or any additional policy layer applies NetworkPolicy to traffic a pod sends to its own loopback (expected not to); and (4) whether the retained 'egress audit' lines for the pool already contain records with sourceIP 127.0.0.1 or ::1, or with host 127.0.0.1 or localhost on the pool's CONNECT or attributed ports, and whether the log pipeline correlates such an outer record with the inner one.

**Outcome.** Confirmed and fixed.
A transport-only proxy returned 200 to CONNECTs naming its own CONNECT and health ports, reached as `127.0.0.1`, `localhost` and `0.0.0.0`.
The proxy now refuses any dial that lands on one of its own listening ports at a loopback, unspecified or local interface address, checked on the resolved address, and still tunnels to other loopback ports (`TestHandleConnect_RefusesTheProxysOwnListeners`).
The nested audit and failure-record tests the lead describes were not written, because the tunnel they depend on is now refused.

**Evidence.** `cmd/gmc/internal/controller/egressproxy_builder.go:187`, `cmd/proxy/main.go:96`, `cmd/proxy/proxy.go:420`, `cmd/proxy/proxy.go:760`, `cmd/proxy/audit.go:157`, `cmd/proxy/attribution.go:124`, `cmd/gmc/internal/controller/egressproxy_attribution.go:332`, `docs/design/05-security.md:401`.

## Scale-set identity leads

Four leads on which controller owns a GitHub scale set or runner record, tracked as [Q1325](../queue/Q1325.md).
The first two are the class Q1290 gated: one tenant's edit can cost another a job with no visible failure.
They are not gated here because both are unvalidated, they need two mutually untrusted tenants in one GitHub scope, and neither fix needs a major.

### Worker-pod reaper deletes whichever GitHub runner the pod's runner-name annotation names, with no ownership test

`agc/reaper/deregister-by-unverified-runner-name-annotation`: needs validation.
Starts at `cmd/agc/internal/provisioner/target.go:53`.

reapTarget.delete (cmd/agc/internal/controller/runner_shared.go:356) passes a mutable pod annotation, unvalidated, as the selector for a credentialed DELETE, and Client.DeregisterRunnerByName / resolveRunnerID (scaleset/client.go:563, 710) match that name across the whole org or repo listing with no scale-set, runner-group, minted-name-shape, online or claimed test.
The ownership test the sweep applies (mintedRunnerSuffix, cmd/agc/internal/scalesetlistener/listener.go:1057) is absent on this path.

**Blocked on:**

- Principal separation is a deployment fact.
  With credentials.type GitHubApp the key Secret sits in the tenant namespace, and with any credential type a principal who can create a pod running as the AGC ServiceAccount can obtain the same signing authority, so a namespace admin is same-principal for this delete.
  A boundary is crossed only where the pod writer lacks that authority, for example RBAC granting pods patch or update without Secret read or pod create, or a WorkloadIdentity signer that the writer cannot reach.
- Scope co-tenancy is a deployment fact: whether another tenant's scale sets or non-gateway self-hosted runners are registered in the same org or repo that this gateway's githubURL names, under an App installation the pod writer does not otherwise control.
- GitHub behaviour, not in source: whether GET /orgs/{org}/actions/runners?name= with this App's installation token returns runners of other scale sets and other runner groups (05-security.md:735 records this as unmeasured).
- GitHub behaviour, not in source: whether DELETE /actions/runners/{id} succeeds for an idle online runner or only for an offline one; the code relies solely on a 422 'running' answer for a busy runner.
- Source-only run with no sandbox: no test was executed, so the AGC issuing a DELETE for a foreign name is read from source, not observed.

**Local test.** In cmd/agc/internal/controller/runnerset_reap_deregister_test.go add a case beside TestRunnerSetReaper_DeregistersReapedWorkersRunnerRecord, using the same scalesettest server and newReapScaleSetClient: seed a runner record named 'other-set-runner' that this set did not mint (a name without the set's '<scale set>-<UUID>' shape), create a terminal pod labelled actions-gateway.com/runner-set=<set> carrying actions-gateway.com/runner-name: other-set-runner, with a second variant that has no owner reference and no acquisition-protocol label, run the reaper past completedPodTTL and assert the server's delete-runner call count (the stub exposes DeleteRunnerCalls in scaleset/scalesetstub/stub.go).
Current source predicts one DELETE for the foreign record; a fix that binds the name to the set's minted shape should issue none.

**Owner check.** Owner-observed on a disposable org with two RunnerSets and a ServiceAccount holding only pods/patch in the first set's namespace: register an idle persistent self-hosted runner and let the second set mint a not-yet-connected JIT runner, patch a first-set worker pod's actions-gateway.com/runner-name to each name in turn (back-dating actions-gateway.com/job-completed-at to trigger the reap), and record from the org runners page and the AGC log line 'reaper: deregistered worker runner record' whether each record was removed and whether the name filter returned a runner in a different runner group.

**Evidence.** `cmd/agc/internal/controller/runner_shared.go:356`, `cmd/agc/internal/controller/runner_shared.go:324`, `cmd/agc/internal/controller/runner_shared.go:484`, `cmd/agc/internal/controller/runnerset_gateway_teardown.go:62`, `scaleset/client.go:741`, `cmd/agc/internal/scalesetlistener/listener.go:1057`, `docs/design/05-security.md:737`, `cmd/agc/internal/provisioner/target.go:51`, `docs/operations/observability-metrics.md:229`.

### Editing runnerLabels[0] releases the scale-set name claim while the AGC keeps its live session on the old name

`agc/runnerset-scaleset/listener-name-not-reconciled-on-first-label-change`: needs validation.
Starts at `cmd/gmc/internal/webhook/v2/runnerset_webhook.go:80`.

ensureScaleSetListener keys the running listener on the RunnerSet's namespace/name and treats the runner group as the only spec property whose change requires a restart; the scale-set name captured from runnerLabels[0] at listener construction is never compared with the current spec, so the registration the AGC is driving and the claim the GMC inventory attributes to it diverge until the AGC process restarts.

**Blocked on:**

- Source-only run with no sandbox: the stale-listener state after a first-label update and the second set's admission were derived by reading and not observed in envtest or against the scalesettest fake.
- GitHub Actions Service behaviour is decisive for impact and outside source: whether a second CreateSession on one scale set is refused with 409 rather than replacing or splitting the session, and whether the stale session keeps being assigned jobs that the second tenant's repositories target at X (including after the second AGC PATCHes the set into another runner group).
- Deployment fact: whether two gateways bound to one GitHub scope are mutually untrusted tenants or shards of one tenant, and whether both hold App credentials for that scope.
  A tenant holding those credentials can already open a session on any scale set directly at GitHub, which limits the deliberate variant to same-authority; the unintended-collision path does not depend on that.
- Duration is bounded by an unobserved lifecycle fact: the stale binding ends at the next AGC process restart or any listener stop (start failure, unresolved reference), so how long the window stays open in a real deployment was not measured.

**Local test.** AGC half: in cmd/agc/internal/controller, beside runnerset_labels_q726_test.go, add a test using the scalesettest fake through ScaleSetClientFactory: reconcile a ScaleSet RunnerSet with runnerLabels ["x"] until a listener handle exists, update the spec to ["y"], reconcile again, and assert the handle's listener Status().ScaleSetID is still the id of the set named x and that the fake holds no scale set named y (passes today, demonstrating the stale binding; a fix makes it fail).
GMC half: in cmd/gmc/internal/controller/integration/v2_scaleset_name_collision_test.go add a case with two namespaces whose gateways share one githubURL: create set A with first label x, update A to y, create set B with first label x, and assert B is admitted and neither gateway reports ScaleSetNameCollision.
Combined: point a second reconciler for B at the same fake and assert its CreateSession returns SessionConflictError, B's status carries no condition naming it, and a job enqueued for x is provisioned through A's target.
The fake's 409 only mirrors the assumed GitHub behaviour, so this confirms the controller path, not the service's.

**Owner check.** On a non-production GitHub org with two gateways bound to it in separate namespaces, the operator renames a live ScaleSet RunnerSet's first label, applies a second RunnerSet with the old first label in the other namespace, queues one workflow job with runs-on set to the old label, and records which namespace the worker pod appears in, what the second RunnerSet's status and its AGC log show, and whether the ScaleSetNameCollision condition or gauge fires.
Then restart the first AGC and record that the session moves to the new name.
Nothing is deleted; revert by removing the second RunnerSet and the test scale sets at GitHub.

**Evidence.** `cmd/agc/internal/controller/runnerset_scaleset.go:609`, `cmd/agc/internal/controller/runnerset_scaleset.go:285`, `cmd/agc/internal/controller/runnerset_scaleset.go:309`, `api/v2/runnerset_types.go:85`, `cmd/gmc/internal/controller/actionsgateway_scaleset_collision.go:54`, `docs/design/05-security.md:672`, `docs/operations/upgrade.md:331`, `docs/operations/troubleshooting.md:2439`.

### Scale-set name uniqueness guard is check-then-act across two objects, so one tenant creating its RunnerSet and ActionsGateway concurrently may claim another tenant's scale-set name

`gmc-webhook-scaleset-scope:concurrent-pair-admission`: needs validation.
Starts at `cmd/gmc/internal/webhook/v2/runnerset_webhook.go:145`.

validateScaleSetLabelUniqueness and validateScaleSetLabelsVsScope each arbitrate a two-object invariant from an uncached List of already-persisted objects, with no reservation, lock or enforcing post-persist check.
The uncached reader removes informer staleness but not the gap between one webhook's read and the other object's write, so two admissions in flight together can each see the pair as incomplete.

**Blocked on:**

- Source-only run with no sandbox: whether two parallel CREATE requests both finish their webhook Lists before either object is persisted depends on apiserver admission timing and could not be exercised.
- Whether GitHub lets a second listener open a message session on a scale set that already has one, and so whether tenant B receives tenant A's jobs, is GitHub runtime behaviour not determinable from this repository; docs/design/05-security.md:658 asserts it but the repository tests it against a fake server only.
- Tenant B's gateway must authenticate against the same GitHub org, enterprise or repository as tenant A. Whether tenants share a scope, and whether a tenant holding a GitHub App key valid for that scope could adopt the same scale-set name directly at GitHub regardless of admission (which would make this same-principal), are deployment facts about App installation and permissions outside source.

**Local test.** In the envtest suite cmd/gmc/internal/controller/integration, beside v2_scaleset_name_collision_test.go and webhook_admission_test.go, add a test that stores gateway G_a (githubURL https://github.com/acme) and RunnerSet R_a (runnerLabels [shared]) in namespace a, then from two goroutines released by one channel creates gateway G_b (same githubURL) and RunnerSet R_b (runnerLabels [shared], gatewayRef G_b) in namespace b, deleting both and repeating up to a fixed bound whenever either is rejected.
Assert that no iteration ends with both stored.
Red on current code shows the race is reachable; the existing sequential cases in the same file are the control showing either order is rejected.
A never-red result within the bound does not refute the race, so also record how many iterations ran.

**Owner check.** On a cluster where two tenants register against one GitHub org, the owner reads the actions_gateway_scale_set_name_collision gauge and the ScaleSetNameCollision condition on every ActionsGateway, reviews the apiserver audit log for a RunnerSet and an ActionsGateway created in one namespace within the same second, and confirms whether each tenant's GitHub App installation can manage scale sets across the whole shared scope.

**Evidence.** `cmd/gmc/internal/scalesetscope/scalesetscope.go:88`, `cmd/gmc/internal/webhook/v2/actionsgateway_webhook.go:129`, `cmd/gmc/internal/controller/actionsgateway_scaleset_collision.go:29`, `docs/design/05-security.md:672`, `docs/design/05-security.md:210`, `charts/actions-gateway/templates/webhook.yaml:31`.

### AGC adopts the first scale set a server-side name filter returns without comparing its name, while admission compares first labels as exact case-sensitive strings

`scaleset-adopt-by-name-unverified-match`: needs validation.
Starts at `api/v2/runnerset_types.go:88`.

GetRunnerScaleSetByName trusts the server-side name filter (first result, no name equality check, no runnerGroupId, no Count > 1 handling), while the admission guard's notion of a distinct scale-set name is exact case-sensitive string equality; the two agree only if the Actions service filter is exact and case-sensitive.

**Blocked on:**

- Whether the Actions service's runnerscalesets?name= filter is exact and case-sensitive (versus case-insensitive, prefix or substring) is GitHub behaviour; no source file or committed capture in this repository records it.
- Whether that filter, sent without runnerGroupId, can return more than one set (same name in different runner groups) and in what order is GitHub behaviour not visible in source.
- Whether GitHub refuses to create a scale set named "CI" beside an existing "ci" in one scope is unmeasured; a refusal combined with an exact filter would make the case a self-inflicted start failure rather than a cross-tenant adoption.

**Local test.** In cmd/agc/internal/scalesetlistener/listener_test.go add TestEnsureScaleSet_RejectsNameMismatch: a fake client (or a scalesetstub knob) answers the by-name GET for "CI" with a set named "ci" in a different runner group; assert ensureScaleSet returns an error and issues no UpdateRunnerScaleSet and no session create.
Add TestEnsureScaleSet_RejectsMultipleMatches returning count 2.
Both are expected to fail on current code, which demonstrates the missing check but not the GitHub behaviour.
In scaleset/client_test.go add a GetRunnerScaleSetByName case where the server returns a differently named first value.
In cmd/gmc/internal/scalesetscope/scalesetscope_test.go add a CollidesWith case for labels differing only by case to pin whichever policy is chosen.

**Owner check.** Ask the owner of a GitHub scope used with the gateway (github.com, and a supported GitHub Enterprise Server version) to observe and record these runtime facts of the Actions service: whether the runnerscalesets name filter matches exactly and case-sensitively or also returns sets whose names differ by case, prefix or substring; whether scale-set names are unique per scope case-insensitively (a set named "CI" is refused beside an existing "ci"); and whether one name can exist in two runner groups of a scope, and if so what count and order the by-name lookup without a runner group returns.

**Evidence.** `scaleset/client.go:740`, `scaleset/client.go:466`, `cmd/agc/internal/scalesetlistener/listener.go:938`, `docs/design/05-security.md:711`, `scaleset/scalesetstub/stub.go:1210`.

## Release and CI leads

Seven leads on the release pipeline and CI, tracked as Q1326 and closed under it.
The first four turn on repository settings that source does not show: who may create a `v*` tag, who may dispatch `publish.yml` and `pages.yml`, and whether a deployment environment protects either.
Read on 2026-11-09, they are: the repository has one collaborator and the organisation one member, the same admin account, so for a person the four are same-principal today.
Nothing but that headcount holds them closed, since no ruleset targets tags and `publish.yml` runs under no deployment environment.
The 2026-11-11 re-read under Q1326 found the same, and the four are recorded as an accepted residual, with the two events that reopen them, in [05-security.md](../design/05-security.md#who-can-start-a-release-is-held-by-a-headcount-not-a-control).
The remedy, a `v*` tag ruleset and an environment with required reviewers on `publish.yml`, is a repository settings change and was not applied.
The other three each carry an **Outcome** line below.

### publish.yml dispatch `tag` input is never compared with the dispatch ref, so one tag's source can be published and signed under another version name while the stop-ship gates evaluate the named version

`publish-dispatch-tag-input-not-bound-to-ref`: needs validation.
Starts at `.github/workflows/publish.yml:38`.

The 'Resolve publish tag' steps validate the ref class (refs/tags/*) and the input prefix (v*) independently and never require the `tag` input to equal GITHUB_REF_NAME, and the announce-bar and validated-candidate gates evaluate the input name's commit rather than the checked-out commit that the publish jobs build.

**Blocked on:**

- Whether GHCR lets an existing `ghcr.io/actions-gateway/<image>:<version>` tag and the OCI chart version tags be moved by a later push is a registry and package-settings fact; publish.yml itself calls the version tag 'floating' (716) but source cannot establish registry behaviour.
- Who may run workflow_dispatch on publish.yml and who may create or move v* tags depends on repository roles and tag rulesets that are not in the repository.
  If every dispatcher can also create v* tags, the actor already holds an equivalent path (a new tag whose commit carries an edited publish.yml) and the finding reduces to a robustness and documentation gap; if tag creation is restricted to fewer principals than dispatch, the input is a gate bypass for the wider group.
- Whether the release steps stop the run for an already published name is a repository setting: release.md:855 says published releases are immutable and uploads fail HTTP 422.
  Those steps run in chart-publish after the images (job publish) and both charts (641-680) were already pushed and signed, so the setting decides only whether release assets are also replaced.
- Whether the publish.yml copies at old tags (v1.0.0 and v1.2.0 have neither gate job; v1.5.0 and v1.6.0 have one) still run through the push and sign steps on today's hosted runners is a runtime fact.
- Which existing stable names still have unexpired gate evidence (90-day retention) decides whether the validated-candidate gate passes for a stable input name when dispatching from a tag that carries the gate.

**Local test.** No sandbox was available in this run, so nothing was executed.
Add a shell test beside scripts/release/check-validated-candidate-test.sh (for example scripts/release/publish-tag-binding-test.sh) that extracts both 'Resolve publish tag' run blocks from .github/workflows/publish.yml and runs them with GITHUB_REF=refs/tags/v1.0.0-rc.1, GITHUB_REF_NAME=v1.0.0-rc.1, INPUT_TAG=v1.10.0 and a temporary GITHUB_OUTPUT.
Reading lines 237-249 predicts it writes tag=v1.10.0 and exits 0 today; a fix that requires INPUT_TAG to be empty or equal to GITHUB_REF_NAME makes it exit non-zero.
Add a case to scripts/release/check-validated-candidate-test.sh in which HEAD is not the named tag's commit and assert the gate refuses once it is bound to HEAD.

**Owner check.** A repository admin reads, without changing anything: the tag rulesets for v* (who may create, update or delete), which roles may dispatch Actions workflows, the GHCR package settings for the seven images and the charts package (whether an existing tag can be re-pushed), and whether immutable releases are enabled.
Then list publish.yml runs with event=workflow_dispatch (`gh run list --workflow=publish.yml --event=workflow_dispatch --json headBranch,displayTitle,createdAt`) and compare each run's head ref with the version it published.
Do not dispatch publish.yml against the real registry to test this.

**Evidence.** `.github/workflows/publish.yml:237`, `.github/workflows/publish.yml:245`, `.github/workflows/publish.yml:94`, `scripts/release/check-validated-candidate.sh:54`, `scripts/lib/common.sh:360`, `docs/operations/release.md:1485`, `docs/operations/release.md:728`, `.github/workflows/validate-candidate.yml:144`.

### Release verification identity pattern accepts a publish.yml signature from any v* tag for any version

`release-signing-identity-not-bound-to-version`: needs validation.
Starts at `scripts/release/verify-release.sh:32`.

release_identity_regexp (scripts/lib/common.sh:358-361) takes only the owner/repo slug and ends in `@refs/tags/v.*$`. verify-release.sh:49 computes it once with no VERSION argument and check() passes it to every cosign verify, verify-attestation and verify-blob call; validate-release.sh:1586 and 1631 and the operator docs use the same wildcard.
The verifier proves a signature came from publish.yml at some v* tag, and does not bind the signing ref to the version under verification.

**Blocked on:**

- Whether a principal with repository write and no merge right can create a new refs/tags/v* ref is a GitHub ruleset or tag-protection setting that is not in the repository.
- Whether a publish.yml run from a tag other than the release's own can push to the existing ghcr.io/actions-gateway/<image>:<released version> tag and the charts repository depends on GHCR package access and tag-immutability settings that are not in the repository.
- Whether Fulcio issues, and cosign accepts, a certificate for the scratch-tag run when verifying the moved tag is third-party behaviour; no sandbox exists on this host to run cosign against a fixture.
- No source-visible consumer follows a release image by mutable tag after the swap: the chart pins digests and the runbook tells operators to verify by digest, so reach to a real consumer (rule 2 of the supply-chain validation rules) is unproven.

**Local test.** Add cross-version cases to scripts/release/verify-release-test.sh next to line 57: with a version-aware helper, `expect_match "$(release_identity_regexp v1.2.3)" "${base}@refs/tags/v1.2.4" reject` and the same for `v1.2.3-rc.1`.
Against the current helper the equivalent identity is accepted, which the existing accept cases at lines 57-59 already show by inspection.
Extend the stub-cosign section of the same test to assert the --certificate-identity-regexp logged for `verify-release.sh v1.2.3` ends in `@refs/tags/v1\.2\.3$`.

**Owner check.** Owner-observed, read-only: (1) list the repository rulesets and report whether any targets refs/tags/v* with a creation restriction and who is on its bypass list; (2) report whether the GHCR packages gmc, agc, proxy, worker, wrapper, build-runner, cache-runner and charts/* have tag immutability or restrict Actions write access to specific workflows or refs; (3) confirm whether every published Release is immutable, so a released tag cannot be deleted and re-pushed.
If (1) restricts tag creation to maintainers, the finding reduces to a hardening note; if not and (2) permits a tag move, exact-tag binding in release_identity_regexp, validate-release.sh and the operator docs is the fix.

**Evidence.** `scripts/lib/common.sh:358`, `scripts/release/verify-release.sh:118`, `scripts/release/verify-release-test.sh:57`, `scripts/dogfood/validate-release.sh:1586`, `.github/workflows/publish.yml:237`, `.github/workflows/publish.yml:207`, `docs/operations/release.md:1485`, `docs/design/05-security.md:289`, `docs/operations/release.md:820`, `docs/operations/release.md:718`, `docs/operations/security-operations.md:1411`, `docs/operations/gitops.md:120`, `docs/operations/install.md:218`.

### Historical publish.yml copies at 28 v* tags have no validated-candidate gate and accept a free-form version name, under a signing identity verify-release accepts for any version

`publish-historical-tag-copies-bypass-release-gates`: needs validation.
Starts at `.github/workflows/publish.yml:36`.

Release authorization lives only in the workflow file at the dispatched ref, and nothing outside that file binds which revision of publish.yml may publish a given version: the publishing jobs declare no `environment`, and the verifier's identity regexp accepts a certificate from any v* tag ref for any version.
Every historical tag is therefore a standing release entry point with weaker gates that hardening on main cannot reach.

**Blocked on:**

- Decisive and not in source: whether any principal can dispatch workflows on a v* tag ref without also being able to create v* tags.
  If every dispatch-capable actor may push a v* tag, they can tag a commit with an edited publish.yml and obtain the same `@refs/tags/v*` signature with no gate, so this is same-principal and should be rejected.
  Needs the repository's tag rulesets and role assignments.
- That GitHub accepts workflow_dispatch of publish.yml on a historical tag ref and executes that ref's copy, and that the resulting Fulcio certificate identity is `publish.yml@refs/tags/<dispatched tag>`, are platform behaviours not observable in the repository.
- Whether an old tree still builds, pushes and signs end to end today (base-image digests, tool downloads pinned in the old copies, GHCR accepting the push, package write permission on the token) is a runtime fact.
- How `gh release create <name> --draft` followed by `gh release edit --draft=false --latest` behaves when the name is not an existing git tag, and how the repository's immutable-release setting and any tag ruleset treat the implicit tag creation, are GitHub-side facts.
- Pages variant: whether the github-pages environment restricts which refs may deploy is a repository setting not in source.

**Local test.** Read-only, publishes nothing.
(1) Add a case to scripts/release/verify-release-test.sh asserting that the identity pattern derived for VERSION=v1.10.0 rejects `https://github.com/actions-gateway/github-actions-gateway/.github/workflows/publish.yml@refs/tags/v1.6.0`; it fails today because release_identity_regexp ends in `@refs/tags/v.*$` and takes no version.
(2) Add a test beside scripts/ci/workflow-acting-steps-test.sh that, for each `git tag -l 'v*'`, reads `git show <tag>:.github/workflows/publish.yml` and asserts the publish job's `needs` includes `validated-candidate`; expected today: 28 tags fail.

**Owner check.** Owner-observed configuration only, nothing dispatched or published: read the repository's rulesets (`gh api repos/actions-gateway/github-actions-gateway/rulesets` and each ruleset's detail), its environments (`gh api repos/actions-gateway/github-actions-gateway/environments`) and its role assignments, to establish whether v* tag creation is restricted to a narrower set of principals than workflow dispatch, and whether any rule or environment limits publish.yml or github-pages deployments by ref.
If tag creation is not narrower than dispatch, close as same-principal.
If it is narrower, the candidate stands as a gate bypass for the dispatch-only principals and the remaining platform-behaviour blockers are for the owner to settle.

**Evidence.** `.github/workflows/publish.yml:156`, `.github/workflows/publish.yml:106`, `scripts/release/check-validated-candidate.sh:54`, `scripts/release/verify-release.sh:103`, `docs/operations/release.md:1485`, `docs/operations/release.md:1436`, `.github/workflows/pages.yml:36`.

### pages.yml publish job deploys dispatch-chosen content from any ref and serves the whole gh-pages branch

`pages-publish-accepts-unreviewed-refs`: needs validation.
Starts at `.github/workflows/pages.yml:45`.

The publish job's condition and its resolve and source-selection steps trust dispatch inputs for the published version, alias, default and content ref without restricting docs_ref to release tags or release-X.Y branches, and the Pages artifact is assembled from the mutable gh-pages branch rather than from this run's build output; the only controls that could limit either (environment deployment policy, a gh-pages ruleset) live outside the repository.

**Blocked on:**

- Whether any principal holds repository write (needed for workflow_dispatch and for a push to gh-pages) without also being able to land the same content on main is a repository role and ruleset fact not in source.
  If every write-holder is a maintainer, or main's ruleset requires status checks but no independent review, the actor already holds the authority and this is same-principal.
- The github-pages environment's deployment branch and tag policy and required reviewers are not in the repository.
  If it admits only main and v* tags, a dispatch from a scratch branch (which could also carry an edited pages.yml) is refused at the environment; a dispatch on main with docs_ref set to an unreviewed ref is not affected by that policy.
- Whether a ruleset restricts pushes to gh-pages is not in the repository; without one, a write-holder's direct push to gh-pages is published by the next push to main with no dispatch.
- Whether mkdocs 1.6.1 with Material 9.8.0 lets a mkdocs.yml taken from docs_ref do more than change content (plugins, markdown_extensions options, theme settings, YAML python tags) inside a job holding contents: write is third-party runtime behaviour and was not executed; no sandbox exists on this host.

**Local test.** In scripts/ci/workflow-acting-steps-test.sh, which already drives pages.yml's mike step (case pages-dispatch-alias), add cases that extract and drive the 'Resolve version, alias, and default' and 'Select the docs source' run bodies with stubs on PATH: (1) EVENT=workflow_dispatch, REF=refs/heads/scratch, IN_VERSION=9.9.9, IN_ALIAS=stable, IN_DEFAULT=true and a temporary GITHUB_OUTPUT, asserting what is written (today: deploy=true, alias=stable, is_default=true); (2) DOCS_REF=refs/pull/1/head with a stub git, asserting whether `git fetch` and `git checkout ... -- docs mkdocs.yml` are called (today: both).
A guard that requires docs_ref to match a stable v* tag or release-X.Y branch, and REF to be refs/heads/main or a stable refs/tags/v* when version is set, turns both cases red.
Run only where the repository's test sandbox is available.

**Owner check.** A repository admin reads, without changing anything: the github-pages environment's deployment branches and tags setting and required reviewers; the rulesets covering gh-pages and whether main's ruleset requires an approving review; the list of collaborators, teams and apps with write or higher; and `gh run list --workflow pages.yml --event workflow_dispatch` to see whether any run used a ref other than main or a release tag, or a docs_ref other than a tag or release-X.Y branch.

**Evidence.** `.github/workflows/pages.yml:189`, `.github/workflows/pages.yml:193`, `.github/workflows/pages.yml:441`, `.github/workflows/pages.yml:69`, `docs/development/website.md:216`, `requirements-docs.txt:4`.

### Dependabot sync and rebase writers compile and run a freshly bumped build tool in a job holding a contents:write token

`ci/dependabot-sync-runs-unreviewed-tools-with-contents-write`: needs validation.
Starts at `.github/dependabot.yml:17`.

The job that needs the write token to commit and push the regenerated tree is the same job that builds and executes third-party tool code selected by the unreviewed bump; execution and write authority are not separated, and the checkout leaves the token in git config for every later step.

**Blocked on:**

- Source-only run with no sandbox: the tool build and execution under `make deps-sync` after a tools bump was traced from the Makefiles and scripts, not observed.
- Whether GitHub grants the requested contents: write scope to a Dependabot-triggered pull_request run is platform behaviour; the workflow header says the push succeeds, which implies it does, but it was not observed in this run.
- What the token can reach beyond the bot's branch depends on branch and tag rulesets, release immutability and whether GITHUB_TOKEN-pushed refs start any workflow here, none of which are in source.
- The precondition is a malicious or compromised release of a module in the tools graph; the Go checksum database fixes a version's content but not its intent, and no such release is in evidence.

**Local test.** In a sandboxed copy of the repository with a file-based GOPROXY and no network, publish a fixture version of one module that controller-gen links from tools/go.sum whose package init writes a marker file recording whether `git config --get-regexp extraheader` returns a value.
Bump tools/go.mod to it and run `make deps-sync`.
The marker appearing shows unreviewed tool code runs on this path with the checkout credential readable.
Then add a case beside the replay cases in scripts/ci/dependabot-rebase-stale-test.sh asserting that the replay's `make deps-sync` runs with GH_TOKEN unset and no credential in git config.

**Owner check.** Owner opens a past dependabot-go-sync run for a PR that changed tools/go.mod and reads two things from the log: the `GITHUB_TOKEN Permissions` block in Set up job shows Contents: write, and the `make deps-sync` step shows controller-gen being built from tools/vendor.
Owner then reads the repository rulesets and release settings to state whether github-actions[bot] may create tags, push branches other than main, and edit published releases or their assets.

**Outcome.** Confirmed from a run log; the dispatch path is refuted and the tag path is an accepted residual.
The Dependabot-triggered sync run `42702512168` (2026-11-09) logged `Contents: write` and `Metadata: read` as its whole token, `persist-credentials: true` at checkout, and controller-gen built from `tools/vendor` and run under `make deps-sync`, all in the job that pushes.
Neither writer holds `actions: write`, so neither token can dispatch `publish.yml`.
Neither ruleset targets tags (read 2026-11-11), so as far as the rulesets go either token can create a `v*` tag; whether that tag would start `publish.yml` was not tested, because the test is the write.
The bound and the control, the same `v*` tag ruleset the first four need, are in [05-security.md](../design/05-security.md#who-can-start-a-release-is-held-by-a-headcount-not-a-control); moving tool execution out of the writing job is [Q1341](../queue/Q1341.md).
The sandboxed fixture-module test above was not run: the run log already shows tool code executing beside the credential, which is what it would establish.

**Evidence.** `.github/workflows/dependabot-go-sync.yml:63`, `.github/workflows/dependabot-rebase-stale.yml:84`, `scripts/ci/dependabot-rebase-stale.sh:473`, `Makefile:670`, `.github/workflows/dependabot-go-sync.yml:11`, `docs/development/go-workspaces.md:259`.

### No source-visible control stops a fork pull request from naming a dogfood scale-set label in runs-on

`ci/pr-editable-runs-on-reaches-dogfood-scaleset`: needs validation.
Starts at `.github/workflows/conflict-markers.yml:12`.

Runner selection is by label alone, the dogfood scale sets are registered repo-scoped against a public repository so no runner group gates them, and on pull_request events the label is text in a workflow file the pull request can edit.
The opt-in gate is an expression inside those same editable files, and until Q1330 the AGC did not distinguish a fork pull request's job from any other; it now refuses one on every `RunnerSet` that does not set `spec.forkPullRequests: Accept`.

**Blocked on:**

- The current value of the repository's fork pull request approval setting is GitHub-side state.
  Source holds one dated reading (first_time_contributors, docs/queue/Q1052.md:54); whether it still holds decides if an external contributor's edited workflow runs without a maintainer click.
- Whether GitHub assigns a fork pull_request job to a repo-scoped runner scale set on a public repository is platform behaviour.
  The repository's notes assume it does (docs/plan/archive/q468-jobcompleted-retention.md:68) but no measurement of a fork job landing on a dogfood scale set is in source.
- The dogfood tenants are on-demand (scripts/dogfood/start.sh, e2e-start.sh).
  Reachability needs a tenant to be up, or a queued job to still be waiting when one comes up.
  Source records that a job queued against a not-yet-registered label waits server-side and is assigned when the scale set appears (docs/plan/archive/q468-jobcompleted-retention.md:69); how long GitHub holds it was not established here.
- Which e2e overlay is live during a window (Kata by default, privileged dind by opt-in) and what a worker pod can reach inside the dogfood cluster are deployment facts that set the impact; neither is determined by source.

**Local test.** No local test can settle this: the decisive step is GitHub's job assignment.
The source-side guard Q1330 built is tested locally in cmd/agc/internal/provisioner/fork_refusal_internal_test.go: the refusal compares the run object's head repository name with the run's own repository for a pull_request event (the job message has no head-repository field, decided under Q1220).

**Owner check.** Owner reads the repository's current fork pull request approval policy (Settings > Actions > General, or the actions/permissions/fork-pr-contributor-approval setting) and records its value and the date.
Owner lists the runner scale sets registered for the repository and confirms whether each dogfood label (gag-ci-scaleset, gag-ci-e2e, the probe labels) is repo-scoped with no runner-group restriction.
Owner checks the repository's existing Actions run history and the dogfood AGC's job records for any pull_request-event job from a fork head repository that was assigned to a dogfood scale set, and records which e2e overlay (Kata or dind) is applied during dogfood windows and for how long tenants stay up.

**Outcome.** Closed by Q1330, which refuses fork jobs by default on every `RunnerSet` unless it sets `spec.forkPullRequests: Accept`; it binds the dogfood sets once the dogfood cluster runs a release carrying it.
GitHub's fork approval policy read `first_time_contributors` on 2026-11-11, the same value Q1052 recorded, so until that deploy that setting is what holds the lead, as [05-security.md](../design/05-security.md#who-can-start-a-release-is-held-by-a-headcount-not-a-control) records.

**Evidence.** `scripts/dogfood/start.sh:3`, `.github/workflows/unit-test.yml:262`, `.github/actionlint.yaml:15`, `docs/plan/archive/q468-jobcompleted-retention.md:66`, `docs/queue/Q1052.md:54`, `docs/operations/personas.md:93`, `deploy/dogfood-e2e/README.md:37`, `scripts/dogfood/e2e-start.sh:7`, `docs/development/testing.md:2224`.

### go-throttle hook returns `allow` for single `go build`/`go test` commands whose flags or GOFLAGS run a caller-named program or write to a caller-named path

`gothrottle-auto-allow-go-exec-flags`: needs validation.
Starts at `.claude/settings.json:13`.

Decide (decide.go:119-139) allows whenever simple(f) holds and no wrapper was peeled. simple (decide.go:277-298) rejects only multiple statements, redirects, background, negation, a non-CallExpr, and CmdSubst/ProcSubst nodes. goInvocation (decide.go:169-188) matches on the first two literal words and keeps the remaining words only for a `-race` substring test.
A CallExpr's leading assignments and go's own program-executing or path-writing flags (-exec, -toolexec, -o, -C, GOFLAGS) are never examined, so they inherit the allow.

**Blocked on:**

- Whether Claude Code skips its permission prompt when a PreToolUse hook returns permissionDecision allow with updatedInput, in the default permission mode and in auto mode, is third-party runtime behaviour not visible in this repository and not executable in this source-only run.
- How a hook allow composes with an ask or deny from the contributor's other installed hooks (workspace-guard, branch-guard, prod-guard), which are user-installed plugins and not part of this repository, is undocumented, as main.go:27 notes.
- Whether a given contributor already allowlists Bash(go build *) / Bash(go test *) in personal or local settings, in which case the hook grants nothing beyond what that contributor already granted, is a per-machine fact.
- The mvdan.cc/sh parse of the example commands (single CallExpr, flags as plain words, GOFLAGS as a CallExpr assignment, no CmdSubst) was derived by reading decide.go and the pinned GOFLAGS test case, not by running the parser.

**Local test.** In devtools/agent/gothrottle/decide_test.go add cases to the Decide table (non-empty testPrefix): `go test -exec /bin/echo ./api/...`, `go build -toolexec /bin/echo ./cmd/proxy`, `GOFLAGS=-toolexec=/bin/echo go build ./cmd/proxy`, and `go build -o /tmp/outside ./cmd/proxy`, each expecting a decision other than allow.
Run the gothrottle package tests in devtools/ with GOWORK=off in a sandbox.
By source reading all four currently return allow, so the new cases fail until Decide rejects those flags and non-allowlisted leading assignments.
Mirror one case in scripts/agent/claude-go-throttle-hook-test.sh.

**Owner check.** Owner-observed, non-destructive: on a contributor machine with no Bash(go ...) permission rule in any settings scope and Claude Code in its default permission mode, open this repo and have the session issue `go test -exec 'sh -c "touch tmp/gothrottle-marker"' -run XXX ./api/...`.
Record whether a permission prompt appears before tmp/gothrottle-marker is created.
Repeat with the go-throttle entry removed from .claude/settings.json to confirm the hook is the difference, and once more with workspace-guard installed using `go build -o` to a path outside the worktree to observe how the allow composes with that guard.

**Outcome.** Confirmed and fixed.
Against the real toolchain (go1.27.1, a scratch module), `go test -exec PROG`, `go build -toolexec PROG` and `GOFLAGS=-toolexec=PROG go build` each ran the named program, and `go build -o PATH` and `go test -cpuprofile PATH` wrote outside the module; `-vettool` is no longer a `go test` flag there.
The real hook returned `allow` for all of them, and for `go test $FLAGS` and `CC=PROG go build` too.
The comment justifying the allow cited a repository `Bash(go test *)` allowlist that no committed settings file carries, so the allow was a grant of its own.
The auto-allow now also requires literal words with no shell escape, neither a backslash nor ANSI-C quoting (review found `go test \-exec PROG` still allowed, and it ran PROG), no package path that leaves the working directory, flags from `safeFlags`, and only `GOFLAGS` (checked the same way), `GOWORK=off` or `CGO_ENABLED` as leading assignments; the same commands now get no decision, or an ask when they carry `-race`.
Twenty-five new `TestDecide` cases go red with the checks removed.
The owner check above was not run, because the fix holds whatever Claude Code does with an allow.

**Evidence.** `devtools/agent/gothrottle/decide_test.go:48`, `devtools/agent/gothrottle/main.go:19`, `.claude/settings.json:6`, `devtools/agent/gothrottle/decide.go:273`, `devtools/agent/gothrottle/main.go:27`, `scripts/agent/local-throttle.sh:297`, `docs/development/testing.md:277`.

## AGC run and recovery leads

Five leads on state the Actions Gateway Controller (AGC) acts on, tracked as [Q1327](../queue/Q1327.md).

### AGC re-runs and force-cancels whatever run its namespace ConfigMaps and pod annotations name; docs/design/05-security.md line 200 describes only the pod annotation and says a run ID cannot be substituted

`agc-recovery-state-unauthenticated-run-identity`: needs validation.
Starts at `cmd/agc/internal/controller/scaleset_guards.go:84`.

Recovery state that selects the target of installation-token write calls is stored as plain ConfigMap data and pod annotations in the tenant namespace and is trusted on read as if only the controller wrote it.
Validation is syntactic only (repoSegmentRE, PathEscape, non-empty run ID); no stored run identity is tied to an assignment the scale-set session delivered or to the RunnerSet's GitHub scope, and the retry budget and recovery claims that cap the calls are stored beside it under the same write access.

**Blocked on:**

- Deployment fact not in source: whether any principal can update ConfigMaps or patch pods in a tenant namespace without also being able to read the GitHub App key Secret, create or exec a pod under the AGC ServiceAccount, or otherwise mint installation tokens (for example a ConfigMap-only role, a deploy bot, or a worker ServiceAccount bound to ConfigMap write, combined with a Vault-transit signer).
  Where every such writer is the tenant namespace admin, this is a same-principal action and only the documentation is incomplete.
- No sandbox on this host, so nothing was executed.
  The orphan path needs an AGC process start and passes runtime checks (live worker listing, claims read, GitHub run read, futility checks) that were traced by reading only.
- GitHub's response to rerun-failed-jobs and force-cancel for a run the scale set never served is third-party behaviour, and the installation token's repository reach depends on how the App is installed.
- Maintainer intent: whether line 200 of docs/design/05-security.md is a guarantee against writers of namespace state or a statement about the controller's own behaviour under a compromised-AGC threat, which is the abuse case the row names.

**Local test.** Add TestRecoverOrphaned_ForgedInFlightIdentity to cmd/agc/internal/provisioner/orphaned_scaleset_internal_test.go: with the fake client and the existing httptest GitHub stub, call RecoverOrphanedScaleSetWorkers with an OrphanedWorker whose JobID was never provisioned and whose Owner/Repository/RunID name an unrelated repository, the stub answering the run read status=completed, conclusion=failure, and assert whether a POST to /repos/<other>/<repo>/actions/runs/<id>/rerun-failed-jobs is recorded.
Add a sibling in pending_rerun_internal_test.go: write a failures ledger whose pending entry names an unrelated repository with a large failedAttempt and whose reruns record already lists the disruption key, call AdoptPendingReruns then sweepPendingReruns, and assert the rerun POST and that Count did not rise.
Add one in eviction_scaleset_internal_test.go that sets run-id and repository annotations on a never-started worker pod, marks it externally deleted, and asserts the force-cancel POST.
For the envtest tier, extend cmd/agc/internal/controller/integration/q844_orphaned_worker_test.go by editing the scaleset-guards ConfigMap between two manager generations.

**Owner check.** Owner-observed, no probing: for each tenant namespace, list the subjects holding update or patch on configmaps and patch on pods (review the RoleBindings, or kubectl auth can-i --list per bound subject) and compare with the subjects that can get the App key Secret, create or exec pods, or assume the AGC ServiceAccount.
A subject in the first set and not the second can drive rerun and force-cancel across the installation.
Also confirm which repositories each tenant's App installation covers, and state whether line 200 is meant to hold against namespace writers.

**Evidence.** `docs/design/05-security.md:200`, `cmd/agc/internal/provisioner/pending_rerun.go:285`, `cmd/agc/internal/provisioner/pending_rerun.go:132`, `cmd/agc/internal/provisioner/eviction_scaleset.go:436`, `cmd/agc/internal/provisioner/abandoned_scaleset.go:218`, `cmd/agc/internal/provisioner/abandoned.go:138`, `cmd/agc/internal/provisioner/rerun_ledger.go:400`, `cmd/agc/internal/provisioner/rerun_futility.go:45`, `cmd/agc/internal/provisioner/naming.go:12`, `cmd/agc/internal/scalesetlistener/listener.go:2323`.

### One repository's OOM-killed jobs can fill the per-set failure ledger and withhold automatic re-runs for every repository on the RunnerSet

`agc/rerun-ledger/unevictable-per-job-records-fill-shared-cap`: needs validation.
Starts at `scaleset/types.go:217`.

updateRerunLedger enforces one aggregate byte cap over four maps but evicts only the failures map, while memoryFloors, pending and the per-attempt jobs/disruptions lists accept one entry per workflow-controlled (run, job display name) with no per-run, per-repository or per-entry length bound, so the shared cap is reachable from one repository's jobs and its failure mode is refusal for every other writer on the set.

**Blocked on:**

- Nothing was built or run (no sandbox on this host): the bytes each OOM-killed job adds to the ledger, and so the number of jobs needed to reach 512 KiB, are estimated from struct tags and not measured.
- Whether workflow code can reliably make the kubelet report the runner container itself as OOMKilled (rather than only a child process dying and the step failing) depends on cgroup version and memory.oom.group behaviour on the node, outside the repository.
- GitHub-side limits are not visible in source: the maximum job display name length delivered in a scale-set message, the maximum jobs per run, and run-creation rate limits set how few runs are needed and whether the 10-runs-per-10-minutes breaker threshold constrains the fill rate in practice.
- The path needs spec.sizing.profile Binpack or Throughput with usage history present (the pod must carry the sizing-profile annotation), and a RunnerSet that serves more than one repository; both are deployment choices.
  The set's runner capacity bounds how fast pods can be killed.
- Whether loss of automatic re-run for other repositories in the same tenant, recoverable by manual re-run or by deleting the ConfigMap, is inside the product's accepted fail-closed residual (docs/operations/upgrade.md:770-771) is an owner decision; the doc's capacity figure does not account for floors, pending entries or multi-job attempts.
- The alternative route through the eviction arm (a job forcing node-pressure eviction of its own pod, one 30-day reruns entry per run) depends on the tenant pod template and kubelet ranking and was not traced to a reliable trigger.

**Local test.** In cmd/agc/internal/provisioner/rerun_ledger_internal_test.go add a test beside TestRerunLedger_CapRefusesRatherThanEvicts using ledgerFixture and ledgerProvisioner: for a handful of run IDs call recordMemoryFloor and then reserveScaleSetRerun with a non-nil park for 256 distinct job names of 100 bytes and distinct 63-byte pod names, as recoverOOMKilled and handleScaleSetEviction would, and count jobs accepted before errRerunLedgerFull.
Then call reserveScaleSetRerun for a fresh run ID with a short job name and cause eviction and assert whether it returns errRerunLedgerFull.
Extend TestRerunLedger_CapHoldsEnoughBudgets with a case that includes floors, pending entries and multi-job attempts so the documented capacity reflects them.
A fix is demonstrated when the second repository's reservation still succeeds, for example with a per-run cap on floors and merged jobs/disruptions, a length cap on stored display names, and size-driven eviction of floors and pending entries ahead of refusing a budget write.

**Owner check.** On a non-production cluster with a RunnerSet on the Throughput or Binpack profile that has usage history, the owner runs one matrix workflow whose jobs allocate past the runner memory limit, confirms the pods report the runner container as OOMKilled, reads kubectl get configmap scaleset-failures-<set> -o jsonpath='{.data.failures\.json}' to measure bytes added per killed job and the longest display name stored, and checks whether actions_gateway_eviction_rerun_failures_total{reason="budget_unrecorded"} is reachable before the breaker pauses the class.

**Evidence.** `cmd/agc/internal/provisioner/rerun_ledger.go:525`, `cmd/agc/internal/provisioner/flaky_jobs.go:344`, `cmd/agc/internal/provisioner/eviction.go:268`, `cmd/agc/internal/provisioner/oom_class.go:378`, `cmd/agc/internal/provisioner/oom_class.go:134`, `cmd/agc/internal/provisioner/oom_class.go:197`, `cmd/agc/internal/provisioner/pending_rerun.go:349`, `cmd/agc/internal/controller/runnerset_sizing_profile.go:120`, `cmd/agc/internal/provisioner/rerun_ledger_internal_test.go:523`, `docs/operations/upgrade.md:770`, `docs/design/04-operational-flows.md:617`.

### Throughput sizing profile derives the worker memory limit from job-controlled peak usage with no maxRequests clamp, so the limit can exceed the operator's envelope and may ratchet

`agc/sizing-profile/throughput-memory-limit-unclamped-ratchet`: needs validation.
Starts at `cmd/agc/internal/usage/sampler.go:229`.

applySizingProfile's Throughput arm computes memLimit as scaleQuantity(rec.ObservedPeak[memory], headroom), floors it at the clamped request, and never calls clampQuantity on it, while its input is an undecayed, status-persisted all-time maximum of usage the job itself controls.

**Blocked on:**

- Nothing was executed (no sandbox on this host).
  The single-step result, a derived limit above maxRequests.memory, is determined by source; the multi-generation ratchet is not.
- Whether a job can hold memory close enough to its limit to be sampled there without being OOM-killed depends on kubelet cgroup accounting and metrics-server's roughly 15 s resolution, neither in this repository.
  The operator page itself notes a job killed at its limit is sampled at or below it.
- Shared impact depends on deployment facts not in source: whether nodes are shared across tenants (memory overcommit, since requests stay small while limits grow), and whether the namespace has a LimitRange max or a limits.memory ResourceQuota (growth then turns into admission rejection of every worker pod of the set until the profile is left or status is reset).
- Whether workflow authors are a different principal from the tenant admin who enabled the profile is a deployment fact.
  The tenant admin can already set any static memory limit in the RunnerTemplate, so with one principal there is no boundary crossed.
- Maintainer intent is not settled by source: the plan document records the unclamped limit as current behaviour without calling it a defect, while the operator page and CRD description read as promising a bound.

**Local test.** In cmd/agc/internal/controller/runnerset_sizing_profile_test.go add TestApplySizingProfileThroughputMemoryLimitHonoursMaxRequests beside TestApplySizingProfileClamps: call applySizingProfile with WorkerSizing{Profile: Throughput, MaxRequests: {memory: 1Gi}} and confidentRec("runner") (ObservedPeak memory 1536Mi) and assert the runner container's memory limit is at most 1Gi; current source returns 2304Mi.
In the same file add a ratchet test (applySizingProfile is unexported in package controller, so it cannot live in package usage): start from confidentRec("runner"), and for five generations set rec.ObservedPeak[memory] to the previous generation's derived memory limit (the most a job could be sampled at), call applySizingProfile again, and assert the derived limit never exceeds MaxRequests.memory instead of growing by the headroom factor each generation.
In cmd/agc/internal/usage/sampler_internal_test.go, using the existing fakeLister, add a test that records 20 ordinary pods, then one pod with a much larger memory reading, then 20 more ordinary pods, and reads SizingStatus to pin whether ObservedPeak[memory] ever comes back down (current source: it does not).

**Owner check.** The owner of a deployment using profile Throughput reports, without running any new workload: for an existing Throughput RunnerSet, the values of spec.sizing.maxRequests.memory, status.sizingRecommendation[].observedPeak.memory and the memory limit on its recent worker pods, and whether that limit exceeds maxRequests.memory or has grown over time; whether the tenant namespace has a LimitRange max or a limits.memory ResourceQuota; whether worker nodes are shared with other tenants; and whether the people who can author workflows on the tenant's repositories are a different principal from the tenant admin who sets spec.sizing.

**Evidence.** `cmd/agc/internal/controller/runnerset_sizing_profile.go:94`, `cmd/agc/internal/controller/runnerset_sizing_profile.go:111`, `cmd/agc/internal/controller/runnerset_sizing_profile_test.go:137`, `docs/operations/worker-rightsizing.md:134`, `api/v2/runnerset_types.go:390`, `docs/plan/q555-automatic-recovery-design.md:326`, `docs/plan/runner-sizing-profiles.md:439`, `api/v2/runnerset_types.go:380`.

### Tenant-chosen workerImage host (and registry redirects) steer AGC GETs to in-cluster 443/6443 endpoints off the egress proxy, with the outcome reported in RunnerSet status

`runnerimage-tenant-steered-agc-fetch`: needs validation.
Starts at `api/v2/runnertemplate_types.go:43`.

The registry client accepts any host from the tenant-authored image reference and follows redirects with net/http defaults, while running in a pod whose egress policy (any destination on 443/6443) and NO_PROXY list were sized for the AGC's fixed apiserver and proxy destinations; nothing confines the registry read to non-cluster-internal hosts or to the proxied path.

**Blocked on:**

- Source-only run: redirect following by the resolver's client and the exact error text that reaches the condition (dial error, x509 hostname-mismatch text with certificate names, HTTP status) are net/http and crypto/x509 behaviour not determined by repository source; no fixture was executed.
- Reachability depends on a policy-enforcing CNI applying the AGC and workload NetworkPolicies as written, and on which in-cluster or node-network endpoints listen on post-DNAT 443/6443 and admit ingress from the tenant namespace; neither is in source.
- Impact depends on whether any reachable endpoint discloses data or performs a side effect on an unauthenticated GET to /v2/<repo>/manifests|blobs/<ref> (direct naming) or to an arbitrary path (redirect variant); the AGC attaches no ServiceAccount token or GitHub credential, so the apiserver answers as anonymous.
- The redirect variant needs a tenant-controlled first hop the AGC can reach: direct-egress mode, a tenant noProxyCIDRs entry, or a platform-allowlisted proxy destination.
  Which of these a given install permits is a deployment fact.
- Whether the RunnerTemplate author already holds equivalent network reach (for example tenant RBAC that allows creating pods, or relabelling a worker pod out of the workload NetworkPolicy's selector) is a deployment RBAC fact; where it does, this is same-principal and not a finding.
- Whether the platform owner treats tenant-directed AGC requests to cluster-internal 443/6443 endpoints as inside the accepted any-destination residual is a design decision the docs do not settle.

**Local test.** In cmd/agc/internal/runnerimage/inspect_test.go add TestInspectFollowsRedirectOffRegistry: two httptest TLS servers under one test CA; server A answers GET /v2/x/y/manifests/latest with 302 Location: <server B>/internal; call Inspect with image '<A host:port>/x/y' and an http.Client built as setupRunnerImageResolver builds it (cloned transport, no CheckRedirect) trusting the test CA; assert whether B recorded a request and capture the returned error string.
Add a case with image '127.0.0.1:<port>/x/y' against a server whose certificate lacks that IP SAN and record whether the error lists the certificate's names.
In reference_test.go assert ParseReference's Registry for 'kubernetes.default.svc/x/y' and 'foo.ns.svc.cluster.local/x/y'.
In cmd/gmc/internal/controller add a unit test feeding buildNoProxy's output to golang.org/x/net/http/httpproxy.Config.ProxyFunc and asserting those hosts resolve to no proxy.

**Owner check.** On a non-production cluster with a policy-enforcing CNI and the default install, the platform owner creates a RunnerTemplate in a test tenant with workerImage 'kubernetes.default.svc/x/y:1' and reads the RunnerSet's RunnerVersionTooOld message: an HTTP status or x509 text (rather than a proxy CONNECT denial or dial timeout) shows the AGC dialled the apiserver directly on the tenant's instruction.
Repeat with a dummy Service on 443 in another namespace to see whether its ingress policy or the AGC policy stops it, and confirm from tenant RBAC that the template author cannot otherwise place an unrestricted pod in the namespace.

**Evidence.** `cmd/agc/main.go:804`, `cmd/agc/internal/runnerimage/resolver.go:208`, `cmd/agc/internal/runnercore/runnerversion.go:133`, `cmd/agc/internal/runnerimage/client.go:121`, `cmd/gmc/internal/controller/actionsgateway_v2_builder.go:279`, `cmd/agc/main.go:549`, `docs/design/05-security.md:345`, `docs/design/05-security.md:298`.

### AGC run-status, re-run and force-cancel errors carry the raw upstream body into logs, and run-status and re-run errors into a tenant-namespace Event, bypassing SanitizeBody

`agc-rest-error-body-unsanitized`: needs validation.
Starts at `cmd/agc/internal/provisioner/eviction.go:795`.

The provisioner's REST helpers sit outside the shared redactor: they format strings.TrimSpace(string(body)) directly into the error, and recordRerunFailure forwards the error text into an Event note.
The sibling scale-set client routes the same kind of body through githubapp.SanitizeBody with a 512-byte cap.

**Blocked on:**

- No source-visible response on these three endpoints carries a credential: the installation token is sent only in the request Authorization header.
  A leak needs an upstream (a GHES instance, or an intermediary in front of it) whose non-2xx body echoes the Authorization header or other secret material, which is a deployment fact outside source.
- Whether a lower-trust reader exists is a deployment fact: the Event lands in the tenant's own namespace and the body comes from the GitHub host that tenant configured, so a boundary is crossed only where Events or AGC logs are readable by a principal who may not read the tenant's credentials (for example a cluster event or log pipeline).
- Whether the apiserver rejects or the recorder drops an Event whose note holds a large body was not measured; nothing was run.

**Local test.** In cmd/agc/internal/provisioner/eviction_internal_test.go add TestRerunFailure_UpstreamBodyIsSanitized: serve a 500 from httptest whose body holds a ghs_-shaped dummy token and 100 KiB of padding, drive the eviction recovery path through runConclusion, and assert the dummy token and the padding are absent from both the returned error and the note passed to the fake Target.RecordEvent.
Repeat for rerunFailedJobs (a 403 without 'already running') and, in cmd/agc/internal/provisioner/abandoned_rerun_internal_test.go, for forceCancelRun's returned error.
Expected today by source reading: the token and padding are present.

**Owner check.** Owner check: on a GHES or proxied deployment, capture one non-2xx response body from /actions/runs/{id}, /rerun-failed-jobs and /force-cancel and confirm whether it ever reflects request headers or tokens; and confirm who can read Events in tenant namespaces and the AGC's logs.

**Evidence.** `docs/design/05-security.md:199`, `cmd/agc/internal/provisioner/abandoned.go:162`, `cmd/agc/internal/provisioner/eviction.go:860`, `scaleset/client.go:413`, `cmd/agc/internal/controller/runnerset_controller.go:538`, `cmd/agc/internal/controller/runnerset_target.go:152`.

## Worker pod and NetworkPolicy leads

Five leads on worker pods and the policies around them, tracked as [Q1328](../queue/Q1328.md).
The last is already open as [Q1253](../queue/Q1253.md), which holds a dogfood measurement this audit did not have; its entry stays here so the audit's record is whole, and Q1253 owns the work.

### Gateway teardown deletes the workload NetworkPolicy while worker pods running job code can still be alive

`gmc/actionsgateway-v2/reconcileDelete/workload-netpol-removed-under-live-workers`: needs validation.
Starts at `cmd/gmc/internal/controller/actionsgateway_v2_controller.go:216`.

reconcileDelete gates teardown on RunnerSet status counts (activeJobs + pendingJobs), not on the absence of pods labelled actions-gateway/component=workload, and then deletes the workload NetworkPolicy unconditionally, including on the WorkerDrainTimeout branch where it has just recorded that worker pods are being left running.
The provisioning direction states the ordering rule (policy before pods, egressproxy_controller.go:182); teardown has no inverse.

**Blocked on:**

- Source-only run with no sandbox: whether a still-running worker pod gains connectivity after the policy is deleted depends on a policy-enforcing CNI and how quickly it reprograms on policy deletion; neither is in source.
- Whether the platform applies its own default-deny or baseline NetworkPolicy to tenant namespaces is a deployment fact; where one exists the pod stays isolated and the impact is absent.
- The healthy-path window depends on runtime behaviour: how long job processes survive after the runner container receives SIGTERM, and the terminationGracePeriodSeconds of the tenant template (no cap found in cmd/agc/internal/provisioner, but admission-side caps were not exhaustively excluded).
- The WorkerDrainTimeout path needs the AGC to be unable to reap during a gateway delete; whether a tenant admin can induce that (scale or break the AGC Deployment) depends on deployment RBAC.
- If tenant RBAC lets a tenant admin write NetworkPolicies or run unlabelled pods in its own namespace, the admin is not confined by this policy at all and only third-party workflow job code is an affected principal; that RBAC is a deployment fact.

**Local test.** Unit: in cmd/gmc/internal/controller/actionsgateway_v2_worker_drain_test.go add a case beside TestReconcileDeleteV2_WorkerDrainTimesOut that seeds the <gateway>-workload NetworkPolicy and a Running pod labelled actions-gateway/component=workload owned by a bound RunnerSet, calls reconcileDelete past the 90 s deadline, and asserts the policy still exists (expected to fail today).
Add a second case where the RunnerSet reports 0 active / 0 pending but the labelled pod carries a deletion timestamp and a finalizer.
Envtest: extend TestV2_Teardown_FailClosedUntilChildrenGone in cmd/gmc/internal/controller/integration/v2_teardown_test.go with a finalizer-held worker pod and assert the gateway finalizer and workload policy are retained until the pod is gone.
Enforcement needs the kind + Calico e2e lane: run a job that sleeps and ignores SIGTERM, scale the AGC to zero, delete the gateway, and after WorkerDrainTimeout probe from the worker pod to an address outside the proxy and GitHub ranges, and to the worker pod from another namespace.

**Owner check.** Owner-observed, non-destructive: in a tenant namespace with one gateway, list NetworkPolicies and confirm whether any platform-owned policy selects pods labelled actions-gateway/component=workload besides <gateway>-workload; confirm whether tenant admins can write NetworkPolicies or scale the AGC Deployment; for any past gateway delete that emitted WorkerDrainTimeout, check from events and pod timestamps whether pods with that label were still Running after the policy was gone.

**Evidence.** `cmd/gmc/internal/controller/actionsgateway_v2_builder.go:298`, `cmd/agc/internal/provisioner/pod.go:314`, `cmd/agc/internal/provisioner/pod.go:347`, `cmd/gmc/internal/controller/actionsgateway_v2_controller.go:1259`, `cmd/agc/internal/controller/runner_shared.go:402`, `cmd/agc/internal/controller/runnerset_controller.go:373`, `cmd/gmc/internal/controller/egressproxy_controller.go:182`, `docs/design/05-security.md:120`, `docs/design/04-operational-flows.md:69`, `docs/operations/troubleshooting.md:1785`.

### Unmarking a tenant namespace revokes only the one canonical reader RoleBinding, so other GMC-written bindings keep granting Secret access there

`gmc/tenant-secret-access/unmark-revokes-only-the-named-reader-binding`: needs validation.
Starts at `charts/actions-gateway/templates/tenant-resource-guard.yaml:112`.

Revocation on unmark is keyed to one object name and exact shape (deleteReaderRoleBinding) and is executed by the principal being revoked, while the grant path it is meant to undo (GMC-authored RoleBindings to its two bind-able ClusterRoles in a marked namespace) is unconstrained in name, roleRef and subject by the tenant-resource-guard.
Nothing enumerates or removes other RoleBindings that reference gmc-tenant-secret-reader or agc-tenant-role when the marker goes, and the guard does not restrict which RoleBindings the GMC may write inside a tenant namespace.

**Blocked on:**

- Source-only run with no sandbox or envtest assets: the end result rests on kube-apiserver behaviour that was read, not observed, namely the ValidatingAdmissionPolicy admitting a second RoleBinding from the GMC ServiceAccount in a marked namespace, RBAC escalation prevention accepting it on the bind grant, and that binding still authorizing Secret reads after the label is removed.
- Design intent is not settled by the docs: whether a namespace that was once a tenant falls inside the 'unmarked namespaces' bound of docs/design/05-security.md:54 and docs/operations/upgrade.md:595, given that the revocation is carried out by the GMC itself and so already assumes a GMC that was honest up to the unmark.
  If that is an accepted residual, this is a documentation gap (the Residuals list omits it) and not a control failure.
  The maintainer decides.
- The scenario needs the GMC ServiceAccount's credentials to be misused while the namespace is marked, and the operator to unmark the namespace while leaving it and its Secrets in place instead of deleting it.
  Both are deployment facts.

**Local test.** In cmd/gmc/internal/controller/integration/tenant_secret_access_test.go add TestGMC_TenantSecretAccess_UnmarkRevokesEveryGMCBinding beside TestGMC_TenantSecretAccess_DeletesReaderBindingOutsideTenants.
With installTenantResourceGuard and installGMCShippedRBAC, create a marked namespace holding one dummy Secret.
Using the impersonating GMC ServiceAccount client that the neighbouring tests use, create RoleBinding 'extra-reader' (roleRef ClusterRole gmc-tenant-secret-reader, subject the GMC ServiceAccount) and RoleBinding 'extra-agc' (roleRef ClusterRole agc-tenant-role, subject the GMC ServiceAccount) and require both creates to succeed.
Start the GMC with startGMCAsServiceAccount, remove the marker with the admin client, wait for the canonical binding to be NotFound, then assert the GMC client's Get of the dummy Secret is Forbidden.
The expectation from source is that the Get succeeds and both extra bindings still exist, so the test goes red.
Stop at that one Get.

**Owner check.** Owner-observed, read-only: for each namespace without actions-gateway.com/tenant=managed, list RoleBindings whose roleRef is ClusterRole gmc-tenant-secret-reader or agc-tenant-role (kubectl get rolebindings -A -o json, filtered on roleRef.name and joined to namespace labels) and confirm that none exist beyond AGC bindings awaiting a documented blocked teardown; and run kubectl auth can-i get secrets --as=system:serviceaccount:<gmc-namespace>:<gmc-sa> -n <ex-tenant-namespace>, expecting 'no'.

**Evidence.** `charts/actions-gateway/templates/tenant-resource-guard.yaml:10`, `docs/design/05-security.md:54`, `docs/operations/upgrade.md:595`, `charts/actions-gateway/templates/tenant-resource-guard.yaml:98`, `charts/actions-gateway/files/agc-tenant-role-rules.yaml:61`, `cmd/gmc/internal/controller/integration/tenant_secret_access_test.go:217`, `docs/operations/tenant-onboarding.md:25`.

### namespace-psa-guard admits the GMC ServiceAccount setting Pod Security enforce=privileged on a marked tenant namespace that lacks the platform eligibility label

`gmc-namespace-psa-guard:privileged-stamp-without-eligibility`: needs validation.
Starts at `charts/actions-gateway/files/manager-role-rules.yaml:16`.

namespace-psa-guard has no validation tying the pod-security.kubernetes.io/* values the GMC writes to the namespace's own security-profile and privileged-profile labels, although both are on the same object, are immutable to the GMC under the guard's own label rule, and are the only inputs the reconciler derives the stamped value from.

**Blocked on:**

- Source-only run: whether Pod Security Admission then admits a privileged or hostPath pod created by the ReplicaSet controller for a GMC-authored Deployment, and whether such a pod reaches anything outside the tenant namespace, depends on the apiserver, the container runtime and node sharing, and was not observed.
  The admission of the namespace UPDATE itself is determined by the policy text and is not a blocker.
- The precondition is control of the GMC ServiceAccount identity outside its reconciler.
  No tenant-reachable route to the write exists in source: the reconciler stamps only the value derived from the security-profile label, which namespace-security-profile-guard gates on eligibility.
- The missing privileged ban is documented as deliberate at docs/design/05-security.md:805.
  Whether this is a finding or an accepted residual turns on whether the confinement claims at lines 54 and 806 hold once a non-eligible tenant namespace can be made privileged, which needs the runtime observation above and then an owner triage decision.
- Deployment facts not in source: whether tenant worker nodes are shared with platform or other-tenant workloads, and whether any additional pod-spec policy engine sits behind Pod Security Admission.

**Local test.** Step 1 (envtest, apiserver only): in cmd/gmc/internal/controller/integration/namespace_psa_guard_test.go add a case beside TestGMC_NamespacePSAGuard_EnforcesMarkerAndFieldScope that, as the impersonated GMC ServiceAccount, patches pod-security.kubernetes.io/enforce=privileged on a marked namespace carrying security-profile=restricted and no actions-gateway.com/privileged-profile label, and records whether it is admitted (source reading predicts admitted).
Step 2 (needs controllers, so a kind cluster via make e2e tooling rather than envtest): with the chart's policies installed, as the GMC ServiceAccount create a Deployment in that namespace whose pod template sets securityContext.privileged: true and a hostPath volume, and record whether the ReplicaSet's pod is created or rejected by Pod Security Admission.
Step 3: after adding a namespace-psa-guard validation that each written enforce/warn/audit value equals the profile derived from oldObject's security-profile label (baseline when absent) and that privileged additionally requires privileged-profile=allowed, the step 1 patch must be denied while the existing reconciler-stamp cases still pass; mirror the change in cmd/gmc/config/admission-policy/namespace-psa-guard.yaml and the chart template.

**Owner check.** Read-only on an installed cluster: list tenant namespaces whose pod-security.kubernetes.io/enforce is privileged without actions-gateway.com/privileged-profile=allowed (kubectl get namespaces -l 'actions-gateway.com/tenant=managed,pod-security.kubernetes.io/enforce=privileged,actions-gateway.com/privileged-profile!=allowed'), and establish whether tenant worker nodes are shared with platform or other-tenant workloads and whether a second pod-spec policy layer exists.

**Evidence.** `charts/actions-gateway/templates/namespace-security-profile-guard.yaml:61`, `cmd/gmc/internal/controller/namespace_psa_controller.go:83`, `docs/design/05-security.md:805`, `docs/design/05-security.md:783`.

### DNS egress rule confines which resolver a pod may contact, not which names it resolves; the design states the DNS exfiltration channel is closed

`netpol-dns-recursion-exfil-claim`: needs validation.
Starts at `cmd/gmc/internal/controller/actionsgateway_v2_builder.go:281`.

The control restricts the DNS server a pod may contact but not the names it may resolve, while the documented security property is stated in terms of data leaving by DNS queries to an attacker-controlled authoritative server and of DNS staying on the attributable egress path.

**Blocked on:**

- Whether cluster DNS forwards arbitrary external names queried by a worker pod (CoreDNS forward/stub/ACL configuration) is a deployment fact about a third-party component; the design asserts recursion for the proxy's lookups but source cannot show the Corefile or that workers are not filtered.
- Which source address the recursed query leaves from (node, NAT gateway, provider resolver) and whether the operator attributes it to a tenant is a deployment fact.
- Whether a recursive resolver answers UDP/TCP 53 on a 169.254.0.0/16 address reachable from a pod is a cloud-provider fact not in source.
- A policy-enforcing CNI is required to distinguish this from the unenforced case; no sandbox or cluster is available in this source-only run, so nothing was executed.

**Local test.** On the Calico e2e lane in cmd/gmc/test/e2e/direct_egress_test.go (Describe E2E_V2_DirectEgress, beside E2E_V2_DirectEgress_NonGitHubBlocked) add a case that deploys an in-cluster authoritative DNS fixture with a query log, configures the kind cluster's CoreDNS to forward a test zone to it, resolves a unique random label under that zone from a workload pod, and asserts whether the label appears in the fixture's log.
The documented 'exfiltration channel stays closed' predicts it does not appear; appearance validates the candidate.
Use only dummy data and the in-cluster fixture.

**Owner check.** Owner inspects the cluster DNS Corefile (forward, stub domains, any acl/view/policy plugin scoped to tenant pods) and the source address upstream DNS queries carry, and states whether, on the cloud in use, a recursive resolver answers port 53 on a link-local address from a pod network namespace.
No live probing by the auditor.
If recursion is confirmed, either reword 05-security.md:310-319, network-architecture.md:368/375 and the shared_networkpolicy.go comment to claim only removal of direct any-resolver access and record recursion through cluster DNS as an accepted residual, or add a resolver-side name policy.

**Evidence.** `docs/design/05-security.md:310`, `docs/design/05-security.md:319`, `docs/design/05-security.md:201`, `cmd/gmc/internal/controller/shared_networkpolicy.go:112`, `docs/design/network-architecture.md:368`, `cmd/gmc/internal/controller/shared_networkpolicy_test.go:78`.

### GitHub CIDR allowlist admits /meta `actions` ranges that serve Azure blob storage, so direct-mode worker 443 egress may not be GitHub-only

`ipranges-meta-actions-shared-cloud-ranges`: needs validation.
Starts at `cmd/gmc/internal/controller/ipranges.go:196`.

The allowlist treats every range under the /meta `actions` key (and the hostname suffix blob.core.windows.net) as a GitHub-only destination, but those are Azure Storage addresses and names that GitHub uses rather than ones only GitHub answers on, while the security design describes the resulting egress as GitHub-only.

**Blocked on:**

- Whether an Azure storage account (or any other 443 endpoint) not operated by GitHub is served from an address inside a range GitHub publishes under /meta `actions` is a GitHub/Azure fact that cannot be read from this repository; the repo's own Q1253 records it as unmeasured, and this run has no network access.
- The effect requires a policy-enforcing CNI; that is a deployment fact, and on a non-enforcing CNI all egress is open regardless of this rule.

**Local test.** No local fixture can decide the external range fact; the local step only pins the source half.
In cmd/gmc/internal/controller/shared_networkpolicy_test.go add TestBuildWorkloadNetworkPolicyV2_DirectAdmitsEveryFetchedCIDR beside TestBuildWorkloadNetworkPolicy_NoGitHubEgressWhenProxied: call buildWorkloadNetworkPolicyV2 with direct=true and a fixed slice holding one CIDR per /meta key (the fixture shape of TestHTTPFetcher_MergesAllRanges in ipranges_test.go) and assert each appears as an ipBlock peer on port 443 with no per-key filtering.

**Owner check.** Owner settles the fact Q1253 leaves open: take an Azure storage account GitHub does not operate, in a region the published `actions` ranges cover, and record whether its blob endpoint address falls inside the current /meta `actions` listing that the GMC rendered into the direct-mode workload NetworkPolicy (and the CIDR-mode EgressProxy policy) on a cluster whose CNI enforces NetworkPolicy.
Record the answer as a threat-table residual in docs/design/05-security.md or narrow the admitted set.

**Evidence.** `docs/plan/q215-worker-cache.md:190`, `docs/queue/Q1253.md:20`, `docs/design/05-security.md:120`, `cmd/gmc/internal/controller/ipranges.go:165`, `cmd/gmc/internal/controller/egressproxy_fqdn.go:123`, `cmd/gmc/internal/controller/egressproxy_builder.go:187`, `cmd/proxy/proxy.go:751`, `docs/design/network-architecture.md:60`, `cmd/gmc/internal/controller/ipranges_test.go:297`.

## Rejected candidates

Three candidates were rejected, and are listed so a later audit does not re-raise them without new evidence:

- An unbounded `result` label on the scale-set listener's job-completed metric.
  The label is unbounded, but only the tenant can point its own controller at a host that would send arbitrary values, and the series land in that tenant's own memory-limited pod.
- Worker egress lockdown keyed on mutable pod labels.
  A documented residual.
- The worker wrapper passing the just-in-time runner config on a command line.
  Visible only to the principal that already holds it.

## Hardening notes

The hunting passes also recorded 195 hardening observations that are not findings and were not independently checked.
They are not filed.
The ones most likely to matter: `vendor-check` does not notice a file deleted from a vendored tree, the admission-policy copies that envtest loads can drift from the chart templates with no gate comparing them, the proxy's default ServiceAccount token is automounted, and deleting an EgressProxy leaves its share ConfigMaps behind.
