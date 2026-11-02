//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/broker"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func createNSForAGC(t *testing.T, name string) {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	err := k8sClient.Create(ctx, ns)
	if err != nil {
		require.NoError(t, client.IgnoreNotFound(err))
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), ns)
	})
}

// enqueueJobOnOwnerSession blocks until the broker stub has an active session for
// the given OWNER STEM that is not in alreadySeen, then enqueues a job on it and
// returns its ID (or "" on timeout). It scopes to one owner, so it never enqueues
// onto a session another test left active on the shared stub.
func enqueueJobOnOwnerSession(timeout time.Duration, group string, alreadySeen map[string]bool, payload broker.RunnerJobRequestBody) string {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, id := range brokerStub.ActiveSessionsForOwner(group) {
			if !alreadySeen[id] {
				brokerStub.EnqueueJob(id, payload)
				return id
			}
		}
		time.Sleep(1 * time.Millisecond)
	}
	return ""
}

// enqueueJobOnSetSession is enqueueJobOnOwnerSession for a v2 RunnerSet. A
// RunnerSet's listener owns its session as its registered runner name, which is
// kind-scoped to "rs-<set>-<index>" (Q466/Q677), so the owner stem is not the bare
// CR name. Separate helper rather than a prefix at each call site: passing the bare
// name matches nothing and the failure reads as "no session was ever created",
// which is what it looked like when Q677 first landed.
func enqueueJobOnSetSession(timeout time.Duration, set string, alreadySeen map[string]bool, payload broker.RunnerJobRequestBody) string {
	return enqueueJobOnOwnerSession(timeout, "rs-"+set, alreadySeen, payload)
}

// podGone reports whether the named pod no longer exists.
func podGone(nsName, name string) bool {
	err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: name}, &corev1.Pod{})
	return apierrors.IsNotFound(err)
}
