# Q880 — validate release candidates in CI

The dogfood gate that stands between a release candidate and a stable tag runs on a maintainer's Mac today.
Its verdict reaches `publish.yml` as `refs/validated/<rc-tag>`, a ref anyone who can push a tag can also push, so it records that the gate was reported to and never that it passed ([release.md](../operations/release.md#the-gate-records-its-verdict-and-publish-reads-it)).
Running the same gate as a workflow on the candidate tag makes the verdict a job result on that tag's commit: auditable, and keyed to the tag rather than to whoever ran it.

## Status

| # | Milestone | Status |
|---|---|---|
| 1 | Keyless CI identity on the dogfood project | ✅ Bootstrap run 2026-10-04; the probe passed on its second run (37230799055), after #2028 fixed how it read GKE's refusal |
| 2 | The gate runs on a Linux runner with no keychain | ⚠️ Code in review; unproven until the first CI run |
| 3 | A workflow runs the gate on each `v*-rc.*` tag | ⚠️ Code in review; needs `ci-identity-setup.sh` re-run for the new pin and grant |
| 4 | `publish.yml` reads the workflow's verdict instead of `refs/validated/` | ❌ |

## 1. Keyless CI identity

CI has no access to the dogfood project, and a service-account key is the wrong way to give it some: this project ships no-PEM workload identity as a feature.
So the job proves who it is with its own GitHub OIDC token, exchanged through Workload Identity Federation for a short-lived token of one service account.

[`ci-identity-setup.sh`](../../scripts/dogfood/ci-identity-setup.sh) sets it up, and a maintainer runs it once.
It is idempotent, and it draws the trust boundary on the GCP side, where no repository setting can loosen it:

- The pool's provider accepts a token only when it carries this repository's numeric id, its owner's numeric id, `environment: dogfood-validation`, and a `job_workflow_ref` naming the probe workflow on `main`.
  Ids rather than the `owner/name` slug, so a renamed or re-created repository is a different principal.
  The workflow ref pins both the file and the branch, so another workflow on `main`, or a `v*-rc.*` tag cut from any commit, gets no token until milestone 3 names the gate's workflow.
- Only principals carrying that environment claim may impersonate the service account.

A fork, a pull request, or any other workflow gets no token at all.

The `dogfood-validation` GitHub environment is a second layer.
It admits `main` and tags matching `v*-rc.*`, and its jobs wait for a named reviewer.
The script creates it before any GCP write, because a workflow that names a missing environment creates it with no protection rules, and it fails on any ref policy beyond those two.

**The reviewer is a click, not a second person.** The reviewer named in the command below is `karlkfi`, the script allows self-review so a lone reviewer can approve their own dispatch, and every agent session on the maintainer's machine runs `gh` as that account.
Whether that token can approve a pending deployment through the API is unmeasured.
This matters little in milestone 1, whose grant is read-only; it is milestone 3's risk, when the grant can scale and redeploy the cluster, and the workflow pin above is what bounds it rather than the approval.

**Service-account impersonation rather than direct resource access.** Federated principals can hold IAM roles directly, without a service account in between.
Whether `kubectl` authenticates to GKE as such a principal is unmeasured here, and the impersonation path is the one `google-github-actions/auth` and `get-gke-credentials` document together, so milestone 1 takes it.

**Grants follow the milestone.** Milestone 1 grants `roles/container.viewer` and nothing else.
A probe workflow was its acceptance check: it listed namespaces and failed if the service account could create a pod.
It retired with milestone 3, when the provider's pin moved to the gate's workflow.

Run the setup, and re-run it whenever its grant or pin changes, from a machine with owner access to the project and admin on the repository.
The project, cluster and zone below are the examples `validate-release.sh` documents; use the ones you run the gate with.

```bash
PROJECT=actions-gateway-dogfood CLUSTER=gag-dogfood ZONE=us-east1-b \
REPO=actions-gateway/github-actions-gateway REVIEWERS=karlkfi \
  scripts/dogfood/ci-identity-setup.sh
```

**GKE words a refusal with its reason attached**, `no - requires one of ["container.pods.create"] permission(s) in Cloud IAM …`, measured on the first probe run on 2026-10-04.
The probe compared the whole answer to `no`, so a correct refusal failed; #2028 made it compare the first word, so anything else, an error included, still failed.

## 2. The gate on a Linux runner

Three of the gate's assumptions break on a hosted runner, read off the scripts on 2026-10-04 rather than measured in a run.

- **The GitHub App key.** `setup.sh` rebuilds the `github-app-v1` Secret from the macOS keychain on every run.
  `GAG_APP_KEY_FROM_CLUSTER=1` keeps the Secret already in the cluster instead, after checking it names the App and installation the run was given, and fails when it is absent or names others.
  The Secret survives the gate's scale to zero nodes, so a run with no keychain can use it; rotating the key still needs one keychain run.
- **Repository variables.** The gate sets `vars.GAG_RUNNER` on bring-up and resets it and `vars.GAG_E2E_RUNNER` on teardown, and a workflow's `GITHUB_TOKEN` has no permission that writes repository variables.
  `unit-test.yml` and `integration-test.yml` now take the per-run `runner` input `e2e-test.yml` already had, `start.sh` routes its dispatches through it, and `GAG_ROUTE_VARS=0` skips the writes in `start.sh`, `stop.sh` and `e2e-stop.sh`.
- **The App's installation id.** The gate resolves it through `/orgs/<org>/installations`, which needs an org-scoped token.
  `ci-identity-setup.sh` resolves it where `gh` has that access and publishes it as an environment variable, and the workflow passes it in as `INSTALLATION_ID`.

The tools come from pinned actions (`setup-gcloud` with `kubectl` and `gke-gcloud-auth-plugin`, `setup-helm`) and `make cosign`.

## 3. The workflow

[`validate-candidate.yml`](../../.github/workflows/validate-candidate.yml) runs `validate-release.sh <tag>` on a GitHub-hosted runner inside the `dogfood-validation` environment, never on dogfood: the gate scales the dogfood cluster from zero and redeploys the gateway those runners depend on.

**A tag push dispatches the gate on `main`; it never runs the gate itself.** The provider accepts a token only from this workflow on `refs/heads/main`.
A push of a `v*-rc.*` tag runs the tag's own copy of the file, and a tag can be cut from any commit, so trusting the tag would let anyone who can push one run a modified workflow with the gate's grant.
The push job holds no cloud access and only dispatches `validate` on `main` with the tag as input, so the gate always runs `main`'s scripts against the tag's signed artifacts, as the local gate does.

**The grant is `roles/container.admin` and `roles/compute.viewer`**, approved by the maintainer on 2026-10-04.
The gate resizes node pools, updates the cluster, installs the GMC's Helm release (ClusterRoles, webhooks, CRDs) and reads the project's CPU quota and instance groups.
No narrower GKE role set that still writes ClusterRoles was measured.
The list is derived from the calls the dogfood scripts make, so the first CI run is what confirms it.
The milestone 1 probe retires with this change: the provider now names the gate's workflow, so the probe can no longer get a token.

**The lease is host-local, so the workflow carries its own reclaim.** `lib/lease.sh` records a pid on the host that ran the gate, so a CI run's lease is invisible to `--reclaim` anywhere else and vanishes with the runner.
The gate step's limit is shorter than the job's, and a final step runs `--reclaim` on the same runner after a failure or cancellation, while the lease is still there to read.
A `concurrency` group keeps two CI runs off the cluster at once; a CI run and a local gate still cannot see each other, which [Q1158](../queue/Q1158.md) tracks.

## 4. Publish reads the CI verdict

`publish.yml`'s `validated-candidate` job reads `refs/validated/` today ([check-validated-candidate.sh](../../scripts/release/check-validated-candidate.sh)).
Once milestone 3 has run on a real candidate, it reads the workflow's conclusion on that tag's commit instead, and the marker and its recorder retire.
Until then the local gate and the marker stay the release path, so nothing about how a release is cut changes before milestone 4 lands.
