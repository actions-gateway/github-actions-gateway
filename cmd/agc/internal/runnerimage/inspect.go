package runnerimage

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
)

const (
	// runnerDepsFile is the .NET dependency manifest actions/runner ships beside its
	// binaries, the only version record in the image. The same file the wrapper reads
	// (cmd/worker); matched by suffix here because a custom image may install the
	// runner anywhere.
	runnerDepsFile = "bin/Runner.Listener.deps.json"

	// runnerListenerLib is the deps.json library key prefix, "Runner.Listener/<version>".
	runnerListenerLib = "Runner.Listener/"

	// maxDepsFileBytes caps the manifest read (~110 KB at 2.335.1), so a tenant image
	// cannot make the AGC buffer something huge under this name.
	maxDepsFileBytes = 4 << 20

	// maxImageBytes caps the compressed bytes one inspection streams across all
	// layers. The official image is 544 MB and an inspection reads ~114 MB of it
	// (measured 2026-09-07); the cap bounds a tenant image built to be enormous.
	maxImageBytes = 2 << 30

	// whiteoutPrefix and opaqueWhiteout are the OCI layer markers for a path a layer
	// deletes from the layers below it.
	whiteoutPrefix = ".wh."
	opaqueWhiteout = ".wh..wh..opq"
)

// Reading is what one inspection of an image established.
type Reading struct {
	// Digest addresses the manifest the reference resolved to — the index for a
	// multi-arch reference — which is what a pin of this reference would name.
	Digest string
	// Platform is the os/arch whose layers were read.
	Platform string
	// Version is the runner version deps.json names; empty when Found is false.
	Version string
	// Found reports whether any layer carries the runner's deps.json. False is a
	// complete reading: the image is not actions/runner-derived where the runner
	// layout puts the file.
	Found bool
}

// Inspect reads the runner version out of image in its registry. It fetches the
// manifest (preferring linux/amd64 from an index), then streams the layers from the
// topmost down and stops at the first deps.json that no higher layer deleted, which
// is the file the container would see. Nothing is written to disk. hc must carry no
// overall Timeout — the stream is long and ctx bounds it — which is why there is no
// default client here.
func Inspect(ctx context.Context, hc *http.Client, image string, creds Credentials) (Reading, error) {
	reading, _, err := inspect(ctx, hc, image, creds, "")
	return reading, err
}

// inspect is Inspect with a short-circuit: when the reference resolves to
// knownDigest, the layers are not streamed again and unchanged reports true with an
// empty reading, since the caller already holds the reading for that digest.
func inspect(ctx context.Context, hc *http.Client, image string, creds Credentials, knownDigest string) (reading Reading, unchanged bool, err error) {
	ref, err := ParseReference(image)
	if err != nil {
		return Reading{}, false, err
	}
	if hc == nil {
		return Reading{}, false, errors.New("no HTTP client configured for the registry read")
	}
	c := &client{http: hc, ref: ref}
	c.cred, _ = creds.lookup(ref.Registry)

	m, digest, err := c.manifest(ctx, ref.manifestRef())
	if err != nil {
		return Reading{}, false, err
	}
	if knownDigest != "" && digest == knownDigest {
		return Reading{}, true, nil
	}
	reading = Reading{Digest: digest}
	if len(m.Manifests) > 0 {
		d, ok := selectPlatform(m.Manifests)
		if !ok {
			return reading, false, fmt.Errorf("%s: the index lists no linux platform", ref)
		}
		reading.Platform = d.Platform.OS + "/" + d.Platform.Architecture
		if m, _, err = c.manifest(ctx, d.Digest); err != nil {
			return reading, false, err
		}
	}
	if len(m.Layers) == 0 {
		return reading, false, fmt.Errorf("%s: the manifest lists no layers", ref)
	}

	version, found, err := scanLayers(ctx, c, m.Layers)
	if err != nil {
		return reading, false, err
	}
	reading.Version, reading.Found = version, found
	return reading, false, nil
}

// selectPlatform picks the manifest to read from an index: linux/amd64 when present,
// else the first linux entry. Attestation manifests carry an unknown/unknown
// platform and are skipped.
func selectPlatform(manifests []descriptor) (descriptor, bool) {
	var first *descriptor
	for i := range manifests {
		d := &manifests[i]
		if d.Platform == nil || d.Platform.OS != "linux" {
			continue
		}
		if d.Platform.Architecture == "amd64" {
			return *d, true
		}
		if first == nil {
			first = d
		}
	}
	if first == nil {
		return descriptor{}, false
	}
	return *first, true
}

// hidden records what higher layers have deleted, so a deps.json in a lower layer
// that the container would never see is not read as the running version.
type hidden struct {
	files map[string]struct{}
	dirs  []string
}

func (h *hidden) hides(name string) bool {
	if _, ok := h.files[name]; ok {
		return true
	}
	for _, d := range h.dirs {
		if strings.HasPrefix(name, d+"/") {
			return true
		}
	}
	return false
}

// scanLayers walks the layers topmost-first, streaming each and stopping at the
// first surviving deps.json. A layer without the file is streamed to its end, since
// a tar reveals its members only in order.
func scanLayers(ctx context.Context, c *client, layers []descriptor) (string, bool, error) {
	budget := &countingReader{limit: maxImageBytes}
	h := &hidden{files: map[string]struct{}{}}
	for i := len(layers) - 1; i >= 0; i-- {
		version, found, err := scanLayer(ctx, c, layers[i], budget, h)
		if err != nil {
			return "", false, fmt.Errorf("layer %d (%s): %w", i, layers[i].Digest, err)
		}
		if found {
			return version, true, nil
		}
	}
	return "", false, nil
}

func scanLayer(ctx context.Context, c *client, layer descriptor, budget *countingReader, h *hidden) (string, bool, error) {
	body, err := c.blob(ctx, layer.Digest)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = body.Close() }()
	budget.r = body

	var stream io.Reader
	switch layer.MediaType {
	case mediaOCILayerGzip, mediaDockerLayerGzip:
		gz, err := gzip.NewReader(budget)
		if err != nil {
			return "", false, fmt.Errorf("gunzip: %w", err)
		}
		defer func() { _ = gz.Close() }()
		stream = gz
	case mediaOCILayerTar:
		stream = budget
	case mediaOCILayerZstd:
		return "", false, fmt.Errorf("zstd-compressed layers are not supported")
	default:
		return "", false, fmt.Errorf("unsupported layer media type %q", layer.MediaType)
	}

	// A whiteout deletes from the layers below, never from its own: Docker emits an
	// opaque marker beside the files that replace the directory in one layer, so the
	// layer's whiteouts are collected here and merged into h once it is fully read.
	deletes := hidden{files: map[string]struct{}{}}
	versions := map[string]struct{}{}
	tr := tar.NewReader(stream)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if budget.exhausted() {
				return "", false, fmt.Errorf("image exceeds the %d-byte inspection budget", maxImageBytes)
			}
			return "", false, fmt.Errorf("read tar: %w", err)
		}
		name := path.Clean(strings.TrimPrefix(hdr.Name, "/"))
		dir, base := path.Split(name)
		dir = strings.TrimSuffix(dir, "/")
		switch {
		case base == opaqueWhiteout:
			deletes.dirs = append(deletes.dirs, dir)
			continue
		case strings.HasPrefix(base, whiteoutPrefix):
			deletes.files[path.Join(dir, strings.TrimPrefix(base, whiteoutPrefix))] = struct{}{}
			continue
		}
		if hdr.Typeflag != tar.TypeReg || !strings.HasSuffix(name, runnerDepsFile) || h.hides(name) {
			continue
		}
		v, err := parseDepsVersion(io.LimitReader(tr, maxDepsFileBytes))
		if err != nil {
			return "", false, fmt.Errorf("%s: %w", name, err)
		}
		versions[v] = struct{}{}
	}
	h.dirs = append(h.dirs, deletes.dirs...)
	for f := range deletes.files {
		h.files[f] = struct{}{}
	}
	switch len(versions) {
	case 0:
		return "", false, nil
	case 1:
		for v := range versions {
			return v, true, nil
		}
	}
	list := make([]string, 0, len(versions))
	for v := range versions {
		list = append(list, v)
	}
	return "", false, fmt.Errorf("the layer carries %d runner installs of different versions: %s", len(versions), strings.Join(list, ", "))
}

// parseDepsVersion reads the runner version off deps.json's root library key. One
// entry per target framework, each naming the same library; a disagreement is
// reported rather than sampled, since map order is random.
func parseDepsVersion(r io.Reader) (string, error) {
	var doc struct {
		Targets map[string]map[string]json.RawMessage `json:"targets"`
	}
	if err := json.NewDecoder(r).Decode(&doc); err != nil {
		return "", fmt.Errorf("parse deps.json: %w", err)
	}
	seen := map[string]struct{}{}
	for _, libs := range doc.Targets {
		for lib := range libs {
			if v, ok := strings.CutPrefix(lib, runnerListenerLib); ok && v != "" {
				seen[v] = struct{}{}
			}
		}
	}
	switch len(seen) {
	case 0:
		return "", fmt.Errorf("deps.json names no %s* library", runnerListenerLib)
	case 1:
		for v := range seen {
			return v, nil
		}
	}
	list := make([]string, 0, len(seen))
	for v := range seen {
		list = append(list, v)
	}
	return "", fmt.Errorf("deps.json names %d different runner versions: %s", len(seen), strings.Join(list, ", "))
}

// countingReader enforces one byte budget across every layer an inspection streams.
type countingReader struct {
	r     io.Reader
	n     int64
	limit int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	if c.n >= c.limit {
		return 0, errBudget
	}
	if remaining := c.limit - c.n; int64(len(p)) > remaining {
		p = p[:remaining]
	}
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func (c *countingReader) exhausted() bool { return c.n >= c.limit }

var errBudget = errors.New("inspection byte budget exhausted")
