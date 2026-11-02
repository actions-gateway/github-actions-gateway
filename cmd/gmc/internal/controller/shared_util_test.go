package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFormatResourceAttributes_Deterministic(t *testing.T) {
	attrs := map[string]string{"z": "1", "a": "2", "m": "3"}
	// Sorted-key rendering must be identical on repeated calls.
	assert.Equal(t, "a=2,m=3,z=1", formatResourceAttributes(attrs))
	assert.Equal(t, "a=2,m=3,z=1", formatResourceAttributes(attrs))
	assert.Equal(t, "", formatResourceAttributes(nil))
}
