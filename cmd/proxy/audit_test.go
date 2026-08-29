package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

// auditSink captures a server's JSON log stream so a test can assert on the
// records it emitted — and, just as load-bearing here, on the ones it did not.
type auditSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *auditSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

// records returns every log line whose msg is the audit message, decoded.
func (s *auditSink) records(t *testing.T) []map[string]any {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]any
	for _, line := range strings.Split(s.buf.String(), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &rec), "log line is not JSON: %s", line)
		if rec["msg"] == auditMsg {
			out = append(out, rec)
		}
	}
	return out
}

// newAuditServer returns a proxy whose log stream is captured, in the given mode.
func newAuditServer(t *testing.T, mode AuditMode) (*Server, *auditSink) {
	t.Helper()
	srv, _ := newTestServer(t)
	sink := &auditSink{}
	srv.Log = slog.New(slog.NewJSONHandler(sink, &slog.HandlerOptions{Level: slog.LevelInfo}))
	srv.AuditLogging = mode
	return srv, sink
}

// tunnelOnce opens one CONNECT tunnel through srv to target, writes payload,
// reads the echo back, and closes the client end. It returns once the handler
// has finished, so an audit record it writes is in the sink.
func tunnelOnce(t *testing.T, srv *Server, target, payload string) {
	t.Helper()
	handlerDone := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(handlerDone)
		srv.handleConnect(w, r)
	}))
	t.Cleanup(ts.Close)

	conn, err := net.Dial("tcp", ts.Listener.Addr().String())
	require.NoError(t, err)
	_, _ = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	_, err = io.WriteString(conn, payload)
	require.NoError(t, err)
	buf := make([]byte, len(payload))
	_, err = io.ReadFull(conn, buf)
	require.NoError(t, err)
	require.Equal(t, payload, string(buf))

	_ = conn.Close()
	select {
	case <-handlerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("CONNECT handler did not return after the client closed")
	}
}

// TestAudit_OffEmitsNoRecord is the control for every assertion below: the
// default mode must produce no per-connection record even on a tunnel that
// carried traffic. It is the security property the field exists to preserve, so
// it is asserted directly rather than inferred from the enabled case.
func TestAudit_OffEmitsNoRecord(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })

	echoAddr := startEchoServer(t)
	srv, sink := newAuditServer(t, AuditOff)
	require.Equal(t, AuditOff, srv.AuditLogging, "zero value must be Off")

	tunnelOnce(t, srv, echoAddr, "hello proxy")

	assert.Empty(t, sink.records(t), "audit Off must write no per-connection record")
}

// TestAudit_ZeroValueIsOff pins that a Server nobody configured records nothing:
// the off-by-default guarantee must not depend on main.go having run.
func TestAudit_ZeroValueIsOff(t *testing.T) {
	var s Server
	assert.NotEqual(t, AuditConnections, s.AuditLogging)
	assert.Equal(t, AuditMode(""), s.AuditLogging)
}

func TestAudit_ConnectionsRecordsAcceptedConnect(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })

	echoAddr := startEchoServer(t)
	srv, sink := newAuditServer(t, AuditConnections)
	srv.Namespace = "tenant-a"

	payload := "hello proxy"
	tunnelOnce(t, srv, echoAddr, payload)

	recs := sink.records(t)
	require.Len(t, recs, 1, "one accepted CONNECT must produce exactly one record")
	rec := recs[0]

	host, port, err := net.SplitHostPort(echoAddr)
	require.NoError(t, err)
	assert.Equal(t, "tenant-a", rec["namespace"])
	assert.Equal(t, auditEventConnect, rec["event"])
	assert.Equal(t, host, rec["host"])
	assert.Equal(t, port, rec["port"])
	assert.Equal(t, float64(len(payload)), rec["bytesToDestination"])
	assert.Equal(t, float64(len(payload)), rec["bytesFromDestination"])
	assert.GreaterOrEqual(t, rec["durationSeconds"], float64(0))
	assert.Equal(t, "INFO", rec["level"], "an audit record must not need LOG_LEVEL=debug")
}

// TestAudit_RecordCarriesNoRequestHeaders is the negative half of the field
// choice: a worker that sends credential-shaped headers on the CONNECT must not
// see any of them in the record. The proxy never reads r.Header on this path;
// this asserts that from the outside, where a future edit would break it.
func TestAudit_RecordCarriesNoRequestHeaders(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })

	echoAddr := startEchoServer(t)
	srv, sink := newAuditServer(t, AuditConnections)

	// Deliberately credential-shaped so a redactor-style false pass is visible:
	// if this ever appears, the record read the header map.
	const headerMarker = "ghs_ThisMustNeverReachTheLogLine"
	handlerDone := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(handlerDone)
		srv.handleConnect(w, r)
	}))
	t.Cleanup(ts.Close)

	conn, err := net.Dial("tcp", ts.Listener.Addr().String())
	require.NoError(t, err)
	_, _ = fmt.Fprintf(conn,
		"CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: Bearer %s\r\nX-Tenant-Token: %s\r\n\r\n",
		echoAddr, echoAddr, headerMarker, headerMarker)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	_ = conn.Close()
	<-handlerDone

	require.Len(t, sink.records(t), 1)
	sink.mu.Lock()
	defer sink.mu.Unlock()
	assert.NotContains(t, sink.buf.String(), headerMarker,
		"no header value may reach the log stream")
	assert.NotContains(t, strings.ToLower(sink.buf.String()), "proxy-authorization")
}

// TestAudit_TunneledBytesNeverReachTheRecord asserts the record counts payload
// rather than carrying it: what moved through the tunnel is a number, never
// content.
func TestAudit_TunneledBytesNeverReachTheRecord(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })

	echoAddr := startEchoServer(t)
	srv, sink := newAuditServer(t, AuditConnections)

	const payload = "PAYLOAD-MARKER-b2f1c9"
	tunnelOnce(t, srv, echoAddr, payload)

	require.Len(t, sink.records(t), 1)
	sink.mu.Lock()
	defer sink.mu.Unlock()
	assert.NotContains(t, sink.buf.String(), payload,
		"tunneled bytes must be counted, never logged")
}

// TestAudit_RefusedConnectWritesNoRecord: the record is per ACCEPTED CONNECT.
// A refusal is already covered by the deny counter and its warn line, and
// recording it here would double-count a connection that never carried egress.
func TestAudit_RefusedConnectWritesNoRecord(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })

	srv, sink := newAuditServer(t, AuditConnections)
	srv.AllowedHostSuffixes = []string{"github.com"}

	ts := httptest.NewServer(http.HandlerFunc(srv.handleConnect))
	t.Cleanup(ts.Close)

	conn, err := net.Dial("tcp", ts.Listener.Addr().String())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	_, _ = fmt.Fprint(conn, "CONNECT evil.example.com:443 HTTP/1.1\r\nHost: evil.example.com:443\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)

	assert.Empty(t, sink.records(t), "a refused CONNECT is not an accepted one")
}

// TestAudit_DialFailureWritesNoRecord: same rule on the other rejection path.
func TestAudit_DialFailureWritesNoRecord(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })

	srv, sink := newAuditServer(t, AuditConnections)
	srv.DialTimeout = 100 * time.Millisecond

	ts := httptest.NewServer(http.HandlerFunc(srv.handleConnect))
	t.Cleanup(ts.Close)

	conn, err := net.Dial("tcp", ts.Listener.Addr().String())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	_, _ = fmt.Fprint(conn, "CONNECT 127.0.0.1:1 HTTP/1.1\r\nHost: 127.0.0.1:1\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusBadGateway, resp.StatusCode)

	assert.Empty(t, sink.records(t))
}

// TestAudit_NamespaceOmittedWhenUnset keeps a standalone proxy from emitting an
// empty attribution field that a collector would index as a real namespace.
func TestAudit_NamespaceOmittedWhenUnset(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })

	echoAddr := startEchoServer(t)
	srv, sink := newAuditServer(t, AuditConnections)

	tunnelOnce(t, srv, echoAddr, "x")

	recs := sink.records(t)
	require.Len(t, recs, 1)
	_, present := recs[0]["namespace"]
	assert.False(t, present, "an unset namespace must be omitted, not empty")
}

func TestParseAuditMode(t *testing.T) {
	// Everything unrecognized must resolve to Off: a GMC newer than the proxy
	// image can inject a mode this binary does not know, and under-recording is
	// the only safe direction.
	for _, in := range []string{"", "  ", "off", "Off", "OFF", "on", "true", "1", "Connections\n\r", "connectionz", "Full", "ConnectionsWithSourceIP", "WithSource"} {
		got := parseAuditMode(in)
		switch trimmed := strings.TrimSpace(in); {
		case strings.EqualFold(trimmed, "connections"):
			assert.Equal(t, AuditConnections, got, "input %q", in)
		case strings.EqualFold(trimmed, "connectionswithsource"):
			assert.Equal(t, AuditConnectionsWithSource, got, "input %q", in)
		default:
			assert.Equal(t, AuditOff, got, "input %q must resolve to Off", in)
		}
	}
	assert.Equal(t, AuditConnections, parseAuditMode("Connections"))
	assert.Equal(t, AuditConnections, parseAuditMode("connections"))
	assert.Equal(t, AuditConnections, parseAuditMode("  Connections  "))
	assert.Equal(t, AuditConnectionsWithSource, parseAuditMode("ConnectionsWithSource"))
	assert.Equal(t, AuditConnectionsWithSource, parseAuditMode("  connectionswithsource  "))

	// The prefix relationship between the two enabled values is the one way a
	// naive matcher silently under-records: ConnectionsWithSource must not fall
	// back to Connections, and Connections must not widen into it.
	assert.False(t, parseAuditMode("Connections").carriesSource())
	assert.True(t, parseAuditMode("ConnectionsWithSource").records())
}

// TestAudit_ConnectionsOmitsSourceAddress is the control for the source-address
// half: the mode that shipped first must keep recording exactly what it did, so
// upgrading the proxy image cannot start a movement log on a pool that opted
// into Connections and nothing more.
func TestAudit_ConnectionsOmitsSourceAddress(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })

	echoAddr := startEchoServer(t)
	srv, sink := newAuditServer(t, AuditConnections)

	tunnelOnce(t, srv, echoAddr, "hello proxy")

	recs := sink.records(t)
	require.Len(t, recs, 1)
	_, ip := recs[0]["sourceIP"]
	_, port := recs[0]["sourcePort"]
	assert.False(t, ip, "Connections must not carry sourceIP")
	assert.False(t, port, "Connections must not carry sourcePort")
}

// TestAudit_ConnectionsWithSourceRecordsClientAddress drives the new mode through
// the real handler, so the address on the record is the one net/http read off the
// accepted connection rather than anything the test handed the logger.
func TestAudit_ConnectionsWithSourceRecordsClientAddress(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })

	echoAddr := startEchoServer(t)
	srv, sink := newAuditServer(t, AuditConnectionsWithSource)
	srv.Namespace = "tenant-a"

	tunnelOnce(t, srv, echoAddr, "hello proxy")

	recs := sink.records(t)
	require.Len(t, recs, 1)
	assert.Equal(t, "tenant-a", recs[0]["namespace"])
	assert.Equal(t, auditEventConnect, recs[0]["event"])

	// The loopback client dialled from an ephemeral port on the local address,
	// which is what the record must name — not the destination, and not empty.
	sourceIP, _ := recs[0]["sourceIP"].(string)
	require.NotEmpty(t, sourceIP, "ConnectionsWithSource must carry the client address")
	assert.True(t, net.ParseIP(sourceIP).IsLoopback(), "sourceIP %q must be the client's, not the destination's", sourceIP)
	sourcePort, _ := recs[0]["sourcePort"].(string)
	require.NotEmpty(t, sourcePort)
	n, err := strconv.Atoi(sourcePort)
	require.NoError(t, err, "sourcePort %q must be numeric", sourcePort)
	assert.Positive(t, n)
}

// TestSplitSourceAddr covers the shape a RemoteAddr can take that the record
// still has to report: net/http always sets host:port, but a Server driven
// directly (or a future non-TCP listener) need not, and dropping the address
// would silently lose attribution rather than fail.
func TestSplitSourceAddr(t *testing.T) {
	addr, port := splitSourceAddr("10.1.2.3:41000")
	assert.Equal(t, "10.1.2.3", addr)
	assert.Equal(t, "41000", port)

	addr, port = splitSourceAddr("[fd00::1]:41000")
	assert.Equal(t, "fd00::1", addr)
	assert.Equal(t, "41000", port)

	addr, port = splitSourceAddr("no-port-here")
	assert.Equal(t, "no-port-here", addr)
	assert.Equal(t, "", port)
}

func TestTruncateHost(t *testing.T) {
	short := strings.Repeat("a", maxAuditHostLen)
	assert.Equal(t, short, truncateHost(short), "a legal-length host is untouched")

	long := strings.Repeat("b", maxAuditHostLen+1)
	got := truncateHost(long)
	assert.True(t, strings.HasSuffix(got, auditTruncatedSuffix), "a cut host must say so")
	assert.Equal(t, maxAuditHostLen, len(strings.TrimSuffix(got, auditTruncatedSuffix)))
}

// TestAudit_HostIsTruncatedInRecord drives the cap through the real handler: the
// CONNECT authority is tenant-controlled text, and http.Server admits a request
// line far longer than any legal DNS name.
func TestAudit_HostIsTruncatedInRecord(t *testing.T) {
	srv, sink := newAuditServer(t, AuditConnections)
	longHost := strings.Repeat("c", maxAuditHostLen+50)

	srv.logConnectAudit(longHost+":443", "10.1.2.3:41000", 1, 2, time.Second)

	recs := sink.records(t)
	require.Len(t, recs, 1)
	host, _ := recs[0]["host"].(string)
	assert.Equal(t, maxAuditHostLen+len(auditTruncatedSuffix), len(host))
	assert.Equal(t, "443", recs[0]["port"])
}

// TestAudit_UnsplittableAuthorityIsLoggedWhole: a CONNECT authority with no port
// is malformed, and dropping the record would lose the one line saying a tenant
// sent it. Log it as the host with an empty port instead.
func TestAudit_UnsplittableAuthorityIsLoggedWhole(t *testing.T) {
	srv, sink := newAuditServer(t, AuditConnections)

	srv.logConnectAudit("no-port-here", "10.1.2.3:41000", 3, 4, 2*time.Second)

	recs := sink.records(t)
	require.Len(t, recs, 1)
	assert.Equal(t, "no-port-here", recs[0]["host"])
	assert.Equal(t, "", recs[0]["port"])
	assert.Equal(t, float64(3), recs[0]["bytesToDestination"])
	assert.Equal(t, float64(4), recs[0]["bytesFromDestination"])
	assert.Equal(t, float64(2), recs[0]["durationSeconds"])
}

// TestLogging_OverLengthAuthorityIsBoundedOnEveryPath is the reachability test
// behind the cap. The CONNECT authority is tenant-controlled and http.Server
// admits a request line up to MaxHeaderBytes, so an uncapped log site is a
// log-volume amplifier a worker drives on demand.
//
// The dial-failure path is the one that matters in the GMC-built configuration:
// with an allowlist injected, matchesHostSuffix does a bare strings.HasSuffix
// with no length bound, so junk+".github.com" PASSES the allowlist and reaches
// the dial. The deny path is only reachable for a host that fails the match.
// Both the host attr and the dial error must be bounded — the error embeds the
// same authority, so capping only the attr halves the fix.
func TestLogging_OverLengthAuthorityIsBoundedOnEveryPath(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })

	const junkLen = 200000
	junkHost := strings.Repeat("a", junkLen) + ".github.com"

	t.Run("dial failure under an allowlist that the junk host passes", func(t *testing.T) {
		srv, sink := newAuditServer(t, AuditOff)
		srv.AllowedHostSuffixes = []string{"github.com"}
		srv.DialTimeout = 100 * time.Millisecond

		require.True(t, matchesHostSuffix(junkHost, srv.AllowedHostSuffixes),
			"precondition: the junk host must PASS the allowlist, else this exercises the deny path")

		ts := httptest.NewServer(http.HandlerFunc(srv.handleConnect))
		t.Cleanup(ts.Close)
		conn, err := net.Dial("tcp", ts.Listener.Addr().String())
		require.NoError(t, err)
		defer func() { _ = conn.Close() }()
		_, _ = fmt.Fprintf(conn, "CONNECT %s:443 HTTP/1.1\r\nHost: h\r\n\r\n", junkHost)
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		require.NoError(t, err)
		require.Equal(t, http.StatusBadGateway, resp.StatusCode,
			"precondition: the request must reach the dial, not be refused")

		sink.mu.Lock()
		got := sink.buf.Len()
		sink.mu.Unlock()
		assert.Less(t, got, 4096,
			"an over-length authority must not reach the log stream unbounded (got %d bytes for a %d-byte authority)", got, junkLen)
	})

	// The ACCEPTED path is the one this PR adds and the only one that SPLITS the
	// authority, so it is the only place a cap on the host alone can leak. Go's
	// port parser ignores leading zeros without saturating, so a zero-padded port
	// dials the real port and carries arbitrary length into the record.
	t.Run("accepted CONNECT with a zero-padded port", func(t *testing.T) {
		echoAddr := startEchoServer(t)
		echoHost, echoPort, err := net.SplitHostPort(echoAddr)
		require.NoError(t, err)
		padded := net.JoinHostPort(echoHost, strings.Repeat("0", 200000)+echoPort)

		srv, sink := newAuditServer(t, AuditConnections)
		handlerDone := make(chan struct{})
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer close(handlerDone)
			srv.handleConnect(w, r)
		}))
		t.Cleanup(ts.Close)

		conn, err := net.Dial("tcp", ts.Listener.Addr().String())
		require.NoError(t, err)
		defer func() { _ = conn.Close() }()
		_, _ = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: h\r\n\r\n", padded)
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode,
			"precondition: the padded port must DIAL, so the accepted path is what runs")
		_ = conn.Close()
		<-handlerDone

		require.Len(t, sink.records(t), 1, "precondition: an accepted CONNECT must record")
		sink.mu.Lock()
		got := sink.buf.Len()
		sink.mu.Unlock()
		assert.Less(t, got, 4096,
			"the authority is host:port; capping only the host leaves the record unbounded (got %d bytes)", got)
	})

	t.Run("denied destination", func(t *testing.T) {
		srv, sink := newAuditServer(t, AuditOff)
		srv.AllowedHostSuffixes = []string{"example.invalid"}

		ts := httptest.NewServer(http.HandlerFunc(srv.handleConnect))
		t.Cleanup(ts.Close)
		conn, err := net.Dial("tcp", ts.Listener.Addr().String())
		require.NoError(t, err)
		defer func() { _ = conn.Close() }()
		_, _ = fmt.Fprintf(conn, "CONNECT %s:443 HTTP/1.1\r\nHost: h\r\n\r\n", junkHost)
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		require.NoError(t, err)
		require.Equal(t, http.StatusForbidden, resp.StatusCode)

		sink.mu.Lock()
		got := sink.buf.Len()
		sink.mu.Unlock()
		assert.Less(t, got, 4096, "got %d bytes", got)
	})
}

// TestTruncateForLog_CutsOnARuneBoundary: maxAuditHostLen is a byte cap, and an
// IDN authority sent as raw UTF-8 can put a multi-byte rune across it. Emitting
// the partial rune would render as U+FFFD in the JSON stream.
func TestTruncateForLog_CutsOnARuneBoundary(t *testing.T) {
	// "é" is two bytes, so a cap of 3 lands mid-rune on the second one.
	got := truncateForLog("éé", 3)
	body := strings.TrimSuffix(got, auditTruncatedSuffix)
	assert.True(t, utf8.ValidString(body), "truncated body must stay valid UTF-8, got %q", body)
	assert.Equal(t, "é", body, "the partial rune is dropped, not emitted")

	// The whole-rune case is untouched.
	assert.Equal(t, "éé", truncateForLog("éé", 4))
}

func TestTruncateLogError_IsBounded(t *testing.T) {
	short := errors.New("dial tcp: connection refused")
	assert.Equal(t, short.Error(), truncateLogError(short), "a real diagnostic is never cut")

	long := errors.New(strings.Repeat("x", maxLoggedErrorLen*3))
	got := truncateLogError(long)
	assert.True(t, strings.HasSuffix(got, auditTruncatedSuffix))
	assert.Equal(t, maxLoggedErrorLen, len(strings.TrimSuffix(got, auditTruncatedSuffix)))
}
