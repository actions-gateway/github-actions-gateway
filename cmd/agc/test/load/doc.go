// Package load holds the in-process memory probe for the Actions Gateway
// Controller (AGC): TestScaleSetPerListenerMemory (scaleset_mem_test.go)
// isolates the AGC's own cost of one live scale-set session (Q722), driving the
// real scale-set listener through scalesetMemTransport, an in-process
// http.RoundTripper with no server, socket, or per-session server-side state.
// It needs no cluster and no real GitHub credentials.
//
// The probe is behind the `load` build tag, so it is excluded from the default
// `make check` gate and runs only via `make scaleset-mem-profile`. This file is
// the untagged package home and carries the godoc.
//
// See cmd/agc/test/load/README.md for how to run it and read its result.
package load
