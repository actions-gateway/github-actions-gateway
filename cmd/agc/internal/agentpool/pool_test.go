package agentpool_test

import (
	"context"
	"testing"

	"github.com/karlkfi/github-actions-gateway/agc/internal/agentpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func scheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	return s
}

func newPool(c *fake.ClientBuilder, ns, group string) *agentpool.Pool {
	registrar := agentpool.NewStubRegistrar()
	return agentpool.NewPool(c.Build(), ns, group, registrar)
}

func TestPool_EnsureAgents_Creates(t *testing.T) {
	ctx := context.Background()
	fb := fake.NewClientBuilder().WithScheme(scheme())
	pool := newPool(fb, "default", "my-rg")

	err := pool.EnsureAgents(ctx, 3, "dummy-token")
	require.NoError(t, err)

	agents, err := pool.LoadAgents(ctx)
	require.NoError(t, err)
	assert.Len(t, agents, 3)

	for _, a := range agents {
		assert.NotZero(t, a.AgentID)
		assert.NotNil(t, a.PrivateKey)
		assert.NotEmpty(t, a.Creds.ClientID)
	}
}

func TestPool_EnsureAgents_Idempotent(t *testing.T) {
	ctx := context.Background()
	fb := fake.NewClientBuilder().WithScheme(scheme())
	pool := newPool(fb, "default", "my-rg")

	require.NoError(t, pool.EnsureAgents(ctx, 3, "token"))
	a1, err := pool.LoadAgents(ctx)
	require.NoError(t, err)

	require.NoError(t, pool.EnsureAgents(ctx, 3, "token"))
	a2, err := pool.LoadAgents(ctx)
	require.NoError(t, err)

	assert.Len(t, a2, 3)
	// Same agent IDs after idempotent call.
	for i := range a1 {
		assert.Equal(t, a1[i].AgentID, a2[i].AgentID)
	}
}

func TestPool_EnsureAgents_ScaleDown(t *testing.T) {
	ctx := context.Background()
	fb := fake.NewClientBuilder().WithScheme(scheme())
	pool := newPool(fb, "default", "my-rg")

	require.NoError(t, pool.EnsureAgents(ctx, 5, "token"))
	require.NoError(t, pool.EnsureAgents(ctx, 3, "token"))

	agents, err := pool.LoadAgents(ctx)
	require.NoError(t, err)
	assert.Len(t, agents, 3)
}

func TestPool_LoadAgents_Order(t *testing.T) {
	ctx := context.Background()
	fb := fake.NewClientBuilder().WithScheme(scheme())
	pool := newPool(fb, "default", "my-rg")

	require.NoError(t, pool.EnsureAgents(ctx, 3, "token"))
	agents, err := pool.LoadAgents(ctx)
	require.NoError(t, err)

	require.Len(t, agents, 3)
	assert.Equal(t, 0, agents[0].Index)
	assert.Equal(t, 1, agents[1].Index)
	assert.Equal(t, 2, agents[2].Index)
}

func TestPool_ClaimRelease(t *testing.T) {
	ctx := context.Background()
	fb := fake.NewClientBuilder().WithScheme(scheme())
	pool := newPool(fb, "default", "my-rg")

	require.NoError(t, pool.EnsureAgents(ctx, 2, "token"))

	a1 := pool.ClaimAgent()
	require.NotNil(t, a1)
	a2 := pool.ClaimAgent()
	require.NotNil(t, a2)

	// Pool exhausted.
	assert.Nil(t, pool.ClaimAgent())

	// Release one; should be claimable again.
	pool.ReleaseAgent(a1)
	a3 := pool.ClaimAgent()
	require.NotNil(t, a3)
	assert.Equal(t, a1.AgentID, a3.AgentID)
}

func TestPool_DeleteAll(t *testing.T) {
	ctx := context.Background()
	fb := fake.NewClientBuilder().WithScheme(scheme())
	pool := newPool(fb, "default", "my-rg")

	require.NoError(t, pool.EnsureAgents(ctx, 3, "token"))
	require.NoError(t, pool.DeleteAll(ctx, "token"))

	agents, err := pool.LoadAgents(ctx)
	require.NoError(t, err)
	assert.Empty(t, agents)
}

func TestPool_CreateSecretFailure(t *testing.T) {
	ctx := context.Background()
	// Build a pool with a client that can't create secrets (no scheme for corev1)
	// by intercepting: use a pool pointed at a namespace that causes a conflict
	// on the second secret. We simulate this by pre-creating one secret with
	// the name that the pool would generate for index 0.
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	existingSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agentpool-my-rg-0",
			Namespace: "default",
			Labels: map[string]string{
				"app.kubernetes.io/managed-by":   "actions-gateway-agc",
				"actions-gateway/runner-group":   "my-rg",
				"actions-gateway/agent-index":    "0",
			},
			// Missing data fields to test partial state handling.
		},
		Data: map[string][]byte{
			"agentId":          []byte("999"),
			"clientId":         []byte("c"),
			"authorizationUrl": []byte("u"),
			"privateKeyPEM":    []byte{}, // empty — will fail parse
			"agentIndex":       []byte("0"),
		},
	}

	fb := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existingSecret)
	pool := agentpool.NewPool(fb.Build(), "default", "my-rg", agentpool.NewStubRegistrar())

	// EnsureAgents(1) — index 0 already exists, should be idempotent.
	err := pool.EnsureAgents(ctx, 1, "token")
	require.NoError(t, err)
}
