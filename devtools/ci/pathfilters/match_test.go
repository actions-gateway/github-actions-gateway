package main

import (
	"bufio"
	"strings"
	"testing"
)

// The rows pin picomatch's reading as measured in docs/development/testing.md
// § Where a globstar works in a filter glob, plus the shapes the workflows use.
func TestMatchPattern(t *testing.T) {
	cases := []struct {
		pattern, file string
		want          bool
	}{
		{"cmd/gmc/**", "cmd/gmc/main.go", true},
		{"cmd/gmc/**", "cmd/gmc/internal/x/y.go", true},
		{"cmd/gmc/**", "cmd/agc/internal/provisioner/admission.go", false},
		{"cmd/gmc/**", "cmd/gmcx/main.go", false},
		{"Dockerfile", "Dockerfile", true},
		{"Dockerfile", "cmd/Dockerfile", false},
		{"**.go", "main.go", true},
		{"**.go", "cmd/agc/config.go", true},
		{"**/*.go", "cmd/agc/config.go", true},
		{"**/go.mod", "go.mod", true},
		{"**/go.mod", "cmd/agc/go.mod", true},
		{"*.go", "cmd/agc/config.go", false},
		{"cmd/**.go", "cmd/agc/config.go", false},
		{"cmd/**.go", "cmd/config.go", true},
		{"deploy/monitoring/grafana-dashboard-*.json", "deploy/monitoring/grafana-dashboard-tenant.json", true},
		{"deploy/monitoring/grafana-dashboard-*.json", "deploy/monitoring/rules.yaml", false},
		{".github/workflows/**", ".github/workflows/e2e-calico.yml", true},
		{"**", ".github/workflows/e2e-calico.yml", true},
		{"./cmd/gmc/**", "cmd/gmc/main.go", true},
	}
	for _, c := range cases {
		if got := matchPattern(c.pattern, c.file); got != c.want {
			t.Errorf("matchPattern(%q, %q) = %v, want %v", c.pattern, c.file, got, c.want)
		}
	}
}

func matches(t *testing.T, src, input string) (string, error) {
	t.Helper()
	var sb strings.Builder
	w := bufio.NewWriter(&sb)
	err := writeMatches(w, strings.NewReader(input), rootOf(t, src))
	if ferr := w.Flush(); ferr != nil {
		t.Fatalf("flush: %v", ferr)
	}
	return sb.String(), err
}

// Q1103's instance: an AGC file is outside the Calico lane, a GMC one inside.
func TestWriteMatchesUnionsFilters(t *testing.T) {
	got, err := matches(t, literalBlock, "cmd/agc/internal/provisioner/admission.go\napi/v1/types.go\ndocs/x.md\n\n")
	if err != nil {
		t.Fatalf("writeMatches: %v", err)
	}
	if want := "api/v1/types.go\ndocs/x.md\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// autoscaler-drift.yml splices a shared list with `- *shared`; dorny resolves the
// alias and flattens it, so the lane covers the anchored patterns too.
func TestWriteMatchesFollowsAliases(t *testing.T) {
	src := "jobs:\n  c:\n    with:\n      filters: |\n        shared: &shared\n          - 'scripts/fetch/**'\n        a:\n          - *shared\n          - 'test/a/**'\n"
	got, err := matches(t, src, "scripts/fetch/kind.sh\ntest/a/x.go\ncmd/agc/x.go\n")
	if err != nil {
		t.Fatalf("writeMatches: %v", err)
	}
	if want := "scripts/fetch/kind.sh\ntest/a/x.go\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// Every refusal guards against a narrower lane than the workflow's, which would
// let a covered path read as uncovered.
func TestWriteMatchesRefuses(t *testing.T) {
	for name, src := range map[string]string{
		"no filters":  "jobs: {}\n",
		"negation":    "jobs:\n  c:\n    with:\n      filters: |\n        a:\n          - '!docs/**'\n",
		"braces":      "jobs:\n  c:\n    with:\n      filters: |\n        a:\n          - 'cmd/{agc,gmc}/**'\n",
		"bad class":   "jobs:\n  c:\n    with:\n      filters: |\n        a:\n          - 'cmd/[agc/**'\n",
		"posix class": "jobs:\n  c:\n    with:\n      filters: |\n        a:\n          - 'deploy/[[:alpha:]]*/**'\n",
		"file path":   "jobs:\n  c:\n    with:\n      filters: .github/filters.yaml\n  d:\n    with:\n      filters: |\n        b:\n          - 'docs/**'\n",
		"change type": "jobs:\n  c:\n    with:\n      filters: |\n        a:\n          - added: 'cmd/**'\n",
	} {
		if _, err := matches(t, src, "cmd/agc/x.go\n"); err == nil {
			t.Errorf("%s: want an error, got none", name)
		}
	}
}
