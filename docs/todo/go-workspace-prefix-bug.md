# Go Workspace Module Path Prefix Bug

## Summary

When a `go.work` workspace contains two modules where one module path is a strict
prefix of the other, Go's workspace resolver fails to apply longest-prefix matching
and routes packages to the wrong module directory.

## Reproduction

```
# go.work
use (
    .            # module: github.com/foo/bar        (root dir: .)
    ./cmd/widget # module: github.com/foo/bar/widget (root dir: ./cmd/widget)
)
```

Resolving `github.com/foo/bar/widget/internal/pkg`:
- **Expected:** routed to `./cmd/widget/internal/pkg` (longest module-path prefix)
- **Actual:** routed to `./widget/internal/pkg` (shorter prefix wins), producing
  "not a directory" or "no required module provides" errors

## Workaround (used in this repo)

Omit the root module from `use` and supply it via `replace`:

```
use (
    ./cmd/agc   # github.com/karlkfi/github-actions-gateway/agc
    ./cmd/probe # github.com/karlkfi/github-actions-gateway/probe
)

replace github.com/karlkfi/github-actions-gateway => ./
```

This makes the root module available to other workspace modules without triggering
the prefix ambiguity.

## Investigation needed

- Search golang/go issue tracker for workspace + prefix matching bug
- Check Go 1.22–1.24 release notes for a fix
- If fixed: drop the `replace` workaround and restore `use .` in `go.work`
- If unfixed: file an issue upstream
