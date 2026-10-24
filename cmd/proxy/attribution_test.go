package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"syscall"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

// testFailureStore returns a store with small caps and a settable clock.
func testFailureStore(t *testing.T, perNS, perSource int) (*failureStore, *time.Time) {
	t.Helper()
	f, err := newFailureStore()
	require.NoError(t, err)
	now := time.Unix(1_700_000_000, 0)
	f.now = func() time.Time { return now }
	f.perNS, f.perSource = perNS, perSource
	return f, &now
}

func sources(recs []failureRecord) []string {
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.SourceIP)
	}
	return out
}

func TestFailureStore_PerSourceCapEvictsOnlyThatSource(t *testing.T) {
	f, _ := testFailureStore(t, 100, 2)
	f.record("ns", failureRecord{SourceIP: "10.0.0.1", Host: "a"})
	f.record("ns", failureRecord{SourceIP: "10.0.0.2", Host: "b"})
	f.record("ns", failureRecord{SourceIP: "10.0.0.1", Host: "c"})
	f.record("ns", failureRecord{SourceIP: "10.0.0.1", Host: "d"})

	_, recs := f.read("ns", 0)
	hosts := []string{}
	for _, r := range recs {
		hosts = append(hosts, r.Host)
	}
	assert.Equal(t, []string{"b", "c", "d"}, hosts,
		"the third record from 10.0.0.1 evicts its own oldest, never 10.0.0.2's")
}

func TestFailureStore_PartitionsIsolateNamespaces(t *testing.T) {
	f, _ := testFailureStore(t, 3, 100)
	f.record("victim", failureRecord{SourceIP: "10.1.0.1"})
	for i := range 10 {
		f.record("noisy", failureRecord{SourceIP: fmt.Sprintf("10.2.0.%d", i)})
	}

	_, victim := f.read("victim", 0)
	assert.Equal(t, []string{"10.1.0.1"}, sources(victim),
		"filling one namespace's partition must not evict another namespace's records")
	_, noisy := f.read("noisy", 0)
	assert.Equal(t, []string{"10.2.0.7", "10.2.0.8", "10.2.0.9"}, sources(noisy),
		"a partition at its cap keeps its newest records")
}

func TestFailureStore_ExpiresAfterWindow(t *testing.T) {
	f, now := testFailureStore(t, 100, 100)
	f.record("ns", failureRecord{SourceIP: "old"})
	*now = now.Add(failureWindow - time.Second)
	f.record("ns", failureRecord{SourceIP: "new"})
	*now = now.Add(2 * time.Second)

	_, recs := f.read("ns", 0)
	assert.Equal(t, []string{"new"}, sources(recs))
}

func TestFailureStore_ReadAfterCursorAndEpoch(t *testing.T) {
	f, _ := testFailureStore(t, 100, 100)
	f.record("ns", failureRecord{SourceIP: "a"})
	f.record("other", failureRecord{SourceIP: "x"})
	f.record("ns", failureRecord{SourceIP: "b"})

	epoch, all := f.read("ns", 0)
	require.Len(t, all, 2)
	assert.Equal(t, []uint64{1, 2}, []uint64{all[0].Seq, all[1].Seq},
		"another namespace's record leaves no gap in this one's sequence")

	epoch2, rest := f.read("ns", all[0].Seq)
	assert.Equal(t, epoch, epoch2, "the epoch is fixed for the life of the process")
	assert.Equal(t, []string{"b"}, sources(rest))

	g, err := newFailureStore()
	require.NoError(t, err)
	assert.NotEqual(t, f.epoch, g.epoch, "a restarted proxy reports a new epoch")
}

// timeoutErr is a net.Error that reports a timeout.
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestClassifyDialError(t *testing.T) {
	_, refused := net.DialTimeout("tcp", "127.0.0.1:1", time.Second)
	require.ErrorIs(t, refused, syscall.ECONNREFUSED, "precondition: nothing listens on port 1")

	cases := map[string]error{
		"refused": refused,
		"dns":     &net.OpError{Op: "dial", Err: &net.DNSError{Err: "no such host", Name: "x.invalid", IsTimeout: true}},
		"timeout": &net.OpError{Op: "dial", Err: timeoutErr{}},
		"other":   fmt.Errorf("something else"),
	}
	for want, err := range cases {
		assert.Equal(t, want, classifyDialError(err), "%v", err)
	}
}

func TestParseAttributionPorts(t *testing.T) {
	got, err := parseAttributionPorts(" team-a = 08090 , team-b=8091")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"team-a": ":8090", "team-b": ":8091"}, got)

	none, err := parseAttributionPorts("")
	require.NoError(t, err)
	assert.Nil(t, none)

	for _, bad := range []string{
		"team-a",           // no port
		"Team_A=8090",      // not a namespace name
		"team-a=0",         // out of range
		"team-a=http",      // not a number
		"a=8090,a=8091",    // namespace twice
		"a=8090,b=8090",    // port twice
		"=8090",            // empty namespace
		"team-a=8090=8091", // trailing junk
	} {
		_, err := parseAttributionPorts(bad)
		assert.Error(t, err, "%q must be rejected", bad)
	}
}

// connectThrough sends one CONNECT for target to the proxy listener at addr and
// returns the response status.
func connectThrough(t *testing.T, addr, target string) int {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	_, _ = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err)
	_ = resp.Body.Close()
	return resp.StatusCode
}

// startAttributedProxy starts a Server with attributed listeners for team-a and
// team-b, an allowlist admitting only 127.0.0.1, and the attribution endpoint.
func startAttributedProxy(t *testing.T, ca *testCA) *Server {
	t.Helper()
	certFile, keyFile, caFile := writeServerBundle(t, ca, ca.leaf(t, "127.0.0.1", true))
	srv := NewServer(testListenAddr, testListenAddr, 500*time.Millisecond, nil, prometheus.NewRegistry())
	srv.AllowedCIDRs = []*net.IPNet{{IP: net.IPv4(127, 0, 0, 1), Mask: net.CIDRMask(32, 32)}}
	srv.AttributedAddrs = map[string]string{"team-a": testListenAddr, "team-b": testListenAddr}
	srv.AttributionAddr = testListenAddr
	srv.AttributionTLSCertFile, srv.AttributionTLSKeyFile, srv.AttributionClientCAFile = certFile, keyFile, caFile
	srv.ShutdownLinger = -1
	startServer(t, srv)
	return srv
}

func readFailures(t *testing.T, ca *testCA, srv *Server, cn string) (int, failuresResponse) {
	t.Helper()
	cert := ca.leaf(t, cn, false)
	resp, err := httpsClient(ca, &cert).Get("https://" + srv.AttributionAddr + "/v1/failures")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	var body failuresResponse
	if resp.StatusCode == http.StatusOK {
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	}
	return resp.StatusCode, body
}

func TestAttribution_RecordsByListenerAndServesOnlyTheCertsNamespace(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })
	ca := newTestCA(t)
	srv := startAttributedProxy(t, ca)

	assert.Equal(t, http.StatusForbidden, connectThrough(t, srv.AttributedAddrs["team-a"], "blocked.example:443"))
	assert.Equal(t, http.StatusBadGateway, connectThrough(t, srv.AttributedAddrs["team-b"], "127.0.0.1:1"))
	// The unattributed port refuses the same way and keeps nothing.
	assert.Equal(t, http.StatusForbidden, connectThrough(t, srv.Addr, "unattributed.example:443"))

	status, a := readFailures(t, ca, srv, "team-a")
	require.Equal(t, http.StatusOK, status)
	require.Len(t, a.Records, 1, "team-a sees its own denial only: %+v", a.Records)
	assert.Equal(t, outcomeDenied, a.Records[0].Outcome)
	assert.Equal(t, "blocked.example", a.Records[0].Host)
	assert.Equal(t, "443", a.Records[0].Port)
	assert.Equal(t, "127.0.0.1", a.Records[0].SourceIP)
	assert.NotEmpty(t, a.Epoch)

	status, b := readFailures(t, ca, srv, "team-b")
	require.Equal(t, http.StatusOK, status)
	require.Len(t, b.Records, 1, "team-b sees its own dial failure only: %+v", b.Records)
	assert.Equal(t, outcomeDialFailed, b.Records[0].Outcome)
	assert.Equal(t, "refused", b.Records[0].DialClass)

	status, _ = readFailures(t, ca, srv, "team-c")
	assert.Equal(t, http.StatusForbidden, status, "a CA-signed cert for an unmapped namespace reads nothing")
}

func TestAttribution_RejectsClientWithoutCert(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })
	ca := newTestCA(t)
	srv := startAttributedProxy(t, ca)

	_, err := httpsClient(ca, nil).Get("https://" + srv.AttributionAddr + "/v1/failures")
	assert.Error(t, err, "the handshake must refuse a client with no certificate")
}

func TestAttribution_RejectsCertFromAnotherCA(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })
	ca := newTestCA(t)
	srv := startAttributedProxy(t, ca)

	other := newTestCA(t)
	cert := other.leaf(t, "team-a", false)
	_, err := httpsClient(ca, &cert).Get("https://" + srv.AttributionAddr + "/v1/failures")
	assert.Error(t, err, "a team-a cert from another CA must not read team-a's records")
}

func TestAttribution_RefusesPortsWithoutMTLS(t *testing.T) {
	srv := NewServer(testListenAddr, testListenAddr, time.Second, nil, prometheus.NewRegistry())
	srv.AttributedAddrs = map[string]string{"team-a": testListenAddr}
	err := srv.ListenAndServe(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "attribution mTLS")
}

func TestAttribution_UnconfiguredKeepsNothing(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })
	srv := NewServer(testListenAddr, testListenAddr, time.Second, nil, prometheus.NewRegistry())
	srv.AllowedCIDRs = []*net.IPNet{{IP: net.IPv4(127, 0, 0, 1), Mask: net.CIDRMask(32, 32)}}
	startServer(t, srv)

	assert.Equal(t, http.StatusForbidden, connectThrough(t, srv.Addr, "blocked.example:443"))
	assert.Nil(t, srv.failures, "no attributed ports means no store at all")
}
