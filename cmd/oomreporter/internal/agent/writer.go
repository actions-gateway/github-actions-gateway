package agent

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/actions-gateway/github-actions-gateway/oomreporter/internal/report"
)

const (
	// Two writes are minGap to maxGap apart, and quiet resets that.
	minGap = time.Second
	maxGap = 8 * time.Second
	quiet  = 30 * time.Second
)

// Writer writes the newest report there is, and again while that fails.
type Writer struct {
	// Put writes a report.
	Put func(ctx context.Context, data string) error
	Log *slog.Logger

	changed chan struct{}
	mu      sync.Mutex
	kills   []report.Kill
	lines   []report.Line
}

// NewWriter is a Writer for put.
func NewWriter(put func(ctx context.Context, data string) error, log *slog.Logger) *Writer {
	return &Writer{Put: put, Log: log, changed: make(chan struct{}, 1)}
}

// Write has kills written, with the newest lines, in place of any not yet.
func (w *Writer) Write(kills []report.Kill) {
	w.mu.Lock()
	w.kills = kills
	w.mu.Unlock()
	w.poke()
}

// WriteLines has lines written, with the newest kills, in place of any not yet.
func (w *Writer) WriteLines(lines []report.Line) {
	w.mu.Lock()
	w.lines = lines
	w.mu.Unlock()
	w.poke()
}

func (w *Writer) poke() {
	select {
	case w.changed <- struct{}{}:
	default:
	}
}

// Run writes until ctx is done.
func (w *Writer) Run(ctx context.Context) {
	pending := false
	var last time.Time
	gap := minGap
	for {
		if !pending {
			select {
			case <-ctx.Done():
				return
			case <-w.changed:
			}
			if time.Since(last) >= quiet {
				gap = minGap
			}
		}
		if !sleep(ctx, time.Until(last.Add(gap))) {
			return
		}
		select {
		case <-w.changed:
		default:
		}
		ok := w.put(ctx)
		last, gap = time.Now(), min(2*gap, maxGap)
		pending = !ok
	}
}

func (w *Writer) put(ctx context.Context) bool {
	w.mu.Lock()
	kills, lines := w.kills, w.lines
	w.mu.Unlock()
	data, err := report.Encode(kills, lines)
	if err == nil {
		err = w.Put(ctx, data)
	}
	if err != nil {
		w.Log.Warn("cannot write the report", "kills", len(kills), "lines", len(lines), "error", err)
	}
	return err == nil
}

// sleep waits for d, and reports whether ctx is not done.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}
