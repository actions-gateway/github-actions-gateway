# Air-gapped / private-registry install

> **Audience:** Platform engineer

This guide installs the Gateway Manager Controller (GMC) on a cluster that
**cannot pull from GitHub Container Registry (GHCR)** — an air-gapped or
egress-restricted environment. The default [install](install.md) pulls every
image (and the Helm chart) straight from `ghcr.io`; here you instead **relocate**
them to a private registry, point the chart at that registry, and authenticate
the pulls. Read [install.md](install.md) first — this guide only covers the
deltas for a private-registry install.

The secure-by-default posture is unchanged: **all images stay pinned by digest**
(relocation is content-addressed, so digests do not change) and registry
credentials are **never** committed to `values.yaml`.

---

## What you relocate

GAG references five images plus the OCI Helm chart. Only the GMC image is pulled
by the chart itself; the AGC, proxy, wrapper, and worker images are pulled by
pods the GMC and AGC provision **at runtime**.

| Image | Public ref | Pulled by | Where you point it at the mirror |
|---|---|---|---|
| **GMC** | `ghcr.io/actions-gateway/gmc` | the chart's GMC Deployment | `gmc.image.repository` + `gmc.image.digest` (chart value) |
| **AGC** | `ghcr.io/actions-gateway/agc` | per-tenant AGC Deployments (GMC-provisioned) | `agc.image.repository` + `agc.image.digest` (chart value; GMC injects it as `AGC_IMAGE`) |
| **Proxy** | `ghcr.io/actions-gateway/proxy` | per-tenant egress-proxy pools (GMC-provisioned) | `proxy.image.repository` + `proxy.image.digest` (chart value; GMC injects it as `PROXY_IMAGE`) |
| **Wrapper** | `ghcr.io/actions-gateway/wrapper` | worker pods (init container the AGC injects, or a read-only image volume) | `wrapper.image.repository` + `wrapper.image.digest` (chart value; GMC injects it as `WRAPPER_IMAGE`, forwarded to each AGC) |
| **Worker** | `ghcr.io/actions/actions-runner` (default) | worker pods (AGC-provisioned) | `RunnerGroup.spec.workerImage` per tenant — **not** a chart value |
| **Helm chart** | `oci://ghcr.io/actions-gateway/charts/actions-gateway` | `helm install` | the `oci://` ref you pass to `helm` |

> The **wrapper** is on by default: the chart always sets `WRAPPER_IMAGE`, and
> the GMC rejects a floating wrapper tag at startup exactly as it does for
> `agc`/`proxy`, so you must mirror it and pin `wrapper.image.digest` like the
> others. It is a tiny (~2 MB) `FROM scratch` image the AGC injects into each
> worker pod, letting the runner container be the unmodified upstream
> `actions-runner`. See [release.md § The worker images](release.md#the-worker-images-wrapper-and-worker).

> The worker default is the upstream digest-pinned `ghcr.io/actions/actions-runner`.
> The project also publishes a first-party `ghcr.io/actions-gateway/worker`; if a
> tenant uses it, mirror that ref instead. Either way the worker image is set per
> tenant on the `RunnerGroup`, so it is configured during tenant onboarding, not
> at chart install. See [release.md § The worker images](release.md#the-worker-images-wrapper-and-worker).

Copy the five image digests from the
[release notes](https://github.com/actions-gateway/github-actions-gateway/releases)
for the version you are installing. Throughout this guide, replace
`registry.internal/gag` with your private registry path.

---

## 1. Mirror the images (preserving digests)

Copy each image from GHCR to your private registry **by digest**, so the mirrored
copy is byte-identical and the same `sha256:` digest pins it on both sides. Run
this from a host that can reach *both* registries (a bastion / jump host), then
move the registry behind the air gap — or pull to a tarball and `load` on the
inside.

Use whichever tool you already have. [`cosign copy`](https://docs.sigstore.dev/cosign/working_with_other_artifacts/)
carries the cosign signature and SBOM/provenance attestations along with the
image, so prefer it when you want to keep verifying provenance after relocation:

```sh
# Replace <gmc>/<agc>/<proxy>/<wrapper> with the digests from the release notes,
# and X.Y.Z with the release version.
for img in gmc agc proxy wrapper; do
  cosign copy \
    ghcr.io/actions-gateway/$img:X.Y.Z \
    registry.internal/gag/$img:X.Y.Z
done

# Worker (upstream runner image — has no GAG cosign signature):
crane copy \
  ghcr.io/actions/actions-runner:2.335.1 \
  registry.internal/gag/actions-runner:2.335.1
```

`crane copy` or `skopeo copy --all` (use `--all` to carry the full multi-arch
OCI index, not just one platform) are equivalent for the copy itself; they do
not relocate the cosign signature, so use `cosign copy` for the signed GAG
images if you intend to re-verify provenance from the mirror.

**Verify the digest is preserved** — it must match the release notes:

```sh
crane digest registry.internal/gag/gmc:X.Y.Z
# Expected: sha256:<gmc>  (identical to the public digest)
```

> Tarball variant (no shared-network bastion): `crane pull … out.tar` on the
> outside, transfer the tarball across the gap, `crane push out.tar
> registry.internal/gag/gmc:X.Y.Z` on the inside. The digest is preserved.

---

## 2. Relocate the Helm chart

The chart is an OCI artifact. If your cluster cannot reach `ghcr.io`, mirror it
too (skip this if you install from a [source checkout](install.md#install) — the
`charts/actions-gateway` path needs no registry):

```sh
oras cp \
  ghcr.io/actions-gateway/charts/actions-gateway:X.Y.Z \
  registry.internal/gag/charts/actions-gateway:X.Y.Z
```

`helm pull oci://ghcr.io/actions-gateway/charts/actions-gateway --version X.Y.Z`
followed by `helm push` to the internal registry is an equivalent two-step. If
you use the opt-in v2 CRD chart (`actions-gateway-crds-v2`), relocate it the same
way.

---

## 3. Create the image-pull Secret (the secure pattern)

The private registry requires authentication. Create a
`kubernetes.io/dockerconfigjson` Secret **from a credentials file** — never
embed registry credentials in `values.yaml` (it gets committed to Git) or pass
them on a command line (they leak into shell history and process listings).

Write a Docker config file containing a **scoped, read-only** registry token,
`chmod 600` it, create the Secret from it, then delete the file:

```sh
umask 077
# Use a robot/read-only pull token, not a human admin credential.
cat > dockerconfig.json <<'EOF'
{ "auths": { "registry.internal": { "auth": "<base64 user:token>" } } }
EOF

kubectl create secret docker-registry private-registry \
  --namespace gmc-system \
  --from-file=.dockerconfigjson=dockerconfig.json

shred -u dockerconfig.json   # or: rm -P / rm -f
```

`kubectl create secret docker-registry` also accepts `--docker-server/-username/-password`
flags, but those place the password in your shell history and the process
table — prefer the `--from-file` form above.

You will create this Secret in **`gmc-system`** (for the GMC pull, this step) and
in **each tenant namespace** (for the AGC/proxy/worker pulls, [step 5](#5-wire-pull-secrets-for-the-runtime-workloads-agc--proxy--worker)).

---

## 4. Install the chart with the overrides

Install from the relocated chart, overriding each image `repository` to the
mirror, keeping the `digest` from the release notes, and referencing the pull
Secret:

```sh
helm install gag oci://registry.internal/gag/charts/actions-gateway \
  --version X.Y.Z \
  --namespace gmc-system --create-namespace \
  --set gmc.image.repository=registry.internal/gag/gmc \
  --set gmc.image.digest=sha256:<gmc> \
  --set agc.image.repository=registry.internal/gag/agc \
  --set agc.image.digest=sha256:<agc> \
  --set proxy.image.repository=registry.internal/gag/proxy \
  --set proxy.image.digest=sha256:<proxy> \
  --set wrapper.image.repository=registry.internal/gag/wrapper \
  --set wrapper.image.digest=sha256:<wrapper> \
  --set 'imagePullSecrets[0].name=private-registry'
```

- `imagePullSecrets` is attached to the **GMC pod only** — that is the one
  workload this chart runs.
- `agc.image.*` / `proxy.image.*` / `wrapper.image.*` set the references the GMC
  **injects** into the gateways it provisions — the AGC and proxy pods, and the
  wrapper init container in every worker pod; relocating the repository here is
  enough for the GMC to pull them from the mirror. Their *pull Secret* is wired
  separately in [step 5](#5-wire-pull-secrets-for-the-runtime-workloads-agc--proxy--worker).
- Digest pinning is unchanged — `gmc.image.digest` is still mandatory (rendering
  fails without it) and the GMC still rejects floating AGC/proxy/wrapper tags at
  startup. Do **not** reach for `allowFloatingImageTags`; relocation keeps the digest.

> Many `--set` flags are easier to manage in a values file. Put the
> `repository`/`digest`/`imagePullSecrets` overrides in `air-gapped-values.yaml`
> and `helm install … -f air-gapped-values.yaml`. The pull Secret is referenced
> by **name** there — its credentials still live only in the Secret from step 3,
> never in this file.

---

## 5. Wire pull Secrets for the runtime workloads (AGC / proxy / worker)

The AGC, proxy, and worker pods are created by the GMC/AGC at runtime, so the
chart cannot attach `imagePullSecrets` to them. Use the standard Kubernetes
mechanism: an `imagePullSecrets` reference on a **ServiceAccount** is
auto-injected into every pod that uses that SA. All the relevant ServiceAccounts
live in the **tenant namespace**.

For each tenant namespace, first create the pull Secret there (repeat step 3 with
`--namespace <tenant-ns>`), then patch the ServiceAccounts:

```sh
TENANT_NS=team-a

# AGC pods run as the fixed AGC ServiceAccount "actions-gateway-controller"
# (one per tenant namespace; the name is not derived from the gateway name).
kubectl patch serviceaccount actions-gateway-controller -n "$TENANT_NS" \
  -p '{"imagePullSecrets":[{"name":"private-registry"}]}'

# Worker pods run as "actions-gateway-worker" — this SA also pulls the injected
# wrapper init container, so no separate wrapper Secret/SA is needed.
kubectl patch serviceaccount actions-gateway-worker -n "$TENANT_NS" \
  -p '{"imagePullSecrets":[{"name":"private-registry"}]}'

# Proxy pods run as the namespace "default" ServiceAccount.
kubectl patch serviceaccount default -n "$TENANT_NS" \
  -p '{"imagePullSecrets":[{"name":"private-registry"}]}'
```

The GMC creates the `actions-gateway-controller` and `actions-gateway-worker`
ServiceAccounts when it reconciles the `ActionsGateway` CR; create the gateway
first, then patch. The
patch **survives GMC reconciliation** — the GMC only manages those SAs' labels
and owner reference, never their `imagePullSecrets` — and it never touches the
`default` SA at all.

> Alternative: if your registry is reachable by the cluster but only needs auth,
> a cluster-wide approach (e.g. a `default`-SA mutating policy, or a registry
> pull-through cache that injects credentials) also works. The per-SA patch above
> is the no-extra-tooling baseline.

---

## 6. Point the worker image at the mirror

The worker image is **not** a chart value — it is set per tenant. When onboarding
a tenant (see [tenant-onboarding.md](tenant-onboarding.md)), set the mirrored,
digest-pinned worker reference on the `RunnerGroup`:

```yaml
apiVersion: actions-gateway.github.com/v1alpha1
kind: RunnerGroup
spec:
  # Mirror of ghcr.io/actions/actions-runner, digest preserved.
  workerImage: registry.internal/gag/actions-runner:2.335.1@sha256:08c30b0a7105f64bddfc485d2487a22aa03932a791402393352fdf674bda2c29
```

Keep the `@sha256:` digest — it must match the upstream image you mirrored in
step 1. The worker SA patched in step 5 supplies the pull credentials.

---

## 7. Verify

```sh
# GMC pulls from the mirror and is Running (no ImagePullBackOff).
kubectl get pods -n gmc-system -l app.kubernetes.io/name=actions-gateway
kubectl get pod -n gmc-system -l app.kubernetes.io/name=actions-gateway \
  -o jsonpath='{.items[0].spec.containers[0].image}{"\n"}'
# Expected: registry.internal/gag/gmc@sha256:<gmc>

# The GMC injects the mirrored AGC/proxy refs into provisioned gateways.
kubectl get deploy -n gmc-system gmc-controller-manager \
  -o jsonpath='{range .spec.template.spec.containers[0].env[?(@.name=="AGC_IMAGE")]}{.value}{"\n"}{end}'
# Expected: registry.internal/gag/agc@sha256:<agc>

# After creating a tenant gateway: its AGC / proxy / worker pods (and the
# injected wrapper init container) pull from the mirror with no ImagePullBackOff.
kubectl get pods -n team-a
```

Then continue the standard [healthy-install verification](install.md#verify-a-healthy-install).
If a pod shows `ImagePullBackOff`, `kubectl describe pod` it: a 401/403 means the
pull Secret is missing from that namespace or not referenced by the pod's
ServiceAccount (step 5); a "manifest unknown" means the digest was not preserved
during relocation (step 1).

---

## Next steps

- [install.md](install.md) — the full install reference these deltas build on.
- [tenant-onboarding.md](tenant-onboarding.md) — onboard a tenant, where the
  worker image and tenant-namespace pull Secret are configured.
- [release.md](release.md) — published image/chart digests, signatures, and the
  worker-image distinction.

---

← [Back to Operations](.)
