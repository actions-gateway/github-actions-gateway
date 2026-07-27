package controller

import (
	"os"
	"testing"

	"github.com/actions-gateway/github-actions-gateway/gmc/internal/logtest"
)

// TestMain fulfills controller-runtime's root logger before any test runs, so
// this binary's output does not depend on how long it takes to finish. See
// package logtest for what fires at the 30-second mark otherwise (Q455).
func TestMain(m *testing.M) {
	logtest.Install()
	os.Exit(m.Run())
}
