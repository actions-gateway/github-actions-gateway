# Release 1.10 Milestone Definition

> **Status: scoped 2026-10-09, no candidate cut.** The rung was decided by the maintainer on 2026-10-09, when scoping 2.0 found that the storage advance cannot share a CustomResourceDefinition apply with the removals.
> Ten rows gate the tag and four ride; Q1086, the storage advance, has landed, Q1184 joined the gate on 2026-10-25 and landed the same day, and Q1210, Q1211 and Q1212 joined it on 2026-10-26.
> Q1214's fix gated `v1.10.0-rc.2`; its flake-watch soak rides, since it counts green runs on `main` after the fix.
> Nothing here is a commitment to a date.

## Why this release exists

`v2.0.0` removes `v2beta1`, `v2alpha1`, `v1alpha1` and classic acquisition, and every object an operator has stored must be rewritten as `v2` first.
The apiserver will not remove a version from a CRD's `spec.versions` while `status.storedVersions` still lists it, and every 1.x cluster lists `v2beta1` there, so the rewrite has to finish in a state where `v2` is storage and every version is still served.
This release is that state: it marks `v2` the storage version and ships the sweep that rewrites stored objects and prunes `storedVersions` to `["v2"]`, and `v2.0.0` is then removals only, in one apply.

The alternative was a two-phase `v2.0.0` upgrade, apply, sweep, apply, inside one release.
It saved a release and made the one breaking upgrade a procedure an operator can stop halfway through.
Full reasoning: [release-ladder.md](release-ladder.md#why-110-exists-the-storage-advance-cannot-share-an-apply-with-the-removals), and the contract's source in [v2-ga.md](v2-ga.md#the-storage-advance-needs-a-state-between-two-applies).

Rule #4b is not what forces it: `v1.9.0` already served `v2` beside `v2beta1`, which is what allows the storage version to advance here at all.
Rollback from this release reaches `v1.9.0`, which serves `v2` and so reads what this release stored.

## What this release must NOT do

**It must not stop serving any version.** `v1alpha1`, `v2alpha1` and `v2beta1` stay served, so every client and every stored object an operator has keeps working.
Dropping one is [Q1167](../queue/Q1167.md)'s and [Q273](../queue/Q273.md)'s, in `v2.0.0`, after this tag.

**It must not remove classic acquisition.** That is [Q264](../queue/Q264.md), also in `v2.0.0`.

## Scope ledger

Six gating rows and the candidate validation, then the three items that ride.

| Q-ID | Item | Gates? | Status |
|---|---|---|---|
| Q1086 | Mark `v2` the storage version, rewrite every stored object with a read-write sweep, and prune `storedVersions` to `["v2"]` | `1.10-gate` | ✅ landed: all six kinds store `v2`, and `gag-migrate storage-version` rewrites and prunes, proven on envtest |
| Q1171 | RC validation runs the storage sweep and reads `storedVersions` on dogfood | `1.10-gate` | ✅ landed: the gate's `storage` leg applies the chart's `PriorityClassAllowlist` CRD, runs the RC's signed `gag-migrate storage-version`, and fails unless all six CRDs read `["v2"]`. Unit-tested with stubs; not yet run on dogfood |
| Q1152 | Scale-set recovery ties a job to the runner that took it, not the worker created for it | `1.10-gate` | ✅ landed: a `JobStarted` replaces the worker's run identity with the started job's, and start-up recovery finds a job's worker by `started-job-id`. Unit-tested on crossed workers; not reproduced on a cluster |
| Q1153 | Reclaim the idle scale-set worker a cancelled job leaves behind | `1.10-gate` | ✅ landed: a runnerless completion whose own worker is busy stamps the oldest idle `Running` worker, once per job, and a start moves the stamp on. Unit-tested on crossed workers; not reproduced on a cluster |
| Q1154 | Reclaim the JIT-config Secret of a worker reaped before its job completes | `1.10-gate` | ✅ landed: the worker pod becomes the Secret's only owner once created, so the garbage collector deletes it with the pod. Unit-tested with a collector stand-in; not reproduced on a cluster |
| Q1069 | Per-`RunnerSet` egress-audit attribution condition and gauge | `1.10-gate` | ✅ landed: `EgressAuditUnattributed` on each `RunnerSet` and `actions_gateway_runnerset_egress_audit_unattributed{namespace,runner_set,reason}` |
| Q1184 | Attribute egress proxy failures to the job that hit them, through phase 4 | `1.10-gate` | ✅ landed (#2112, #2122, #2125, #2127, #2131): an opted-in namespace's workers move onto a port of their own, and the AGC reads and attributes their failure records, reporting a denial as an Event through Q1210. The Calico e2e lane measures a pod refused on another namespace's port. Q1208's port hold on opt-out landed with it (#2126) |
| Q1210 | The AGC records an Event naming the host when it attributes a policy denial, as `proxyFailureAttribution`'s description says it does | `1.10-gate` | ✅ landed: an `EgressDestinationDenied` Warning Event on the worker's `RunnerSet`, once per job and destination, at most eight a job. Unit-tested against a fake replica; not reproduced on a cluster. Found by the candidate's API pre-flight 2026-10-26 |
| Q1211 | Onboarding pages and shipped runner templates tell a new tenant to write `actions-gateway.com/v2`, not `v2beta1`, which `v2.0.0` removes | `1.10-gate` | ✅ landed: every new-tenant page, the README, the three `deploy/templates/` entries and the generated API reference name `v2`; the dogfood e2e overlays' template patch targets moved with them. Not on the released surface by `check-artifact-unchanged.sh`, but the template library ships outside it |
| Q1212 | Dogfood tenant objects are written at `actions-gateway.com/v2`, so the project runs what onboarding now recommends | `1.10-gate` | ✅ landed: `setup.sh`'s tenant and the e2e tenant's base and overlays author `v2`, and `setup.sh` defaults to `v1.10.0-rc.1` so a defaults run serves it. Kustomize renders and unit tests only; reaches the cluster at the next `setup.sh` |
| [Q1214](../queue/Q1214.md) | An attribution pool that gains a consumer namespace reissues its certificates, so `E2E_V2_ProxyAttribution` stops flaking | rides | ✅ fixed: the GMC writes its owned Secrets by fetch-then-update, under the role it already holds, closing Q1200; envtest as the GMC ServiceAccount fails on the patch path. In flake watch. Found diagnosing the flake 2026-10-26 |
| — | RC validated on dogfood | gates | 🔲 no candidate cut |
| Q1070 | Security-dashboard panel keyed on Q1069's per-set gauge | rides | ✅ landed: a fourth series in the security dashboard's egress-posture timeline, filtered to tenants that turned on at least one half of the pair |
| [Q1066](../queue/Q1066.md) | Registry read presents the worker ServiceAccount's pull secrets, then node-identity credentials for Artifact Registry and ECR | rides | Stage one ✅ landed: the worker ServiceAccount's `imagePullSecrets` stand in when the template names none, under a `get` on `serviceaccounts`. Stage two 🔲 ready |
| Q540 | Validate Kata with Dragonfly at the node: the P2P mesh stays unreachable from worker pods | rides | ✅ landed: the node client ships as `deploy/dragonfly-node/`, validated on dogfood 2026-10-19 with a Kata worker's reachable set unchanged ([plan §6](q540-node-layer-p2p.md#6-dogfood-validation)), and this release's Dragonfly leg is its first full e2e run |

**The split is the maintainer's, chosen 2026-10-09; the reasons below are this plan's, offered with it.** The theme is in the maintainer's own words: the storage advance plus scale-set correctness.
Q1086 is what the release is for, and Q1171 is what shows it worked on a cluster that has run 1.x.
Q1152, Q1153 and Q1154 are defects on the scale-set tier, the one `v2.0.0` keeps as the only tier: recovery that re-runs the wrong run, a worker that holds a node for up to 12 hours, and a credential-bearing Secret that outlives its worker.
Q1152 and Q1154 were found reviewing Q1151's fix, and all three follow from the same fact, that GitHub hands a scale-set job to whichever runner asks first.
Q1069 gates as the one feature: the gateway-level attribution flag reads `0` where a set's traffic leaves through a pool that logs no source address, the wrong direction for an audit signal.

**Q1184 joined the gate on 2026-10-25, through phase 4, by the maintainer's choice.** Phase 2a put its two fields into `v2`, `v2beta1` and `v2alpha1`, and their godoc says they are not yet effective, so tagging before 2b would publish a `v2` field that does nothing for workers.
Phase 3 gives the recorded failures their reader, and phase 4 is the Calico measurement of a worker refused on another namespace's port, which Q1184's row already makes a ship requirement for a security-labelled feature.

**The riders are planned and worked, and the tag does not wait for them.** Each can come back short for a reason a gate should not absorb.
Q540 was a measurement whose answer might have been a fix of unknown size, because [Q539](../plan/q539-dragonfly-mirror-backend.md#8-what-this-plan-does-not-cover) measured Dragonfly's node proxy as an open forward proxy; binding it to loopback was enough.
Q1066's second stage adds a cloud dependency and an identity grant per provider; its first stage has landed, so the second can slip without costing the air-gapped install path.
Q1070 keys on Q1069's per-set gauge, whose `reason` label is what the panel filters on.

`scripts/release/semver-floor.sh v1.9.0` read `FLOOR: NONE` on 2026-10-09 over two commits, so nothing merged yet rides beyond the three above.
The ledger takes a row for each further item that merges onto the released surface before the tag.

## What the candidate must show

The dogfood gate validates every candidate; this one also has to show the storage advance completed on a cluster that has run 1.x.
Q1086 shipped the sweep and proved it on envtest; the candidate is where it is read on a real upgrade: every one of the six kinds' CRDs reports `storedVersions: ["v2"]` after the upgrade and the sweep, on the dogfood cluster, whose objects are stored as `v2beta1` today.
The gate's `storage` leg runs the sweep and takes that reading (Q1171, landed), so the candidate's evidence carries it.
Only the first candidate's run rewrites anything: it leaves dogfood at `["v2"]`, so every later candidate's sweep takes the no-op path, and a sweep fix shipped in a later candidate is never exercised on a real cluster.

## A release-notes edit owed at the candidate

The notes' scale-set caveat reads "None of these five scale-set fixes was reproduced on a cluster.
Each is covered by unit tests."
That is accurate, but it understates Q1169, whose fix also passed the AGC envtest suite, and it does not say what the tests prove.
The proposed wording, for the candidate's prose pass:

> None of these five was reproduced on a cluster, and the release gate does not assert them.
> Each has unit tests that fail with the fix removed, and Q1169's also passed the AGC's envtest suite.

It holds only if all five PRs report an inversion check.
Q1152 (#2059), Q1169 (#2103) and Q1174 (#2101) do.
Read Q1153's (#2062) and Q1154's (#2063) before applying it, and narrow the claim to the three if either does not.
The caveat stays true after dogfood validation, because nothing in the gate builds a crossed worker on purpose.

## Decided: an operator runs the sweep, as `gag-migrate storage-version`

**Decided by the maintainer on 2026-10-09**, choosing between the operator-run command and a sweep the GMC runs itself at startup.
The GMC's ClusterRole had `get` on `customresourcedefinitions` and no write on four of the six kinds, so a GMC-run sweep would have widened a long-lived controller's privileges for a job that runs once per cluster.
The command runs under the credentials that apply the CRDs, ships in the signed `gag-migrate` binary each release already publishes, and keeps that tool's `--context` pin and confirmation prompt.
What it costs is a manual upgrade step an operator can skip; skipping it fails loudly at the `v2.0.0` CRD apply rather than silently, and the [pre-upgrade check](../operations/upgrade.md#before-upgrading-to-v200-every-actions-gatewaycom-crd-stores-only-v2) catches it first.
The sweep refuses to start while any `EgressProxy` names a deprecated alias, so Q1085's check stays a precondition rather than something the sweep works around.
