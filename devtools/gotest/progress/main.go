// Command progress renders a `go test -json` stream as an ordinary `go test`
// log plus a periodic heartbeat naming what is still running (Q618).
//
// The unit tier's silence is measured: the -race gate runs 200 s at 8 % output
// density with 58 s between lines, and a deadlocked test says nothing at all
// until -timeout fires. Plain `go test` cannot do better, because it buffers
// each package's output until that package ends *and* releases packages in
// command-line order, so one slow package holds back every package behind it.
// `go test -json` has neither property — measured: a second package's events
// land while the first is still running — which is what makes a live heartbeat
// possible with no change to a single test.
//
// Usage:
//
//	go test -json ./... | progress -label unit -packages 48 -strip example.com/repo/
//
// stdout is the reconstructed log: a package's buffered output is released when
// the package ends, minus the chunks belonging to tests that passed or skipped.
// That is `go test -v` shaped rather than byte-identical to plain `go test` — a
// failing test keeps its `=== RUN` header and its log lines in emission order,
// where plain output moves the logs under a `--- FAIL` header.
//
// Exits 0 whatever the tests did. The verdict rides on `go test`'s own status
// through the caller's pipefail; a progress renderer must never be the thing
// that fails a gate.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// event is one `go test -json` record. Build diagnostics carry ImportPath and
// no Package; everything else carries Package.
type event struct {
	Action     string
	Package    string
	ImportPath string
	Test       string
	Output     string
	Time       time.Time
}

// chunk is one output record held until its package finishes. test is empty for
// package-level output.
type chunk struct {
	test string
	text string
}

type pkgState struct {
	chunks  []chunk
	settled map[string]bool // tests that ended pass or skip; their output is dropped
}

type runState struct {
	started time.Time
	paused  bool
}

type renderer struct {
	out      io.Writer
	label    string
	total    int
	strip    string
	maxShown int
	started  time.Time

	mu      sync.Mutex
	pkgs    map[string]*pkgState
	running map[string]map[string]*runState
	done    int
	ok      int
	failed  int
	skipped int
}

func newRenderer(out io.Writer, label string, total int, strip string, maxShown int, started time.Time) *renderer {
	return &renderer{
		out:      out,
		label:    label,
		total:    total,
		strip:    strip,
		maxShown: maxShown,
		started:  started,
		pkgs:     map[string]*pkgState{},
		running:  map[string]map[string]*runState{},
	}
}

func (r *renderer) handle(e event) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// A build diagnostic's ImportPath is "pkg [pkg.test]", which never equals
	// the Package the failure is later reported against — measured. It cannot
	// be buffered against a package, so it goes out as it arrives rather than
	// risk a compile error that never prints.
	if e.Action == "build-output" {
		r.writeLocked(e.Output)
		return
	}
	if e.Package == "" {
		return
	}

	switch e.Action {
	case "output":
		p := r.pkg(e.Package)
		p.chunks = append(p.chunks, chunk{test: e.Test, text: e.Output})
	case "run":
		if e.Test != "" {
			r.runners(e.Package)[e.Test] = &runState{started: e.Time}
		}
	case "pause", "cont":
		if s := r.running[e.Package][e.Test]; s != nil {
			s.paused = e.Action == "pause"
		}
	case "pass", "fail", "skip":
		if e.Test == "" {
			r.flush(e.Package)
			delete(r.pkgs, e.Package)
			delete(r.running, e.Package)
			r.done++
			return
		}
		delete(r.running[e.Package], e.Test)
		if e.Action != "fail" {
			r.pkg(e.Package).settled[e.Test] = true
		}
		// Subtests are counted through their parent: they are created at run
		// time, so a tally over them has no stable meaning between runs.
		if !strings.Contains(e.Test, "/") {
			switch e.Action {
			case "pass":
				r.ok++
			case "fail":
				r.failed++
			case "skip":
				r.skipped++
			}
		}
	}
}

func (r *renderer) pkg(name string) *pkgState {
	p := r.pkgs[name]
	if p == nil {
		p = &pkgState{settled: map[string]bool{}}
		r.pkgs[name] = p
	}
	return p
}

func (r *renderer) runners(name string) map[string]*runState {
	m := r.running[name]
	if m == nil {
		m = map[string]*runState{}
		r.running[name] = m
	}
	return m
}

// flush releases a finished package's output. Chunks from a test that passed or
// skipped are dropped, which is what keeps the log plain-`go test` shaped
// instead of verbose. The bare "PASS" line goes with them: cmd/go swallows it
// and prints its own "ok <pkg> <elapsed>", which arrives as a later chunk.
func (r *renderer) flush(name string) {
	p := r.pkgs[name]
	if p == nil {
		return
	}
	var b strings.Builder
	for _, c := range p.chunks {
		switch {
		case c.test != "" && p.settled[c.test]:
		case c.test == "" && c.text == "PASS\n":
		default:
			b.WriteString(c.text)
		}
	}
	r.writeLocked(b.String())
}

// emitHeartbeat renders and prints the progress line in one critical section,
// so a heartbeat cannot land in the middle of a package's output.
func (r *renderer) emitHeartbeat(now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.writeLocked(r.heartbeatLine(now) + "\n")
}

// heartbeatLine renders the progress line for now. Callers hold r.mu.
func (r *renderer) heartbeatLine(now time.Time) string {
	pkgs := fmt.Sprintf("%d", r.done)
	if r.total > 0 {
		pkgs = fmt.Sprintf("%d/%d", r.done, r.total)
	}
	return fmt.Sprintf("[%s t+%s] %s pkgs | %d ok, %d failed, %d skipped | running: %s",
		r.label, clock(now.Sub(r.started)), pkgs, r.ok, r.failed, r.skipped, r.runningText(now))
}

// runningText names the tests currently executing, longest-running first. A
// parent whose subtest is running is omitted: the leaf is what identifies where
// a hung run is stuck.
func (r *renderer) runningText(now time.Time) string {
	type entry struct {
		name    string
		started time.Time
	}
	var entries []entry
	for pkg, tests := range r.running {
		for test, s := range tests {
			if s.paused || hasRunningChild(tests, test) {
				continue
			}
			entries = append(entries, entry{strings.TrimPrefix(pkg, r.strip) + "." + test, s.started})
		}
	}
	if len(entries) == 0 {
		return "none"
	}
	sort.Slice(entries, func(i, j int) bool {
		if !entries[i].started.Equal(entries[j].started) {
			return entries[i].started.Before(entries[j].started)
		}
		return entries[i].name < entries[j].name
	})

	shown := entries
	if r.maxShown > 0 && len(shown) > r.maxShown {
		shown = shown[:r.maxShown]
	}
	parts := make([]string, 0, len(shown)+1)
	for _, e := range shown {
		parts = append(parts, fmt.Sprintf("%s (%s)", e.name, dur(now.Sub(e.started))))
	}
	if len(shown) < len(entries) {
		parts = append(parts, fmt.Sprintf("+%d more", len(entries)-len(shown)))
	}
	return strings.Join(parts, ", ")
}

func hasRunningChild(tests map[string]*runState, name string) bool {
	for other := range tests {
		if strings.HasPrefix(other, name+"/") {
			return true
		}
	}
	return false
}

// writeLocked prints s. Callers hold r.mu, which is what serializes a heartbeat
// against a package flush. Best-effort by design rule: a failed write skips
// output rather than killing the run it reports on.
func (r *renderer) writeLocked(s string) {
	if s == "" {
		return
	}
	_, _ = io.WriteString(r.out, s)
}

func clock(d time.Duration) string {
	s := int(d.Seconds())
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

func dur(d time.Duration) string {
	s := int(d.Seconds())
	if s < 60 {
		return fmt.Sprintf("%ds", s)
	}
	return fmt.Sprintf("%dm%02ds", s/60, s%60)
}

// startHeartbeat prints a progress line every interval until stop closes. The
// returned channel closes once the ticker has stopped.
func startHeartbeat(r *renderer, interval time.Duration, stop <-chan struct{}) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case now := <-t.C:
				r.emitHeartbeat(now)
			case <-stop:
				return
			}
		}
	}()
	return done
}

// run consumes a `go test -json` stream. A line that is not a JSON event is
// echoed verbatim: the alternative is discarding output nobody else will print.
func run(r *renderer, in io.Reader) {
	br := bufio.NewReader(in)
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			var e event
			if json.Unmarshal([]byte(line), &e) == nil && e.Action != "" {
				r.handle(e)
			} else {
				r.mu.Lock()
				r.writeLocked(line)
				r.mu.Unlock()
			}
		}
		if err != nil {
			return
		}
	}
}

func main() {
	label := flag.String("label", "test", "tier name shown in the heartbeat prefix")
	interval := flag.Duration("interval", 30*time.Second, "heartbeat cadence; 0 prints only the closing summary")
	total := flag.Int("packages", 0, "package count for the heartbeat denominator; 0 omits it")
	strip := flag.String("strip", "", "import-path prefix trimmed from package names in the heartbeat")
	maxShown := flag.Int("running", 4, "most running tests named on one heartbeat line")
	flag.Parse()

	r := newRenderer(os.Stdout, *label, *total, *strip, *maxShown, time.Now())

	stop := make(chan struct{})
	var ticking <-chan struct{}
	if *interval > 0 {
		ticking = startHeartbeat(r, *interval, stop)
	}

	run(r, os.Stdin)

	close(stop)
	if ticking != nil {
		<-ticking
	}
	r.emitHeartbeat(time.Now())
}
