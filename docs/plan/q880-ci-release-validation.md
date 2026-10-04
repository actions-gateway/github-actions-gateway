# Q880 — validate release candidates in CI

The dogfood gate that stands between a release candidate and a stable tag runs on a maintainer's Mac today.
Its verdict reaches `publish.yml` as `refs/validated/<rc-tag>`, a ref anyone who can push a tag can also push, so it records that the gate was reported to and never that it passed ([release.md](../operations/release.md#the-gate-records-its-verdict-and-publish-reads-it)).
Running the same gate as a workflow on the candidate tag makes the verdict a job result on that tag's commit: auditable, and keyed to the tag rather than to whoever ran it.

## Status

| # | Milestone | Status |
|---|---|---|
| 1 | Keyless CI identity on the dogfood project | ⚠️ Bootstrap run 2026-10-04; the probe read the cluster and was refused pod creation, but reported the refusal as a failure, fixed here and to be re-run |
| 2 | The gate runs on a Linux runner with no keychain | ⚠️ The App-key half is in with milestone 1; the rest is unmeasured |
| 3 | A workflow runs the gate on each `v*-rc.*` tag | ❌ |
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
[`dogfood-identity-probe.yml`](../../.github/workflows/dogfood-identity-probe.yml) is its acceptance check: it lists namespaces, and it fails if the service account can create a pod, so a grant wider than the milestone asked for goes red rather than passing silently.

To finish milestone 1, from a machine with owner access to the project and admin on the repository.
The project, cluster and zone below are the examples `validate-release.sh` documents; use the ones you run the gate with.

```bash
PROJECT=actions-gateway-dogfood CLUSTER=gag-dogfood ZONE=us-east1-b \
REPO=actions-gateway/github-actions-gateway REVIEWERS=karlkfi \
  scripts/dogfood/ci-identity-setup.sh
```

```bash
gh workflow run dogfood-identity-probe.yml --repo actions-gateway/github-actions-gateway --ref main
```

The run waits for the environment's reviewer, then must go green.

**GKE words a refusal with its reason attached**, `no - requires one of ["container.pods.create"] permission(s) in Cloud IAM …`, measured on the first probe run on 2026-10-04.
The probe compared the whole answer to `no`, so a correct refusal failed; it now compares the first word, and anything else, an error included, still fails.

## 2. The gate on a Linux runner

The gate was written for macOS, and two of its assumptions are known to break on a hosted runner.

- **The GitHub App key.** `setup.sh` rebuilds the `github-app-v1` Secret from the macOS keychain on every run and required `security` unconditionally.
  `GAG_APP_KEY_FROM_CLUSTER=1` now keeps the Secret already in the cluster instead, after checking it names the App and installation the run was given, and fails when it is absent or names others.
  The Secret survives the gate's scale to zero nodes, so a run with no keychain can use it.
  Rotating the key still needs one keychain run.
- **Local tools.** The gate checks for `gcloud`, `kubectl`, `gke-gcloud-auth-plugin`, `helm`, the pinned `cosign` and more before it spends anything.
  Which of these a hosted runner already carries is unmeasured; the workflow in milestone 3 installs whatever is missing, pinned.

## 3. The workflow

A `push: tags: ['v*-rc.*']` workflow runs `validate-release.sh <tag>` with `ASSUME_YES=1` and `GAG_APP_KEY_FROM_CLUSTER=1`, inside the `dogfood-validation` environment.
The gate runs for the better part of an hour, and its e2e leg dispatches `e2e-test.yml` itself, which `GITHUB_TOKEN` can do with `actions: write`.

**The runner is GitHub-hosted, never dogfood.** The gate scales the dogfood cluster up from zero, redeploys the gateway the dogfood runners depend on, and then drives e2e through them, so a job running on those runners would redeploy its own host.

The service account's roles widen here, and the probe's write assertion moves with them.
The candidates are `roles/container.admin` for the node-pool resizes, `clusters update`, and the Helm and CRD writes, and `roles/compute.viewer` for the quota read and the managed instance group listing.
That list is derived from the `gcloud` calls the dogfood scripts make, not from a run, so the first CI run is what confirms it.

## 4. Publish reads the CI verdict

`publish.yml`'s `validated-candidate` job reads `refs/validated/` today ([check-validated-candidate.sh](../../scripts/release/check-validated-candidate.sh)).
Once milestone 3 has run on a real candidate, it reads the workflow's conclusion on that tag's commit instead, and the marker and its recorder retire.
Until then the local gate and the marker stay the release path, so nothing about how a release is cut changes before milestone 4 lands.
