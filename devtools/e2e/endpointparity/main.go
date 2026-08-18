// Command endpointparity fails when the AGC calls a GitHub REST endpoint the
// e2e fake does not serve (Q871).
//
// Q811 added a run read to the eviction path. test/fakegithub served two paths
// on that prefix and 404'd the rest, so the drained-worker spec failed thirteen
// merge-queue entries while the PR that introduced the call stayed green. The
// venue and the code it exercises had drifted apart with nothing comparing them:
// the fake's endpoints were a list somebody maintained by hand, and a list is
// only ever as fresh as the last person who remembered it.
//
// So neither side is a list here. The caller side is folded out of the AGC's own
// source at every http.NewRequest site (derive.go); the fake side is the running
// fake answering a probe for each derived path (probe.go). Both move when the
// code moves, which is the property a hand-kept inventory cannot have.
//
// Two checks:
//
//	parity      every endpoint the source composes, the fake dispatches
//	derivation  every request site either composes a path or is pinned as one
//	            whose URL the caller was handed
//
// derivation is the one that reports the tool's own blind spot rather than the
// venue's, and it is the load-bearing half: parity can only demand endpoints it
// derived, so a call shape the fold stops recognising would quietly stop being
// demanded and leave the gate green. Pins are the escape hatch, and they are
// checked in both directions — a pin naming a site that now composes a path, or
// no site at all, fails too.
package main

import (
	"fmt"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// pinnedSite is a request site whose URL the caller was handed whole, so there
// is no AGC-composed path in it to hold the fake to. Every entry says who
// supplies the URL, because that is the claim being made: a URL the fake itself
// minted, or one an operator configured, cannot be out of parity with the fake.
//
// This is the one hand-kept input. It can only ever silence a demand, never
// manufacture one — and it cannot silence one quietly, because a pin whose site
// has started composing a path is itself a failure.
var pinnedSites = map[string]string{
	"broker/client.go:newJSONRequest": "the broker root is GITHUB_BROKER_URL and the path under it is the caller's argument; broker's own suite holds those against brokerstub",

	"scaleset/client.go:serviceRequest": "the Actions Service root is the admin URL the fake minted in the RemoteAuth hop, and the route under it is the caller's argument",
	"scaleset/client.go:GetMessage":     "the message-queue URL comes whole from the session the fake minted",
	"scaleset/client.go:DeleteMessage":  "the message-queue URL comes whole from the session the fake minted",

	"githubapp/runner_auth.go:FetchRunnerOAuthToken": "AuthorizationURL comes from the JIT config blob the fake generated",

	"githubapp/vaultsigner/vaultsigner.go:do": "Vault, not GitHub: the fake serves no part of this path",
}

func main() {
	args := os.Args[1:]
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: endpointparity <fakegithub-binary> <src-root>...")
		os.Exit(2)
	}
	bin, roots := args[0], args[1:]

	fset := token.NewFileSet()
	files, err := parseRoots(fset, roots)
	if err != nil {
		fmt.Fprintf(os.Stderr, "endpointparity: %v\n", err)
		os.Exit(2)
	}
	if len(files) == 0 {
		fmt.Fprintf(os.Stderr, "endpointparity: no Go source under %s\n", strings.Join(roots, " "))
		os.Exit(2)
	}

	sites := deriveSites(files, fset)
	findings := checkDerivation(sites)

	parity, err := checkParity(bin, sites)
	if err != nil {
		fmt.Fprintf(os.Stderr, "endpointparity: %v\n", err)
		os.Exit(2)
	}
	findings = append(findings, parity...)

	if len(findings) > 0 {
		for _, f := range findings {
			fmt.Fprintln(os.Stderr, f)
		}
		fmt.Fprintf(os.Stderr, "\nendpointparity: %d finding(s) across %d request site(s)\n",
			len(findings), len(sites))
		os.Exit(1)
	}
	fmt.Printf("endpointparity: %d request site(s), %d endpoint(s) served by the e2e fake\n",
		len(sites), len(endpoints(sites)))
}

// checkDerivation reconciles the request sites against the pins, both ways.
//
// A pin is judged stale only against a file the walk actually parsed. The roots
// are an argument, so a run scoped to one of them would otherwise report every
// pin outside it as stale — a finding about the invocation rather than about the
// tree, and one that would drown the real ones.
func checkDerivation(sites []callSite) []string {
	var findings []string
	matched := map[string]bool{}
	walked := map[string]bool{}
	for _, s := range sites {
		walked[s.File] = true
	}
	for _, s := range sites {
		pinned, isPinned := pinnedSites[s.ID()]
		switch {
		case s.composesPath() && isPinned:
			matched[s.ID()] = true
			findings = append(findings, fmt.Sprintf(
				"derivation: %s:%d %s composes %q but is pinned as caller-supplied (%s); drop the pin so the endpoint is demanded",
				s.File, s.Line, s.Func, s.Template, pinned))
		case !s.composesPath() && !isPinned:
			findings = append(findings, fmt.Sprintf(
				"derivation: %s:%d %s builds a request from a URL this walk could not resolve to a path. "+
					"If the caller composes the path, teach derive.go the shape; if it was handed the URL, pin it in pinnedSites with who supplies it",
				s.File, s.Line, s.Func))
		case isPinned:
			matched[s.ID()] = true
		}
	}
	var stale []string
	for id := range pinnedSites {
		if matched[id] {
			continue
		}
		if file, _, ok := strings.Cut(id, ":"); ok && walked[file] {
			stale = append(stale, id)
		}
	}
	sort.Strings(stale)
	for _, id := range stale {
		findings = append(findings, fmt.Sprintf(
			"derivation: pinnedSites names %s, which is no longer a request site; remove the pin", id))
	}
	return findings
}

// endpoints reduces the sites to the distinct method-and-path pairs to probe.
// Two call sites reaching the same endpoint probe once, and both are named if
// it fails.
func endpoints(sites []callSite) []endpoint {
	seen := map[string]*endpoint{}
	for _, s := range sites {
		if !s.composesPath() {
			continue
		}
		for _, t := range expandAlternatives(s.Template) {
			path := probePath(t)
			key := s.Method + " " + path
			if e, ok := seen[key]; ok {
				e.sites = append(e.sites, s)
				continue
			}
			seen[key] = &endpoint{method: s.Method, path: path, sites: []callSite{s}}
		}
	}
	out := make([]endpoint, 0, len(seen))
	for _, e := range seen {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].path != out[j].path {
			return out[i].path < out[j].path
		}
		return out[i].method < out[j].method
	})
	return out
}

// endpoint is one method-and-path the fake is held to, with the call sites that
// reach it so a failure names the code that would break.
type endpoint struct {
	method string
	path   string
	sites  []callSite
}

// checkParity starts the fake and probes every derived endpoint against it.
func checkParity(bin string, sites []callSite) ([]string, error) {
	eps := endpoints(sites)
	if len(eps) == 0 {
		return nil, fmt.Errorf("no endpoint composed a path; the derivation is broken, not the venue")
	}

	client := probeClient()
	base, stop, err := startFake(bin, filepath.Dir(bin), client)
	if err != nil {
		return nil, err
	}
	defer stop()

	if err := checkMarker(client, base); err != nil {
		return nil, err
	}

	var findings []string
	for _, e := range eps {
		ok, tried, err := served(client, base, e.method, e.path)
		if err != nil {
			return nil, err
		}
		if ok {
			continue
		}
		findings = append(findings, fmt.Sprintf(
			"parity: the e2e fake serves no %s %s (tried %s), called from %s",
			e.method, e.path, strings.Join(tried, ", "), callers(e.sites)))
	}
	return findings, nil
}

// callers renders the source locations that reach an endpoint.
func callers(sites []callSite) string {
	var out []string
	for _, s := range sites {
		out = append(out, fmt.Sprintf("%s:%d", s.File, s.Line))
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}
