package agent

import (
	"context"
	"log/slog"
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

	newest chan []report.Kill
}

// NewWriter is a Writer for put.
func NewWriter(put func(ctx context.Context, data string) error, log *slog.Logger) *Writer {
	return &Writer{Put: put, Log: log, newest: make(chan []report.Kill, 1)}
}

// Write has kills written, in place of any not yet. One goroutine calls it.
func (w *Writer) Write(kills []report.Kill) {
	select {
	case <-w.newest:
	default:
	}
	w.newest <- kills
}

// Run writes until ctx is done.
func (w *Writer) Run(ctx context.Context) {
	var pending []report.Kill
	var last time.Time
	gap := minGap
	for {
		if pending == nil {
			select {
			case <-ctx.Done():
				return
			case pending = <-w.newest:
			}
			if time.Since(last) >= quiet {
				gap = minGap
			}
		}
		if !sleep(ctx, time.Until(last.Add(gap))) {
			return
		}
		select {
		case pending = <-w.newest:
		default:
		}
		ok := w.put(ctx, pending)
		last, gap = time.Now(), min(2*gap, maxGap)
		if ok {
			pending = nil
		}
	}
}

func (w *Writer) put(ctx context.Context, kills []report.Kill) bool {
	data, err := report.Encode(kills)
	if err == nil {
		err = w.Put(ctx, data)
	}
	if err != nil {
		w.Log.Warn("cannot write the report", "kills", len(kills), "error", err)
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
