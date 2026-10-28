# In-Cluster Worker Cache — Q215

> **Status: design settled and spiked 2026-10-27; the opt-in `cache-runner` image is built, nothing else is.** Revived on a tenant's request for an in-cluster cache.
> The primary case is the untrusted-PR tenant, where a shared cache is a vector for compromise by cache poisoning.
> The same design serves a trusted tenant's `actions/cache` and a Docker build's layer cache, on every profile in [reference-architectures.md](../operations/reference-architectures.md).
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

So an in-cluster cache matches GitHub's ref boundary if it enforces the same scope **on the server**, from something the job cannot forge.
That holds for the cache entries themselves; the transport that carries their bytes is weaker than GitHub's, as [the contract's residuals](#the-write-path-is-a-contract) say.

## The scope comes from GitHub's own token

Every action receives `ACTIONS_RUNTIME_TOKEN`, a JWT GitHub signs.
Its `ac` claim lists the job's cache scopes with a permission each, and its `repository_id` claim names the repository.
The claim is undocumented: one decode in [actions/toolkit#1053](https://github.com/actions/toolkit/issues/1053) (2022) shows `{"Scope":"refs/heads/master","Permission":3}`.

[falcondev-oss/github-actions-cache-server](https://github.com/falcondev-oss/github-actions-cache-server) (MIT) verifies that token against the JWKS at `token.actions.githubusercontent.com`, takes the scope set from `ac`, writes only into a scope whose permission is at least 2, and keys every entry by scope and repository (`lib/scope.ts`, `CreateCacheEntry.post.ts` at v9.8.0).
A client-supplied scope is never read.

That makes the server apply GitHub's policy exactly, including whatever GitHub decides for an event an administrator marks untrusted, with no reimplementation in the gateway.
The spike confirmed the claim is present on a live token: see [the spike](#the-spike-2026-10-27).

## How a job finds the cache

Source reads, each pinned to the commit read:

- **The runner** (`actions/runner` 2.338.0, `1ba885c`) copies the job message's `ResultsServiceUrl` into `ACTIONS_RESULTS_URL` for node and container actions, overwriting whatever the action's environment already held (`NodeScriptActionHandler.cs`, `ContainerActionHandler.cs`).
  Its own log and step-summary uploads read a different variable, `system.github.results_endpoint`, so they never touch this URL.
- **`@actions/cache`** 6.3.0 (`actions/toolkit` `0c3a9c3`) speaks v2 to `ACTIONS_RESULTS_URL` as three Twirp calls on `github.actions.results.api.v1.CacheService`.
  It treats a `cache write denied:` refusal as a non-fatal warning and a `cache read denied:` one as a miss.
- **`@actions/artifact`** uses the same URL, on `ArtifactService`.
- **BuildKit's `type=gha` cache** (`moby/buildkit` `892eaea`) takes the same URL as its `url_v2` attribute, so a Docker build's layer cache speaks the same protocol as `actions/cache`.

### Why the hook is a patched runner

v2, the API new tenants onboard on, is scale-set only: the runner acquires its own job, so the gateway never holds the job message.
The three places GAG does sit in the worker's path cannot change the URL:

- **The pod environment** is overwritten by the runner, per the first bullet above.
- **The egress proxy** is CONNECT-only with no TLS termination (`cmd/proxy/proxy.go`); intercepting a GitHub host there would be a security regression.
- **The classic payload hook** exists only on the deprecated v1alpha1 path.

So the worker image patches the runner.
The `cache-runner` target in the root `Dockerfile` builds on `build-runner` and renames the one UTF-16LE `ACTIONS_RESULTS_URL` literal in `Runner.Worker.dll` to `ACTIONS_RESULTS_GAG`, the patch falcondev-oss documents.
The runner then writes the job's URL under the new name and leaves the pod's `ACTIONS_RESULTS_URL` alone.
Measured on 2.338.1 and 2.338.2: the literal occurs exactly once in `Runner.Worker.dll` and in no other assembly under `/home/runner/bin`.
The build fails unless the literal is in that assembly alone, exactly once, before the patch, and nowhere after it.

The image is opt-in, because a pod running it without `ACTIONS_RESULTS_URL` set loses both cache and artifacts.

**Upstream** is asked for a real override in [community discussion #211436](https://github.com/orgs/community/discussions/211436).
Earlier attempts, [actions/runner#3411](https://github.com/actions/runner/pull/3411) and [#3906](https://github.com/actions/runner/pull/3906), were closed without a maintainer reply.

## Design

1. **A per-tenant cache server**, falcondev's, in the tenant namespace, with token validation on.
   It serves `CacheService` from its own storage and forwards every other results call to `https://results-receiver.actions.githubusercontent.com`, so artifacts keep working.
2. **Workers point at it** by running `cache-runner` with `ACTIONS_RESULTS_URL` set to the server's `svc.cluster.local` name, which the default `NO_PROXY` keeps off the egress proxy.
3. **NetworkPolicy** admits worker pods to the server's port and nothing else to it.
   The server is not labelled `actions-gateway/component=workload`, so the managed default-deny leaves it free egress to GitHub's token keys and results service.

### The write path is a contract

The analogue of [q408 §3.5](q408-untrusted-pr-egress.md#35-the-mirror-role-is-a-contract), whose read-only property this role cannot have:

1. **Selectable by NetworkPolicy**, as the mirror is.
2. **Scope from GitHub's signed token.** The three `CacheService` calls (reserve an entry, finalize it, look one up) are checked against the scopes and repository in a verified `ACTIONS_RUNTIME_TOKEN`; an unverifiable token gets nothing.
   The byte transfers those calls hand out are not, as the residuals below say.
3. **Writes land only in a scope the token grants write.** GitHub grants a fork pull request `refs/pull/<n>/merge` alone, and the default-branch scope only to its listed triggers.
4. **Tenant-local storage.** One server holds one tenant's entries, in the per-tenant mode; the shared modes give this up, as [tenancy modes](#tenancy-modes) says.

Residuals, each read from falcondev v9.8.0 (`c866ae4`) and none measured:

- **The ref boundary's own gap is GitHub's:** a workflow that runs fork code in a default-branch context can write that scope.
  This design reproduces the boundary and does not claim more.
- **The blob transport takes no token.** `PUT /upload/<id>` and `PUT /devstoreaccount1/upload/<id>` read no `Authorization` header, so while an upload is open its ID is the only credential: 10 decimal digits from `customAlphabet` in `lib/helpers.ts`, about 33 bits.
  An unknown ID gets `201` with nothing written, so a guess learns nothing from the reply.
  `GET /download/<id>` is tokenless too, keyed by a random UUID.
  GitHub hands out signed SAS URLs for the same step, so whether a job can plant parts into another job's open upload has to be measured or closed before [Q215](../queue/Q215.md) ships `deploy/gha-cache/`.
- **The server holds every job's runtime token in flight.** It forwards each non-cache results call to GitHub with the caller's bearer token (`routes/[...path].ts`), so compromising the server exposes those tokens until they expire.
- **No job-lifetime credentials.** A token stays valid until its own expiry, as it does against GitHub.

### Tenancy modes

Who pays for the storage follows from where the server runs, and the shapes are the ones the egress proxy already has ([§H.9](../design/appendix-h-v2-api-decomposition.md#h9-cross-namespace-proxy-sharing)):

1. **Per tenant.** The server sits in the tenant namespace and its volume counts against that tenant's `ResourceQuota`.
   This is the mode Q215 ships, and the only one for an untrusted-PR tenant.
2. **Shared between authorized tenants.** One tenant's server admits others it names, with consent on the provider's side as `EgressProxy`'s `spec.sharing.allowedNamespaces` does, and the provider's namespace pays.
3. **Platform-operated.** The server sits in a namespace a cluster tenant owns, not a GAG tenant, granted to whichever GAG tenants the platform team picks.
   Quota, cost accounting and chargeback are then the cluster administrator's, through whatever they already use for that namespace.

Modes 2 and 3 are one mechanism with different owners: a provider namespace plus grants.
Managing it with a CRD shaped like `EgressProxy` would let the GMC write both sides' NetworkPolicy and hand each granted namespace the server's URL, where today an operator hand-wires both; [Q1225](../queue/Q1225.md) holds that.

Sharing gives up more than it does for a proxy.
Read from falcondev v9.8.0 source, not measured:

- **Entries stay apart.** Every entry is keyed by the token's `repository_id`, so one tenant cannot restore another's.
- **Space does not.** `CACHE_MAX_SIZE_BYTES` is one budget for the whole server, evicted least-recently-read first (`enforceStorageBudget` in `lib/storage.ts`), so one tenant's saves can evict another's entries.
  There is no per-repository cap to set.
- **The server accepts a token from any repository GitHub signs.** It checks the signature and the claims, not the owner (`getCacheScope` in `lib/scope.ts`), so the NetworkPolicy grant is the only thing deciding which tenants are callers.
- **The residuals cross tenants.** The tokenless upload and the token custody above span every tenant the server admits.

So, as §H.9 says of a shared proxy, the shared modes are for cooperating tenants.

## The spike (2026-10-27)

A private kind cluster (`gag-q215`, Kubernetes 1.36.1, kindnet) running this branch's gateway images against real GitHub, one v2 tenant registered at repo scope as scale set 27, the `cache-runner` image, and falcondev v9.8.0 pinned by digest with token validation on.
The fixture workflows ran on throwaway branches and were never merged.

**S1, routing: holds.** [Run 40578710381](https://github.com/actions-gateway/github-actions-gateway/actions/runs/40578710381), on push to `spike/q215-cache`, two jobs on two worker pods:

- A run step read `ACTIONS_RESULTS_URL` as `http://gha-cache.q215-cache.svc.cluster.local:3000/`.
- `actions/cache/save@v5` saved key `q215-40578710381-1`, and the server's store gained a 277-byte entry at the same minute.
- `actions/cache/restore@v5` in the second job reported `Cache hit for: q215-40578710381-1`.
- `upload-artifact@v7` and `download-artifact@v8` succeeded through the server's forward, and the restored file matched the downloaded one byte for byte.
- GitHub's own cache held no `q215-` key afterwards, while the same listing returned other entries, so neither the save nor the restore touched it.

**S2, trust inputs: present.** The runner logged `Cache mode: write` for both jobs, and the server stored the entry under scope `refs/heads/spike/q215-cache` and repository `1241076663`, values it can only have taken from the verified token.

**S3, the read boundary: holds for a sibling branch.** [Run 40578988657](https://github.com/actions-gateway/github-actions-gateway/actions/runs/40578988657), on push to `spike/q215-sibling`, asked for the same key with the same path and reported `Cache not found for input keys: q215-40578710381-1`.
The toolkit prints that only when the server answers a lookup with no match; an unreachable server prints `Failed to restore` instead.
The server's `cache_requests_total` read `hit` 1 and `miss` 2 after both runs: the hit is S1's restore and one miss is this lookup; the second miss is unattributed.

**Not measured.** A fork pull request's scope, which needs a fork; BuildKit's `type=gha` cache, which needs a Docker daemon in the job and rides the same URL by source read only; and the tight-egress profile, since kindnet does not enforce egress negatives.

## Open decisions

- **Eviction.** falcondev's defaults are 90 days unused and a volume-percentage cap.
  `CACHE_CLEANUP_OLDER_THAN_DAYS` can match GitHub's 7 days, but `CACHE_MAX_SIZE_BYTES` caps the whole server, so GitHub's 10 GB per repository has no equivalent; a per-tenant server's cap is the tenant's total.
