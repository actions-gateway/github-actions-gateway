# Go best-practices cleanup

Backlog items tracked in `docs/STATUS.md` Queue rows #38, #39, #40, #41. Each
item is independently shippable. None block any milestone. Pick them up
opportunistically when touching the affected packages.

## 1. Unify Go versions ✅ DONE

**Queue row:** #38 · **Size:** S · **Label:** `infra` · **Status:** shipped — all 9 `go.mod` files pinned to a single version (matches `go.work` and CI's `go-version-file: go.work`). Subsequently bumped `1.26.3` → `1.26.4` repo-wide as part of the Q23 govulncheck remediation (stdlib CVE fix for GO-2026-5037/5038/5039); the single-version invariant held through that bump.

Three distinct `go` directives were in use across 9 `go.mod` files:

| Version | Modules |
|---|---|
| `1.26`    | `broker`, `githubapp`, `cmd/proxy`, `cmd/probe`, `test/fakegithub` |
| `1.26.0`  | `cmd/agc`, `cmd/gmc`, `tools` |
| `1.26.3`  | `cmd/worker` (matches `go.work`) |

CLAUDE.md is explicit: *"All go modules in the repo must use the same Go
version."* Every `go.mod` was first unified on `1.26.3` (the highest previously
in use and the version `go.work` then targeted); the Q23 remediation later moved
the whole set to `1.26.4` in lockstep.

The per-module `replace` directives in `broker/`, `cmd/agc/`, `cmd/gmc/`,
`cmd/probe/` were **kept**, not dropped. They are not redundant under this
vendored workspace: the cross-module `require`s use the
`v0.0.0-00010101000000-000000000000` placeholder, and `go work vendor`
cannot resolve those to local paths from the `use` directives alone — it
needs the `replace`s. Removing them breaks `go work vendor` and the
vendored Docker builds.

**Acceptance:** `grep -h '^go ' **/go.mod | sort -u` returns a single line
(`go 1.26.4`). Build + vet + unit tests green across all modules.

## 2. Async-channel violation: `StartRenewLoop` ✅ DONE

**Queue row:** #39 · **Size:** S · **Label:** `bug` · **Status:** shipped — `StartRenewLoop` now returns `(stop func(), done <-chan struct{})`; `handleJob` waits on `done`; test asserts `done` closes after `stop()`.

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

## 3. Extend `goleak` coverage ✅ DONE

**Queue row:** #41 · **Size:** S · **Label:** `tests` · **Status:** shipped — `cmd/worker` now runs `goleak.VerifyTestMain(m)` (its `run()` spawns a payload-writer and an output-drain goroutine, both joined before return). `broker/` was already covered by the `TestMain` in `broker/client_test.go`, so no change was needed there; its only sub-package, `broker/brokertest/`, is a test helper with no test files. Both modules' tests are green under goleak and `make check` passes.

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
