# GitHub App Credentials for Live Testing

Live-cluster tests (M2 kind check, M3/M4 end-to-end, Ed25519 probe, egress
proxy validation) require a real GitHub App installed on the `actions-gateway`
org. This document describes how the credentials are stored and how to
provision the Kubernetes Secret the Actions Gateway Controller (AGC) reads.

## App details

| Field           | Value                  |
|-----------------|------------------------|
| App name        | `actions-gateway-test` |
| App ID          | `3752347`              |
| Installation ID | `135739122`            |
| Org             | `actions-gateway`      |

The private key is stored in the macOS Keychain — **not** on disk or in the
repository.

## Storing the private key (one-time setup)

Download the `.pem` file from the GitHub App settings page. Keep it on disk
(don't read it into a shell variable or command-line argument) and import it
into the login Keychain with an interactive prompt — `security add-generic-password`
does not accept a file as input, so the safest one-time handoff is to paste
the key into the prompt rather than pass it on the command line (which would
leak it via `ps` and shell history):

```bash
# Prompts for the password — paste the entire PEM (including BEGIN/END lines),
# then press Ctrl-D on a new line to finish.
security add-generic-password \
  -a "actions-gateway-test" \
  -s "github-app-private-key" \
  -w
```

If pasting a multi-line PEM into the prompt is impractical, the fallback is to
pass the key via `-w "$(cat <file>)"`. This briefly exposes the key as a
process argument; only use it on a trusted single-user workstation and delete
the downloaded file immediately after.

Delete the downloaded file once the import succeeds:

```bash
rm ~/Downloads/actions-gateway-test.*.private-key.pem
```

To verify the key is present:

```bash
security find-generic-password -a "actions-gateway-test" -s "github-app-private-key" -w \
  | xxd -r -p | head -1
# should print: -----BEGIN RSA PRIVATE KEY-----
```

> **Note:** `security find-generic-password -w` outputs the password as ASCII
> hex. Pipe through `xxd -r -p` to convert it back to the raw PEM bytes before
> use.

## Creating the Kubernetes Secret

The AGC reads credentials from files projected into
`/etc/actions-gateway/github-app/` by the GMC. The Secret must contain three
keys: `appId`, `installationId`, and `privateKey`.

Materialise the private key into a short-lived temp file (mode `0600`) and
load it into the Secret with `--from-file`. This avoids putting the PEM on
the `kubectl` command line (where it would be visible in `ps` and shell
history) and ensures the on-disk copy is cleaned up even on failure:

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

`appId` and `installationId` are not secret, so `--from-literal` is fine for
those. Only the PEM goes via the temp file.

Reference the Secret in the `ActionsGateway` CR:

```yaml
spec:
  gitHubAppRef:
    name: github-app-secret
```

## Rotating the private key

1. Generate a new private key on the [GitHub App settings page](https://github.com/organizations/actions-gateway/settings/apps/actions-gateway-test).
2. Import it with `security add-generic-password` (add `-U` to update the existing entry).
   Prefer the interactive `-w` prompt so the key is never on the command line:

   ```bash
   security add-generic-password -U \
     -a "actions-gateway-test" \
     -s "github-app-private-key" \
     -w
   # Paste the PEM contents, then Ctrl-D.
   ```

3. Delete the downloaded `.pem` file from `~/Downloads`.
4. Recreate the Kubernetes Secret using the `mktemp` + `--from-file` flow
   from the previous section (the `trap` ensures the temp file is removed
   even if `kubectl` fails).
5. Delete the old key from the GitHub App settings page.
