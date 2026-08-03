//go:build e2e
// +build e2e

package e2e

import (
	"testing"

	"github.com/onsi/gomega"
)

// rerunCallsOnAcceptance turns a log line into the number the Q503 assertion rests on,
// and a parser that returns a plausible wrong value does it silently: reading 1 off a
// recovery that made 19 calls would fail the spec against a working build, and reading
// 30 off a terminal-failure line would pass it against a broken one. Both are text-to-
// number and need no cluster, so they are asserted here rather than discovered on a
// live-GitHub run that costs an hour and cannot be re-run concurrently.
//
// Rides the e2e binary like TestProgressEventFitsPipeBuf: ginkgo run executes the
// package's plain Test functions alongside the suite.
func TestRerunCallsOnAcceptance(t *testing.T) {
	// The subject asserts with Gomega but runs outside an It, so the fail handler has
	// to be registered explicitly.
	gomega.RegisterTestingT(t)

	cases := []struct {
		name string
		log  string
		want int
	}{
		{
			name: "json handler",
			log:  `{"msg":"disruption auto-retry triggered","runID":"1","attempt":1,"cause":"eviction","rerunCalls":19}`,
			want: 19,
		},
		{
			name: "text handler",
			log:  `level=INFO msg="disruption auto-retry triggered" runID=1 attempt=1 cause=eviction rerunCalls=19`,
			want: 19,
		},
		{
			name: "accepted on the first call, the value the spec rejects",
			log:  `{"msg":"disruption auto-retry triggered","rerunCalls":1}`,
			want: 1,
		},
		{
			name: "no acceptance line",
			log:  `{"msg":"worker pod disrupted; scheduling auto-retry","attempt":1}`,
			want: 0,
		},
		{
			// Both terminal-failure lines carry rerunCalls too. Counting one as an
			// acceptance would report a recovery that never landed as a healthy one.
			name: "terminal failure is not an acceptance",
			log:  `{"msg":"disruption auto-retry failed; run never concluded within the re-run window; manual rerun may be required","rerunCalls":30}`,
			want: 0,
		},
		{
			name: "a failed recovery earlier in the log does not shadow this one",
			log: `{"msg":"disruption auto-retry failed; manual rerun may be required","rerunCalls":30}` + "\n" +
				`{"msg":"disruption auto-retry triggered","rerunCalls":3}`,
			want: 3,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := rerunCallsOnAcceptance(tc.log); got != tc.want {
				t.Errorf("rerunCallsOnAcceptance() = %d, want %d", got, tc.want)
			}
		})
	}
}
