package broker_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/actions-gateway/github-actions-gateway/broker"
)

func TestPaceEmptyPoll_PadsAPollShorterThanTheFloor(t *testing.T) {
	started := time.Now()
	ok := broker.PaceEmptyPoll(context.Background(), 0)
	assert.True(t, ok, "an uncancelled context may poll again")
	assert.GreaterOrEqual(t, time.Since(started), broker.MinPollInterval,
		"a poll the server answered instantly must be padded out to the floor")
}

// A poll the server really held already paid the floor, so pacing it again would
// be pure added latency on every long-poll cycle.
func TestPaceEmptyPoll_DoesNotWaitAfterALongPoll(t *testing.T) {
	started := time.Now()
	ok := broker.PaceEmptyPoll(context.Background(), 50*time.Second)
	assert.True(t, ok)
	assert.Less(t, time.Since(started), broker.MinPollInterval)
}

func TestPaceEmptyPoll_ReportsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	assert.False(t, broker.PaceEmptyPoll(ctx, 0),
		"a context cancelled mid-wait must stop the caller polling")
	assert.False(t, broker.PaceEmptyPoll(ctx, time.Minute),
		"cancellation is reported on the no-wait path too")
}
