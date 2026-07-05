package listener

// Q267: white-box coverage for the registration-propagation classifier. The
// match must be narrow — only GitHub's transient "Registration … was not found"
// token-exchange 400 warrants riding out the lag by retrying the SAME fresh
// credential; a broad credential rejection must NOT, because that is the Q114
// signal to recycle a genuinely-consumed record.

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/actions-gateway/github-actions-gateway/githubapp"
)

func TestIsRegistrationNotFound(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "400 registration not found (canonical propagation lag)",
			err:  &githubapp.TokenExchangeError{StatusCode: http.StatusBadRequest, Body: `{"error_description":"Registration 12345 was not found"}`},
			want: true,
		},
		{
			name: "404 registration not found",
			err:  &githubapp.TokenExchangeError{StatusCode: http.StatusNotFound, Body: "registration not found"},
			want: true,
		},
		{
			name: "wrapped propagation error still matches through fmt.Errorf",
			err:  fmt.Errorf("refresh broker token: %w", &githubapp.TokenExchangeError{StatusCode: http.StatusBadRequest, Body: "The Registration was not found."}),
			want: true,
		},
		{
			name: "400 but a different body (genuine bad client) does not match",
			err:  &githubapp.TokenExchangeError{StatusCode: http.StatusBadRequest, Body: `{"error":"invalid_client"}`},
			want: false,
		},
		{
			name: "401 unauthorized (consumed single-use record) does not match — recycle instead",
			err:  &githubapp.TokenExchangeError{StatusCode: http.StatusUnauthorized, Body: "Registration was not found"},
			want: false,
		},
		{
			name: "unrelated error does not match",
			err:  errors.New("dial tcp: connection refused"),
			want: false,
		},
		{
			name: "nil error does not match",
			err:  nil,
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRegistrationNotFound(tc.err); got != tc.want {
				t.Fatalf("isRegistrationNotFound(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
