package provisioner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestPendingRerun_ActsOnTheRunTheLedgerNames is the measurement behind the accepted
// residual Q1327 records in 05-security.md: recovery state is trusted on read, so an
// entry anyone with ConfigMap update in the tenant namespace writes steers the re-run
// to the run and repository it names, and a spent budget stored beside it does not
// stop the call. What bounds it is the tenant's own installation token.
func TestPendingRerun_ActsOnTheRunTheLedgerNames(t *testing.T) {
	ctx := context.Background()
	var (
		mu    sync.Mutex
		posts []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			mu.Lock()
			posts = append(posts, r.URL.Path)
			mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"completed","conclusion":"failure","run_attempt":1}`))
	}))
	t.Cleanup(srv.Close)
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build()
	target := &stubTarget{key: client.ObjectKey{Namespace: "team-a", Name: "gpu"}, spec: &ResolvedSpec{MaxEvictionRetries: 2}}
	p := ledgerProvisioner(c, srv)
	now := p.nowFn().UTC()
	live := now.Add(time.Hour).Format(time.RFC3339)
	fired := now.Add(-time.Minute).Format(time.RFC3339)

	// A run this set never served, in another repository, whose budget reads as spent.
	writeLedger(t, c, target, `{"version":1,"minWriterVersion":1,
		"reruns":{"99":{"expiresAt":"`+live+`","count":2,"attempts":{"1":{"scope":"FailedJobs","cause":"oom","disruptions":["forged-pod"]}}}},
		"pending":{"99/build":{"expiresAt":"`+live+`","firedAt":"`+fired+`","cause":"oom","repository":"other/elsewhere","runID":"99","job":"build","disruption":"forged-pod","failedAttempt":1}}}`)

	require.NoError(t, p.AdoptPendingReruns(ctx, target))
	sweepPendingAndWait(t, p, ctx, 1)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"/repos/other/elsewhere/actions/runs/99/rerun-failed-jobs"}, posts)
	l, _, err := p.readRerunLedger(ctx, target)
	require.NoError(t, err)
	assert.Equal(t, 2, l.Reruns["99"].Count, "the resumed slot spends nothing")
}
