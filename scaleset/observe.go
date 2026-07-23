package scaleset

import (
	"net/http"
	"time"
)

// ResponseObserver receives one ResponseInfo for every HTTP response the Client
// receives, before that response is mapped to a typed value or a typed error. It
// exists so diagnostic tooling can see the raw wire the typed API deliberately
// hides: GetMessage collapses a 202 to (nil, nil), svcCall turns a 404 into
// *NotFoundError, and neither surfaces the response headers or the latency a
// protocol investigation is looking for.
//
// cmd/probe is the motivating caller. Its job is to report what the live wire
// actually did, and it must be able to do that while driving this package rather
// than a parallel reimplementation of it — an observer is what makes "use the
// shipping client" and "report the raw wire" compatible (Q362).
//
// Implementations are called synchronously on the request path, so they must not
// block, and must not retain the Header map beyond the call.
type ResponseObserver interface {
	ObserveResponse(ResponseInfo)
}

// ResponseInfo describes one HTTP response the Client received.
//
// The request URL is split into Host and Path deliberately: the message-queue URL
// carries a signed query string, so no observer is ever handed a query. Response
// headers carry no credential and are passed through whole — they are the
// rate-limit evidence (Q264 plan §2a-5).
type ResponseInfo struct {
	// Op names the client operation that issued the request. One of
	// "RegistrationToken", "RunnerRegistration", "ServiceCall", "GetMessage",
	// "AcquireJobs", "DeleteMessage", "ListRunners", or "DeleteRunner".
	Op string
	// Method is the HTTP method.
	Method string
	// Host is the request URL's host — the evidence of which Actions Service
	// tenant (or queue backend) answered.
	Host string
	// Path is the request URL's path, without its query string.
	Path string
	// Status is the HTTP status code.
	Status int
	// Header is the response header set.
	Header http.Header
	// Elapsed is the wall time from issuing the request to reading its body —
	// the long-poll hold, for GetMessage.
	Elapsed time.Duration
	// BodyLen is the length in bytes of the response body the client read.
	BodyLen int
}

// observe reports one response to the configured observer, if any. start is when
// the request was issued; bodyLen the length of the body already read from it.
func (c *Client) observe(op string, req *http.Request, resp *http.Response, start time.Time, bodyLen int) {
	if c.observer == nil || req == nil || req.URL == nil || resp == nil {
		return
	}
	c.observer.ObserveResponse(ResponseInfo{
		Op:      op,
		Method:  req.Method,
		Host:    req.URL.Host,
		Path:    req.URL.Path,
		Status:  resp.StatusCode,
		Header:  resp.Header,
		Elapsed: time.Since(start),
		BodyLen: bodyLen,
	})
}
