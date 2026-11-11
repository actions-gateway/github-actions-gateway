package scalesetlistener_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/scalesetlistener"
	"github.com/actions-gateway/github-actions-gateway/scaleset/scalesettest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestListener_RefusesAScaleSetWhoseNameDiffersOnlyByCase is Q1325's fourth lead at
// the listener. The stub's name filter is widened to case-insensitive, which GitHub's
// may or may not be: the listener asking for "CI" is answered with another
// controller's "ci", in a different runner group. Adopting it would open a session on
// that set and PATCH it into this listener's group; the start must fail instead and
// leave the set as it was.
func TestListener_RefusesAScaleSetWhoseNameDiffersOnlyByCase(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)
	srv.SetRunnerGroups(map[string]int{"tenant-a": 42, "tenant-b": 43})
	srv.SetScaleSetNameMatch(strings.EqualFold)
	theirs := srv.AddScaleSet("ci", 43)

	l, err := scalesetlistener.New(scalesetlistener.Config{
		Client:          newClient(t, srv),
		ScaleSetName:    "CI",
		RunnerGroupName: "tenant-a",
		OwnerName:       "acme/CI",
		Provision:       func(context.Context, scalesetlistener.Job) error { return nil },
		Capacity:        fixedCapacity(1),
		PollBackoff:     20 * time.Millisecond,
	})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done, err := l.Start(ctx)
	if err == nil {
		cancel()
		<-done
	}

	require.Error(t, err, "a case-only match is another controller's scale set and must not be adopted")
	assert.False(t, srv.HasActiveSession(theirs), "no session may be opened on the other set")
	assert.Equal(t, 43, groupOf(t, srv, theirs), "the other set's runner group must be left alone")
}
