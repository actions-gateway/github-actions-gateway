package migrate

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestRenderWarningsComment_FormatsEachWarningAsABulletLine asserts the
// warnings block is a "# Warnings:" header followed by one "#   - <text>" line
// per warning, in the given order, with no trailing newline.
func TestRenderWarningsComment_FormatsEachWarningAsABulletLine(t *testing.T) {
	got := renderWarningsComment([]string{
		"proxy egressPolicyMode defaulted to CIDR",
		"namespace already carries the v2 security-profile label",
	})
	want := "# Warnings:\n" +
		"#   - proxy egressPolicyMode defaulted to CIDR\n" +
		"#   - namespace already carries the v2 security-profile label"
	assert.Equal(t, want, got)
}

// TestRenderWarningsComment_SingleWarning covers the one-element case
// separately from the multi-line join logic exercised above.
func TestRenderWarningsComment_SingleWarning(t *testing.T) {
	got := renderWarningsComment([]string{"only warning"})
	assert.Equal(t, "# Warnings:\n#   - only warning", got)
}
