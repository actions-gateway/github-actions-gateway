package compat

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
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

// TestReport_FailingCheck drives Report's failure-summary branches — the
// "⚠️ One or more contracts are failing" header and the per-row "❌ FAIL"
// status — which the always-green TestReportInSync never reaches because
// every real check passes. A synthetic failing Check exercises them without
// touching the real catalogue.
func TestReport_FailingCheck(t *testing.T) {
	results := []Result{
		{
			Check: Check{ID: "C01", Title: "ok check", Contract: "§ ok", Asserts: "passes"},
			Err:   nil,
		},
		{
			Check: Check{ID: "C02", Title: "broken check", Contract: "§ broken", Asserts: "fails"},
			Err:   errors.New("simulated failure"),
		},
	}

	got := Report(results)

	if !strings.Contains(got, "**1/2 contracts verified.**") {
		t.Errorf("Report did not render the pass/total summary; got:\n%s", got)
	}
	if !strings.Contains(got, "⚠️ One or more contracts are failing") {
		t.Errorf("Report did not render the failure-summary line for a mixed pass/fail result set")
	}
	if !strings.Contains(got, "| C01 | § ok | passes | ✅ PASS |") {
		t.Errorf("Report did not render a PASS row for the passing check")
	}
	if !strings.Contains(got, "| C02 | § broken | fails | ❌ FAIL |") {
		t.Errorf("Report did not render a FAIL row for the failing check")
	}
}

// TestEscapePipes verifies the markdown-table delimiter escape used when
// rendering a Check's Contract/Asserts fields into the report table.
func TestEscapePipes(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"no pipes", "plain text", "plain text"},
		{"single pipe", "a|b", `a\|b`},
		{"multiple pipes", "a|b|c", `a\|b\|c`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := escapePipes(tt.in); got != tt.want {
				t.Errorf("escapePipes(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestEncryptCBC_InvalidKeySize drives encryptCBC's error branch: aes.NewCipher
// rejects any key length other than 16/24/32 bytes, which the message-body
// crypto check's producer helper must surface as an error rather than panic.
func TestEncryptCBC_InvalidKeySize(t *testing.T) {
	_, err := encryptCBC([]byte("too-short-key"), []byte("plaintext"))
	if err == nil {
		t.Fatal("encryptCBC with a 13-byte key: want error, got nil")
	}
}

// TestPkcs7Pad_PadsToBlockSize confirms pkcs7Pad's output is always a
// multiple of blockSize and that every appended pad byte carries the pad
// length (the PKCS#7 invariant broker.DecryptMessageBody strips on decode).
func TestPkcs7Pad_PadsToBlockSize(t *testing.T) {
	tests := []struct {
		name      string
		data      []byte
		blockSize int
	}{
		{"needs full block of padding", []byte("0123456789012345"), 16}, // 17 bytes -> pad to 32
		{"needs partial padding", []byte("hello"), 16},
		{"empty input", []byte{}, 16},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := pkcs7Pad(append([]byte(nil), tt.data...), tt.blockSize)
			if len(got)%tt.blockSize != 0 {
				t.Fatalf("pkcs7Pad output length %d is not a multiple of block size %d", len(got), tt.blockSize)
			}
			padLen := int(got[len(got)-1])
			if len(got)-len(tt.data) != padLen {
				t.Fatalf("pad length byte = %d, want %d (appended bytes)", padLen, len(got)-len(tt.data))
			}
			for _, b := range got[len(got)-padLen:] {
				if int(b) != padLen {
					t.Fatalf("pad byte = %d, want %d (every pad byte must equal the pad length)", b, padLen)
				}
			}
		})
	}
}

// TestAcquireJobServer_UnknownPath404s drives acquireJobServer's default
// branch: any path other than /acquirejob must 404, confirming the stub only
// answers the one route it advertises rather than acting as a catch-all.
func TestAcquireJobServer_UnknownPath404s(t *testing.T) {
	srv := acquireJobServer("plan-1", "")
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/not-acquirejob")
	if err != nil {
		t.Fatalf("GET unknown path: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for a path other than /acquirejob", resp.StatusCode)
	}
}

// TestCheckJobDelivery_CreateSessionError drives checkJobDelivery's first
// error branch: a context that is already cancelled makes the initial
// CreateSession call fail, so the check must return a wrapped error rather
// than proceed to enqueue or poll for a job.
func TestCheckJobDelivery_CreateSessionError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := checkJobDelivery(ctx)
	if err == nil {
		t.Fatal("checkJobDelivery with a cancelled context: want error, got nil")
	}
	if !strings.Contains(err.Error(), "CreateSession") {
		t.Errorf("error = %q, want it to name CreateSession as the failing step", err.Error())
	}
}

// TestCheckSessionReuse_CreateSessionError mirrors
// TestCheckJobDelivery_CreateSessionError for checkSessionReuse's identical
// first branch.
func TestCheckSessionReuse_CreateSessionError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := checkSessionReuse(ctx)
	if err == nil {
		t.Fatal("checkSessionReuse with a cancelled context: want error, got nil")
	}
	if !strings.Contains(err.Error(), "CreateSession") {
		t.Errorf("error = %q, want it to name CreateSession as the failing step", err.Error())
	}
}

// TestCheckAcknowledgeOptional_CreateSessionError mirrors the above for
// checkAcknowledgeOptional's identical first branch.
func TestCheckAcknowledgeOptional_CreateSessionError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := checkAcknowledgeOptional(ctx)
	if err == nil {
		t.Fatal("checkAcknowledgeOptional with a cancelled context: want error, got nil")
	}
	if !strings.Contains(err.Error(), "CreateSession") {
		t.Errorf("error = %q, want it to name CreateSession as the failing step", err.Error())
	}
}

// TestCheckCreateSession_CreateSessionError mirrors the above for
// checkCreateSession's identical first branch.
func TestCheckCreateSession_CreateSessionError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := checkCreateSession(ctx)
	if err == nil {
		t.Fatal("checkCreateSession with a cancelled context: want error, got nil")
	}
	if !strings.Contains(err.Error(), "CreateSession") {
		t.Errorf("error = %q, want it to name CreateSession as the failing step", err.Error())
	}
}

// TestCheckMessageEmpty_CreateSessionError mirrors the above for
// checkMessageEmpty's identical first branch.
func TestCheckMessageEmpty_CreateSessionError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := checkMessageEmpty(ctx)
	if err == nil {
		t.Fatal("checkMessageEmpty with a cancelled context: want error, got nil")
	}
	if !strings.Contains(err.Error(), "CreateSession") {
		t.Errorf("error = %q, want it to name CreateSession as the failing step", err.Error())
	}
}

// TestCheckPlanIDPrecedence_AcquireJobError drives checkPlanIDPrecedence's
// first branch: a cancelled context fails the initial AcquireJob call against
// the header-precedence stub before the body-fallback stub is ever reached.
func TestCheckPlanIDPrecedence_AcquireJobError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := checkPlanIDPrecedence(ctx)
	if err == nil {
		t.Fatal("checkPlanIDPrecedence with a cancelled context: want error, got nil")
	}
	if !strings.Contains(err.Error(), "AcquireJob (header)") {
		t.Errorf("error = %q, want it to name the header-precedence AcquireJob call as the failing step", err.Error())
	}
}

// TestCheckRenewJob_RenewJobError drives checkRenewJob's only error branch: a
// cancelled context fails the RenewJob call itself.
func TestCheckRenewJob_RenewJobError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := checkRenewJob(ctx)
	if err == nil {
		t.Fatal("checkRenewJob with a cancelled context: want error, got nil")
	}
	if !strings.Contains(err.Error(), "RenewJob") {
		t.Errorf("error = %q, want it to name RenewJob as the failing step", err.Error())
	}
}

// TestCheckTwoURLModel_AcquireJobError drives checkTwoURLModel's first
// branch: a cancelled context fails the AcquireJob call against the run
// service before the broker-host cached-URL scenario is ever exercised.
func TestCheckTwoURLModel_AcquireJobError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := checkTwoURLModel(ctx)
	if err == nil {
		t.Fatal("checkTwoURLModel with a cancelled context: want error, got nil")
	}
	if !strings.Contains(err.Error(), "AcquireJob on run_service_url") {
		t.Errorf("error = %q, want it to name the run-service AcquireJob call as the failing step", err.Error())
	}
}

// TestCheckRenewJobToken_AcquireJobError drives checkRenewJobToken's first
// branch: a cancelled context fails the initial AcquireJob call before the
// job-token vs session-token renewal comparison is ever reached.
func TestCheckRenewJobToken_AcquireJobError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := checkRenewJobToken(ctx)
	if err == nil {
		t.Fatal("checkRenewJobToken with a cancelled context: want error, got nil")
	}
	if !strings.Contains(err.Error(), "AcquireJob") {
		t.Errorf("error = %q, want it to name AcquireJob as the failing step", err.Error())
	}
}
