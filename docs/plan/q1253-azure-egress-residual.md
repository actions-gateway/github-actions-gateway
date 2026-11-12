# Q1253: what GitHub's published ranges let a worker reach on Azure

**Status:** public data and source settled on 2026-11-12; the in-cluster confirmation probe below was written and skipped, so the residual rests on public data and the 2026-11-01 run.
Q1253 closed on that basis, and the narrowing is [Q1350](../queue/Q1350.md).

Q1253 asked whether a storage account nobody at GitHub owns resolves into the ranges a direct-mode worker may reach on 443.
It does, and the admitted set is wider than storage: the `actions` key of GitHub's `/meta` covers at least 97% of Azure's public IPv4 space in 14 regions, East US and West Europe among them.
The residual is recorded in [05-security.md](../design/05-security.md#worker-exfiltration-to-azure-hosted-endpoints-inside-githubs-published-ranges).

## What public data settles

Both inputs were read on 2026-11-12: GitHub's `/meta` through `gh api meta`, and Microsoft's weekly service-tag file `ServiceTags_Public_20261109.json` (change number 426) from `download.microsoft.com`.
The comparison is IPv4 only; the 2,728 IPv6 ranges under `actions` were not compared.

| Measurement | Value |
| --- | --- |
| Ranges under `/meta` `actions` | 8,350 IPv4 and IPv6, 28,648,329 IPv4 addresses |
| Share of those IPv4 addresses inside the `AzureCloud` tag | 99.954%; 13,267 addresses in 172 ranges fall outside it |
| Share of all `AzureCloud` IPv4 addresses inside `actions` | 28,635,062 of 65,446,795, about 44% |
| `AzureCloud.<region>` tags at least 97% inside `actions` | 14 (East US 2 is the lowest, at 97.47%): West US, West US 2, West US 3, West Central US, North Central US, South Central US, Central US, East US, East US 2, East US 3, Canada Central, Mexico Central, West Europe, North Europe; Canada East is at 91.3% |
| `Storage` tag IPv4 addresses inside `actions` | 268,016 of 563,553, 47.6% |
| `Storage.<region>` tags wholly inside `actions` | 14, among them East US 2, Central US, West US, West Europe and North Europe; East US is at 96.7% and 62 regions at 0% |

`AzureCloud.<region>` is Microsoft's tag for a region's public address space, so an address an Azure customer rents in one of those regions (a VM's public IP, an App Service, a storage account) is drawn from inside the admitted set.
That is read from what the tag means, not from renting one.

Storage accounts GitHub does not operate already resolve inside it.
From this workstation's resolver on 2026-11-12, `azureopendatastorage.blob.core.windows.net` answered `20.209.75.225` and `pandemicdatalake.blob.core.windows.net` answered `20.209.163.226`, inside the admitted `20.209.74.0/23` and `20.209.162.0/23` (both `Storage.EastUS`).
Both are Microsoft's public open-dataset accounts.
GitHub's own `productionresultssa0` resolved into `20.209.226.0/23`, and `productionresultssa19` into `20.209.112.0/23` and `20.209.178.0/23`, the accounts [run 41371432425](https://github.com/actions-gateway/github-actions-gateway/actions/runs/41371432425) reached.
A storage account's blob name points at its storage cluster's addresses rather than a per-client answer, so the cluster's resolver should return the same addresses; that is unverified from the cluster.

## What source settles

Direct mode and the CIDR-mode proxy admit the address set above; the FQDN backends admit no CIDRs, but every storage account by name.

- **Direct mode and the CIDR-mode proxy pool.** The GMC merges `/meta` `api`, `actions` and `web` into one CIDR list (`cmd/gmc/internal/controller/ipranges.go:231:	combined = append(combined, meta.Actions...)`) and patches it into both the direct-mode workload policy and the proxy pool's policy, with no per-key filter.
- **The FQDN backends.** `githubEgressFQDNs` names `*.blob.core.windows.net` (`cmd/gmc/internal/controller/egressproxy_fqdn.go:123:	"*.blob.core.windows.net",`), which matches every account by construction.
- **The proxy's CONNECT allowlist**, present only once an operator opts in to extra destinations, carries the same names as suffixes (`proxyAllowlistEnv`, `cmd/gmc/internal/controller/egressproxy_builder.go`), so `blob.core.windows.net` passes any account there too.

Run 41371432425's logs show only GitHub's two accounts answering HTTP 400 and `example.com` timing out; it probed no account outside GitHub's, which is why the row called the question unmeasured.

## The probe

The probe confirms what the sections above derive, that a Kata worker under the e2e tenant's direct-mode policy completes a TLS request to a storage account GitHub does not operate.
It needs no Azure account: the two open-dataset accounts above are third-party targets that already exist, and an anonymous `GET /` on any account answers HTTP 400 once the connection is through.
It does not test an upload, which a NetworkPolicy cannot see either way.

It runs on the dogfood cluster, which is production, so running it is a maintainer step.

1. Bring the e2e tenant up with the Kata overlay, as for [Q215's Kata run](q215-worker-cache.md#the-kata-run-2026-11-01): `scripts/dogfood/e2e-start.sh` with `deploy/dogfood-e2e/overlays/kata`.
   This books the e2e pool, which nothing else can use while it is up.
2. Push a branch `spike/q1253-probe` from `origin/main` carrying `.github/workflows/q1253-probe.yml` below, and nothing else.
   The push starts one job on a `gag-ci-e2e` worker; it takes under a minute.
3. Read the job log.
   Each line is `ok` or `FAIL`, and the job exits non-zero on any `FAIL`.
   `third-party-blob` answering a status is the residual confirmed; `-` there, with the GitHub control open, would mean the cluster resolves or routes those names differently from this workstation, and the 05-security entry would need re-deriving.
4. Delete the branch and stop the e2e tenant (`scripts/dogfood/e2e-stop.sh`).

```yaml
# Q1253 probe, temporary: lives only on spike/q1253-probe.
name: q1253-probe
on:
  push:
    branches: ["spike/q1253-probe"]
permissions:
  contents: read
jobs:
  probe:
    runs-on: gag-ci-e2e
    steps:
      - name: egress probes
        run: |
          set -u
          uname -r
          fail=0
          check() { # name url want(open|blocked)
            local host="${2#*://}"; host="${host%%[:/]*}"
            getent hosts "$host" || { echo "FAIL $1: $host does not resolve"; fail=1; return; }
            local got; got="$(curl -s -o /dev/null -w '%{http_code}' --max-time 20 "$2" || true)"
            [[ -n "$got" && "$got" != 000 ]] || got=-
            if [[ "$3" == open && "$got" != - ]] || [[ "$3" == blocked && "$got" == - ]]; then
              echo "ok   $1: $got"; else echo "FAIL $1: $got (want $3)"; fail=1; fi
          }
          check github-api https://api.github.com/ open
          check github-blob https://productionresultssa0.blob.core.windows.net/ open
          check third-party-blob https://azureopendatastorage.blob.core.windows.net/ open
          check third-party-blob-2 https://pandemicdatalake.blob.core.windows.net/ open
          check internet https://example.com/ blocked
          exit "$fail"
```

`uname -r` prints the guest kernel, which is how Q215's run told a Kata worker from a runc one.
The `getent` line prints the address each name resolved to in the cluster, which answers the one inference above that was not measured.

A stronger probe would point a worker at a listener on an Azure VM in East US 2, which tests the `AzureCloud` reading rather than the storage one.
That needs an Azure subscription and costs a VM for the length of the run; the storage probe answers the row's question without either.

The EgressProxy leg the row asked for is settled by source: in CIDR mode the proxy pool's policy is patched from the same CIDR list, and in FQDN mode the wildcard admits any account.
Probing it needs a tenant with an `EgressProxy`, which the e2e tenant does not carry.

## Can the admitted set be narrowed

**The FQDN backends can, from source.** `/meta` now publishes `domains.actions`, 192 names on 2026-11-12, 140 of them `productionresultssa<N>.blob.core.windows.net`.
The GMC already fetches `/meta`, so `githubEgressFQDNs` could carry those blob names in place of `*.blob.core.windows.net`, refreshed on the same cadence as the CIDR list.
The cost is a list that changes when GitHub adds an account, so a job uploading to a new account fails until the next refresh, and 140 names where there was one, against GKE's per-hostname IP quotas ([q245](q245-fqdn-intent-backend-split.md)) whose behaviour at that count is unmeasured.
Whether any runner feature writes to a blob account outside that list is also unmeasured; the list is GitHub's published contract, and a gap in it fails a job rather than opening anything.

**Direct mode and the CIDR-mode proxy cannot be narrowed by address alone.** A storage cluster's addresses serve every account on it, and the `actions` ranges also carry GitHub's broker and results endpoints, which resolve into the same Azure regions.
The closest CIDR form resolves the published `domains.actions` names and admits only the answers, which drops most of the 28.6 million addresses.
It inherits DNS churn as job failures, it cannot expand the wildcard entries (`*.actions.githubusercontent.com`, `*.github.com`), and the storage clusters it does admit still serve other customers' accounts, so it bounds the residual rather than closing it.

**What bounds it furthest** is routing workers through an FQDN-mode proxy with the narrowed list, plus an in-cluster mirror for what jobs fetch.
Neither closes it: a CNI FQDN policy admits the addresses the names resolve to, and those storage clusters also serve other customers' accounts.
Changing the default is a security-posture decision the row leaves open.
