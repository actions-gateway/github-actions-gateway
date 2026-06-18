package main

import (
	"testing"

	"go.uber.org/zap/zapcore"
)

// TestZapLevelFromEnv verifies the LOG_LEVEL → zap level mapping the GMC drives
// via ActionsGateway.spec.logLevel (logging-audit Theme G). debug must enable a
// debug AtomicLevel so the per-session/per-job debug lines surface; info and
// unset must return nil so the production default (info) stands; any other value
// falls back to info defensively (the CRD enum gates real input upstream).
func TestZapLevelFromEnv(t *testing.T) {
	t.Run("debug enables debug level", func(t *testing.T) {
		lvl := zapLevelFromEnv("debug")
		if lvl == nil {
			t.Fatal("LOG_LEVEL=debug must return a non-nil level override")
		}
		if !lvl.Enabled(zapcore.DebugLevel) {
			t.Error("the override must enable the debug level")
		}
	})

	t.Run("debug is case-insensitive", func(t *testing.T) {
		if zapLevelFromEnv("DEBUG") == nil {
			t.Error("LOG_LEVEL matching must be case-insensitive")
		}
	})

	for _, v := range []string{"info", "", "trace", "warn"} {
		t.Run("no override for "+v, func(t *testing.T) {
			if lvl := zapLevelFromEnv(v); lvl != nil {
				t.Errorf("LOG_LEVEL=%q must not override the default level, got %v", v, lvl)
			}
		})
	}
}
