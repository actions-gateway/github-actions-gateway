package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"go.uber.org/zap/zapcore"
	crzap "sigs.k8s.io/controller-runtime/pkg/log/zap"
)

// TestZapLevelFromEnv verifies the LOG_LEVEL → zap level mapping the GMC drives
// via ActionsGateway.spec.logLevel (logging-audit Theme G). debug must enable a
// level low enough to surface the hot-path slog.Debug lines; info and unset must
// return nil so the production default (info) stands; any other value falls back
// to info defensively (the CRD enum gates real input upstream).
func TestZapLevelFromEnv(t *testing.T) {
	t.Run("debug enables the slog.Debug zap level", func(t *testing.T) {
		lvl := zapLevelFromEnv("debug")
		if lvl == nil {
			t.Fatal("LOG_LEVEL=debug must return a non-nil level override")
		}
		// The listener/provisioner/agentpool hot-path logs go through
		// slog.Debug, which the slog→logr→zap bridge gates at slogDebugZapLevel
		// (-4) — below zapcore.DebugLevel. Enabling only DebugLevel (-1) would
		// silently drop them (Q148), so assert the deeper level is enabled.
		if !lvl.Enabled(slogDebugZapLevel) {
			t.Errorf("the override must enable the slog.Debug zap level %d so hot-path lines surface", slogDebugZapLevel)
		}
		if !lvl.Enabled(zapcore.DebugLevel) {
			t.Error("the override must also enable the standard debug level")
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

// TestSlogDebugSurfacesThroughBridge is the regression guard for Q148: it
// reproduces main()'s logging wiring end-to-end — a zap logger gated at the
// LOG_LEVEL=debug level, exposed as logr, with log/slog bridged onto it exactly
// as slog.SetDefault(logr.ToSlogHandler(ctrl.Log)) does — and asserts that a
// slog.Debug line actually reaches the sink. The pre-fix DebugLevel (-1) dropped
// it because the bridge gates slog.Debug at zap -4; the package unit tests that
// used a native slog handler could not catch that, only this real bridge does.
func TestSlogDebugSurfacesThroughBridge(t *testing.T) {
	// Mirror main()'s wiring: a controller-runtime zap logger (the same
	// constructor production uses, here gated at the LOG_LEVEL level and writing
	// to a buffer) exposed as logr, with log/slog bridged onto it.
	build := func(level zapcore.LevelEnabler) (*slog.Logger, *bytes.Buffer) {
		buf := &bytes.Buffer{}
		logrLogger := crzap.New(crzap.Level(level), crzap.WriteTo(buf), crzap.UseDevMode(false))
		return slog.New(logr.ToSlogHandler(logrLogger)), buf
	}

	t.Run("LOG_LEVEL=debug surfaces slog.Debug", func(t *testing.T) {
		lvl := zapLevelFromEnv("debug")
		if lvl == nil {
			t.Fatal("LOG_LEVEL=debug must return a non-nil level override")
		}
		logger, buf := build(lvl)
		logger.Debug("job message received", "messageId", 7)
		if !strings.Contains(buf.String(), "job message received") {
			t.Fatalf("slog.Debug line must surface at LOG_LEVEL=debug; got %q", buf.String())
		}
		// Sanity-check it is structured and carries the field.
		var rec map[string]any
		if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
			t.Fatalf("debug line is not valid JSON: %v (%q)", err, buf.String())
		}
	})

	t.Run("default (info) drops slog.Debug", func(t *testing.T) {
		// No override → production default is info; slog.Debug must stay hidden so
		// the demotion to Debug (Q87) keeps steady-state volume down.
		logger, buf := build(zapcore.InfoLevel)
		logger.Debug("job message received", "messageId", 7)
		if strings.Contains(buf.String(), "job message received") {
			t.Fatalf("slog.Debug must not surface at info level; got %q", buf.String())
		}
	})
}
