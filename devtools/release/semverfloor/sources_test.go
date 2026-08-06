package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestGoBuildsIn(t *testing.T) {
	tests := []struct {
		name    string
		cmd     string
		wantMod string
		wantPkg string
		wantErr bool
	}{
		{"root module", `go build -trimpath -o /bin/proxy ./cmd/proxy`, ".", "./cmd/proxy", false},
		{"-C module", `go build -C cmd/gmc -trimpath -o /bin/manager ./cmd`, "cmd/gmc", "./cmd", false},
		{"-C= form", `go build -C=cmd/agc -o /bin/agc .`, "cmd/agc", ".", false},
		{"quoted -C", `go build -C "cmd/gmc" -o /x ./migrate`, "cmd/gmc", "./migrate", false},

		// The regression that motivated parsing with a shell grammar: a quoted
		// -ldflags value holds spaces, and splitting on whitespace turns its
		// tail into a second "package pattern".
		{
			"ldflags with spaces stays one word",
			`go build -C cmd/gmc -trimpath -ldflags="-buildid= -X main.version=${VERSION}" -o /bin/manager ./cmd`,
			"cmd/gmc", "./cmd", false,
		},
		{
			"single-quoted ldflags",
			`go build -trimpath -ldflags='-s -w' -o /bin/x ./cmd/x`,
			".", "./cmd/x", false,
		},
		// A build nested in a subshell after a cd — the shape the migrate
		// script uses — is still found.
		{
			"nested in a subshell chain",
			`( cd cmd/gmc && CGO_ENABLED=0 go build -trimpath -o "$out" ./migrate )`,
			".", "./migrate", false,
		},

		// A list with no package pattern is an error rather than a guess: the
		// alternative is silently deriving the wrong surface, which would read
		// every commit against the missing tree as non-shipping.
		{"ends in a flag value", `go build -trimpath -o /bin/proxy`, "", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			builds, err := goBuildsIn(tc.cmd)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %+v", builds)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(builds) != 1 {
				t.Fatalf("found %d builds, want 1: %+v", len(builds), builds)
			}
			if builds[0].ModuleDir != tc.wantMod || builds[0].Package != tc.wantPkg {
				t.Errorf("got (%q, %q), want (%q, %q)",
					builds[0].ModuleDir, builds[0].Package, tc.wantMod, tc.wantPkg)
			}
		})
	}
}

func TestGoBuildsInIgnoresNonBuilds(t *testing.T) {
	for _, cmd := range []string{
		`go list -deps ./...`,
		`go vet ./...`,
		`echo "go build is mentioned but not run"`,
		`apk add --no-cache git`,
	} {
		builds, err := goBuildsIn(cmd)
		if err != nil {
			t.Errorf("goBuildsIn(%q): %v", cmd, err)
		}
		if len(builds) != 0 {
			t.Errorf("goBuildsIn(%q) = %+v, want none", cmd, builds)
		}
	}
}

func TestLogicalLinesJoinsContinuations(t *testing.T) {
	got := logicalLines("RUN go build \\\n  -o /bin/x \\\n  ./cmd/x\nFROM a AS b\n# a comment\n")

	// The join is asserted through what reads it: a continued RUN must yield
	// one build, and the comment must not survive as a line of its own.
	if len(got) == 0 || !strings.HasPrefix(got[0], "RUN ") {
		t.Fatalf("first logical line = %q, want the joined RUN", got)
	}
	builds, err := goBuildsIn(strings.TrimPrefix(got[0], "RUN "))
	if err != nil {
		t.Fatalf("goBuildsIn: %v", err)
	}
	if len(builds) != 1 || builds[0].Package != "./cmd/x" {
		t.Errorf("builds = %+v, want one ./cmd/x", builds)
	}
	for _, l := range got {
		if strings.HasPrefix(l, "#") {
			t.Errorf("comment survived as %q", l)
		}
	}
}

const testDockerfile = `
FROM golang:1.26 AS deps
WORKDIR /src

FROM deps AS src
COPY . .

FROM src AS build-gmc
RUN go build -C cmd/gmc -trimpath -o /bin/manager ./cmd

FROM src AS build-wrapper
RUN go build -trimpath -o /wrapper ./cmd/worker

FROM src AS build-fakegithub
RUN go build -trimpath -o /bin/fakegithub ./test/fakegithub

FROM distroless AS gmc
COPY --from=build-gmc /bin/manager /manager

FROM scratch AS wrapper
COPY --from=build-wrapper /wrapper /wrapper

FROM distroless AS fakegithub
COPY --from=build-fakegithub /bin/fakegithub /fakegithub
`

func TestParseDockerfileAndBuildsFor(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Dockerfile")
	if err := os.WriteFile(path, []byte(testDockerfile), 0o600); err != nil {
		t.Fatal(err)
	}
	stages, err := ParseDockerfile(path)
	if err != nil {
		t.Fatalf("ParseDockerfile: %v", err)
	}

	builds, err := buildsFor(stages, "gmc")
	if err != nil {
		t.Fatalf("buildsFor(gmc): %v", err)
	}
	if len(builds) != 1 || builds[0].ModuleDir != "cmd/gmc" || builds[0].Package != "./cmd" {
		t.Errorf("gmc builds = %+v, want one cmd/gmc ./cmd", builds)
	}

	// A test fixture stage exists in the Dockerfile but is not a published
	// image, so it only reaches the surface if publish.yml names it.
	if _, err := buildsFor(stages, "fakegithub"); err != nil {
		t.Errorf("fakegithub is a real stage: %v", err)
	}

	// An image publish.yml names that the Dockerfile cannot build is an error,
	// not an empty result — that is the derivation going stale.
	if _, err := buildsFor(stages, "nosuchimage"); err == nil {
		t.Error("expected an error for an image with no Dockerfile stage")
	}
}

const testWorkflow = `
name: publish
jobs:
  image:
    strategy:
      matrix:
        name: [gmc, wrapper]
    steps:
      - name: build
        run: docker buildx build .
  chart-publish:
    steps:
      - name: package
        run: |
          helm package charts/actions-gateway --version 1.0.0
          helm package charts/actions-gateway-crds-v2 --version 1.0.0
      - name: migrate
        run: scripts/release/build-migrate-binaries.sh "${out}" "${TAG}"
`

func TestParsePublishWorkflow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "publish.yml")
	if err := os.WriteFile(path, []byte(testWorkflow), 0o600); err != nil {
		t.Fatal(err)
	}
	images, charts, scripts, err := ParsePublishWorkflow(path)
	if err != nil {
		t.Fatalf("ParsePublishWorkflow: %v", err)
	}
	if want := []string{"gmc", "wrapper"}; !reflect.DeepEqual(images, want) {
		t.Errorf("images = %v, want %v", images, want)
	}
	if want := []string{"charts/actions-gateway", "charts/actions-gateway-crds-v2"}; !reflect.DeepEqual(charts, want) {
		t.Errorf("charts = %v, want %v", charts, want)
	}
	found := false
	for _, s := range scripts {
		if s == "scripts/release/build-migrate-binaries.sh" {
			found = true
		}
	}
	if !found {
		t.Errorf("scripts = %v, want the migrate build script among them", scripts)
	}
}

// TestCheckSourcesCatchesADroppedScript is the staleness gate's own red case:
// a declared build whose publish.yml step is gone must be reported, because the
// declaration would otherwise outlive what it describes.
func TestCheckSourcesCatchesADroppedScript(t *testing.T) {
	root := t.TempDir()
	problems := CheckSources(root, Sources{RunScript: []string{"scripts/fetch/retry.sh"}})
	if len(problems) == 0 {
		t.Fatal("expected a problem when the declared build's script is not invoked")
	}
}

func TestCheckSourcesCleanWhenScriptPresent(t *testing.T) {
	root := t.TempDir()
	var invoked []string
	for _, d := range declaredBuilds {
		invoked = append(invoked, d.Script)
	}
	if problems := CheckSources(root, Sources{RunScript: invoked}); len(problems) != 0 {
		t.Errorf("expected no problems, got %v", problems)
	}
}

// A publish-invoked script that builds a Go binary nothing declares is a
// release asset the surface may be missing.
func TestCheckSourcesFlagsAnUndeclaredGoBuildingScript(t *testing.T) {
	root := t.TempDir()
	scriptDir := filepath.Join(root, "scripts", "release")
	if err := os.MkdirAll(scriptDir, 0o750); err != nil {
		t.Fatal(err)
	}
	rel := "scripts/release/build-something-else.sh"
	if err := os.WriteFile(filepath.Join(root, rel), []byte("#!/bin/bash\ngo build -o /x ./cmd/x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var invoked []string
	for _, d := range declaredBuilds {
		invoked = append(invoked, d.Script)
	}
	problems := CheckSources(root, Sources{RunScript: append(invoked, rel)})
	if len(problems) != 1 {
		t.Fatalf("problems = %v, want exactly the undeclared builder", problems)
	}
}
