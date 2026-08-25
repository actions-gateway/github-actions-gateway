// Package main implements a minimal stateless HTTPS CONNECT proxy.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Server is a minimal stateless HTTPS CONNECT proxy.
// It handles only CONNECT tunneling — no TLS termination, no inspection.
type Server struct {
	// Addr is the listen address for CONNECT requests. Default ":8080".
	Addr string
	// HealthAddr is the listen address for /healthz and /readyz (plaintext,
	// kubelet probes). Default ":8081".
	HealthAddr string
	// MetricsAddr is the listen address for the mTLS /metrics endpoint. When the
	// metrics mTLS files below are all set, /metrics is served here over HTTPS
	// requiring a CA-signed client cert; otherwise /metrics is served plaintext
	// on HealthAddr (dev/test fallback).
	MetricsAddr string
	// MetricsTLSCertFile / MetricsTLSKeyFile / MetricsClientCAFile configure the
	// mTLS metrics listener. All three must be set to enable it: the first two
	// are the server cert+key, the third is the CA that scraper client certs are
	// verified against (Q69).
	MetricsTLSCertFile  string
	MetricsTLSKeyFile   string
	MetricsClientCAFile string
	// DialTimeout is the upstream TCP dial timeout. Default 10s.
	DialTimeout time.Duration
	// ReadHeaderTimeout caps how long the server waits for request headers on
	// both the CONNECT and health listeners. Default 5s. Mitigates slowloris.
	ReadHeaderTimeout time.Duration
	// HTTPIdleTimeout caps idle keep-alive on both HTTP listeners. Default 60s.
	// Distinct from TunnelIdleTimeout, which applies to the hijacked CONNECT relay.
	HTTPIdleTimeout time.Duration
	// MaxTunnelLifetime is the hard upper bound on a single CONNECT tunnel.
	// Default 6h. A stalled long-poll cannot tie up a relay goroutine beyond this.
	MaxTunnelLifetime time.Duration
	// TunnelIdleTimeout is the per-direction idle deadline applied to the
	// hijacked CONNECT relay. Reset on every successful read. Default 5m.
	TunnelIdleTimeout time.Duration
	// ShutdownDrainTimeout bounds the WHOLE graceful drain on context
	// cancellation (SIGTERM): the endpoint-removal linger, stopping the listener,
	// and waiting for in-flight CONNECT tunnels to finish. Default 45s, sized to
	// fit inside the pod's 60s terminationGracePeriodSeconds with headroom.
	// Tunnels still open when it expires are force-closed so the process can exit
	// before SIGKILL.
	ShutdownDrainTimeout time.Duration
	// ShutdownLinger caps how long shutdown keeps the CONNECT listener OPEN
	// after SIGTERM so connections steered here by a kube-proxy that has not yet
	// observed our endpoint removal are served rather than refused. Default 10s;
	// negative disables the linger entirely. It is a CEILING, not a fixed wait —
	// the linger exits as soon as arrivals go quiet (see lingerForEndpointRemoval)
	// — and it is spent INSIDE ShutdownDrainTimeout, not added to it.
	ShutdownLinger time.Duration
	Log            *slog.Logger
	// TLSCertFile and TLSKeyFile enable TLS on the CONNECT listener when both are set.
	// The health port always remains plaintext.
	TLSCertFile string
	TLSKeyFile  string

	// AllowedHostSuffixes and AllowedCIDRs are the CONNECT destination allowlist
	// (Q242 G.1). They hold the FULL permitted set the GMC injects — the GitHub
	// hosts/ranges PLUS any operator-allowlisted destinationFQDNs/destinationCIDRs.
	// When BOTH are empty the proxy is transport-only: any CONNECT target is
	// tunneled and the proxy pod's NetworkPolicy is the sole destination gate (the
	// historical behavior). When either is set, a CONNECT target must match an
	// allowed host suffix or resolve into an allowed CIDR, else it is refused 403.
	AllowedHostSuffixes []string
	AllowedCIDRs        []*net.IPNet

	// AuditLogging selects the per-connection egress record this proxy writes
	// (Q564, design appendix G.3). AuditOff — the default, and what an unset or
	// unrecognized PROXY_AUDIT_LOGGING resolves to — writes nothing per
	// connection. See AuditMode for why it is opt-in.
	AuditLogging AuditMode
	// Namespace is the tenant namespace this pool runs in, read from the
	// downward API and stamped on the audit record so the record attributes
	// itself without the log collector's pod metadata. Empty omits the field.
	Namespace string
	// dnsResolver resolves CONNECT hostnames for the CIDR allowlist check; nil uses
	// net.DefaultResolver. Injected in tests.
	dnsResolver ipResolver
	// quiescence overrides connQuiescence, the no-new-arrivals interval the
	// shutdown linger waits out. Zero uses the constant. Injected in tests so
	// they exercise the real timing logic without paying seconds per case.
	quiescence time.Duration

	connectionsActive *prometheus.GaugeVec
	connectionsTotal  *prometheus.CounterVec
	dialErrors        *prometheus.CounterVec
	tunnelDuration    *prometheus.HistogramVec
	connectDenied     *prometheus.CounterVec

	// metricsGatherer is the registry the /metrics endpoint serves — the same
	// one the proxy metrics above are registered on, so a custom registry (tests,
	// or multiple in-process servers) is served consistently instead of always
	// falling back to the global default registry.
	metricsGatherer prometheus.Gatherer

	// ready is closed by ListenAndServe once the CONNECT listener has bound.
	// /readyz returns 200 only after this channel is closed, so the kubelet
	// keeps the pod out of the Service EndpointSlice until workers can
	// actually reach the CONNECT port. Mirrors the §11.D GMC webhook
	// readiness fix.
	ready chan struct{}

	// draining is closed when graceful shutdown begins. /readyz fails from that
	// moment so the endpoint controller stops steering new CONNECT traffic here
	// while already-established tunnels drain.
	draining chan struct{}

	// tunnels tracks in-flight hijacked CONNECT relays. http.Server.Shutdown
	// does not wait for hijacked connections, so shutdown waits on this
	// instead (Q384).
	tunnels tunnelTracker

	// lastConnNanos is the wall-clock time (UnixNano) the CONNECT listener last
	// accepted a NEW connection, stamped from the http.Server ConnState hook.
	// Shutdown reads it as its endpoint-convergence signal (Q386): new
	// connections are precisely what Service routing controls, so arrivals going
	// quiet is direct evidence that dataplanes have stopped steering here.
	// Requests multiplexed onto an already-accepted keep-alive connection do not
	// stamp it — that connection was routed before shutdown began.
	lastConnNanos atomic.Int64
}

// ipResolver is the subset of *net.Resolver the CONNECT CIDR allowlist check
// needs; an interface so tests can inject a deterministic resolver.
type ipResolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

const (
	defaultReadHeaderTimeout = 5 * time.Second
	defaultHTTPIdleTimeout   = 60 * time.Second
	defaultMaxTunnelLifetime = 6 * time.Hour
	defaultTunnelIdleTimeout = 5 * time.Minute
	// defaultShutdownDrainTimeout bounds the graceful drain END TO END — the
	// endpoint-removal linger AND the tunnel drain, which share this one budget
	// rather than each getting their own. The proxy Deployment sets
	// terminationGracePeriodSeconds: 60, so 45s leaves headroom for the
	// force-close and process exit before the kubelet sends SIGKILL.
	//
	// Mirrored as proxyDrainBudgetSeconds in the GMC's pod builder
	// (cmd/gmc/internal/controller/builder.go) — a separate Go module, so the two
	// cannot share a constant. Raising this without raising the grace period
	// there puts the drain back outside the budget.
	defaultShutdownDrainTimeout = 45 * time.Second
	// defaultShutdownLinger caps the endpoint-removal linger. Sized for a large
	// cluster's EndpointSlice propagation tail rather than its median, because it
	// is only a ceiling: the linger exits early on arrival quiescence, so a
	// typical termination pays connQuiescence, not this.
	defaultShutdownLinger = 10 * time.Second
	// connQuiescence is how long the CONNECT listener must go without accepting a
	// new connection before the linger concludes that routing has converged away
	// from this pod. Also the linger's effective FLOOR: an already-idle proxy has
	// no arrivals to wait out, so it still holds the listener open this long
	// rather than exiting instantly into the very race the linger exists to close.
	connQuiescence = 2 * time.Second
	// lingerPollInterval is how often the linger re-evaluates quiescence. Fine
	// enough that the common case rounds to connQuiescence, coarse enough to be
	// free.
	lingerPollInterval = 100 * time.Millisecond
	// tunnelCloseGrace is how long shutdown waits for force-closed relays to
	// unwind after the drain deadline expires. They are already unblocked by
	// the close, so this only guards against exiting mid-write.
	tunnelCloseGrace = 2 * time.Second
	// healthShutdownTimeout bounds the health/metrics listener shutdown, which
	// runs after the tunnel drain and serves only short probe/scrape requests.
	healthShutdownTimeout = 5 * time.Second
)

// NewServer returns a Server with metrics registered on reg.
func NewServer(addr, healthAddr string, dialTimeout time.Duration, log *slog.Logger, reg prometheus.Registerer) *Server {
	if reg == nil {
		reg = prometheus.DefaultRegisterer
	}
	active := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "actions_gateway_proxy_connections_active",
		Help: "Currently active CONNECT tunnels.",
	}, nil)
	total := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "actions_gateway_proxy_connections_total",
		Help: "Total CONNECT tunnels opened.",
	}, nil)
	dialErr := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "actions_gateway_proxy_dial_errors_total",
		Help: "Upstream dial failures.",
	}, nil)
	tunnelDur := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "actions_gateway_proxy_tunnel_duration_seconds",
		Help:    "Duration of CONNECT tunnels in seconds, observed at tunnel close.",
		Buckets: []float64{0.1, 0.5, 1, 5, 10, 60, 300, 1800, 3600, 21600},
	}, nil)
	denied := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "actions_gateway_proxy_connect_denied_total",
		Help: "CONNECT requests refused because the destination is not on the allowlist.",
	}, nil)
	reg.MustRegister(active, total, dialErr, tunnelDur, denied)
	// Emit actions_gateway_build_info{component="proxy",version=…} 1 on the same
	// registry so the running binary version is correlatable straight from
	// metrics during an incident (Q318).
	registerBuildInfo(reg, "proxy", version)

	// Serve from the same registry the metrics were registered on. A
	// *prometheus.Registry satisfies both Registerer and Gatherer; the default
	// registerer is also the default gatherer.
	gatherer, ok := reg.(prometheus.Gatherer)
	if !ok {
		gatherer = prometheus.DefaultGatherer
	}

	return &Server{
		Addr:              addr,
		HealthAddr:        healthAddr,
		DialTimeout:       dialTimeout,
		Log:               log,
		connectionsActive: active,
		connectionsTotal:  total,
		dialErrors:        dialErr,
		tunnelDuration:    tunnelDur,
		connectDenied:     denied,
		metricsGatherer:   gatherer,
		ready:             make(chan struct{}),
		draining:          make(chan struct{}),
	}
}

// ListenAndServe starts both the CONNECT listener and the health server.
// Blocks until ctx is cancelled.
//
// Both listeners are bound synchronously before either serve loop starts and
// before s.ready is closed. Binding the CONNECT socket puts it in LISTEN state
// at the kernel level, so workers can complete the TCP handshake the instant
// /readyz flips to 200 — no race window where the EndpointSlice points at a
// pod whose CONNECT port is still unbound.
func (s *Server) ListenAndServe(ctx context.Context) error {
	metricsEnabled := s.MetricsTLSCertFile != "" && s.MetricsTLSKeyFile != "" && s.MetricsClientCAFile != ""

	// Build the metrics mTLS config first so a bad cert fails fast, before any
	// listener is bound.
	var metricsTLS *tls.Config
	if metricsEnabled {
		var err error
		metricsTLS, err = s.metricsTLSConfig()
		if err != nil {
			return fmt.Errorf("configure metrics mTLS: %w", err)
		}
	}

	connectLn, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return fmt.Errorf("bind connect listener: %w", err)
	}
	s.Addr = connectLn.Addr().String()

	healthLn, err := net.Listen("tcp", s.HealthAddr)
	if err != nil {
		_ = connectLn.Close()
		return fmt.Errorf("bind health listener: %w", err)
	}
	s.HealthAddr = healthLn.Addr().String()

	var metricsLn net.Listener
	if metricsEnabled {
		metricsLn, err = net.Listen("tcp", s.MetricsAddr)
		if err != nil {
			_ = connectLn.Close()
			_ = healthLn.Close()
			return fmt.Errorf("bind metrics listener: %w", err)
		}
		s.MetricsAddr = metricsLn.Addr().String()
	}

	close(s.ready)

	readHeaderTimeout := s.ReadHeaderTimeout
	if readHeaderTimeout == 0 {
		readHeaderTimeout = defaultReadHeaderTimeout
	}
	httpIdleTimeout := s.HTTPIdleTimeout
	if httpIdleTimeout == 0 {
		httpIdleTimeout = defaultHTTPIdleTimeout
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/readyz", s.handleReadyz)
	if !metricsEnabled {
		// Dev/test fallback: serve metrics plaintext on the health port when no
		// mTLS bundle is configured. In production the GMC always mounts the
		// bundle, so metrics are served over mTLS on the dedicated listener below.
		mux.Handle("/metrics", promhttp.HandlerFor(s.metricsGatherer, promhttp.HandlerOpts{}))
	}
	healthSrv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       httpIdleTimeout,
	}

	proxySrv := &http.Server{
		Handler: http.HandlerFunc(s.handleConnect),
		// ReadHeaderTimeout caps the CONNECT request-line + headers read.
		// ReadTimeout is intentionally NOT set — the CONNECT body is hijacked
		// and a non-zero ReadTimeout would cap the post-handshake tunnel
		// lifetime to a fixed value. Per-tunnel deadlines live in handleConnect.
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       httpIdleTimeout,
		// CONNECT is HTTP/1.1-only. Without disabling HTTP/2, Go's http.Server
		// negotiates h2 via ALPN when TLS is configured; the AGC's HTTPS proxy
		// client then sends an HTTP/1.1 CONNECT line over what is now an HTTP/2
		// connection and the proxy responds with an HTTP/2 SETTINGS frame —
		// surfaced to the client as `malformed HTTP response`.
		//
		// MinVersion is pinned to TLS 1.2 to match the metrics listener
		// (metricsTLSConfig) rather than inheriting Go's default floor: the
		// worker→proxy CONNECT leg carries every tenant's GitHub-bound traffic,
		// so its TLS floor is a tenant-isolation boundary and must be explicit
		// and modern, not whatever the toolchain happens to default to.
		TLSConfig:    &tls.Config{MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}},
		TLSNextProto: map[string]func(*http.Server, *tls.Conn, http.Handler){},
		// Stamps the arrival of every NEW connection on the CONNECT listener.
		// This is shutdown's endpoint-convergence signal (Q386) — see
		// lingerForEndpointRemoval. StateNew only, deliberately: a request
		// arriving on an existing keep-alive connection says nothing about
		// whether routing still points here.
		ConnState: func(_ net.Conn, state http.ConnState) {
			if state == http.StateNew {
				s.lastConnNanos.Store(time.Now().UnixNano())
			}
		},
	}

	var metricsSrv *http.Server
	if metricsEnabled {
		metricsMux := http.NewServeMux()
		metricsMux.Handle("/metrics", promhttp.HandlerFor(s.metricsGatherer, promhttp.HandlerOpts{}))
		metricsSrv = &http.Server{
			Handler:           metricsMux,
			TLSConfig:         metricsTLS,
			ReadHeaderTimeout: readHeaderTimeout,
			IdleTimeout:       httpIdleTimeout,
			// HTTP/1.1 only — consistent with the CONNECT listener and avoids the
			// h2-over-ALPN surprise documented on proxySrv below.
			TLSNextProto: map[string]func(*http.Server, *tls.Conn, http.Handler){},
		}
	}

	errCh := make(chan error, 3)
	go func() { errCh <- healthSrv.Serve(healthLn) }()
	if s.TLSCertFile != "" && s.TLSKeyFile != "" {
		go func() { errCh <- proxySrv.ServeTLS(connectLn, s.TLSCertFile, s.TLSKeyFile) }()
	} else {
		go func() { errCh <- proxySrv.Serve(connectLn) }()
	}
	if metricsSrv != nil {
		// Cert+key live in metricsSrv.TLSConfig.Certificates, so ServeTLS gets "".
		go func() { errCh <- metricsSrv.ServeTLS(metricsLn, "", "") }()
	}

	select {
	case <-ctx.Done():
		s.shutdown(proxySrv, healthSrv, metricsSrv)
		return nil
	case err := <-errCh:
		return err
	}
}

// shutdown performs the graceful drain triggered by context cancellation
// (SIGTERM on a rollout, node drain, eviction, or scale-down).
//
// It runs on a context detached from the cancelled one and bounded by
// ShutdownDrainTimeout: teardown issued on the context that was just cancelled
// would fail instantly, and an unbounded drain would silently outgrow
// terminationGracePeriodSeconds and be SIGKILLed mid-write anyway.
//
// Order matters, and there are two distinct hazards to clear:
//
//	SIGTERM does not mean traffic has stopped arriving. Marking the pod
//	terminating, removing it from EndpointSlices, and each kube-proxy applying
//	that removal are independent control loops, so NEW connections can still be
//	steered here (Q386). /readyz fails immediately and the listener is then held
//	OPEN for a bounded linger, so those stragglers are served rather than refused.
//
//	Do NOT read the /readyz failure as what drives endpoint removal. Measured on
//	kind (Kubernetes 1.35, Q388): the 1s-period probe failed throughout a full
//	48s termination, but its result never reached the pod's Ready condition,
//	which stayed True the whole time. Endpoint removal on the ordinary delete
//	path is driven by the deletionTimestamp instead, and that happens whether or
//	not /readyz fails. (kubernetes#124648 is the related but distinct
//	eviction-path gap — there the probe worker is halted outright; the
//	delete-path behaviour above is the Q388 measurement, not that issue.)
//	Failing it stays worthwhile (it is what the upstream design intends, and it
//	is correct for any consumer that does watch readiness) but it earns no part
//	of the drain budget, and the linger cannot be justified by it.
//
//	http.Server.Shutdown does not wait for hijacked connections, and every
//	CONNECT tunnel is hijacked, so tunnels are tracked separately (Q384).
//
// The linger is spent INSIDE the drain budget rather than ahead of it: the two
// waits are for different things and overlap freely, since tunnels opened before
// SIGTERM keep finishing throughout the linger. Worst case is therefore
// max(linger, drain), not their sum — which is what keeps the whole sequence
// inside terminationGracePeriodSeconds, and what keeps a truncated shutdown
// window (spot preemption, graceful node shutdown) from spending its scarce
// budget idling instead of draining.
//
// The health listener is stopped last, so probes get a 503 for the whole
// sequence rather than a refused connection.
func (s *Server) shutdown(proxySrv, healthSrv, metricsSrv *http.Server) {
	log := s.logger()
	drainTimeout := s.ShutdownDrainTimeout
	if drainTimeout == 0 {
		drainTimeout = defaultShutdownDrainTimeout
	}

	close(s.draining)

	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	// Phase 1: listener stays OPEN. Bounded by the same ctx, so a long linger
	// can only ever eat into the drain budget, never extend past it.
	s.lingerForEndpointRemoval(ctx, log)

	// Phase 2: closes the listener and finishes non-hijacked requests. It
	// explicitly does NOT close or wait for hijacked connections, so every
	// CONNECT tunnel is still relaying when this returns — that is what the
	// tracker below is for. Once it has returned, no new tunnel can be
	// registered, which makes the drained() snapshot complete (Q384).
	_ = proxySrv.Shutdown(ctx)

	select {
	case <-s.tunnels.drained():
		log.Info("in-flight CONNECT tunnels drained")
	case <-ctx.Done():
		cut := s.tunnels.closeAll()
		// Loud and countable: this is CI egress being severed mid-request, and
		// it is the signal that the drain budget is too small for this tenant's
		// traffic (or that a tunnel is wedged).
		log.Warn("drain deadline expired; cutting in-flight CONNECT tunnels",
			"tunnels", cut, "drainTimeout", drainTimeout)
		select {
		case <-s.tunnels.drained():
		case <-time.After(tunnelCloseGrace):
			log.Warn("force-closed CONNECT tunnels did not unwind", "grace", tunnelCloseGrace)
		}
	}

	healthCtx, cancelHealth := context.WithTimeout(context.Background(), healthShutdownTimeout)
	defer cancelHealth()
	_ = healthSrv.Shutdown(healthCtx)
	if metricsSrv != nil {
		_ = metricsSrv.Shutdown(healthCtx)
	}
}

// lingerForEndpointRemoval holds the CONNECT listener open after SIGTERM until
// routing has plausibly converged away from this pod, then returns so the drain
// can close it (Q386).
//
// Kubernetes offers no signal that EndpointSlice removal has propagated — there
// is no API and no readback aggregated across the N kube-proxies applying it —
// so the canonical remedy is a fixed preStop sleep. This measures instead of
// sleeping: the proxy is the server, so the arrival of new connections is
// directly observable, and arrivals stopping is the property a preStop sleep can
// only approximate by waiting long enough to be sure.
//
// The rule is one line: wait until no new connection has arrived for
// connQuiescence, measuring from the later of (shutdown start, last arrival).
// Both bounds fall out of it rather than needing separate knobs —
//
//   - an idle proxy has no arrivals to wait out, so it waits connQuiescence
//     from the start and no longer: a floor, so it cannot exit instantly into
//     the very race this closes;
//   - a proxy still being handed traffic keeps extending, because each arrival
//     is fresh evidence that some dataplane has not converged yet.
//
// A quiet interval is evidence, not proof — bursty traffic can gap — so the wait
// is capped by ShutdownLinger and by ctx (the drain budget), whichever binds
// first. Overshooting the ceiling costs a slower rollout; undershooting refuses
// a connection, so the ceiling is sized for a large cluster's propagation tail
// while the quiescence exit keeps the typical case short.
func (s *Server) lingerForEndpointRemoval(ctx context.Context, log *slog.Logger) {
	ceiling := s.ShutdownLinger
	if ceiling == 0 {
		ceiling = defaultShutdownLinger
	}
	if ceiling < 0 {
		// Explicitly disabled — the operator has accepted the race, typically to
		// conserve a truncated node-shutdown window.
		return
	}

	quiescence := s.quiescence
	if quiescence == 0 {
		quiescence = connQuiescence
	}

	start := time.Now()
	deadline := start.Add(ceiling)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}

	ticker := time.NewTicker(lingerPollInterval)
	defer ticker.Stop()

	for {
		now := time.Now()

		// Measuring from max(start, lastArrival) is what makes this both a floor
		// and an extending wait, with no separate minimum to keep in sync.
		quietSince := start
		if nanos := s.lastConnNanos.Load(); nanos > 0 {
			if last := time.Unix(0, nanos); last.After(quietSince) {
				quietSince = last
			}
		}
		if quiet := now.Sub(quietSince); quiet >= quiescence {
			log.Info("CONNECT arrivals quiesced; endpoint removal has propagated",
				"lingered", now.Sub(start).Round(time.Millisecond), "quiet", quiet.Round(time.Millisecond))
			return
		}

		if !now.Before(deadline) {
			// Still being handed new connections at the ceiling. Closing the
			// listener now WILL refuse some of them; the alternative is spending
			// the tunnel drain's budget here and being SIGKILLed mid-relay
			// instead, which is strictly worse and silent.
			log.Warn("linger ceiling reached with CONNECT arrivals still landing; closing listener anyway",
				"lingered", now.Sub(start).Round(time.Millisecond), "ceiling", ceiling)
			return
		}

		select {
		case <-ticker.C:
		case <-ctx.Done():
			return
		}
	}
}

// logger returns s.Log, or slog.Default() when unset.
func (s *Server) logger() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// metricsTLSConfig builds the mTLS server config for the metrics listener:
// the server cert+key plus the CA that scraper client certificates are verified
// against. RequireAndVerifyClientCert means the TLS handshake itself rejects any
// client that does not present a CA-signed certificate.
func (s *Server) metricsTLSConfig() (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(s.MetricsTLSCertFile, s.MetricsTLSKeyFile)
	if err != nil {
		return nil, fmt.Errorf("load metrics server cert/key: %w", err)
	}
	caPEM, err := os.ReadFile(s.MetricsClientCAFile)
	if err != nil {
		return nil, fmt.Errorf("read metrics client CA %s: %w", s.MetricsClientCAFile, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("no certificates parsed from %s", s.MetricsClientCAFile)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		MinVersion:   tls.VersionTLS12,
		NextProtos:   []string{"http/1.1"},
	}, nil
}

// handleReadyz returns 200 only once the CONNECT listener has bound. The
// readiness probe gates a worker pod's HTTPS_PROXY traffic on the proxy
// kernel socket being in LISTEN state — without this, kubelet adds the
// pod to the Service EndpointSlice as soon as the health port is up,
// and concurrent worker traffic races the proxy serve goroutine.
func (s *Server) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	// Draining takes precedence over ready: once shutdown has begun this pod
	// must stop attracting new CONNECT traffic, even though its listener stays
	// up for a moment while in-flight tunnels finish.
	select {
	case <-s.draining:
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	default:
	}

	select {
	case <-s.ready:
		w.WriteHeader(http.StatusOK)
	default:
		w.WriteHeader(http.StatusServiceUnavailable)
	}
}

// checkDestination enforces the CONNECT allowlist (Q242 G.1). It returns the
// address to dial and whether the destination is permitted. With no allowlist
// configured it is a no-op (transport-only): any destination is permitted and
// the original host:port is returned. With an allowlist, a hostname matching an
// allowed suffix is permitted and dialed by name (the FQDN egress policy is the
// hard gate); otherwise, if it resolves into an allowed CIDR, the validated IP
// is pinned as the dial target (defeating a rebinding resolver between this
// check and the dial); a literal-IP target is permitted only if it falls in an
// allowed CIDR. Anything else is refused.
func (s *Server) checkDestination(ctx context.Context, hostport string) (dialAddr string, allowed bool) {
	if len(s.AllowedHostSuffixes) == 0 && len(s.AllowedCIDRs) == 0 {
		return hostport, true
	}
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return "", false
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))

	if ip := net.ParseIP(host); ip != nil {
		// Literal IP: only a CIDR rule can permit it — a host suffix can't match an IP.
		if ipInAny(ip, s.AllowedCIDRs) {
			return hostport, true
		}
		return "", false
	}

	if matchesHostSuffix(host, s.AllowedHostSuffixes) {
		return hostport, true
	}

	if len(s.AllowedCIDRs) > 0 {
		resolver := s.dnsResolver
		if resolver == nil {
			resolver = net.DefaultResolver
		}
		if addrs, err := resolver.LookupIPAddr(ctx, host); err == nil {
			for _, a := range addrs {
				if ipInAny(a.IP, s.AllowedCIDRs) {
					return net.JoinHostPort(a.IP.String(), port), true
				}
			}
		}
	}
	return "", false
}

// matchesHostSuffix reports whether host equals or is a subdomain of any suffix
// (so an allowed "golang.org" permits "proxy.golang.org").
func matchesHostSuffix(host string, suffixes []string) bool {
	for _, suf := range suffixes {
		suf = strings.ToLower(strings.Trim(suf, "."))
		if suf == "" {
			continue
		}
		if host == suf || strings.HasSuffix(host, "."+suf) {
			return true
		}
	}
	return false
}

// ipInAny reports whether ip falls within any of the CIDRs.
func ipInAny(ip net.IP, cidrs []*net.IPNet) bool {
	for _, c := range cidrs {
		if c != nil && c.Contains(ip) {
			return true
		}
	}
	return false
}

func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		http.Error(w, "only CONNECT is supported", http.StatusMethodNotAllowed)
		return
	}

	dialTimeout := s.DialTimeout
	if dialTimeout == 0 {
		dialTimeout = 10 * time.Second
	}

	dialAddr, allowed := s.checkDestination(r.Context(), r.Host)
	if !allowed {
		s.connectDenied.WithLabelValues().Inc()
		s.logger().Warn("CONNECT destination not allowed", "host", truncateHost(r.Host))
		http.Error(w, "destination not allowed", http.StatusForbidden)
		return
	}

	upstream, err := net.DialTimeout("tcp", dialAddr, dialTimeout)
	if err != nil {
		s.dialErrors.WithLabelValues().Inc()
		s.logger().Error("upstream dial failed", "host", r.Host, "error", err)
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
		return
	}
	// Register with the tracker BEFORE hijacking. Up to the hijack, net/http
	// counts this connection as active and Shutdown waits for it; registering
	// first means shutdown can never observe an empty tracker in the window
	// between a hijack and its registration (Q384).
	//
	// The release is deferred ahead of the two Close defers so it runs after
	// them: a tunnel is reported drained only once both of its connections are
	// actually closed, never while a relay is still unwinding.
	tn := s.tunnels.add()
	defer s.tunnels.release(tn)
	defer func() { _ = upstream.Close() }()
	tn.track(upstream)

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	conn, _, err := hijacker.Hijack()
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	tn.track(conn)

	if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		// The client is already gone or the conn is broken: counting and
		// tunneling a connection whose CONNECT-200 reply never landed would
		// dirty the metrics and immediately die in io.Copy. Bail before either.
		// The deferred conn.Close()/upstream.Close() handle cleanup.
		s.logger().Debug("CONNECT response write failed", "host", r.Host, "error", err)
		return
	}

	s.connectionsTotal.WithLabelValues().Inc()
	s.connectionsActive.WithLabelValues().Inc()
	defer s.connectionsActive.WithLabelValues().Dec()

	maxLifetime := s.MaxTunnelLifetime
	if maxLifetime == 0 {
		maxLifetime = defaultMaxTunnelLifetime
	}
	idleTimeout := s.TunnelIdleTimeout
	if idleTimeout == 0 {
		idleTimeout = defaultTunnelIdleTimeout
	}
	hardDeadline := time.Now().Add(maxLifetime)

	start := time.Now()
	defer func() {
		s.tunnelDuration.WithLabelValues().Observe(time.Since(start).Seconds())
	}()

	clientSrc := &idleDeadlineConn{Conn: conn, idle: idleTimeout, hardDeadline: hardDeadline}
	upstreamSrc := &idleDeadlineConn{Conn: upstream, idle: idleTimeout, hardDeadline: hardDeadline}

	// Per-direction byte counters for the audit record. Each relay stores its
	// own exactly once, and the receives below order both stores before the
	// read; atomic keeps that ordering self-evident rather than resting on the
	// channel.
	var bytesToDestination, bytesFromDestination atomic.Int64

	done := make(chan struct{}, 2)
	relay := func(dst, src net.Conn, n *atomic.Int64) {
		defer func() { done <- struct{}{} }()
		copied, _ := io.Copy(dst, src)
		n.Store(copied)
		if tc, ok := dst.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}
	go relay(upstream, clientSrc, &bytesToDestination)
	go relay(conn, upstreamSrc, &bytesFromDestination)
	<-done

	if s.AuditLogging == AuditConnections {
		// Close both ends so the far relay unwinds and its counter is final
		// before the record is written. The deferred Closes do exactly this,
		// but they run after the body returns — too late to be read here. Both
		// relays are then guaranteed to send, so the second receive cannot
		// block: io.Copy returns once its conn is closed, and the per-direction
		// read deadline bounds it even if it did not.
		_ = conn.Close()
		_ = upstream.Close()
		<-done
		s.logConnectAudit(r.Host, bytesToDestination.Load(), bytesFromDestination.Load(), time.Since(start))
	}
}

// idleDeadlineConn refreshes the underlying conn's read deadline on every
// Read so an idle stream is torn down after `idle` of inactivity, while
// hardDeadline imposes an absolute upper bound on tunnel lifetime.
type idleDeadlineConn struct {
	net.Conn
	idle         time.Duration
	hardDeadline time.Time
}

func (c *idleDeadlineConn) Read(p []byte) (int, error) {
	deadline := time.Now().Add(c.idle)
	if !c.hardDeadline.IsZero() && deadline.After(c.hardDeadline) {
		deadline = c.hardDeadline
	}
	_ = c.Conn.SetReadDeadline(deadline)
	return c.Conn.Read(p)
}
