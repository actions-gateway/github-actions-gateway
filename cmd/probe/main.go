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
//
// The decrypted AcquireJob response body is printed to stdout as JSON.
// Pipe it to testdata/job_payload.json after a successful run (redact
// ACTIONS_RUNTIME_TOKEN before committing).
package main

import (
	"context"
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
	var brokerToken string
	credsFile := os.Getenv("GITHUB_RUNNER_CREDENTIALS_FILE")
	rsaFile := os.Getenv("GITHUB_RUNNER_RSA_PARAMS_FILE")
	if credsFile != "" && rsaFile != "" {
		runnerCreds, err := githubapp.ParseRunnerCredentials(credsFile)
		if err != nil {
			return fmt.Errorf("parse runner credentials: %w", err)
		}
		runnerKey, err := githubapp.ParseRunnerRSAKey(rsaFile)
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
		BrokerURL: brokerURL,
		PoolID:    poolID,
		Token:     brokerToken,
	}
	sessionID, activeBrokerURL, err := bc.CreateSession(ctx, runnerVersion)
	if err != nil {
		return fmt.Errorf("CreateSession: %w", err)
	}
	bc.BrokerURL = activeBrokerURL
	logger.Info("session created", "sessionId", sessionID, "brokerURL", activeBrokerURL)

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

		// TODO(milestone-2): session key will be returned in CreateSession response
		// and stored per session. For the probe, we read it from an env var as a
		// temporary stand-in while the full session handshake is mapped out.
		sessionKeyB64 := os.Getenv("GITHUB_SESSION_KEY")
		if sessionKeyB64 != "" {
			sessionKey, err = base64.StdEncoding.DecodeString(sessionKeyB64)
			if err != nil {
				return fmt.Errorf("decode GITHUB_SESSION_KEY: %w", err)
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
