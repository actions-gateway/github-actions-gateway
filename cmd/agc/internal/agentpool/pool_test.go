package agentpool_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/agentpool"
	agcnames "github.com/actions-gateway/github-actions-gateway/agc/names"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// deregErrRegistrar delegates Register to a StubRegistrar but always errors on Deregister.
type deregErrRegistrar struct {
	stub *agentpool.StubRegistrar
	err  error
}

func (r *deregErrRegistrar) Register(ctx context.Context, tok string, params agentpool.RegisterParams) (*agentpool.AgentCredentials, error) {
	return r.stub.Register(ctx, tok, params)
}

func (r *deregErrRegistrar) Deregister(_ context.Context, _ string, _ int64) error {
	return r.err
}

func scheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	return s
}

func newPool(c *fake.ClientBuilder, ns, group string) *agentpool.Pool {
	registrar := agentpool.NewStubRegistrar()
	return agentpool.NewPool(c.Build(), ns, group, "2.327.1", registrar, agentpool.KeyTypeEd25519)
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
		assert.Equal(t, "2.327.1", a.RunnerVersion, "runnerVersion should be stored in Secret")
		assert.Equal(t, "https://stub.example.com/broker", a.BrokerURL, "brokerURL should be stored in Secret")
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

func TestPool_EnsureAgents_DeregisterErrorContinues(t *testing.T) {
	ctx := context.Background()
	reg := &deregErrRegistrar{
		stub: agentpool.NewStubRegistrar(),
		err:  fmt.Errorf("deregistration failed: temporary error"),
	}
	c := fake.NewClientBuilder().WithScheme(scheme()).Build()
	pool := agentpool.NewPool(c, "default", "my-rg", "2.327.1", reg, agentpool.KeyTypeEd25519)

	// Create 3 agents.
	require.NoError(t, pool.EnsureAgents(ctx, 3, "token"))

	// Scale down to 1; Deregister will error but EnsureAgents should still return nil.
	err := pool.EnsureAgents(ctx, 1, "token")
	assert.NoError(t, err, "EnsureAgents should return nil even when Deregister errors")

	// Excess Secrets should be deleted despite the Deregister error.
	var secrets corev1.SecretList
	require.NoError(t, c.List(ctx, &secrets,
		client.InNamespace("default"),
		client.MatchingLabels{"actions-gateway/runner-group": "my-rg"},
	))
	assert.Len(t, secrets.Items, 1, "only 1 Secret should remain after scale-down")
}

func TestPool_LoadAgents_SkipsCorruptSecret(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(scheme()).Build()
	pool := agentpool.NewPool(c, "default", "my-rg", "2.327.1", agentpool.NewStubRegistrar(), agentpool.KeyTypeEd25519)

	// Create 2 valid agents via EnsureAgents.
	require.NoError(t, pool.EnsureAgents(ctx, 2, "token"))

	// Manually inject a corrupt Secret with valid labels but invalid PEM.
	corruptSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agentpool-my-rg-99",
			Namespace: "default",
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": agcnames.ControllerName,
				"actions-gateway/runner-group": "my-rg",
				"actions-gateway/agent-index":  "99",
			},
		},
		Data: map[string][]byte{
			"agentId":       []byte("9999"),
			"clientId":      []byte("bad-client"),
			"privateKeyPEM": []byte("not-valid-pem"),
			"agentIndex":    []byte("99"),
		},
	}
	require.NoError(t, c.Create(ctx, corruptSecret))

	// LoadAgents must return only the 2 valid agents and no error.
	agents, err := pool.LoadAgents(ctx)
	require.NoError(t, err, "LoadAgents should not return error for a corrupt Secret")
	assert.Len(t, agents, 2, "corrupt Secret should be silently skipped")
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
				"app.kubernetes.io/managed-by":   agcnames.ControllerName,
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
	pool := agentpool.NewPool(fb.Build(), "default", "my-rg", "2.327.1", agentpool.NewStubRegistrar(), agentpool.KeyTypeEd25519)

	// EnsureAgents(1) — index 0 already exists, should be idempotent.
	err := pool.EnsureAgents(ctx, 1, "token")
	require.NoError(t, err)
}
