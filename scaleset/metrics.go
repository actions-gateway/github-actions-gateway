package scaleset

// MetricsRecorder records scale-set client statistics. The AGC listener tier
// (Q264 P3) wires this to a Prometheus CounterVec; unit tests and the probe use a
// stub. A nil recorder is safe — the client skips every metrics call — so the
// zero-value Client works without one.
//
// Known IncPollError reason labels:
//
//	"rate_limited" — 429 Too Many Requests on the queue
//	"unauthorized" — 401/403 on the queue (token needs refresh)
//	"server_error" — 5xx responses
//	"timeout"      — context deadline / connection timeout
//
// Known IncTokenRefresh kind labels:
//
//	"admin" — the admin JWT was re-minted (lazy pre-expiry refresh)
//	"queue" — the message-queue access token was refreshed via session PATCH
type MetricsRecorder interface {
	IncPollError(reason string)
	IncTokenRefresh(kind string)
}
