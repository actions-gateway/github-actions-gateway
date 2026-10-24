package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Failure attribution (Q1184): a CONNECT refused or failed on a consumer
// namespace's own listener is kept in memory, so that namespace's AGC can tell
// which of its jobs hit it. The posture is docs/design/05-security.md § Proxy
// failure attribution; nothing here is written to disk or the log.

// failureOutcome is why a CONNECT on an attributed listener did not tunnel.
type failureOutcome string

const (
	// outcomeDenied is a destination refused by the allowlist.
	outcomeDenied failureOutcome = "denied"
	// outcomeDialFailed is an allowed destination the upstream dial failed on.
	outcomeDialFailed failureOutcome = "dial-failed"
)

const (
	// failureWindow is how long a record is kept. Long enough for an AGC polling
	// on a short interval to miss a poll, short enough that the buffer never
	// becomes a history of where a namespace's jobs went.
	failureWindow = 5 * time.Minute
	// maxFailuresPerNamespace caps one namespace's partition. Partitioning is
	// what keeps one tenant's workers from evicting another tenant's records.
	maxFailuresPerNamespace = 1024
	// maxFailuresPerSource caps one source address within a partition, so one
	// job producing denials at will cannot evict its neighbours' records.
	maxFailuresPerSource = 64
)

// failureRecord is one attributed failure. Every field is either read off the
// accepted connection or capped tenant text; the dial error itself is reduced
// to a class because its text embeds the address.
type failureRecord struct {
	Seq       uint64         `json:"seq"`
	Time      time.Time      `json:"time"`
	SourceIP  string         `json:"sourceIP"`
	Outcome   failureOutcome `json:"outcome"`
	DialClass string         `json:"dialClass,omitempty"`
	Host      string         `json:"host"`
	Port      string         `json:"port"`
}

// failureStore holds recent failures partitioned by consumer namespace.
type failureStore struct {
	mu sync.Mutex
	// epoch is drawn once per process, so a reader whose cursor came from a
	// previous process can tell the restart from a quiet period.
	epoch     string
	seq       uint64
	window    time.Duration
	perNS     int
	perSource int
	now       func() time.Time
	parts     map[string][]failureRecord
}

func newFailureStore() (*failureStore, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("draw attribution epoch: %w", err)
	}
	return &failureStore{
		epoch:     hex.EncodeToString(b),
		window:    failureWindow,
		perNS:     maxFailuresPerNamespace,
		perSource: maxFailuresPerSource,
		now:       time.Now,
		parts:     map[string][]failureRecord{},
	}, nil
}

// expire drops the records older than the window. Records are appended in
// time order, so the expired ones are a prefix.
func (f *failureStore) expire(p []failureRecord, now time.Time) []failureRecord {
	cutoff := now.Add(-f.window)
	i := 0
	for i < len(p) && p[i].Time.Before(cutoff) {
		i++
	}
	return append(p[:0], p[i:]...)
}

// record stamps rec with the time and the next sequence number and adds it to
// ns's partition, evicting that source's oldest record at the per-source cap
// and the partition's oldest at the per-namespace cap.
func (f *failureStore) record(ns string, rec failureRecord) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.now()
	f.seq++
	rec.Seq, rec.Time = f.seq, now

	p := f.expire(f.parts[ns], now)
	count, oldest := 0, -1
	for i := range p {
		if p[i].SourceIP == rec.SourceIP {
			if oldest < 0 {
				oldest = i
			}
			count++
		}
	}
	if count >= f.perSource {
		p = append(p[:oldest], p[oldest+1:]...)
	}
	if len(p) >= f.perNS {
		p = append(p[:0], p[1:]...)
	}
	f.parts[ns] = append(p, rec)
}

// read returns the epoch and ns's unexpired records after the sequence number.
func (f *failureStore) read(ns string, after uint64) (string, []failureRecord) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.expire(f.parts[ns], f.now())
	f.parts[ns] = p
	out := []failureRecord{}
	for _, r := range p {
		if r.Seq > after {
			out = append(out, r)
		}
	}
	return f.epoch, out
}

// classifyDialError reduces an upstream dial error to a class a retry decision
// can use. A DNS failure is checked first, since a resolver timeout is a DNS
// failure rather than a connect timeout.
func classifyDialError(err error) string {
	var dnsErr *net.DNSError
	var netErr net.Error
	switch {
	case errors.As(err, &dnsErr):
		return "dns"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "refused"
	case errors.As(err, &netErr) && netErr.Timeout():
		return "timeout"
	default:
		return "other"
	}
}

// namespaceKey carries an attributed listener's namespace on a request context.
type namespaceKey struct{}

// connContext tags a connection accepted on an attributed listener with that
// listener's namespace, keyed on the connection's local port. Nothing on the
// request can set it, so a worker cannot claim another namespace.
func (s *Server) connContext(ctx context.Context, c net.Conn) context.Context {
	if len(s.portNamespace) == 0 {
		return ctx
	}
	addr, ok := c.LocalAddr().(*net.TCPAddr)
	if !ok {
		return ctx
	}
	if ns, ok := s.portNamespace[addr.Port]; ok {
		return context.WithValue(ctx, namespaceKey{}, ns)
	}
	return ctx
}

// recordFailure keeps a refused or failed CONNECT when it arrived on an
// attributed listener. dialErr is nil for a denial.
func (s *Server) recordFailure(r *http.Request, outcome failureOutcome, dialErr error) {
	ns, ok := r.Context().Value(namespaceKey{}).(string)
	if !ok || s.failures == nil {
		return
	}
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil {
		host, port = r.Host, ""
	}
	sourceIP, _ := splitSourceAddr(r.RemoteAddr)
	rec := failureRecord{
		SourceIP: sourceIP,
		Outcome:  outcome,
		Host:     truncateHost(host),
		Port:     auditPort(port),
	}
	if dialErr != nil {
		rec.DialClass = classifyDialError(dialErr)
	}
	s.failures.record(ns, rec)
}

// failuresResponse is the body of GET /v1/failures.
type failuresResponse struct {
	Epoch   string          `json:"epoch"`
	Records []failureRecord `json:"records"`
}

// handleFailures serves the records of the namespace the verified client
// certificate names. The TLS handshake has already required a certificate from
// the attribution CA; the name must also be a namespace in the current port
// map, so removing a namespace from the map ends its access.
func (s *Server) handleFailures(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "only GET is supported", http.StatusMethodNotAllowed)
		return
	}
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.VerifiedChains[0]) == 0 {
		http.Error(w, "client certificate required", http.StatusForbidden)
		return
	}
	ns := r.TLS.VerifiedChains[0][0].Subject.CommonName
	if _, ok := s.AttributedAddrs[ns]; !ok {
		http.Error(w, "namespace not attributed", http.StatusForbidden)
		return
	}
	var after uint64
	if v := r.URL.Query().Get("after"); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			http.Error(w, "after must be a sequence number", http.StatusBadRequest)
			return
		}
		after = n
	}
	epoch, recs := s.failures.read(ns, after)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(failuresResponse{Epoch: epoch, Records: recs})
}

// dnsLabel is a Kubernetes namespace name (RFC 1123 label).
var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// parseAttributionPorts parses PROXY_ATTRIBUTION_PORTS, a comma-separated
// namespace=port list, into namespace → listen address. It fails closed on any
// malformed or duplicate entry, since the GMC builds the value and a bad one is
// a configuration bug.
func parseAttributionPorts(s string) (map[string]string, error) {
	items := splitList(s)
	if len(items) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(items))
	ports := map[int]bool{}
	for _, item := range items {
		ns, portStr, ok := strings.Cut(item, "=")
		ns, portStr = strings.TrimSpace(ns), strings.TrimSpace(portStr)
		if !ok || !dnsLabel.MatchString(ns) {
			return nil, fmt.Errorf("invalid entry %q: want namespace=port", item)
		}
		port, err := strconv.Atoi(portStr)
		if err != nil || port < 1 || port > 65535 {
			return nil, fmt.Errorf("invalid port in %q", item)
		}
		if _, dup := out[ns]; dup {
			return nil, fmt.Errorf("namespace %q listed twice", ns)
		}
		if ports[port] {
			return nil, fmt.Errorf("port %d listed twice", port)
		}
		ports[port] = true
		out[ns] = ":" + strconv.Itoa(port)
	}
	return out, nil
}
