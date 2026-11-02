# In-cluster `actions/cache` server

> **Audience:** Tenant operator, Platform engineer

A tenant's jobs can keep their `actions/cache` entries, and BuildKit's `type=gha` layer cache, on a cache server in the tenant's own namespace instead of on GitHub.
This matters most for a tenant that runs untrusted fork pull requests, where a cache shared between jobs is a way to plant content that a trusted job later restores.

The server is [falcondev-oss/github-actions-cache-server](https://github.com/falcondev-oss/github-actions-cache-server) (MIT), deployed from [`deploy/gha-cache/`](https://github.com/actions-gateway/github-actions-gateway/tree/main/deploy/gha-cache).
It takes each job's cache scope from the runtime token GitHub signs for that job, so it applies GitHub's own rule: a fork pull request can write only to its own `refs/pull/<n>/merge` scope, which the base branch and other pull requests never restore from.

**Scope:** v2 `RunnerSet` workers, one cache server per tenant.
Sharing one server between tenants is not supported.

## How a job reaches the server

Three pieces, and a job uses the server only when all three are in place:

1. **The server**, a Deployment and Service named `gha-cache` in the tenant namespace.
2. **The `cache-runner` worker image**, `ghcr.io/actions-gateway/cache-runner`.
   The stock GitHub runner overwrites `ACTIONS_RESULTS_URL` with GitHub's address on every job, so no pod setting can redirect it.
   `cache-runner` is the [`build-runner` image](release.md#the-worker-images-wrapper-worker-build-runner-and-cache-runner) with that overwrite disabled.
3. **`ACTIONS_RESULTS_URL`** on the runner container, set to the server's address.

A worker on any other image ignores the server and keeps using GitHub's cache.
A worker on `cache-runner` without `ACTIONS_RESULTS_URL` loses both cache and artifacts, because that one URL carries both.
Artifacts still reach GitHub: the server forwards every call it does not serve itself.

## Deploy it for a tenant

1. Copy [`deploy/gha-cache/example/`](https://github.com/actions-gateway/github-actions-gateway/tree/main/deploy/gha-cache/example) and set `namespace` to the tenant's namespace.
   Keep the `replacements` block in that copy.
   It writes the namespace into the server's own address, and kustomize applies the namespace too late for the replacement to work anywhere else.
2. To keep the cache across a server restart, uncomment the `components` entry, which swaps the default `emptyDir` for a 16 GiB `PersistentVolumeClaim`.
   The claim is billed while it exists and counts against the tenant's storage quota.
3. Apply it:

   ```bash
   kubectl apply -k path/to/your/copy
   ```

4. Point a `RunnerTemplate` at the server, replacing `team-a` with the tenant namespace and the digest with the one from the release notes of the version you run:

   ```yaml
   apiVersion: actions-gateway.com/v2
   kind: RunnerTemplate
   metadata:
     name: cache-runner
     namespace: team-a
   spec:
     workerImage: ghcr.io/actions-gateway/cache-runner@sha256:<digest from the release notes>
     podTemplate:
       spec:
         containers:
           - name: runner
             env:
               - name: ACTIONS_RESULTS_URL
                 value: http://gha-cache.team-a.svc.cluster.local:3000/
   ```

5. Point the tenant's `RunnerSet` at it with `templateRef: {name: cache-runner, kind: RunnerTemplate}`.

Use the full `svc.cluster.local` name.
The worker's default `NO_PROXY` exempts `svc.cluster.local` and nothing shorter, so a bare `gha-cache` would be sent to the egress proxy.

## Check it works

Run a workflow that saves and then restores a cache on the tenant's runners, and read three things:

- **The job log.** `actions/cache/save` prints `Cache saved with key: …` and a later restore prints `Cache hit for: …`.
- **GitHub's cache.** It should hold no entry for that key: `gh cache list --repo <owner>/<repo> --key <key>` prints nothing.
- **The server's counters.** The server admits only worker pods, so read them through the kubelet:

  ```bash
  kubectl -n team-a port-forward svc/gha-cache 13999:3000
  ```

  Then, from another shell:

  ```bash
  curl -s http://127.0.0.1:13999/metrics | grep ^cache_
  ```

  `cache_uploads_total` and `cache_requests_total{result="hit"}` should count your runs.

## What the defaults do

- **Eviction.** An entry unused for 7 days is deleted, as on GitHub.
  The server's total is capped at 10 GiB, evicting the least recently read entry first.
  GitHub caps 10 GB per repository, which this server cannot express, so the cap covers every repository the tenant runs.
- **Network.** Worker pods may reach the server on port 3000, and no other pod may, nor the API server's service proxy.
  The server's own egress is open, because it checks every token against GitHub's signing keys and forwards non-cache calls to GitHub.
- **Security.** Token validation is on and the management API is off.
  The pod runs as a non-root user with a read-only root filesystem and no capabilities.

## Limits

- **It reproduces GitHub's rule, and GitHub's gap with it.** A workflow that runs fork code in a default-branch context, such as `pull_request_target` checking out the fork, can write the default branch's scope, here exactly as on GitHub.
- **The server sees every job's runtime token.** It forwards non-cache calls to GitHub with the job's own token, so whoever compromises the server holds those tokens until they expire.
- **Downloads take no token.** A download URL is protected only by a random identifier, which the server hands out to a job that passed the token check.
- **The `cache-runner` image is a patched GitHub runner.** It changes one string in the runner's own code, and its build fails if a runner upgrade moves that string.
  It goes away once GitHub's runner lets an operator set `ACTIONS_RESULTS_URL` itself.

## Validation

Measured against real GitHub on 2026-11-01, falcondev 10.0.0 deployed from this base, first on kind and then on GKE under Kata workers with the untrusted-PR egress profile: a save and a restore on two worker pods, an artifact through the server's forward, and a restore from a sibling branch that the server refused.
On kind, a pull request from a fork saved under its own `refs/pull/<n>/merge` scope and read its base branch's entries, and the base branch could not restore what it saved.
The details are in the design ([kind](../plan/q215-worker-cache.md#the-v1000-re-run-2026-11-01), [Kata](../plan/q215-worker-cache.md#the-kata-run-2026-11-01), [fork](../plan/q215-worker-cache.md#the-fork-pull-request-run-2026-11-01)).

Under that profile a worker can still reach GitHub's own cache storage, because Azure blob storage is in the address ranges GitHub requires runners to reach.
The server keeps cache traffic in the cluster; it does not make GitHub's cache unreachable.

Not yet measured: BuildKit's `type=gha` cache, which uses the same address but has not been run against it.

## Troubleshooting

| What you see | Why |
|---|---|
| Saves land in GitHub's cache, not the server | The worker is not running `cache-runner`, or `ACTIONS_RESULTS_URL` is unset on the `runner` container. |
| A restore prints `Failed to restore`, and artifact steps fail too | The worker cannot reach the server. Check the address is the full `svc.cluster.local` name, and that both NetworkPolicies from the base are present. |
| `cache write denied` as a warning in a save step | The job's token grants no write to that scope, as for a fork pull request writing outside its own scope. This is the server enforcing GitHub's rule. |
| The server pod restarts and the cache is empty | The default `emptyDir` is cleared on restart. Use the persistent component. |

## Related

- [Runner template library](runner-template-library.md): the other worker images and pod shapes
- [Kata DinD / image-build workloads](kata-dind-workloads.md): the isolation untrusted pull requests run under
- [Reference architectures](reference-architectures.md): the deployment profiles
- [ResourceQuota sizing](resourcequota-sizing.md): what the server and its claim cost a tenant
