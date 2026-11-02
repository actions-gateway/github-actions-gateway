# Q1167: how the `v2` retype and the `v2alpha1`/`v2beta1` drop split into PRs

Three PRs, and the classic removal moves ahead of the second one.
Q1262 retypes the Gateway Manager Controller (GMC) onto `api/v2`, [Q1263](../queue/Q1263.md) retypes the Actions Gateway Controller (AGC), and [Q1167](../queue/Q1167.md) then deletes `api/v2alpha1`, `api/v2beta1` and the conversion machinery in one PR.
The AGC retype waits for Q264's classic removal rather than preceding it, because the AGC reads two fields `v2` does not have, and nothing else forces an order between the two.

This is the scoping answer [Q1167](../queue/Q1167.md) asked for ("how the retype splits into PRs is the first thing to decide").
It changes no code; the background is [v2-ga.md](v2-ga.md) Phase 3.

## Status at a glance

| Step | Row | Scope | Depends on | Status |
|---|---|---|---|---|
| 0a | Q273's test port (in flight, ID not yet allocated) | Port the e2e tenant fixtures, the RunnerGroup-driven AGC envtests and `cmd/agc/test/load` onto `v2` tenants typed on `api/v2` | none | in flight |
| 0b | Q273 (in flight) | Remove `v1alpha1` and `gag-migrate`'s v1→v2 fan-out | 0a | in flight |
| 1 | Q1262 | Retype the GMC onto `api/v2` | Q273 | ✅ done; the classic-reading trio stays on `v2alpha1` ([what it left](#what-q1262-left-on-the-older-versions)) |
| 2 | Q264 | Remove classic acquisition | Q273 | ❌ open |
| 3 | [Q1263](../queue/Q1263.md) | Retype the AGC onto `api/v2` | Q273, Q264 | ❌ open |
| 4 | [Q1167](../queue/Q1167.md) | Drop `v2alpha1`, `v2beta1`, the hub and the conversion webhook | Q1262, Q1263 | ❌ open |

Steps 1 and 2 can run in parallel; step 3 waits for step 2, and step 4 waits for steps 1 and 3.
The critical path is Q273 → Q264 → Q1263 → Q1167 → Q1107 (Phase 4's docs and tag), the same depth as the order the rows carried before this plan, because Q264 sat between the removals either way.

## Why a retype is mechanical everywhere except two places

`api/v2` exports the same symbols as `api/v2alpha1` apart from four constants, and the same struct fields apart from two.
Measured off `origin/main` at `7b292627a` by extracting every exported top-level identifier from each package's non-test files:

```text
v2alpha1: 195 symbols   v2beta1: 196   v2: 194
in v2alpha1 not v2: AcquisitionProtocolClassic, AcquisitionProtocolScaleSet,
                    EgressPolicyModeCalicoFQDN, EgressPolicyModeCiliumFQDN
in v2beta1  not v2: EgressPolicyModeCalicoFQDN, EgressPolicyModeCiliumFQDN
```

The only field difference is `RunnerSetSpec.AcquisitionProtocol` and `RunnerSetSpec.MaxListeners`, which `v2alpha1` has and `v2` lacks.
Every other shared file is held byte-identical across adjacent versions by `scripts/go/check-v2-api-sync.sh`, and the condition vocabulary is the same `api/apiconditions` re-export block in all three, so `v2alpha1.ConditionX` becomes `v2.ConditionX` with nothing else changing.
`api/apiconditions` imports no API version, so it couples nothing.

That leaves two semantic sites, and they fall on different binaries:

- **The FQDN aliases (GMC).** `cmd/gmc/internal/controller/egressproxy_fqdn.go:94:	case gmcv2alpha1.EgressPolicyModeCiliumFQDN:` and the `CalicoFQDN` arm below it pick a backend from an alias `v2` cannot name.
  Q1262 deletes both arms.
  No cluster can reach `v2.0.0` holding an alias: the 1.10 storage sweep checks for one before writing anything and stops (`cmd/gmc/internal/migrate/storageversion.go:220:			if mode == string(v2beta1.EgressPolicyModeCiliumFQDN) ||`), so `storedVersions` is never pruned and the `v2.0.0` CRD apply is rejected, and admission has refused new alias writes since 1.9 (Q1085).
- **The classic fields (AGC).** `runnerset_controller.go` reads `rs.Spec.AcquisitionProtocol` or `rs.Spec.MaxListeners` at nine sites, for example `cmd/agc/internal/controller/runnerset_controller.go:536:	if err := pool.EnsureAgents(ctx, rs.Spec.MaxListeners, instToken); err != nil {`.
  These cannot compile against `v2`.
  That is the boundary with Q264, in the next section.

## Where Q264's boundary falls: before the AGC retype

**Q264 lands before Q1263, and Q264 deletes every read of the two classic fields.** The AGC retype then has nothing classic left to translate, and the fields themselves leave with the `v2alpha1` package in Q1167.
Q264's row says it strips `acquisitionProtocol`/`maxListeners`; under this order Q264 strips their *readers*, and the declarations go in Q1167, because deleting a field from a package that is about to be deleted buys nothing.

The other order costs a rewrite.
Retyping the AGC first means reading the classic marker from the conversion annotations that carry it on a `v2` object, as the `v2` validators already do (`acquisitionProtocolOf` in `cmd/gmc/internal/webhook/v2/conversion_annotations.go`).
`maxListeners` needs the same, with a decimal parse and the default of 10.
Q264 would then delete all of it a PR later.

**Q264's stated blocker does not bind inside the bundle.** Its row reads "removing it while either version is served breaks those sets": a `v2alpha1` set written `Classic` would run as ScaleSet.
That matters for a tag, and only a `v2.0.0` candidate can be tagged off `main` in the window: the 1.10 fallback is cut from a `release-1.10` branch off the `v1.10.0` tag (open PR #2234's `release-ladder.md` change, read 2026-11-02).
Candidates are cut from `main`, so what keeps one out of the window is the process, not the tag mechanics: Q1107, the cut, is blocked on every removal, and the scope ledger lists them all.
No gate under `scripts/release/` refuses a candidate while a ledger row is open (searched 2026-11-02), so this holds only as long as nobody cuts one early.
Inside the window a `Classic` set also skips the scale-set label-uniqueness check (`cmd/gmc/internal/webhook/v2/runnerset_webhook.go:178:	if acquisitionProtocolOf(rs) != acquisitionProtocolScaleSet {`) while running as ScaleSet once Q264 lands.

Q264's row and the [v2-ga.md scope ledger](v2-ga.md#scope-ledger) carry this order, and both retype rows gate `v2.0.0` beside Q1167, since the tag waits on them as much as on the drop.

## Each step leaves `main` building

The claim for each step is that no package left in the tree imports something the step removed or retyped incompatibly.
The import graph backs it.

**Step 1, Q1262 (GMC).** The GMC and the AGC are separate Go modules, and the only AGC packages the GMC imports are `agc/api/v1alpha1` (gone with Q273) and `agc/names`, which names no `v2` version.
So no `v2alpha1` value crosses between the binaries, and retyping one module cannot break the other's compile.
Within the GMC, a function's signature and every caller change together, which is why a module, not a package, is the unit: `scalesetscope.FromLists` takes `[]agcv2alpha1.RunnerSet` and `webhook/v2/runnerset_read.go` passes it one.
`v2alpha1` and `v2beta1` stay registered in the GMC's scheme (`cmd/gmc/cmd/main.go`) because the conversion webhook still serves them.

Two files in the GMC read the classic field, `scalesetscope.go:152` and through it `runnerset_read.go`.
If Q264 has merged when Q1262 is cut, they retype with the rest; if not, they stay on `v2alpha1` and Q1167 retypes them.
The closed set is three files, not two: `runnerset_read.go` also hands its `v2alpha1` lists to `agentidentity.FromLists`, the only production caller of `cmd/gmc/internal/agentidentity`, so that package stays with them (measured by compiling the retype, 2026-11-02).

**Step 3, Q1263 (AGC).** After Q264 the AGC's `v2alpha1` use is the shared surface only, so the swap is mechanical.
`cmd/agc/internal/controller/v2_detect.go` asks the REST mapper whether `RunnerSet` is served at `v2alpha1`; retyped, it asks about `v2`, which every release since 1.9 serves.

**Step 4, Q1167 (the drop).** Once steps 1 and 3 have landed, the remaining importers of `api/v2alpha1` and `api/v2beta1` are the conversion code itself, the GMC's scheme and cache setup, and tests of conversion.
`api/v2` depends on `api/v2beta1` only through `api/v2/conversion.go`, so deleting that file makes `api/v2` standalone and both older packages deletable in the same PR.

The drop is one PR rather than two (`v2alpha1` first, then `v2beta1`).
Two would build, since `v2alpha1` imports the hub and not the reverse, but the second would rewrite the conversion surface the first had just narrowed, and the whole drop is deletion that reviews in one pass.

## The hub: delete the conversion, don't move it

**Once `v2` is the only served version, Q1167 deletes the hub and the conversion webhook instead of rebuilding them around `v2`.** With one version there is nothing to convert, so each of the five converting CRDs' `spec.conversion` becomes `strategy: None` and the webhook stanza leaves the chart.
[v2-ga.md § The hub stays at v2beta1](v2-ga.md#the-hub-stays-at-v2beta1) already reasoned that a hub moved to `v2` "would be torn down one release later"; this is that teardown.
A future `v3` re-adds `Hub()` to whichever version stores, as each previous hop did.

What goes:

- `Hub()` in `api/v2beta1/conversion.go`, `ConvertTo`/`ConvertFrom` in `api/v2alpha1/conversion.go` and `api/v2/conversion.go`, and their tests.
- `cmd/gmc/internal/webhook/v2beta1/conversion.go`, the conversion webhook's registration.
- `cmd/gmc/internal/webhook/v2/conversion_annotations.go` and whatever of its readers Q264 left.
- The `conversion:` stanza in each of `charts/actions-gateway-crds-v2/templates/crds/*.yaml`.

Objects stored before the upgrade keep any `conversion.actions-gateway.com/*` annotation they carried.
After Q1167 nothing reads one, so they are inert; removing them is not needed for correctness.

## What compiles and still breaks

Six things a green build does not catch, each owed by the step that would trip it.

1. **The GMC's CRD schema check names versions as strings.** `cmd/gmc/internal/controller/crd_schema.go:50:	{crd: "runnersets.actions-gateway.com", version: "v2beta1", path: []string{"spec", "runnerGroup"}},` and three sibling entries check a field at `v2beta1` and `v2alpha1`.
   After the drop those versions are absent from the CRD; `VerifyCRDSchemas` runs at startup, so Q1167 repoints the entries at `v2`.
2. **An admission policy matches `runnersets` at `v2alpha1` alone.** `charts/actions-gateway/templates/tenant-resource-guard.yaml:65:        apiVersions: ["v2alpha1"]` confines the GMC's own writes, and it reaches a `v2` write today only through the default `matchPolicy: Equivalent`.
   Once `v2alpha1` is not served it matches nothing and the guard stops confining `runnersets` without an error anywhere.
   `make webhook-versions-check` exists for exactly this failure and does not see it: it reads `cmd/gmc/config/webhook/manifests.yaml` and the CRDs, never the chart's `ValidatingAdmissionPolicy` templates.
   Q1167 retargets the rule to `v2` and widens the gate to read those templates, so the gate's stated purpose holds.
   `priorityclass-allowlist-guard.yaml` names all three versions and needs only tidying.
3. **The GMC's cache is scoped by Go type.** `cmd/gmc/cmd/config.go:148:		byObject[&v2beta1.PriorityClassAllowlist{}] = cache.ByObject{` must retype in the same PR as the `PriorityClassAllowlist` controller (Q1262), or the controller's informer runs at `v2` without the scoping.
4. **The released-chart upgrade check meets `storedVersions`.** `make chart-released-upgrade-check` installs the last published chart and upgrades it to `HEAD`.
   The apiserver refuses to drop a version still listed in a CRD's `status.storedVersions`; a fresh 1.10 install stores at `v2` only, so this is expected to pass, but it is unmeasured until Q1167 runs it.
   The check runs in the e2e lanes, which are merge-group-only, so Q1167 gets an earlier verdict from `make e2e` before it is enqueued.
5. **Dogfood's storage leg.** A `v2.0.0` candidate can only be applied to a cluster the 1.10 sweep has pruned to `["v2"]`; `scripts/dogfood/validate-release.sh` reads it.
   This binds the release candidate, not the merge.
6. **The `v2.0.0` sweep would fail on the cluster it was built for.** `gag-migrate storage-version` survives `v2.0.0`, and the candidate gate's `storage` leg, one of its default legs (`GATE_LEGS_ALL` in `scripts/dogfood/lib/gate-legs.sh`), applies the candidate's own CRDs and then runs the candidate's own sweep.
   The sweep lists `EgressProxy` at `v2beta1` to look for aliases (`cmd/gmc/internal/migrate/storageversion.go:215:func aliasEgressProxies(`) before it reaches the per-kind "`storedVersions` already `[v2]`; nothing to do" short-circuit, and a `v2.0.0` CRD set serves no `v2beta1`.
   Read from the code; the list error itself is inferred, not measured.
   Q1167 skips the alias check when every kind already stores `[v2]`, since an alias cannot be stored at `v2`, or treats a no-match error from the list as no aliases, and tests the sweep against a `v2`-only CRD set.

Beyond the Go: `scripts/go/check-v2-api-sync.sh` fails on any stale exemption entry, so Q1167 deletes the script, its `v2-api-sync-check` target and its `unit-test.yml` step together with the packages.
Non-Go files naming `actions-gateway.com/v2alpha1` or `v2beta1` (charts, `deploy/`, `scripts/`, operator docs and examples) move to `v2` in Q1167, since they stop applying the moment the CRDs stop serving those versions.

## The tree this plan assumes, after Q273

Q273 is in flight as two PRs, and both land first, so the counts below assume them.
Its plan as relayed by the batch's orchestrator on 2026-11-02, not read from a branch:

- **A test port lands first.** The e2e tenant fixture (`cmd/gmc/test/utils/fixtures.go`), the AGC envtests that run through the RunnerGroup reconciler, and the load harness under `cmd/agc/test/load` move onto `v2` tenants typed on `api/v2`, so neither retype redoes them.
- **The removal deletes** `cmd/agc/api/v1alpha1/`, `cmd/gmc/api/v1alpha1/`, `cmd/gmc/internal/webhook/v1alpha1/` and `gag-migrate`'s v1→v2 fan-out (`cmd/gmc/internal/migrate/migrate.go` and its tests).
- **`gag-migrate` survives `v2.0.0` with `storage-version` only.**

The counts take the deleted paths above out, and take out the test files the port is assumed to cover: the e2e and load files, and the AGC envtests importing `v1alpha1`.
That assumption removes six files from the retypes' scope that import an older `v2` version (five AGC envtests and one e2e test) and changes no non-test count.
The removal leaves six more files importing one gone.

**What survives of `gag-migrate` is in scope, and the sweep is the exception.** `cmd/gmc/migrate/main.go` and any fan-out helper Q273 keeps (`cmd/gmc/internal/migrate/names.go` takes `v2alpha1.RunnerTemplateSpec`) retype with the GMC in Q1262.
`cmd/gmc/internal/migrate/storageversion.go` does not: it reads `EgressProxy` at `v2beta1` on purpose, because the sweep runs against a 1.10 cluster before the upgrade and only `v2beta1` still shows a stored alias.
It cannot retype onto `v2` and keep that check, so Q1167 replaces its import of the deleted package with the version string and the two alias values held locally; it already reads through unstructured objects.
It must also keep working against a `v2`-only cluster, which it does not today ([item 6 above](#what-compiles-and-still-breaks)).

**34 surviving files import both `v1alpha1` and `v2alpha1`/`v2beta1`, 29 after the test port.** Q273 edits each to drop its `v1alpha1` half, and Q1262 or Q1263 then edits the same file, so each retype starts by rebasing onto Q273's edits.
Seven are non-test: `cmd/agc/main.go`, `cmd/agc/internal/controller/runnerset_controller.go`, `cmd/gmc/cmd/main.go`, `cmd/gmc/internal/agentidentity/agentidentity.go`, `cmd/gmc/internal/webhook/v2/runnerset_read.go`, `cmd/gmc/internal/webhook/validation/validation.go` and `cmd/gmc/migrate/main.go`.
The rest are tests, mostly under the two `internal/controller/integration` suites.

## What Q1262 left on the older versions

Q264 had not merged when Q1262 was cut, so the classic-reading trio stayed on `v2alpha1`: `scalesetscope.go`, `webhook/v2/runnerset_read.go` and `agentidentity.go`, with their own unit tests.
Q1167 retypes them once Q264 has removed the classic read.

Their callers are on `v2`, and the fake client does not convert between versions, so unit tests reaching the trio store each fixture at both versions.
`withV2alpha1Views` in `webhook/v2/actionsgateway_webhook_test.go` does it for the validators, and `asV2Gateway` in `controller/actionsgateway_scaleset_collision_test.go` for the collision check; both go when the trio retypes.

The split also costs the GMC a second informer for `ActionsGateway` and `RunnerSet` until then: `evalScaleSetNameCollisions` lists through the reconciler's cached client at `v2alpha1`, while the controllers watch `v2`, and controller-runtime keys informers by group, version and kind (read from source, not measured).
And a stored `CiliumFQDN`/`CalicoFQDN` alias on a `main` build in this window is admitted unchanged but read by the controller as plain `FQDN`, so it follows `--fqdn-policy-backend`; under `none` the pool loses its CNI-native policy without a condition.
No `v2.0.0` cluster can hold one ([why](#why-a-retype-is-mechanical-everywhere-except-two-places)).

The rest of what stays names a version on purpose, and Q1167 deletes or retargets it:

- `cmd/gmc/cmd/main.go` registers `v2alpha1` and `v2beta1`, which the conversion webhook still serves, and `storageversion.go` reads `v2beta1` as above.
- The conversion tests (`v2_conversion_test.go`, `v2_ga_conversion_test.go`, `conversion_annotations_test.go`), the storage sweep's tests, the hub reads in `v2_scheduling_test.go` and `actionsgateway_webhook_test.go`, and `v2_egressproxy_admission_test.go`, which pins the alias admission (Q1085) at the versions that can name one.
- `priorityclass_vap_test.go` writes at each served version, so the guard's rules for `v2alpha1` and `v2beta1` stay covered until Q1167 retargets them ([item 2](#what-compiles-and-still-breaks)).
- `webhook_admission_test.go` writes one Classic set at `v2alpha1`, and `cmd/gmc/test/utils/fixtures.go` still builds Classic sets for e2e; both are Q264's.

The `v2alpha1` CRD tests retyped onto `v2` as `v2_crd_test.go`: their CEL rules are byte-identical across versions, which `check-v2-api-sync.sh` holds, so they now cover the version that survives.

## Sizes, measured

Counted off `origin/main` at `7b292627a` on 2026-11-02: tracked `.go` files outside any `vendor/` tree, matched on the full import path of each version, with test and non-test files counted apart.
`cmd/agc/api/v1alpha1` and `cmd/gmc/api/v1alpha1` are imported as `…/agc/api/v1alpha1` and `…/gmc/api/v1alpha1`, which a pattern on `/api/v1alpha1"` alone misses.

| Importers of | Non-test | Test |
|---|---|---|
| `api/v2alpha1` | 50 (GMC 30, AGC 20) | 154 |
| `api/v2beta1` | 7 | 25 |
| `api/v2` | 8 | 19 |
| `v1alpha1` and `v2alpha1`/`v2beta1` | 9 | 30 |

The row's 2026-10-09 figures were 43 non-test and 180 in all; the tree has grown since, to 204.
The batch's own count of 37 files importing both `v1alpha1` and an older `v2` version is two short of the 39 here; the difference was not reconciled, so read either as approximate.

After Q273's two PRs as assumed above, the files each retype edits:

| Step | Non-test | Test | Note |
|---|---|---|---|
| Q1262 (GMC) | 32 | 77 | the non-test count includes `storageversion.go`, which goes to Q1167 instead; the test count includes conversion tests (`v2_conversion_test.go`, `v2_ga_conversion_test.go` and others), which Q1167 deletes rather than Q1262 retyping them |
| Q1263 (AGC) | 20 | 67 | the test count includes `integration/conversion_webhook_test.go` and `v2alpha1_crd_test.go`, which go to Q1167 likewise |
| Q1167 (drop) | `api/` 2, plus what the retypes left | `api/` 5, plus conversion tests | most of the diff is deleted packages and generated CRDs |

These count files that name a version, not every reader of the change: generated CRDs, chart templates and the admission policies above read the served set without importing Go.
Treat them as floors.

## Collisions

**Draft PR #2084 (Q1066)** edits `api/v2alpha1`, `api/v2beta1` and `api/v2` types and the `v2` `ActionsGateway` CRD in both `api/config/crd` and the chart; idle since 2026-10-14.
Whichever of it and Q1167 lands second rebases: if #2084 is revived after Q1167, its `v2alpha1` and `v2beta1` hunks disappear and only the `api/v2` half remains.

## If the deadline needs one PR fewer

Q1263 and Q1167 can fold into one PR once Q1262 has landed: the AGC retype plus the drop.
It builds for the same reasons each half does, and costs one review of roughly 90 mechanical files beside the deletions.
Folding Q1262 in as well makes a single PR of about 200 retyped files plus the drop, which this plan does not recommend for review.
