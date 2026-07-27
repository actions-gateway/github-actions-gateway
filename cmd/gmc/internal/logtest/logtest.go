// Package logtest installs a controller-runtime root logger for GMC test
// binaries. It is test support only — nothing in a shipped binary imports it.
//
// controller-runtime's root logger starts life unfulfilled: every call through
// it is a no-op until something calls log.SetLogger. If nothing does within 30
// seconds of process start, the next call through the root logger fulfills it
// with a null sink and prints a "log.SetLogger(...) was never called" banner
// followed by that goroutine's entire stack to os.Stderr — see
// eventuallyFulfillRoot in sigs.k8s.io/controller-runtime/pkg/log.
//
// Without this package no GMC test binary calls SetLogger, which makes their
// output a function of wall-clock time: silent on a quiet machine, and on a
// saturated one a stack trace spliced into the middle of a *passing* test —
// exit code 0, and near-indistinguishable at a glance from a panic. Which test
// wears it is whichever one makes the first log call after the 30-second mark,
// so the attribution is decided by host load rather than by anything in the
// test. That is how Q455 was reported: a stack through reconcileDelete →
// deleteIfExists in a cover-check run with two concurrent `make check` runs
// saturating the host. Under heavier load the same package hands the stack to
// an entirely different test. Fulfilling the root logger up front removes the
// timer, and with it the whole class.
package logtest

import (
	"flag"
	"testing"

	"github.com/go-logr/logr"
	"go.uber.org/zap/zapcore"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

// Install fulfills controller-runtime's root logger. Call it from a package's
// TestMain before m.Run(), so it lands before any test can log:
//
//	func TestMain(m *testing.M) {
//		logtest.Install()
//		os.Exit(m.Run())
//	}
//
// Under `go test -v` the logger writes to stderr, so controller-runtime output
// from the code under test is visible when asked for (`V=1 make test`,
// `V=1 make cover-check`). Otherwise it discards, keeping a green run as quiet
// as it was before. Either way the root logger ends up fulfilled, which is the
// part that matters.
//
// Install parses the command line if TestMain has not already done so, because
// testing.Verbose panics before flag.Parse. m.Run tolerates an already-parsed
// flag set, so calling Install first is safe.
func Install() {
	if !flag.Parsed() {
		flag.Parse()
	}
	if testing.Verbose() {
		// Dev mode's own stacktrace threshold is Warn, which would print a stack
		// under every warning and error the code under test logs — reintroducing
		// exactly the "is this a crash?" output this package exists to remove.
		// Panic is effectively never, since controller-runtime does not log at
		// that level.
		install(zap.New(zap.UseDevMode(true), zap.StacktraceLevel(zapcore.PanicLevel)))
		return
	}
	install(logr.Discard())
}

// install is the seam Install's own test drives with an observable logger. Only
// the first call in a process has an effect: SetLogger fulfills the root
// logger's outstanding promise and clears it, so later calls are no-ops. That
// is why Install belongs in TestMain rather than in individual tests.
func install(l logr.Logger) {
	logf.SetLogger(l)
}
