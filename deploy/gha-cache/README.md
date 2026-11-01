# In-cluster `actions/cache` server

One [falcondev-oss/github-actions-cache-server](https://github.com/falcondev-oss/github-actions-cache-server) per tenant, in the tenant's namespace, serving `actions/cache` and BuildKit's `type=gha` cache to that tenant's workers.
The server takes each job's cache scope from GitHub's signed runtime token, so a fork pull request can write only its own `refs/pull/<n>/merge` scope.
The design and its trust model are in [q215-worker-cache.md](../../docs/plan/q215-worker-cache.md).

Workers reach it only when they run the `cache-runner` image with `ACTIONS_RESULTS_URL` set to the server.
A worker on any other image ignores the server and keeps using GitHub's cache.

## Layout

| Path | What it is |
|---|---|
| `base/` | The server Deployment, its Service, and two NetworkPolicies. Namespace-free: never apply it on its own. |
| `components/persistent/` | Swaps the base's `emptyDir` for a 16 GiB PVC so the cache survives a pod restart. |
| `example/` | The tenant kustomization to copy: it sets the namespace and writes it into the server's `API_BASE_URL`. |

## Deploy for a tenant

1. Copy `example/` and set `namespace` to the tenant's namespace.
   Keep the `replacements` block in that file: kustomize sets the namespace after the base is built, so the replacement only works there.
2. Optionally uncomment the `components` entry to keep the cache across restarts.
3. Render and apply it:

   ```bash
   kubectl apply -k path/to/your/copy
   ```

4. In the tenant's runner template, set the worker image to `ghcr.io/actions-gateway/cache-runner`, pinned by digest from the release notes, and set `ACTIONS_RESULTS_URL` on the runner container to `http://gha-cache.<namespace>.svc.cluster.local:3000/`.

## What the defaults do

- **Eviction:** entries unused for 7 days are deleted, matching GitHub, and the server's total is capped at 10 GiB, evicted least-recently-read first.
  GitHub caps 10 GB per repository; this server can only cap its own total.
- **Network:** workload pods may reach the server on port 3000, and nothing else may.
  The server's own egress is open, because it verifies every token against GitHub's keys and forwards non-cache calls, artifacts among them, to GitHub.
- **Security:** token validation is on, the management API is off, and the pod runs non-root with a read-only root filesystem.
