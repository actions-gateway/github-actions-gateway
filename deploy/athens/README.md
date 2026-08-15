# Athens — in-cluster Go module cache (dogfood)

Athens proxies `proxy.golang.org` so dogfood CI workers can resolve Go modules without external egress.
It is a **cache**: a miss is fetched upstream and re-populated, so correctness never depends on the cache surviving.
This is dogfood/dev infrastructure, not a shipped product component.

The one thing that varies is **where the cache lives**, which is a cost vs. first-run-latency trade-off.

## Storage options

| Variant | Render | Cache on scale-to-zero | Cost at rest |
|---|---|---|---|
| **Ephemeral** (default) | `kubectl apply -k deploy/athens` | Discarded — cold on next wake | **$0** |
| **Persistent** | `kubectl apply -k deploy/athens/overlays/persistent` | Survives — warm on next wake | ~$2/mo (20 GB Balanced PD, billed while the PVC exists) |

The dogfood cluster idles its system node pool to zero, so with the **ephemeral** default the Athens pod is removed on every idle cycle and the first `vendor-check`/`tidy-check` after a wake re-downloads modules from `proxy.golang.org` (tens of seconds), warming again for the rest of the session.

Choose **persistent** when you dogfood daily and the warm cache is worth the standing disk — persistence is also what enables the "pre-populated / air-gapped / survives-upstream-outages" properties the caching-mirror pattern advertises in [`docs/operations/security-operations.md`](../../docs/operations/security-operations.md).
`scripts/dogfood/setup.sh` selects it via `ATHENS_PERSISTENT=1`.

## Reclaiming the disk during long idle stretches

The disk bills continuously while the PVC exists — deleting the pod or scaling the cluster to zero does **not** stop it.
To stop paying, delete the PVC:

```bash
kubectl delete pvc athens-storage -n gag-dogfood
```

The `standard-rwo` StorageClass uses the GKE default `Delete` reclaim policy, so removing the PVC deletes the backing PD.
Re-render the persistent overlay to recreate a fresh (empty) cache next time.
