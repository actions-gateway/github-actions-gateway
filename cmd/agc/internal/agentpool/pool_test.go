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
	return agentpool.NewPool(c.Build(), ns, group, "2.327.1", []string{"self-hosted"}, registrar, agentpool.KeyTypeEd25519)
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

// frozenListClient wraps a client.Client so that List against SecretList always
// returns the pre-seeded snapshot, regardless of any Creates issued in the
// meantime. This simulates a controller-runtime cache that has not yet
// observed recent writes — the real-world condition that caused the AGC
// integration-test flake fixed in this commit.
type frozenListClient struct {
	client.Client
	snapshot corev1.SecretList
}

func (c *frozenListClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if sl, ok := list.(*corev1.SecretList); ok {
		c.snapshot.DeepCopyInto(sl)
		return nil
	}
	return c.Client.List(ctx, list, opts...)
}

// TestPool_EnsureAgents_PostCreateStateDoesNotDependOnListCache pins the fix
// for the AGC reconciler integration flake. The wrapping client's List
// returns an empty SecretList no matter how many Secrets are Created, so the
// only way for EnsureAgents to publish both agents is to build the in-memory
// pool from the Secrets it created directly — without a post-create re-list.
//
// Prior to the fix, EnsureAgents called reload() (which calls listSecrets)
// after Creates. Against this client, listSecrets returns [] → p.available
// would be [], and the second ClaimAgent in this test would have returned
// nil — the exact "pool exhausted: no agent available" the integration
// suite hit under CI cache-lag conditions.
func TestPool_EnsureAgents_PostCreateStateDoesNotDependOnListCache(t *testing.T) {
	ctx := context.Background()
	inner := fake.NewClientBuilder().WithScheme(scheme()).Build()
	stale := &frozenListClient{Client: inner} // empty snapshot — List always returns []

	registrar := agentpool.NewStubRegistrar()
	pool := agentpool.NewPool(stale, "default", "my-rg", "2.327.1",
		[]string{"self-hosted"}, registrar, agentpool.KeyTypeEd25519)

	require.NoError(t, pool.EnsureAgents(ctx, 2, "token"))

	a1 := pool.ClaimAgent()
	require.NotNil(t, a1, "expected first agent to be claimable after EnsureAgents(2)")
	a2 := pool.ClaimAgent()
	require.NotNil(t, a2,
		"expected SECOND agent to be claimable even though the client's List returns nothing — "+
			"this is the regression that surfaced as TestAGC_Reconciler_JobAcquisitionCycle "+
			"flaking with \"non-retriable: pool exhausted: no agent available\"")
	assert.NotEqual(t, a1.AgentID, a2.AgentID, "claimed agents must be distinct")
	assert.Nil(t, pool.ClaimAgent(), "pool should be exhausted after both agents are claimed")
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
	pool := agentpool.NewPool(c, "default", "my-rg", "2.327.1", []string{"self-hosted"}, reg, agentpool.KeyTypeEd25519)

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
	pool := agentpool.NewPool(c, "default", "my-rg", "2.327.1", []string{"self-hosted"}, agentpool.NewStubRegistrar(), agentpool.KeyTypeEd25519)

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
	pool := agentpool.NewPool(fb.Build(), "default", "my-rg", "2.327.1", []string{"self-hosted"}, agentpool.NewStubRegistrar(), agentpool.KeyTypeEd25519)

	// EnsureAgents(1) — index 0 already exists, should be idempotent.
	err := pool.EnsureAgents(ctx, 1, "token")
	require.NoError(t, err)
}

// TestPool_EnsureAgents_StoresEncodedJITConfig verifies that a registrar's
// EncodedJITConfig flows into the agent Secret and back out through
// LoadAgents. This is the data path the wrapper depends on to materialize
// .runner / .credentials / .credentials_rsaparams (Queue item 5a).
func TestPool_EnsureAgents_StoresEncodedJITConfig(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(scheme()).Build()

	reg := agentpool.NewStubRegistrar()
	reg.SetEncodedJITConfig("aGVsbG8tand0LWNvbmZpZw==") // arbitrary opaque blob

	pool := agentpool.NewPool(c, "default", "my-rg", "2.327.1",
		[]string{"self-hosted"}, reg, agentpool.KeyTypeEd25519)

	require.NoError(t, pool.EnsureAgents(ctx, 2, "token"))

	// Round-trip via LoadAgents: the blob must come back unchanged.
	agents, err := pool.LoadAgents(ctx)
	require.NoError(t, err)
	require.Len(t, agents, 2)
	for _, a := range agents {
		assert.Equal(t, "aGVsbG8tand0LWNvbmZpZw==", a.EncodedJITConfig,
			"agent must carry the JIT blob in memory")
	}

	// And the Secret on disk must have the encodedJITConfig key set so the
	// provisioner — running in a fresh AGC after restart — sees it too.
	var secrets corev1.SecretList
	require.NoError(t, c.List(ctx, &secrets,
		client.InNamespace("default"),
		client.MatchingLabels{"actions-gateway/runner-group": "my-rg"},
	))
	require.Len(t, secrets.Items, 2)
	for _, s := range secrets.Items {
		assert.Equal(t, "aGVsbG8tand0LWNvbmZpZw==", string(s.Data["encodedJITConfig"]),
			"Secret %s must carry the JIT blob", s.Name)
	}
}
