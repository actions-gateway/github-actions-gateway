// Package load contains the in-process load-test harness for the Actions
// Gateway Controller (AGC), pinning the design's headline capacity claim:
// thousands of virtual runner sessions multiplexed as goroutines inside one
// AGC, each costing one runner re-registration per job (the single-use JIT
// lifecycle, Q114).
//
// The harness drives the AGC's real listener-multiplexing core
// (listener.Multiplexer + agentpool.Pool + a per-goroutine broker.Client) — the
// same wiring RunnerGroupReconciler.getOrCreateMultiplexer builds in production
// — against an in-process broker stub, a controller-runtime fake client for
// agent Secrets, and an in-memory registrar. It needs no cluster and no real
// GitHub credentials.
//
// The executable harness, broker stub, report, and TestAGCLoad entrypoint are
// all behind the `load` build tag (see broker_stub.go, harness.go, report.go,
// load_test.go), so they are excluded from the default `make check` gate and
// run only via `make load-test-quick` / `make load-test-full`. This file is the
// untagged package home and carries the godoc.
//
// See cmd/agc/test/load/README.md for how to run the harness and interpret its
// results, and the Milestone 5 plan §2 for the design rationale.
package load
