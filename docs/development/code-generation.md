# Agent reference: Code generation

## When to regenerate

Whenever you modify CRD types (`api/` for the shared v2 kinds, `cmd/agc/api/` or `cmd/gmc/api/` for the v1 kinds), run the corresponding targets.
Also run `make manifests` whenever you add or remove RBAC verbs/resources in a controller.

A `v2beta1` change also has a second output: `make api-reference` rewrites the published field reference from the same markers.
It is not part of `make generate` — that target is controller-gen's three modules — so a doc-comment edit that changes no schema still owes it.
See [§ The generated API reference](#the-generated-api-reference).

The v2 (`actions-gateway.com`) `v2alpha1` kinds live in the neutral `api/` module shared by both controllers — the Actions Gateway Controller (AGC) and the Gateway Manager Controller (GMC) (Q164); the v1 (`actions-gateway.github.com`) kinds stay split across `cmd/agc/api/v1alpha1` and `cmd/gmc/api/v1alpha1`.

## The root targets regenerate everything (start here)

From the repo root, either target covers all three modules — you do not need to know which module a type change reaches:

```bash
make generate    # manifests + DeepCopy, for all of api, cmd/gmc, cmd/agc
make manifests   # manifests only, for all three (the `make codegen-check` remedy)
```

**`make generate` is the one to reach for after a type change**, and it supersets `make manifests` — every module's `generate` is `manifests deepcopy`, so there is no second controller-gen pass to pay for.
Reach for `make manifests` only when a change alters no Go type: adding or removing a `+kubebuilder:rbac` or `+kubebuilder:webhook` marker.

Both cost about two seconds.
Because `make generate` covers every module, it also handles the cross-module case below — an AGC type edit reaches the GMC manifest without you having to know it embeds one.

This has to stay true.
`cmd/gmc`'s `generate` was once DeepCopy-only, so the root loop over the three modules' `generate` skipped GMC's manifests — the one module whose CRD embeds another module's types, and so the exact miss behind [Q440](../STATUS.md) ([Q458](../STATUS.md)).
**A module's `generate` must keep depending on its `manifests`**; if one stops, the root target silently under-covers again and only `make codegen-check` notices.

## Per-module targets

`make -C <module> generate` is per-module, so editing a v2 type means regenerating the `api/` module, not the controller modules.
Use these when you know the blast radius and want the faster loop; when in doubt, use the root targets above.

## API module (the shared v2 kinds)

```bash
make -C api generate   # regenerates zz_generated.deepcopy.go + the five v2 CRD YAMLs under api/config/crd
```

The `api/` module owns only API artifacts: DeepCopy methods and the five v2 CRD manifests (`ActionsGateway`, `EgressProxy`, `RunnerSet`, `RunnerTemplate`, `ClusterRunnerTemplate`).
It emits **no** RBAC or webhook manifests — those markers live on the controllers/webhooks in `cmd/gmc` and `cmd/agc` and are generated there.

## AGC

```bash
make -C cmd/agc generate   # regenerates zz_generated.deepcopy.go + CRD YAML + RBAC manifests
```

## GMC

```bash
make -C cmd/gmc generate   # regenerates zz_generated.deepcopy.go + CRD YAML + RBAC + webhook manifests
```

Each module also exposes the two halves separately — `manifests` (CRD/RBAC/webhook YAML) and `deepcopy` (`zz_generated.deepcopy.go`) — but `generate` runs both, and running both is what you want after a type change.
**The manifests half is the one that is easy to skip and expensive to skip**: leaving the CRD YAML out of sync with the Go types makes the apiserver silently prune unknown fields, so tests that set those fields see the zero value instead.

## Codegen drift is gated by `make check`

`make codegen-check` ([`scripts/go/check-codegen-drift.sh`](../../scripts/go/check-codegen-drift.sh)) regenerates every module's CRD/RBAC/webhook manifests **and** its `zz_generated.deepcopy.go` into a scratch tree and fails if any committed copy differs.
It runs in `make check` and in CI's `lint` job, so a forgotten `make generate` is caught on the PR that caused it rather than by the next contributor.

**The cross-module case is why it exists.** The GMC's `ActionsGateway` CRD embeds AGC types (`RunnerGroupSpec`), so a doc comment or field edited in `cmd/agc/api/` changes the *GMC* manifest — and only `make -C cmd/gmc manifests` propagates it.
`make -C cmd/agc manifests` does not.
PR #793 edited a `quotaRetryDelay` doc comment in `RunnerGroupSpec` and the GMC CRD stayed stale, handing every later GMC contributor that hunk as unrelated diff noise ([Q440](../STATUS.md)).

The root `make generate` (or `make manifests`) is the way not to have to reason about this: it covers all three modules, so an AGC type edit reaches the GMC manifest without you knowing it had to.
**If you use the per-module targets instead, regenerate every module that embeds the type you changed, not just the one you edited.**

**The DeepCopy half is gated for a different reason.** Missing DeepCopy code was assumed to fail the build, and for a *changed* type it does.
For an *added* one it does not: `ClusterCapacity` ([Q470](../STATUS.md)) shipped with no `DeepCopy`/`DeepCopyInto` and an `ActionsGatewaySpec.DeepCopyInto` that never copied the field, so `ActionsGateway.DeepCopy()` handed back an object aliasing the caller's pointer into the shared informer cache — compiling cleanly and passing CI ([Q477](../STATUS.md)).
**`make manifests` alone no longer satisfies the gate after a type change; `make generate` does.**

The gate also fails if a module gains a `manifests:` or `deepcopy:` target without being registered in the script's `MODULES` table or `DEEPCOPY_MODULES` list, or if a registration stops matching the module's own recipe — so it cannot quietly under-cover.
Full assertion list: [testing.md § The codegen drift gate](testing.md#the-codegen-drift-gate).

**Editing a `manifests:` or `deepcopy:` recipe means re-syncing its registration** — adding or dropping a generator, or pointing an `output:` rule at a different dir, fails the gate until the registration matches again — and so does changing the `object` call, which `DEEPCOPY_MODULES` holds as one shared string for all three modules.
The recipe is parsed out of the `Makefile` as text, tabs and backslash continuations included, so that parsing is itself asserted by [`scripts/go/check-codegen-drift-test.sh`](../../scripts/go/check-codegen-drift-test.sh) under `make scripts-test` ([why](testing.md#the-codegen-drift-gate)); re-wrapping a recipe is safe, but do it with that suite green.
Commenting a call out is safe too — shell comments are stripped before the parse, so a `#`-disabled generator or `output:` rule counts as absent, exactly as `make` sees it (Q464).
Dropping the row is still what a permanently removed target needs.

## The generated API reference

[`docs/reference/api.md`](../reference/api.md) is the published field-level API reference, generated by [`crd-ref-docs`](https://github.com/elastic/crd-ref-docs) from the same doc comments and `+kubebuilder` validation markers controller-gen turns into the CRD schemas.
Editing a field's description means editing the Go doc comment, then:

```bash
make api-reference        # scripts/docs/gen-api-reference.sh — rewrite docs/reference/api.md
make api-reference-check  # the drift gate, run by `make check` and CI's lint job
```

`crd-ref-docs` is pinned in the vendored `tools/` module like controller-gen, so `make tools` builds it into `.build/` and no host install is involved.

**Scope is `api/v2beta1` only** — the served, storage, non-deprecated version.
`v1alpha1` and `v2alpha1` are removed at v2.0.0; a deprecated version documented field by field beside the current one reads as a supported choice, and `v2alpha1` is `v2beta1`'s shape anyway apart from the two RunnerSet fields the conversion webhook round-trips.
Readers on either get `kubectl explain` and the [deprecation](../operations/v1alpha1-deprecation.md) and [migration](../operations/migration-v1-to-v2.md) pages instead — spelled out on the [reference overview](../reference/README.md).
**Adding a version to the page is a deliberate decision, not a mechanical one**; adding a *kind* to `v2beta1` needs nothing beyond `make api-reference`, but do add it to the generator's completeness assertion (below).

Three inputs shape the output, all under [`api/hack/crd-ref-docs/`](../../api/hack/crd-ref-docs/):

- **`config.yaml`** — the ignored types/fields, and `render.kubernetesVersion`, which pins the Kubernetes release the embedded core types (`PodTemplateSpec`, `Affinity`, `ObjectMeta`) link to on kubernetes.io.
  **Bump it alongside the `k8s.io/api` minor in `api/go.mod`** so the linked schema is the one the CRDs actually embed (`k8s.io/api` v0.36.x is Kubernetes 1.36).
- **`templates/*.tpl`** — a fork of the tool's built-in markdown set.
  A `--templates-dir` replaces the whole set, so all four are present even where only one changed.
  Three changes: the page preamble in `gv_list.tpl`; a regex in `gv_details.tpl`/`type.tpl` that renders godoc's `# Section` heading syntax as a bold lead (left alone those become `<h1>`s in the middle of the page and put package-internal asides in the site TOC beside the kinds); and the `field_doc` helper in `type_members.tpl` that every doc cell renders through.
  The tool's `markdownRenderFieldDoc` turns *every* newline into `<br />`, so a description reached the table cell still wrapped at the Go source's ~80 columns and broke mid-sentence at a width the browser never chose; `field_doc` joins the soft wraps back into prose and keeps a break only for a blank line or a list item.
  **Doc comments are reflowed, so write them for godoc**: the site no longer inherits their wrap points.
  **Re-diff the fork against `tools/vendor/github.com/elastic/crd-ref-docs/templates/markdown/` when the pinned `crd-ref-docs` version moves.** A Dependabot bump that changes the rendering turns `api-reference-check` red on its own PR — `tools/**` is in the gate's CI path filter — and `make api-reference` is the remedy.
- **the kind list in [`gen-api-reference.sh`](../../scripts/docs/gen-api-reference.sh)** — the script fails unless every `v2beta1` kind has a section in the output.
  A tool that finds no API types exits 0 having rendered a stub, which would make `--check` pass on any tree and the writing mode quietly replace a good page.

## Sync the Helm chart CRDs (after any CRD change)

The Helm charts ship the CRDs under `templates/crds/`, but the **authoritative** schema is the controller-gen output under `cmd/*/config/crd` (the v1alpha1 CRDs) and `api/config/crd` (the v2alpha1 CRDs).
The chart copies are *generated* from those sources — do not hand-edit them.
The split:

- **`charts/actions-gateway/templates/crds/`** — the two **v1alpha1** (`actions-gateway.github.com`) CRDs: `ActionsGateway`, `RunnerGroup` (sourced from `cmd/*/config/crd`).
- **`charts/actions-gateway-crds-v2/templates/crds/`** — the five **v2alpha1** (`actions-gateway.com`) CRDs: `ActionsGateway`, `EgressProxy`, `RunnerSet`, `RunnerTemplate`, `ClusterRunnerTemplate` (sourced from `api/config/crd`).
  They live in a **separate, opt-in chart** because the `RunnerTemplate`/`ClusterRunnerTemplate` CRDs each embed a full `PodTemplateSpec` (~600 KB) and adding them to the main chart pushed its Helm release Secret past the hard **1 MiB** limit (Helm stores the rendered manifest *plus* a copy of the chart source, gzipped, in one Secret).
  A separate release keeps each chart within budget and makes v2 opt-in ([Q149](../STATUS.md)).

`scripts/manifest/sync-chart-crds.sh` writes both charts in one pass.
After regenerating manifests, re-sync:

```bash
make chart-crds   # scripts/manifest/sync-chart-crds.sh — regenerates the chart CRD templates from the sources
```

`make chart-crds-check` (run by `make check`, `make manifest-validate`, and CI's `manifest-validate.yml`) fails if a chart copy drifted from its source, or if the **GMC-bundled** RunnerGroup CRD (`cmd/gmc/config/crd/bases/…runnergroups.yaml`, a bundled copy of the *imported* type) has drifted from the AGC-authoritative copy — a k8s.io/api skew that would otherwise silently prune fields on deploy ([Q73](../STATUS.md)).
**`make -C cmd/gmc manifests` cannot refresh the bundled copy** — controller-gen walks only the GMC module's own packages (`paths="./..."`), and the `RunnerGroup` type lives in `cmd/agc/api/`.
The remedy after any RunnerGroup type change: regenerate the AGC copy (`make -C cmd/agc manifests`), `cp` it over the GMC-bundled path, then `make chart-crds`.
For a k8s.io/api skew, align the module versions ([Q68](../STATUS.md)) first, then do the same.

## Sync the Helm chart RBAC (after any RBAC marker change)

The chart's GMC `manager-role` ClusterRole templates the metadata/binding, but its **rules** are the controller-gen output of the GMC controllers' `+kubebuilder:rbac` markers (`cmd/gmc/config/rbac/role.yaml`).
The chart embeds them via `.Files.Get` from a *generated* fragment — do not hand-edit it.
After regenerating manifests, re-sync the chart:

```bash
make chart-rbac   # scripts/manifest/sync-chart-rbac.sh — regenerates charts/actions-gateway/files/manager-role-rules.yaml
```

`make chart-rbac-check` (run by `make check`, `make manifest-validate`, and CI's `manifest-validate.yml`) fails if the fragment drifted from `cmd/gmc/config/rbac/role.yaml` — so a permission added via a marker but not propagated to the chart, which would leave the deployed GMC missing the grant, fails CI ([Q142](../STATUS.md)).

## Sync the Helm chart webhook (after any webhook-marker change)

The chart ships the `ValidatingWebhookConfiguration` at `charts/actions-gateway/templates/webhook.yaml`, but the authoritative webhook **body** (rules, `failurePolicy`, `sideEffects`, `admissionReviewVersions`, the service path) is the controller-gen output of the `+kubebuilder:webhook` marker (`cmd/gmc/config/webhook/manifests.yaml` — the same file the GMC integration suite loads into envtest).
The chart template is *generated* from that source, re-injecting the chart's Helm wiring (name prefix, labels, the cert-manager CA-inject annotation, the templated namespace, and the non-cert-manager `caBundle`) — do not hand-edit it.
After regenerating manifests, re-sync the chart:

```bash
make chart-webhook   # scripts/manifest/sync-chart-webhook.sh — regenerates charts/actions-gateway/templates/webhook.yaml
```

`make chart-webhook-check` (run by `make check`, `make manifest-validate`, and CI's `manifest-validate.yml`) fails if the chart template drifted from the controller-gen source — so a marker change (a new intercepted resource/operation, a path or `failurePolicy` change) that is regenerated into `config/` but not propagated to the chart fails CI ([Q143](../STATUS.md)).

## AGC ClusterRole rules (NOT controller-gen)

Both AGC ClusterRoles the chart ships are hand-maintained, and both are gated by `cmd/agc/internal/controller/rbac_chart_drift_test.go` (a unit test, so `make check` runs it): every `(group, resource)` the AGC's `+kubebuilder:rbac` markers declare must be shipped by one of the two fragments, or a real install 403s on it.

### agc-tenant-role

The `agc-tenant-role` ClusterRole — the permission set every AGC ServiceAccount runs as — is **not** generated from a `+kubebuilder:rbac` marker.
It deliberately withholds permissions the AGC's own marker role (`cmd/agc/config/rbac/role.yaml`, ClusterRole `agc-role`) grants (e.g.
`runnergroups` create/delete, `secrets` patch) for least privilege, so generating it from the markers would be a privilege escalation.
Its single source is the hand-maintained fragment `charts/actions-gateway/files/agc-tenant-role-rules.yaml`: the chart embeds it via `.Files.Get` in `templates/agc-tenant-role.yaml`, and the GMC integration suite (`installAGCTenantClusterRole`) reads the same file — so the shipped role and the RBAC-scope test can never drift.
Edit the fragment, not either consumer ([Q143](../STATUS.md)).

### agc-clusterrunnertemplate-reader

The `agc-clusterrunnertemplate-reader` ClusterRole carries the cluster-scoped reads a namespaced `RoleBinding` cannot authorize — `clusterrunnertemplates` and (since [Q450](../STATUS.md)) `runtimeclasses`.
Its source is likewise the hand-maintained fragment `charts/actions-gateway/files/agc-clusterrunnertemplate-reader-rules.yaml`.
Unlike the tenant role it holds *exactly* what the markers declare for those kinds, so the drift test asserts verb-level equality both ways, plus the read-only property the cluster-wide scope depends on: any write verb here fails the test ([Q454](../STATUS.md), rationale in [design/05-security.md](../design/05-security.md#the-agcs-cluster-scoped-read-surface)).
Adding a cluster-scoped kind to the AGC means editing the marker in `doc.go` *and* this fragment in the same change.

## RBAC marker placement

`+kubebuilder:rbac` is a **package-level** marker (controller-gen v0.21+).
It must appear before the `package` declaration, not in a type's doc comment.
Placing it on a struct silently produces no output — controller-gen won't warn, it will just generate nothing.

```go
// Correct — before the package declaration:
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch

package controller
```

```go
// Wrong — on a type, silently ignored:

// MyReconciler reconciles things.
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch
type MyReconciler struct { ... }
```

The markers live at the top of `cmd/gmc/internal/controller/actionsgateway_controller.go`.
Non-standard verbs (`bind`, `escalate`) are supported in `verbs=` and appear in the generated role.

## CRD marker and API-file gotchas

Hard-won, easy-to-reintroduce mistakes when editing the API types.
Each is a claim about how gofmt, controller-gen, or the apiserver behaves, so a new entry cites the version it was measured against per [documentation-standards.md § An upstream-behavior claim cites a measurement](documentation-standards.md#an-upstream-behavior-claim-cites-a-measurement):

- **gofmt corrupts `''` inside CEL markers.** A doubled single-quote in a `+kubebuilder:validation:XValidation` marker comment gets rewritten by gofmt into a single curly quote, silently breaking the CEL rule (and `''` is not a CEL quote escape anyway — that's SQL).
  Never use empty-string literals in XValidation: write `size(x) == 0`, not `x == ''`.
- **`selectableFields` on one served version only.** On a multi-version CRD, declaring `+kubebuilder:selectablefield` markers on more than one version makes controller-gen hoist them in a way the apiserver rejects at CRD apply time.
  Declare them on a single version.
- **The v2 condition vocabulary lives in `api/apiconditions`, not in the version packages.** Condition types and reasons are runtime `.status.conditions[]` values, not schema, so they carry no version — they are declared once, with their doc comments, in [`api/apiconditions`](../../api/apiconditions/conditions.go).
  `api/v2alpha1/conditions.go` and `api/v2beta1/conditions.go` are thin re-export blocks that keep every existing `v2alpha1.ConditionReady` call site compiling.
  **Adding a condition or reason: declare it in `apiconditions`, then add the one-line re-export to both version files.** They must stay byte-identical except the `package` line — `make v2-api-sync-check` fails on a one-sided add, naming the divergent lines.
  A new condition needs **no** `make manifests`/`make generate`.
  The same split applies to the worker-pod sidecar contract ([`api/apisidecar`](../../api/apisidecar/sidecar.go)): the heuristic and the annotation key live there, and each version keeps a `RunnerTemplateSpec`-typed wrapper.
- **Everything else the two v2 packages share must stay identical.** Kubernetes requires the *versioned types* to be duplicated per version, but the shared spec fragments beside them (`shared_types.go`, `scheduling_types.go`, the near-identical `actionsgateway_types.go`/`egressproxy_types.go`/`runnertemplate_types.go`) are identical by contract, and a one-sided edit breaks the storage/hub conversion silently.
  [`scripts/go/check-v2-api-sync.sh`](../../scripts/go/check-v2-api-sync.sh) (wired into `make check` and CI's `lint` job via `make v2-api-sync-check`) holds **every** `.go` file present in both packages identical, normalising away the entitled differences: the `package` clause, a `+kubebuilder:storageversion` marker, and a `+kubebuilder:deprecatedversion` marker (only `v2alpha1` carries it, and its warning text names the deprecated version and Kind, so it cannot be mirrored).
  Files that genuinely differ per version (`runnerset_types.go`, `conversion.go`, `groupversion_info.go`, `types_test.go`, `zz_generated.deepcopy.go`) are named in the script's `EXEMPT` list with the reason — the DeepCopy exemption leans on `make codegen-check` guarding those two files instead, which it does since [Q477](../STATUS.md); a stale entry fails the gate, and a file added to both packages is covered the day it lands with no edit to the script.

## No per-file license headers

First-party Go files carry **no** per-file Apache license header — the repo-root `LICENSE` (Apache-2.0) is the canonical, sufficient grant, so the boilerplate that Kubebuilder scaffolds is redundant.
The codegen boilerplate sources (`api/hack/boilerplate.go.txt`, `cmd/agc/hack/boilerplate.go.txt`, `cmd/gmc/hack/boilerplate.go.txt`) are intentionally **empty**, so `make generate`/`make manifests` emit header-free files — keep them empty.
`make check` runs `scripts/go/check-no-license-headers.sh`, which fails if any first-party `.go` file reintroduces the header.
Vendored trees (`vendor/`, `tools/vendor/`) keep their headers — those third-party notices are legally required and are excluded from the check.
This is unrelated to the `license-notices` / `THIRD-PARTY-NOTICES` tooling, which aggregates *dependency* notices for image distribution.
