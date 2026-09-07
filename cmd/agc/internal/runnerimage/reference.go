// Package runnerimage reads the actions/runner version a worker image ships out of
// the image in its registry, before any pod runs it (Q988).
//
// It is the attestable counterpart of the wrapper's self-report (Q792): the registry
// copy is immutable once addressed by digest and nothing that runs inside the
// container can rewrite it. The version is read from the same file the wrapper
// reads, bin/Runner.Listener.deps.json, because it is the only version record the
// image carries — the config blob's org.opencontainers.image.version label is the
// Ubuntu base's ("24.04" on ghcr.io/actions/actions-runner:2.335.1, measured
// 2026-09-07) and the history names no tarball, the runner arriving by COPY from a
// build stage.
//
// The client speaks the OCI distribution API with the standard library alone: a
// manifest GET, an index GET when the reference is multi-arch, a token exchange when
// the registry challenges, and a streamed blob GET per layer scanned. That is the
// whole surface the feature needs, and it keeps a vendored registry library out of
// the tree.
package runnerimage

import (
	"fmt"
	"strings"
)

// Reference is a parsed container image reference: registry host, repository path,
// and the tag and/or digest that address a manifest in it.
type Reference struct {
	Registry   string
	Repository string
	Tag        string
	Digest     string
}

// String reassembles the reference in canonical form.
func (r Reference) String() string {
	s := r.Registry + "/" + r.Repository
	if r.Tag != "" {
		s += ":" + r.Tag
	}
	if r.Digest != "" {
		s += "@" + r.Digest
	}
	return s
}

// manifestRef is what addresses the manifest: the digest when one is pinned, since
// it is immutable, else the tag.
func (r Reference) manifestRef() string {
	if r.Digest != "" {
		return r.Digest
	}
	return r.Tag
}

// ParseReference splits an image reference the way the container runtime does: a
// first path component holding a '.' or ':' (or "localhost") is the registry,
// otherwise the registry is docker.io and a single-segment path is under library/.
// A missing tag and digest defaults the tag to latest, which is what kubelet pulls.
func ParseReference(image string) (Reference, error) {
	var ref Reference
	if image == "" {
		return ref, fmt.Errorf("empty image reference")
	}
	rest := image
	if at := strings.IndexByte(rest, '@'); at >= 0 {
		ref.Digest = rest[at+1:]
		rest = rest[:at]
		if !strings.HasPrefix(ref.Digest, "sha256:") || len(ref.Digest) != len("sha256:")+64 {
			return ref, fmt.Errorf("image reference %q: unsupported digest %q", image, ref.Digest)
		}
	}
	// A tag follows the last ':' after the last '/'; a registry port's colon sits
	// before the final path separator.
	if colon := strings.LastIndexByte(rest, ':'); colon > strings.LastIndexByte(rest, '/') {
		ref.Tag = rest[colon+1:]
		rest = rest[:colon]
	}
	if rest == "" {
		return ref, fmt.Errorf("image reference %q: no repository", image)
	}
	first, remainder, hasSlash := strings.Cut(rest, "/")
	if hasSlash && (strings.ContainsAny(first, ".:") || first == "localhost") {
		ref.Registry, ref.Repository = first, remainder
	} else {
		ref.Registry, ref.Repository = "docker.io", rest
		if !strings.Contains(rest, "/") {
			ref.Repository = "library/" + rest
		}
	}
	if ref.Repository == "" || strings.HasPrefix(ref.Repository, "/") || strings.HasSuffix(ref.Repository, "/") {
		return ref, fmt.Errorf("image reference %q: malformed repository", image)
	}
	if ref.Tag == "" && ref.Digest == "" {
		ref.Tag = "latest"
	}
	return ref, nil
}

// apiHost is the host the distribution API is served from. Docker Hub's API lives
// on registry-1.docker.io while its references say docker.io.
func (r Reference) apiHost() string {
	if r.Registry == "docker.io" {
		return "registry-1.docker.io"
	}
	return r.Registry
}
