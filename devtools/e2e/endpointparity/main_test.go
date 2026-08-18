package main

import (
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// deriveSource parses one synthetic file and folds its request sites, which is
// how every fold case below states the shape it is about.
func deriveSource(t *testing.T, src string) []callSite {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "src.go")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	files, err := parseRoots(fset, []string{dir})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return deriveSites(files, fset)
}

func TestFoldShapes(t *testing.T) {
	// Each case is one of the four shapes the package comment names, written the
	// way the tree writes it. A fold that stopped recognising a shape would stop
	// demanding its endpoint, so every one is pinned to its template here.
	cases := []struct {
		name     string
		body     string
		method   string
		template string
	}{
		{
			name:     "literal concatenation",
			body:     `req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.apiBase+"/actions/runner-registration", nil)`,
			method:   "POST",
			template: "{}/actions/runner-registration",
		},
		{
			name:     "format string",
			body:     `u := fmt.Sprintf("%s/repos/%s/%s/actions/runs/%s", base, owner, repo, id)` + "\n\treq, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)",
			method:   "GET",
			template: "{}/repos/{}/{}/actions/runs/{}",
		},
		{
			name:     "integer verb keeps its own hole",
			body:     `u := fmt.Sprintf("%s/actions/runners/%d", prefix, id)` + "\n\treq, _ := http.NewRequestWithContext(ctx, http.MethodDelete, u, nil)",
			method:   "DELETE",
			template: "{}/actions/runners/{d}",
		},
		{
			name:     "opaque URL composes nothing",
			body:     `req, _ := http.NewRequestWithContext(ctx, method, u, nil)`,
			method:   "*",
			template: "{}",
		},
		{
			name:     "NewRequest without a context",
			body:     `req, _ := http.NewRequest("GET", base+"/repos/x/actions/runners", nil)`,
			method:   "GET",
			template: "{}/repos/x/actions/runners",
		},
		{
			name:     "reassigned local is not resolved",
			body:     "u := base + \"/one\"\n\tu = base + \"/two\"\n\treq, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)",
			method:   "GET",
			template: "{}",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sites := deriveSource(t, "package p\n\nfunc f() {\n\t"+tc.body+"\n\t_ = req\n}\n")
			if len(sites) != 1 {
				t.Fatalf("want 1 site, got %d", len(sites))
			}
			if sites[0].Method != tc.method {
				t.Errorf("method = %q, want %q", sites[0].Method, tc.method)
			}
			if sites[0].Template != tc.template {
				t.Errorf("template = %q, want %q", sites[0].Template, tc.template)
			}
		})
	}
}

func TestFoldResolvesCalleeBranches(t *testing.T) {
	// runnerAPIPrefix's two scopes are the live case: one endpoint reachable at
	// two paths, and the venue must serve whichever the caller is configured for.
	src := `package p

func prefix() string {
	if repoScoped {
		return base + "/repos/" + path
	}
	return base + "/orgs/" + path
}

func f() {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, prefix()+"/actions/runners/generate-jitconfig", nil)
	_ = req
}
`
	sites := deriveSource(t, src)
	if len(sites) != 1 {
		t.Fatalf("want 1 site, got %d", len(sites))
	}
	got := expandAlternatives(sites[0].Template)
	want := []string{
		"{}/orgs/{}/actions/runners/generate-jitconfig",
		"{}/repos/{}/actions/runners/generate-jitconfig",
	}
	if len(got) != len(want) {
		t.Fatalf("expanded to %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("alternative %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestFoldResolvesMultiValueAssignment(t *testing.T) {
	// `path, err := helper()` is how every path-returning helper here is called.
	// Before the fold understood it, scaleset's registration-token hop folded to
	// a bare hole and quietly stopped being demanded.
	src := `package p

func tokenPath() (string, error) {
	if bad {
		return "", errFoo
	}
	return prefix + "/actions/runners/registration-token", nil
}

func f() {
	path, err := tokenPath()
	_ = err
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, apiBase+path, nil)
	_ = req
}
`
	sites := deriveSource(t, src)
	if len(sites) != 1 {
		t.Fatalf("want 1 site, got %d", len(sites))
	}
	const want = "{}{}/actions/runners/registration-token"
	if sites[0].Template != want {
		t.Errorf("template = %q, want %q", sites[0].Template, want)
	}
}

func TestTestFilesAreNotDerived(t *testing.T) {
	dir := t.TempDir()
	src := "package p\n\nfunc f() {\n\treq, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+\"/repos/x/actions/invented\", nil)\n\t_ = req\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "a_test.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	files, err := parseRoots(fset, []string{dir})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatalf("a _test.go file was parsed; a stub URL in a test is not an endpoint the AGC calls")
	}
}

func TestComposesPath(t *testing.T) {
	cases := []struct {
		template string
		want     bool
	}{
		{"{}/repos/{}/actions/runs/{}", true},
		{"{}", false},
		{"{}{}", false},
		// A query separator is a literal, but it is not a path the source wrote.
		{"{}?api-version={}", false},
	}
	for _, tc := range cases {
		got := callSite{Template: tc.template}.composesPath()
		if got != tc.want {
			t.Errorf("composesPath(%q) = %v, want %v", tc.template, got, tc.want)
		}
	}
}

func TestProbePath(t *testing.T) {
	cases := map[string]string{
		"{}/repos/{}/{}/actions/runs/{}": "/repos/x/x/actions/runs/x",
		"{}/actions/runners/{d}":         "/actions/runners/1",
		"{}{}/actions/runners?name={}":   "/x/actions/runners?name=x",
	}
	for in, want := range cases {
		if got := probePath(in); got != want {
			t.Errorf("probePath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTemplateFromFormat(t *testing.T) {
	cases := map[string]string{
		"%s/repos/%s/actions/runs/%s": "{}/repos/{}/actions/runs/{}",
		"%s/actions/runners/%d":       "{}/actions/runners/{d}",
		"%s/pct/%d%%":                 "{}/pct/{d}%",
		"%-8s/x":                      "{}/x",
	}
	for in, want := range cases {
		if got := templateFromFormat(in); got != want {
			t.Errorf("templateFromFormat(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestCheckDerivationBothDirections is the check that keeps the gate honest
// about itself: a site the fold stopped resolving must be reported rather than
// dropped, and a pin that has outlived its site must be reported too. Both
// failures are silent otherwise — an unreported site simply stops being
// demanded, which leaves the gate green in exactly the state it exists to fail.
func TestCheckDerivationBothDirections(t *testing.T) {
	composed := callSite{Template: "{}/repos/{}/actions/runs/{}", File: "a.go", Func: "composes", Line: 10}
	opaque := callSite{Template: "{}", File: "b.go", Func: "opaque", Line: 20}

	t.Run("unpinned opaque site is reported", func(t *testing.T) {
		restore := swapPins(t, nil)
		defer restore()
		findings := checkDerivation([]callSite{composed, opaque})
		if len(findings) != 1 || !strings.Contains(findings[0], "b.go:20") {
			t.Fatalf("want one finding naming b.go:20, got %v", findings)
		}
	})

	t.Run("pinned opaque site is silent", func(t *testing.T) {
		restore := swapPins(t, map[string]string{"b.go:opaque": "the caller supplies it"})
		defer restore()
		if findings := checkDerivation([]callSite{composed, opaque}); len(findings) != 0 {
			t.Fatalf("want no findings, got %v", findings)
		}
	})

	t.Run("pin on a site that composes a path is reported", func(t *testing.T) {
		restore := swapPins(t, map[string]string{"a.go:composes": "wrong"})
		defer restore()
		findings := checkDerivation([]callSite{composed})
		if len(findings) != 1 || !strings.Contains(findings[0], "drop the pin") {
			t.Fatalf("want one finding telling the pin to go, got %v", findings)
		}
	})

	t.Run("pin naming no site in a walked file is reported", func(t *testing.T) {
		restore := swapPins(t, map[string]string{"a.go:vanished": "stale"})
		defer restore()
		findings := checkDerivation([]callSite{composed})
		if len(findings) != 1 || !strings.Contains(findings[0], "a.go:vanished") {
			t.Fatalf("want one finding naming the stale pin, got %v", findings)
		}
	})

	// The roots are an argument. A run scoped to one of them leaves the others
	// unparsed, and calling their pins stale would be a finding about the
	// invocation — noisy enough to bury the real ones.
	t.Run("pin in a file the walk never reached is silent", func(t *testing.T) {
		restore := swapPins(t, map[string]string{"elsewhere.go:untouched": "not in these roots"})
		defer restore()
		if findings := checkDerivation([]callSite{composed}); len(findings) != 0 {
			t.Fatalf("want no findings, got %v", findings)
		}
	})
}

func swapPins(t *testing.T, pins map[string]string) func() {
	t.Helper()
	saved := pinnedSites
	pinnedSites = pins
	return func() { pinnedSites = saved }
}

func TestEndpointsDedupeAndNameEveryCaller(t *testing.T) {
	a := callSite{Method: "GET", Template: "{}/repos/{}/actions/runs/{}", File: "a.go", Func: "one", Line: 1}
	b := callSite{Method: "GET", Template: "{}/repos/{}/actions/runs/{}", File: "b.go", Func: "two", Line: 2}
	eps := endpoints([]callSite{a, b})
	if len(eps) != 1 {
		t.Fatalf("want 1 endpoint, got %d", len(eps))
	}
	if got := callers(eps[0].sites); got != "a.go:1, b.go:2" {
		t.Errorf("callers = %q, want both sites named", got)
	}
}
