//go:build load

package load

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// memSample is a heap+stack snapshot taken after a forced GC.
type memSample struct {
	heapAlloc  uint64 // live heap objects
	stackInuse uint64 // goroutine stacks in use
	sys        uint64 // total OS-reserved (context only)
	goroutines int
}

// readMemSample forces GC and reads a stable heap+stack snapshot. It GCs twice
// with a brief pause so finalizers run and the stack scavenger settles before
// the read, so the delta between two samples reflects retained structures rather
// than transient allocation.
func readMemSample() memSample {
	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return memSample{
		heapAlloc:  ms.HeapAlloc,
		stackInuse: ms.StackInuse,
		sys:        ms.Sys,
		goroutines: runtime.NumGoroutine(),
	}
}

// perSession returns (after-before)/n, clamped at zero so GC jitter that makes a
// later sample momentarily smaller does not surface a negative figure.
func perSession(after, before uint64, n int) float64 {
	if after <= before || n <= 0 {
		return 0
	}
	return float64(after-before) / float64(n)
}

// jsonResponse builds a fixed-length in-memory *http.Response. ContentLength is
// set so the client's json.Decoder consumes the whole body and never blocks
// waiting for more.
func jsonResponse(req *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode:    status,
		Status:        http.StatusText(status),
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{"Content-Type": {"application/json"}},
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}
}

func bytesH(n uint64) string { return bytesHf(float64(n)) }

func bytesHf(f float64) string {
	const unit = 1024.0
	switch {
	case f >= unit*unit*unit:
		return fmt.Sprintf("%.2f GiB", f/(unit*unit*unit))
	case f >= unit*unit:
		return fmt.Sprintf("%.2f MiB", f/(unit*unit))
	case f >= unit:
		return fmt.Sprintf("%.2f KiB", f/unit)
	default:
		return fmt.Sprintf("%.0f B", f)
	}
}

func envInt(t *testing.T, key string, def int) int {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		t.Fatalf("invalid %s=%q: %v", key, v, err)
	}
	return n
}

func envDur(t *testing.T, key string, def time.Duration) time.Duration {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		t.Fatalf("invalid %s=%q: %v", key, v, err)
	}
	return d
}
