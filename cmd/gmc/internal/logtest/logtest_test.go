package logtest

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// captureSink is a logr.LogSink that records the messages logged through it.
type captureSink struct {
	mu   sync.Mutex
	msgs []string
}

func (s *captureSink) Init(logr.RuntimeInfo)               {}
func (s *captureSink) Enabled(int) bool                    { return true }
func (s *captureSink) Info(_ int, msg string, _ ...any)    { s.record(msg) }
func (s *captureSink) Error(_ error, msg string, _ ...any) { s.record(msg) }
func (s *captureSink) WithValues(...any) logr.LogSink      { return s }
func (s *captureSink) WithName(string) logr.LogSink        { return s }

func (s *captureSink) record(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msgs = append(s.msgs, msg)
}

func (s *captureSink) messages() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.msgs...)
}

// TestInstall covers the whole contract in one function because installing the
// root logger is a once-per-process event: the phases must run in this order,
// and splitting them into separate top-level tests would let a -run filter
// reorder or skip the first one.
//
// The property under test is the one Q455 turns on: after Install, a
// logf.FromContext call on a plain context reaches the installed sink. That can
// only happen once controller-runtime's root logger promise is fulfilled — and
// a fulfilled root logger is exactly what stops eventuallyFulfillRoot from
// dumping a goroutine stack to stderr at the 30-second mark.
func TestInstall(t *testing.T) {
	sink := &captureSink{}
	install(logr.New(sink))

	// The call shape ActionsGatewayReconciler.deleteIfExists makes: a root
	// logger fetched from a context that carries none.
	logf.FromContext(context.Background()).Error(errors.New("apiserver unavailable"),
		"failed to delete resource during teardown")
	require.Equal(t, []string{"failed to delete resource during teardown"}, sink.messages(),
		"a fulfilled root logger must deliver to the installed sink")

	t.Run("only the first install wins", func(t *testing.T) {
		// SetLogger fulfills the outstanding promise and clears it, so a second
		// install cannot redirect anything. This is why Install belongs in
		// TestMain, ahead of every test, rather than inside one.
		later := &captureSink{}
		install(logr.New(later))

		logf.Log.Info("after the second install")
		assert.Empty(t, later.messages(), "a later install must not take over the root logger")
		assert.Contains(t, sink.messages(), "after the second install",
			"the first installed sink keeps receiving")
	})

	t.Run("Install is safe to call on an already-fulfilled root", func(t *testing.T) {
		// The exported entry point: it must not panic when the root logger has
		// already been fulfilled (a package whose TestMain ran after another's).
		require.NotPanics(t, Install)
	})
}
