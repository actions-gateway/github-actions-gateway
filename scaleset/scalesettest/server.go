// Package scalesettest serves the runner-scale-set protocol model from
// scaleset/scalesetstub over an httptest.Server, for in-process tests of the
// scaleset client and of the controllers built on it.
//
// The protocol itself — auto-assign under an advertised capacity, the GHES-style
// JobAvailable→acquire flow, cursor-based message replay, claim-once acquisition,
// the long poll — lives in scalesetstub and is shared with the deployed
// fake-GitHub image. This package is the transport, plus the fixed self-referential
// base an in-process test needs.
package scalesettest

import (
	"net/http"
	"net/http/httptest"

	"github.com/actions-gateway/github-actions-gateway/scaleset/scalesetstub"
)

// DefaultPollTimeout is how long the stub holds an empty poll open before answering
// 202 ("nothing to deliver"). See scalesetstub.DefaultPollTimeout.
const DefaultPollTimeout = scalesetstub.DefaultPollTimeout

// The identity the fake stamps on every enqueued job. Exported so a test can assert
// against the same values the fake will deliver rather than restating them.
const (
	DefaultJobOwner      = scalesetstub.DefaultJobOwner
	DefaultJobRepository = scalesetstub.DefaultJobRepository
)

// Server is an httptest-backed scale-set protocol stub. Construct it with New;
// call Close when done. The embedded *scalesetstub.Stub carries the whole control
// surface — EnqueueJob, EnableGHESAcquireFlow, DropSession, Calls, and the rest.
type Server struct {
	*scalesetstub.Stub

	// URL is the Actions Service tenant base AND the REST API base (self-referential):
	// pass it as both scaleset.Config.APIBase and the base the admin connection
	// returns.
	URL    string
	server *httptest.Server
}

// New creates and starts a stub in the default dotcom auto-assign mode.
func New() *Server {
	s := &Server{}
	s.Stub = scalesetstub.New(func(*http.Request) string { return s.URL })
	// Unstarted, so the bound address is readable — and the base the stub's
	// self-referential URLs close over is set — before the first request can arrive.
	s.server = httptest.NewUnstartedServer(s.Stub.Handler())
	s.URL = "http://" + s.server.Listener.Addr().String()
	s.server.Start()
	return s
}

// HTTPClient returns an *http.Client suitable for the stub (a real local TCP
// listener, so the default client's unbounded read timeout is harmless — the test
// bounds the call). Pass it as both scaleset.Config.HTTPClient and PollClient.
func (s *Server) HTTPClient() *http.Client {
	return s.server.Client()
}

// Close shuts down the stub server. It first releases any parked long-poll, because
// httptest.Server.Close waits for outstanding requests to finish and would otherwise
// block for a whole poll window. Safe to call more than once.
func (s *Server) Close() {
	s.Stub.Close()
	s.server.Close()
}
