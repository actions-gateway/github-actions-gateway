package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/migrate"
)

// shrinkRetryPacing shortens the retry budget/gap for the duration of a test so the
// bounded-budget paths run in milliseconds instead of a minute and a half.
func shrinkRetryPacing(t *testing.T, budget, gap time.Duration) {
	t.Helper()
	oldBudget, oldGap := webhookRetryBudget, webhookRetryGap
	webhookRetryBudget, webhookRetryGap = budget, gap
	t.Cleanup(func() { webhookRetryBudget, webhookRetryGap = oldBudget, oldGap })
}

// unreachableErr is the apiserver's text when it could not COMPLETE a webhook call.
// This is the string the fix keys on, reproduced verbatim from the Q391/Q461 e2e
// failures rather than paraphrased — a paraphrase would let the regex rot silently.
func unreachableErr() error {
	return fmt.Errorf(`Internal error occurred: failed calling webhook ` +
		`"vclusterrunnertemplate.kb.io": failed to call webhook: Post ` +
		`"https://gmc-webhook-service.gag-system.svc:443/validate?timeout=10s": ` +
		`context deadline exceeded`)
}

// deniedErr is what a webhook that RAN and rejected the request reads like. It must
// never be retried: it is a verdict, not a blip.
func deniedErr() error {
	return apierrors.NewForbidden(
		schema.GroupResource{Group: "actions-gateway.com", Resource: "runnertemplates"}, "team-a-linux",
		errors.New(`admission webhook "vrunnertemplate.kb.io" denied the request: `+
			`spec.podTemplate declares a privileged container`))
}

func TestRetryOnTransientWebhookError_RidesOutTransientStall(t *testing.T) {
	shrinkRetryPacing(t, 5*time.Second, time.Millisecond)

	calls := 0
	err := retryOnTransientWebhookError(context.Background(), "RunnerSet/team-a", &bytes.Buffer{}, func() error {
		calls++
		if calls < 3 {
			return unreachableErr()
		}
		return nil
	})

	require.NoError(t, err, "a stall that clears inside the budget must succeed")
	assert.Equal(t, 3, calls, "it must keep retrying until the webhook answers")
}

// TestRetryOnTransientWebhookError_DenialFailsFast is the load-bearing half of the
// contract: retrying a genuine rejection would turn a clear, immediate failure into a
// 90-second stall ending in the same error, and would hide a real admission problem
// behind what looks like flakiness.
func TestRetryOnTransientWebhookError_DenialFailsFast(t *testing.T) {
	shrinkRetryPacing(t, 5*time.Second, time.Millisecond)

	calls := 0
	var stderr bytes.Buffer
	err := retryOnTransientWebhookError(context.Background(), "RunnerTemplate/team-a-linux", &stderr, func() error {
		calls++
		return deniedErr()
	})

	require.Error(t, err)
	assert.Equal(t, 1, calls, "a webhook DENIAL must fail on the first attempt, never be retried")
	assert.Contains(t, err.Error(), "denied the request", "the denial reason must reach the operator verbatim")
	assert.Empty(t, stderr.String(), "a fail-fast error must not print a retry banner")
}

// TestRetryOnTransientWebhookError_NonTransientErrorsFailFast covers the other errors
// applyResult depends on seeing immediately — AlreadyExists in particular, since the
// caller's idempotency skip is downstream of this helper.
func TestRetryOnTransientWebhookError_NonTransientErrorsFailFast(t *testing.T) {
	shrinkRetryPacing(t, 5*time.Second, time.Millisecond)

	for name, giveErr := range map[string]error{
		"already exists": apierrors.NewAlreadyExists(
			schema.GroupResource{Group: "actions-gateway.com", Resource: "runnersets"}, "team-a"),
		"forbidden (RBAC)": apierrors.NewForbidden(
			schema.GroupResource{Group: "actions-gateway.com", Resource: "clusterrunnertemplates"},
			"crt-team-a", errors.New("user cannot create resource at the cluster scope")),
		"invalid (CEL)": apierrors.NewInvalid(
			schema.GroupKind{Group: "actions-gateway.com", Kind: "RunnerSet"}, "team-a", nil),
	} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			err := retryOnTransientWebhookError(context.Background(), "obj", &bytes.Buffer{}, func() error {
				calls++
				return giveErr
			})
			require.Error(t, err)
			assert.Equal(t, 1, calls, "only a webhook TRANSPORT failure is retryable")
		})
	}
}

// TestRetryOnTransientWebhookError_BudgetSurfacesLastError proves a persistent outage
// is reported rather than papered over or retried forever.
func TestRetryOnTransientWebhookError_BudgetSurfacesLastError(t *testing.T) {
	shrinkRetryPacing(t, 50*time.Millisecond, time.Millisecond)

	calls := 0
	var stderr bytes.Buffer
	err := retryOnTransientWebhookError(context.Background(), "EgressProxy/team-a-egress", &stderr, func() error {
		calls++
		return unreachableErr()
	})

	require.Error(t, err, "a persistent outage must surface, not succeed silently")
	assert.Contains(t, err.Error(), "context deadline exceeded")
	assert.Greater(t, calls, 1, "it should have retried before giving up")
	assert.Contains(t, stderr.String(), "admission webhook unreachable",
		"the operator must be told why --apply is taking time")
}

// TestRetryOnTransientWebhookError_HonoursContext proves a cancelled run stops
// waiting instead of sitting out the remaining budget.
func TestRetryOnTransientWebhookError_HonoursContext(t *testing.T) {
	shrinkRetryPacing(t, time.Minute, time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	err := retryOnTransientWebhookError(ctx, "RunnerSet/team-a", &bytes.Buffer{}, unreachableErr)

	require.Error(t, err)
	assert.Less(t, time.Since(start), 10*time.Second, "a cancelled context must not wait out the gap")
}

// TestMigrateAll_ApplyRidesOutTransientWebhookStall is the wiring test: it proves the
// retry is actually reached through applyResult, not merely present as a helper. The
// interceptor stalls the FIRST create the way an unreachable webhook does; without the
// retry, --apply aborts there and the later objects (and the namespace patch) never
// happen — the partial-migration failure mode Q461 describes.
func TestMigrateAll_ApplyRidesOutTransientWebhookStall(t *testing.T) {
	shrinkRetryPacing(t, 5*time.Second, time.Millisecond)

	stalled := false
	c := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithObjects(
			v1Namespace("team-a", map[string]string{"actions-gateway.github.com/tenant": "true"}, nil),
			v1Gateway("team-a", "team-a", "restricted"),
			v1RunnerGroup("team-a-linux", "team-a", "img:1", []string{"linux"}),
		).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if !stalled {
					stalled = true
					return unreachableErr()
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).
		Build()

	ctx := context.Background()
	var stderr bytes.Buffer
	require.NoError(t, migrateAll(ctx, c, options{namespace: "team-a", apply: true}, &bytes.Buffer{}, &stderr),
		"a transient webhook stall must not abort --apply")
	assert.True(t, stalled, "the interceptor must actually have stalled a create")

	// The whole fan-out landed — including the objects created after the stall and
	// the namespace patch, which is the last step.
	var v2gw v2alpha1.ActionsGateway
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: "team-a"}, &v2gw))
	var proxy v2alpha1.EgressProxy
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: "team-a-egress"}, &proxy))
	var sets v2alpha1.RunnerSetList
	require.NoError(t, c.List(ctx, &sets, client.InNamespace("team-a")))
	assert.Len(t, sets.Items, 1)

	var ns corev1.Namespace
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "team-a"}, &ns))
	assert.Equal(t, "restricted", ns.Labels[v2alpha1.SecurityProfileLabel],
		"the namespace patch runs last, so it only lands if the stall was ridden out")
}

// TestMigrateAll_ApplyPropagatesWebhookDenial is the negative wiring test: a genuine
// rejection must still abort --apply immediately with the webhook's own reason.
func TestMigrateAll_ApplyPropagatesWebhookDenial(t *testing.T) {
	shrinkRetryPacing(t, 5*time.Second, time.Millisecond)

	creates := 0
	c := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithObjects(
			v1Namespace("team-a", nil, nil),
			v1Gateway("team-a", "team-a", "restricted"),
			v1RunnerGroup("team-a-linux", "team-a", "img:1", []string{"linux"}),
		).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.CreateOption) error {
				creates++
				return deniedErr()
			},
		}).
		Build()

	err := migrateAll(context.Background(), c, options{namespace: "team-a", apply: true},
		&bytes.Buffer{}, &bytes.Buffer{})

	require.Error(t, err, "a webhook denial must fail the migration")
	assert.Contains(t, err.Error(), "denied the request")
	assert.Equal(t, 1, creates, "the denial must abort on the first create, not be retried")
}

// TestPatchNamespace_RidesOutTransientStall covers the second write path: the
// namespace patch is a read-modify-write, and the whole of it is the retry unit.
func TestPatchNamespace_RidesOutTransientStall(t *testing.T) {
	shrinkRetryPacing(t, 5*time.Second, time.Millisecond)

	patches := 0
	c := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithObjects(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}}).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
				patches++
				if patches == 1 {
					return unreachableErr()
				}
				return cl.Patch(ctx, obj, p, opts...)
			},
		}).
		Build()

	ctx := context.Background()
	patch := &migrate.NamespacePatch{
		Name:   "team-a",
		Labels: map[string]string{v2alpha1.SecurityProfileLabel: "restricted"},
	}
	require.NoError(t, patchNamespace(ctx, c, patch, &bytes.Buffer{}))
	assert.Equal(t, 2, patches, "the stalled patch must be retried")

	var ns corev1.Namespace
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "team-a"}, &ns))
	assert.Equal(t, "restricted", ns.Labels[v2alpha1.SecurityProfileLabel])
}
