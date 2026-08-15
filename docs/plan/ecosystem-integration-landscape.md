# Kubernetes Ecosystem Integration Landscape — Research & GAG Relevance

> Research artifact (2026-06-25).
> Goal: catalog the ~100 most-adopted Kubernetes integrations, flag which **interact**, **conflict**, or **need integration** with github-actions-gateway (GAG), and propose backlog items + conventions to adopt.
> Popularity is a rough guestimate from CNCF landscape + GitHub stars + field adoption; exact ranking is not the point.

## How to read the relevance column

| Tag | Meaning for GAG |
|---|---|
| 🔴 **Conflict** | Will actively break or degrade GAG behavior unless handled; needs a documented stance/guard. |
| 🟠 **Integrate** | Users will expect first-class support or examples; a real enhancement opportunity. |
| 🟡 **Interact** | Operates in the same plane (networking, scheduling, policy); coexists but worth a compatibility note. |
| ⚪ **Neutral** | Common in clusters but no meaningful coupling to GAG. |

GAG facts that drive the mapping (from `docs/design/`):
- NetworkPolicy is the **isolation boundary** → requires a policy-enforcing CNI.
- Worker pods are **ephemeral, per-job, run-to-completion** (Job-like lifecycle).
- cert-manager is the **default** TLS path; Prometheus Operator `ServiceMonitor` + OTel are supported.
- Vault transit is the shipped **workload-identity** signer; KMS/SPIRE are pluggable/deferred.
- Helm OCI chart is the **only** install path; namespace = tenant boundary.

---

## The list (~100, grouped by function, roughly by adoption within group)

### A. Container runtime & orchestration core
| # | Project | Pop. | Rel. | Note for GAG |
|--:|---|:--:|:--:|---|
|1|Kubernetes|★★★★★|🟡|Min v1.30 (ValidatingAdmissionPolicy GA). Track version-skew for newer admission features.|
|2|containerd|★★★★★|🟡|Default CRI; worker image-pull semantics ride on it.|
|3|etcd|★★★★★|⚪|Indirect.|
|4|CRI-O|★★★★|🟡|OpenShift default CRI; verify worker pod defaults behave identically.|
|5|Docker/Moby|★★★★★|🟠|Runners frequently need Docker-in-Docker / `docker build`; isolation story interacts (rootless, sysbox).|
|6|runc|★★★★★|⚪|Default OCI runtime.|
|7|gVisor (runsc)|★★★|🟠|Supported via `runtimeClassName`; the recommended worker hardening path — needs an example + perf note.|
|8|Kata Containers|★★★|🟠|VM-isolated workers via `runtimeClassName`; strongest escape defense for untrusted jobs.|
|9|Firecracker|★★★|🟡|Reachable via Kata; competitors (Actuated) use it directly for runner microVMs.|
|10|Sysbox|★★|🟠|Popular for "real" DinD without privileged; relevant to runner build workloads.|

### B. CNI & networking (highest-coupling area)
| # | Project | Pop. | Rel. | Note for GAG |
|--:|---|:--:|:--:|---|
|11|Cilium|★★★★★|🔴/🟠|Top CNI. eBPF NetworkPolicy enforces GAG isolation. **FQDN/DNS-aware policy can replace GMC's GitHub-CIDR egress rules** — first-class integration opportunity (Cilium `CiliumNetworkPolicy` toGroups/toFQDNs for `api.github.com`).|
|12|Calico|★★★★★|🔴/🟠|Co-required reference CNI. GlobalNetworkPolicy + DNS policy alternative to CIDR feed.|
|13|kube-proxy|★★★★★|🟡|DNAT path; e2e already exercises it.|
|14|CoreDNS|★★★★★|🔴|DNS egress is **confined to cluster DNS**; GAG assumes CoreDNS/kube-dns in kube-system.|
|15|NodeLocal DNSCache|★★★|🔴|Explicitly in the allowed DNS egress path (169.254.0.0/16).|
|16|Multus|★★★|🟠|Multi-NIC; could give per-tenant egress NICs as an alternative/complement to the proxy pool.|
|17|Flannel|★★★|🔴|**No NetworkPolicy enforcement → silently breaks isolation** (same failure class as kindnet). Must be documented as unsupported.|
|18|Antrea|★★★|🟡|Policy-enforcing CNI; should work, untested.|
|19|MetalLB|★★★|⚪|Bare-metal LB; proxy egress doesn't need it.|
|20|kube-vip|★★|⚪|Control-plane/LB VIPs.|
|21|Submariner|★★|⚪|Multi-cluster networking.|
|22|Gateway API|★★★★|🟡|Ingress successor; GAG exposes no public ingress, but metrics/webhook services could adopt conventions.|

### C. Service mesh (highest-conflict area)

> **Delivered (Q206):** the operator-facing coexistence guide — injection opt-out, sidecar lifecycle (native sidecars / ambient), and egress exclusions for Istio/Linkerd/ambient with concrete config — is at [operations/service-mesh-coexistence.md](../operations/service-mesh-coexistence.md).

| # | Project | Pop. | Rel. | Note for GAG |
|--:|---|:--:|:--:|---|
|23|Istio|★★★★★|🔴|**Sidecar injection breaks run-to-completion worker pods** (job + sidecar never exits) and **mesh mTLS/egress interception conflicts with the per-tenant proxy egress model**. Needs: namespace/pod opt-out guidance, ambient-mode note, `holdApplicationUntilProxyStarts`/`EXIT_ON_ZERO_ACTIVE_CONNECTIONS` caveats.|
|24|Linkerd|★★★★|🔴|Same sidecar-vs-Job lifecycle conflict; `linkerd-await --shutdown` / native sidecar mitigation worth documenting.|
|25|Envoy|★★★★★|🟡|GAG's egress proxy could be Envoy-based conceptually; today it's bespoke.|
|26|Consul|★★★|🟡|Mesh + Vault sibling; Vault path already integrated.|
|27|Cilium Service Mesh / ambient|★★★|🟠|Sidecar-less mesh sidesteps the Job conflict — preferred coexistence story.|
|28|Kuma|★★|🟡|Same sidecar caveats as Istio/Linkerd.|

### D. Ingress controllers
| # | Project | Pop. | Rel. | Note |
|--:|---|:--:|:--:|---|
|29|ingress-nginx|★★★★★|⚪|GAG exposes no ingress.|
|30|Traefik|★★★★|⚪|—|
|31|HAProxy Ingress|★★★|⚪|—|
|32|Contour|★★★|⚪|—|
|33|Kong|★★★|⚪|—|
|34|Emissary/Ambassador|★★|⚪|—|

### E. Secrets, identity & supply-chain security
| # | Project | Pop. | Rel. | Note for GAG |
|--:|---|:--:|:--:|---|
|35|cert-manager|★★★★★|🟠|**Already the default** TLS issuer (webhook, proxy CA, metrics mTLS). Keep conventions (Issuer names, annotations) idiomatic; document BYO-issuer.|
|36|HashiCorp Vault|★★★★★|🟠|**Already integrated** as the workload-identity transit signer. Expand: External Secrets path for the App key, Vault Agent injector caveats.|
|37|External Secrets Operator|★★★★|🟠|Many shops manage the GitHub App key via ESO; ship an example wiring `gitHubAppRef` to an ESO-synced Secret. **High-value, low-risk integration.**|
|38|Sealed Secrets|★★★|🟠|GitOps-friendly way to ship the App key; document compatibility.|
|39|SPIFFE/SPIRE|★★★|🟠|Pluggable alternative to Vault for workload identity (deferred KMS/SPIRE interface). Strong fit for keyless App-JWT signing.|
|40|Sigstore cosign|★★★★|🟠|GAG **already signs images** (publish.yml). Document verifying GAG images; verifying *runner* images is a user concern.|
|41|Kyverno|★★★★★|🔴/🟠|Top policy engine. Can **block GAG worker pods** if cluster policies disallow what PSA `baseline` allows (e.g. requires `runAsNonRoot`, blocks `:latest`, mandates registries). Need a compatibility matrix + sample policies that *complement* GAG.|
|42|OPA Gatekeeper|★★★★|🔴/🟠|Same conflict class as Kyverno; constraint templates may reject worker/proxy pods.|
|43|Trivy / trivy-operator|★★★★★|🟡|Image/vuln scanning; CI already runs trivy. Operator may scan worker images.|
|44|Falco|★★★★|🟡|Runtime threat detection; ephemeral worker churn can be noisy — provide tuning guidance.|
|45|Tetragon|★★★|🟡|eBPF runtime enforcement (Cilium); complements worker isolation.|
|46|Connaisseur / Kyverno verify-images|★★|🟠|Admission-time signature verification — interacts with GAG's digest-pinned images.|
|47|kube-bench / kube-hunter|★★★|⚪|CIS benchmarking.|
|48|Kubescape|★★★|🟡|Posture scanning; may flag worker pods.|

### F. Observability
| # | Project | Pop. | Rel. | Note for GAG |
|--:|---|:--:|:--:|---|
|49|Prometheus|★★★★★|🟠|**ServiceMonitor supported**; ensure metric names follow Prometheus/OTel semantic conventions.|
|50|Prometheus Operator|★★★★★|🟠|`ServiceMonitor`/`PrometheusRule` already shipped; consider packaging alerts as an installable rule set.|
|51|Grafana|★★★★★|🟠|Dashboards already provided as JSON; consider a Grafana dashboard ID / mixin.|
|52|OpenTelemetry|★★★★★|🟠|Tracing already wired via `OTEL_*`. Align span/attribute naming to OTel **semconv**; expand metric coverage.|
|53|Loki|★★★★|🟡|JSON logs are Loki-ready; document label conventions.|
|54|Fluent Bit|★★★★|🟡|Common log shipper; JSON logs compatible.|
|55|Fluentd|★★★|🟡|—|
|56|Grafana Tempo|★★★|🟡|OTLP traces land here.|
|57|Thanos|★★★|⚪|Long-term Prom storage.|
|58|Grafana Mimir / Cortex|★★★|⚪|—|
|59|Jaeger|★★★|🟡|Alt OTLP trace backend.|
|60|metrics-server|★★★★★|🔴|**Required** — proxy-pool HPA uses CPU from metrics-server.|
|61|kube-state-metrics|★★★★|🟡|Cluster-state metrics; useful for monitoring GAG objects.|
|62|node-exporter|★★★★|⚪|—|
|63|Pixie|★★|⚪|—|
|64|Datadog Agent|★★★★|🟡|Big enterprise presence; mTLS metrics endpoint may need a custom scrape config — document it.|

### G. GitOps, CD & CI (direct competitive/install plane)
| # | Project | Pop. | Rel. | Note for GAG |
|--:|---|:--:|:--:|---|
|65|Argo CD|★★★★★|🟠|Primary install vehicle for many users; ship an **Application** example for the OCI Helm chart (CRD `resource-policy: keep` interacts with pruning).|
|66|Flux|★★★★|🟠|Ditto — `HelmRelease`/`OCIRepository` example; common in regulated shops.|
|67|Actions Runner Controller (ARC)|★★★★★|🔴/🟠|**The incumbent.** GAG must own its differentiation + **migration guide** (already started). Coexistence: both can run in one cluster on different namespaces/labels. Watch for runner-label collisions.|
|68|Tekton|★★★★|🟡|Alternative CI; can *trigger* GAG or coexist.|
|69|Argo Workflows|★★★|🟡|Job orchestration; conceptually adjacent.|
|70|Argo Rollouts|★★★|⚪|Progressive delivery.|
|71|Argo Events|★★|⚪|—|
|72|Jenkins / Jenkins X|★★★★|🟡|Legacy CI; migration narratives.|
|73|GitLab Runner|★★★★|🟡|Sibling self-hosted-runner model; useful UX comparison.|
|74|Spinnaker|★★|⚪|—|
|75|Drone / Woodpecker|★★|⚪|—|
|76|Dagger|★★★|🟡|Runs CI in containers; could execute *inside* GAG runners.|
|77|RunsOn / Actuated / Cirun|★★★|🔴|Direct alternatives (AWS/microVM). Competitive positioning input, not integration.|

### H. Autoscaling, scheduling & capacity (high-coupling)
| # | Project | Pop. | Rel. | Note for GAG |
|--:|---|:--:|:--:|---|
|78|Cluster Autoscaler|★★★★★|🟠|Worker pods drive node scale-up. Document `cluster-autoscaler.kubernetes.io/safe-to-evict` on long jobs to avoid mid-job eviction.|
|79|Karpenter|★★★★★|🔴/🟠|Fast-growing node autoscaler. **Consolidation can disrupt running jobs** → must set `karpenter.sh/do-not-disrupt` on worker pods (or document it). High-value compatibility item.|
|80|KEDA|★★★★★|🟠|Event-driven scaling. **Can scale the proxy pool** via `EgressProxy.spec.managedAutoscaling: false` (Q173, shipped); a GitHub-queue-depth capacity signal remains open. ARC users know KEDA.|
|81|VPA|★★★|🟡|Vertical scaling of proxy; unblocked by `managedAutoscaling: false` (Q173, shipped).|
|82|Kueue|★★★|🔴/🟡|Batch job queueing. GAG queues at the **broker-claim layer (below Kueue)** — overlap/competition; document the boundary (already noted in appendix-d).|
|83|Volcano|★★|🟡|Batch/gang scheduling; relevant for GPU runner fleets.|
|84|Descheduler|★★★|🔴|**Will evict running worker pods** by default → strand jobs. Must document exclusion (`descheduler.alpha.kubernetes.io/...` / PDB).|
|85|PodDisruptionBudget (core)|★★★★|🟠|Proxy PDB shipped; consider worker-job protection guidance.|
|86|Goldilocks|★★|⚪|VPA recommendations UI.|

### I. Packaging, platform & multi-tenancy
| # | Project | Pop. | Rel. | Note for GAG |
|--:|---|:--:|:--:|---|
|87|Helm|★★★★★|🟠|**Sole install path.** Keep chart idiomatic; OCI registry conventions.|
|88|Kustomize|★★★★★|🟠|Many shops Kustomize-only; consider rendered-manifest export or post-render guidance.|
|89|Operator Lifecycle Manager (OLM)|★★★|🟠|OpenShift/OperatorHub users expect a bundle. Evaluate publishing a catalog entry.|
|90|Crossplane|★★★|🟡|Platform composition; GAG CR could be composed.|
|91|Cluster API|★★★|⚪|Cluster provisioning.|
|92|vCluster|★★★|🟡|Virtual clusters; GAG-per-vcluster is a valid tenancy model — test it.|
|93|Capsule|★★|🟡|Namespace-as-tenant operator; overlaps GAG's tenancy model — compatibility note.|
|94|Hierarchical Namespaces (HNC)|★★|🟡|Namespace tree; interacts with per-tenant namespace marking.|
|95|Carvel (ytt/kapp)|★★|⚪|Alt packaging.|

### J. Registry, build cache & images (runner-workload plane)
| # | Project | Pop. | Rel. | Note for GAG |
|--:|---|:--:|:--:|---|
|96|Harbor|★★★★|🟠|Private registry; document pulling worker images from Harbor + signature/digest flow.|
|97|Kaniko|★★★|🟠|Rootless in-cluster image build inside runners — pairs with `restricted` PSA. Document.|
|98|BuildKit / buildkitd|★★★★|🟠|`docker buildx` in runners; rootless mode + isolation guidance is a real ask.|
|99|Spegel / Dragonfly|★★★|🟠|P2P registry mirror — **big win for ephemeral worker image-pull storms** at scale. ✅ **Done (Q211):** recommended-companion guide at [operations/p2p-image-distribution.md](../operations/p2p-image-distribution.md).|
|100|Velero|★★★★|🟡|Backup/DR of GAG CRs and tenant namespaces; document what's safe to restore (Secrets/CA rotation caveats).|

**Honorable mentions (just outside 100, still worth tracking):** NVIDIA GPU Operator + Node Feature Discovery (GPU runner tiers), MinIO / Rook-Ceph / Longhorn / OpenEBS (cache PVCs if GAG ever adds caching), OpenCost/Kubecost (per-tenant cost attribution — natural fit for GAG's tenant model), Knative/Dapr/Crossplane (adjacent platforms), ko (Go image build, already relevant to GAG's own build), ExternalDNS, Reloader (config/secret rollout — overlaps GAG's own roll-on-rotation).

---

## What to evaluate further — backlog candidates

> **Filed to the backlog 2026-06-25** (`docs/STATUS.md`).
> Mapping: Q218 worker disruption-safety (**v2beta1 gate**), Q205 label/metric naming audit (**recommended before the beta freeze**), Q206 service-mesh, Q207 policy-engine matrix, Q208 CNI FQDN egress, Q209 GitOps+ESO examples, Q210 in-runner build, Q211 P2P image distribution, Q212 Velero, Q213 OpenCost — all Queue.
> Deferred (trigger-gated, additive): Q214 SPIFFE/SPIRE signer, Q215 worker cache backend, Q216 GPU runner support, Q217 OLM bundle.
> KEDA proxy scaling shipped as the Q173 `managedAutoscaling` opt-out.
> Only Q218/Q205 touch the v2beta1 cut; everything else is additive and sorts after it.
> See [v2beta1.md](v2beta1.md) for why Q218 gates the beta.

Ranked by value × likelihood users hit it.
Bare-ID Queue items to file:

1. **Service-mesh coexistence guide (Istio/Linkerd/Cilium ambient).** 🔴 The #1 silent breakage: injected sidecars prevent run-to-completion worker pods from terminating, and mesh egress interception fights the per-tenant proxy.
   Deliver: per-namespace injection opt-out, native-sidecar/ambient guidance, egress-exclusion notes.
   *Highest priority — affects every mesh user.*
2. **Node-autoscaler disruption safety (Karpenter + Cluster Autoscaler).** ✅ **Done (Q218).** The provisioner gap-fills `karpenter.sh/do-not-disrupt: "true"` and `cluster-autoscaler.kubernetes.io/safe-to-evict: "false"` on every worker pod so consolidation/scale-down doesn't strand running jobs.
   Overridable per-key via `podTemplate.metadata.annotations`.
3. **Descheduler exclusion.** ✅ **Done (Q218).** Worker pods are gap-filled with `descheduler.alpha.kubernetes.io/prefer-no-eviction: "true"` (current well-known key) so the descheduler doesn't evict mid-job.
4. **Policy-engine compatibility matrix (Kyverno / Gatekeeper).** ✅ **Done (Q207).** [`docs/operations/admission-policies.md`](../operations/admission-policies.md) maps each common policy class to GAG's real pod posture (per worker profile + proxy/AGC/GMC) and ships applyable Kyverno + Gatekeeper enforce/exception samples under [`operations/examples/policies/`](../operations/examples/policies/README.md).
5. **CNI-native egress policy (Cilium FQDN / Calico DNS policy).** 🟠 Offer an opt-in that replaces GMC's GitHub-CIDR feed with `toFQDNs: api.github.com` — simpler, no 24h CIDR reconcile.
   Pairs with existing `managedNetworkPolicy: false`.
6. **External Secrets Operator example for the GitHub App key.** 🟠 Low-risk, high-demand: wire `gitHubAppRef` to an ESO-synced Secret; also Sealed Secrets variant for GitOps.
7. **GitOps install examples (Argo CD Application + Flux HelmRelease).** 🟠 OCI Helm chart + CRD `resource-policy: keep` has pruning gotchas worth a tested example.
8. **KEDA-driven scaling (proxy pool and/or capacity signal).** 🟠 Proxy-pool half shipped (Q173 `managedAutoscaling: false`); a GitHub-queue-depth capacity signal remains open.
   ARC users expect KEDA.
9. **In-runner image build guidance (BuildKit/Kaniko/Sysbox + PSA profiles).** 🟠 The most common runner workload; map each build approach to the right `securityProfile`.
10. **P2P image distribution (Spegel/Dragonfly) recommendation.** ✅ **Done (Q211).** Ephemeral workers cause image-pull storms at scale; recommended-companion guide at [operations/p2p-image-distribution.md](../operations/p2p-image-distribution.md) covers Spegel vs Dragonfly and the `imagePullPolicy`/digest-pin interplay.
11. **SPIFFE/SPIRE workload-identity signer.** 🟠 Realize the pluggable signer interface beyond Vault; keyless App-JWT signing.
12. **OLM/OperatorHub bundle.** 🟠 OpenShift reach; evaluate cost vs Helm-only stance.
13. **Velero backup/restore guidance.** 🟡 What's safe to back up/restore (CA/Secret rotation caveats).
14. **OpenCost/Kubecost per-tenant cost attribution.** 🟡 Natural fit for the tenant=namespace model; label conventions to enable it.

---

## Conventions & best practices to adopt (so GAG feels native to these users)

- **Pod disruption annotations as a contract.** ✅ **Done (Q218).** Worker pods declare `karpenter.sh/do-not-disrupt: "true"`, `cluster-autoscaler.kubernetes.io/safe-to-evict: "false"`, and `descheduler.alpha.kubernetes.io/prefer-no-eviction: "true"` (gap-filled, per-key overridable) — the single biggest "plays well with my cluster" signal.
- **Standard well-known labels.** Apply `app.kubernetes.io/{name,instance,component,part-of,managed-by}` consistently across GMC/AGC/proxy/worker objects (verify current coverage).
  Tools like Lens/k9s/Argo group by these.
- **OTel semantic conventions.** Name spans/attributes per OTel semconv; align metric names with Prometheus naming guidelines (`_total`, base units) so dashboards and recording rules are portable.
- **`ServiceMonitor` + `PrometheusRule` as packaged, opt-in extras** (already partly done) — ship alerts as an installable bundle, not just sample YAML.
- **BYO everything the secure default provides.** cert-manager issuer, CNI policy engine, metrics CA — each should have a documented "bring your own / disable managed" path (mostly present).
  Make the secure default explicit and the opt-out idiomatic.
- **GitOps-first packaging.** Treat Argo CD / Flux as primary consumers: CRDs installable separately, `resource-policy: keep` documented, server-side-apply friendliness, no Helm hooks that break GitOps.
- **PSA + policy-engine layering.** Document that GAG uses PSA as a floor and is *compatible with* (not a replacement for) Kyverno/Gatekeeper; provide complementary policies rather than assuming none exist.
- **Sidecar-aware lifecycle.** Where mesh injection is unavoidable, lean on Kubernetes native sidecars (restartPolicy: Always init containers, 1.29+) and document the egress-exclusion annotations.
- **Image distribution at scale.** ✅ **Done (Q211).** Recommend digest pinning (already enforced) + a P2P mirror for ephemeral pull storms; the `imagePullPolicy` interplay is documented in [operations/p2p-image-distribution.md](../operations/p2p-image-distribution.md).

---

## Sources

- [CNCF Landscape (landscape.cncf.io / cncf/landscape)](https://github.com/cncf/landscape)
- [Actions Runner Controller](https://github.com/actions/actions-runner-controller)
- [awesome-github-actions-runners](https://github.com/neysofu/awesome-github-actions-runners)
- [RunsOn (ARC alternative)](https://github.com/runs-on/runs-on)
- [GitHub Docs — About Actions Runner Controller](https://docs.github.com/en/actions/hosting-your-own-runners/managing-self-hosted-runners-with-actions-runner-controller/about-actions-runner-controller)
