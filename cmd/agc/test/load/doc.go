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
// A second entrypoint, TestAGCPerSessionMemory (mem_test.go), isolates the
// AGC's own per-session memory (Q181) without the broker stub: it drives the
// same multiplexing core through memTransport, an in-process http.RoundTripper
// with no server, socket, or per-session server-side state, and reports a
// heap+stack differential as bytes/session. Run it via `make mem-profile`.
//
// The executable harness, broker stub, report, and both test entrypoints are
// all behind the `load` build tag (see broker_stub.go, harness.go, report.go,
// load_test.go, mem_transport.go, mem_test.go), so they are excluded from the
// default `make check` gate and run only via `make load-test-quick` /
// `make load-test-full` / `make mem-profile`. This file is the untagged package
// home and carries the godoc.
//
// See cmd/agc/test/load/README.md for how to run the harness and interpret its
// results, and the Milestone 5 plan §2 for the design rationale.
package load
