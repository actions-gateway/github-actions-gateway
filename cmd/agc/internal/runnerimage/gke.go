package runnerimage

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// GKEMetadataEndpoint is the GKE metadata server's own address. The AGC dials it
// rather than 169.254.169.254:80 because its egress NetworkPolicy admits only this
// destination: a policy is evaluated against the address dialled, before GKE's
// redirect, so the Compute Engine metadata server and the node credentials it
// serves on a pool without Workload Identity stay unreachable (measured on GKE
// 1.35.8 under Dataplane V2, docs/design/05-security.md, Q1066).
const GKEMetadataEndpoint = "http://169.254.169.252:988"

// gkeTokenPath is the metadata path that returns an access token for the pod's
// Workload Identity.
const gkeTokenPath = "/computeMetadata/v1/instance/service-accounts/default/token"

// artifactRegistryUser is the login name Artifact Registry accepts with an OAuth
// access token as the password.
const artifactRegistryUser = "oauth2accesstoken"

// GKEMetadataToken fetches an access token for the AGC's own GKE Workload Identity.
type GKEMetadataToken struct {
	// HTTP must not route through a proxy: the metadata server is node-local.
	HTTP *http.Client
	// Endpoint is the metadata server's base URL; empty means GKEMetadataEndpoint.
	Endpoint string
}

// Token returns an access token. The token is never part of an error.
func (g *GKEMetadataToken) Token(ctx context.Context) (string, error) {
	endpoint := g.Endpoint
	if endpoint == "" {
		endpoint = GKEMetadataEndpoint
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+gkeTokenPath, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := g.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("GKE metadata server: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GKE metadata server: HTTP %d", resp.StatusCode)
	}
	var body struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body); err != nil {
		return "", fmt.Errorf("GKE metadata server: decode token response: %w", err)
	}
	if body.AccessToken == "" {
		return "", fmt.Errorf("GKE metadata server returned no access token")
	}
	return body.AccessToken, nil
}

// isArtifactRegistryHost reports whether a registry host is Artifact Registry's,
// the only hosts a Workload Identity token is presented to. A host carrying a port
// matches nothing.
func isArtifactRegistryHost(host string) bool {
	return strings.HasSuffix(host, "-docker.pkg.dev") || host == "gcr.io" || strings.HasSuffix(host, ".gcr.io")
}
