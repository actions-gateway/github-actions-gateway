package scaleset

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/actions-gateway/github-actions-gateway/githubapp"
	"github.com/actions-gateway/github-actions-gateway/githubapp/httpx"
)

// apiVersion is the Actions Service api-version query parameter sent on every
// _apis/runtime call (Azure-DevOps-style preview versioning).
const apiVersion = "6.0-preview"

// defaultAPIBase is the public GitHub REST API root used for the two bootstrap
// hops when Config.APIBase is empty.
const defaultAPIBase = "https://api.github.com"

// defaultAdminRefreshLead is how far before the admin JWT's expiry the client
// re-mints it. The live probe proved the admin JWT expires within ~17 minutes and
// that ARC's parse-exp-and-refresh (60s pre-expiry) is mandatory, not defensive
// polish (Q264 plan §2b-7).
const defaultAdminRefreshLead = 60 * time.Second

// longPollHold is the queue's server-side long-poll window: GetMessage holds the
// connection open for up to this long before returning 202. Observed live at ~51s
// (Q264 plan §2a-4). Any client response deadline must sit above it so a healthy
// long-poll is never severed.
const longPollHold = 50 * time.Second

// longPollResponseHeaderSlack is added to longPollHold to derive the poll client's
// ResponseHeaderTimeout — absorbing scheduling/network jitter so a healthy poll is
// never cut short, while still tearing down a black-holed connection shortly after
// the hold instead of blocking for the multi-minute OS TCP timeout (mirrors the
// broker long-poll client, Q108).
const longPollResponseHeaderSlack = 5 * time.Second

// NewPollHTTPClient returns an *http.Client tuned for the queue long-poll. Like
// broker.NewHTTPClient it clones http.DefaultTransport (preserving the per-tenant
// egress proxy's patched transport and CA — Q219) and sets ResponseHeaderTimeout
// just above longPollHold, deliberately setting no overall Timeout, because such a
// deadline would sever a healthy long-poll. Short control-plane calls use
// githubapp/httpx.NewClient instead (bounded by their per-call context).
func NewPollHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = longPollHold + longPollResponseHeaderSlack
	return &http.Client{Transport: transport}
}

// Config configures a Client. TokenProvider and ConfigURL are required.
type Config struct {
	// TokenProvider mints the GitHub App installation access token that authorizes
	// the registration-token bootstrap hop. Reuse the AGC's existing githubapp
	// provider — the App token never leaves the AGC (Q264 plan §4 security check).
	TokenProvider githubapp.TokenProvider
	// ConfigURL is the org or repo URL the scale set registers against. Its shape
	// selects scope: an org URL (…/org) registers org-scoped (runner-group gated);
	// an owner/repo URL (…/owner/repo) registers repo-scoped (bypasses groups —
	// the recommended path for public repos, Q264 plan §2a-6).
	ConfigURL string
	// APIBase is the REST API root. Empty selects the public GitHub API. Set it to
	// a GHES API base for GHES tenants (which also require the JobAvailable→acquire
	// path — Client.AcquireJobs).
	APIBase string
	// HTTPClient serves the short control-plane calls. Empty selects
	// httpx.NewClient (bounded, egress-proxy-aware). Build the Client after main()
	// patches http.DefaultTransport so the default client inherits the proxy CA.
	HTTPClient *http.Client
	// PollClient serves the queue long-poll. Empty selects NewPollHTTPClient.
	PollClient *http.Client
	// Metrics records poll-error and token-refresh statistics. Nil is safe.
	Metrics MetricsRecorder
	// AdminRefreshLead overrides how far before expiry the admin JWT is re-minted.
	// Non-positive selects defaultAdminRefreshLead.
	AdminRefreshLead time.Duration
}

// Client is the GAG-owned runner-scale-set protocol client. It manages the two-hop
// auth bootstrap and the admin-JWT lifecycle internally: every admin call re-mints
// the admin connection lazily when it is within AdminRefreshLead of expiry. The
// message-queue token is refreshed explicitly by the caller via RefreshSession on a
// 401 from the queue (GetMessage/AcquireJobs return *UnauthorizedError).
//
// A Client makes no calls of its own and starts no goroutines; all methods are
// synchronous and context-aware, so there is no done channel to manage. It is safe
// for concurrent use — admin-connection state is mutex-guarded — though ARC's model
// (and GAG's P3 listener) uses one Client per scale set.
type Client struct {
	provider         githubapp.TokenProvider
	configURL        string
	apiBase          string
	hc               *http.Client
	pollClient       *http.Client
	metrics          MetricsRecorder
	adminRefreshLead time.Duration

	mu   sync.Mutex
	conn *AdminConnection
}

// New builds a Client from cfg, validating the required fields and filling
// defaults for the HTTP clients, API base, and refresh lead.
func New(cfg Config) (*Client, error) {
	if cfg.TokenProvider == nil {
		return nil, errors.New("scaleset: Config.TokenProvider is required")
	}
	if cfg.ConfigURL == "" {
		return nil, errors.New("scaleset: Config.ConfigURL is required")
	}
	c := &Client{
		provider:         cfg.TokenProvider,
		configURL:        cfg.ConfigURL,
		apiBase:          strings.TrimRight(cfg.APIBase, "/"),
		hc:               cfg.HTTPClient,
		pollClient:       cfg.PollClient,
		metrics:          cfg.Metrics,
		adminRefreshLead: cfg.AdminRefreshLead,
	}
	if c.apiBase == "" {
		c.apiBase = defaultAPIBase
	}
	if c.hc == nil {
		c.hc = httpx.NewClient()
	}
	if c.pollClient == nil {
		c.pollClient = NewPollHTTPClient()
	}
	if c.adminRefreshLead <= 0 {
		c.adminRefreshLead = defaultAdminRefreshLead
	}
	return c, nil
}

// Connect forces the two-hop auth bootstrap now (minting the admin connection),
// so a caller can fail fast on an auth error at startup rather than on the first
// scale-set call. Subsequent calls reuse the cached connection until it nears expiry.
func (c *Client) Connect(ctx context.Context) error {
	_, err := c.admin(ctx)
	return err
}

// admin returns a valid admin connection, re-minting it under the lock when the
// cached one is absent or within adminRefreshLead of expiry. It returns a copy so
// callers cannot mutate the shared connection.
func (c *Client) admin(ctx context.Context) (*AdminConnection, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil || c.conn.ExpiresWithin(c.adminRefreshLead) {
		refreshed := c.conn != nil // an existing-but-expiring connection is a refresh
		conn, err := c.mintAdminConnection(ctx)
		if err != nil {
			return nil, err
		}
		c.conn = conn
		if refreshed && c.metrics != nil {
			c.metrics.IncTokenRefresh("admin")
		}
	}
	cp := *c.conn
	return &cp, nil
}

// mintAdminConnection runs the two bootstrap hops: installation token →
// registration token → RemoteAuth runner-registration → admin connection.
func (c *Client) mintAdminConnection(ctx context.Context) (*AdminConnection, error) {
	installToken, err := c.provider.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("scaleset: installation token: %w", err)
	}
	regToken, err := c.registrationToken(ctx, installToken)
	if err != nil {
		return nil, fmt.Errorf("scaleset: registration token: %w", err)
	}
	conn, err := c.runnerRegistration(ctx, regToken)
	if err != nil {
		return nil, fmt.Errorf("scaleset: runner-registration hop: %w", err)
	}
	return conn, nil
}

// registrationToken exchanges the installation token for a short-lived runner
// registration token, at org scope (ConfigURL has a single path segment) or repo
// scope (owner/repo path).
func (c *Client) registrationToken(ctx context.Context, installToken string) (string, error) {
	path, err := registrationTokenPath(c.configURL)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiBase+path, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+installToken)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := c.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("POST %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("POST %s: status %d: %s", path, resp.StatusCode, githubapp.SanitizeBody(body, 256))
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("decode registration token: %w", err)
	}
	if out.Token == "" {
		return "", errors.New("registration token response had no token")
	}
	return out.Token, nil
}

// registrationTokenPath maps an org or owner/repo config URL to its
// registration-token REST path.
func registrationTokenPath(configURL string) (string, error) {
	u, err := url.Parse(configURL)
	if err != nil {
		return "", fmt.Errorf("parse ConfigURL: %w", err)
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	switch len(segs) {
	case 1:
		if segs[0] == "" {
			return "", fmt.Errorf("ConfigURL must be an org or owner/repo URL, got path %q", u.Path)
		}
		return "/orgs/" + segs[0] + "/actions/runners/registration-token", nil
	case 2:
		return "/repos/" + segs[0] + "/" + segs[1] + "/actions/runners/registration-token", nil
	default:
		return "", fmt.Errorf("ConfigURL must be an org or owner/repo URL, got path %q", u.Path)
	}
}

// runnerRegistration performs the RemoteAuth hop that discovers the Actions
// Service tenant URL and mints the admin JWT.
func (c *Client) runnerRegistration(ctx context.Context, regToken string) (*AdminConnection, error) {
	payload, err := json.Marshal(map[string]string{
		"url":          c.configURL,
		"runner_event": "register",
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.apiBase+"/actions/runner-registration", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	// The one place the protocol uses the nonstandard RemoteAuth scheme.
	req.Header.Set("Authorization", "RemoteAuth "+regToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("POST /actions/runner-registration: status %d: %s",
			resp.StatusCode, githubapp.SanitizeBody(body, 256))
	}
	var conn AdminConnection
	if err := json.Unmarshal(body, &conn); err != nil {
		return nil, fmt.Errorf("decode admin connection: %w", err)
	}
	if conn.URL == "" || conn.Token == "" {
		return nil, fmt.Errorf("admin connection missing url or token (urlSet=%t tokenLen=%d)",
			conn.URL != "", len(conn.Token))
	}
	return &conn, nil
}

// svcCall issues one Actions Service call ({adminURL}{path}?api-version=6.0-preview)
// with the admin JWT, marshalling in (when non-nil) and decoding into out (when
// non-nil). Status codes map to the package's typed errors.
func (c *Client) svcCall(ctx context.Context, method, path string, in, out any) error {
	conn, err := c.admin(ctx)
	if err != nil {
		return err
	}
	u := strings.TrimSuffix(conn.URL, "/") + path
	if strings.Contains(path, "?") {
		u += "&api-version=" + apiVersion
	} else {
		u += "?api-version=" + apiVersion
	}

	var bodyReader io.Reader
	if in != nil {
		payload, err := json.Marshal(in)
		if err != nil {
			return err
		}
		bodyReader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, bodyReader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+conn.Token)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("scaleset: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	if err := statusError(resp.StatusCode, resp.Header, body); err != nil {
		return fmt.Errorf("scaleset: %s %s: %w", method, path, err)
	}
	if out != nil && len(body) > 0 {
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("scaleset: %s %s: decode response: %w", method, path, err)
		}
	}
	return nil
}

// statusError maps a non-2xx status to the package's typed error, or nil for 2xx.
func statusError(status int, header http.Header, body []byte) error {
	switch {
	case status >= 200 && status <= 299:
		return nil
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return &UnauthorizedError{StatusCode: status}
	case status == http.StatusConflict:
		return &SessionConflictError{StatusCode: status}
	case status == http.StatusNotFound || status == http.StatusGone:
		return &NotFoundError{StatusCode: status}
	case status == http.StatusTooManyRequests:
		return parseRateLimitError(header)
	default:
		return fmt.Errorf("unexpected status %d: %s", status, githubapp.SanitizeBody(body, 512))
	}
}

// ResolveRunnerGroup looks up a runner group's id by name, returning
// (id, true, nil) on a hit and (0, false, nil) when no group matches. Callers that
// want GitHub's default group on a miss can substitute 1 (the default-group id).
func (c *Client) ResolveRunnerGroup(ctx context.Context, name string) (int, bool, error) {
	var out struct {
		Count int           `json:"count"`
		Value []RunnerGroup `json:"value"`
	}
	if err := c.svcCall(ctx, http.MethodGet,
		"/_apis/runtime/runnergroups/?groupName="+url.QueryEscape(name), nil, &out); err != nil {
		return 0, false, err
	}
	if out.Count == 0 || len(out.Value) == 0 {
		return 0, false, nil
	}
	return out.Value[0].ID, true, nil
}

// CreateRunnerScaleSet creates a scale set and returns the server-assigned object
// (with its id). The scale set's single System label is its runs-on match target.
func (c *Client) CreateRunnerScaleSet(ctx context.Context, in RunnerScaleSet) (*RunnerScaleSet, error) {
	var out RunnerScaleSet
	if err := c.svcCall(ctx, http.MethodPost, "/_apis/runtime/runnerscalesets", in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetRunnerScaleSet fetches a scale set by id.
func (c *Client) GetRunnerScaleSet(ctx context.Context, id int) (*RunnerScaleSet, error) {
	var out RunnerScaleSet
	if err := c.svcCall(ctx, http.MethodGet,
		fmt.Sprintf("/_apis/runtime/runnerscalesets/%d", id), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetRunnerScaleSetByName looks a scale set up by name, returning (nil, nil) when
// none matches. Used to reconcile against an already-registered scale set (and to
// recover one leaked by an interrupted run).
func (c *Client) GetRunnerScaleSetByName(ctx context.Context, name string) (*RunnerScaleSet, error) {
	var out struct {
		Count int              `json:"count"`
		Value []RunnerScaleSet `json:"value"`
	}
	if err := c.svcCall(ctx, http.MethodGet,
		"/_apis/runtime/runnerscalesets?name="+url.QueryEscape(name), nil, &out); err != nil {
		return nil, err
	}
	if out.Count == 0 || len(out.Value) == 0 {
		return nil, nil
	}
	return &out.Value[0], nil
}

// UpdateRunnerScaleSet applies a PATCH to the scale set with the given id.
func (c *Client) UpdateRunnerScaleSet(ctx context.Context, id int, patch RunnerScaleSet) (*RunnerScaleSet, error) {
	var out RunnerScaleSet
	if err := c.svcCall(ctx, http.MethodPatch,
		fmt.Sprintf("/_apis/runtime/runnerscalesets/%d", id), patch, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteRunnerScaleSet deletes the scale set with the given id.
func (c *Client) DeleteRunnerScaleSet(ctx context.Context, id int) error {
	return c.svcCall(ctx, http.MethodDelete, fmt.Sprintf("/_apis/runtime/runnerscalesets/%d", id), nil, nil)
}

// GenerateJITConfig mints a just-in-time runner config for one job: the server
// pre-registers the runner and returns a base64 blob a runner pod consumes with
// run.sh --jitconfig. name is the runner's name; workFolder its work directory
// (conventionally "_work").
func (c *Client) GenerateJITConfig(ctx context.Context, scaleSetID int, name, workFolder string) (*JITRunnerConfig, error) {
	if workFolder == "" {
		workFolder = "_work"
	}
	var out JITRunnerConfig
	if err := c.svcCall(ctx, http.MethodPost,
		fmt.Sprintf("/_apis/runtime/runnerscalesets/%d/generatejitconfig", scaleSetID),
		map[string]string{"name": name, "workFolder": workFolder}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CreateSession opens the scale set's message-queue session (one active per scale
// set). ownerName identifies the listener. A 409 surfaces as *SessionConflictError.
func (c *Client) CreateSession(ctx context.Context, scaleSetID int, ownerName string) (*RunnerScaleSetSession, error) {
	var out RunnerScaleSetSession
	if err := c.svcCall(ctx, http.MethodPost,
		fmt.Sprintf("/_apis/runtime/runnerscalesets/%d/sessions", scaleSetID),
		map[string]string{"ownerName": ownerName}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RefreshSession refreshes the session's message-queue access token via PATCH,
// updating sess.MessageQueueAccessToken (and URL, if the server returns one) in
// place. Call it after GetMessage or AcquireJobs returns *UnauthorizedError — the
// queue token expired (Q264 plan §2b-2).
func (c *Client) RefreshSession(ctx context.Context, scaleSetID int, sess *RunnerScaleSetSession) error {
	var out RunnerScaleSetSession
	if err := c.svcCall(ctx, http.MethodPatch,
		fmt.Sprintf("/_apis/runtime/runnerscalesets/%d/sessions/%s", scaleSetID, sess.SessionID), nil, &out); err != nil {
		return err
	}
	if out.MessageQueueAccessToken != "" {
		sess.MessageQueueAccessToken = out.MessageQueueAccessToken
	}
	if out.MessageQueueURL != "" {
		sess.MessageQueueURL = out.MessageQueueURL
	}
	if out.Statistics != nil {
		sess.Statistics = out.Statistics
	}
	if c.metrics != nil {
		c.metrics.IncTokenRefresh("queue")
	}
	return nil
}

// DeleteSession tears down the scale set's session on shutdown, allowing a later
// re-create to replay unacked messages (Q264 plan §2b-3).
func (c *Client) DeleteSession(ctx context.Context, scaleSetID int, sessionID string) error {
	return c.svcCall(ctx, http.MethodDelete,
		fmt.Sprintf("/_apis/runtime/runnerscalesets/%d/sessions/%s", scaleSetID, sessionID), nil, nil)
}

// GetMessage issues one message-queue long-poll advertising capacity as the
// X-ScaleSetMaxCapacity header — the admission gate that bounds how many jobs the
// backend auto-assigns (Q264 plan §2b-1). lastMessageID is the cursor; pass 0 to
// re-read from the queue head (e.g. on a freshly re-created session, which replays
// unacked messages). Returns (nil, nil) on 202 (no message — poll again).
//
// The queue token, not the admin JWT, authorizes this call. On 401/403 the token
// expired: refresh it with RefreshSession and retry (the returned
// *UnauthorizedError signals exactly this). A 404/410 surfaces as *NotFoundError
// (session gone — re-create it); a 429 as *RateLimitError.
func (c *Client) GetMessage(ctx context.Context, sess *RunnerScaleSetSession, capacity int, lastMessageID int64) (*RunnerScaleSetMessage, error) {
	u := appendQuery(sess.MessageQueueURL, "lastMessageId="+strconv.FormatInt(lastMessageID, 10))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+sess.MessageQueueAccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-ScaleSetMaxCapacity", strconv.Itoa(capacity))

	resp, err := c.pollClient.Do(req)
	if err != nil {
		c.recordPollError(pollErrorReason(err))
		return nil, fmt.Errorf("scaleset: GetMessage: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	switch {
	case resp.StatusCode == http.StatusAccepted: // 202 — no message
		return nil, nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		c.recordPollError("unauthorized")
		return nil, &UnauthorizedError{StatusCode: resp.StatusCode}
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return nil, &NotFoundError{StatusCode: resp.StatusCode}
	case resp.StatusCode == http.StatusTooManyRequests:
		c.recordPollError("rate_limited")
		return nil, parseRateLimitError(resp.Header)
	case resp.StatusCode >= 500:
		c.recordPollError("server_error")
		return nil, fmt.Errorf("scaleset: GetMessage: unexpected status %d: %s",
			resp.StatusCode, githubapp.SanitizeBody(body, 256))
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("scaleset: GetMessage: unexpected status %d: %s",
			resp.StatusCode, githubapp.SanitizeBody(body, 256))
	}
	if len(body) == 0 {
		return nil, nil
	}
	var msg RunnerScaleSetMessage
	if err := json.Unmarshal(body, &msg); err != nil {
		return nil, fmt.Errorf("scaleset: GetMessage: decode response: %w", err)
	}
	return &msg, nil
}

// AcquireJobs claims the given RunnerRequestIDs (from JobAvailable messages) in one
// transactional batch and returns the subset actually won. This is the GHES path of
// the one-rule acquisition model: on the dotcom broker-host backend jobs auto-assign
// and this call is never made; on GHES the client must claim each offered id
// (Q264 plan §5a-U8). The QUEUE token authorizes it (not the admin JWT — §2.5).
//
// The URL is the scale set's _apis/runtime acquirejobs route (the ARC-era route
// GHES serves). Passing already-claimed ids simply omits them from the returned
// subset (claim-once). A 401/403 surfaces as *UnauthorizedError — refresh the
// session token and retry.
func (c *Client) AcquireJobs(ctx context.Context, scaleSetID int, sess *RunnerScaleSetSession, ids []int64) ([]int64, error) {
	conn, err := c.admin(ctx)
	if err != nil {
		return nil, err
	}
	u := fmt.Sprintf("%s/_apis/runtime/runnerscalesets/%d/acquirejobs?api-version=%s",
		strings.TrimSuffix(conn.URL, "/"), scaleSetID, apiVersion)
	if ids == nil {
		ids = []int64{}
	}
	payload, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	// The acquire call is authorized by the queue token, not the admin JWT (§2.5).
	req.Header.Set("Authorization", "Bearer "+sess.MessageQueueAccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("scaleset: AcquireJobs: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if err := statusError(resp.StatusCode, resp.Header, body); err != nil {
		return nil, fmt.Errorf("scaleset: AcquireJobs: %w", err)
	}
	var out struct {
		Count int     `json:"count"`
		Value []int64 `json:"value"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("scaleset: AcquireJobs: decode response: %w", err)
	}
	return out.Value, nil
}

// DeleteMessage acknowledges a message by deleting it from the queue. The full ack
// is "advance lastMessageId past it AND delete it" (Q264 plan §2.2); GetMessage's
// cursor handles the first half, this the second, so a redelivery on a re-created
// session (which polls from cursor 0) no longer replays an acked message.
//
// WIRE SHAPE SOURCE-DERIVED, NOT LIVE-PROBED: the DELETE endpoint
// ({messageQueueUrl}/{messageId}) is taken from the official actions/scaleset
// listener; the live probe acknowledged by cursor only and never exercised the
// message DELETE (Q264 plan §2.2 caveat). P4 live validation must confirm this URL
// shape and status semantics before the P3 listener relies on delete-based acking
// over pure cursor advance — flagged as a P2-surfaced unknown.
func (c *Client) DeleteMessage(ctx context.Context, sess *RunnerScaleSetSession, messageID int64) error {
	u, err := url.Parse(sess.MessageQueueURL)
	if err != nil {
		return fmt.Errorf("scaleset: DeleteMessage: parse queue URL: %w", err)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/" + strconv.FormatInt(messageID, 10)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+sess.MessageQueueAccessToken)

	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("scaleset: DeleteMessage: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	// A 404/410 means the message is already gone — a benign no-op for an ack.
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
		return nil
	}
	if err := statusError(resp.StatusCode, resp.Header, body); err != nil {
		return fmt.Errorf("scaleset: DeleteMessage: %w", err)
	}
	return nil
}

// recordPollError increments the poll-error metric when a recorder is wired.
func (c *Client) recordPollError(reason string) {
	if c.metrics != nil {
		c.metrics.IncPollError(reason)
	}
}

// appendQuery appends a raw query fragment to a URL, choosing ? or & correctly.
func appendQuery(u, frag string) string {
	if strings.Contains(u, "?") {
		return u + "&" + frag
	}
	return u + "?" + frag
}

// pollErrorReason classifies a transport-level GetMessage error for metrics.
func pollErrorReason(err error) string {
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "transport"
}

// parseRateLimitError builds a *RateLimitError from a 429 response's headers,
// honoring Retry-After (seconds) when present.
func parseRateLimitError(header http.Header) *RateLimitError {
	ra := header.Get("Retry-After")
	if ra == "" {
		return &RateLimitError{RetryAfter: -1}
	}
	secs, err := strconv.ParseFloat(ra, 64)
	if err != nil {
		return &RateLimitError{RetryAfter: -1}
	}
	return &RateLimitError{RetryAfter: time.Duration(secs * float64(time.Second))}
}

// parseJWTExpiry reads the exp claim from a JWT without verifying its signature —
// the admin JWT is GitHub's, so the client only needs its expiry to schedule a
// pre-expiry refresh, not to authenticate it. It base64url-decodes the payload
// segment and reads exp (seconds since the Unix epoch).
func parseJWTExpiry(token string) (time.Time, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, fmt.Errorf("not a JWT (%d segments)", len(parts))
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// Tolerate padded encodings just in case.
		payload, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return time.Time{}, fmt.Errorf("decode JWT payload: %w", err)
		}
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return time.Time{}, fmt.Errorf("decode JWT claims: %w", err)
	}
	if claims.Exp == 0 {
		return time.Time{}, errors.New("JWT has no exp claim")
	}
	return time.Unix(claims.Exp, 0), nil
}
