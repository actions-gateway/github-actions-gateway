package controller

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/scalesetlistener"
	"github.com/actions-gateway/github-actions-gateway/api/v2"
	"github.com/stretchr/testify/assert"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Q1330: the fork policy is read from the live RunnerSet per job, and only an explicit
// Accept admits a fork pull request's job.
func TestAcceptsForkPullRequests(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy string
		want   bool
	}{
		{name: "unset refuses", policy: "", want: false},
		{name: "Refuse refuses", policy: v2.ForkPullRequestsRefuse, want: false},
		{name: "Accept accepts", policy: v2.ForkPullRequestsAccept, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rs := rsObj("set", "ns", func(r *v2.RunnerSet) { r.Spec.ForkPullRequests = tc.policy })
			r := &RunnerSetReconciler{Client: fake.NewClientBuilder().WithScheme(runnerSetTestScheme(t)).WithObjects(rs).Build()}
			assert.Equal(t, tc.want, r.acceptsForkPullRequests(context.Background(), keyOf(rs)))
		})
	}

	t.Run("an unreadable RunnerSet refuses", func(t *testing.T) {
		r := &RunnerSetReconciler{Client: fake.NewClientBuilder().WithScheme(runnerSetTestScheme(t)).Build()}
		assert.False(t, r.acceptsForkPullRequests(context.Background(), keyOf(rsObj("set", "ns", nil))))
	})
}

// A refusal must reach the listener as ErrJobRefused, or the listener reads it as a
// check that could not be made and holds the job forever.
func TestListenerProvisionErrorTranslatesForkRefusal(t *testing.T) {
	refused := fmt.Errorf("%w: head repository %q", provisioner.ErrForkJobRefused, "contributor/widgets")
	assert.True(t, errors.Is(listenerProvisionError(refused), scalesetlistener.ErrJobRefused))

	held := errors.New("run-status API returned 502")
	assert.False(t, errors.Is(listenerProvisionError(held), scalesetlistener.ErrJobRefused))
}
