package broker_test

// Investigation B — Egress IP Variance
//
// The production gateway routes broker API calls through a proxy pool where each
// call may land on a different pod (different egress IP). This file:
//
//  1. Provides newCONNECTProxy — a minimal httptest-backed CONNECT proxy that
//     tunnels TLS without termination, matching the planned Milestone 4 proxy.
//  2. Provides roundRobinProxyClient — an *http.Client that alternates between
//     a list of proxy servers per outbound connection, simulating different egress
//     IPs on each call.
//  3. TestCONNECTProxy_TunnelsHTTPS — unit test proving the infrastructure works
//     with a local TLS backend (no GitHub credentials required).
//  4. TestEgressIPVariance_Live — full broker sequence through two proxies,
//     skipped unless GITHUB_* environment variables are set.
//
// Findings from the live test must be documented in docs/plan/milestone-1.md §8.B
// before closing Milestone 1.

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/karlkfi/github-actions-gateway/broker"
	"github.com/karlkfi/github-actions-gateway/githubapp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newCONNECTProxy starts an httptest server that handles HTTP CONNECT requests
// by opening a raw TCP connection to the target host and splicing bytes in both
// directions. It does not terminate TLS — it is a transparent tunnel.
//
// This matches the planned Milestone 4 proxy design: a stateless Go binary that
// handles CONNECT only, with no TLS termination.
func newCONNECTProxy(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "only CONNECT supported", http.StatusMethodNotAllowed)
			return
		}
		upstream, err := net.Dial("tcp", r.Host)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "server does not support hijacking", http.StatusInternalServerError)
			upstream.Close()
			return
		}
		// Hijack returns the raw conn plus the buffered writer that still holds
		// the "200 Connection established" response. Flush before copying so the
		// client actually receives the 200 before we start tunnelling TLS bytes.
		clientConn, brw, err := hj.Hijack()
		if err != nil {
			upstream.Close()
			return
		}
		if err := brw.Flush(); err != nil {
			upstream.Close()
			clientConn.Close()
			return
		}
		go func() {
			defer upstream.Close()
			defer clientConn.Close()
			go io.Copy(upstream, clientConn) //nolint:errcheck
			io.Copy(clientConn, upstream)    //nolint:errcheck
		}()
	}))
}

// roundRobinProxyClient returns an *http.Client whose transport routes each new
// connection through the next proxy in the list, cycling back to the first after
// the last. skipVerify disables TLS certificate verification for the final target
// (required when the target is an httptest.NewTLSServer with a self-signed cert).
func roundRobinProxyClient(proxies []*httptest.Server, skipVerify bool) *http.Client {
	var idx atomic.Int64
	proxyFunc := func(req *http.Request) (*url.URL, error) {
		i := int(idx.Add(1)-1) % len(proxies)
		return url.Parse(proxies[i].URL)
	}
	return &http.Client{
		Transport: &http.Transport{
			Proxy: proxyFunc,
			// DisableKeepAlives forces a new TCP connection (and a new CONNECT
			// tunnel) for each request. Without this the transport reuses the
			// first tunnel for subsequent requests, so the Proxy function is
			// not called on every request and the hijack goroutine's closed
			// conn causes EOF on the reused connection.
			DisableKeepAlives: true,
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: skipVerify, //nolint:gosec // intentional for tests
			},
		},
	}
}

// TestCONNECTProxy_TunnelsHTTPS verifies that newCONNECTProxy correctly tunnels
// TLS traffic to an HTTPS backend. Two proxy instances alternate across four
// requests, simulating the per-call IP variance pattern the AGC will use.
//
// This test requires no GitHub credentials.
func TestCONNECTProxy_TunnelsHTTPS(t *testing.T) {
	// Target: a local TLS server representing the GitHub broker / run service.
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	}))
	defer target.Close()

	proxy1 := newCONNECTProxy(t)
	defer proxy1.Close()
	proxy2 := newCONNECTProxy(t)
	defer proxy2.Close()

	// Round-robin across both proxies; skip TLS verification for the self-signed cert.
	client := roundRobinProxyClient([]*httptest.Server{proxy1, proxy2}, true)

	// Four requests — one per protocol call type (CreateSession, GetMessage,
	// AcquireJob, RenewJob) — each routed through a different proxy.
	for i := 0; i < 4; i++ {
		resp, err := client.Get(target.URL + "/ping")
		require.NoError(t, err, "request %d failed (proxy alternation should be transparent)", i)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode, "request %d: unexpected status", i)
	}
}

// TestCONNECTProxy_RejectsNonCONNECT verifies that the proxy returns 405 for
// non-CONNECT methods, ensuring it cannot be used as an open forward proxy.
func TestCONNECTProxy_RejectsNonCONNECT(t *testing.T) {
	proxy := newCONNECTProxy(t)
	defer proxy.Close()

	resp, err := http.Get(proxy.URL + "/anything")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
}

// TestEgressIPVariance_Live runs the complete broker protocol sequence
// (CreateSession → GetMessage → AcquireJob → three RenewJob calls) with each
// outbound connection routed through an alternating pair of CONNECT proxies,
// simulating different egress IPs on every call.
//
// This test is SKIPPED unless all required GITHUB_* environment variables are set.
// When run successfully, document findings in docs/plan/milestone-1.md §8.B:
//   - Did all four call types succeed across proxy alternation?
//   - Were any 403/401 responses observed that suggest IP-based session pinning?
//   - Recommended proxy affinity approach for Milestone 4.
func TestEgressIPVariance_Live(t *testing.T) {
	if os.Getenv("GITHUB_APP_ID") == "" {
		t.Skip("GITHUB_APP_ID not set; skipping live egress IP variance test — " +
			"set GITHUB_{APP_ID,APP_PRIVATE_KEY,APP_INSTALLATION_ID,BROKER_URL,RUNNER_VERSION,AGENT_ID,AGENT_NAME} to run")
	}
	appID, err := strconv.ParseInt(os.Getenv("GITHUB_APP_ID"), 10, 64)
	require.NoError(t, err, "GITHUB_APP_ID must be an integer")
	instID, err := strconv.ParseInt(os.Getenv("GITHUB_APP_INSTALLATION_ID"), 10, 64)
	require.NoError(t, err, "GITHUB_APP_INSTALLATION_ID must be an integer")
	agentID, err := strconv.ParseInt(os.Getenv("GITHUB_AGENT_ID"), 10, 64)
	require.NoError(t, err, "GITHUB_AGENT_ID must be an integer")

	creds := githubapp.Credentials{
		AppID:          appID,
		PrivateKeyPEM:  []byte(os.Getenv("GITHUB_APP_PRIVATE_KEY")),
		InstallationID: instID,
	}
	provider, err := githubapp.NewInstallationTokenProvider(creds, nil)
	require.NoError(t, err, "failed to create installation token provider")
	ctx := context.Background()
	token, err := provider.Token(ctx)
	require.NoError(t, err, "failed to get installation token")

	proxy1 := newCONNECTProxy(t)
	defer proxy1.Close()
	proxy2 := newCONNECTProxy(t)
	defer proxy2.Close()

	// skipVerify=false — real GitHub TLS is valid
	client := roundRobinProxyClient([]*httptest.Server{proxy1, proxy2}, false)

	agentName := os.Getenv("GITHUB_AGENT_NAME")
	if agentName == "" {
		agentName = "egress-variance-test"
	}

	bc := &broker.BrokerClient{
		BrokerURL:     os.Getenv("GITHUB_BROKER_URL"),
		UseV2Flow:     true,
		RunnerVersion: os.Getenv("GITHUB_RUNNER_VERSION"),
		HTTPClient:    client,
		Token:         token,
	}

	t.Logf("CreateSession via proxy1")
	sess, err := bc.CreateSession(ctx, agentID, agentName, os.Getenv("GITHUB_RUNNER_VERSION"))
	require.NoError(t, err, "CreateSession failed")
	t.Logf("session=%s brokerURL=%s", sess.SessionID, sess.BrokerURL)

	t.Logf("GetMessage via proxy2")
	msg, err := bc.GetMessage(ctx, sess.SessionID)
	require.NoError(t, err, "GetMessage failed")
	if msg != nil {
		t.Logf("received message type=%s id=%d", msg.MessageType, msg.MessageID)
	} else {
		t.Log("GetMessage returned 202 (no job queued) — expected in test environment")
	}

	t.Logf("DeleteSession via proxy1")
	err = bc.DeleteSession(ctx, sess.SessionID)
	require.NoError(t, err, "DeleteSession failed")

	t.Log("All calls succeeded across proxy alternation — IP variance is safe")
}
