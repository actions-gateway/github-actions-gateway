package main

import "testing"

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
