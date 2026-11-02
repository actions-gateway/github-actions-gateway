package controller

import (
	"net"
	"testing"
	"time"

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

// TestRefreshStalledMessage pins both RefreshStalled messages: a cache still serving
// ranges predicts drift, and one whose seed has expired serves nothing, so the
// message says egress is denied rather than understating it.
func TestRefreshStalledMessage(t *testing.T) {
	now := time.Now()

	serving := &IPRangeCache{}
	serving.Set([]net.IPNet{{IP: net.IPv4(140, 82, 112, 0), Mask: net.CIDRMask(20, 32)}})
	serving.MarkRefreshed(now.Add(-72 * time.Hour))
	assert.Contains(t, refreshStalledMessage(serving, 72*time.Hour, 49*time.Hour), "silently dropped")

	expired := &IPRangeCache{now: func() time.Time { return now }}
	expired.Seed(nil, now.Add(-50*time.Hour), now.Add(-time.Hour))
	assert.Contains(t, refreshStalledMessage(expired, 50*time.Hour, 49*time.Hour), "GitHub egress is denied",
		"an expired seed serves nothing, which the message must not understate")
}
