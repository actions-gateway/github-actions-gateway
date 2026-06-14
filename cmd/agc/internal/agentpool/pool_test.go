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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
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

func (r *deregErrRegistrar) ResolveAgentID(ctx context.Context, tok, name string) (int64, error) {
	return r.stub.ResolveAgentID(ctx, tok, name)
}

func scheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	return s
}

func newPool(c *fake.ClientBuilder, ns, group string) *agentpool.Pool {
	registrar := agentpool.NewStubRegistrar()
	return agentpool.NewPool(c.Build(), ns, group, "2.335.1", []string{"self-hosted"}, registrar, agentpool.KeyTypeEd25519)
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
		assert.Equal(t, "2.335.1", a.RunnerVersion, "runnerVersion should be stored in Secret")
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

// secretAbsent reports whether the named Secret no longer exists. It fails the
// test on any error other than NotFound.
func secretAbsent(t *testing.T, ctx context.Context, c client.Client, name string) bool {
	t.Helper()
	var s corev1.Secret
	err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: name}, &s)
	if apierrors.IsNotFound(err) {
		return true
	}
	require.NoError(t, err)
	return false
}

// TestPool_ClaimSurvivesReload pins the Q76 double-claim fix: a reconcile
// (EnsureAgents → reload) firing while an agent is claimed must not return that
// agent to the available set, or a second listener would claim the same agent.
func TestPool_ClaimSurvivesReload(t *testing.T) {
	ctx := context.Background()
	fb := fake.NewClientBuilder().WithScheme(scheme())
	pool := newPool(fb, "default", "my-rg")
	require.NoError(t, pool.EnsureAgents(ctx, 2, "token"))

	a1 := pool.ClaimAgent()
	require.NotNil(t, a1)

	// A reconcile fires while a1 is in use. Before Q76, reload() reset available
	// to all agents, so the next claim could hand out a1 again.
	require.NoError(t, pool.EnsureAgents(ctx, 2, "token"))

	a2 := pool.ClaimAgent()
	require.NotNil(t, a2)
	assert.NotEqual(t, a1.Index, a2.Index, "reload must not re-hand-out a claimed agent (double-claim)")

	// Both indexes now claimed → pool exhausted even across the reload.
	assert.Nil(t, pool.ClaimAgent())

	// Releasing a1 makes exactly its index claimable again.
	pool.ReleaseAgent(a1)
	a3 := pool.ClaimAgent()
	require.NotNil(t, a3)
	assert.Equal(t, a1.Index, a3.Index)
}

// TestPool_ScaleDown_SkipsClaimedSecret pins the second half of Q76: scale-down
// must not delete the Secret of an excess agent a listener still holds. The
// agent is deleted only once released, on a later reconcile.
func TestPool_ScaleDown_SkipsClaimedSecret(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(scheme()).Build()
	pool := agentpool.NewPool(c, "default", "my-rg", "2.335.1", []string{"self-hosted"}, agentpool.NewStubRegistrar(), agentpool.KeyTypeEd25519)

	require.NoError(t, pool.EnsureAgents(ctx, 5, "token"))

	// Claim all 5, then release every agent except the highest index (4), so
	// index 4 — which becomes excess when we scale down to 3 — stays in use.
	claimed := make([]*agentpool.Agent, 0, 5)
	for i := 0; i < 5; i++ {
		a := pool.ClaimAgent()
		require.NotNil(t, a)
		claimed = append(claimed, a)
	}
	var held *agentpool.Agent
	for _, a := range claimed {
		if a.Index == 4 {
			held = a
			continue
		}
		pool.ReleaseAgent(a)
	}
	require.NotNil(t, held, "expected to claim index 4")

	// Scale down to 3: index 3 (excess, idle) is deleted; index 4 (excess but
	// claimed) survives — deleting it mid-session would break the in-flight job.
	require.NoError(t, pool.EnsureAgents(ctx, 3, "token"))
	assert.True(t, secretAbsent(t, ctx, c, "agentpool-my-rg-3"), "idle excess agent 3 should be deleted")
	assert.False(t, secretAbsent(t, ctx, c, "agentpool-my-rg-4"), "claimed excess agent 4 must NOT be deleted while in use")

	// Once released, the next reconcile reaps the now-idle excess agent.
	pool.ReleaseAgent(held)
	require.NoError(t, pool.EnsureAgents(ctx, 3, "token"))
	assert.True(t, secretAbsent(t, ctx, c, "agentpool-my-rg-4"), "released excess agent 4 should be deleted on the next reconcile")
}

// TestPool_ReleaseAfterDeleteAll_NoResurrect ensures a late release from a
// goroutine after the RunnerGroup is torn down does not resurrect the agent into
// a now-empty pool.
func TestPool_ReleaseAfterDeleteAll_NoResurrect(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(scheme()).Build()
	pool := agentpool.NewPool(c, "default", "my-rg", "2.335.1", []string{"self-hosted"}, agentpool.NewStubRegistrar(), agentpool.KeyTypeEd25519)
	require.NoError(t, pool.EnsureAgents(ctx, 1, "token"))

	a := pool.ClaimAgent()
	require.NotNil(t, a)

	// RunnerGroup deleted while the agent is in flight.
	require.NoError(t, pool.DeleteAll(ctx, "token"))

	// A late release must be a no-op beyond clearing the claim.
	pool.ReleaseAgent(a)
	assert.Nil(t, pool.ClaimAgent(), "released agent must not be claimable after DeleteAll")
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
	pool := agentpool.NewPool(c, "default", "my-rg", "2.335.1", []string{"self-hosted"}, reg, agentpool.KeyTypeEd25519)

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
	pool := agentpool.NewPool(c, "default", "my-rg", "2.335.1", []string{"self-hosted"}, agentpool.NewStubRegistrar(), agentpool.KeyTypeEd25519)

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
				"app.kubernetes.io/managed-by": agcnames.ControllerName,
				"actions-gateway/runner-group": "my-rg",
				"actions-gateway/agent-index":  "0",
			},
			// Missing data fields to test partial state handling.
		},
		Data: map[string][]byte{
			"agentId":          []byte("999"),
			"clientId":         []byte("c"),
			"authorizationUrl": []byte("u"),
			"privateKeyPEM":    {}, // empty — will fail parse
			"agentIndex":       []byte("0"),
		},
	}

	fb := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existingSecret)
	pool := agentpool.NewPool(fb.Build(), "default", "my-rg", "2.335.1", []string{"self-hosted"}, agentpool.NewStubRegistrar(), agentpool.KeyTypeEd25519)

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

	pool := agentpool.NewPool(c, "default", "my-rg", "2.335.1",
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

// TestPool_EnumeratesSecretsAsMetadataOnly pins the k8s-best-practices §B B4
// fix: the pool must enumerate its agent Secrets with a metadata-only
// PartialObjectMetadataList, never a full SecretList that would pull every
// agent's credential body (private key, JIT config) in a single bulk read.
// Bodies are fetched only per-name via Get, for the paths that genuinely need
// them (reload, deregistration on scale-down/delete).
func TestPool_EnumeratesSecretsAsMetadataOnly(t *testing.T) {
	ctx := context.Background()

	var listTypes []string
	secretGets := 0
	c := fake.NewClientBuilder().WithScheme(scheme()).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				listTypes = append(listTypes, fmt.Sprintf("%T", list))
				return cl.List(ctx, list, opts...)
			},
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.Secret); ok {
					secretGets++
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()

	reg := agentpool.NewStubRegistrar()
	pool := agentpool.NewPool(c, "default", "my-rg", "2.335.1",
		[]string{"self-hosted"}, reg, agentpool.KeyTypeEd25519)

	// Exercise every code path that enumerates Secrets: create, reload via
	// LoadAgents, scale-down (which deregisters and deletes the excess), and
	// teardown.
	require.NoError(t, pool.EnsureAgents(ctx, 2, "token"))
	_, err := pool.LoadAgents(ctx)
	require.NoError(t, err)
	require.NoError(t, pool.EnsureAgents(ctx, 1, "token")) // scale down: 1 excess agent
	require.NoError(t, pool.DeleteAll(ctx, "token"))

	require.NotEmpty(t, listTypes, "pool must list Secrets at least once")
	for _, ty := range listTypes {
		assert.Equal(t, "*v1.PartialObjectMetadataList", ty,
			"pool must enumerate Secrets as metadata only, but issued a %s", ty)
	}
	// reload after a 2-agent create reads both bodies per-name; the scale-down
	// reads the excess agent's body to obtain its agentId for deregistration.
	assert.Positive(t, secretGets, "bodies must be fetched per-name via Get, not via the bulk list")
}
