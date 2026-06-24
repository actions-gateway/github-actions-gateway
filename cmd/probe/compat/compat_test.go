package compat

import (
	"context"
	"os"
	"testing"
)

// reportPath is the committed report, relative to this package directory
// (cmd/probe/compat → repo root is three levels up).
const reportPath = "../../../docs/development/broker-compatibility.md"

// TestCompat runs every broker-compatibility check. It is the runnable suite
// wired into `make check`/CI: a failing check means the broker.Client no longer
// honours a documented wire-protocol/API contract.
func TestCompat(t *testing.T) {
	ctx := context.Background()
	for _, c := range Checks() {
		t.Run(c.ID+"_"+c.Title, func(t *testing.T) {
			if err := c.Run(ctx); err != nil {
				t.Fatalf("%s (%s): %v", c.Title, c.Contract, err)
			}
		})
	}
}

// TestReportInSync renders the compatibility report from the live check results
// and asserts the committed docs/development/broker-compatibility.md matches.
// Set COMPAT_WRITE_REPORT=1 (or run `make compat-report`) to regenerate it.
func TestReportInSync(t *testing.T) {
	got := Report(RunAll(context.Background()))

	if os.Getenv("COMPAT_WRITE_REPORT") == "1" {
		if err := os.WriteFile(reportPath, []byte(got), 0o600); err != nil {
			t.Fatalf("write report: %v", err)
		}
		t.Logf("wrote %s", reportPath)
		return
	}

	want, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("read committed report (run `make compat-report`): %v", err)
	}
	if got != string(want) {
		t.Fatalf("%s is out of date — regenerate with `make compat-report`", reportPath)
	}
}
