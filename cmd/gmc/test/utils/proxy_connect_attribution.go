package utils

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// ProxyConnectAttribution is what an EgressProxy's own logs say about the
// CONNECT requests it handled, for a `real-github-egress` spec that failed with
// `curl: (56) CONNECT tunnel failed, response 502`.
//
// The proxy emits exactly two CONNECT-path log lines and each names a distinct
// hop (cmd/proxy/proxy.go handleConnect): a destination the allowlist rejects
// logs "CONNECT destination not allowed" and answers 403, and a net.DialTimeout
// that fails logs "upstream dial failed" and answers 502. A CONNECT it
// establishes logs nothing, which is why silence is its own verdict rather than
// a success.
type ProxyConnectAttribution int

const (
	// ProxyConnectNoEvidence — no replica's log could be read, so the logs say
	// nothing in either direction. Not the same as silence.
	ProxyConnectNoEvidence ProxyConnectAttribution = iota

	// ProxyConnectSilent — every replica's log was read and none of them
	// refused a destination or failed a dial. The proxy did not emit the 502
	// the client reported, so the client did not reach the replicas dumped
	// here: the workload NetworkPolicy, the Service, or the CONNECT TLS
	// handshake is where to look, not the upstream.
	ProxyConnectSilent

	// ProxyConnectRefused — a replica rejected the destination against its
	// allowlist. That hop answers 403, so this verdict under a 502 failure
	// means the two are not the same request.
	ProxyConnectRefused

	// ProxyConnectUpstreamDialFailed — a replica accepted the destination and
	// its TCP dial to the upstream did not complete. The proxy is not the
	// refusing hop; the dial error names what happened past it.
	ProxyConnectUpstreamDialFailed
)

// String renders the verdict as the token stamped into the CI failure banner.
func (a ProxyConnectAttribution) String() string {
	switch a {
	case ProxyConnectNoEvidence:
		return "NO-EVIDENCE"
	case ProxyConnectSilent:
		return "PROXY-NOT-REACHED"
	case ProxyConnectRefused:
		return "PROXY-REFUSED"
	case ProxyConnectUpstreamDialFailed:
		return "UPSTREAM-DIAL-FAILED"
	default:
		return "UNKNOWN"
	}
}

// ProxyReplicaEvidence is one EgressProxy replica's CONNECT-path log evidence.
// Readable false means the log could not be fetched at all — every count is
// then meaningless and the replica contributes nothing to the attribution.
type ProxyReplicaEvidence struct {
	Replica      string
	Readable     bool
	Denials      int
	DialFailures int
	// DialErrors holds each distinct dial error string, sorted. Distinctness
	// is the point: eight identical "i/o timeout" lines are one fact repeated,
	// while an "i/o timeout" beside a "connection refused" is two.
	DialErrors []string
	// Restarted reports that a previous container's log was found and folded
	// into the text scored here. It is rendered because a restart changes what
	// silence means: without the earlier log a replica that did fail a dial
	// reads as readable-and-silent.
	Restarted bool
}

const (
	proxyDenyMarker     = "CONNECT destination not allowed"
	proxyDialFailMarker = "upstream dial failed"
)

// proxyDialErrorRe lifts the error field out of the proxy's JSON dial-failure
// line. The proxy logs through slog's JSON handler, so the field is quoted and
// escape sequences are the encoder's; matching the field directly keeps this
// independent of the rest of the record's shape.
var proxyDialErrorRe = regexp.MustCompile(`"error":"((?:[^"\\]|\\.)*)"`)

// ScoreProxyReplicaLog reads one replica's log text into evidence. readable
// reports whether the fetch succeeded: a failed fetch and an empty log are
// different findings, and passing an empty string for both would merge them.
func ScoreProxyReplicaLog(replica, logText string, readable bool) ProxyReplicaEvidence {
	ev := ProxyReplicaEvidence{Replica: replica, Readable: readable}
	if !readable {
		return ev
	}

	seen := map[string]bool{}
	for _, line := range strings.Split(logText, "\n") {
		switch {
		case strings.Contains(line, proxyDenyMarker):
			ev.Denials++
		case strings.Contains(line, proxyDialFailMarker):
			ev.DialFailures++
			if m := proxyDialErrorRe.FindStringSubmatch(line); m != nil {
				seen[m[1]] = true
			}
		}
	}
	for e := range seen {
		ev.DialErrors = append(ev.DialErrors, e)
	}
	sort.Strings(ev.DialErrors)
	return ev
}

// AttributeProxyConnect folds every replica's evidence into one verdict.
//
// A dial failure outranks a denial when both appear: they are different
// requests, and the dial failure is the one that produced the 502 the spec
// failed on. No readable replica at all is NO-EVIDENCE rather than silence —
// the distinction is the whole point of the verdict, because an unreadable dump
// and a proxy nothing reached produce the same empty text.
func AttributeProxyConnect(evidence []ProxyReplicaEvidence) ProxyConnectAttribution {
	anyReadable := false
	denials, dialFailures := 0, 0
	for _, ev := range evidence {
		if !ev.Readable {
			continue
		}
		anyReadable = true
		denials += ev.Denials
		dialFailures += ev.DialFailures
	}
	switch {
	case !anyReadable:
		return ProxyConnectNoEvidence
	case dialFailures > 0:
		return ProxyConnectUpstreamDialFailed
	case denials > 0:
		return ProxyConnectRefused
	default:
		return ProxyConnectSilent
	}
}

// ProxyConnectGuidance is the triage instruction stamped under each verdict.
//
// UPSTREAM-DIAL-FAILED deliberately says what the runner-host GitHub preflight
// (Q352) cannot settle. That probe scores an HTTP answer, so a 403 rate limit
// scores BLOCKED — yet a 403 arrives through a completed TCP connection and a
// finished TLS handshake, which is the hop this failure never got past. The two
// banners answer different questions and the dial-level one is the one that
// matches this signature.
func ProxyConnectGuidance(a ProxyConnectAttribution) string {
	switch a {
	case ProxyConnectNoEvidence:
		return "No proxy replica log could be read, so the proxy's own account of these CONNECTs is\n" +
			"missing. This attributes nothing: read the dump above for why the fetch failed before\n" +
			"concluding anything about the egress path.\n"
	case ProxyConnectSilent:
		return "Every replica log was read and none refused a destination or failed a dial, so these\n" +
			"replicas did not emit the 502 the client reported. Look at the hops before the proxy —\n" +
			"the workload NetworkPolicy, the Service endpoints, and the CONNECT TLS handshake — not\n" +
			"at the upstream.\n"
	case ProxyConnectRefused:
		return "The proxy rejected the destination against its allowlist. That hop answers 403, not 502,\n" +
			"so a 502 failure alongside this verdict is a different request: check whether the\n" +
			"destination resolved to an address outside the egress allowlist between the two.\n"
	case ProxyConnectUpstreamDialFailed:
		return "The proxy accepted the destination and its TCP dial to the upstream did not complete, so\n" +
			"the proxy is not the refusing hop and the dial error above names what happened past it.\n" +
			"An HTTP-layer verdict cannot explain this: a 403 or 429 from GitHub arrives through a\n" +
			"completed TCP connection, which is the hop this failure never reached. Separate a local\n" +
			"drop from an upstream one by whether anything outside the proxy's egress NetworkPolicy\n" +
			"reached the same address in the same window.\n"
	default:
		return "Unrecognized attribution; read the replica evidence above directly.\n"
	}
}

// FormatProxyConnectEvidence renders the per-replica evidence as one line each,
// for the CI failure banner. An unreadable replica is named as unreadable
// rather than dropped, so the banner's replica count matches the Deployment's.
func FormatProxyConnectEvidence(evidence []ProxyReplicaEvidence) string {
	if len(evidence) == 0 {
		return "  (no proxy replicas found)\n"
	}
	var b strings.Builder
	for _, ev := range evidence {
		if !ev.Readable {
			fmt.Fprintf(&b, "  %s: log unreadable\n", ev.Replica)
			continue
		}
		fmt.Fprintf(&b, "  %s: %d destination-denied, %d upstream-dial-failed", ev.Replica, ev.Denials, ev.DialFailures)
		if len(ev.DialErrors) > 0 {
			fmt.Fprintf(&b, " %v", ev.DialErrors)
		}
		if ev.Restarted {
			b.WriteString(" (restarted; previous container's log folded in)")
		}
		b.WriteString("\n")
	}
	return b.String()
}
