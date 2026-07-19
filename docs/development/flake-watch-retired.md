# Retired flakes

Cold storage for flake-watch rows that have graduated out of the live [Flake watch](../STATUS.md#flake-watch) table. A row lands here when its recurrence-memory has decayed to ~zero — see the retirement bar in [maintaining-backlog.md](maintaining-backlog.md#retiring-a-flake-watch-row) (**soaked** — the spec's blast-radius run threshold of green `main` runs since the fix, **or** the test/code path is **obsolete**).

This ledger exists so retirement is not deletion: it keeps the "a fix was already attempted here" memory `grep`-able at zero live-table cost. If a retired flake ever recurs, it re-enters the Queue as a fresh find (flakes-first); this row is the pointer back to the original diagnosis, so re-add it to the escalation history rather than starting cold.

Newest retirement first.

| ID | Item | Fix PR | Retired | Why retired |
|---|---|---|---|---|
| _(none yet)_ | | | | |
