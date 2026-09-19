package utils

import (
	"strings"
	"testing"
)

// The three log lines below are copied from the 2026-09-16 kindnet e2e run
// (job 104998010114), the occurrence Q1119 was filed on. Keeping the real
// encoding matters: the dial error is a nested field inside slog's JSON record,
// so a scorer that scanned for a bare substring would read the whole record.
const (
	proxyStartLine = `{"time":"2026-09-16T22:22:27.120546159Z","level":"INFO","msg":"proxy starting","proxyPort":"8080","healthPort":"8081","metricsPort":"8443","tls":true}`
	proxyDialFail  = `{"time":"2026-09-16T22:22:51.45581933Z","level":"ERROR","msg":"upstream dial failed","host":"api.github.com:443","error":"dial tcp 172.182.252.137:443: i/o timeout"}`
	proxyDialRefus = `{"time":"2026-09-16T22:22:53.10000000Z","level":"ERROR","msg":"upstream dial failed","host":"api.github.com:443","error":"dial tcp 172.182.252.137:443: connect: connection refused"}`
	proxyDenied    = `{"time":"2026-09-16T22:22:55.00000000Z","level":"WARN","msg":"CONNECT destination not allowed","host":"evil.example:443"}`
)

func logOf(lines ...string) string { return strings.Join(lines, "\n") + "\n" }

// TestAttributeProxyConnectReachesEveryVerdict drives all four arms. The suite
// exists because the banner's value is that it can come back with the opposite
// answer: a classifier that returned UPSTREAM-DIAL-FAILED for everything would
// have read the 2026-09-16 failure exactly as correctly as this one does, and
// nothing in a green CI run would have said otherwise.
func TestAttributeProxyConnectReachesEveryVerdict(t *testing.T) {
	cases := []struct {
		name     string
		evidence []ProxyReplicaEvidence
		want     ProxyConnectAttribution
	}{
		{
			// No replica log fetched. Distinct from silence: both produce empty
			// text and only one of them attributes anything.
			name:     "no readable replica",
			evidence: []ProxyReplicaEvidence{ScoreProxyReplicaLog("pod/a", "", false)},
			want:     ProxyConnectNoEvidence,
		},
		{
			// Readable and carrying no CONNECT-path line. The proxy did not
			// emit the client's 502, so the refusing hop is before it.
			name:     "readable and silent",
			evidence: []ProxyReplicaEvidence{ScoreProxyReplicaLog("pod/a", logOf(proxyStartLine), true)},
			want:     ProxyConnectSilent,
		},
		{
			name:     "destination denied",
			evidence: []ProxyReplicaEvidence{ScoreProxyReplicaLog("pod/a", logOf(proxyStartLine, proxyDenied), true)},
			want:     ProxyConnectRefused,
		},
		{
			name:     "upstream dial failed",
			evidence: []ProxyReplicaEvidence{ScoreProxyReplicaLog("pod/a", logOf(proxyStartLine, proxyDialFail), true)},
			want:     ProxyConnectUpstreamDialFailed,
		},
		{
			// A dial failure and a denial are different requests; the dial
			// failure is the one that answers 502, which is the signature the
			// spec failed on.
			name: "dial failure outranks a denial",
			evidence: []ProxyReplicaEvidence{
				ScoreProxyReplicaLog("pod/a", logOf(proxyDenied, proxyDialFail), true),
			},
			want: ProxyConnectUpstreamDialFailed,
		},
		{
			// The 2026-09-16 shape: one replica unreadable, the other carrying
			// the evidence. An unreadable replica must not veto a readable one.
			name: "one unreadable replica beside one with evidence",
			evidence: []ProxyReplicaEvidence{
				ScoreProxyReplicaLog("pod/a", "", false),
				ScoreProxyReplicaLog("pod/b", logOf(proxyDialFail), true),
			},
			want: ProxyConnectUpstreamDialFailed,
		},
		{
			name:     "no replicas at all",
			evidence: nil,
			want:     ProxyConnectNoEvidence,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := AttributeProxyConnect(tc.evidence); got != tc.want {
				t.Fatalf("AttributeProxyConnect = %v (%s), want %v (%s)", got, got, tc.want, tc.want)
			}
		})
	}
}

// TestScoreProxyReplicaLogCountsAndDistinguishesDialErrors pins the two facts a
// reader triages on: how many CONNECTs failed, and whether they failed the same
// way. Eight identical timeouts are one fact repeated; a timeout beside a
// refusal is two, and they point at different hops.
func TestScoreProxyReplicaLogCountsAndDistinguishesDialErrors(t *testing.T) {
	ev := ScoreProxyReplicaLog("pod/a", logOf(
		proxyStartLine,
		proxyDialFail, proxyDialFail, proxyDialFail,
		proxyDialRefus,
		proxyDenied,
	), true)

	if ev.DialFailures != 4 {
		t.Errorf("DialFailures = %d, want 4", ev.DialFailures)
	}
	if ev.Denials != 1 {
		t.Errorf("Denials = %d, want 1", ev.Denials)
	}
	want := []string{
		"dial tcp 172.182.252.137:443: connect: connection refused",
		"dial tcp 172.182.252.137:443: i/o timeout",
	}
	if len(ev.DialErrors) != len(want) {
		t.Fatalf("DialErrors = %q, want %q", ev.DialErrors, want)
	}
	for i := range want {
		if ev.DialErrors[i] != want[i] {
			t.Errorf("DialErrors[%d] = %q, want %q", i, ev.DialErrors[i], want[i])
		}
	}
}

// TestScoreProxyReplicaLogIgnoresCountsWhenUnreadable asserts an unreadable
// replica contributes nothing rather than a zeroed vote, so a dump whose fetch
// failed cannot be read as a proxy that handled nothing.
func TestScoreProxyReplicaLogIgnoresCountsWhenUnreadable(t *testing.T) {
	ev := ScoreProxyReplicaLog("pod/a", logOf(proxyDialFail), false)
	if ev.Readable {
		t.Fatal("Readable = true for an unreadable fetch")
	}
	if ev.DialFailures != 0 || ev.Denials != 0 || ev.DialErrors != nil {
		t.Fatalf("unreadable replica carries evidence: %+v", ev)
	}
}

// TestProxyConnectGuidanceRefusesTheHTTPLayerExplanation pins the sentence that
// separates this banner from the runner-host GitHub preflight (Q352). That
// probe scored the 2026-09-16 v1 failure BLOCKED on a 403 rate limit and told
// the reader to re-run — but a 403 arrives through a completed TCP connection,
// which is the hop this failure never reached, and the v2 failure minutes later
// scored REACHABLE on the identical signature.
func TestProxyConnectGuidanceRefusesTheHTTPLayerExplanation(t *testing.T) {
	g := ProxyConnectGuidance(ProxyConnectUpstreamDialFailed)
	for _, want := range []string{"An HTTP-layer verdict cannot explain this", "completed TCP connection"} {
		if !strings.Contains(g, want) {
			t.Errorf("UPSTREAM-DIAL-FAILED guidance does not say %q:\n%s", want, g)
		}
	}
	// The complement: PROXY-NOT-REACHED must send the reader to the hops before
	// the proxy. Asserted by what it names rather than by the absence of the
	// word "upstream", which the sentence "not at the upstream" contains.
	silent := ProxyConnectGuidance(ProxyConnectSilent)
	for _, want := range []string{"workload NetworkPolicy", "Service endpoints", "TLS handshake"} {
		if !strings.Contains(silent, want) {
			t.Errorf("PROXY-NOT-REACHED guidance does not name %q:\n%s", want, silent)
		}
	}
}

// TestFormatProxyConnectEvidenceNamesUnreadableReplicas asserts the banner's
// replica count matches the Deployment's. Dropping an unreadable replica would
// render a two-replica proxy as one and hide that half the evidence is missing.
func TestFormatProxyConnectEvidenceNamesUnreadableReplicas(t *testing.T) {
	out := FormatProxyConnectEvidence([]ProxyReplicaEvidence{
		ScoreProxyReplicaLog("pod/a", "", false),
		ScoreProxyReplicaLog("pod/b", logOf(proxyDialFail), true),
	})
	if !strings.Contains(out, "pod/a: log unreadable") {
		t.Errorf("unreadable replica not named:\n%s", out)
	}
	if !strings.Contains(out, "pod/b: 0 destination-denied, 1 upstream-dial-failed") {
		t.Errorf("readable replica's counts not rendered:\n%s", out)
	}
	if !strings.Contains(out, "i/o timeout") {
		t.Errorf("dial error not rendered:\n%s", out)
	}
}

// TestProxyConnectAttributionTokensAreDistinct guards the banner's own
// vocabulary: two verdicts rendering the same token would make the CI output
// unreadable in exactly the case the verdicts exist to separate.
func TestProxyConnectAttributionTokensAreDistinct(t *testing.T) {
	seen := map[string]ProxyConnectAttribution{}
	for _, a := range []ProxyConnectAttribution{
		ProxyConnectNoEvidence, ProxyConnectSilent, ProxyConnectRefused, ProxyConnectUpstreamDialFailed,
	} {
		if prev, dup := seen[a.String()]; dup {
			t.Errorf("verdicts %v and %v both render %q", prev, a, a.String())
		}
		seen[a.String()] = a
	}
}

// TestFormatProxyConnectEvidenceNamesARestart pins the restart annotation. A
// restarted replica keeps its CONNECT record in the previous container, so
// without it a replica that DID fail a dial reads as readable-and-silent, and
// PROXY-NOT-REACHED is a positive verdict pointing at the wrong hop.
func TestFormatProxyConnectEvidenceNamesARestart(t *testing.T) {
	ev := ScoreProxyReplicaLog("pod/a", logOf(proxyDialFail), true)
	ev.Restarted = true
	out := FormatProxyConnectEvidence([]ProxyReplicaEvidence{ev})
	if !strings.Contains(out, "restarted") {
		t.Errorf("a restarted replica is not named as one:\n%s", out)
	}

	quiet := FormatProxyConnectEvidence([]ProxyReplicaEvidence{
		ScoreProxyReplicaLog("pod/b", logOf(proxyDialFail), true),
	})
	if strings.Contains(quiet, "restarted") {
		t.Errorf("a replica that did not restart is annotated as one:\n%s", quiet)
	}
}
