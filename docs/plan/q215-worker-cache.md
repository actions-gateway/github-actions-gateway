# In-Cluster Worker Cache — Q215

> **Status: design in progress, spike not yet run.** Revived 2026-10-27 on a tenant's request for an in-cluster cache.
> The primary case is the untrusted-PR tenant, where a shared cache is a vector for compromise by cache poisoning.
> The same design is meant to serve a trusted tenant's `actions/cache` and a Docker build's layer cache, on every profile in [reference-architectures.md](../operations/reference-architectures.md).
> The umbrella goal and its definition of done are in [caching-and-worker-storage.md](caching-and-worker-storage.md); this file is the design that goal calls for.

## Goal

Give a job an in-cluster `actions/cache` that an untrusted fork pull request can use without being able to plant content a trusted job later restores.

## What GitHub already does, and what this has to reproduce

GitHub's cache enforces one boundary on the server, the **ref scope**, and trusts the client for everything else.
Read from [GitHub's dependency-caching reference](https://docs.github.com/en/actions/reference/workflows-and-actions/dependency-caching) on 2026-10-27:

- A run restores caches from its current branch or the default branch, and a pull request run also from its base branch.
  It never restores from a child or sibling branch, or from another tag.
- A pull request run writes to `refs/pull/<n>/merge`, which only re-runs of that pull request can restore.
  The base branch and other pull requests cannot.
- Only `push`, `schedule` and the other listed triggers write to the default branch's scope; every other event resolving to it reads only.
- Entries unused for 7 days are evicted, and a repository is capped at 10 GB, evicted oldest-access first.

The attack this boundary does not stop is code execution *inside* a default-branch context: a `pull_request_target` workflow that checks out fork code can write the default-branch scope, and keys and versions are client-supplied, so a planted entry can match a future key ([Khan, 2024](https://adnanthekhan.com/2024/05/06/the-monsters-in-your-build-cache-github-actions-cache-poisoning/)).
That article also measured the cache token valid for six hours and not revoked at job end.

So an in-cluster cache is no weaker than GitHub's if it enforces the same ref scope **on the server**, and it can be stronger on token lifetime.

## How a job finds the cache

Source reads, each pinned to the commit read:

- **The runner** (`actions/runner` 2.338.0, `1ba885c`) copies the job payload's `SystemVssConnection` `ResultsServiceUrl` into `ACTIONS_RESULTS_URL`, and sets `ACTIONS_CACHE_SERVICE_V2` from the `actions_uses_cache_service_v2` variable, for node and container actions only (`NodeScriptActionHandler.cs`, `ContainerActionHandler.cs`).
  Its own log and step-summary uploads read a different variable, `system.github.results_endpoint`.
- **`@actions/cache`** 6.3.0 (`actions/toolkit` `0c3a9c3`) speaks v2 to `ACTIONS_RESULTS_URL` as three Twirp calls on `github.actions.results.api.v1.CacheService`: `CreateCacheEntry`, `FinalizeCacheEntryUpload`, `GetCacheEntryDownloadURL`.
  It treats a `cache write denied:` refusal as a non-fatal warning and a `cache read denied:` one as a miss.
- **`@actions/artifact`** uses the **same** `ACTIONS_RESULTS_URL`, on `ArtifactService`.
- **BuildKit's `type=gha` cache** (`moby/buildkit` `892eaea`) takes the same URL as its `url_v2` attribute, so a Docker build's layer cache speaks the same protocol as `actions/cache`.
- **The runner exports `actions_cache_mode`** as `ACTIONS_CACHE_MODE` (`none`, `read`, `write`, `write-only`), and the client skips reads or writes it forbids.
  That is advisory: hostile code ignores its own environment.

The captured live payload (`cmd/agc/internal/provisioner/testdata/job_payload.json`) carries `ResultsServiceUrl: https://results-receiver.actions.githubusercontent.com/`, `actions_uses_cache_service_v2: true`, and the job's `event_name`, `ref`, `repository_id` and event body.

**The stock runner cannot be repointed from outside.** A self-hosted cache server has to ship a forked runner or patch `Runner.Worker.dll` ([falcondev-oss's setup guide](https://gha-cache-server.falcondev.io/getting-started), read 2026-10-27).
GAG does not need either in classic mode: the gateway acquires the job and holds its payload before the worker sees it, so it can rewrite `ResultsServiceUrl`.
In scale-set mode the runner acquires its own job, and there is no such point.

## Proposed design

Not yet confirmed by the spike below.

1. **A per-tenant results router.** The gateway rewrites `ResultsServiceUrl` to a router in the tenant namespace.
   The router serves `CacheService` from an in-cluster backend and forwards every other path to GitHub's results service, so artifacts keep working.
2. **Scope comes from the gateway, never from the job.** At provision the gateway computes the job's scope set from the payload, using GitHub's rules above, and registers it with the backend against a digest of that job's runtime token.
   The backend ignores any scope the client sends.
3. **The credential lives as long as the job.** The gateway revokes the registration when the job completes.
4. **One backend per tenant.** No cross-tenant read path exists to test around.

### The write path is a contract

The analogue of [q408 §3.5](q408-untrusted-pr-egress.md#35-the-mirror-role-is-a-contract), whose read-only property this role cannot have:

1. **Selectable by NetworkPolicy**, as the mirror is.
2. **Scope from the gateway.** Every read and write is checked against the registered scope set for the caller's token; an unregistered token gets nothing.
3. **Writes land only in the job's own scope.** A fork pull request writes `refs/pull/<n>/merge` alone; the default-branch scope is writable only by GitHub's listed triggers.
4. **Job-lifetime credentials.** A token is refused once its job has completed.
5. **Tenant-local storage.** The backend holds one tenant's entries.

The residual is GitHub's own: a workflow that runs fork code in a default-branch context can write that scope.
This design reproduces the boundary and does not claim more.

## The spike

What it measures, on a kind cluster against real GitHub, with a throwaway gateway patch on a branch that is never merged:

- **S1, routing.** With `ResultsServiceUrl` rewritten to a router, does `actions/cache` save and restore against the in-cluster backend, and do `upload-artifact`/`download-artifact` still succeed through the forward?
  Refuted if a cache call reaches GitHub, or an artifact step fails.
- **S2, the trust inputs.** What do a live payload's `actions_cache_mode`, `event_name` and `ref` read on the spike's job?

A fork pull request reading needs a fork and is out of the spike.

## Open decisions

- **Eviction.** Matching GitHub's 7 days and 10 GB per repository avoids surprising tenants; nothing is scoped yet.
- **Who pays for storage.** A backend in the tenant namespace consumes the tenant's `ResourceQuota`; one outside it is platform capacity a tenant can fill.
- **Scale-set mode.** No payload rewrite point exists there.
