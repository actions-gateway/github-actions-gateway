package main

import (
	"bytes"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// The published CRD schema is the one part of the major question that can be
// measured rather than asserted.
//
// A breaking marker is major only if the FROM tag had already published the
// surface it changed, and the committed controller-gen output answers that
// directly: every property the tag published is in its CRD schemas. A field
// added and reshaped inside the window is absent at FROM, so it broke nothing.
// That is what all three markers in v1.2.0..v1.3.0 were — `capacityGate` and
// `windowStartTime` are both absent at v1.2.0 and present at v1.3.0 — and why
// v1.3.0 shipped as a minor.
//
// This narrows the question; it does not answer it. Property *names* are all it
// compares, so a published field whose type changed, whose enum narrowed, or
// whose default moved reads as unchanged here. Nor does it see the other
// operator-visible contracts — metric names, condition reasons, Event reasons,
// chart values, env tunables — which release.md § Diff every surface enumerates
// separately. So a clean result means "no published property was removed",
// never "nothing broke".
//
// The chart's copies are Helm templates and will not parse as YAML; the
// controller-gen output under */config/crd/ is the same schema untemplated, and
// is what these read.

// schemaNode is the part of an OpenAPI v3 schema that carries nested names.
type schemaNode struct {
	Properties map[string]*schemaNode `yaml:"properties"`
	Items      *schemaNode            `yaml:"items"`
}

type crdDoc struct {
	Kind string `yaml:"kind"`
	Spec struct {
		Names struct {
			Kind string `yaml:"kind"`
		} `yaml:"names"`
		Versions []struct {
			Name   string `yaml:"name"`
			Schema struct {
				OpenAPIV3Schema *schemaNode `yaml:"openAPIV3Schema"`
			} `yaml:"schema"`
		} `yaml:"versions"`
	} `yaml:"spec"`
}

// CRDDelta is the change in published CRD property paths across a window.
type CRDDelta struct {
	Added   []string
	Removed []string
	// FromSeen and ToSeen are the schema files actually read at each end, so an
	// empty delta stays distinguishable from a delta nothing was read for.
	FromSeen int
	ToSeen   int
}

// walk collects dotted property paths under a schema node.
func walk(n *schemaNode, prefix string, out map[string]bool) {
	if n == nil {
		return
	}
	for name, child := range n.Properties {
		p := prefix + "." + name
		out[p] = true
		walk(child, p, out)
	}
	// An array's element schema names the same fields as its parent path: the
	// index is not part of the contract.
	walk(n.Items, prefix, out)
}

// propertiesFromYAML adds every property path in a (possibly multi-document)
// CRD file to props, and reports whether it held a CRD at all. A file that is
// valid YAML but holds no CustomResourceDefinition is not an error — the CRD
// trees carry kustomization files too — but it does not count as read, so an
// empty surface stays distinguishable from an unread one.
func propertiesFromYAML(blob []byte, props map[string]bool) (bool, error) {
	dec := yaml.NewDecoder(bytes.NewReader(blob))
	parsed := false
	for {
		var doc crdDoc
		err := dec.Decode(&doc)
		if err == io.EOF {
			return parsed, nil
		}
		if err != nil {
			return parsed, err
		}
		if doc.Kind != "CustomResourceDefinition" {
			continue
		}
		parsed = true
		for _, v := range doc.Spec.Versions {
			walk(v.Schema.OpenAPIV3Schema, doc.Spec.Names.Kind+"."+v.Name, props)
		}
	}
}

// crdFilesAt lists the controller-gen CRD outputs committed at a ref.
func crdFilesAt(ref string) ([]string, error) {
	//nolint:gosec // G204: fixed git verbs plus a ref this program resolved.
	out, err := exec.Command("git", "ls-tree", "-r", "--name-only", ref).Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-tree %s: %w", ref, err)
	}
	var files []string
	for _, f := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.Contains(f, "config/crd/") && strings.HasSuffix(f, ".yaml") {
			files = append(files, f)
		}
	}
	sort.Strings(files)
	return files, nil
}

// crdPropertiesAt returns every published CRD property path at a ref, and the
// number of schema files it read.
func crdPropertiesAt(ref string) (map[string]bool, int, error) {
	files, err := crdFilesAt(ref)
	if err != nil {
		return nil, 0, err
	}
	props := map[string]bool{}
	read := 0
	for _, f := range files {
		// Braces around ref: `$ref:path` would otherwise be read as a shell
		// modifier by some callers, and git needs the exact `ref:path` form.
		//nolint:gosec // G204: a ref plus a path git itself listed under that ref.
		blob, err := exec.Command("git", "show", fmt.Sprintf("%s:%s", ref, f)).Output()
		if err != nil {
			continue // a path that exists in the listing but not as a blob
		}
		parsed, err := propertiesFromYAML(blob, props)
		if err != nil {
			return nil, 0, fmt.Errorf("parse %s at %s: %w", f, ref, err)
		}
		if parsed {
			read++
		}
	}
	return props, read, nil
}

// CRDSurfaceDelta compares the published CRD property paths across a window.
func CRDSurfaceDelta(from, to string) (CRDDelta, error) {
	var d CRDDelta
	fromProps, fromSeen, err := crdPropertiesAt(from)
	if err != nil {
		return d, err
	}
	toProps, toSeen, err := crdPropertiesAt(to)
	if err != nil {
		return d, err
	}
	d.FromSeen, d.ToSeen = fromSeen, toSeen
	for p := range toProps {
		if !fromProps[p] {
			d.Added = append(d.Added, p)
		}
	}
	for p := range fromProps {
		if !toProps[p] {
			d.Removed = append(d.Removed, p)
		}
	}
	sort.Strings(d.Added)
	sort.Strings(d.Removed)
	return d, nil
}
