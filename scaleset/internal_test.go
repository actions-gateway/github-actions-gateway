package scaleset

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestStatusError_Mapping(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   func(error) bool
	}{
		{200, func(e error) bool { return e == nil }},
		{204, func(e error) bool { return e == nil }},
		{401, func(e error) bool { var t *UnauthorizedError; return errors.As(e, &t) }},
		{403, func(e error) bool { var t *UnauthorizedError; return errors.As(e, &t) }},
		// statusError maps a 409 to the neutral ConflictError; each call translates it
		// into its endpoint-specific conflict type (SessionConflict / RunnerNameConflict).
		{409, func(e error) bool { var t *ConflictError; return errors.As(e, &t) }},
		{404, func(e error) bool { var t *NotFoundError; return errors.As(e, &t) }},
		{410, func(e error) bool { var t *NotFoundError; return errors.As(e, &t) }},
		{429, func(e error) bool { var t *RateLimitError; return errors.As(e, &t) }},
		{500, func(e error) bool { return e != nil }},
	} {
		got := statusError(tc.status, http.Header{}, []byte("body"))
		if !tc.want(got) {
			t.Errorf("statusError(%d) = %v, unexpected type", tc.status, got)
		}
	}
}

func TestParseRateLimitError_RetryAfter(t *testing.T) {
	if got := parseRateLimitError(http.Header{}); got.RetryAfter != -1 {
		t.Errorf("no header: RetryAfter = %v, want -1", got.RetryAfter)
	}
	h := http.Header{}
	h.Set("Retry-After", "5")
	if got := parseRateLimitError(h); got.RetryAfter != 5*time.Second {
		t.Errorf("Retry-After 5: RetryAfter = %v, want 5s", got.RetryAfter)
	}
	h.Set("Retry-After", "not-a-number")
	if got := parseRateLimitError(h); got.RetryAfter != -1 {
		t.Errorf("bad Retry-After: RetryAfter = %v, want -1", got.RetryAfter)
	}
}

func TestRegistrationTokenPath(t *testing.T) {
	for _, tc := range []struct {
		url, want string
		wantErr   bool
	}{
		{"https://github.com/my-org", "/orgs/my-org/actions/runners/registration-token", false},
		{"https://github.com/my-org/my-repo", "/repos/my-org/my-repo/actions/runners/registration-token", false},
		{"https://github.com/a/b/c", "", true},
		{"https://github.com/", "", true},
	} {
		got, err := registrationTokenPath(tc.url)
		if tc.wantErr {
			if err == nil {
				t.Errorf("registrationTokenPath(%q) expected error", tc.url)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("registrationTokenPath(%q) = %q, %v; want %q", tc.url, got, err, tc.want)
		}
	}
}

func TestAppendQuery(t *testing.T) {
	if got := appendQuery("https://x/y", "a=1"); got != "https://x/y?a=1" {
		t.Errorf("appendQuery no-query = %q", got)
	}
	if got := appendQuery("https://x/y?z=0", "a=1"); got != "https://x/y?z=0&a=1" {
		t.Errorf("appendQuery with-query = %q", got)
	}
}

func TestPollErrorReason(t *testing.T) {
	if got := pollErrorReason(context.DeadlineExceeded); got != "timeout" {
		t.Errorf("deadline reason = %q, want timeout", got)
	}
	if got := pollErrorReason(errors.New("connection refused")); got != "transport" {
		t.Errorf("generic reason = %q, want transport", got)
	}
}

// makeJWT builds an unsigned JWT with the given exp for the parser tests.
func makeJWT(t *testing.T, exp time.Time) string {
	t.Helper()
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	return enc(map[string]string{"alg": "none"}) + "." + enc(map[string]int64{"exp": exp.Unix()}) + ".sig"
}

func TestParseJWTExpiry(t *testing.T) {
	want := time.Now().Add(30 * time.Minute).Truncate(time.Second)
	got, err := parseJWTExpiry(makeJWT(t, want))
	if err != nil {
		t.Fatalf("parseJWTExpiry: %v", err)
	}
	if !got.Equal(want) {
		t.Errorf("exp = %v, want %v", got, want)
	}

	for _, bad := range []string{"", "a.b", "a.b.c.d", "not.valid.jwt"} {
		if _, err := parseJWTExpiry(bad); err == nil {
			t.Errorf("parseJWTExpiry(%q) expected error", bad)
		}
	}
	// A payload with no exp claim is an error.
	enc := func(v any) string { b, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(b) }
	noExp := enc(map[string]string{"alg": "none"}) + "." + enc(map[string]string{"sub": "x"}) + ".sig"
	if _, err := parseJWTExpiry(noExp); err == nil {
		t.Error("parseJWTExpiry with no exp expected error")
	}
}

func TestAdminConnection_ExpiresWithin(t *testing.T) {
	soon := &AdminConnection{Token: makeJWT(t, time.Now().Add(30*time.Second))}
	if !soon.ExpiresWithin(60 * time.Second) {
		t.Error("token 30s out should be within 60s")
	}
	if soon.ExpiresWithin(10 * time.Second) {
		t.Error("token 30s out should NOT be within 10s")
	}
	far := &AdminConnection{Token: makeJWT(t, time.Now().Add(time.Hour))}
	if far.ExpiresWithin(60 * time.Second) {
		t.Error("token 1h out should not be within 60s")
	}
	// An unparseable token is treated as already expired so the client re-mints.
	if !(&AdminConnection{Token: "garbage"}).ExpiresWithin(time.Second) {
		t.Error("unparseable token should be treated as expired")
	}
}

func TestNew_Validation(t *testing.T) {
	if _, err := New(Config{ConfigURL: "https://github.com/o"}); err == nil {
		t.Error("missing TokenProvider should error")
	}
	if _, err := New(Config{TokenProvider: staticProvider("t")}); err == nil {
		t.Error("missing ConfigURL should error")
	}
	c, err := New(Config{TokenProvider: staticProvider("t"), ConfigURL: "https://github.com/o"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.apiBase != defaultAPIBase || c.hc == nil || c.pollClient == nil || c.adminRefreshLead != defaultAdminRefreshLead {
		t.Errorf("defaults not applied: %+v", c)
	}
}

// staticProvider is a githubapp.TokenProvider returning a fixed token.
type staticProvider string

func (s staticProvider) Token(context.Context) (string, error) { return string(s), nil }
