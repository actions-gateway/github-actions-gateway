package main

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"go.uber.org/zap"
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

// TestNormalizeDebugLevel verifies that a plain "debug" override (whether from
// --zap-log-level=debug or LOG_LEVEL=debug) is deepened to surface the V(4)
// slog.Debug hot-path lines, while every other level is left untouched (Q148).
func TestNormalizeDebugLevel(t *testing.T) {
	t.Run("nil stays nil (default info)", func(t *testing.T) {
		if got := normalizeDebugLevel(nil); got != nil {
			t.Errorf("nil override must stay nil, got %v", got)
		}
	})

	t.Run("plain DebugLevel is deepened to slog.Debug", func(t *testing.T) {
		in := zap.NewAtomicLevelAt(zapcore.DebugLevel)
		got := normalizeDebugLevel(&in)
		if got == nil || !got.Enabled(slogDebugZapLevel) {
			t.Errorf("--zap-log-level=debug must be deepened to enable the slog.Debug level, got %v", got)
		}
	})

	t.Run("already-deep level is unchanged", func(t *testing.T) {
		in := zap.NewAtomicLevelAt(slogDebugZapLevel)
		if got := normalizeDebugLevel(&in); got != &in {
			t.Error("an override already at the slog.Debug level must be returned unchanged")
		}
	})

	for _, tc := range []struct {
		name  string
		level zapcore.Level
	}{
		{"info", zapcore.InfoLevel},
		{"warn", zapcore.WarnLevel},
		{"error", zapcore.ErrorLevel},
	} {
		t.Run(tc.name+" is unchanged", func(t *testing.T) {
			in := zap.NewAtomicLevelAt(tc.level)
			if got := normalizeDebugLevel(&in); got != &in {
				t.Errorf("%s must be returned unchanged, got %v", tc.name, got)
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

// TestConfigureProxyTrust covers the mount states the GMC can leave at
// certDir/tls.crt. A cert that is present but unreadable or unparseable must
// fail startup rather than be swallowed as "no TLS proxy configured" (Q520):
// silently continuing strips proxy trust and surfaces much later as an
// unrelated-looking x509 failure on the first proxied GitHub call.
func TestConfigureProxyTrust(t *testing.T) {
	// configureProxyTrust mutates the process-wide transport; restore it so
	// nothing else in the package inherits a subtest's pool.
	pinDefaultTransport := func(t *testing.T) http.RoundTripper {
		t.Helper()
		orig := http.DefaultTransport
		t.Cleanup(func() { http.DefaultTransport = orig })
		return orig
	}

	t.Run("absent cert leaves the default transport unchanged", func(t *testing.T) {
		orig := pinDefaultTransport(t)
		if err := configureProxyTrust(filepath.Join(t.TempDir(), "does-not-exist"), logr.Discard()); err != nil {
			t.Fatalf("an unmounted proxy CA must be a no-op; got %v", err)
		}
		if http.DefaultTransport != orig {
			t.Error("default transport must not be replaced when no proxy CA is mounted")
		}
	})

	t.Run("unreadable cert fails startup", func(t *testing.T) {
		orig := pinDefaultTransport(t)
		dir := t.TempDir()
		// A directory at the cert path reads back EISDIR: a non-IsNotExist read
		// error reproducible as any user, unlike a chmod-based denial (which root
		// ignores).
		if err := os.Mkdir(filepath.Join(dir, "tls.crt"), 0o700); err != nil {
			t.Fatal(err)
		}
		err := configureProxyTrust(dir, logr.Discard())
		if err == nil {
			t.Fatal("an unreadable proxy CA must fail startup, not pass as no-proxy-configured")
		}
		if !strings.Contains(err.Error(), "read proxy CA") {
			t.Errorf("error must name the read failure; got %v", err)
		}
		if http.DefaultTransport != orig {
			t.Error("default transport must not be replaced when the CA is unreadable")
		}
	})

	t.Run("unparseable cert fails startup", func(t *testing.T) {
		pinDefaultTransport(t)
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "tls.crt"), []byte("not a certificate\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		err := configureProxyTrust(dir, logr.Discard())
		if err == nil {
			t.Fatal("a proxy CA with no parseable certificate must fail startup")
		}
		if !strings.Contains(err.Error(), "build proxy trust pool") {
			t.Errorf("error must name the pool failure; got %v", err)
		}
	})

	t.Run("empty cert leaves the default transport unchanged", func(t *testing.T) {
		orig := pinDefaultTransport(t)
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "tls.crt"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := configureProxyTrust(dir, logr.Discard()); err != nil {
			t.Fatalf("an empty proxy CA is tolerated like the worker wrapper's; got %v", err)
		}
		if http.DefaultTransport != orig {
			t.Error("default transport must not be replaced for an empty proxy CA")
		}
	})

	t.Run("mounted CA is trusted by the default transport", func(t *testing.T) {
		orig := pinDefaultTransport(t)
		caPEM, serverKP, _ := genCABundle(t)
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "tls.crt"), caPEM, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := configureProxyTrust(dir, logr.Discard()); err != nil {
			t.Fatalf("a valid proxy CA must configure trust; got %v", err)
		}
		tr, ok := http.DefaultTransport.(*http.Transport)
		if !ok || http.DefaultTransport == orig {
			t.Fatal("default transport must be replaced with a proxy-trusting clone")
		}
		// The pool, not just the plumbing: a leaf signed by the mounted CA must
		// chain, which is what the AGC↔proxy handshake needs.
		leaf, err := x509.ParseCertificate(serverKP.Certificate[0])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := leaf.Verify(x509.VerifyOptions{
			Roots:     tr.TLSClientConfig.RootCAs,
			KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}); err != nil {
			t.Errorf("a cert signed by the mounted proxy CA must validate against the transport pool: %v", err)
		}
	})
}
