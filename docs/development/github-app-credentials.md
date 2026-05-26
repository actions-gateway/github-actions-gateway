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

Download the `.pem` file from the GitHub App settings page, then import it
into the login Keychain:

```bash
security add-generic-password \
  -a "actions-gateway-test" \
  -s "github-app-private-key" \
  -w "$(cat ~/Downloads/actions-gateway-test.*.private-key.pem)"
```

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

```bash
kubectl create secret generic github-app-secret \
  --namespace <tenant-namespace> \
  --from-literal=appId=3752347 \
  --from-literal=installationId=135739122 \
  --from-literal=privateKey="$(security find-generic-password \
      -a actions-gateway-test \
      -s github-app-private-key \
      -w | xxd -r -p)"
```

Reference the Secret in the `ActionsGateway` CR:

```yaml
spec:
  gitHubAppRef:
    name: github-app-secret
```

## Rotating the private key

1. Generate a new private key on the [GitHub App settings page](https://github.com/organizations/actions-gateway/settings/apps/actions-gateway-test).
2. Import it with `security add-generic-password` (use `-U` to update the existing entry):

   ```bash
   security add-generic-password -U \
     -a "actions-gateway-test" \
     -s "github-app-private-key" \
     -w "$(cat ~/Downloads/actions-gateway-test.*.private-key.pem)"
   ```

3. Delete the downloaded file and recreate the Kubernetes Secret using the
   command above (remember to pipe through `xxd -r -p`).
4. Delete the old key from the GitHub App settings page.
