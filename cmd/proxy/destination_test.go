package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
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
	}{
		{
			name:        "no allowlist is transport-only: any host allowed, dialed verbatim",
			hostport:    "anything.example.com:443",
			wantAllowed: true,
			wantDial:    "anything.example.com:443",
		},
		{
			name:        "exact host suffix match",
			suffixes:    []string{"golang.org"},
			hostport:    "golang.org:443",
			wantAllowed: true,
			wantDial:    "golang.org:443",
		},
		{
			name:        "subdomain of an allowed suffix",
			suffixes:    []string{"golang.org"},
			hostport:    "proxy.golang.org:443",
			wantAllowed: true,
			wantDial:    "proxy.golang.org:443",
		},
		{
			name:        "case-insensitive and trailing-dot host normalize to a match",
			suffixes:    []string{"golang.org"},
			hostport:    "Proxy.GoLang.Org.:443",
			wantAllowed: true,
			wantDial:    "Proxy.GoLang.Org.:443", // dialed verbatim (FQDN policy is the hard gate)
		},
		{
			name:        "non-matching suffix and no CIDR is denied",
			suffixes:    []string{"golang.org"},
			hostport:    "evil.example.com:443",
			wantAllowed: false,
		},
		{
			name:        "a different suffix is not a partial match (evilgolang.org)",
			suffixes:    []string{"golang.org"},
			hostport:    "evilgolang.org:443",
			wantAllowed: false,
		},
		{
			name:        "literal IP inside an allowed CIDR",
			cidrs:       []string{"10.0.0.0/8"},
			hostport:    "10.1.2.3:443",
			wantAllowed: true,
			wantDial:    "10.1.2.3:443",
		},
		{
			name:        "literal IP outside the allowed CIDR is denied",
			cidrs:       []string{"10.0.0.0/8"},
			hostport:    "192.168.1.1:443",
			wantAllowed: false,
		},
		{
			name:        "hostname resolving into an allowed CIDR pins the validated IP",
			cidrs:       []string{"199.36.153.8/30"},
			resolve:     map[string][]net.IPAddr{"private.googleapis.com": ipAddrs("199.36.153.10")},
			hostport:    "private.googleapis.com:443",
			wantAllowed: true,
			wantDial:    "199.36.153.10:443", // pinned, not dialed by name
		},
		{
			name:        "hostname resolving outside every allowed CIDR is denied",
			cidrs:       []string{"199.36.153.8/30"},
			resolve:     map[string][]net.IPAddr{"attacker.example.com": ipAddrs("203.0.113.7")},
			hostport:    "attacker.example.com:443",
			wantAllowed: false,
		},
		{
			name:        "resolver error denies (fail closed)",
			cidrs:       []string{"10.0.0.0/8"},
			resolveErr:  errors.New("dns timeout"),
			hostport:    "unknown.example.com:443",
			wantAllowed: false,
		},
		{
			name:        "suffix match wins without resolving (no resolver consulted)",
			suffixes:    []string{"golang.org"},
			cidrs:       []string{"10.0.0.0/8"},
			hostport:    "proxy.golang.org:443",
			wantAllowed: true,
			wantDial:    "proxy.golang.org:443",
		},
		{
			name:        "malformed host:port is denied",
			suffixes:    []string{"golang.org"},
			hostport:    "no-port",
			wantAllowed: false,
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

			dial, allowed := s.checkDestination(context.Background(), tc.hostport)
			if allowed != tc.wantAllowed {
				t.Fatalf("allowed = %v, want %v", allowed, tc.wantAllowed)
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
