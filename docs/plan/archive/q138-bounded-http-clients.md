# Q138 — Bounded-by-default HTTP clients

**Goal:** Eliminate the `http.DefaultClient` (no-timeout) fallbacks in production
HTTP clients so a slow/black-holed peer can no longer wedge a goroutine (the
Q108/Q134 failure class), and gate new bare uses with a linter.

**Approach:** Add a small `httpx` helper that returns a bounded-by-default
`*http.Client` (overall `Timeout` + transport `ResponseHeaderTimeout`). Retrofit
every nil → `http.DefaultClient` fallback to use it. Keep the broker long-poll
listener the explicit, documented exception (it needs no overall read deadline,
only a `ResponseHeaderTimeout`). Add a `forbidigo` (+ `noctx`) golangci gate so
new `http.DefaultClient` / `http.Get` / `http.Post` uses are rejected.

## Package placement

`githubapp/httpx` (a new package inside the existing `githubapp` module). Every
module with a fallback site (`cmd/agc`, `cmd/gmc`, `cmd/probe`) already requires
`githubapp` via a workspace `replace`, so there is **zero** new go.mod / go.work /
vendor wiring — the lowest-risk placement. The helper is stdlib-only.

## Inventory of `http.DefaultClient` fallbacks (the ~8 prod clients)

| # | Site | Fix |
|---|------|-----|
| 1 | `githubapp/auth.go` `NewInstallationTokenProvider` | `httpx.NewClient()` |
| 2 | `githubapp/runner_auth.go` `FetchRunnerOAuthToken` | `httpx.NewClient()` |
| 3 | `cmd/agc/internal/provisioner/provisioner.go` `rerun` | `httpx.NewClient()` |
| 4 | `cmd/agc/internal/agentpool/github_registrar.go` `httpClient()` | `httpx.NewClient()` |
| 5 | `cmd/gmc/internal/controller/ipranges.go` `FetchIPRanges` | `httpx.NewClient()` |
| 6 | `cmd/probe/main.go` `AcknowledgeRunnerRequest` | `httpx.NewClient()` |
| 7 | `cmd/agc/internal/listener` OAuth path (via #2) | covered by #2 |
| 8 | `broker/client.go` `httpClient()` | long-poll exception → `broker.NewHTTPClient()` default |

Also retrofit the two explicit prod clients that already carry a 60s timeout so
they gain the transport `ResponseHeaderTimeout` too:
- `cmd/agc/main.go` provisioner client → `httpx.NewClientWithTimeout(60s)`
- `cmd/gmc/cmd/main.go` IP-range fetcher client → `httpx.NewClientWithTimeout(60s)`

## Long-poll exception

`broker.NewHTTPClient()` already sets a `ResponseHeaderTimeout` sized just above
the broker's 50s long-poll hold and deliberately sets **no** overall `Timeout`
(an overall deadline would sever a healthy long-poll). Change `Client.httpClient()`
so the zero-value fallback returns a package-level `defaultBrokerClient =
NewHTTPClient()` instead of `http.DefaultClient`, and document it as the one
sanctioned exception. `brokertest.Server.HTTPClient()` keeps `http.DefaultClient`
(talks to a local `httptest` server) with an inline `//nolint:forbidigo` + reason.

## Lint gate (root `.golangci.yml`)

- Enable `forbidigo`: forbid `^http\.DefaultClient$` and `^http\.(Get|Post|PostForm|Head)$`.
- Enable `noctx` (catches request-without-context + the same helpers).
- Exclude `_test.go` from both (tests hit bounded `httptest` servers).
- The single non-test exception (`brokertest`) is annotated inline.

## Tests

`githubapp/httpx/httpx_test.go`: assert `NewClient()` sets a non-zero `Timeout`
and a transport `ResponseHeaderTimeout`; assert a server that stalls past the
response-header timeout makes the call fail (not hang); assert
`NewClientWithTimeout` honours/normalises its argument.

## Docs

- `docs/operations/troubleshooting.md`: added a "Reconcile or Token Mint Hangs
  on a Slow GitHub Endpoint" section (sibling to the Q108 long-poll stall),
  describing the new fail-fast behaviour and the long-poll exception. **Done.**
- Remove the Q138 Queue row (its own isolated commit).

## Outcome

- `lint`/`noctx`: noctx was evaluated and **dropped** — golangci-lint exposes no
  noctx config, so it also fails `net.Listen` / `net.DialTimeout` / `os/exec.Command`
  (the egress proxy's listeners, the e2e harness), all unrelated to the
  no-read-timeout class. forbidigo alone precisely gates the HTTP entrypoints;
  prod request construction already uses `http.NewRequestWithContext` throughout.
- Gate verified: a fresh `http.DefaultClient` / `http.Get` use is rejected by
  `make lint`. `make check` green.
