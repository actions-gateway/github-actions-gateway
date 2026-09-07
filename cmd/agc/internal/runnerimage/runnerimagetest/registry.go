// Package runnerimagetest is an in-process OCI distribution registry for tests of
// the runner-version reader (Q988): layers built in memory, an optional
// multi-platform index, and the token or Basic challenge a real registry issues.
package runnerimagetest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// Media types the fake serves, matching what ghcr.io and Docker Hub emit.
const (
	MediaOCIIndex     = "application/vnd.oci.image.index.v1+json"
	MediaOCIManifest  = "application/vnd.oci.image.manifest.v1+json"
	MediaOCILayerGzip = "application/vnd.oci.image.layer.v1.tar+gzip"
	MediaOCILayerTar  = "application/vnd.oci.image.layer.v1.tar"
	MediaOCILayerZstd = "application/vnd.oci.image.layer.v1.tar+zstd"
)

// DepsPath is where the official image keeps the runner's dependency manifest.
const DepsPath = "home/runner/bin/Runner.Listener.deps.json"

// Auth selects the challenge the registry issues.
type Auth string

const (
	// AuthNone serves every request.
	AuthNone Auth = ""
	// AuthBearer issues a Bearer challenge and serves tokens at /token; with a Login
	// set, the token endpoint demands it as Basic.
	AuthBearer Auth = "bearer"
	// AuthBasic issues a Basic challenge and demands Login on every request.
	AuthBasic Auth = "basic"
)

// Credential is a registry login.
type Credential struct {
	Username string
	Password string
}

// Platform is an index entry's os/arch.
type Platform struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
}

// Descriptor is an OCI content descriptor.
type Descriptor struct {
	MediaType string    `json:"mediaType"`
	Digest    string    `json:"digest"`
	Size      int64     `json:"size"`
	Platform  *Platform `json:"platform,omitempty"`
}

// Entry is one member of a layer.
type Entry struct {
	Name string
	Body string
}

// E is an Entry literal.
func E(name, body string) Entry { return Entry{Name: name, Body: body} }

// Registry is the fake. Every repository is served; manifests are addressed by
// "repo:ref" and blobs are shared.
type Registry struct {
	// Auth and Login may be set between requests.
	Auth  Auth
	Login Credential
	// HoldBlobs, when non-nil, blocks every blob GET until it is closed, so a test
	// can decide when an inspection completes.
	HoldBlobs chan struct{}

	t   testing.TB
	srv *httptest.Server

	mu        sync.Mutex
	blobs     map[string][]byte
	manifests map[string]string
	blobGets  atomic.Int64
}

// New starts a TLS registry that closes with the test.
func New(t testing.TB) *Registry {
	t.Helper()
	r := &Registry{t: t, blobs: map[string][]byte{}, manifests: map[string]string{}}
	r.srv = httptest.NewTLSServer(http.HandlerFunc(r.serve))
	t.Cleanup(r.srv.Close)
	return r
}

// Host is the registry as an image reference names it.
func (r *Registry) Host() string { return strings.TrimPrefix(r.srv.URL, "https://") }

// URL is the server's base URL.
func (r *Registry) URL() string { return r.srv.URL }

// Image builds a reference into this registry: Image("acme/runner", ":v") or
// Image("acme/runner", "@sha256:…").
func (r *Registry) Image(repo, suffix string) string { return r.Host() + "/" + repo + suffix }

// Client trusts the registry's certificate.
func (r *Registry) Client() *http.Client { return r.srv.Client() }

// Close stops the server early.
func (r *Registry) Close() { r.srv.Close() }

// BlobGets counts layer downloads, which is how a test sees how far a scan went.
func (r *Registry) BlobGets() int64 { return r.blobGets.Load() }

// Put stores a raw blob and returns its digest.
func (r *Registry) Put(b []byte) string {
	d := Digest(b)
	r.mu.Lock()
	r.blobs[d] = b
	r.mu.Unlock()
	return d
}

// Digest is the sha256 content digest of b.
func Digest(b []byte) string { return fmt.Sprintf("sha256:%x", sha256.Sum256(b)) }

// GzipLayer packs entries into a gzip tar layer.
func (r *Registry) GzipLayer(entries ...Entry) Descriptor {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	r.writeTar(gz, entries)
	if err := gz.Close(); err != nil {
		r.t.Fatal(err)
	}
	return Descriptor{MediaType: MediaOCILayerGzip, Digest: r.Put(buf.Bytes()), Size: int64(buf.Len())}
}

// TarLayer packs entries into an uncompressed tar layer.
func (r *Registry) TarLayer(entries ...Entry) Descriptor {
	var buf bytes.Buffer
	r.writeTar(&buf, entries)
	return Descriptor{MediaType: MediaOCILayerTar, Digest: r.Put(buf.Bytes()), Size: int64(buf.Len())}
}

func (r *Registry) writeTar(w io.Writer, entries []Entry) {
	tw := tar.NewWriter(w)
	for _, e := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: e.Name, Mode: 0o644, Size: int64(len(e.Body)), Typeflag: tar.TypeReg}); err != nil {
			r.t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.Body)); err != nil {
			r.t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		r.t.Fatal(err)
	}
}

// Manifest registers an image manifest over layers, bottom-most first, and returns
// its descriptor tagged with platform.
func (r *Registry) Manifest(repo, os, arch string, layers ...Descriptor) Descriptor {
	b, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     MediaOCIManifest,
		"config":        map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": Digest([]byte("{}")), "size": 2},
		"layers":        layers,
	})
	if err != nil {
		r.t.Fatal(err)
	}
	d := r.Put(b)
	r.Tag(repo, d, d)
	return Descriptor{MediaType: MediaOCIManifest, Digest: d, Size: int64(len(b)), Platform: &Platform{Architecture: arch, OS: os}}
}

// Index registers a multi-platform index over manifests and returns its digest.
func (r *Registry) Index(repo string, manifests ...Descriptor) string {
	b, err := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": MediaOCIIndex, "manifests": manifests})
	if err != nil {
		r.t.Fatal(err)
	}
	d := r.Put(b)
	r.Tag(repo, d, d)
	return d
}

// Tag points repo:tag at a manifest or index digest. Re-tagging moves it.
func (r *Registry) Tag(repo, tag, digest string) {
	r.mu.Lock()
	r.manifests[repo+":"+tag] = digest
	r.mu.Unlock()
}

// RunnerImage registers the common case, a single linux/amd64 manifest whose one
// layer ships deps.json at version, and tags it. It returns the manifest digest.
func (r *Registry) RunnerImage(repo, tag, version string) string {
	m := r.Manifest(repo, "linux", "amd64", r.GzipLayer(Entry{DepsPath, DepsJSON(version)}))
	r.Tag(repo, tag, m.Digest)
	return m.Digest
}

func (r *Registry) serve(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path == "/token" {
		if r.Login != (Credential{}) {
			u, p, ok := req.BasicAuth()
			if !ok || u != r.Login.Username || p != r.Login.Password {
				http.Error(w, "bad login", http.StatusUnauthorized)
				return
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"token": "tok-" + req.URL.Query().Get("scope")})
		return
	}
	rest, ok := strings.CutPrefix(req.URL.Path, "/v2/")
	if !ok {
		http.NotFound(w, req)
		return
	}
	// <repo>/manifests/<ref> or <repo>/blobs/<digest>; the repo may hold slashes.
	var repo, kind, ref string
	if i := strings.LastIndex(rest, "/manifests/"); i >= 0 {
		repo, kind, ref = rest[:i], "manifests", rest[i+len("/manifests/"):]
	} else if i := strings.LastIndex(rest, "/blobs/"); i >= 0 {
		repo, kind, ref = rest[:i], "blobs", rest[i+len("/blobs/"):]
	} else {
		http.NotFound(w, req)
		return
	}
	switch r.Auth {
	case AuthBearer:
		if req.Header.Get("Authorization") != "Bearer tok-repository:"+repo+":pull" {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="fake",scope="repository:%s:pull"`, r.srv.URL, repo))
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	case AuthBasic:
		u, p, ok := req.BasicAuth()
		if !ok || u != r.Login.Username || p != r.Login.Password {
			w.Header().Set("WWW-Authenticate", `Basic realm="fake"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	switch kind {
	case "manifests":
		d, ok := r.manifests[repo+":"+ref]
		if !ok {
			http.NotFound(w, req)
			return
		}
		var m struct {
			MediaType string `json:"mediaType"`
		}
		_ = json.Unmarshal(r.blobs[d], &m)
		w.Header().Set("Content-Type", m.MediaType)
		w.Header().Set("Docker-Content-Digest", d)
		_, _ = w.Write(r.blobs[d])
	case "blobs":
		b, ok := r.blobs[ref]
		if !ok {
			http.NotFound(w, req)
			return
		}
		if hold := r.HoldBlobs; hold != nil {
			r.mu.Unlock()
			<-hold
			r.mu.Lock()
		}
		r.blobGets.Add(1)
		_, _ = w.Write(b)
	}
}

// DepsJSON is a minimal Runner.Listener.deps.json naming version under two targets,
// the shape the real file has.
func DepsJSON(version string) string {
	return fmt.Sprintf(`{"targets":{".NETCoreApp,Version=v8.0":{"Runner.Listener/%[1]s":{}},".NETCoreApp,Version=v8.0/linux-x64":{"Runner.Listener/%[1]s":{},"Runner.Common/%[1]s":{}}}}`, version)
}

// DockerConfigJSON is a .dockerconfigjson payload with one login for host.
func DockerConfigJSON(host string, c Credential) []byte {
	auth := base64.StdEncoding.EncodeToString([]byte(c.Username + ":" + c.Password))
	return []byte(fmt.Sprintf(`{"auths":{"%s":{"auth":"%s"}}}`, host, auth))
}
