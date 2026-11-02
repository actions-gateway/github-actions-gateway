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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// shrinkRetryPacing shortens the retry budget/gap for the duration of a test so the
// bounded-budget paths run in milliseconds instead of a minute and a half.
func shrinkRetryPacing(t *testing.T, budget, gap time.Duration) {
	t.Helper()
	oldBudget, oldGap := webhookRetryBudget, webhookRetryGap
	webhookRetryBudget, webhookRetryGap = budget, gap
	t.Cleanup(func() { webhookRetryBudget, webhookRetryGap = oldBudget, oldGap })
}

// pinRetryClock stops the retry loop's clock for one test: every gap fires at once,
// and time stands still until the op calls the returned expire, which moves it to the
// end of the budget. The op therefore decides which attempt is the last, so the
// attempt count is exact on a loaded host and an idle one alike. A real budget asks
// only whether N attempts fit inside it, which is a property of the host (Q1111).
//
// Exactly one select case is ever ready at a pass, so the loop has no race to lose.
func pinRetryClock(t *testing.T) (expire func()) {
	t.Helper()
	now := time.Unix(0, 0)
	webhookRetryNow = func() time.Time { return now }
	webhookRetryAfter = func(time.Duration) <-chan time.Time {
		ch := make(chan time.Time, 1)
		ch <- now
		return ch
	}
	t.Cleanup(func() { webhookRetryNow, webhookRetryAfter = nil, nil })
	return func() { now = now.Add(webhookRetryBudget) }
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
// a caller depends on seeing immediately — AlreadyExists in particular, since a
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
	const attempts = 3
	shrinkRetryPacing(t, 5*time.Second, time.Millisecond)
	expire := pinRetryClock(t)

	calls := 0
	var stderr bytes.Buffer
	err := retryOnTransientWebhookError(context.Background(), "EgressProxy/team-a-egress", &stderr, func() error {
		calls++
		if calls == attempts {
			expire()
		}
		return unreachableErr()
	})

	require.Error(t, err, "a persistent outage must surface, not succeed silently")
	assert.Contains(t, err.Error(), "context deadline exceeded")
	assert.Equal(t, attempts, calls, "it must retry until the budget runs out, then stop")
	assert.Contains(t, stderr.String(), "admission webhook unreachable",
		"the operator must be told why --apply is taking time")
	assert.Contains(t, stderr.String(), "still unreachable (attempt 2,",
		"every retry after the first must report progress")
}

// TestRetryOnTransientWebhookError_TheUnseamedBudgetExpires covers what pinRetryClock
// bypasses: that the real clock ever ends the budget. It asserts the surfaced error
// alone, never an attempt count or the banner, so it needs the 50ms budget to elapse
// eventually rather than any number of attempts to fit inside it.
func TestRetryOnTransientWebhookError_TheUnseamedBudgetExpires(t *testing.T) {
	shrinkRetryPacing(t, 50*time.Millisecond, time.Millisecond)

	done := make(chan error, 1)
	go func() {
		done <- retryOnTransientWebhookError(context.Background(), "EgressProxy/team-a-egress", &bytes.Buffer{}, unreachableErr)
	}()

	select {
	case err := <-done:
		require.Error(t, err, "a persistent outage must surface, not succeed silently")
		assert.Contains(t, err.Error(), "context deadline exceeded")
	case <-time.After(30 * time.Second):
		t.Fatal("a 50ms budget never ended the retry loop")
	}
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
