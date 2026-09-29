package main

import (
	"bufio"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func rootOf(t *testing.T, src string) *yaml.Node {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(src), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return contentRoot(&doc)
}

func filters(t *testing.T, src string) string {
	t.Helper()
	var sb strings.Builder
	w := bufio.NewWriter(&sb)
	if err := writeFilters(w, rootOf(t, src)); err != nil {
		t.Fatalf("writeFilters: %v", err)
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	return sb.String()
}

func pushPaths(t *testing.T, src string) string {
	t.Helper()
	return triggerPaths(t, src, "push")
}

func triggerPaths(t *testing.T, src, event string) string {
	t.Helper()
	var sb strings.Builder
	w := bufio.NewWriter(&sb)
	if err := writeTriggerPaths(w, rootOf(t, src), event); err != nil {
		t.Fatalf("writeTriggerPaths: %v", err)
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	return sb.String()
}

const literalBlock = `
jobs:
  changes:
    steps:
      - uses: dorny/paths-filter@v3
        with:
          filters: |
            code:
              - 'api/**'
              - 'broker/**'
            docs:
              - 'docs/**'
`

// The awk this replaces matched `filters: |` and nothing else. Each spelling
// below is valid YAML carrying the same filter set, and every one of them made
// the gate report bogus coverage errors.
func TestFiltersAcceptsEveryValidBlockSpelling(t *testing.T) {
	want := "code\tapi/**\ncode\tbroker/**\ndocs\tdocs/**\n"

	if got := filters(t, literalBlock); got != want {
		t.Errorf("literal block:\n got %q\nwant %q", got, want)
	}

	strip := strings.Replace(literalBlock, "filters: |", "filters: |-", 1)
	if got := filters(t, strip); got != want {
		t.Errorf("strip-chomped block (|-):\n got %q\nwant %q", got, want)
	}

	keep := strings.Replace(literalBlock, "filters: |", "filters: |+", 1)
	if got := filters(t, keep); got != want {
		t.Errorf("keep-chomped block (|+):\n got %q\nwant %q", got, want)
	}
}

func TestFiltersReadsFlowStyleAndQuoting(t *testing.T) {
	src := `
jobs:
  changes:
    steps:
      - with:
          filters: |
            code: ['api/**', "broker/**"]
            docs:
              - docs/**
`
	want := "code\tapi/**\ncode\tbroker/**\ndocs\tdocs/**\n"
	if got := filters(t, src); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// A '#' with no leading space is part of the scalar, not a comment. The awk cut
// the pattern at the '#' and handed the caller a shorter, broader one.
func TestFiltersKeepsAnInlineHashInAnUnquotedPattern(t *testing.T) {
	src := `
jobs:
  changes:
    steps:
      - with:
          filters: |
            code:
              - api/nested#frag/**   # trailing comment is dropped
`
	want := "code\tapi/nested#frag/**\n"
	if got := filters(t, src); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFiltersAcceptsAScalarPatternList(t *testing.T) {
	src := `
jobs:
  changes:
    steps:
      - with:
          filters: |
            code: 'api/**'
`
	if got, want := filters(t, src), "code\tapi/**\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFiltersCollectsEveryBlockInDocumentOrder(t *testing.T) {
	src := `
jobs:
  a:
    steps:
      - with:
          filters: |
            first:
              - 'a/**'
  b:
    steps:
      - with:
          filters: |
            second:
              - 'b/**'
`
	want := "first\ta/**\nsecond\tb/**\n"
	if got := filters(t, src); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// dorny/paths-filter's documented way to share patterns between filters: an
// anchored list spliced into another as `- *shared`. The action flattens the
// nested array; a reader that dropped the alias would report `src` as `src/**`
// alone, and every assertion over it would miss the shared patterns.
func TestFiltersResolvesAnAliasedSequence(t *testing.T) {
	src := `
jobs:
  changes:
    steps:
      - with:
          filters: |
            shared: &shared
              - 'common/**'
              - 'config/**'
            src:
              - *shared
              - 'src/**'
`
	want := "shared\tcommon/**\nshared\tconfig/**\nsrc\tcommon/**\nsrc\tconfig/**\nsrc\tsrc/**\n"
	if got := filters(t, src); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFiltersResolvesAnAliasedScalar(t *testing.T) {
	src := `
jobs:
  changes:
    steps:
      - with:
          filters: |
            a:
              - &verdict 'cmd/verdict.go'
            b:
              - *verdict
`
	want := "a\tcmd/verdict.go\nb\tcmd/verdict.go\n"
	if got := filters(t, src); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFiltersIsEmptyWhenTheWorkflowHasNone(t *testing.T) {
	if got := filters(t, "jobs:\n  a:\n    steps:\n      - run: true\n"); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

// YAML 1.1 resolves a bare `on` to true, which would make the lookup miss
// entirely. yaml.v3 does not, and this pins that.
func TestPushPathsReadsTheUnquotedOnKey(t *testing.T) {
	src := `
on:
  push:
    paths:
      - 'cmd/gmc/**'
      - 'charts/**'
`
	want := "cmd/gmc/**\ncharts/**\n"
	if got := pushPaths(t, src); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestPushPathsIsEmptyWithoutAPushPathsList(t *testing.T) {
	for name, src := range map[string]string{
		"no on":    "jobs:\n  a:\n    steps: []\n",
		"no push":  "on:\n  pull_request:\n    branches: [main]\n",
		"no paths": "on:\n  push:\n    branches: [main]\n",
	} {
		if got := pushPaths(t, src); got != "" {
			t.Errorf("%s: got %q, want empty", name, got)
		}
	}
}

func TestPushPathsIgnoresAPathsListOutsideOnPush(t *testing.T) {
	src := `
on:
  pull_request:
    paths:
      - 'should/not/appear/**'
jobs:
  a:
    steps: []
`
	if got := pushPaths(t, src); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

// The mirror of the case above, and the reason a second mode exists: a workflow
// scoping both legs carries two lists, and each mode must read only its own.
func TestPRPathsReadsOnlyThePullRequestList(t *testing.T) {
	src := `
on:
  pull_request:
    paths:
      - 'pr/only/**'
  push:
    branches: [main]
    paths:
      - 'push/only/**'
`
	if got, want := triggerPaths(t, src, "pull_request"), "pr/only/**\n"; got != want {
		t.Errorf("pull_request: got %q, want %q", got, want)
	}
	if got, want := triggerPaths(t, src, "push"), "push/only/**\n"; got != want {
		t.Errorf("push: got %q, want %q", got, want)
	}
}

func jobs(t *testing.T, src string) string {
	t.Helper()
	var sb strings.Builder
	w := bufio.NewWriter(&sb)
	if err := writeJobs(w, rootOf(t, src)); err != nil {
		t.Fatalf("writeJobs: %v", err)
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	return sb.String()
}

func TestJobsEmitsOneLinePerDependency(t *testing.T) {
	got := jobs(t, `
jobs:
  alpha:
    runs-on: ubuntu-latest
  t-gate:
    needs: [alpha, beta]
  beta:
    runs-on: ubuntu-latest
`)
	want := "alpha\t\nt-gate\talpha\nt-gate\tbeta\nbeta\t\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestJobsReadsAScalarNeeds(t *testing.T) {
	// `needs: alpha` is valid YAML for a single dependency. Reading only the
	// sequence form would report the gate as waiting on nothing, which its
	// caller cannot distinguish from a job list that failed to parse.
	got := jobs(t, `
jobs:
  alpha:
    runs-on: ubuntu-latest
  t-gate:
    needs: alpha
`)
	want := "alpha\t\nt-gate\talpha\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestJobsKeepsDocumentOrder(t *testing.T) {
	// The caller compares a gate's needs against its siblings by id, so the
	// order is only a readability property — but a map-ordered walk would make
	// its output unstable between runs, and a diff nobody can read is a gate
	// nobody trusts.
	got := jobs(t, `
jobs:
  zulu:
    runs-on: ubuntu-latest
  alpha:
    runs-on: ubuntu-latest
  mike:
    runs-on: ubuntu-latest
`)
	want := "zulu\t\nalpha\t\nmike\t\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestJobsIsEmptyWithoutAJobsBlock(t *testing.T) {
	if got := jobs(t, "name: t\non: [push]\n"); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}
