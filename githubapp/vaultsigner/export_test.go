package vaultsigner

import "time"

// NewForTest builds a Signer with the clock and ServiceAccount-token reader
// injected, so tests can drive lease expiry deterministically and supply a fixed
// token without touching the filesystem. It is compiled only under test
// (export_test.go) and is not part of the public API.
func NewForTest(cfg Config, now func() time.Time, readToken func() ([]byte, error)) (*Signer, error) {
	cfg.now = now
	cfg.readToken = readToken
	return New(cfg)
}
