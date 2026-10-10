//go:build integration

package integration_test

import (
	"context"
	"sync"
	"testing"
	"time"

	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// reconcileCounter wraps the EgressProxy reconciler's client to count, per namespace,
// the reconciles that have started and finished. Every reconcile opens with a Get of
// its EgressProxy and, once that finds a live object, writes its status exactly once
// last, on the success and the degraded path alike, so the two counts differ only
// while a reconcile is in flight.
type reconcileCounter struct {
	client.Client
	mu       sync.Mutex
	started  map[string]int
	finished map[string]int
}

func newReconcileCounter(c client.Client) *reconcileCounter {
	return &reconcileCounter{Client: c, started: map[string]int{}, finished: map[string]int{}}
}

func (c *reconcileCounter) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	err := c.Client.Get(ctx, key, obj, opts...)
	if _, ok := obj.(*v2alpha1.EgressProxy); ok && err == nil && obj.GetDeletionTimestamp().IsZero() {
		c.mu.Lock()
		c.started[key.Namespace]++
		c.mu.Unlock()
	}
	return err
}

func (c *reconcileCounter) Status() client.SubResourceWriter {
	return &statusCounter{SubResourceWriter: c.Client.Status(), c: c}
}

type statusCounter struct {
	client.SubResourceWriter
	c *reconcileCounter
}

func (s *statusCounter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	if _, ok := obj.(*v2alpha1.EgressProxy); ok {
		defer func() {
			s.c.mu.Lock()
			s.c.finished[obj.GetNamespace()]++
			s.c.mu.Unlock()
		}()
	}
	return s.SubResourceWriter.Update(ctx, obj, opts...)
}

// waitIdle blocks until no reconcile for a proxy in ns is in flight and none has
// started for quiet. The window covers only the gap between a status write and the
// reconcile its watch event queues, which the in-flight count cannot see.
func (c *reconcileCounter) waitIdle(t *testing.T, ns string, quiet time.Duration) {
	t.Helper()
	read := func() (int, bool) {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.started[ns], c.started[ns] == c.finished[ns]
	}
	last, _ := read()
	since := time.Now()
	require.Eventually(t, func() bool {
		n, settled := read()
		if n != last || !settled {
			last, since = n, time.Now()
			return false
		}
		return time.Since(since) >= quiet
	}, 30*time.Second, 50*time.Millisecond, "EgressProxy reconciles in %s never went idle", ns)
}
