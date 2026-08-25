package main

import (
	"net"
	"strings"
	"time"
)

// AuditMode selects the per-connection egress record the proxy writes to its
// structured log stream, alongside the operational lines every pool emits.
//
// It is a policy, not a switch: a record names the destination a tenant's
// workers reached and when, which is data the platform must choose to retain
// rather than produce by default. AuditOff is therefore the default and the
// only mode that emits nothing per connection, and every path that resolves a
// mode fails toward it (docs/design/05-security.md).
type AuditMode string

const (
	// AuditOff writes no per-connection record. The secure default: an unset,
	// empty, or unrecognized PROXY_AUDIT_LOGGING lands here.
	AuditOff AuditMode = "Off"
	// AuditConnections writes one record per ACCEPTED CONNECT, at tunnel close
	// so the byte counts and duration in it are final. Refused and failed
	// CONNECTs are already covered by the existing warn/error lines and the
	// actions_gateway_proxy_connect_denied_total / _dial_errors_total counters.
	AuditConnections AuditMode = "Connections"
)

const (
	// auditMsg is the stable slog message on every audit record. Collectors
	// select the audit stream on it; auditEvent* discriminates within it, so a
	// later record kind reuses the message and varies the event.
	auditMsg = "egress audit"
	// auditEventConnect marks the per-CONNECT record.
	auditEventConnect = "connect"
	// maxAuditHostLen caps a logged CONNECT host at the longest legal DNS name,
	// in BYTES. The authority is tenant-controlled text and http.Server admits a
	// request line up to MaxHeaderBytes (1 MiB by default), so an uncapped host
	// is a log-volume amplifier a worker drives on demand: measured at 400,167
	// bytes written for one 200,000-byte authority before this cap covered every
	// site. Truncation is marked so a capped value never reads as the real
	// destination.
	maxAuditHostLen = 253
	// maxLoggedErrorLen caps an error string reaching a log line. A dial error
	// embeds the address it failed on, so the same tenant-controlled authority
	// reaches the stream through the error value as well as the host attribute —
	// capping only the attribute halves the fix. Sized well above any real
	// net.OpError so a genuine diagnostic is never cut.
	maxLoggedErrorLen = 512
	// auditTruncatedSuffix marks a value cut by either cap.
	auditTruncatedSuffix = "…(truncated)"
)

// parseAuditMode maps the PROXY_AUDIT_LOGGING value onto a mode. Anything it
// does not recognize — empty, misspelled, a value from a newer CRD than this
// binary knows — resolves to AuditOff, so a mismatch between the GMC and the
// proxy image can only under-record, never start recording unasked.
func parseAuditMode(s string) AuditMode {
	if strings.EqualFold(strings.TrimSpace(s), string(AuditConnections)) {
		return AuditConnections
	}
	return AuditOff
}

// truncateForLog bounds a tenant-influenced value at max BYTES. A cut can land
// mid-rune, which the JSON encoder would render as U+FFFD, so the partial rune
// is dropped rather than emitted.
func truncateForLog(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	return strings.ToValidUTF8(s[:maxBytes], "") + auditTruncatedSuffix
}

// truncateHost bounds a tenant-supplied CONNECT authority for logging. Every
// site in handleConnect that logs the authority goes through it — one policy
// for one field, so a reader of the security docs does not have to work out
// which log line is covered. See maxAuditHostLen.
func truncateHost(host string) string { return truncateForLog(host, maxAuditHostLen) }

// truncateLogError bounds an error for logging. See maxLoggedErrorLen.
func truncateLogError(err error) string { return truncateForLog(err.Error(), maxLoggedErrorLen) }

// logConnectAudit writes the per-connection egress record: which tenant reached
// which destination, for how long, and how much moved each way.
//
// What it deliberately does NOT carry is the point of the field list. The
// record is built from the CONNECT authority and two byte counters only — no
// request header is read (Proxy-Authorization above all), no tunneled byte is
// inspected, and nothing derived from the TLS session inside the tunnel is
// available to it. The namespace comes from the downward API, not from the
// request, so a worker cannot forge its own attribution. It names the POOL,
// though, which is the consuming tenant only when no other namespace references
// this pool (see Server.Namespace). The client's source IP is omitted: on an
// unshared pool it adds no attribution the namespace does not already carry,
// and including it would turn the record into a per-worker movement log. On a
// shared pool that trade is open rather than settled.
//
// hostport is the raw CONNECT authority; a value that does not split is logged
// whole as the host with no port, rather than dropped.
func (s *Server) logConnectAudit(hostport string, bytesToDestination, bytesFromDestination int64, dur time.Duration) {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		host, port = hostport, ""
	}
	attrs := make([]any, 0, 14)
	if s.Namespace != "" {
		attrs = append(attrs, "namespace", s.Namespace)
	}
	attrs = append(attrs,
		"event", auditEventConnect,
		"host", truncateHost(host),
		"port", port,
		"bytesToDestination", bytesToDestination,
		"bytesFromDestination", bytesFromDestination,
		"durationSeconds", dur.Seconds(),
	)
	s.logger().Info(auditMsg, attrs...)
}
