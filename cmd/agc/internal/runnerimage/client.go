package runnerimage

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Media types the distribution API negotiates. Both the OCI and the Docker
// schema-2 families are accepted, since ghcr.io serves OCI and older registries
// still serve Docker.
const (
	mediaOCIIndex        = "application/vnd.oci.image.index.v1+json"
	mediaOCIManifest     = "application/vnd.oci.image.manifest.v1+json"
	mediaDockerList      = "application/vnd.docker.distribution.manifest.list.v2+json"
	mediaDockerManifest  = "application/vnd.docker.distribution.manifest.v2+json"
	mediaOCILayerGzip    = "application/vnd.oci.image.layer.v1.tar+gzip"
	mediaOCILayerTar     = "application/vnd.oci.image.layer.v1.tar"
	mediaOCILayerZstd    = "application/vnd.oci.image.layer.v1.tar+zstd"
	mediaDockerLayerGzip = "application/vnd.docker.image.rootfs.diff.tar.gzip"
)

var manifestAccept = strings.Join([]string{mediaOCIIndex, mediaDockerList, mediaOCIManifest, mediaDockerManifest}, ", ")

// maxManifestBytes caps a manifest or index read. Real ones are a few KB.
const maxManifestBytes = 4 << 20

// client is one registry session: the HTTP client, the login to present, and the
// bearer token the registry issued, if it challenged for one.
type client struct {
	http  *http.Client
	ref   Reference
	cred  Credential
	auth  bool
	token string
}

// descriptor is the subset of an OCI descriptor the scan needs.
type descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
	Platform  *struct {
		Architecture string `json:"architecture"`
		OS           string `json:"os"`
	} `json:"platform,omitempty"`
}

// manifest is the union of an index and an image manifest: exactly one of
// Manifests or Layers is populated.
type manifest struct {
	MediaType string       `json:"mediaType"`
	Manifests []descriptor `json:"manifests"`
	Layers    []descriptor `json:"layers"`
}

// get issues one authenticated GET. On a 401 carrying a Bearer challenge it
// exchanges for a token at the challenge's realm — anonymously, or with the
// configured login as Basic — and retries once; a Basic challenge is answered with
// the login directly.
func (c *client) get(ctx context.Context, u, accept string) (*http.Response, error) {
	resp, err := c.do(ctx, u, accept)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}
	challenge := resp.Header.Get("WWW-Authenticate")
	_ = resp.Body.Close()
	if err := c.authenticate(ctx, challenge); err != nil {
		return nil, err
	}
	resp, err = c.do(ctx, u, accept)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("%s: unauthorized after authenticating (%s)", u, describeCred(c.cred))
	}
	return resp, nil
}

func (c *client) do(ctx context.Context, u, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	switch {
	case c.token != "":
		req.Header.Set("Authorization", "Bearer "+c.token)
	case c.auth:
		req.SetBasicAuth(c.cred.Username, c.cred.Password)
	}
	return c.http.Do(req)
}

// authenticate answers a WWW-Authenticate challenge.
func (c *client) authenticate(ctx context.Context, challenge string) error {
	scheme, params := parseChallenge(challenge)
	switch strings.ToLower(scheme) {
	case "basic":
		if c.cred == (Credential{}) {
			return fmt.Errorf("registry %s requires a login and the pod template names no imagePullSecret for it", c.ref.Registry)
		}
		c.auth = true
		return nil
	case "bearer":
		return c.fetchToken(ctx, params)
	}
	return fmt.Errorf("registry %s sent an unsupported authentication challenge %q", c.ref.Registry, challenge)
}

// fetchToken performs the token exchange a Bearer challenge asks for. The realm,
// service and scope are the registry's own; the login, when there is one, rides as
// Basic on the exchange, which is how every registry that accepts a
// dockerconfigjson credential expects it.
func (c *client) fetchToken(ctx context.Context, params map[string]string) error {
	realm := params["realm"]
	if realm == "" {
		return fmt.Errorf("registry %s sent a Bearer challenge without a realm", c.ref.Registry)
	}
	u, err := url.Parse(realm)
	if err != nil {
		return fmt.Errorf("registry %s: bearer realm %q: %w", c.ref.Registry, realm, err)
	}
	q := u.Query()
	if s := params["service"]; s != "" {
		q.Set("service", s)
	}
	scope := params["scope"]
	if scope == "" {
		scope = "repository:" + c.ref.Repository + ":pull"
	}
	q.Set("scope", scope)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	if c.cred != (Credential{}) {
		req.SetBasicAuth(c.cred.Username, c.cred.Password)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("token exchange with %s: %w", u.Host, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("token exchange with %s: HTTP %d (%s)", u.Host, resp.StatusCode, describeCred(c.cred))
	}
	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxManifestBytes)).Decode(&body); err != nil {
		return fmt.Errorf("token exchange with %s: %w", u.Host, err)
	}
	c.token = body.Token
	if c.token == "" {
		c.token = body.AccessToken
	}
	if c.token == "" {
		return fmt.Errorf("token exchange with %s returned no token", u.Host)
	}
	return nil
}

// parseChallenge splits `Scheme k="v", k2="v2"` into its scheme and parameters.
func parseChallenge(header string) (string, map[string]string) {
	scheme, rest, _ := strings.Cut(strings.TrimSpace(header), " ")
	params := map[string]string{}
	for rest != "" {
		var pair string
		pair, rest = cutQuotedComma(rest)
		k, v, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if !ok {
			continue
		}
		params[strings.ToLower(k)] = strings.Trim(v, `"`)
	}
	return scheme, params
}

// cutQuotedComma cuts at the first comma outside double quotes.
func cutQuotedComma(s string) (string, string) {
	quoted := false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			quoted = !quoted
		case ',':
			if !quoted {
				return s[:i], s[i+1:]
			}
		}
	}
	return s, ""
}

func describeCred(c Credential) string {
	if c == (Credential{}) {
		return "anonymous"
	}
	return "as " + c.Username
}

// manifest fetches the manifest or index a reference addresses.
func (c *client) manifest(ctx context.Context, ref string) (*manifest, string, error) {
	u := fmt.Sprintf("https://%s/v2/%s/manifests/%s", c.ref.apiHost(), c.ref.Repository, ref)
	resp, err := c.get(ctx, u, manifestAccept)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("manifest %s: HTTP %d", ref, resp.StatusCode)
	}
	var m manifest
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxManifestBytes)).Decode(&m); err != nil {
		return nil, "", fmt.Errorf("manifest %s: %w", ref, err)
	}
	if m.MediaType == "" {
		m.MediaType = resp.Header.Get("Content-Type")
	}
	digest := resp.Header.Get("Docker-Content-Digest")
	if strings.HasPrefix(ref, "sha256:") {
		digest = ref
	}
	return &m, digest, nil
}

// blob opens a layer for streaming. The caller closes the body.
func (c *client) blob(ctx context.Context, digest string) (io.ReadCloser, error) {
	u := fmt.Sprintf("https://%s/v2/%s/blobs/%s", c.ref.apiHost(), c.ref.Repository, digest)
	resp, err := c.get(ctx, u, "")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("blob %s: HTTP %d", digest, resp.StatusCode)
	}
	return resp.Body, nil
}
