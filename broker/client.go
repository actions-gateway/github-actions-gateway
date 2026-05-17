package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
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

// BrokerClient is the low-level HTTP client for the GitHub broker protocol.
// All methods are context-aware and propagate cancellation.
//
// BrokerURL is the static base URL used for CreateSession, GetMessage, and
// DeleteSession. AcquireJob and RenewJob require the per-job run_service_url
// passed as an explicit parameter — see the package-level two-URL model note.
type BrokerClient struct {
	// BrokerURL is the static base URL for session and message calls.
	BrokerURL string
	// HTTPClient is used for all outbound calls. Tests substitute an
	// httptest-backed client.
	HTTPClient *http.Client
	// Token is the installation access token set before each call.
	Token string
}

func (c *BrokerClient) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}

// newRequest builds a JSON POST request to url with body marshalled from v.
func (c *BrokerClient) newJSONRequest(ctx context.Context, method, url string, body any) (*http.Request, error) {
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

// CreateSession registers a virtual runner with the broker and returns the
// session ID and the broker URL to use for subsequent message polls.
//
// A 400 response with a version-too-old message body is returned as a
// *VersionTooOldError so callers can surface it as a non-retriable condition.
func (c *BrokerClient) CreateSession(ctx context.Context, runnerVersion string) (sessionID string, brokerURL string, err error) {
	reqBody := map[string]any{
		"agentName":       "github-actions-gateway",
		"agentVersion":    runnerVersion,
		"agentLabel":      runnerVersion,
		"useFipsEncryption": false,
		"userAgent":       fmt.Sprintf("GitHubActionsGateway/%s", runnerVersion),
	}
	req, err := c.newJSONRequest(ctx, http.MethodPost, c.BrokerURL+"/sessions", reqBody)
	if err != nil {
		return "", "", err
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return "", "", fmt.Errorf("broker: CreateSession: %w", err)
	}
	defer resp.Body.Close()
	rawBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusBadRequest {
		msg := string(rawBody)
		if strings.Contains(strings.ToLower(msg), "version") ||
			strings.Contains(strings.ToLower(msg), "too old") ||
			strings.Contains(strings.ToLower(msg), "minimum") {
			return "", "", &VersionTooOldError{Message: msg}
		}
		return "", "", fmt.Errorf("broker: CreateSession: 400 %s", msg)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", "", fmt.Errorf("broker: CreateSession: unexpected status %d", resp.StatusCode)
	}

	var respBody struct {
		SessionID string `json:"sessionId"`
		// GitHub may return an updated broker URL in the session response.
		// Use it if present; fall back to BrokerClient.BrokerURL.
		BrokerURL string `json:"brokerURL"`
	}
	if err := json.Unmarshal(rawBody, &respBody); err != nil {
		return "", "", fmt.Errorf("broker: CreateSession: decode response: %w", err)
	}
	if respBody.BrokerURL == "" {
		respBody.BrokerURL = c.BrokerURL
	}
	return respBody.SessionID, respBody.BrokerURL, nil
}

// GetMessage opens a 50-second long-poll against the broker.
// Returns (nil, nil) on 202 Accepted (no job queued).
// Returns a non-nil *TaskAgentMessage when a job is available.
// Callers are responsible for retrying on nil/nil with appropriate backoff.
// Returns *RateLimitError on 429.
func (c *BrokerClient) GetMessage(ctx context.Context, sessionID string) (*TaskAgentMessage, error) {
	url := c.BrokerURL + "/message?sessionId=" + sessionID
	req, err := c.newJSONRequest(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("broker: GetMessage: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusAccepted: // 202 — no job queued
		return nil, nil
	case http.StatusTooManyRequests: // 429
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
// from BrokerClient.BrokerURL.
//
// Returns the parsed response, the raw response body bytes (forwarded opaquely
// to the worker pod), and any error.
//
// PlanID extraction: the x-plan-id response header takes precedence over
// AcquireJobResponse.Plan.PlanID from the body.
func (c *BrokerClient) AcquireJob(ctx context.Context, runServiceURL string, reqData JobAcquisitionRequest) (*AcquireJobResponse, []byte, error) {
	req, err := c.newJSONRequest(ctx, http.MethodPost, runServiceURL+"/acquirejob", reqData)
	if err != nil {
		return nil, nil, err
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("broker: AcquireJob: %w", err)
	}
	defer resp.Body.Close()

	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("broker: AcquireJob: read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("broker: AcquireJob: unexpected status %d: %s", resp.StatusCode, string(rawBody))
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
// RunnerJobRequestBody.RunServiceURL, not from BrokerClient.BrokerURL.
func (c *BrokerClient) RenewJob(ctx context.Context, runServiceURL string, reqData RenewJobRequest) (*RenewJobResponse, error) {
	req, err := c.newJSONRequest(ctx, http.MethodPost, runServiceURL+"/renewjob", reqData)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("broker: RenewJob: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("broker: RenewJob: unexpected status %d: %s", resp.StatusCode, string(body))
	}

	var result RenewJobResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("broker: RenewJob: decode response: %w", err)
	}
	return &result, nil
}

// DeleteSession tears down a broker session, allowing GitHub to re-queue any
// unacquired work. Called during graceful shutdown.
func (c *BrokerClient) DeleteSession(ctx context.Context, sessionID string) error {
	req, err := c.newJSONRequest(ctx, http.MethodDelete, c.BrokerURL+"/sessions/"+sessionID, nil)
	if err != nil {
		return err
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("broker: DeleteSession: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("broker: DeleteSession: unexpected status %d", resp.StatusCode)
	}
	return nil
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
