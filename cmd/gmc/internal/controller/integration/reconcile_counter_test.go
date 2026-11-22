//go:build integration

package integration_test

import (
	"context"
	"sync"
	"testing"
	"time"

	v2 "github.com/actions-gateway/github-actions-gateway/api/v2"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// reconcileCounter wraps a reconciler's client to count, per namespace, the reconciles
// that have started and finished. Every reconcile opens with a Get of its live object
// and, for the EgressProxy reconciler, writes its status exactly once last, on the
// success and the degraded path alike, so the two counts differ only while a reconcile
// is in flight. isKind picks the reconciled kind.
type reconcileCounter struct {
	client.Client
	isKind   func(client.Object) bool
	mu       sync.Mutex
	started  map[string]int
	finished map[string]int
}

func newReconcileCounter(c client.Client) *reconcileCounter {
	return newReconcileCounterFor(c, func(o client.Object) bool { _, ok := o.(*v2.EgressProxy); return ok })
}

// newReconcileCounterFor counts the reconciles of the kind isKind matches. Only the
// started count is meaningful for a kind whose reconciler does not end on one status
// Update, so waitIdle is EgressProxy-only.
func newReconcileCounterFor(c client.Client, isKind func(client.Object) bool) *reconcileCounter {
	return &reconcileCounter{Client: c, isKind: isKind, started: map[string]int{}, finished: map[string]int{}}
}

// startedIn reports how many reconciles of a live object in ns have started.
func (c *reconcileCounter) startedIn(ns string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.started[ns]
}

func (c *reconcileCounter) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	err := c.Client.Get(ctx, key, obj, opts...)
	if c.isKind(obj) && err == nil && obj.GetDeletionTimestamp().IsZero() {
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
	if s.c.isKind(obj) {
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
