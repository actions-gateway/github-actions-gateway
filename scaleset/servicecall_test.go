package scaleset

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// newConnectedClient returns a Client whose admin connection is already minted
// against srv, so a service call runs without the two bootstrap hops.
func newConnectedClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	c, err := New(Config{
		TokenProvider: staticProvider("install-token"),
		ConfigURL:     "https://github.com/test-org",
		APIBase:       srv.URL,
		HTTPClient:    srv.Client(),
		PollClient:    srv.Client(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.conn = &AdminConnection{URL: srv.URL, Token: makeJWT(t, time.Now().Add(time.Hour))}
	return c
}

// wireCapture records the requests a test server received.
type wireCapture struct {
	mu   sync.Mutex
	reqs []capturedRequest
}

type capturedRequest struct {
	method string
	url    string
	header http.Header
	body   string
}

func (w *wireCapture) add(r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	w.mu.Lock()
	defer w.mu.Unlock()
	w.reqs = append(w.reqs, capturedRequest{r.Method, r.URL.String(), r.Header.Clone(), string(body)})
}

func (w *wireCapture) taken() []capturedRequest {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]capturedRequest(nil), w.reqs...)
}

// TestServiceCallBuildersAgreeOnTheWire pins svcCall and RawServiceCall to the same
// request: URL join, api-version placement, auth, and JSON framing. Both build an
// admin-JWT service request and are expected to differ only in what they return.
func TestServiceCallBuildersAgreeOnTheWire(t *testing.T) {
	wire := &wireCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wire.add(r)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := newConnectedClient(t, srv)

	cases := []struct {
		name   string
		method string
		path   string
		in     any
	}{
		{"no query, no body", http.MethodGet, "/_apis/runtime/x", nil},
		{"path already carries a query", http.MethodGet, "/_apis/runtime/x?groupName=a+b", nil},
		{"query and a body", http.MethodPost, "/_apis/runtime/x?groupName=a+b", map[string]string{"k": "v"}},
		{"empty body object", http.MethodPost, "/_apis/runtime/x", struct{}{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wire.mu.Lock()
			wire.reqs = nil
			wire.mu.Unlock()

			ctx := context.Background()
			if err := c.svcCall(ctx, tc.method, tc.path, tc.in, nil); err != nil {
				t.Fatalf("svcCall: %v", err)
			}
			var payload []byte
			if tc.in != nil {
				var err error
				if payload, err = json.Marshal(tc.in); err != nil {
					t.Fatalf("marshal: %v", err)
				}
			}
			if _, _, err := c.RawServiceCall(ctx, tc.method, tc.path, payload); err != nil {
				t.Fatalf("RawServiceCall: %v", err)
			}

			reqs := wire.taken()
			if len(reqs) != 2 {
				t.Fatalf("server saw %d requests, want 2", len(reqs))
			}
			typed, raw := reqs[0], reqs[1]
			if !strings.Contains(typed.url, "api-version="+apiVersion) {
				t.Errorf("svcCall URL %q lost the api-version parameter", typed.url)
			}
			if typed.method != raw.method || typed.url != raw.url || typed.body != raw.body {
				t.Errorf("wire differs:\n svcCall = %s %s body=%q\n raw     = %s %s body=%q",
					typed.method, typed.url, typed.body, raw.method, raw.url, raw.body)
			}
			for _, h := range []string{"Authorization", "Accept", "Content-Type"} {
				if typed.header.Get(h) != raw.header.Get(h) {
					t.Errorf("%s differs: svcCall %q, raw %q", h, typed.header.Get(h), raw.header.Get(h))
				}
			}
			if got := typed.header.Get("Authorization"); !strings.HasPrefix(got, "Bearer ") {
				t.Errorf("Authorization = %q, want a Bearer admin JWT", got)
			}
			wantCT := ""
			if tc.in != nil {
				wantCT = "application/json"
			}
			if got := typed.header.Get("Content-Type"); got != wantCT {
				t.Errorf("Content-Type = %q, want %q", got, wantCT)
			}
		})
	}
}

// TestSvcCall_ErrorPaths pins which svcCall failures carry the "scaleset: METHOD PATH"
// prefix and which surface bare, so an extraction cannot re-wrap one of them.
func TestSvcCall_ErrorPaths(t *testing.T) {
	t.Run("bootstrap failure surfaces unwrapped", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		}))
		defer srv.Close()
		c, err := New(Config{
			TokenProvider: staticProvider("install-token"),
			ConfigURL:     "https://github.com/test-org",
			APIBase:       srv.URL,
			HTTPClient:    srv.Client(),
			PollClient:    srv.Client(),
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		err = c.svcCall(context.Background(), http.MethodGet, "/_apis/runtime/x", nil, nil)
		if err == nil {
			t.Fatal("want the bootstrap failure to surface")
		}
		if !strings.Contains(err.Error(), "registration token") {
			t.Errorf("err = %v, want the bootstrap hop's own error", err)
		}
		if strings.Contains(err.Error(), "/_apis/runtime/x") {
			t.Errorf("err = %v, want no per-call prefix on a bootstrap failure", err)
		}
	})

	t.Run("unmarshalable request body surfaces bare", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		defer srv.Close()
		c := newConnectedClient(t, srv)
		err := c.svcCall(context.Background(), http.MethodPost, "/_apis/runtime/x", make(chan int), nil)
		if err == nil {
			t.Fatal("want a marshal error")
		}
		if strings.HasPrefix(err.Error(), "scaleset:") {
			t.Errorf("err = %v, want the encoding/json error unwrapped", err)
		}
	})

	t.Run("unbuildable request surfaces bare", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		defer srv.Close()
		c := newConnectedClient(t, srv)
		err := c.svcCall(context.Background(), "BAD METHOD", "/_apis/runtime/x", nil, nil)
		if err == nil {
			t.Fatal("want a request-construction error")
		}
		if strings.HasPrefix(err.Error(), "scaleset:") {
			t.Errorf("err = %v, want the net/http error unwrapped", err)
		}
	})

	t.Run("transport failure is prefixed", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		c := newConnectedClient(t, srv)
		srv.Close() // the connection now fails at dial

		err := c.svcCall(context.Background(), http.MethodGet, "/_apis/runtime/x", nil, nil)
		if err == nil {
			t.Fatal("want a transport error")
		}
		if !strings.HasPrefix(err.Error(), "scaleset: GET /_apis/runtime/x: ") {
			t.Errorf("err = %v, want the scaleset: METHOD PATH prefix", err)
		}
	})

	t.Run("status maps to a typed error under the prefix", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusConflict)
		}))
		defer srv.Close()
		c := newConnectedClient(t, srv)

		err := c.svcCall(context.Background(), http.MethodPost, "/_apis/runtime/x", nil, nil)
		var ce *ConflictError
		if !errors.As(err, &ce) {
			t.Fatalf("err = %v, want *ConflictError", err)
		}
		if !strings.HasPrefix(err.Error(), "scaleset: POST /_apis/runtime/x: ") {
			t.Errorf("err = %v, want the scaleset: METHOD PATH prefix", err)
		}
	})

	t.Run("undecodable response is prefixed and named", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("not json"))
		}))
		defer srv.Close()
		c := newConnectedClient(t, srv)

		var out RunnerScaleSet
		err := c.svcCall(context.Background(), http.MethodGet, "/_apis/runtime/x", nil, &out)
		if err == nil {
			t.Fatal("want a decode error")
		}
		if !strings.HasPrefix(err.Error(), "scaleset: GET /_apis/runtime/x: decode response: ") {
			t.Errorf("err = %v, want the decode-response prefix", err)
		}
	})

	t.Run("empty body leaves out untouched", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}))
		defer srv.Close()
		c := newConnectedClient(t, srv)

		out := RunnerScaleSet{Name: "kept"}
		if err := c.svcCall(context.Background(), http.MethodDelete, "/_apis/runtime/x", nil, &out); err != nil {
			t.Fatalf("svcCall: %v", err)
		}
		if out.Name != "kept" {
			t.Errorf("out.Name = %q, want the caller's value untouched", out.Name)
		}
	})
}

// TestRawServiceCall_ErrorPaths pins the escape hatch's error shape: a non-2xx is a
// status, and a real failure reports (0, nil, err) with svcCall's prefix rules.
func TestRawServiceCall_ErrorPaths(t *testing.T) {
	t.Run("non-2xx is a status, not an error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"message":"taken"}`))
		}))
		defer srv.Close()
		c := newConnectedClient(t, srv)

		status, body, err := c.RawServiceCall(context.Background(), http.MethodPost, "/_apis/runtime/x", nil)
		if err != nil {
			t.Fatalf("RawServiceCall: %v", err)
		}
		if status != http.StatusConflict {
			t.Errorf("status = %d, want 409", status)
		}
		if !strings.Contains(string(body), "taken") {
			t.Errorf("body = %q, want the server's body verbatim", body)
		}
	})

	t.Run("unbuildable request surfaces bare", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		defer srv.Close()
		c := newConnectedClient(t, srv)

		status, body, err := c.RawServiceCall(context.Background(), "BAD METHOD", "/_apis/runtime/x", nil)
		if err == nil {
			t.Fatal("want a request-construction error")
		}
		if strings.HasPrefix(err.Error(), "scaleset:") {
			t.Errorf("err = %v, want the net/http error unwrapped", err)
		}
		if status != 0 || body != nil {
			t.Errorf("got (%d, %q), want (0, nil) on a failed call", status, body)
		}
	})

	t.Run("transport failure is prefixed", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		c := newConnectedClient(t, srv)
		srv.Close()

		status, body, err := c.RawServiceCall(context.Background(), http.MethodGet, "/_apis/runtime/x", nil)
		if err == nil {
			t.Fatal("want a transport error")
		}
		if !strings.HasPrefix(err.Error(), "scaleset: GET /_apis/runtime/x: ") {
			t.Errorf("err = %v, want the scaleset: METHOD PATH prefix", err)
		}
		if status != 0 || body != nil {
			t.Errorf("got (%d, %q), want (0, nil) on a failed call", status, body)
		}
	})

	t.Run("an empty body still sets Content-Type", func(t *testing.T) {
		wire := &wireCapture{}
		srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			wire.add(r)
		}))
		defer srv.Close()
		c := newConnectedClient(t, srv)

		if _, _, err := c.RawServiceCall(context.Background(), http.MethodPost, "/_apis/runtime/x", []byte{}); err != nil {
			t.Fatalf("RawServiceCall: %v", err)
		}
		reqs := wire.taken()
		if len(reqs) != 1 {
			t.Fatalf("server saw %d requests, want 1", len(reqs))
		}
		if got := reqs[0].header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json for a non-nil body", got)
		}
	})
}
