package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// fakeResolver is a deterministic ipResolver for the CIDR-allowlist tests.
type fakeResolver struct {
	ips map[string][]net.IPAddr
	err error
}

func (f fakeResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.ips[host], nil
}

func mustCIDR(t *testing.T, s string) *net.IPNet {
	t.Helper()
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatalf("parse CIDR %q: %v", s, err)
	}
	return n
}

func ipAddrs(ips ...string) []net.IPAddr {
	out := make([]net.IPAddr, 0, len(ips))
	for _, ip := range ips {
		out = append(out, net.IPAddr{IP: net.ParseIP(ip)})
	}
	return out
}

func TestCheckDestination(t *testing.T) {
	tests := []struct {
		name        string
		suffixes    []string
		cidrs       []string
		resolve     map[string][]net.IPAddr
		resolveErr  error
		hostport    string
		wantAllowed bool
		wantDial    string // only checked when wantAllowed
		wantDest    string
		wantLookErr bool
	}{
		{
			name:        "no allowlist is transport-only: any host allowed, dialed verbatim",
			hostport:    "anything.example.com:443",
			wantAllowed: true,
			wantDial:    "anything.example.com:443",
			wantDest:    destinationAny,
		},
		{
			name:        "exact host suffix match",
			suffixes:    []string{"golang.org"},
			hostport:    "golang.org:443",
			wantAllowed: true,
			wantDial:    "golang.org:443",
			wantDest:    "golang.org",
		},
		{
			name:        "subdomain of an allowed suffix",
			suffixes:    []string{"golang.org"},
			hostport:    "proxy.golang.org:443",
			wantAllowed: true,
			wantDial:    "proxy.golang.org:443",
			wantDest:    "golang.org",
		},
		{
			name:        "case-insensitive and trailing-dot host normalize to a match",
			suffixes:    []string{"golang.org"},
			hostport:    "Proxy.GoLang.Org.:443",
			wantAllowed: true,
			wantDial:    "Proxy.GoLang.Org.:443", // dialed verbatim (FQDN policy is the hard gate)
			wantDest:    "golang.org",
		},
		{
			name:        "non-matching suffix and no CIDR is denied",
			suffixes:    []string{"golang.org"},
			hostport:    "evil.example.com:443",
			wantAllowed: false,
			wantDest:    destinationUnlisted,
		},
		{
			name:        "a different suffix is not a partial match (evilgolang.org)",
			suffixes:    []string{"golang.org"},
			hostport:    "evilgolang.org:443",
			wantAllowed: false,
			wantDest:    destinationUnlisted,
		},
		{
			name:        "literal IP inside an allowed CIDR",
			cidrs:       []string{"10.0.0.0/8"},
			hostport:    "10.1.2.3:443",
			wantAllowed: true,
			wantDial:    "10.1.2.3:443",
			wantDest:    "10.0.0.0/8",
		},
		{
			name:        "literal IP outside the allowed CIDR is denied",
			cidrs:       []string{"10.0.0.0/8"},
			hostport:    "192.168.1.1:443",
			wantAllowed: false,
			wantDest:    destinationUnlisted,
		},
		{
			name:        "hostname resolving into an allowed CIDR pins the validated IP",
			cidrs:       []string{"199.36.153.8/30"},
			resolve:     map[string][]net.IPAddr{"private.googleapis.com": ipAddrs("199.36.153.10")},
			hostport:    "private.googleapis.com:443",
			wantAllowed: true,
			wantDial:    "199.36.153.10:443", // pinned, not dialed by name
			wantDest:    "199.36.153.8/30",
		},
		{
			name:        "hostname resolving outside every allowed CIDR is denied",
			cidrs:       []string{"199.36.153.8/30"},
			resolve:     map[string][]net.IPAddr{"attacker.example.com": ipAddrs("203.0.113.7")},
			hostport:    "attacker.example.com:443",
			wantAllowed: false,
			wantDest:    destinationUnlisted,
		},
		{
			name:        "resolver error denies (fail closed)",
			cidrs:       []string{"10.0.0.0/8"},
			resolveErr:  errors.New("dns timeout"),
			hostport:    "unknown.example.com:443",
			wantAllowed: false,
			wantDest:    destinationUnlisted,
			wantLookErr: true,
		},
		{
			name:        "a name that does not exist is a refusal, not a resolver failure",
			cidrs:       []string{"10.0.0.0/8"},
			resolveErr:  &net.DNSError{Err: "no such host", Name: "metadata.google.internal", IsNotFound: true},
			hostport:    "metadata.google.internal:80",
			wantAllowed: false,
			wantDest:    destinationUnlisted,
		},
		{
			name:        "suffix match wins without resolving (no resolver consulted)",
			suffixes:    []string{"golang.org"},
			cidrs:       []string{"10.0.0.0/8"},
			hostport:    "proxy.golang.org:443",
			wantAllowed: true,
			wantDial:    "proxy.golang.org:443",
			wantDest:    "golang.org",
		},
		{
			name:        "the destination is the suffix as configured, not as normalized",
			suffixes:    []string{".GoLang.org."},
			hostport:    "proxy.golang.org:443",
			wantAllowed: true,
			wantDial:    "proxy.golang.org:443",
			wantDest:    ".GoLang.org.",
		},
		{
			name:        "malformed host:port is denied",
			suffixes:    []string{"golang.org"},
			hostport:    "no-port",
			wantAllowed: false,
			wantDest:    destinationUnlisted,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{AllowedHostSuffixes: tc.suffixes}
			for _, c := range tc.cidrs {
				s.AllowedCIDRs = append(s.AllowedCIDRs, mustCIDR(t, c))
			}
			if tc.resolve != nil || tc.resolveErr != nil {
				s.dnsResolver = fakeResolver{ips: tc.resolve, err: tc.resolveErr}
			}

			dial, dest, allowed, lookupErr := s.checkDestination(context.Background(), tc.hostport)
			if allowed != tc.wantAllowed {
				t.Fatalf("allowed = %v, want %v", allowed, tc.wantAllowed)
			}
			if dest != tc.wantDest {
				t.Errorf("destination = %q, want %q", dest, tc.wantDest)
			}
			if (lookupErr != nil) != tc.wantLookErr {
				t.Errorf("lookupErr = %v, want error: %v", lookupErr, tc.wantLookErr)
			}
			if tc.wantAllowed && dial != tc.wantDial {
				t.Errorf("dialAddr = %q, want %q", dial, tc.wantDial)
			}
			if !allowed && dial != "" {
				t.Errorf("denied destination should return empty dialAddr, got %q", dial)
			}
		})
	}
}

// TestHandleConnect_DeniedDestination covers the 403 deny path and its metric.
// The deny branch returns before the hijack, so an httptest recorder suffices.
func TestHandleConnect_DeniedDestination(t *testing.T) {
	s := NewServer(":0", ":0", time.Second, slog.Default(), prometheus.NewRegistry())
	s.AllowedHostSuffixes = []string{"golang.org"}

	req := httptest.NewRequest(http.MethodConnect, "http://evil.example.com:443", nil)
	req.Host = "evil.example.com:443"
	rec := httptest.NewRecorder()

	s.handleConnect(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if got := testutil.ToFloat64(s.connectDenied.WithLabelValues()); got != 1 {
		t.Errorf("connect_denied_total = %v, want 1", got)
	}
	if got := testutil.ToFloat64(s.connects.WithLabelValues(destinationUnlisted, "denied")); got != 1 {
		t.Errorf("connects_total{destination=unlisted,outcome=denied} = %v, want 1", got)
	}
	if got := testutil.CollectAndCount(s.connects); got != 1 {
		t.Errorf("connects_total has %d series, want 1", got)
	}
}

// TestHandleConnect_CIDRLookupFailure pins that a CIDR-path lookup that failed
// without an answer is refused like a denial but counted as its own outcome, and that it still
// counts on connect_denied_total, whose meaning dashboards and alerts read.
func TestHandleConnect_CIDRLookupFailure(t *testing.T) {
	s := NewServer(":0", ":0", time.Second, slog.Default(), prometheus.NewRegistry())
	s.AllowedCIDRs = []*net.IPNet{mustCIDR(t, "10.0.0.0/8")}
	s.dnsResolver = fakeResolver{err: &net.DNSError{Err: "i/o timeout", Name: "unknown.example.com", IsTimeout: true, IsTemporary: true}}

	req := httptest.NewRequest(http.MethodConnect, "http://unknown.example.com:443", nil)
	req.Host = "unknown.example.com:443"
	rec := httptest.NewRecorder()

	s.handleConnect(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if got := testutil.ToFloat64(s.connects.WithLabelValues(destinationUnlisted, "dns_error")); got != 1 {
		t.Errorf("connects_total{destination=unlisted,outcome=dns_error} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(s.connects.WithLabelValues(destinationUnlisted, "denied")); got != 0 {
		t.Errorf("connects_total{destination=unlisted,outcome=denied} = %v, want 0", got)
	}
	if got := testutil.ToFloat64(s.connectDenied.WithLabelValues()); got != 1 {
		t.Errorf("connect_denied_total = %v, want 1", got)
	}
}

// TestHandleConnect_CIDRLookupNotFound pins that a name which does not exist is
// counted as a denial: probing nonexistent names is a probe, not a DNS outage.
func TestHandleConnect_CIDRLookupNotFound(t *testing.T) {
	s := NewServer(":0", ":0", time.Second, slog.Default(), prometheus.NewRegistry())
	s.AllowedCIDRs = []*net.IPNet{mustCIDR(t, "10.0.0.0/8")}
	s.dnsResolver = fakeResolver{err: &net.DNSError{Err: "no such host", Name: "metadata.google.internal", IsNotFound: true}}

	req := httptest.NewRequest(http.MethodConnect, "http://metadata.google.internal:80", nil)
	req.Host = "metadata.google.internal:80"
	rec := httptest.NewRecorder()

	s.handleConnect(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if got := testutil.ToFloat64(s.connects.WithLabelValues(destinationUnlisted, "denied")); got != 1 {
		t.Errorf("connects_total{destination=unlisted,outcome=denied} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(s.connects.WithLabelValues(destinationUnlisted, "dns_error")); got != 0 {
		t.Errorf("connects_total{destination=unlisted,outcome=dns_error} = %v, want 0", got)
	}
}

// TestHandleConnect_DialRefusedCountsTheMatchedEntry covers a dial failure to
// an allowed destination: the outcome names the class and the destination names
// the CIDR that permitted it, never the address dialed.
func TestHandleConnect_DialRefusedCountsTheMatchedEntry(t *testing.T) {
	// A port that was bound and released refuses the dial.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	s := NewServer(":0", ":0", time.Second, slog.Default(), prometheus.NewRegistry())
	s.AllowedCIDRs = []*net.IPNet{mustCIDR(t, "127.0.0.0/8")}

	req := httptest.NewRequest(http.MethodConnect, "http://"+addr, nil)
	req.Host = addr
	rec := httptest.NewRecorder()

	s.handleConnect(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
	if got := testutil.ToFloat64(s.connects.WithLabelValues("127.0.0.0/8", "refused")); got != 1 {
		t.Errorf("connects_total{destination=127.0.0.0/8,outcome=refused} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(s.dialErrors.WithLabelValues()); got != 1 {
		t.Errorf("dial_errors_total = %v, want 1", got)
	}
}

func TestDialOutcome(t *testing.T) {
	tests := []struct {
		err  error
		want string
	}{
		{&net.DNSError{Err: "no such host", IsNotFound: true}, "dns_error"},
		{&net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, "refused"},
		{&net.OpError{Op: "dial", Err: timeoutErr{}}, "timeout"},
		{&net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.EHOSTUNREACH)}, "unreachable"},
		{&net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.ENETUNREACH)}, "unreachable"},
		{errors.New("something else"), "error"},
	}
	for _, tc := range tests {
		if got := dialOutcome(tc.err); got != tc.want {
			t.Errorf("dialOutcome(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

func TestSplitList(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"a.com", []string{"a.com"}},
		{" a.com , b.com ,, c.com ", []string{"a.com", "b.com", "c.com"}},
	}
	for _, tc := range tests {
		got := splitList(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("splitList(%q) = %v, want %v", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("splitList(%q)[%d] = %q, want %q", tc.in, i, got[i], tc.want[i])
			}
		}
	}
}

func TestParseCIDRList(t *testing.T) {
	t.Run("empty is nil, no error", func(t *testing.T) {
		got, err := parseCIDRList("")
		if err != nil || got != nil {
			t.Fatalf("parseCIDRList(\"\") = %v, %v; want nil, nil", got, err)
		}
	})
	t.Run("valid CIDRs parse", func(t *testing.T) {
		got, err := parseCIDRList("10.0.0.0/8, 199.36.153.8/30")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("got %d CIDRs, want 2", len(got))
		}
	})
	t.Run("malformed CIDR fails closed", func(t *testing.T) {
		if _, err := parseCIDRList("10.0.0.0/8, not-a-cidr"); err == nil {
			t.Error("expected an error for a malformed CIDR, got nil")
		}
	})
}
