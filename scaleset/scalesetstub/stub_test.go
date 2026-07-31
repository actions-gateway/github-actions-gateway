package scalesetstub_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/actions-gateway/github-actions-gateway/scaleset/scalesetstub"
)

// The stub's self-referential URLs — the admin connection's tenant URL, a session's
// messageQueueUrl, a JobAvailable's acquireJobUrl — are the seam that lets one model
// serve both an in-process httptest wrapper (fixed base) and a deployed pod (whatever
// host the caller dialled). These tests drive the handler directly, so they assert the
// base the handler emits rather than the one a wrapper happened to configure.

// adminConnect runs the two REST bootstrap hops against h and returns the admin
// connection's tenant URL and token.
func adminConnect(t *testing.T, h http.Handler, host string) (url, token string) {
	t.Helper()
	tokenRec := httptest.NewRecorder()
	tokenReq := httptest.NewRequest(http.MethodPost, "/orgs/acme/actions/runners/registration-token", nil)
	tokenReq.Host = host
	h.ServeHTTP(tokenRec, tokenReq)
	if tokenRec.Code != http.StatusCreated {
		t.Fatalf("registration-token: status %d", tokenRec.Code)
	}

	regRec := httptest.NewRecorder()
	regReq := httptest.NewRequest(http.MethodPost, "/actions/runner-registration",
		strings.NewReader(`{"url":"https://github.com/acme","runner_event":"register"}`))
	regReq.Host = host
	regReq.Header.Set("Authorization", "RemoteAuth reg-token")
	h.ServeHTTP(regRec, regReq)
	if regRec.Code != http.StatusOK {
		t.Fatalf("runner-registration: status %d body %s", regRec.Code, regRec.Body)
	}
	var conn struct {
		URL   string `json:"url"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(regRec.Body.Bytes(), &conn); err != nil {
		t.Fatalf("decode admin connection: %v", err)
	}
	return conn.URL, conn.Token
}

func TestBaseURLDefaultsToRequestHost(t *testing.T) {
	s := scalesetstub.New(nil)
	defer s.Close()

	url, _ := adminConnect(t, s.Handler(), "fakegithub.e2e-infra.svc.cluster.local:8080")
	if want := "http://fakegithub.e2e-infra.svc.cluster.local:8080"; url != want {
		t.Errorf("admin connection URL = %q, want %q", url, want)
	}
}

// A deployed stub is reached under whatever host the caller dialled, so two callers
// arriving on different hosts must each be handed URLs on their own host — a fixed
// base would send one of them somewhere it cannot reach.
func TestSessionURLFollowsTheCallersHost(t *testing.T) {
	s := scalesetstub.New(nil)
	defer s.Close()
	h := s.Handler()
	id := s.AddScaleSet("set-a", 7)

	for _, host := range []string{"fakegithub.e2e-infra.svc.cluster.local:8080", "127.0.0.1:9999"} {
		_, token := adminConnect(t, h, host)

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost,
			"/_apis/runtime/runnerscalesets/1/sessions?api-version=6.0-preview",
			strings.NewReader(`{"ownerName":"owner"}`))
		req.Host = host
		req.Header.Set("Authorization", "Bearer "+token)
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("host %s: create-session status %d body %s", host, rec.Code, rec.Body)
		}
		var sess struct {
			MessageQueueURL string `json:"messageQueueUrl"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &sess); err != nil {
			t.Fatalf("decode session: %v", err)
		}
		if want := "http://" + host + "/queue/1/message"; sess.MessageQueueURL != want {
			t.Errorf("host %s: messageQueueUrl = %q, want %q", host, sess.MessageQueueURL, want)
		}
		s.DropSession(id)
	}
}

// A consumer that mounts the stub behind a prefix supplies its own base, and the
// scheme is part of what it supplies — a stub fronted by TLS must not hand back
// http:// URLs.
func TestBaseURLOverrideIsUsedVerbatim(t *testing.T) {
	s := scalesetstub.New(func(*http.Request) string { return "https://gh.example.test/svc" })
	defer s.Close()

	url, _ := adminConnect(t, s.Handler(), "ignored.example")
	if want := "https://gh.example.test/svc"; url != want {
		t.Errorf("admin connection URL = %q, want %q", url, want)
	}
}
