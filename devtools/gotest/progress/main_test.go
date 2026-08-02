package main

import (
	"strings"
	"testing"
	"time"
)

// base is the zero point every fixture's timestamps are offsets from.
var base = time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)

// at renders a timestamp d into a fixture event.
func at(d time.Duration) string {
	return base.Add(d).Format(time.RFC3339Nano)
}

// render feeds lines through a renderer and returns what it printed.
func render(t *testing.T, lines ...string) (string, *renderer) {
	t.Helper()
	var out strings.Builder
	r := newRenderer(&out, "unit", 0, "", 4, base)
	run(r, strings.NewReader(strings.Join(lines, "\n")+"\n"))
	return out.String(), r
}

func TestPassingPackagePrintsOnlyItsSummary(t *testing.T) {
	got, _ := render(t,
		`{"Action":"start","Package":"repo/b"}`,
		`{"Action":"run","Package":"repo/b","Test":"TestB"}`,
		`{"Action":"output","Package":"repo/b","Test":"TestB","Output":"=== RUN   TestB\n"}`,
		`{"Action":"output","Package":"repo/b","Test":"TestB","Output":"    b_test.go:9: chatty\n"}`,
		`{"Action":"output","Package":"repo/b","Test":"TestB","Output":"--- PASS: TestB (0.00s)\n"}`,
		`{"Action":"pass","Package":"repo/b","Test":"TestB"}`,
		`{"Action":"output","Package":"repo/b","Output":"PASS\n"}`,
		`{"Action":"output","Package":"repo/b","Output":"ok  \trepo/b\t0.440s\n"}`,
		`{"Action":"pass","Package":"repo/b"}`,
	)

	// Exact, not "contains": the bare "PASS" the test binary prints is dropped
	// here because cmd/go swallows it too, and only equality catches it coming
	// back.
	if want := "ok  \trepo/b\t0.440s\n"; got != want {
		t.Errorf("passing package output = %q, want %q", got, want)
	}
}

func TestFailingPackageKeepsOnlyTheFailingTest(t *testing.T) {
	got, _ := render(t,
		`{"Action":"run","Package":"repo/c","Test":"TestPass"}`,
		`{"Action":"output","Package":"repo/c","Test":"TestPass","Output":"=== RUN   TestPass\n"}`,
		`{"Action":"output","Package":"repo/c","Test":"TestPass","Output":"    c_test.go:5: noise\n"}`,
		`{"Action":"pass","Package":"repo/c","Test":"TestPass"}`,
		`{"Action":"run","Package":"repo/c","Test":"TestFail"}`,
		`{"Action":"output","Package":"repo/c","Test":"TestFail","Output":"=== RUN   TestFail\n"}`,
		`{"Action":"output","Package":"repo/c","Test":"TestFail","Output":"    c_test.go:6: boom\n"}`,
		`{"Action":"output","Package":"repo/c","Test":"TestFail","Output":"--- FAIL: TestFail (0.00s)\n"}`,
		`{"Action":"fail","Package":"repo/c","Test":"TestFail"}`,
		`{"Action":"run","Package":"repo/c","Test":"TestSkip"}`,
		`{"Action":"output","Package":"repo/c","Test":"TestSkip","Output":"    c_test.go:7: nope\n"}`,
		`{"Action":"skip","Package":"repo/c","Test":"TestSkip"}`,
		`{"Action":"output","Package":"repo/c","Output":"FAIL\n"}`,
		`{"Action":"output","Package":"repo/c","Output":"FAIL\trepo/c\t0.289s\n"}`,
		`{"Action":"fail","Package":"repo/c"}`,
	)

	want := "=== RUN   TestFail\n    c_test.go:6: boom\n--- FAIL: TestFail (0.00s)\nFAIL\nFAIL\trepo/c\t0.289s\n"
	if got != want {
		t.Errorf("failing package output = %q, want %q", got, want)
	}
}

// A hung test never reaches a terminal event, so nothing marks its output
// settled — which is exactly what must keep the timeout's goroutine dump.
func TestOutputOfATestThatNeverEndsIsKept(t *testing.T) {
	got, _ := render(t,
		`{"Action":"run","Package":"repo/h","Test":"TestHang"}`,
		`{"Action":"output","Package":"repo/h","Test":"TestHang","Output":"panic: test timed out after 5m0s\n"}`,
		`{"Action":"output","Package":"repo/h","Output":"FAIL\trepo/h\t300.1s\n"}`,
		`{"Action":"fail","Package":"repo/h"}`,
	)

	if !strings.Contains(got, "panic: test timed out") {
		t.Errorf("output = %q, want the timeout panic kept", got)
	}
}

// Build diagnostics carry an ImportPath ("pkg [pkg.test]") that never matches
// the Package the failure is reported against, so buffering them against a
// package would drop compile errors entirely.
func TestBuildOutputIsPrintedImmediately(t *testing.T) {
	var out strings.Builder
	r := newRenderer(&out, "unit", 0, "", 4, base)
	run(r, strings.NewReader(
		`{"ImportPath":"repo/e [repo/e.test]","Action":"build-output","Output":"# repo/e [repo/e.test]\n"}`+"\n"+
			`{"ImportPath":"repo/e [repo/e.test]","Action":"build-output","Output":"e/e_test.go:5:33: undefined: nope\n"}`+"\n"+
			`{"ImportPath":"repo/e [repo/e.test]","Action":"build-fail"}`+"\n"))

	want := "# repo/e [repo/e.test]\ne/e_test.go:5:33: undefined: nope\n"
	if got := out.String(); got != want {
		t.Errorf("build output = %q, want %q", got, want)
	}
}

func TestUnparseableLineIsEchoed(t *testing.T) {
	got, _ := render(t, `go: downloading example.com/x v1.2.3`)

	if !strings.HasPrefix(got, "go: downloading example.com/x v1.2.3\n") {
		t.Errorf("output = %q, want the raw line echoed", got)
	}
}

func TestHeartbeatNamesRunningTestsLongestFirst(t *testing.T) {
	_, r := render(t,
		`{"Action":"pass","Package":"repo/done"}`,
		`{"Time":"`+at(10*time.Second)+`","Action":"run","Package":"repo/a","Test":"TestSlow"}`,
		`{"Time":"`+at(70*time.Second)+`","Action":"run","Package":"repo/b","Test":"TestQuick"}`,
	)
	r.total = 12
	r.strip = "repo/"

	got := r.heartbeatLine(base.Add(100 * time.Second))
	want := "[unit t+1:40] 1/12 pkgs | 0 ok, 0 failed, 0 skipped | running: a.TestSlow (1m30s), b.TestQuick (30s)"
	if got != want {
		t.Errorf("heartbeat = %q, want %q", got, want)
	}
}

func TestHeartbeatOmitsPausedTestsAndParentsOfRunningSubtests(t *testing.T) {
	_, r := render(t,
		`{"Time":"`+at(0)+`","Action":"run","Package":"repo/a","Test":"TestParallel"}`,
		`{"Time":"`+at(0)+`","Action":"pause","Package":"repo/a","Test":"TestParallel"}`,
		`{"Time":"`+at(1*time.Second)+`","Action":"run","Package":"repo/a","Test":"TestTable"}`,
		`{"Time":"`+at(2*time.Second)+`","Action":"run","Package":"repo/a","Test":"TestTable/case-3"}`,
	)

	got := r.heartbeatLine(base.Add(10 * time.Second))
	if want := "running: repo/a.TestTable/case-3 (8s)"; !strings.HasSuffix(got, want) {
		t.Errorf("heartbeat = %q, want it to end with %q", got, want)
	}
}

func TestHeartbeatShowsAResumedTestAgain(t *testing.T) {
	_, r := render(t,
		`{"Time":"`+at(0)+`","Action":"run","Package":"repo/a","Test":"TestParallel"}`,
		`{"Time":"`+at(0)+`","Action":"pause","Package":"repo/a","Test":"TestParallel"}`,
		`{"Time":"`+at(3*time.Second)+`","Action":"cont","Package":"repo/a","Test":"TestParallel"}`,
	)

	got := r.heartbeatLine(base.Add(10 * time.Second))
	// The elapsed stays measured from `run`: a caller wants how long the test
	// has been outstanding, not how long since the scheduler resumed it.
	if want := "running: repo/a.TestParallel (10s)"; !strings.HasSuffix(got, want) {
		t.Errorf("heartbeat = %q, want it to end with %q", got, want)
	}
}

func TestHeartbeatCapsTheRunningList(t *testing.T) {
	lines := []string{}
	for _, name := range []string{"TestA", "TestB", "TestC", "TestD", "TestE", "TestF"} {
		lines = append(lines, `{"Time":"`+at(0)+`","Action":"run","Package":"repo/a","Test":"`+name+`"}`)
	}
	_, r := render(t, lines...)

	got := r.heartbeatLine(base.Add(5 * time.Second))
	if want := "running: repo/a.TestA (5s), repo/a.TestB (5s), repo/a.TestC (5s), repo/a.TestD (5s), +2 more"; !strings.HasSuffix(got, want) {
		t.Errorf("heartbeat = %q, want it to end with %q", got, want)
	}
}

// Subtests are created at run time, so counting them would make the tally
// depend on which table cases happened to execute.
func TestHeartbeatCountsTopLevelTestsOnly(t *testing.T) {
	_, r := render(t,
		`{"Action":"pass","Package":"repo/a","Test":"TestTable/case-1"}`,
		`{"Action":"pass","Package":"repo/a","Test":"TestTable/case-2"}`,
		`{"Action":"pass","Package":"repo/a","Test":"TestTable"}`,
		`{"Action":"fail","Package":"repo/a","Test":"TestBroken"}`,
		`{"Action":"skip","Package":"repo/a","Test":"TestSkipped"}`,
	)

	got := r.heartbeatLine(base)
	if want := "1 ok, 1 failed, 1 skipped"; !strings.Contains(got, want) {
		t.Errorf("heartbeat = %q, want it to contain %q", got, want)
	}
}

func TestHeartbeatOmitsTheDenominatorWhenUnknown(t *testing.T) {
	_, r := render(t, `{"Action":"pass","Package":"repo/a"}`)

	got := r.heartbeatLine(base)
	if want := "] 1 pkgs |"; !strings.Contains(got, want) {
		t.Errorf("heartbeat = %q, want it to contain %q", got, want)
	}
}

func TestHeartbeatCountsEveryFinishedPackage(t *testing.T) {
	_, r := render(t,
		`{"Action":"pass","Package":"repo/a"}`,
		`{"Action":"skip","Package":"repo/b"}`,
		`{"Action":"fail","Package":"repo/c"}`,
	)

	got := r.heartbeatLine(base)
	if want := "3 pkgs"; !strings.Contains(got, want) {
		t.Errorf("summary = %q, want it to contain %q", got, want)
	}
}

func TestStartHeartbeatStopsWhenAsked(t *testing.T) {
	var out strings.Builder
	r := newRenderer(&out, "unit", 0, "", 4, base)

	stop := make(chan struct{})
	done := startHeartbeat(r, time.Millisecond, stop)
	close(stop)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("startHeartbeat did not close its done channel after stop")
	}
}
