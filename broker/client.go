package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/actions-gateway/github-actions-gateway/githubapp"
)

// VersionTooOldError is returned by CreateSession when GitHub rejects the
// runner version as below the enforced minimum. Callers should surface this
// as a non-retriable condition rather than retrying in a tight loop.
type VersionTooOldError struct {
	Message string
}

func (e *VersionTooOldError) Error() string {
	return fmt.Sprintf("broker: runner version too old: %s", e.Message)
}

// RateLimitError is returned when GitHub responds with 429 Too Many Requests.
// RetryAfter is the duration the caller should wait before retrying.
// It is parsed from the Retry-After header when present; otherwise -1 signals
// that the caller should apply exponential backoff.
type RateLimitError struct {
	RetryAfter time.Duration // -1 if no Retry-After header was present
}

func (e *RateLimitError) Error() string {
	if e.RetryAfter < 0 {
		return "broker: rate limited (no Retry-After header)"
	}
	return fmt.Sprintf("broker: rate limited, retry after %s", e.RetryAfter)
}

// UnauthorizedError is returned by CreateSession and GetMessage when the
// broker responds with 401 Unauthorized or 403 Forbidden. Callers should
// treat this as a signal to refresh the bearer token before retrying.
type UnauthorizedError struct {
	StatusCode int
}

func (e *UnauthorizedError) Error() string {
	return fmt.Sprintf("broker: unauthorized (HTTP %d)", e.StatusCode)
}

// SessionExpiredError is returned by GetMessage when the broker responds with
// 404 Not Found or 410 Gone, indicating the session no longer exists server-side.
// Callers should delete and re-create the session.
type SessionExpiredError struct {
	StatusCode int
}

func (e *SessionExpiredError) Error() string {
	return fmt.Sprintf("broker: session expired (HTTP %d)", e.StatusCode)
}

// Client is the low-level HTTP client for the GitHub broker protocol.
// All methods are context-aware and propagate cancellation.
//
// BrokerURL is the static base URL used for CreateSession, GetMessage, and
// DeleteSession. AcquireJob and RenewJob require the per-job run_service_url
// passed as an explicit parameter — see the package-level two-URL model note.
type Client struct {
	// BrokerURL is the static base URL for session and message calls.
	// It is the serverUrl value from the runner's .runner config file.
	BrokerURL string
	// PoolID is the GitHub Actions runner pool ID from the .runner config.
	// Defaults to 1 when zero, which is the standard self-hosted runner pool.
	// Used to construct VSTS Task Agent API paths:
	//   _apis/distributedtask/pools/{poolId}/sessions
	//   _apis/distributedtask/pools/{poolId}/messages
	// Ignored when UseV2Flow is true.
	PoolID int
	// UseV2Flow switches to the broker v2 HTTP API (BrokerHttpClient.cs) instead
	// of the VSTS pool API. Set when the runner's .runner config has useV2Flow: true.
	//
	// In v2 mode, session and message URLs are:
	//   POST/DELETE {BrokerURL}session
	//   GET         {BrokerURL}message?sessionId=...&status=online&runnerVersion=...
	// No pool path, no api-version query param.
	UseV2Flow bool
	// RunnerVersion is the runner version string (e.g. "2.334.0").
	// Required in v2 mode: sent as a query parameter on GetMessage calls.
	RunnerVersion string
	// RunnerOS is the OS identifier for v2 GetMessage query params (e.g. "osx", "linux").
	RunnerOS string
	// RunnerArch is the architecture for v2 GetMessage query params (e.g. "x64", "arm64").
	RunnerArch string
	// HTTPClient is used for all outbound calls. Tests substitute an
	// httptest-backed client.
	HTTPClient *http.Client
	// Token is the installation access token set before each call.
	Token string
	// PollMetrics records GetMessage polling error statistics.
	// If nil, metrics calls are skipped (zero-value Client is safe).
	PollMetrics PollMetricsRecorder
}

// CreateSessionResult is returned by CreateSession.
type CreateSessionResult struct {
	// SessionID is the unique identifier for this session.
	SessionID string
	// BrokerURL is the URL to use for subsequent GetMessage and DeleteSession calls.
	// May differ from Client.BrokerURL if the server redirected.
	BrokerURL string
	// EncryptionKey is the RSA-encrypted symmetric key for message decryption.
	// It must be RSA-OAEP (SHA-1) decrypted with the runner's private key to
	// obtain the 32-byte AES-256-CBC session key used by DecryptMessageBody.
	// Nil if the server did not return an encryption key.
	EncryptionKey []byte
	// EncryptionKeyEncrypted indicates whether EncryptionKey is RSA-encrypted
	// (true) or a raw plaintext key (false).
	EncryptionKeyEncrypted bool
}

// VSTS Task Agent API versions for each endpoint, sourced from the runner SDK's
// TaskAgentHttpClientBase.cs. These must be sent on every request to the VSTS
// pool API or the server returns 500 VssVersionNotSpecifiedException.
const (
	// vstsAPIVersionSession is the api-version for CreateSession and DeleteSession
	// (locationId 134e239e-2df3-4794-a6f6-24f1f19ec8dc, ApiResourceVersion(5.1,1)).
	vstsAPIVersionSession = "5.1-preview.1"
	// vstsAPIVersionMessage is the api-version for GetMessage
	// (locationId c3a054f6-7a8a-49c0-944e-3a8e5d7adfd7, ApiResourceVersion(6.0,1)).
	vstsAPIVersionMessage = "6.0-preview.1"
)

// vstsURL appends an api-version query parameter to a VSTS Task Agent API URL.
// The VSTS platform requires api-version on every request; without it the server
// returns 500 VssVersionNotSpecifiedException.
func vstsURL(base, apiVersion string) string {
	if strings.Contains(base, "?") {
		return base + "&api-version=" + apiVersion
	}
	return base + "?api-version=" + apiVersion
}

// PoolBase returns the base URL for VSTS Task Agent pool API calls, e.g.:
//
//	https://pipelines.actions.githubusercontent.com/TOKEN/_apis/distributedtask/pools/1
//
// It trims any trailing slash from BrokerURL. If PoolID is zero it defaults to 1.
// Exported so probe and gateway code can construct auxiliary VSTS paths (e.g.
// the delete-message acknowledgement endpoint) without duplicating the logic.
func (c *Client) PoolBase() string {
	poolID := c.PoolID
	if poolID == 0 {
		poolID = 1
	}
	return fmt.Sprintf("%s/_apis/distributedtask/pools/%d",
		strings.TrimRight(c.BrokerURL, "/"), poolID)
}

func (c *Client) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}

// newRequest builds a JSON POST request to url with body marshalled from v.
func (c *Client) newJSONRequest(ctx context.Context, method, url string, body any) (*http.Request, error) {
	var r io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("broker: marshal request body: %w", err)
		}
		r = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// CreateSession registers a virtual runner with the broker and returns session
// info including the AES session key (RSA-encrypted) for message decryption.
//
// agentID must match the agent's registered ID in the pool (the agentId field
// from the runner's .runner config file written by config.sh). agentName is
// the registered runner name. The server validates that agent.id is non-zero
// and refers to a known agent in the pool.
//
// When UseV2Flow is true, the broker v2 API path ({BrokerURL}session) is used
// instead of the VSTS pool API, matching BrokerHttpClient.CreateSessionAsync.
//
// A 400 response with a version-too-old message body is returned as a
// *VersionTooOldError so callers can surface it as a non-retriable condition.
func (c *Client) CreateSession(ctx context.Context, agentID int64, agentName, runnerVersion string) (*CreateSessionResult, error) {
	// The body follows the TaskAgentSession shape from the runner SDK:
	//   ownerName         — machine/process that owns the session
	//   agent             — TaskAgentReference: registered agent id, name, version
	//   useFipsEncryption — FIPS-compliant AES key wrapping (false for normal runners)
	reqBody := map[string]any{
		"ownerName": agentName,
		"agent": map[string]any{
			"id":      agentID,
			"name":    agentName,
			"version": runnerVersion,
		},
		"useFipsEncryption": false,
	}

	var url string
	if c.UseV2Flow {
		// Broker v2 API: POST {serverUrl}session (no pool path, no api-version).
		url = strings.TrimRight(c.BrokerURL, "/") + "/session"
	} else {
		url = vstsURL(c.PoolBase()+"/sessions", vstsAPIVersionSession)
	}

	req, err := c.newJSONRequest(ctx, http.MethodPost, url, reqBody)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("broker: CreateSession: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	rawBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusBadRequest {
		msg := capBody(rawBody, 200)
		if strings.Contains(strings.ToLower(msg), "version") ||
			strings.Contains(strings.ToLower(msg), "too old") ||
			strings.Contains(strings.ToLower(msg), "minimum") {
			return nil, &VersionTooOldError{Message: msg}
		}
		return nil, fmt.Errorf("broker: CreateSession: 400 %s", msg)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, &UnauthorizedError{StatusCode: resp.StatusCode}
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("broker: CreateSession: unexpected status %d from %s: %s",
			resp.StatusCode, url, capBody(rawBody, 200))
	}

	var respBody struct {
		SessionID string `json:"sessionId"`
		// GitHub may return an updated broker URL in the session response.
		// Use it if present; fall back to Client.BrokerURL.
		BrokerURL     string `json:"brokerURL"`
		EncryptionKey *struct {
			Encrypted bool   `json:"encrypted"`
			Value     []byte `json:"value"` // raw bytes; JSON decoder handles base64
		} `json:"encryptionKey"`
	}
	if err := json.Unmarshal(rawBody, &respBody); err != nil {
		return nil, fmt.Errorf("broker: CreateSession: decode response: %w", err)
	}
	if respBody.BrokerURL == "" {
		respBody.BrokerURL = c.BrokerURL
	}

	result := &CreateSessionResult{
		SessionID: respBody.SessionID,
		BrokerURL: respBody.BrokerURL,
	}
	if respBody.EncryptionKey != nil {
		result.EncryptionKey = respBody.EncryptionKey.Value
		result.EncryptionKeyEncrypted = respBody.EncryptionKey.Encrypted
	}
	return result, nil
}

// GetMessage opens a 50-second long-poll against the broker.
// Returns (nil, nil) on 202 Accepted (no job queued).
// Returns a non-nil *TaskAgentMessage when a job is available.
// Callers are responsible for retrying on nil/nil with appropriate backoff.
// Returns *RateLimitError on 429.
func (c *Client) GetMessage(ctx context.Context, sessionID string) (*TaskAgentMessage, error) {
	var reqURL string
	if c.UseV2Flow {
		// Broker v2 API: GET {serverUrl}message with status/version/os/arch params.
		// Matches BrokerHttpClient.GetRunnerMessageAsync.
		u, err := url.Parse(strings.TrimRight(c.BrokerURL, "/") + "/message")
		if err != nil {
			return nil, fmt.Errorf("broker: GetMessage: parse URL: %w", err)
		}
		q := u.Query()
		q.Set("sessionId", sessionID)
		q.Set("status", "online")
		if c.RunnerVersion != "" {
			q.Set("runnerVersion", c.RunnerVersion)
		}
		if c.RunnerOS != "" {
			q.Set("os", c.RunnerOS)
		}
		if c.RunnerArch != "" {
			q.Set("architecture", c.RunnerArch)
		}
		q.Set("disableUpdate", "false")
		u.RawQuery = q.Encode()
		reqURL = u.String()
	} else {
		u, err := url.Parse(c.PoolBase() + "/messages")
		if err != nil {
			return nil, fmt.Errorf("broker: GetMessage: parse URL: %w", err)
		}
		q := u.Query()
		q.Set("sessionId", sessionID)
		u.RawQuery = q.Encode()
		reqURL = vstsURL(u.String(), vstsAPIVersionMessage)
	}
	req, err := c.newJSONRequest(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("broker: GetMessage: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusAccepted: // 202 — no job queued
		return nil, nil
	case http.StatusUnauthorized, http.StatusForbidden: // 401, 403
		return nil, &UnauthorizedError{StatusCode: resp.StatusCode}
	case http.StatusNotFound, http.StatusGone: // 404, 410 — session no longer exists
		return nil, &SessionExpiredError{StatusCode: resp.StatusCode}
	case http.StatusTooManyRequests: // 429
		if c.PollMetrics != nil {
			c.PollMetrics.IncPollError("rate_limited")
		}
		return nil, parseRateLimitError(resp)
	case http.StatusOK:
		// fall through to decode
	default:
		return nil, fmt.Errorf("broker: GetMessage: unexpected status %d", resp.StatusCode)
	}

	var msg TaskAgentMessage
	if err := json.NewDecoder(resp.Body).Decode(&msg); err != nil {
		return nil, fmt.Errorf("broker: GetMessage: decode response: %w", err)
	}
	return &msg, nil
}

// AcquireJob claims a job on the run service URL extracted from the message
// body. runServiceURL must come from RunnerJobRequestBody.RunServiceURL, not
// from Client.BrokerURL.
//
// Returns the parsed response, the raw response body bytes (forwarded opaquely
// to the worker pod), and any error.
//
// PlanID extraction: the x-plan-id response header takes precedence over
// AcquireJobResponse.Plan.PlanID from the body.
func (c *Client) AcquireJob(ctx context.Context, runServiceURL string, reqData JobAcquisitionRequest) (*AcquireJobResponse, []byte, error) {
	req, err := c.newJSONRequest(ctx, http.MethodPost, runServiceURL+"/acquirejob", reqData)
	if err != nil {
		return nil, nil, err
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("broker: AcquireJob: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("broker: AcquireJob: read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("broker: AcquireJob: unexpected status %d: %s", resp.StatusCode, capBody(rawBody, 200))
	}

	var parsed AcquireJobResponse
	if err := json.Unmarshal(rawBody, &parsed); err != nil {
		return nil, nil, fmt.Errorf("broker: AcquireJob: decode response: %w", err)
	}

	// x-plan-id header takes precedence over the body's .plan.planId.
	if headerPlanID := resp.Header.Get("x-plan-id"); headerPlanID != "" {
		parsed.Plan.PlanID = headerPlanID
	}

	return &parsed, rawBody, nil
}

// RenewJob renews the job lock on the run service URL. Must be called every
// 60 seconds after AcquireJob succeeds. runServiceURL must come from
// RunnerJobRequestBody.RunServiceURL, not from Client.BrokerURL.
func (c *Client) RenewJob(ctx context.Context, runServiceURL string, reqData RenewJobRequest) (*RenewJobResponse, error) {
	req, err := c.newJSONRequest(ctx, http.MethodPost, runServiceURL+"/renewjob", reqData)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("broker: RenewJob: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("broker: RenewJob: unexpected status %d: %s", resp.StatusCode, capBody(body, 200))
	}

	var result RenewJobResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("broker: RenewJob: decode response: %w", err)
	}
	return &result, nil
}

// RenewJobLoop starts a background goroutine that calls RenewJob on every tick
// until ctx is cancelled or RenewJob returns a non-nil error.
//
// tickC drives the renewal cadence. Pass time.NewTicker(60*time.Second).C for
// production use, or a manually-driven channel in tests to advance time without
// sleeping (zero drift, deterministic call counts).
//
// The returned channel receives the first RenewJob error and is then closed.
// On a clean context cancellation the channel is closed with no value sent.
// The goroutine is guaranteed to have exited by the time the channel is closed.
func (c *Client) RenewJobLoop(ctx context.Context, runServiceURL string, req RenewJobRequest, tickC <-chan time.Time) <-chan error {
	errCh := make(chan error, 1)
	go func() {
		defer close(errCh)
		for {
			select {
			case <-ctx.Done():
				return
			case _, ok := <-tickC:
				if !ok {
					return // ticker stopped externally
				}
				if _, err := c.RenewJob(ctx, runServiceURL, req); err != nil {
					errCh <- err
					return
				}
			}
		}
	}()
	return errCh
}

// DeleteSession tears down a broker session, allowing GitHub to re-queue any
// unacquired work. Called during graceful shutdown.
//
// In v2 flow mode, sessionID is ignored: the server identifies the session
// from the bearer token, and the URL is simply {BrokerURL}session.
func (c *Client) DeleteSession(ctx context.Context, sessionID string) error {
	var deleteURL string
	if c.UseV2Flow {
		deleteURL = strings.TrimRight(c.BrokerURL, "/") + "/session"
	} else {
		deleteURL = vstsURL(c.PoolBase()+"/sessions/"+sessionID, vstsAPIVersionSession)
	}
	req, err := c.newJSONRequest(ctx, http.MethodDelete, deleteURL, nil)
	if err != nil {
		return err
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("broker: DeleteSession: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("broker: DeleteSession: unexpected status %d", resp.StatusCode)
	}
	return nil
}

// capBody returns at most n bytes of b as a string with credential-shaped
// substrings redacted, preventing unbounded — or credential-bearing — error
// messages from being logged or returned when a server sends a large response.
// It delegates to githubapp.SanitizeBody, the single redaction implementation
// shared across the repo (see githubapp/sanitize.go).
func capBody(b []byte, n int) string {
	return githubapp.SanitizeBody(b, n)
}

// parseRateLimitError builds a *RateLimitError from a 429 response, honoring
// the Retry-After header when present.
func parseRateLimitError(resp *http.Response) *RateLimitError {
	ra := resp.Header.Get("Retry-After")
	if ra == "" {
		return &RateLimitError{RetryAfter: -1}
	}
	secs, err := strconv.ParseFloat(ra, 64)
	if err != nil {
		return &RateLimitError{RetryAfter: -1}
	}
	return &RateLimitError{RetryAfter: time.Duration(secs * float64(time.Second))}
}
