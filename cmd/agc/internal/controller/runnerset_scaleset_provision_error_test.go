package controller

import (
	"errors"
	"fmt"
	"testing"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/scalesetlistener"
	"github.com/stretchr/testify/assert"
)

// The listener re-offers a job only for an error wrapping one of its own sentinels; any
// other error holds the queue cursor and redelivers immediately. So each provisioner
// verdict that must wait has to arrive translated (Q576, Q1199), and a transient error
// must not.
func TestListenerProvisionError(t *testing.T) {
	ceiling := fmt.Errorf("provision: %w", &provisioner.CeilingReachedError{ActivePods: 8})
	terminating := fmt.Errorf("provision: %w", &provisioner.WorkerTerminatingError{Pod: "gpu-job-a"})
	transient := errors.New("apiserver: 500")

	assert.ErrorIs(t, listenerProvisionError(ceiling), scalesetlistener.ErrCapacityUnavailable)
	assert.ErrorIs(t, listenerProvisionError(terminating), scalesetlistener.ErrWorkerTerminating)
	assert.NotErrorIs(t, listenerProvisionError(terminating), scalesetlistener.ErrCapacityUnavailable,
		"a terminating worker is not reported as a full ceiling")
	assert.Same(t, transient, listenerProvisionError(transient), "a transient error travels as-is, to be redelivered")
	assert.NoError(t, listenerProvisionError(nil))
}
