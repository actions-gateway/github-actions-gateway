package scalesetlistener_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/scalesetlistener"
	"github.com/actions-gateway/github-actions-gateway/scaleset"
	"github.com/actions-gateway/github-actions-gateway/scaleset/scalesettest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The runner group is GitHub's authorization point for which repositories may target a
// scale set (Q712). These tests pin the four answers that boundary depends on: a
// declared group is registered, an undeclared one changes nothing, a group the
// installation does not have fails closed instead of widening to the default group, and
// a scale set adopted from an earlier run is moved into the group now declared.

// startGroupListener starts a listener for scale set name in runner group group,
// returning it with the scale-set id. Fails the test if Start does not succeed.
func startGroupListener(t *testing.T, srv *scalesettest.Server, name, group string) int {
	t.Helper()
	l, err := scalesetlistener.New(scalesetlistener.Config{
		Client:          newClient(t, srv),
		ScaleSetName:    name,
		RunnerGroupName: group,
		OwnerName:       "acme/" + name,
		Provision:       func(context.Context, scalesetlistener.Job) error { return nil },
		Capacity:        fixedCapacity(1),
		PollBackoff:     20 * time.Millisecond,
	})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	done, err := l.Start(ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("listener did not stop within 5s")
		}
	})
	return l.Status().ScaleSetID
}

// groupOf reports the runner group id the scale set with the given id is registered in.
func groupOf(t *testing.T, srv *scalesettest.Server, ssID int) int {
	t.Helper()
	ss, err := newClient(t, srv).GetRunnerScaleSet(context.Background(), ssID)
	require.NoError(t, err)
	return ss.RunnerGroupID
}

// TestListener_RegistersScaleSetInDeclaredRunnerGroup is the wiring Q712 was filed for:
// a declared group reaches the scale-set create call, so the set is not registered into
// the installation's default group.
func TestListener_RegistersScaleSetInDeclaredRunnerGroup(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)
	srv.SetRunnerGroups(map[string]int{"tenant-a": 42, "tenant-b": 43})

	ssID := startGroupListener(t, srv, "linux", "tenant-a")

	assert.Equal(t, 42, groupOf(t, srv, ssID),
		"scale set must register into the declared runner group, not GitHub's default")
}

// TestListener_NoDeclaredRunnerGroupUsesGitHubDefault pins the unchanged behaviour for
// a set that declares nothing: GitHub's default group, id 1.
func TestListener_NoDeclaredRunnerGroupUsesGitHubDefault(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)
	srv.SetRunnerGroups(map[string]int{"tenant-a": 42})

	ssID := startGroupListener(t, srv, "linux", "")

	assert.Equal(t, 1, groupOf(t, srv, ssID))
}

// TestListener_UnknownRunnerGroupFailsClosed is the security half. A name the
// installation has no group for used to fall through to the default group, which
// silently widens the set of repositories that may target these runners to the whole
// installation — the opposite of what declaring a group asks for.
func TestListener_UnknownRunnerGroupFailsClosed(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)
	srv.SetRunnerGroups(map[string]int{"tenant-a": 42})

	client := newClient(t, srv)
	l, err := scalesetlistener.New(scalesetlistener.Config{
		Client:          client,
		ScaleSetName:    "linux",
		RunnerGroupName: "tenant-typo",
		OwnerName:       "acme/linux",
		Provision:       func(context.Context, scalesetlistener.Job) error { return nil },
		Capacity:        fixedCapacity(1),
		PollBackoff:     20 * time.Millisecond,
	})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err = l.Start(ctx)

	require.Error(t, err)
	assert.ErrorIs(t, err, scalesetlistener.ErrRunnerGroupNotFound)
	assert.Contains(t, err.Error(), "tenant-typo")

	existing, err := client.GetRunnerScaleSetByName(ctx, "linux")
	require.NoError(t, err)
	assert.Nil(t, existing, "a set that cannot reach its declared group must register nothing")
}

// TestListener_MovesAdoptedScaleSetIntoDeclaredGroup covers the case wiring the field
// alone does not fix. A scale set registered by an earlier run is adopted by name, so
// declaring a group on a set that is already live would otherwise leave the original,
// wider group in force with nothing reporting it.
func TestListener_MovesAdoptedScaleSetIntoDeclaredGroup(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)
	srv.SetRunnerGroups(map[string]int{"tenant-a": 42})

	adopted, err := newClient(t, srv).CreateRunnerScaleSet(context.Background(), scaleset.RunnerScaleSet{
		Name:          "linux",
		RunnerGroupID: 1,
		RunnerSetting: &scaleset.RunnerSetting{Ephemeral: true},
	})
	require.NoError(t, err)

	ssID := startGroupListener(t, srv, "linux", "tenant-a")

	assert.Equal(t, adopted.ID, ssID, "the existing scale set is reused, not replaced")
	assert.Equal(t, 42, groupOf(t, srv, ssID))
}

// TestListener_LeavesAdoptedScaleSetGroupAloneWhenUndeclared is the other side of the
// repair: with no group declared, an adopted set stays where it is. Dragging it into
// the default group by omission would itself widen the boundary.
func TestListener_LeavesAdoptedScaleSetGroupAloneWhenUndeclared(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	adopted, err := newClient(t, srv).CreateRunnerScaleSet(context.Background(), scaleset.RunnerScaleSet{
		Name:          "linux",
		RunnerGroupID: 9,
		RunnerSetting: &scaleset.RunnerSetting{Ephemeral: true},
	})
	require.NoError(t, err)

	ssID := startGroupListener(t, srv, "linux", "")

	assert.Equal(t, adopted.ID, ssID)
	assert.Equal(t, 9, groupOf(t, srv, ssID))
}

// TestListener_RunnerGroupLookupErrorIsNotFallback separates a backend that will not
// answer from a group that does not exist: both must stop the listener, and neither may
// register into the default group, but only the second is ErrRunnerGroupNotFound.
func TestListener_RunnerGroupLookupErrorIsNotFallback(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)
	srv.FailRunnerGroups(true)

	client := newClient(t, srv)
	l, err := scalesetlistener.New(scalesetlistener.Config{
		Client:          client,
		ScaleSetName:    "linux",
		RunnerGroupName: "tenant-a",
		OwnerName:       "acme/linux",
		Provision:       func(context.Context, scalesetlistener.Job) error { return nil },
		Capacity:        fixedCapacity(1),
		PollBackoff:     20 * time.Millisecond,
	})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err = l.Start(ctx)

	require.Error(t, err)
	assert.False(t, errors.Is(err, scalesetlistener.ErrRunnerGroupNotFound),
		"an unreachable lookup is not evidence the group is absent")

	existing, err := client.GetRunnerScaleSetByName(ctx, "linux")
	require.NoError(t, err)
	assert.Nil(t, existing)
}
