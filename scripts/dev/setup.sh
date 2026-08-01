#!/usr/bin/env bash
# setup.sh — initialise Go module dependencies and verify the build.
# Run once after cloning, and again after any dependency change.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$REPO_ROOT"

echo "==> go mod tidy (root module)"
go mod tidy

echo "==> go mod tidy (probe module)"
(cd cmd/probe && go mod tidy)

echo "==> go work sync"
go work sync

echo "==> go build ./..."
go build ./...

echo "==> go build ./cmd/probe/..."
go build ./cmd/probe/...

echo "==> installing git hooks (core.hooksPath -> .githooks)"
git config core.hooksPath .githooks

# .gitattributes routes docs/STATUS.md to `merge=backlog`, but git will not let a
# tracked file define the driver's command, so the config half has to be
# per-clone. Without it, git just uses its built-in three-way merge.
echo "==> installing the docs/STATUS.md merge driver (merge.backlog)"
scripts/docs/git-merge-status.sh --install

echo ""
echo "Setup complete. Run tests with:"
echo "  go test -race ./..."
echo ""
echo "Before requesting review, run the fast pre-review gate:"
echo "  make check"
