package runnerimage

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// State is where a Lookup stands.
type State int

const (
	// Pending means an inspection is in flight and nothing has been read yet.
	Pending State = iota
	// Done means Result holds a completed reading.
	Done
	// Failed means the last inspection errored; Err says why and the resolver
	// retries on a backoff.
	Failed
)

// Lookup is the resolver's answer for one request. A Done lookup that is being
// re-resolved in the background still reports Done with the last result, so a
// verdict never regresses while a tag is re-checked.
type Lookup struct {
	State  State
	Result Reading
	// Err is the failure a Failed lookup carries, phrased for a condition message.
	Err string
	// Attempts counts failed inspections since the last success.
	Attempts int
}

// Request identifies an image to inspect and the credentials to inspect it with.
type Request struct {
	// Image is the effective worker image reference.
	Image string
	// PullSecrets names the kubernetes.io/dockerconfigjson Secrets the pod template
	// lists; each is read through Resolver.ReadSecret at inspection time.
	PullSecrets []string
	// Wake is called once the lookup's state changes, so the caller can reconcile
	// again. It must not block.
	Wake func()
}

func (r Request) key() string {
	names := append([]string(nil), r.PullSecrets...)
	sort.Strings(names)
	return r.Image + "|" + strings.Join(names, ",")
}

// Resolver answers image-version lookups asynchronously: a first Lookup for a key
// starts an inspection and returns Pending, and later ones return whatever the
// inspection established. It bounds concurrency, backs off on failure, and re-checks
// a tag-addressed reference on a TTL, since a tag can move while a digest cannot.
//
// It implements manager.Runnable so the manager owns the inspections' lifetime.
type Resolver struct {
	// HTTP performs the registry requests. It must carry no overall Timeout: an
	// inspection streams ~100 MB and is bounded by InspectTimeout instead.
	HTTP *http.Client
	// ReadSecret returns a named imagePullSecret's .dockerconfigjson payload.
	ReadSecret func(ctx context.Context, name string) ([]byte, error)
	Log        *slog.Logger
	// Now is the clock; nil means time.Now (tests).
	Now func() time.Time

	// MaxInFlight bounds concurrent inspections; zero means 1.
	MaxInFlight int
	// InspectTimeout bounds one inspection; zero means 15 minutes.
	InspectTimeout time.Duration
	// TagTTL is how long a reading addressed by tag alone stands before it is
	// re-resolved; zero means 1 hour. A digest-pinned reading never expires.
	TagTTL time.Duration
	// RetryBase and RetryMax bound the failure backoff; zero means 1 minute and 1 hour.
	RetryBase, RetryMax time.Duration

	mu      sync.Mutex
	entries map[string]*entry
	sem     chan struct{}
	ctx     context.Context
}

type entry struct {
	req        Request
	state      State
	result     Reading
	err        string
	attempts   int
	inFlight   bool
	resolvedAt time.Time
	nextTry    time.Time
	wakes      []func()
}

// Start blocks until ctx is done. Inspections started before it runs use the
// background context; those started after inherit ctx.
func (r *Resolver) Start(ctx context.Context) error {
	r.mu.Lock()
	r.ctx = ctx
	r.mu.Unlock()
	<-ctx.Done()
	return nil
}

// NeedLeaderElection keeps the resolver running on every replica: a lookup is a
// read the reconciler on this replica needs, whether or not it holds the lease.
func (r *Resolver) NeedLeaderElection() bool { return false }

// Lookup returns the current state for req, starting or retrying an inspection when
// one is due. It never blocks on the registry.
func (r *Resolver) Lookup(req Request) Lookup {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entries == nil {
		r.entries = make(map[string]*entry)
	}
	key := req.key()
	e := r.entries[key]
	if e == nil {
		e = &entry{req: req}
		r.entries[key] = e
	}
	if req.Wake != nil {
		e.wakes = append(e.wakes, req.Wake)
	}
	if !e.inFlight && r.due(e, now) {
		e.inFlight = true
		go r.inspect(key, e.req)
	}
	return Lookup{State: e.state, Result: e.result, Err: e.err, Attempts: e.attempts}
}

// due reports whether an inspection should start for e: never yet, a failure whose
// backoff has elapsed, or a tag-only reading past its TTL.
func (r *Resolver) due(e *entry, now time.Time) bool {
	switch e.state {
	case Failed:
		return !now.Before(e.nextTry)
	case Done:
		ref, err := ParseReference(e.req.Image)
		if err != nil || ref.Digest != "" {
			return false
		}
		return now.Sub(e.resolvedAt) >= r.tagTTL()
	}
	return !e.inFlight
}

func (r *Resolver) inspect(key string, req Request) {
	base := r.baseCtx()
	ctx, cancel := context.WithTimeout(base, r.inspectTimeout())
	defer cancel()

	r.acquire()
	defer r.release()

	reading, err := r.run(ctx, req)

	r.mu.Lock()
	e := r.entries[key]
	e.inFlight = false
	if err != nil {
		e.state = Failed
		e.err = err.Error()
		e.attempts++
		e.nextTry = r.now().Add(r.backoff(e.attempts))
		r.log().Warn("worker image runner version: registry read failed",
			"image", req.Image, "attempt", e.attempts, "retryAfter", r.backoff(e.attempts), "error", err)
	} else {
		e.state = Done
		e.result = reading
		e.err = ""
		e.attempts = 0
		e.resolvedAt = r.now()
		r.log().Info("worker image runner version: read from the registry",
			"image", req.Image, "digest", reading.Digest, "platform", reading.Platform,
			"version", reading.Version, "found", reading.Found)
	}
	wakes := e.wakes
	e.wakes = nil
	r.mu.Unlock()

	for _, w := range wakes {
		w()
	}
}

func (r *Resolver) run(ctx context.Context, req Request) (Reading, error) {
	if r.HTTP == nil {
		return Reading{}, fmt.Errorf("no HTTP client configured for the registry read")
	}
	creds := Credentials{}
	for _, name := range req.PullSecrets {
		if r.ReadSecret == nil {
			return Reading{}, fmt.Errorf("imagePullSecret %q: no secret reader configured", name)
		}
		data, err := r.ReadSecret(ctx, name)
		if err != nil {
			return Reading{}, fmt.Errorf("imagePullSecret %q: %w", name, err)
		}
		if err := creds.ParseDockerConfigJSON(data); err != nil {
			return Reading{}, fmt.Errorf("imagePullSecret %q: %w", name, err)
		}
	}
	return Inspect(ctx, r.HTTP, req.Image, creds)
}

func (r *Resolver) acquire() {
	r.mu.Lock()
	if r.sem == nil {
		n := r.MaxInFlight
		if n <= 0 {
			n = 1
		}
		r.sem = make(chan struct{}, n)
	}
	sem := r.sem
	r.mu.Unlock()
	sem <- struct{}{}
}

func (r *Resolver) release() {
	r.mu.Lock()
	sem := r.sem
	r.mu.Unlock()
	<-sem
}

func (r *Resolver) backoff(attempts int) time.Duration {
	base, maxDelay := r.RetryBase, r.RetryMax
	if base <= 0 {
		base = time.Minute
	}
	if maxDelay <= 0 {
		maxDelay = time.Hour
	}
	d := base
	for i := 1; i < attempts && d < maxDelay; i++ {
		d *= 2
	}
	return min(d, maxDelay)
}

func (r *Resolver) baseCtx() context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ctx != nil {
		return r.ctx
	}
	return context.Background()
}

func (r *Resolver) inspectTimeout() time.Duration {
	if r.InspectTimeout > 0 {
		return r.InspectTimeout
	}
	return 15 * time.Minute
}

func (r *Resolver) tagTTL() time.Duration {
	if r.TagTTL > 0 {
		return r.TagTTL
	}
	return time.Hour
}

func (r *Resolver) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Resolver) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}
