package controller

import (
	"strings"
	"testing"
)

func TestLabelSafe(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantSeg  string // expected sanitized segment (before the hash)
		wantLen  int    // exact expected output length, 0 means don't check
		distinct []string // inputs that must produce a different output than input
	}{
		{
			name:    "lowercase passthrough",
			input:   "linux",
			wantSeg: "linux",
		},
		{
			name:    "uppercase is lowercased",
			input:   "Linux",
			wantSeg: "linux",
		},
		{
			name:    "slash replaced with hyphen",
			input:   "gpu/a100",
			wantSeg: "gpu-a100",
			distinct: []string{"gpu_a100", "gpu.a100"},
		},
		{
			name:    "underscore replaced with hyphen",
			input:   "gpu_a100",
			wantSeg: "gpu-a100",
			distinct: []string{"gpu/a100"},
		},
		{
			name:    "leading and trailing hyphens trimmed",
			input:   "_leading_trailing_",
			wantSeg: "leading-trailing",
		},
		{
			name:    "all special chars produces label fallback",
			input:   "///",
			wantSeg: "label",
			distinct: []string{"___", "..."},
		},
		{
			name:    "empty string produces label fallback",
			input:   "",
			wantSeg: "label",
		},
		{
			name: "long input truncated at 40 chars",
			// 50 'a' chars
			input:   strings.Repeat("a", 50),
			wantSeg: strings.Repeat("a", 40),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := labelSafe(tc.input)

			// Output must end with "-" + 7 hex chars.
			parts := strings.SplitN(got, "-", 2)
			if len(parts) < 2 {
				t.Fatalf("labelSafe(%q) = %q: missing hash suffix", tc.input, got)
			}
			hashPart := got[len(got)-7:]
			for _, c := range hashPart {
				if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
					t.Fatalf("labelSafe(%q) = %q: hash suffix %q contains non-hex char %q", tc.input, got, hashPart, string(c))
				}
			}

			// Segment (everything before the last "-<7hex>") must match expected.
			seg := got[:len(got)-8] // strip "-" + 7 hex
			if seg != tc.wantSeg {
				t.Errorf("labelSafe(%q) segment = %q, want %q", tc.input, seg, tc.wantSeg)
			}

			// Segment must not exceed 40 chars.
			if len(seg) > 40 {
				t.Errorf("labelSafe(%q) segment length %d > 40", tc.input, len(seg))
			}

			// Distinct inputs must produce different outputs.
			for _, other := range tc.distinct {
				if otherGot := labelSafe(other); otherGot == got {
					t.Errorf("labelSafe(%q) == labelSafe(%q) = %q; want distinct outputs", tc.input, other, got)
				}
			}

			// Output must be idempotent.
			if got2 := labelSafe(tc.input); got2 != got {
				t.Errorf("labelSafe(%q) is not idempotent: %q != %q", tc.input, got, got2)
			}
		})
	}
}
