# GKE Workload Identity for the Registry Read — Q1066

> **Status (2026-10-13): 🔲 in progress.** Design decided and its premise measured on the dogfood cluster ([§2](#2-what-the-dogfood-probe-measured)); implemented with unit tests, not yet merged.
> No run has yet presented the token to a real Artifact Registry repository.

The AGC reads a worker image out of its registry to judge the runner version it ships (Q988).
It authenticates with `kubernetes.io/dockerconfigjson` Secrets, and since Q1066's first stage also with those on the worker ServiceAccount.
An Artifact Registry repository that kubelet pulls through the node's Workload Identity has no Secret, so the read fails authentication and the verdict falls back to the tag.

This plan adds an opt-in, per gateway, for the AGC to present an access token from its own GKE Workload Identity to Artifact Registry.
ECR is out of scope here and stays on the Q1066 row.

## 1. The constraint

The AGC's egress NetworkPolicy admits link-local `169.254.0.0/16` on port 53 only, so the metadata server is unreachable; `TestBuildNetworkPolicy_DeniesCloudMetadataServer` pins that for every generated policy.
Workload Identity hands out tokens through the metadata server, so the opt-in has to open something.

What it must not open is the plain Compute Engine metadata server.
On a node pool without Workload Identity, `169.254.169.254:80` answers with the node's own service-account token, so an AGC allowed to reach it would hold node credentials whenever the opt-in met a misconfigured pool.

## 2. What the dogfood probe measured

Measured 2026-10-13 on `gag-dogfood` (GKE `1.35.8-gke.1439001`, `datapathProvider: ADVANCED_DATAPATH`, every node pool `workloadMetadataConfig.mode: GKE_METADATA`), with one pod per arm on the `workers` pool and an egress NetworkPolicy per arm that also admits DNS.
Each pod requested `…/service-accounts/default/email` and `…/default/token` with `Metadata-Flavor: Google`, and printed HTTP codes only.

| Egress the policy admits | Via `169.254.169.254:80` | Via `169.254.169.252:988` directly |
|---|---|---|
| DNS only (control) | timeout, timeout | timeout |
| `169.254.169.254/32:80` | 200 (`actions-gateway-dogfood.svc.id.goog`), 200 | timeout |
| `169.254.169.252/32:988` | timeout, timeout | 200 (`actions-gateway-dogfood.svc.id.goog`), 200 |

The policy is evaluated against the address the pod dials, before GKE redirects `169.254.169.254:80` to its metadata server.
So admitting `169.254.169.252:988` alone does not let a pod use the standard address, and the GKE metadata server serves the metadata API on its own address.

**Not measured:** a node pool without Workload Identity (dogfood has none), the legacy Calico dataplane, and any other GKE version.

## 3. Decisions

**Decided 2026-10-13 by the maintainer:** the AGC dials `169.254.169.252:988` directly, and the opt-in admits only that destination.
It was chosen over the standard `169.254.169.254:80`, Google's documented path, because of the direction each fails in.
On a pool without Workload Identity, `.252:988` should have nothing listening, so the read fails and the verdict falls back to the tag; `.254:80` would hand the AGC the node's token.
The address is a GKE implementation detail rather than a documented API, and a GKE change that moves it degrades the read to the same tag fallback.

Also considered and declined: Google's generic Kubernetes federation through `sts.googleapis.com`, which Google's page says GKE users should not use, and stopping at stage one.

## 4. Design

- **API.** `ActionsGateway.spec.registryIdentity`, a string enum `None` | `GKEWorkloadIdentity`, defaulted to `None` by the API server (`+kubebuilder:default=None`), and empty treated as `None`.
  It is on the gateway because whether its node pools run Workload Identity is the platform's fact, and it names the method so an ECR value can join it.
  Added identically to `v2alpha1`, `v2beta1` and `v2`, whose conversions are identity round-trips.
- **GMC.** When set, it passes `AGC_REGISTRY_IDENTITY=GKEWorkloadIdentity` to the AGC and adds one egress rule, `169.254.169.252/32` TCP 988, to the AGC's NetworkPolicy.
  No other policy changes; worker and proxy policies keep denying the metadata server.
- **AGC.** A token source requests `/computeMetadata/v1/instance/service-accounts/default/token` from `http://169.254.169.252:988` with `Metadata-Flavor: Google`, over a client with no proxy, since a proxied AGC would otherwise send the request to the egress proxy.
  The token is fetched per inspection and never logged or stored past it.
- **Where the token goes.** Only to registries on Artifact Registry hosts (`*-docker.pkg.dev`, `gcr.io`, `*.gcr.io`), as the login `oauth2accesstoken`, and only where no pull Secret supplies a login for that host.
  A request, a Bearer realm or a redirect that would carry this credential off the registry's own host is refused, since the AGC's own identity must not reach a host the image reference chose.
  The host match accepts only a bare DNS name: review on the PR measured `evil.example#-docker.pkg.dev/…` passing a suffix-only match and sending the token to `evil.example`.
  A redirect carrying any login is refused off `https`, since Go forwards `Authorization` to the same host whatever the scheme (go1.27.2, pinned by `TestRedirectOffHTTPSDropsTheLogin`).
- **Failure.** A token fetch that fails degrades to the anonymous read, and a refused read quotes why, as the ServiceAccount read does.

## 5. Out of scope

- ECR, which stays on the Q1066 row.
- GHCR, which documents no workload-identity path; a GitHub App installation token measured on 2026-10-13 authenticates to `ghcr.io`'s token endpoint (200, where a wrong password gets 403), but only against a public package by an App holding no `packages` permission, so private-package reads are unmeasured.
  Filed as [Q1190](../queue/Q1190.md).
