package main

import (
	"strings"
	"testing"
)

const dockerfileBase = `# header comment
FROM --platform=$BUILDPLATFORM golang:1.27@sha256:aaa AS deps
RUN go mod download

FROM deps AS build-app
RUN go build -o /bin/app ./cmd/app

FROM gcr.io/distroless/static:nonroot@sha256:111 AS app
COPY --from=build-app /bin/app /app

FROM docker:29-cli@sha256:222 AS docker-cli
FROM scratch AS docker-tools
COPY --from=docker-cli /usr/local/bin/docker /docker

FROM app AS tool-runner
COPY --from=docker-tools /docker /docker

FROM alpine:3.20 AS fixture
RUN echo not published
`

var publishedImages = []string{"app", "tool-runner"}

// TestDockerfileChanged pins which Dockerfile edits put a commit on the released
// surface. Each "ships" case is one the package-directory test could not see:
// the Dockerfile compiles no package.
func TestDockerfileChanged(t *testing.T) {
	dir := narrowRepo(t)
	commit := func(subject, body string) string {
		return commitFiles(t, dir, subject, map[string]string{dockerfilePath: body})
	}
	commit("chore: base", dockerfileBase)

	cases := []struct {
		name string
		edit func(string) string
		want bool
	}{
		{"a published image's base digest moves", func(s string) string {
			return replace(t, s, "distroless/static:nonroot@sha256:111", "distroless/static:nonroot@sha256:999")
		}, true},
		{"a base reached only through COPY --from moves", func(s string) string {
			return replace(t, s, "docker:29-cli@sha256:222", "docker:29-cli@sha256:999")
		}, true},
		{"the builder base behind a published binary moves", func(s string) string {
			return replace(t, s, "golang:1.27@sha256:aaa", "golang:1.27@sha256:999")
		}, true},
		{"a build step in a published stage changes", func(s string) string {
			return replace(t, s, "-o /bin/app", "-trimpath -o /bin/app")
		}, true},
		{"a stage nothing published reads from changes", func(s string) string {
			return replace(t, s, "alpine:3.20", "alpine:3.21")
		}, false},
		{"a comment and a blank line change", func(s string) string {
			return replace(t, replace(t, s, "# header comment", "# a different header\n"), "RUN go mod download", "RUN go mod download\n\n")
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cur, ok := gitShow(dir, "HEAD:"+dockerfilePath)
			if !ok {
				t.Fatal("cannot read the Dockerfile at HEAD")
			}
			edited := tc.edit(string(cur))
			if edited == string(cur) {
				t.Fatal("the edit changed nothing, so the case asserts about an unmutated file")
			}
			sha := commit("fix: "+tc.name, edited)
			if got := dockerfileChanged(dir, sha, publishedImages); got != tc.want {
				t.Errorf("dockerfileChanged = %v, want %v", got, tc.want)
			}
		})
	}

	// A global ARG before the first FROM feeds `FROM ${BASE}`, so bumping it moves
	// a published image's base while every stage body stays textually identical.
	t.Run("a global ARG ahead of the first FROM counts", func(t *testing.T) {
		d := narrowRepo(t)
		before := "ARG BASE=gcr.io/distroless/static:nonroot@sha256:111\nFROM ${BASE} AS app\nCOPY app /app\n"
		commitFiles(t, d, "chore: base", map[string]string{dockerfilePath: before})
		sha := commitFiles(t, d, "fix: bump the base", map[string]string{dockerfilePath: replace(t, before, "sha256:111", "sha256:999")})
		if !dockerfileChanged(d, sha, []string{"app"}) {
			t.Error("a bumped global ARG read as unchanged")
		}
	})

	t.Run("an added Dockerfile counts, since nothing can be compared", func(t *testing.T) {
		d := narrowRepo(t)
		commitFiles(t, d, "chore: other", map[string]string{"README.md": "x\n"})
		sha := commitFiles(t, d, "feat: add it", map[string]string{dockerfilePath: dockerfileBase})
		if !dockerfileChanged(d, sha, publishedImages) {
			t.Error("an added Dockerfile read as unchanged")
		}
	})
}

// TestDockerfileDiffers pins the window reading -ships uses: the answer is the
// two ends compared, so an edit reverted inside the window is no change.
func TestDockerfileDiffers(t *testing.T) {
	dir := narrowRepo(t)
	bumped := replace(t, dockerfileBase, "distroless/static:nonroot@sha256:111", "distroless/static:nonroot@sha256:999")
	base := commitFiles(t, dir, "chore: base", map[string]string{dockerfilePath: dockerfileBase})
	comment := commitFiles(t, dir, "docs: reword", map[string]string{dockerfilePath: "# only a comment\n" + dockerfileBase})
	bump := commitFiles(t, dir, "build(deps): bump", map[string]string{dockerfilePath: bumped})
	revert := commitFiles(t, dir, "revert: bump", map[string]string{dockerfilePath: dockerfileBase})

	for _, tc := range []struct {
		name     string
		from, to string
		want     bool
	}{
		{"a comment-only edit", base, comment, false},
		{"a published base bump", comment, bump, true},
		{"a bump reverted inside the window", base, revert, false},
		{"a revision that does not resolve", "no-such-ref", bump, true},
	} {
		if got := dockerfileDiffers(dir, tc.from, tc.to, publishedImages); got != tc.want {
			t.Errorf("%s: dockerfileDiffers = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestShippedFilesCountsDockerfile covers the wiring: the Dockerfile is on the
// surface only for a commit that changed a published stage, and a surface with
// no Dockerfile rule behaves as before.
func TestShippedFilesCountsDockerfile(t *testing.T) {
	c := Commit{SHA: "abc", Files: []string{"Dockerfile", "docs/x.md"}}
	with := func(changed bool) Surface {
		return Surface{PkgDirs: map[string]bool{}, DockerfileChanged: func(string) bool { return changed }}
	}
	if got := with(true).ShippedFiles(c); len(got) != 1 || got[0] != "Dockerfile" {
		t.Errorf("a changed published stage shipped %v, want [Dockerfile]", got)
	}
	if got := with(false).ShippedFiles(c); len(got) != 0 {
		t.Errorf("an unchanged published stage shipped %v, want none", got)
	}
	if got := (Surface{PkgDirs: map[string]bool{}}).ShippedFiles(c); len(got) != 0 {
		t.Errorf("a surface with no Dockerfile rule shipped %v, want none", got)
	}
}

// TestClassifyDockerfileBump is the reported case: a base bump under a `feat`
// subject is Raising, not Withheld.
func TestClassifyDockerfileBump(t *testing.T) {
	c := Commit{SHA: "abc", Type: "feat", Files: []string{"Dockerfile"}}
	surface := Surface{PkgDirs: map[string]bool{}, DockerfileChanged: func(string) bool { return true }}
	r := Classify([]Commit{c}, surface, nil)
	if r.Floor != LevelMinor || len(r.Raising) != 1 || len(r.Withheld) != 0 {
		t.Errorf("floor=%v raising=%d withheld=%d, want minor/1/0", r.Floor, len(r.Raising), len(r.Withheld))
	}
}

func replace(t *testing.T, s, old, repl string) string {
	t.Helper()
	if !strings.Contains(s, old) {
		t.Fatalf("fixture lacks %q", old)
	}
	return strings.Replace(s, old, repl, 1)
}
