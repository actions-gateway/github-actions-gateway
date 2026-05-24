package broker

// PollMetricsRecorder records GetMessage polling error statistics.
// The AGC (Milestone 2) wires this to a Prometheus CounterVec; the probe and
// unit tests use a stub implementation.
//
// Known reason labels:
//
//	"rate_limited" — 429 Too Many Requests
//	"server_error" — 5xx responses
//	"timeout"      — context deadline / connection timeout
type PollMetricsRecorder interface {
	IncPollError(reason string)
}
