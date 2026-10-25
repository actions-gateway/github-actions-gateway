package proxyattribution

import (
	"slices"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

const (
	// failureRetention is how long an attributed record is kept, long enough for
	// a session to look its pod up after the job ends.
	failureRetention = 10 * time.Minute
	// maxFailuresPerPod caps one pod's records, as the proxy caps one source.
	maxFailuresPerPod = 64
)

// Failure is one CONNECT a worker pod's job could not tunnel.
type Failure struct {
	// Time is the proxy's clock when the CONNECT failed.
	Time time.Time
	// Outcome is "denied" (the allowlist refused the destination) or
	// "dial-failed" (the upstream dial failed).
	Outcome string
	// DialClass is a dial failure's class: timeout, refused, dns or other.
	DialClass string
	// Host and Port are the destination the job asked for, as the proxy capped it.
	Host string
	Port string
}

// failureStore holds attributed records by pod UID. A repeat delivery of a job
// reuses its pod's name, never its UID.
type failureStore struct {
	mu    sync.Mutex
	byPod map[types.UID][]Failure
}

func newFailureStore() *failureStore {
	return &failureStore{byPod: map[types.UID][]Failure{}}
}

func (s *failureStore) add(pod types.UID, f Failure) {
	s.mu.Lock()
	defer s.mu.Unlock()
	recs := append(s.byPod[pod], f)
	if len(recs) > maxFailuresPerPod {
		recs = recs[len(recs)-maxFailuresPerPod:]
	}
	s.byPod[pod] = recs
}

func (s *failureStore) lookup(pod types.UID) []Failure {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.byPod[pod])
}

func (s *failureStore) sweep(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for pod, recs := range s.byPod {
		recs = slices.DeleteFunc(recs, func(f Failure) bool { return now.Sub(f.Time) >= failureRetention })
		if len(recs) == 0 {
			delete(s.byPod, pod)
			continue
		}
		s.byPod[pod] = recs
	}
}

func (s *failureStore) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.byPod)
}
