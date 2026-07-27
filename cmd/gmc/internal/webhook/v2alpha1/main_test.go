package v2alpha1

import (
	"os"
	"testing"

	"github.com/actions-gateway/github-actions-gateway/gmc/internal/logtest"
)

// TestMain fulfills controller-runtime's root logger before any test runs — the
// admission-denied path logs through it, so without this the binary's output
// depends on how long it takes to finish. See package logtest (Q455).
func TestMain(m *testing.M) {
	logtest.Install()
	os.Exit(m.Run())
}
