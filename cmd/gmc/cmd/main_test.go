package main

import (
	"strings"
	"testing"
	"time"
)

func TestParseAllowedPriorityClasses(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{name: "empty yields nil (secure default)", raw: "", want: nil},
		{name: "whitespace only yields nil", raw: "  ,  , ", want: nil},
		{name: "single class", raw: "system-cluster-critical", want: []string{"system-cluster-critical"}},
		{
			name: "multiple with whitespace and empties",
			raw:  " high-priority ,, low-priority ",
			want: []string{"high-priority", "low-priority"},
		},
		{name: "duplicate entries preserved", raw: "a,a", want: []string{"a", "a"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseAllowedPriorityClasses(tt.raw)
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("parseAllowedPriorityClasses(%q) = %v, want %v", tt.raw, got, tt.want)
			}
			if tt.want == nil && got != nil {
				t.Errorf("parseAllowedPriorityClasses(%q) = %v, want nil", tt.raw, got)
			}
		})
	}
}

func TestParseAllowedEgressCIDRs(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    []string
		wantErr bool
	}{
		{name: "empty yields nil (secure default)", raw: "", want: nil},
		{name: "whitespace only yields nil", raw: "  ,  , ", want: nil},
		{name: "single CIDR", raw: "10.0.0.0/8", want: []string{"10.0.0.0/8"}},
		{
			name: "multiple with whitespace and empties",
			raw:  " 10.0.0.0/8 ,, 172.16.0.0/12 ",
			want: []string{"10.0.0.0/8", "172.16.0.0/12"},
		},
		{name: "IPv6 CIDR", raw: "fd00::/8", want: []string{"fd00::/8"}},
		{name: "bare IP without mask is rejected", raw: "10.0.0.1", wantErr: true},
		{name: "garbage is rejected", raw: "not-a-cidr", wantErr: true},
		{name: "one bad entry fails the whole flag", raw: "10.0.0.0/8,bogus", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseAllowedEgressCIDRs(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Errorf("parseAllowedEgressCIDRs(%q) = %v, nil; want error", tt.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseAllowedEgressCIDRs(%q) returned error: %v", tt.raw, err)
			}
			if tt.want == nil {
				if got != nil {
					t.Errorf("parseAllowedEgressCIDRs(%q) = %v, want nil", tt.raw, got)
				}
				return
			}
			gotStrs := make([]string, len(got))
			for i, n := range got {
				gotStrs[i] = n.String()
			}
			if strings.Join(gotStrs, ",") != strings.Join(tt.want, ",") {
				t.Errorf("parseAllowedEgressCIDRs(%q) = %v, want %v", tt.raw, gotStrs, tt.want)
			}
		})
	}
}

func TestMustEnv(t *testing.T) {
	t.Run("set returns the value", func(t *testing.T) {
		t.Setenv("GMC_TEST_MUST_ENV", "some-value")
		got, err := mustEnv("GMC_TEST_MUST_ENV")
		if err != nil {
			t.Fatalf("mustEnv() returned error: %v", err)
		}
		if got != "some-value" {
			t.Errorf("mustEnv() = %q, want %q", got, "some-value")
		}
	})

	t.Run("unset returns an error", func(t *testing.T) {
		// Ensure the variable is not set in the environment (t.Setenv with
		// os.Unsetenv is not available pre-1.17 semantics, but Setenv("") still
		// leaves it "set but empty", which mustEnv treats identically to unset).
		t.Setenv("GMC_TEST_MUST_ENV_UNSET", "")
		got, err := mustEnv("GMC_TEST_MUST_ENV_UNSET")
		if err == nil {
			t.Errorf("mustEnv() = %q, nil; want error", got)
		}
		if got != "" {
			t.Errorf("mustEnv() = %q, want empty string on error", got)
		}
	})
}

func TestValidateImageDigest(t *testing.T) {
	const validDigest = "@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	tests := []struct {
		name    string
		ref     string
		wantErr bool
	}{
		{
			name: "digest-pinned with tag",
			ref:  "ghcr.io/org/agc:v1.2.3" + validDigest,
		},
		{
			name: "digest-pinned without tag",
			ref:  "ghcr.io/org/agc" + validDigest,
		},
		{
			name: "digest-pinned localhost registry",
			ref:  "localhost:5000/agc" + validDigest,
		},
		{
			name:    "floating tag",
			ref:     "ghcr.io/org/agc:v1.2.3",
			wantErr: true,
		},
		{
			name:    "latest tag",
			ref:     "ghcr.io/org/agc:latest",
			wantErr: true,
		},
		{
			name:    "bare repository",
			ref:     "agc",
			wantErr: true,
		},
		{
			name:    "empty reference",
			ref:     "",
			wantErr: true,
		},
		{
			name:    "digest too short",
			ref:     "ghcr.io/org/agc@sha256:0123456789abcdef",
			wantErr: true,
		},
		{
			name:    "digest with uppercase hex",
			ref:     "ghcr.io/org/agc@sha256:0123456789ABCDEF0123456789abcdef0123456789abcdef0123456789abcdef",
			wantErr: true,
		},
		{
			name:    "unsupported digest algorithm",
			ref:     "ghcr.io/org/agc@sha512:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			wantErr: true,
		},
		{
			name:    "trailing content after digest",
			ref:     "ghcr.io/org/agc" + validDigest + "x",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateImageDigest("AGC_IMAGE", tt.ref)
			if tt.wantErr && err == nil {
				t.Errorf("validateImageDigest(%q) = nil, want error", tt.ref)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("validateImageDigest(%q) = %v, want nil", tt.ref, err)
			}
		})
	}
}

func TestParseAPIServerCIDRs(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    []string
		wantErr bool
	}{
		{name: "empty yields nil (secure default)", raw: "", want: nil},
		{name: "whitespace only yields nil", raw: "  ,  , ", want: nil},
		{name: "single CIDR", raw: "10.0.0.1/32", want: []string{"10.0.0.1/32"}},
		{
			name: "multiple with whitespace and empties",
			raw:  " 10.0.0.0/8 ,, 172.16.0.0/12 ",
			want: []string{"10.0.0.0/8", "172.16.0.0/12"},
		},
		{name: "IPv6 CIDR", raw: "fd00::/8", want: []string{"fd00::/8"}},
		{name: "bare IP without mask is rejected", raw: "10.0.0.1", wantErr: true},
		{name: "garbage is rejected", raw: "not-a-cidr", wantErr: true},
		{name: "one bad entry fails the whole flag", raw: "10.0.0.0/8,bogus", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseAPIServerCIDRs(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Errorf("parseAPIServerCIDRs(%q) = %v, nil; want error", tt.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseAPIServerCIDRs(%q) returned error: %v", tt.raw, err)
			}
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("parseAPIServerCIDRs(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}

func TestValidateLeaderElectionTimings(t *testing.T) {
	tests := []struct {
		name                string
		lease, renew, retry time.Duration
		wantErr             bool
	}{
		{
			name:  "controller-runtime defaults",
			lease: 15 * time.Second, renew: 10 * time.Second, retry: 2 * time.Second,
		},
		{
			name:  "tightened for fast failover",
			lease: 9 * time.Second, renew: 6 * time.Second, retry: 2 * time.Second,
		},
		{
			name:  "loosened for slow apiserver",
			lease: 30 * time.Second, renew: 20 * time.Second, retry: 4 * time.Second,
		},
		{
			name:  "lease equals renew",
			lease: 10 * time.Second, renew: 10 * time.Second, retry: 2 * time.Second,
			wantErr: true,
		},
		{
			name:  "lease below renew",
			lease: 8 * time.Second, renew: 10 * time.Second, retry: 2 * time.Second,
			wantErr: true,
		},
		{
			// renew (3s) must exceed retry×1.2 (3.6s); it does not.
			name:  "renew not above retry times jitter",
			lease: 10 * time.Second, renew: 3 * time.Second, retry: 3 * time.Second,
			wantErr: true,
		},
		{
			name:  "zero lease",
			lease: 0, renew: 10 * time.Second, retry: 2 * time.Second,
			wantErr: true,
		},
		{
			name:  "negative retry",
			lease: 15 * time.Second, renew: 10 * time.Second, retry: -1 * time.Second,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateLeaderElectionTimings(tt.lease, tt.renew, tt.retry)
			if tt.wantErr && err == nil {
				t.Errorf("validateLeaderElectionTimings(%s, %s, %s) = nil, want error",
					tt.lease, tt.renew, tt.retry)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("validateLeaderElectionTimings(%s, %s, %s) = %v, want nil",
					tt.lease, tt.renew, tt.retry, err)
			}
		})
	}
}
