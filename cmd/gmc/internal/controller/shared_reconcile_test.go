package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestProvisioningError_UnwrapReturnsUnderlyingCause verifies Unwrap exposes the
// wrapped cause so errors.Is/As still see through the step annotation (Q156).
func TestProvisioningError_UnwrapReturnsUnderlyingCause(t *testing.T) {
	underlying := assertableErr{"boom"}
	pe := &provisioningError{step: "applyDeployment", err: underlying}

	assert.Equal(t, underlying, pe.Unwrap())
	assert.Equal(t, "applyDeployment: boom", pe.Error())
}

// assertableErr is a trivial comparable error type for exercising Unwrap.
type assertableErr struct{ msg string }

func (e assertableErr) Error() string { return e.msg }
