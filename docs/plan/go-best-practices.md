# Go best-practices cleanup

Backlog items tracked in `docs/STATUS.md` Queue rows #38, #39, #40, #41. Each
item is independently shippable. None block any milestone. Pick them up
opportunistically when touching the affected packages.

## 1. Unify Go versions

**Queue row:** #38 · **Size:** S · **Label:** `infra`

Three distinct `go` directives are in use across 9 `go.mod` files:

| Version | Modules |
|---|---|
| `1.26`    | `broker`, `githubapp`, `cmd/proxy`, `cmd/probe`, `test/fakegithub` |
| `1.26.0`  | `cmd/agc`, `cmd/gmc`, `tools` |
| `1.26.3`  | `cmd/worker` (matches `go.work`) |

CLAUDE.md is explicit: *"All go modules in the repo must use the same Go
version."* Pin every `go.mod` to `1.26.3` (the highest currently in use and
the version `go.work` already targets).

While editing the `go.mod` files, also drop the `replace` directives that
are now redundant because the workspace covers them — likely candidates are
`broker/`, `cmd/agc/`, `cmd/gmc/`, `cmd/probe/`. Verify with
`go work edit -json` before deleting.

**Acceptance:** `grep -h '^go ' **/go.mod | sort -u` returns a single line.
CI green.

## 2. Async-channel violation: `StartRenewLoop`

**Queue row:** #39 · **Size:** S · **Label:** `bug`

`cmd/agc/internal/listener/goroutine.go:121` `StartRenewLoop` currently
returns `stop func()` and hides the done-channel inside the returned
closure. This violates the explicit CLAUDE.md rule:

> When a function starts something asynchronous, return a `<-chan struct{}`
> done channel so the caller controls whether and how to wait (block,
> select with timeout, ignore). Do not hide the channel inside a closure
> or call site.

**Fix:** Change the signature to return `(stop func(), done <-chan struct{})`
and update every caller to receive the channel (callers may then choose to
wait, ignore, or `select` against it).

**Acceptance:** Signature updated; existing tests pass; new test asserts
`done` closes after `stop()` returns.

## 3. Extend `goleak` coverage

**Queue row:** #41 · **Size:** S · **Label:** `tests`

`broker/` and `cmd/worker/` both spawn goroutines but their test suites
don't apply `goleak.VerifyNone` in `TestMain`. The pattern is already
established in `cmd/proxy/proxy_test.go` and the
`cmd/agc/internal/{listener,token}/*_test.go` files. `goleak` is already a
`broker/` dependency — it's just unused.

**Fix:** Add a `TestMain` in each module's package root that calls
`goleak.VerifyTestMain(m)`. Where intentional long-lived goroutines exist
(e.g. test-server background loops), add the precise `goleak.IgnoreCurrent()`
option rather than disabling the check.

**Acceptance:** Both modules' tests run goleak; CI green.

## 4. Misc idiom cleanup

**Queue row:** #40 · **Size:** S · **Label:** `bug`

Independent small fixes, all touching idiomatic Go usage:

**(a) Silent `json.Unmarshal` error.**
`cmd/agc/internal/provisioner/provisioner.go:210` does
`_ = json.Unmarshal(payload, &ap)` then uses the parsed struct. Silent
payload corruption risk — at minimum log at WARN and return, ideally
surface as a metric.

**(b) `max` builtin shadow.**
`cmd/agc/internal/listener/multiplexer.go:66`
`SetMaxListeners(max int32)` shadows the Go 1.21+ builtin `max`. Rename
the parameter to `maxListeners` (or similar).

**(c) Package-name stutter.**
Rename `broker.BrokerClient` → `broker.Client`. Update every callsite.

**(d) `interface{}` → `any`.**
Replace the 8 remaining `interface{}` occurrences in non-test code with
`any`. Known sites: `test/fakegithub/main.go:67`,
`broker/brokertest/server.go:31,169`. The rest can be found with
`grep -rn 'interface{}' --include='*.go'` filtered to non-`_test.go`.

**(e) Dead code.**
Remove `_ = name // used for label selector above` at
`cmd/agc/internal/controller/actionsgateway_controller.go:246`. The
comment is wrong (the variable is no longer used at all).

**Acceptance:** Each sub-item is a clean independent diff; tests green;
`go vet` and `golangci-lint` clean.
