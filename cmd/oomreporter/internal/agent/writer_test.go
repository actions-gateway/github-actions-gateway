package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/actions-gateway/github-actions-gateway/oomreporter/internal/cgroup"
	"github.com/actions-gateway/github-actions-gateway/oomreporter/internal/report"
)

func kills(n int) (k []report.Kill) {
	for i := range n {
		k = append(k, report.Kill{Pod: uid, At: int64(i), Processes: 1})
	}
	return k
}

// put is a write: when, and of how many kills.
type put struct {
	at time.Duration
	n  int
}

// puts runs a Writer whose first failing writes fail, has steps done, and returns its writes.
func puts(t *testing.T, failing int, d time.Duration, steps func(w *Writer)) (got []put) {
	t.Helper()
	began := time.Now()
	w := NewWriter(func(_ context.Context, data string) error {
		k, _, err := report.Decode(data)
		require.NoError(t, err)
		got = append(got, put{time.Since(began), len(k)})
		if len(got) <= failing {
			return errors.New("refused")
		}
		return nil
	}, quietLog)
	ctx, cancel := context.WithTimeout(t.Context(), d)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); w.Run(ctx) }()
	steps(w)
	<-done
	return got
}

func TestWriter_WritesAtOnceAndThenMoreAndMoreSlowly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		got := puts(t, 0, 40*time.Second, func(w *Writer) {
			for i := range 2000 {
				time.Sleep(10 * time.Millisecond)
				w.Write(kills(i%report.MaxKills + 1))
			}
		})
		var at []time.Duration
		for _, p := range got {
			at = append(at, p.at.Truncate(100*time.Millisecond))
		}
		assert.Equal(t, []time.Duration{0, 2 * time.Second, 6 * time.Second, 14 * time.Second, 22 * time.Second}, at)
	})
}

func TestWriter_WritesTheNewest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		got := puts(t, 0, time.Minute, func(w *Writer) {
			w.Write(kills(1))
			time.Sleep(time.Millisecond)
			w.Write(kills(2))
			w.Write(kills(3))
		})
		assert.Equal(t, []put{{0, 1}, {2 * time.Second, 3}}, got)
	})
}

func TestWriter_WritesAtOnceAfterAWhile(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		got := puts(t, 0, 2*time.Minute, func(w *Writer) {
			w.Write(kills(1))
			time.Sleep(quiet)
			w.Write(kills(2))
			time.Sleep(time.Second)
			w.Write(kills(3))
		})
		assert.Equal(t, []put{{0, 1}, {quiet, 2}, {quiet + 2*time.Second, 3}}, got)
	})
}

func TestWriter_WritesAgainWhileItFails(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		got := puts(t, 3, time.Minute, func(w *Writer) {
			w.Write(kills(1))
			time.Sleep(3 * time.Second)
			w.Write(kills(2))
		})
		assert.Equal(t, []put{{0, 1}, {2 * time.Second, 1}, {6 * time.Second, 2}, {14 * time.Second, 2}}, got)
	})
}

func TestWriter_StopsBetweenWrites(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		got := puts(t, 100, time.Second, func(w *Writer) { w.Write(kills(1)) })
		assert.Equal(t, []put{{0, 1}}, got)
	})
}

func TestWriter_WritesTheNewestKillsAndLinesTogether(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var reports []string
		w := NewWriter(func(_ context.Context, data string) error { reports = append(reports, data); return nil }, quietLog)
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		done := make(chan struct{})
		go func() { defer close(done); w.Run(ctx) }()
		line := report.Line{Pod: uid, Container: strings.Repeat("a", 64), Level: cgroup.PodLevel, At: 1}
		w.Write(kills(2))
		time.Sleep(time.Millisecond)
		w.WriteLines([]report.Line{line})
		w.Write(kills(3))
		<-done
		require.Len(t, reports, 2)
		k, l, err := report.Decode(reports[1])
		require.NoError(t, err)
		assert.Len(t, k, 3)
		assert.Equal(t, []report.Line{line}, l)
	})
}
