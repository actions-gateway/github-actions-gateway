# GitHub App Credentials for Live Testing

Live-cluster tests (M2 kind check, M3/M4 end-to-end, Ed25519 probe, egress proxy validation) require a real GitHub App installed on the `actions-gateway` org.
This document describes how the credentials are stored and how to provision the Kubernetes Secret the Actions Gateway Controller (AGC) reads.

## App details

| Field           | Value                  |
|-----------------|------------------------|
| App name        | `actions-gateway-test` |
| App ID          | `3752347`              |
| Installation ID | `135739122`            |
| Org             | `actions-gateway`      |

The private key is stored in the macOS Keychain — **not** on disk or in the repository.

## Storing the private key (one-time setup)

Download the `.pem` file from the GitHub App settings page and keep it on disk. `security add-generic-password` takes no file argument, so the key has to reach it some other way — and the two obvious routes are both wrong.
Feed the command below its input on **stdin**, which is neither:

```bash
# The file you just downloaded. Name it exactly — see the warning below.
KEY_FILE=~/Downloads/actions-gateway-test.2026-01-01.private-key.pem

{ printf 'add-generic-password -U -a "actions-gateway-test" -s "github-app-private-key" -X ';
  xxd -p -c 1000000 "$KEY_FILE"; } | security -i
```

`xxd` reads the downloaded file in place; there is no temp copy to make or clean up, which is one fewer copy of the key on disk than any staging approach.

`security -i` reads *commands* from stdin, so the key never becomes a process argument: `xxd` is invoked with only a filename and writes the hex to its stdout, and `security` is invoked with only `-i`. `xxd` is what makes this possible at all — `-X` wants one hex string, and hex is single-line.

Keep the two halves as a literal `printf` and a separate `xxd`.
Collapsing them into `printf '… -X %s' "$(xxd …)"` happens to avoid a leak only because `printf` is a shell builtin and no process is spawned; with `/usr/bin/printf` the key is back in `ps`.

> **Give `xxd` one explicit path, never a glob.** `xxd`'s second positional argument is an *output* file, so `xxd -p -c 1000000 ~/Downloads/*.private-key.pem` with two keys in `~/Downloads` writes the hex of the first **over the second** — exit 0, no warning, the other key destroyed.
> Quote `"$KEY_FILE"` so the shell cannot expand it either.

> **Do not use the `-w` prompt for this key.** It is line-oriented — a multi-line PEM's second line is consumed as the "retype" and the command fails — and it silently truncates input at **128 characters**, which a 2048-bit key exceeds many times over.
> It exits 0 and stores a fragment; the only symptom is authentication failing later. `-w "$(cat <file>)"` avoids the truncation but puts the key in `ps`.

Verify the entry round-trips:

```bash
security find-generic-password -a "actions-gateway-test" -s "github-app-private-key" -w \
  | xxd -r -p | openssl rsa -check -noout
# should print: RSA key ok
```

Check that the whole key parses, not just that the first line looks right: a truncated entry still starts with `-----BEGIN RSA PRIVATE KEY-----`, so `head -1` cannot tell a good key from a broken one.

Only once that passes, delete the download — until then it is the only intact copy:

```bash
rm "$KEY_FILE"
```

> **Note:** `security find-generic-password -w` outputs the password as ASCII hex.
> Pipe through `xxd -r -p` to convert it back to the raw PEM bytes before use.

## Creating the Kubernetes Secret

The AGC reads credentials from files projected into `/etc/actions-gateway/github-app/` by the GMC.
The Secret must contain three keys: `appId`, `installationId`, and `privateKey`.

Materialise the private key into a short-lived temp file (mode `0600`) and load it into the Secret with `--from-file`.
This avoids putting the PEM on the `kubectl` command line (where it would be visible in `ps` and shell history) and ensures the on-disk copy is cleaned up even on failure:

```bash
umask 077
KEY_FILE=$(mktemp -t github-app-private-key.XXXXXX)
trap 'rm -f "$KEY_FILE"' EXIT INT TERM

security find-generic-password \
  -a actions-gateway-test \
  -s github-app-private-key \
  -w | xxd -r -p > "$KEY_FILE"

kubectl create secret generic github-app-secret \
  --namespace <tenant-namespace> \
  --from-literal=appId=3752347 \
  --from-literal=installationId=135739122 \
  --from-file=privateKey="$KEY_FILE"
```

`appId` and `installationId` are not secret, so `--from-literal` is fine for those.
Only the PEM goes via the temp file.

Reference the Secret in the `ActionsGateway` CR:

```yaml
spec:
  gitHubAppRef:
    name: github-app-secret
```

## Rotating the private key

1. Generate a new private key on the [GitHub App settings page](https://github.com/organizations/actions-gateway/settings/apps/actions-gateway-test).
2. Import it with the `security -i` flow from [Storing the private key](#storing-the-private-key-one-time-setup) — `-U` updates the existing entry in place, so there is no window with no key.
3. Verify it round-trips with the `openssl rsa -check` command from that same section.
   Do this **before** step 6: a silently truncated entry and a revoked old key together leave no working credential.
4. Delete the downloaded `.pem` file from `~/Downloads`.
5. Recreate the Kubernetes Secret using the `mktemp` + `--from-file` flow from the previous section (the `trap` ensures the temp file is removed even if `kubectl` fails), then restart the consumers so they re-read it — for the dogfood tenant, `kubectl rollout restart deployment/actions-gateway-controller`.
   Worker pods are single-job and interrupting one costs a job, so check `kubectl get pods -l app.kubernetes.io/managed-by=actions-gateway-controller` first and roll during a quiet window.
6. Delete the old key from the GitHub App settings page. **This is the step that actually ends the exposure** — everything before it only migrates you onto the new key.
   Do it even if the rest is deferred.
