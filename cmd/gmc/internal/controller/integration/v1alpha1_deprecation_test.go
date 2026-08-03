//go:build integration

package integration_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	gmcv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// warningRecorder collects the 299 warning headers the apiserver attaches to a
// response — where a CRD version's deprecationWarning surfaces. The controllers
// install a deduplicating logger here instead (Q515).
type warningRecorder struct {
	mu   sync.Mutex
	msgs []string
}

func (w *warningRecorder) HandleWarningHeaderWithContext(_ context.Context, code int, _ string, text string) {
	if code != 299 || text == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.msgs = append(w.msgs, text)
}

// take returns the warnings seen so far and clears them.
func (w *warningRecorder) take() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	joined := strings.Join(w.msgs, "\n")
	w.msgs = nil
	return joined
}

// warningCapturingClient builds a client whose warning handler records rather
// than logs. The suite's shared k8sClient cannot be used: its handler writes to
// the test log, so nothing is assertable.
func warningCapturingClient(t *testing.T) (client.Client, *warningRecorder) {
	t.Helper()
	rec := &warningRecorder{}
	cfg := rest.CopyConfig(testEnv.Config)
	cfg.WarningHandlerWithContext = rec
	c, err := client.New(cfg, client.Options{Scheme: testScheme})
	require.NoError(t, err)
	return c, rec
}

// TestV1alpha1_ActionsGateway_ApiserverWarnsOnDeprecation pins Q633. The
// actions-gateway.github.com/v1alpha1 ActionsGateway CRD carries
// deprecated: true and a deprecationWarning, so the removal notice reaches an
// operator at apply time rather than only in the docs. v2alpha1 has warned
// since Q411; v1alpha1, the version whose users are furthest behind, did not.
//
// Deprecation removes nothing: the create below still succeeds, the GMC
// validating webhook still admits it, and the version stays served until v2.0.0.
func TestV1alpha1_ActionsGateway_ApiserverWarnsOnDeprecation(t *testing.T) {
	const nsName = "team-deprecation-warning"
	createNamespace(t, nsName)
	createGitHubAppSecret(t, nsName, "github-app")

	c, rec := warningCapturingClient(t)

	ag := newActionsGateway("warned-gateway", nsName, "github-app")
	require.NoError(t, c.Create(ctx, ag))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ag) })

	created := rec.take()
	assert.Contains(t, created, "actions-gateway.github.com/v1alpha1 ActionsGateway is deprecated")
	assert.Contains(t, created, "actions-gateway.com/v2beta1 ActionsGateway", "the warning must name the replacement")
	assert.Contains(t, created, "v2.0.0", "the warning must name the removal release")

	// Reads warn too, so a `kubectl get` surfaces the notice as well as an apply.
	var got gmcv1alpha1.ActionsGateway
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(ag), &got))
	assert.Contains(t, rec.take(), "is deprecated", "a v1alpha1 read must warn as well")
}
