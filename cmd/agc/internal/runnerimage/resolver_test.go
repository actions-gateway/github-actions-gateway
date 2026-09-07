package runnerimage

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/runnerimage/runnerimagetest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// waitFor polls until the lookup for req leaves Pending.
func waitFor(t *testing.T, r *Resolver, req Request) Lookup {
	t.Helper()
	var l Lookup
	require.Eventually(t, func() bool {
		l = r.Lookup(Request{Image: req.Image, PullSecrets: req.PullSecrets})
		return l.State != Pending
	}, 5*time.Second, 10*time.Millisecond)
	return l
}

func TestResolverPendingThenDoneWakes(t *testing.T) {
	f := runnerimagetest.New(t)
	f.Tag("acme/runner", "v", f.Manifest("acme/runner", "linux", "amd64", f.GzipLayer(runnerimagetest.E(runnerimagetest.DepsPath, runnerimagetest.DepsJSON("2.335.1")))).Digest)
	r := &Resolver{HTTP: f.Client()}
	var woken atomic.Int32
	req := Request{Image: f.Image("acme/runner", ":v"), Wake: func() { woken.Add(1) }}

	first := r.Lookup(req)
	assert.Equal(t, Pending, first.State)

	got := waitFor(t, r, req)
	assert.Equal(t, Done, got.State)
	assert.Equal(t, "2.335.1", got.Result.Version)
	assert.Equal(t, int32(1), woken.Load(), "the caller is woken exactly once per state change")
}

func TestResolverDedupsInFlight(t *testing.T) {
	f := runnerimagetest.New(t)
	f.Tag("acme/runner", "v", f.Manifest("acme/runner", "linux", "amd64", f.GzipLayer(runnerimagetest.E(runnerimagetest.DepsPath, runnerimagetest.DepsJSON("2.335.1")))).Digest)
	r := &Resolver{HTTP: f.Client()}
	req := Request{Image: f.Image("acme/runner", ":v")}
	for range 5 {
		r.Lookup(req)
	}
	waitFor(t, r, req)
	assert.Equal(t, int64(1), f.BlobGets(), "five lookups share one inspection")
}

func TestResolverFailureBacksOff(t *testing.T) {
	f := runnerimagetest.New(t)
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	r := &Resolver{HTTP: f.Client(), Now: func() time.Time { return now }, RetryBase: time.Minute, RetryMax: 4 * time.Minute}
	req := Request{Image: f.Image("acme/runner", ":absent")}

	got := waitFor(t, r, req)
	assert.Equal(t, Failed, got.State)
	assert.Equal(t, 1, got.Attempts)
	assert.Contains(t, got.Err, "HTTP 404")

	// Inside the backoff nothing is retried; past it, one retry runs.
	r.Lookup(req)
	assert.Equal(t, 1, r.Lookup(req).Attempts)
	now = now.Add(61 * time.Second)
	r.Lookup(req)
	require.Eventually(t, func() bool { return r.Lookup(req).Attempts == 2 }, 5*time.Second, 10*time.Millisecond)

	// The delay doubles and caps.
	assert.Equal(t, time.Minute, r.backoff(1))
	assert.Equal(t, 2*time.Minute, r.backoff(2))
	assert.Equal(t, 4*time.Minute, r.backoff(3))
	assert.Equal(t, 4*time.Minute, r.backoff(9))

	// Publishing the tag heals it on the next due attempt, and the failure count resets.
	f.Tag("acme/runner", "absent", f.Manifest("acme/runner", "linux", "amd64", f.GzipLayer(runnerimagetest.E(runnerimagetest.DepsPath, runnerimagetest.DepsJSON("2.335.1")))).Digest)
	now = now.Add(time.Hour)
	r.Lookup(req)
	require.Eventually(t, func() bool { return r.Lookup(req).State == Done }, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, 0, r.Lookup(req).Attempts)
}

func TestResolverTagReResolvesOnTTLAndDigestNever(t *testing.T) {
	f := runnerimagetest.New(t)
	old := f.Manifest("acme/runner", "linux", "amd64", f.GzipLayer(runnerimagetest.E(runnerimagetest.DepsPath, runnerimagetest.DepsJSON("2.329.0"))))
	f.Tag("acme/runner", "v", old.Digest)
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	r := &Resolver{HTTP: f.Client(), Now: func() time.Time { return now }, TagTTL: time.Hour}

	byTag := Request{Image: f.Image("acme/runner", ":v")}
	byDigest := Request{Image: f.Image("acme/runner", "@"+old.Digest)}
	assert.Equal(t, "2.329.0", waitFor(t, r, byTag).Result.Version)
	assert.Equal(t, "2.329.0", waitFor(t, r, byDigest).Result.Version)

	// The tag moves. Before the TTL nothing is re-read.
	f.Tag("acme/runner", "v", f.Manifest("acme/runner", "linux", "amd64", f.GzipLayer(runnerimagetest.E(runnerimagetest.DepsPath, runnerimagetest.DepsJSON("2.335.1")))).Digest)
	gets := f.BlobGets()
	now = now.Add(30 * time.Minute)
	assert.Equal(t, "2.329.0", r.Lookup(byTag).Result.Version)
	assert.Equal(t, gets, f.BlobGets())

	// Past it, the tag is re-resolved while the old reading stands, and the digest is not.
	now = now.Add(31 * time.Minute)
	assert.Equal(t, Done, r.Lookup(byTag).State, "stale-while-revalidate: the verdict never regresses to pending")
	require.Eventually(t, func() bool { return r.Lookup(byTag).Result.Version == "2.335.1" }, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, gets+1, f.BlobGets())
	assert.Equal(t, "2.329.0", r.Lookup(byDigest).Result.Version)
	assert.Equal(t, gets+1, f.BlobGets(), "a digest is immutable, so it is never re-read")
}

func TestResolverReadsPullSecrets(t *testing.T) {
	f := runnerimagetest.New(t)
	f.Auth = runnerimagetest.AuthBasic
	f.Login = runnerimagetest.Credential{Username: "bot", Password: "s3cret"}
	f.Tag("acme/runner", "v", f.Manifest("acme/runner", "linux", "amd64", f.GzipLayer(runnerimagetest.E(runnerimagetest.DepsPath, runnerimagetest.DepsJSON("2.335.1")))).Digest)
	secrets := map[string][]byte{"regcred": runnerimagetest.DockerConfigJSON(f.Host(), f.Login)}
	r := &Resolver{HTTP: f.Client(), ReadSecret: func(_ context.Context, name string) ([]byte, error) {
		b, ok := secrets[name]
		if !ok {
			return nil, errors.New("not found")
		}
		return b, nil
	}}

	got := waitFor(t, r, Request{Image: f.Image("acme/runner", ":v"), PullSecrets: []string{"regcred"}})
	assert.Equal(t, Done, got.State)
	assert.Equal(t, "2.335.1", got.Result.Version)

	got = waitFor(t, r, Request{Image: f.Image("acme/runner", ":v"), PullSecrets: []string{"missing"}})
	assert.Equal(t, Failed, got.State)
	assert.Contains(t, got.Err, `imagePullSecret "missing": not found`)

	got = waitFor(t, r, Request{Image: f.Image("acme/runner", ":v")})
	assert.Equal(t, Failed, got.State, "the secret set is part of the key, so no credentials is its own lookup")
}

func TestResolverStopsWithManagerContext(t *testing.T) {
	f := runnerimagetest.New(t)
	f.Tag("acme/runner", "v", f.Manifest("acme/runner", "linux", "amd64", f.GzipLayer(runnerimagetest.E(runnerimagetest.DepsPath, runnerimagetest.DepsJSON("2.335.1")))).Digest)
	r := &Resolver{HTTP: f.Client()}
	assert.False(t, r.NeedLeaderElection())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = r.Start(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Start did not return on context cancel")
	}
	got := waitFor(t, r, Request{Image: f.Image("acme/runner", ":v")})
	assert.Equal(t, Failed, got.State, "an inspection started after the manager stopped inherits the cancelled context")
}
