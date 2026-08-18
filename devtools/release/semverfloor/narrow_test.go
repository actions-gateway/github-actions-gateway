package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestGoShape covers the one distinction the narrowing rests on: a change the
// compiler sees versus one it does not. Getting the second case wrong
// under-reports the floor, which is the direction that breaks a consumer.
func TestGoShape(t *testing.T) {
	shape := func(t *testing.T, src string) string {
		t.Helper()
		s, ok := goShape([]byte(src))
		if !ok {
			t.Fatalf("goShape(%q) did not scan", src)
		}
		return s
	}

	t.Run("a comment rewrite leaves the shape alone", func(t *testing.T) {
		before := "package p\n\n// Count returns the count.\nfunc Count() int { return 1 }\n"
		after := "package p\n\n// Count reports how many, measured 2026-08-08 (Q705).\n// It never returns zero.\nfunc Count() int { return 1 } // trailing note\n"
		if shape(t, before) != shape(t, after) {
			t.Error("a comment-only edit changed the shape")
		}
	})

	t.Run("a block comment and reformatting leave it alone", func(t *testing.T) {
		before := "package p\n\nfunc Count() int {\n\treturn 1\n}\n"
		after := "package p\n\n/*\nCount is now documented\nacross several lines.\n*/\nfunc Count() int {\n\n\treturn 1\n\n}\n"
		if shape(t, before) != shape(t, after) {
			t.Error("a block comment and blank lines changed the shape")
		}
	})

	t.Run("one comment and one line of code still counts", func(t *testing.T) {
		before := "package p\n\n// Count returns the count.\nfunc Count() int { return 1 }\n"
		after := "package p\n\n// Count reports how many.\nfunc Count() int { return 2 }\n"
		if shape(t, before) == shape(t, after) {
			t.Error("a code change hidden alongside a comment change was missed")
		}
	})

	t.Run("comment-shaped text inside a string is code", func(t *testing.T) {
		before := "package p\n\nconst Help = `\n// usage: old\n`\n"
		after := "package p\n\nconst Help = `\n// usage: new\n`\n"
		if shape(t, before) == shape(t, after) {
			t.Error("a raw string literal was read as a comment")
		}
	})

	t.Run("a directive is code however it reads", func(t *testing.T) {
		before := "//go:build linux\n\npackage p\n"
		after := "//go:build linux || darwin\n\npackage p\n"
		if shape(t, before) == shape(t, after) {
			t.Error("//go:build was dropped as a comment; it decides whether the file compiles")
		}
	})

	t.Run("source that does not scan is not narrowable", func(t *testing.T) {
		if _, ok := goShape([]byte("package p\n\nconst S = \"unterminated\n")); ok {
			t.Error("goShape reported a shape for source it could not scan")
		}
	})
}

// narrowRepo builds a throwaway repository holding real Go source, so the
// `<sha>^:<path>` plumbing is exercised rather than assumed. Identity is passed
// per-command so the test cannot depend on, or write to, the developer's config.
func narrowRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitRun(t, dir, "init", "-q", "-b", "main")
	// Q820: no detached maintenance racing the next command in a fixture repo.
	gitRun(t, dir, "config", "maintenance.auto", "false")
	return dir
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	//nolint:gosec // G204: this test's own git verbs against its own temp repo.
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// commitFiles writes files and commits them, returning the commit SHA.
func commitFiles(t *testing.T, dir, subject string, files map[string]string) string {
	t.Helper()
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		gitRun(t, dir, "add", name)
	}
	gitRun(t, dir, "commit", "-q", "-m", subject)
	head := exec.Command("git", "rev-parse", "HEAD")
	head.Dir = dir
	out, err := head.Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func TestGitNarrower(t *testing.T) {
	dir := narrowRepo(t)
	writeCommit := func(subject string, files map[string]string) string {
		return commitFiles(t, dir, subject, files)
	}
	writeCommit("feat: base", map[string]string{
		"pkg/a.go":             "package pkg\n\n// Count returns the count.\nfunc Count() int { return 1 }\n",
		"pkg/b.go":             "package pkg\n\nfunc Name() string { return \"a\" }\n",
		"charts/c/values.yaml": "# a comment\nreplicas: 1\n",
	})

	n := gitNarrower{root: dir}

	t.Run("comment-only across every shipped Go file narrows to nothing", func(t *testing.T) {
		sha := writeCommit("feat: document it", map[string]string{
			"pkg/a.go": "package pkg\n\n// Count reports how many. It never returns zero.\nfunc Count() int { return 1 }\n",
			"pkg/b.go": "package pkg\n\n// Name is the fixture's name.\nfunc Name() string { return \"a\" }\n",
		})
		if got := n.Substantive(Commit{SHA: sha}, []string{"pkg/a.go", "pkg/b.go"}); len(got) != 0 {
			t.Errorf("substantive = %v, want none", got)
		}
	})

	t.Run("one comment and one line of code still ships", func(t *testing.T) {
		sha := writeCommit("fix: bound it", map[string]string{
			"pkg/a.go": "package pkg\n\n// Count reports how many, now bounded.\nfunc Count() int { return 2 }\n",
			"pkg/b.go": "package pkg\n\n// Name is still the fixture's name.\nfunc Name() string { return \"a\" }\n",
		})
		got := n.Substantive(Commit{SHA: sha}, []string{"pkg/a.go", "pkg/b.go"})
		if len(got) != 1 || got[0] != "pkg/a.go" {
			t.Errorf("substantive = %v, want [pkg/a.go] — the code change must survive the comment change beside it", got)
		}
	})

	t.Run("a chart file is never narrowed", func(t *testing.T) {
		sha := writeCommit("docs: comment the chart", map[string]string{
			"charts/c/values.yaml": "# a longer comment\nreplicas: 1\n",
		})
		if got := n.Substantive(Commit{SHA: sha}, []string{"charts/c/values.yaml"}); len(got) != 1 {
			t.Errorf("substantive = %v, want the chart file kept — only Go source is read", got)
		}
	})

	t.Run("an added file ships", func(t *testing.T) {
		sha := writeCommit("feat: add a file", map[string]string{
			"pkg/c.go": "package pkg\n\n// Zero is a constant.\nconst Zero = 0\n",
		})
		if got := n.Substantive(Commit{SHA: sha}, []string{"pkg/c.go"}); len(got) != 1 {
			t.Errorf("substantive = %v, want the added file kept", got)
		}
	})
}

// stubNarrower answers from a fixed set of substantive files, so Classify's
// bucketing can be tested without a repository.
type stubNarrower map[string]bool

func (s stubNarrower) Substantive(_ Commit, shipped []string) []string {
	var out []string
	for _, f := range shipped {
		if s[f] {
			out = append(out, f)
		}
	}
	return out
}

func TestClassifyNarrowsCommentOnlyCommits(t *testing.T) {
	s := testSurface()
	commits := []Commit{
		commit("aaaaaaaa", "feat(agc): document the gauge", "cmd/agc/internal/listener/job.go"),
		commit("bbbbbbbb", "fix(agc): bound the retry", "cmd/agc/internal/listener/job.go", "api/v2beta1/runnerset_types.go"),
	}
	// Only the fix's api file carries a change the compiler sees.
	r := Classify(commits, s, stubNarrower{"api/v2beta1/runnerset_types.go": true})

	if r.Floor != LevelPatch {
		t.Errorf("floor = %v, want patch — the feat's shipped diff is comment-only", r.Floor)
	}
	if len(r.CommentOnly) != 1 || r.CommentOnly[0].Commit.SHA != "aaaaaaaa" {
		t.Fatalf("commentOnly = %v, want the feat", r.CommentOnly)
	}
	if len(r.CommentOnly[0].Shipped) != 1 {
		t.Errorf("evidence = %v, want the shipped path named so the narrowing is checkable", r.CommentOnly[0].Shipped)
	}
	if len(r.Raising) != 1 {
		t.Fatalf("raising = %d, want 1", len(r.Raising))
	}
	if len(r.Raising[0].Shipped) != 1 || r.Raising[0].Shipped[0] != "api/v2beta1/runnerset_types.go" {
		t.Errorf("evidence = %v, want only the file that actually changed", r.Raising[0].Shipped)
	}
}

// A breaking marker is deliberately outside the narrowing: a missed major is the
// costliest thing this tool can drop.
func TestClassifyDoesNotNarrowBreakingMarkers(t *testing.T) {
	r := Classify([]Commit{
		commit("cccccccc", "feat(agc)!: drop a field", "cmd/agc/internal/listener/job.go"),
	}, testSurface(), stubNarrower{})

	if len(r.Unresolved) != 1 {
		t.Errorf("unresolved = %d, want 1 even though the shipped diff narrows to nothing", len(r.Unresolved))
	}
	if len(r.CommentOnly) != 1 {
		t.Errorf("commentOnly = %d, want 1 — the floor half still narrows", len(r.CommentOnly))
	}
}
