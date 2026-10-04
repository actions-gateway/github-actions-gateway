# Dragonfly back end for the registry mirrors

A [Dragonfly](https://d7y.io/) scheduler and one seed peer that the five registry mirrors in [`../registry-mirror`](../registry-mirror/README.md#dragonfly-back-end) use as their outbound HTTPS proxy, so content-addressed blobs and manifests are fetched once and shared peer to peer.
No manager, so no MySQL or Redis: the seed peer finds the scheduler through a static dynconfig.

**Never route a worker here.** dfdaemon's proxy forwards to any host it is asked for and forwards pushes, and no setting restricts it (measured on client v1.5.7).
[`networkpolicy.yaml`](networkpolicy.yaml) admits only pods labelled `app=registry-mirror` in `gag-registry-mirror` to port 4001.

**Keep `proxy.registryMirror.cert` set.** Without it dfdaemon accepts any upstream certificate, self-signed and expired included (measured); with the public bundle it names, it verifies the path that tag lookups and tokens take.

| File | What it holds |
|---|---|
| `namespace.yaml` | `gag-dragonfly`, enforcing Pod Security `restricted` |
| `scheduler.yaml` | The scheduler, non-root with a read-only root filesystem |
| `seed-client.yaml` | The seed peer and its proxy rules |
| `networkpolicy.yaml` | Default-deny ingress, then the mirrors to the proxy, peers to peers, and seeds to the scheduler |
| `ca-chart/` | A Helm chart that mints the proxy's interception CA as a Secret here. The start script copies the certificate alone into a ConfigMap in `gag-registry-mirror` |

The CA chart is rendered, never installed, and only when its Secret is absent: every render is a new key.
It is created rather than applied, so the key is not copied into a `last-applied-configuration` annotation.
`scripts/dogfood/e2e-start.sh` does that under `E2E_MIRROR_BACKEND=dragonfly`.
To rotate, delete the Secret and re-run it.
Why this shape and not Dragonfly as the mirror: [q539-dragonfly-mirror-backend.md](../../docs/plan/q539-dragonfly-mirror-backend.md).
