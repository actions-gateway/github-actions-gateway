package provisioner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRESTErrors_UpstreamBodyIsSanitized: the run-status, re-run and force-cancel
// errors reach logs and, for the first two, a tenant-namespace Event, so a non-2xx body
// goes through githubapp.SanitizeBody like the scale-set client's (Q1327).
func TestRESTErrors_UpstreamBodyIsSanitized(t *testing.T) {
	token := "ghs_" + strings.Repeat("A1b2C3d4", 5)
	padding := strings.Repeat("z", 100<<10)
	for _, tc := range []struct {
		name   string
		status int
		call   func(p *Provisioner) error
	}{
		{"run-status 5xx", http.StatusBadGateway, func(p *Provisioner) error {
			_, err := p.runConclusion(context.Background(), "myorg", "myrepo", "42")
			return err
		}},
		{"run-status 404", http.StatusNotFound, func(p *Provisioner) error {
			_, err := p.runConclusion(context.Background(), "myorg", "myrepo", "42")
			return err
		}},
		{"run-status 4xx", http.StatusUnprocessableEntity, func(p *Provisioner) error {
			_, err := p.runConclusion(context.Background(), "myorg", "myrepo", "42")
			return err
		}},
		{"re-run 403", http.StatusForbidden, func(p *Provisioner) error {
			return p.rerunFailedJobs(context.Background(), "myorg", "myrepo", "42", discardLog)
		}},
		{"force-cancel 500", http.StatusInternalServerError, func(p *Provisioner) error {
			return p.forceCancelRun(context.Background(), "myorg", "myrepo", "42")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"message":"echo: Authorization: token ` + token + `"}` + padding))
			}))
			t.Cleanup(srv.Close)
			p := &Provisioner{
				TokenFunc:    func(context.Context) (string, error) { return token, nil },
				GitHubAPIURL: srv.URL,
				HTTPClient:   srv.Client(),
			}
			err := tc.call(p)
			require.Error(t, err)
			assert.NotContains(t, err.Error(), token, "the echoed token survives into the error")
			assert.Less(t, len(err.Error()), 2<<10, "the body is not capped")
			assert.Contains(t, err.Error(), "[REDACTED]")
		})
	}
}
