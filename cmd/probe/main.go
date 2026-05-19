// Command probe exercises the complete GitHub Actions broker wire protocol.
//
// It authenticates via GitHub App credentials, registers a virtual runner
// session, long-polls for a job, decrypts and acquires it, then renews the
// job lock every 60 seconds until interrupted.
//
// Required environment variables:
//
//	GITHUB_APP_ID              - GitHub App numeric ID
//	GITHUB_APP_PRIVATE_KEY     - Path to PEM file, or PEM literal
//	GITHUB_APP_INSTALLATION_ID - Installation ID for the target org/repo
//	GITHUB_BROKER_URL          - Broker base URL (e.g. https://pipelines.actions.githubusercontent.com/...)
//	GITHUB_RUNNER_VERSION      - Runner version string (e.g. 2.327.1)
//	GITHUB_AGENT_ID            - Registered agent ID from the runner's .runner config (agentId field)
//	GITHUB_AGENT_NAME          - Registered agent name from the runner's .runner config (agentName field)
//	GITHUB_USE_V2_FLOW         - "true" to use broker v2 API (/session, /message); default: v1 VSTS pool API
//	GITHUB_BROKER_URL_V2       - Broker v2 base URL (serverUrlV2 from .runner, e.g. https://broker.actions.githubusercontent.com/)
//	GITHUB_RUNNER_OS           - OS string for v2 message polls (e.g. "osx", "linux")
//	GITHUB_RUNNER_ARCH         - Arch string for v2 message polls (e.g. "x64", "arm64")
//
// The decrypted AcquireJob response body is printed to stdout as JSON.
// Pipe it to testdata/job_payload.json after a successful run (redact
// ACTIONS_RUNTIME_TOKEN before committing).
package main

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/karlkfi/github-actions-gateway/broker"
	"github.com/karlkfi/github-actions-gateway/githubapp"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	if err := run(logger); err != nil {
		logger.Error("probe failed", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	// ── 1. Read credentials from environment ────────────────────────────────
	appID, err := strconv.ParseInt(mustEnv("GITHUB_APP_ID"), 10, 64)
	if err != nil {
		return fmt.Errorf("parse GITHUB_APP_ID: %w", err)
	}
	installID, err := strconv.ParseInt(mustEnv("GITHUB_APP_INSTALLATION_ID"), 10, 64)
	if err != nil {
		return fmt.Errorf("parse GITHUB_APP_INSTALLATION_ID: %w", err)
	}
	pemBytes, err := loadPEM(mustEnv("GITHUB_APP_PRIVATE_KEY"))
	if err != nil {
		return fmt.Errorf("load GITHUB_APP_PRIVATE_KEY: %w", err)
	}
	brokerURL := mustEnv("GITHUB_BROKER_URL")
	runnerVersion := mustEnv("GITHUB_RUNNER_VERSION")
	agentName := mustEnv("GITHUB_AGENT_NAME")
	agentID, err := strconv.ParseInt(mustEnv("GITHUB_AGENT_ID"), 10, 64)
	if err != nil {
		return fmt.Errorf("parse GITHUB_AGENT_ID: %w", err)
	}
	runnerOS := os.Getenv("GITHUB_RUNNER_OS")
	runnerArch := os.Getenv("GITHUB_RUNNER_ARCH")
	useV2Flow := os.Getenv("GITHUB_USE_V2_FLOW") == "true"
	// In v2 flow, the broker API lives at serverUrlV2, not serverUrl.
	if useV2Flow {
		if v2URL := os.Getenv("GITHUB_BROKER_URL_V2"); v2URL != "" {
			brokerURL = v2URL
		}
	}
	poolID := 1
	if v := os.Getenv("GITHUB_POOL_ID"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("parse GITHUB_POOL_ID: %w", err)
		}
		poolID = n
	}

	// ── 2. Mint installation access token ───────────────────────────────────
	creds := githubapp.Credentials{
		AppID:          appID,
		PrivateKeyPEM:  pemBytes,
		InstallationID: installID,
	}
	provider, err := githubapp.NewInstallationTokenProvider(creds, nil)
	if err != nil {
		return fmt.Errorf("create token provider: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	token, err := provider.Token(ctx)
	if err != nil {
		return fmt.Errorf("get installation token: %w", err)
	}
	logger.Info("obtained installation access token")

	// ── 3. Obtain broker token (runner OAuth) ───────────────────────────────
	//
	// The VSTS Task Agent API (sessions, messages) requires the runner's OAuth
	// token, NOT the GitHub App installation token.  After config.sh, GitHub
	// writes two credential files:
	//   .credentials           — clientId + authorizationUrl
	//   .credentials_rsaparams — RSA private key in .NET RSAParameters format
	//
	// We exchange these for a short-lived OAuth2 bearer token via RFC 7523
	// JWT bearer assertion.  The script exports the paths; fall back to the
	// installation token only when the files are absent (e.g. unit-test mode).
	// runnerKey is kept in scope so it can decrypt the AES session key from
	// the CreateSession response later. It is nil when credential files are absent.
	var runnerKey *rsa.PrivateKey
	var brokerToken string
	credsFile := os.Getenv("GITHUB_RUNNER_CREDENTIALS_FILE")
	rsaFile := os.Getenv("GITHUB_RUNNER_RSA_PARAMS_FILE")
	if credsFile != "" && rsaFile != "" {
		runnerCreds, err := githubapp.ParseRunnerCredentials(credsFile)
		if err != nil {
			return fmt.Errorf("parse runner credentials: %w", err)
		}
		runnerKey, err = githubapp.ParseRunnerRSAKey(rsaFile)
		if err != nil {
			return fmt.Errorf("parse runner rsa key: %w", err)
		}
		brokerToken, err = githubapp.FetchRunnerOAuthToken(ctx, runnerCreds, runnerKey, nil)
		if err != nil {
			return fmt.Errorf("fetch runner OAuth token: %w", err)
		}
		logger.Info("obtained runner OAuth token via JWT bearer assertion")
	} else {
		logger.Warn("GITHUB_RUNNER_CREDENTIALS_FILE/RSA_PARAMS_FILE not set; using installation token (expect 401)")
		brokerToken = token
	}

	bc := &broker.BrokerClient{
		BrokerURL:     brokerURL,
		PoolID:        poolID,
		Token:         brokerToken,
		UseV2Flow:     useV2Flow,
		RunnerVersion: runnerVersion,
		RunnerOS:      runnerOS,
		RunnerArch:    runnerArch,
	}
	sess, err := bc.CreateSession(ctx, agentID, agentName, runnerVersion)
	if err != nil {
		return fmt.Errorf("CreateSession: %w", err)
	}
	bc.BrokerURL = sess.BrokerURL
	logger.Info("session created", "sessionId", sess.SessionID, "brokerURL", sess.BrokerURL,
		"hasEncryptionKey", sess.EncryptionKey != nil)

	sessionID := sess.SessionID

	// Ensure DeleteSession runs on exit regardless of success or error path.
	defer func() {
		deleteCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if delErr := bc.DeleteSession(deleteCtx, sessionID); delErr != nil {
			logger.Error("DeleteSession failed", "error", delErr)
		} else {
			logger.Info("session deleted", "sessionId", sessionID)
		}
	}()

	// ── 4. Long-poll for a job ───────────────────────────────────────────────
	//
	// Retry policy mirrors MessageListener.cs:
	//   Up to 5 consecutive errors: [15s, 30s] jitter.
	//   Beyond 5 consecutive errors: [30s, 60s] jitter.
	var msg *broker.TaskAgentMessage
	var sessionKey []byte
	consecutiveErrors := 0

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		got, pollErr := bc.GetMessage(ctx, sessionID)
		if pollErr != nil {
			consecutiveErrors++
			delay := backoffDelay(consecutiveErrors)
			logger.Warn("GetMessage error", "error", pollErr, "consecutiveErrors", consecutiveErrors, "retryAfter", delay)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
			continue
		}
		consecutiveErrors = 0

		if got == nil {
			logger.Debug("no job queued, polling again…")
			continue
		}
		if got.MessageType != "RunnerJobRequest" {
			logger.Debug("ignoring message", "type", got.MessageType)
			continue
		}

		msg = got
		logger.Info("job message received", "messageId", msg.MessageID)

			// Decrypt the session key from the CreateSession response using the
		// runner's RSA private key, or fall back to the GITHUB_SESSION_KEY env var.
		if len(sess.EncryptionKey) > 0 && runnerKey != nil {
			sessionKey, err = broker.DecryptSessionKey(sess.EncryptionKey, runnerKey)
			if err != nil {
				logger.Warn("failed to decrypt session key from CreateSession response; falling back to GITHUB_SESSION_KEY", "error", err)
			} else {
				logger.Info("session key decrypted from CreateSession response")
			}
		}
		if len(sessionKey) == 0 {
			sessionKeyB64 := os.Getenv("GITHUB_SESSION_KEY")
			if sessionKeyB64 != "" {
				sessionKey, err = base64.StdEncoding.DecodeString(sessionKeyB64)
				if err != nil {
					return fmt.Errorf("decode GITHUB_SESSION_KEY: %w", err)
				}
			}
		}
		break
	}

	// ── 5. Decrypt the message body ──────────────────────────────────────────
	var jobReq broker.RunnerJobRequestBody
	if len(sessionKey) > 0 {
		plaintext, decErr := broker.DecryptMessageBody(msg.Body, sessionKey)
		if decErr != nil {
			return fmt.Errorf("decrypt message body: %w", decErr)
		}
		if jsonErr := json.Unmarshal(plaintext, &jobReq); jsonErr != nil {
			return fmt.Errorf("unmarshal job request body: %w", jsonErr)
		}
		logger.Info("message body decrypted", "runnerRequestId", jobReq.RunnerRequestID, "runServiceURL", jobReq.RunServiceURL)
	} else {
		// When no session key is provided (investigation mode), attempt to
		// unmarshal the body directly — it may already be plaintext JSON in
		// some broker configurations.
		logger.Warn("GITHUB_SESSION_KEY not set; attempting to parse body as plaintext JSON")
		if jsonErr := json.Unmarshal([]byte(msg.Body), &jobReq); jsonErr != nil {
			return fmt.Errorf("parse unencrypted body: %w", jsonErr)
		}
	}

	// ── 6. Acquire the job ───────────────────────────────────────────────────
	acqResp, rawPayload, err := bc.AcquireJob(ctx, jobReq.RunServiceURL, broker.JobAcquisitionRequest{
		JobMessageID:   jobReq.RunnerRequestID,
		RunnerOS:       "Linux",
		BillingOwnerID: jobReq.BillingOwnerID,
	})
	if err != nil {
		return fmt.Errorf("AcquireJob: %w", err)
	}
	logger.Info("job acquired", "planId", acqResp.Plan.PlanID)

	// ── 7. Print full payload (pipe to testdata/job_payload.json) ────────────
	fmt.Println(string(rawPayload))

	// ── 8. Start renewjob loop ───────────────────────────────────────────────
	renewDone := make(chan struct{})
	go func() {
		defer close(renewDone)
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case t := <-ticker.C:
				renewResp, renewErr := bc.RenewJob(ctx, jobReq.RunServiceURL, broker.RenewJobRequest{
					PlanID: acqResp.Plan.PlanID,
					JobID:  jobReq.RunnerRequestID,
				})
				if renewErr != nil {
					logger.Error("RenewJob failed", "error", renewErr, "tick", t)
					return
				}
				logger.Info("job renewed", "lockedUntil", renewResp.LockedUntil)
			}
		}
	}()

	// ── 9. Optional: AcknowledgeRunnerRequest (Investigation A) ──────────────
	// Attempt the VSTS delete-message call and record the result; this feeds
	// into the Investigation A findings in milestone-1.md.
	// The VSTS Task Agent protocol acknowledges job delivery by deleting the
	// message: DELETE {poolBase}/messages/{messageId}?sessionId={sessionId}
	acknowledgeResult := probeAcknowledge(ctx, logger, bc, msg.MessageID, sessionID)
	logger.Info("AcknowledgeRunnerRequest result", "status", acknowledgeResult)

	// ── Investigation C: Session Reuse After acquirejob ──────────────────────
	// Set PROBE_SESSION_REUSE_TEST=true and queue a second workflow job before
	// running. The probe re-enters GetMessage on the same sessionId and reports
	// whether the session is still valid. Findings feed §8.C of milestone-1.md.
	if os.Getenv("PROBE_SESSION_REUSE_TEST") == "true" {
		investigateSessionReuse(ctx, logger, bc, sessionID)
	}

	// ── Investigation D: Job Delivery Throttling by Session Count ────────────
	// Set PROBE_JOB_DELIVERY_TEST=true and queue a second workflow job before
	// running. The probe registers a second session after the first job is
	// acquired and checks whether the second job is delivered. Findings feed
	// §8.D of milestone-1.md.
	if os.Getenv("PROBE_JOB_DELIVERY_TEST") == "true" {
		investigateJobDelivery(ctx, logger, bc, agentID, agentName, runnerVersion)
	}

	// ── 10. Block until interrupted ──────────────────────────────────────────
	<-ctx.Done()
	logger.Info("shutdown signal received; waiting for renew goroutine")
	<-renewDone
	return nil
}

// probeAcknowledge attempts the VSTS Task Agent delete-message call which is
// the standard way to acknowledge job delivery:
//
//	DELETE {poolBase}/messages/{messageId}?sessionId={sessionId}
//
// We make the call directly rather than via a dedicated BrokerClient method —
// the method will only be added once Investigation A confirms this call is
// required for correct delivery semantics.
//
// Document findings (HTTP status, response body, effect of omitting the call)
// in docs/plan/milestone-1.md §8.A before closing Milestone 1.
func probeAcknowledge(ctx context.Context, logger *slog.Logger, bc *broker.BrokerClient, messageID int64, sessionID string) string {
	url := fmt.Sprintf("%s/messages/%d?sessionId=%s", bc.PoolBase(), messageID, sessionID)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return fmt.Sprintf("build-request-error: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+bc.Token)
	req.Header.Set("Accept", "application/json")

	client := bc.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Sprintf("request-error: %v", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	logger.Info("AcknowledgeRunnerRequest response",
		"status", resp.StatusCode,
		"body", string(respBody),
	)
	// Return a compact status string for the top-level log line.
	return fmt.Sprintf("HTTP-%d", resp.StatusCode)
}

// investigateSessionReuse tests whether a session remains valid for GetMessage
// polling immediately after a successful AcquireJob call (Investigation C).
//
// Before running with PROBE_SESSION_REUSE_TEST=true, queue a second workflow
// job so it is waiting when the probe re-enters the poll loop. The probe polls
// for up to 3 minutes and logs a clear CONFIRMED or inconclusive result.
func investigateSessionReuse(ctx context.Context, logger *slog.Logger, bc *broker.BrokerClient, sessionID string) {
	logger.Info("INVESTIGATION-C: re-entering GetMessage on same sessionId after AcquireJob",
		"sessionId", sessionID,
		"instruction", "queue a second workflow job NOW if not already queued")

	deadline, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	for pollCount := 1; ; pollCount++ {
		if deadline.Err() != nil {
			logger.Warn("INVESTIGATION-C: timeout — no second job received within 3 minutes",
				"pollCount", pollCount,
				"finding", "inconclusive: no second job arrived; repeat with job pre-queued before re-entry")
			return
		}
		got, err := bc.GetMessage(deadline, sessionID)
		if err != nil {
			// Distinguish between a context cancellation (deadline/SIGINT — the
			// session itself was live) and a protocol-level rejection (404/410 —
			// the session was invalidated server-side after AcquireJob).
			if deadline.Err() != nil {
				logger.Info("INVESTIGATION-C: polling deadline reached — session was live throughout (202 responses; no session error)",
					"pollCount", pollCount,
					"finding", "session reuse supported: session remained valid; no second job arrived before timeout")
			} else {
				logger.Error("INVESTIGATION-C: GetMessage returned protocol error — session invalidated after AcquireJob",
					"error", err,
					"pollCount", pollCount,
					"finding", "session reuse NOT supported; delete+create cycle required between jobs")
			}
			return
		}
		if got == nil {
			logger.Debug("INVESTIGATION-C: 202 no-job — session still live", "pollCount", pollCount)
			continue
		}
		if got.MessageType == "RunnerJobRequest" {
			logger.Info("INVESTIGATION-C: second RunnerJobRequest received — SESSION REUSE CONFIRMED",
				"messageId", got.MessageID,
				"pollCount", pollCount,
				"finding", "session remains valid after AcquireJob; goroutine can loop without delete+create")
			return
		}
		logger.Debug("INVESTIGATION-C: ignoring non-job message", "type", got.MessageType)
	}
}

// investigateJobDelivery tests whether GitHub delivers a queued job to a
// session that was registered *after* the job was queued (Investigation D).
//
// Before running with PROBE_JOB_DELIVERY_TEST=true, queue a second workflow
// job after the first job is acquired. The probe registers a second session
// and polls to see whether the second job is delivered. Polls for 3 minutes.
func investigateJobDelivery(ctx context.Context, logger *slog.Logger, bc *broker.BrokerClient, agentID int64, agentName, runnerVersion string) {
	logger.Info("INVESTIGATION-D: registering second session after first job acquired",
		"instruction", "queue a SECOND workflow job NOW if not already queued")

	bc2 := &broker.BrokerClient{
		BrokerURL:     bc.BrokerURL,
		PoolID:        bc.PoolID,
		Token:         bc.Token,
		UseV2Flow:     bc.UseV2Flow,
		RunnerVersion: bc.RunnerVersion,
		RunnerOS:      bc.RunnerOS,
		RunnerArch:    bc.RunnerArch,
		HTTPClient:    bc.HTTPClient,
	}

	sess2, err := bc2.CreateSession(ctx, agentID, agentName, runnerVersion)
	if err != nil {
		logger.Error("INVESTIGATION-D: CreateSession for second session failed", "error", err)
		return
	}
	bc2.BrokerURL = sess2.BrokerURL
	logger.Info("INVESTIGATION-D: second session created", "sessionId2", sess2.SessionID)

	defer func() {
		dCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if delErr := bc2.DeleteSession(dCtx, sess2.SessionID); delErr != nil {
			logger.Error("INVESTIGATION-D: DeleteSession for second session failed", "error", delErr)
		} else {
			logger.Info("INVESTIGATION-D: second session deleted", "sessionId2", sess2.SessionID)
		}
	}()

	deadline, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	for pollCount := 1; ; pollCount++ {
		if deadline.Err() != nil {
			logger.Warn("INVESTIGATION-D: timeout — no job delivered to second session within 3 minutes",
				"pollCount", pollCount,
				"finding", "possible throttling: delivery may be bound to sessions present at queue time")
			return
		}
		got, err := bc2.GetMessage(deadline, sess2.SessionID)
		if err != nil {
			logger.Error("INVESTIGATION-D: GetMessage on second session error", "error", err, "pollCount", pollCount)
			return
		}
		if got == nil {
			logger.Debug("INVESTIGATION-D: 202 — second session sees no job yet", "pollCount", pollCount)
			continue
		}
		if got.MessageType == "RunnerJobRequest" {
			logger.Info("INVESTIGATION-D: second session received job — OPPORTUNISTIC DELIVERY CONFIRMED",
				"messageId", got.MessageID,
				"pollCount", pollCount,
				"finding", "GitHub delivers to any ready session; adaptive model is safe; no standby pool needed")
			return
		}
		logger.Debug("INVESTIGATION-D: non-job message on second session", "type", got.MessageType)
	}
}

// backoffDelay returns a randomised delay for GetMessage retries based on the
// number of consecutive errors, matching the two-tier policy in MessageListener.cs.
func backoffDelay(consecutiveErrors int) time.Duration {
	if consecutiveErrors <= 5 {
		return jitter(15*time.Second, 30*time.Second)
	}
	return jitter(30*time.Second, 60*time.Second)
}

// jitter returns a random duration in [lo, hi].
func jitter(lo, hi time.Duration) time.Duration {
	return lo + time.Duration(rand.Int63n(int64(hi-lo+1)))
}

// mustEnv returns the value of the named environment variable or panics.
func mustEnv(name string) string {
	v := os.Getenv(name)
	if v == "" {
		fmt.Fprintf(os.Stderr, "required environment variable %s is not set\n", name)
		os.Exit(1)
	}
	return v
}

// loadPEM returns the PEM bytes from either a file path or a PEM literal.
// If value contains a PEM header (starts with "-----"), it is returned as-is.
// Otherwise it is treated as a file path and the file is read.
func loadPEM(value string) ([]byte, error) {
	const pemHeader = "-----"
	if len(value) >= len(pemHeader) && value[:len(pemHeader)] == pemHeader {
		return []byte(value), nil
	}
	return os.ReadFile(value)
}
