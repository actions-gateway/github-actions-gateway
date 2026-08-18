package main

// Fake-side derivation (Q871).
//
// The served set comes from the fake answering, not from reading how it
// dispatches. Reading would undercount by construction: handleReposAPI and
// handleRunnerAPI each route several endpoints by path suffix inside a single
// mux.HandleFunc, so the three endpoints Q811's incident turned on are invisible
// to a walk over the registrations. Asking the running server removes the whole
// class of question.
//
// "Serves this path" cannot be read off the status code. A served endpoint 404s
// for a resource that does not exist — deleting an unregistered runner is the
// live example — and that is indistinguishable from the dispatch falling through
// to NotFound. So the fake marks the fall-through itself with unservedHeader,
// and the probe reads that.
//
// Each path is probed under both API base shapes the venue produces. The AGC
// addresses GITHUB_API_BASE_URL, which the e2e suite sets to the fake's root,
// while GithubRegistrar derives the GHES /api/v3 prefix from the org URL for the
// same host. Which one reaches a given endpoint depends on how that caller is
// configured, so an endpoint served under either is in parity; one served under
// neither is the hole this gate exists to fail.

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// unservedHeader is the response header test/fakegithub sets when its dispatch
// found no handler for a path. Kept in step with the const of the same name in
// test/fakegithub/main.go; the gate's own suite fails if the fake stops sending
// it, since a header that silently stopped arriving would read as full parity.
const unservedHeader = "X-Fakegithub-Unserved"

// apiBases are the two roots a caller in this tree addresses the fake at. The
// empty base is GITHUB_API_BASE_URL as the e2e suite sets it; /api/v3 is what
// githubapp.DeriveAPIBaseURL appends for a non-github.com host.
var apiBases = []string{"", "/api/v3"}

// startFake runs the fakegithub binary on loopback ports and returns its base
// URL and a stop function. The binary is passed in rather than built here so the
// gate script owns the build, the way check-metric-tiers.sh owns the checker's.
//
// The child's output goes to a file rather than to this process's stderr. The
// fake logs before it binds, so anything that could block that write would stop
// it ever listening, and a regular file cannot block. It is also what a startup
// failure has to report: the child's own error is the whole diagnosis, and under
// a parallel gate runner it would otherwise be interleaved with 30 other gates.
func startFake(bin, workDir string, client *http.Client) (string, func(), error) {
	// Both ports are held open until the moment before the child binds them, so
	// the kernel cannot hand the same pair to a gate running in parallel. The
	// child's own bind can still lose a race with an unrelated process, which is
	// why exiting early is reported as itself rather than as a readiness timeout.
	ports, release, err := freePorts(2)
	if err != nil {
		return "", nil, err
	}
	addr := fmt.Sprintf("127.0.0.1:%d", ports[0])

	logPath := filepath.Join(workDir, "fakegithub.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		release()
		return "", nil, fmt.Errorf("create %s: %w", logPath, err)
	}
	defer func() { _ = logFile.Close() }()

	// The binary to run is this gate's own argument, supplied by the entry point
	// that just built it; there is no untrusted input on this path.
	cmd := exec.Command(bin) //nolint:gosec // G702: bin is the gate's argument, not user input
	cmd.Env = append(os.Environ(),
		"ADDR="+addr,
		// The control API is not probed, but it binds unconditionally, and a
		// failure there takes the whole process down before the main server
		// reports itself.
		fmt.Sprintf("CONTROL_ADDR=127.0.0.1:%d", ports[1]),
	)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	release()
	if err := cmd.Start(); err != nil {
		return "", nil, fmt.Errorf("start %s: %w", bin, err)
	}
	// exited carries the child's status to whoever polls for it first; done just
	// closes, so stop can wait for the reap whether or not waitReady already took
	// the value. Waiting on exited in both places deadlocks the moment the child
	// dies during startup, which is exactly when the diagnosis is wanted.
	exited := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		exited <- cmd.Wait()
		close(done)
	}()
	stop := func() {
		_ = cmd.Process.Kill()
		<-done
	}
	base := "http://" + addr
	if err := waitReady(base, client, exited, logPath); err != nil {
		stop()
		return "", nil, err
	}
	return base, stop, nil
}

// freePorts asks the kernel for n unused ports and holds them all open until
// release is called, so no two of them can be the same and nothing else in this
// process can take one in between.
func freePorts(n int) ([]int, func(), error) {
	var ls []net.Listener
	var ports []int
	release := func() {
		for _, l := range ls {
			_ = l.Close()
		}
	}
	for range n {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			release()
			return nil, nil, err
		}
		ls = append(ls, l)
		ports = append(ports, l.Addr().(*net.TCPAddr).Port)
	}
	return ports, release, nil
}

// waitReady blocks until the fake answers, or reports why it never will.
// Readiness is any HTTP response at all: the root path is not served, so a reply
// carrying the unserved marker is exactly the proof that dispatch is running.
//
// The three ways this ends want three different next steps, and ten seconds of
// silence looks identical from outside, so the failure says which it was: the
// child exited, or it is still alive and never bound. Its own output is quoted
// either way, since that is where a bind error lands.
func waitReady(base string, client *http.Client, exited <-chan error, logPath string) error {
	deadline := time.Now().Add(readyTimeout)
	var last error
	for time.Now().Before(deadline) {
		select {
		case err := <-exited:
			return fmt.Errorf("fakegithub exited before serving %s: %w%s", base, err, childLog(logPath))
		default:
		}
		resp, err := get(client, base+"/")
		if err == nil {
			_ = resp.Body.Close()
			return nil
		}
		last = err
		time.Sleep(20 * time.Millisecond)
	}
	// One last look, so a child that died just before the deadline is reported as
	// having died rather than as having gone quiet.
	select {
	case err := <-exited:
		return fmt.Errorf("fakegithub exited before serving %s: %w%s", base, err, childLog(logPath))
	default:
	}
	return fmt.Errorf("fakegithub was still running after %s and never accepted on %s (%v)%s",
		readyTimeout, base, last, childLog(logPath))
}

// readyTimeout bounds the wait for the fake to accept. It is generous because
// the gate runs alongside thirty others and the cost of a high bound is only
// paid when something is already wrong.
const readyTimeout = 30 * time.Second

// childLog quotes whatever the fake wrote, which is where a bind failure lands.
// It reports the read failing rather than returning nothing, so an empty log and
// an unreadable one do not look the same.
func childLog(path string) string {
	b, err := os.ReadFile(path)
	switch {
	case err != nil:
		return fmt.Sprintf("\n  (could not read the fake's output at %s: %v)", path, err)
	case len(b) == 0:
		return "\n  (the fake wrote nothing, so it had not reached its first log line)"
	default:
		return "\n  fakegithub said:\n" + indent(string(b))
	}
}

func indent(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		b.WriteString("    " + line + "\n")
	}
	return b.String()
}

// get issues one GET through the probe client. The http.Get helper is forbidden
// here: it uses http.DefaultClient, which carries no timeout (Q138).
func get(client *http.Client, url string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return client.Do(req)
}

// served reports whether the fake dispatched path to a handler, under any of the
// API bases. It returns the bases it tried so a failure can name them.
func served(client *http.Client, base, method, path string) (bool, []string, error) {
	var tried []string
	for _, prefix := range apiBases {
		full := prefix + path
		tried = append(tried, full)
		ok, err := dispatched(client, base, method, full)
		if err != nil {
			return false, tried, err
		}
		if ok {
			return true, tried, nil
		}
	}
	return false, tried, nil
}

// dispatched issues one request and reports whether a handler took it. An
// unresolved method ("*") is sent as GET, which the fake answers with a 405
// rather than the unserved marker when the path exists — still a dispatch.
func dispatched(client *http.Client, base, method, path string) (bool, error) {
	if method == "*" {
		method = http.MethodGet
	}
	req, err := http.NewRequest(method, base+path, strings.NewReader("{}"))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return false, fmt.Errorf("probe %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.Header.Get(unservedHeader) == "", nil
}

// probeClient is the client the probes go through. Redirects are not followed:
// a redirect is a dispatch, and following one would attribute the answer to
// whatever it points at.
func probeClient() *http.Client {
	return &http.Client{
		Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// errNoMarker is returned when the fake answers a path it cannot possibly serve
// without the unserved marker. The gate refuses rather than reporting parity,
// because every negative it is about to take rests on that header arriving.
var errNoMarker = errors.New("fakegithub answered without the " + unservedHeader +
	" marker on a path it does not serve; the parity probe cannot tell a missing endpoint from a served one")

// checkMarker proves the probe's own instrument before any verdict rests on it.
// A path no handler claims must come back marked; if it does not, every endpoint
// would read as served and the gate would pass in exactly the state it exists to
// fail.
func checkMarker(client *http.Client, base string) error {
	// Deliberately not under a GitHub prefix: a canary that shares one with a
	// real route would be claimed by whatever serves that prefix, and the check
	// would report a missing marker on a fake that sends it.
	const canary = "/endpoint-parity-canary"
	ok, err := dispatched(client, base, http.MethodGet, canary)
	if err != nil {
		return err
	}
	if ok {
		return errNoMarker
	}
	return nil
}
