package agentpool_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/agentpool"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Q466: a v1 RunnerGroup and a v2 RunnerSet of the same name share a namespace for
// the whole coexistence window of a migration. These tests pin the property that
// makes rollback survivable — the two pools touch disjoint Secrets and disjoint
// GitHub runner records — plus the owner references and the one-time adoption that
// carry an already-deployed install across the rename.

const (
	testNS   = "team-a"
	testName = "shared-name"
)

func runnerSetOwner() []metav1.OwnerReference {
	return []metav1.OwnerReference{{
		APIVersion: "actions-gateway.com/v2alpha1",
		Kind:       "RunnerSet",
		Name:       testName,
		UID:        "set-uid",
		Controller: ptr.To(true),
	}}
}

func runnerGroupOwner() []metav1.OwnerReference {
	return []metav1.OwnerReference{{
		APIVersion: "actions-gateway.github.com/v1alpha1",
		Kind:       "RunnerGroup",
		Name:       testName,
		UID:        "group-uid",
		Controller: ptr.To(true),
	}}
}

// secretNames lists every Secret in the namespace, for whole-namespace assertions
// that no stray copy is left behind.
func secretNames(t *testing.T, c client.Client) []string {
	t.Helper()
	var list corev1.SecretList
	require.NoError(t, c.List(context.Background(), &list, client.InNamespace(testNS)))
	out := make([]string, 0, len(list.Items))
	for _, s := range list.Items {
		out = append(out, s.Name)
	}
	return out
}

// TestLabelRunnerSetMatchesProvisioner pins the re-declared owner-identity label key
// to the provisioner's. The package cannot import provisioner (provisioner → listener
// → agentpool), so this external test is what keeps the two spellings from drifting.
func TestLabelRunnerSetMatchesProvisioner(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(scheme()).Build()
	pool := agentpool.NewRunnerSetPool(c, testNS, testName, "2.335.1", nil, agentpool.NewStubRegistrar(), agentpool.KeyTypeEd25519)
	require.NoError(t, pool.EnsureAgents(ctx, 1, "tok"))

	var sec corev1.Secret
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: testNS, Name: "agentpool-rs-" + testName + "-0"}, &sec))
	assert.Equal(t, testName, sec.Labels[provisioner.LabelRunnerSet],
		"the RunnerSet pool's owner-identity label must be the same key the provisioner stamps on worker pods")
}

// TestCoexistence_SameNameGroupAndSetDoNotCollide is the regression test for the
// defect measured live on the dogfood cluster: with both control planes up, the v1
// RunnerGroup pool error-looped on `secrets "agentpool-<name>-<i>" already exists`
// because the migrated RunnerSet derived the same names.
func TestCoexistence_SameNameGroupAndSetDoNotCollide(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(scheme()).Build()
	registrar := agentpool.NewStubRegistrar()

	setPool := agentpool.NewRunnerSetPool(c, testNS, testName, "2.335.1", []string{"self-hosted"}, registrar, agentpool.KeyTypeEd25519)
	setPool.SetOwner(runnerSetOwner()[0])
	groupPool := agentpool.NewPool(c, testNS, testName, "2.335.1", []string{"self-hosted"}, registrar, agentpool.KeyTypeEd25519)
	groupPool.SetOwner(runnerGroupOwner()[0])

	// The migrated set comes up first, as it does in a migration, then v1 reconciles.
	require.NoError(t, setPool.EnsureAgents(ctx, 2, "tok"))
	require.NoError(t, groupPool.EnsureAgents(ctx, 2, "tok"),
		"the v1 RunnerGroup pool must reconcile cleanly with a same-named RunnerSet already up")

	assert.ElementsMatch(t, []string{
		"agentpool-rs-shared-name-0", "agentpool-rs-shared-name-1",
		"agentpool-shared-name-0", "agentpool-shared-name-1",
	}, secretNames(t, c))

	// Each pool sees only its own agents — the selectors are disjoint, not merely
	// the names, so neither can scale down or deregister the other's.
	setAgents, err := setPool.LoadAgents(ctx)
	require.NoError(t, err)
	groupAgents, err := groupPool.LoadAgents(ctx)
	require.NoError(t, err)
	require.Len(t, setAgents, 2)
	require.Len(t, groupAgents, 2)
	for i := range setAgents {
		assert.NotEqual(t, setAgents[i].AgentID, groupAgents[i].AgentID,
			"the two pools must hold distinct GitHub runner registrations")
	}

	// GitHub runner names are unique per registration scope, so they must differ too:
	// sharing one would have the two pools take turns deregistering each other's live
	// record via the 409-conflict path.
	for i := 0; i < 2; i++ {
		assert.NotZero(t, registrar.AgentIDForName(fmt.Sprintf("rs-%s-%d", testName, i)))
		assert.NotZero(t, registrar.AgentIDForName(fmt.Sprintf("%s-%d", testName, i)))
	}

	// A loaded Agent carries the name its own pool registered, which is what the
	// listener puts on the wire (Q677). Asserted against the registrar rather than a
	// literal, so this fails if Agent.Name and the registered record ever diverge —
	// the divergence itself, not one spelling of it.
	for i := range setAgents {
		assert.Equal(t, registrar.AgentIDForName(setAgents[i].Name), setAgents[i].AgentID,
			"a RunnerSet agent's Name must resolve to its own registered runner")
		assert.Equal(t, registrar.AgentIDForName(groupAgents[i].Name), groupAgents[i].AgentID,
			"a RunnerGroup agent's Name must resolve to its own registered runner")
	}
	assert.Equal(t, "rs-"+testName+"-0", setAgents[0].Name,
		"the RunnerSet wire name is kind-scoped (Q466/Q677)")
	assert.Equal(t, testName+"-0", groupAgents[0].Name)

	// Rollback: the RunnerSet is torn down and v1 is left alone. Its agents survive.
	require.NoError(t, setPool.DeleteAll(ctx, "tok"))
	assert.ElementsMatch(t, []string{"agentpool-shared-name-0", "agentpool-shared-name-1"}, secretNames(t, c))
	require.NoError(t, groupPool.EnsureAgents(ctx, 2, "tok"))
	groupAgents, err = groupPool.LoadAgents(ctx)
	require.NoError(t, err)
	assert.Len(t, groupAgents, 2, "the v1 pool must still be whole after the v2 side is removed")
	assert.Equal(t, 4, registrar.RegisterCalls(),
		"tearing down the RunnerSet must not have forced the RunnerGroup to re-register")
}

// TestEnsureAgents_StampsAndBackfillsOwnerRef covers both halves of the ownership
// fix: newly created agent Secrets carry the owner reference, and ones written
// before it existed are back-filled in place rather than left unowned forever.
func TestEnsureAgents_StampsAndBackfillsOwnerRef(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(scheme()).Build()
	pool := agentpool.NewPool(c, testNS, testName, "2.335.1", nil, agentpool.NewStubRegistrar(), agentpool.KeyTypeEd25519)
	pool.SetOwner(runnerGroupOwner()[0])

	require.NoError(t, pool.EnsureAgents(ctx, 1, "tok"))
	var sec corev1.Secret
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: testNS, Name: "agentpool-shared-name-0"}, &sec))
	require.Len(t, sec.OwnerReferences, 1)
	assert.Equal(t, "RunnerGroup", sec.OwnerReferences[0].Kind)
	assert.Equal(t, "group-uid", string(sec.OwnerReferences[0].UID))

	// Simulate a Secret written by an older AGC: strip the reference and reconcile.
	sec.OwnerReferences = nil
	require.NoError(t, c.Update(ctx, &sec))
	require.NoError(t, pool.EnsureAgents(ctx, 1, "tok"))
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: testNS, Name: "agentpool-shared-name-0"}, &sec))
	require.Len(t, sec.OwnerReferences, 1, "an agent Secret with no owner reference must be back-filled")
	assert.Equal(t, "group-uid", string(sec.OwnerReferences[0].UID))
	assert.NotEmpty(t, sec.Data["privateKeyPEM"], "the back-fill must not disturb the credential body")
}

// legacySecret builds an agent Secret in the pre-Q466 shared shape: the v1 name and
// the v1 selector labels, no owner reference. Before the fix this is exactly what a
// v1 RunnerGroup and a v2 RunnerSet both wrote.
func legacySecret(index int, agentID int64) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("agentpool-%s-%d", testName, index),
			Namespace: testNS,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "actions-gateway-controller",
				"actions-gateway/runner-group": testName,
				"actions-gateway/agent-index":  fmt.Sprintf("%d", index),
			},
		},
		Data: map[string][]byte{
			"agentId":    []byte(fmt.Sprintf("%d", agentID)),
			"agentIndex": []byte(fmt.Sprintf("%d", index)),
			"clientId":   []byte("stub-client-id"),
		},
	}
}

func TestAdoptLegacyRunnerSetSecrets(t *testing.T) {
	ctx := context.Background()
	never := func(context.Context) (bool, error) { return false, nil }

	t.Run("carries an existing v2 install across the rename", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme()).Build()
		registrar := agentpool.NewStubRegistrar()
		// The pre-Q466 layout is exactly what the RunnerGroup scheme writes — that
		// identity is the collision — so build the fixture with it, credentials and all.
		legacy := agentpool.NewPool(c, testNS, testName, "2.335.1", nil, registrar, agentpool.KeyTypeEd25519)
		require.NoError(t, legacy.EnsureAgents(ctx, 2, "tok"))
		var before corev1.Secret
		require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: testNS, Name: "agentpool-shared-name-0"}, &before))

		n, err := agentpool.AdoptLegacyRunnerSetSecrets(ctx, c, testNS, testName, runnerSetOwner(), never)
		require.NoError(t, err)
		assert.Equal(t, 2, n)

		assert.ElementsMatch(t, []string{"agentpool-rs-shared-name-0", "agentpool-rs-shared-name-1"},
			secretNames(t, c), "the legacy copies must be gone, not merely shadowed")

		var sec corev1.Secret
		require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: testNS, Name: "agentpool-rs-shared-name-0"}, &sec))
		assert.Equal(t, before.Data["agentId"], sec.Data["agentId"],
			"the GitHub registration must ride along, or the runner record leaks")
		assert.Equal(t, before.Data["privateKeyPEM"], sec.Data["privateKeyPEM"])
		assert.Equal(t, testName, sec.Labels["actions-gateway.com/runner-set"])
		assert.Equal(t, "0", sec.Labels["actions-gateway/agent-index"])
		require.Len(t, sec.OwnerReferences, 1)
		assert.Equal(t, "RunnerSet", sec.OwnerReferences[0].Kind)

		// The adopted agents load as a normal pool under the RunnerSet scheme, and a
		// reconcile finds them rather than registering a second set of runners.
		pool := agentpool.NewRunnerSetPool(c, testNS, testName, "2.335.1", nil, registrar, agentpool.KeyTypeEd25519)
		require.NoError(t, pool.EnsureAgents(ctx, 2, "tok"))
		agents, err := pool.LoadAgents(ctx)
		require.NoError(t, err)
		assert.Len(t, agents, 2)
		assert.Equal(t, 2, registrar.RegisterCalls(), "adoption must not re-register the agents")
	})

	t.Run("leaves a live v1 RunnerGroup's Secrets alone", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme()).
			WithObjects(legacySecret(0, 7001)).Build()
		claimed := func(context.Context) (bool, error) { return true, nil }

		n, err := agentpool.AdoptLegacyRunnerSetSecrets(ctx, c, testNS, testName, runnerSetOwner(), claimed)
		require.NoError(t, err)
		assert.Zero(t, n)
		assert.Equal(t, []string{"agentpool-shared-name-0"}, secretNames(t, c),
			"taking the RunnerGroup's agents would break the tenant rollback depends on")
	})

	t.Run("fails closed when ownership cannot be determined", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme()).
			WithObjects(legacySecret(0, 7001)).Build()
		boom := func(context.Context) (bool, error) { return false, errors.New("apiserver down") }

		_, err := agentpool.AdoptLegacyRunnerSetSecrets(ctx, c, testNS, testName, runnerSetOwner(), boom)
		require.Error(t, err)
		assert.Equal(t, []string{"agentpool-shared-name-0"}, secretNames(t, c))
	})

	t.Run("is a no-op once the RunnerSet scheme is in use", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme()).
			WithObjects(legacySecret(0, 7001)).Build()
		pool := agentpool.NewRunnerSetPool(c, testNS, testName, "2.335.1", nil, agentpool.NewStubRegistrar(), agentpool.KeyTypeEd25519)
		require.NoError(t, pool.EnsureAgents(ctx, 1, "tok"))

		n, err := agentpool.AdoptLegacyRunnerSetSecrets(ctx, c, testNS, testName, runnerSetOwner(), never)
		require.NoError(t, err)
		assert.Zero(t, n, "once the set has its own agents the legacy names are none of its business")
		assert.Contains(t, secretNames(t, c), "agentpool-shared-name-0")
	})

	t.Run("does nothing when there is nothing to adopt", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme()).Build()
		n, err := agentpool.AdoptLegacyRunnerSetSecrets(ctx, c, testNS, testName, runnerSetOwner(), never)
		require.NoError(t, err)
		assert.Zero(t, n)
	})
}
