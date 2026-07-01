// Package broker implements the GitHub Actions broker protocol client.
// It covers the four pre-execution calls: CreateSession, GetMessage,
// AcquireJob, and RenewJob, plus the DeleteSession teardown call.
//
// # Two-URL model
//
// GitHub's broker protocol uses two distinct base URLs that must never be
// conflated:
//
//   - broker_url  — static for a given runner registration; used by
//     CreateSession and GetMessage.
//   - run_service_url — dynamic, extracted from each GetMessage response body;
//     used for that job's AcquireJob and RenewJob calls.
//
// Caching run_service_url across jobs is the most common cause of mysterious
// 404 errors in custom broker clients.
package broker

import (
	"strings"
	"time"
)

// TaskAgentMessage is the response body from GET {broker_url}/message.
// MessageType is "RunnerJobRequest" when a job is available.
// Body is a JSON string that must be decrypted with DecryptMessageBody before
// being unmarshalled as RunnerJobRequestBody.
type TaskAgentMessage struct {
	MessageID   int64  `json:"messageId"`
	MessageType string `json:"messageType"`
	Body        string `json:"body"`
}

// RunnerJobRequestBody is the parsed (and decrypted) content of
// TaskAgentMessage.Body. It carries the two per-job URLs required for the
// remainder of the execution protocol.
type RunnerJobRequestBody struct {
	// RunnerRequestID is used as jobMessageId in AcquireJob and as jobId in
	// RenewJob.
	RunnerRequestID string `json:"runner_request_id"`
	// RunServiceURL is the base URL for AcquireJob and RenewJob. It is
	// per-job and must not be cached globally across jobs.
	RunServiceURL  string `json:"run_service_url"`
	BillingOwnerID string `json:"billing_owner_id"`
}

// JobAcquisitionRequest is the request body for POST {run_service_url}/acquirejob.
type JobAcquisitionRequest struct {
	// JobMessageID is RunnerJobRequestBody.RunnerRequestID.
	JobMessageID   string `json:"jobMessageId"`
	RunnerOS       string `json:"runnerOS"` // e.g. "Linux"
	BillingOwnerID string `json:"billingOwnerId"`
}

// ServiceEndpoint is one entry in AcquireJobResponse.Resources.Endpoints. The run
// service returns the job's service endpoints in the acquirejob response; the
// SystemVssConnection endpoint carries the job-scoped OAuth token the runner must
// present for that job's subsequent calls (RenewJob) — see
// AcquireJobResponse.JobAuthToken.
type ServiceEndpoint struct {
	Name          string `json:"name"`
	URL           string `json:"url"`
	Authorization struct {
		Scheme     string            `json:"scheme"`
		Parameters map[string]string `json:"parameters"`
	} `json:"authorization"`
}

// systemVssConnectionName is the well-known name of the acquirejob-response
// endpoint whose AccessToken authorization parameter is the job-scoped bearer
// token. Matches WellKnownServiceEndpointNames.SystemVssConnection in the runner
// SDK.
const systemVssConnectionName = "SystemVssConnection"

// accessTokenParam is the authorization-parameters key holding the job token,
// matching EndpointAuthorizationParameters.AccessToken in the runner SDK.
const accessTokenParam = "AccessToken"

// AcquireJobResponse is the parsed response from POST {run_service_url}/acquirejob.
// The AGC extracts PlanID for renewal and the job-scoped auth token
// (JobAuthToken); the full raw response bytes are stored alongside it and
// forwarded opaquely to the worker pod.
// PlanID is populated by AcquireJob from the x-plan-id response header
// (preferred) or .plan.planId in the body (fallback).
type AcquireJobResponse struct {
	Plan struct {
		PlanID string `json:"planId"`
	} `json:"plan"`
	// Resources carries the job's service endpoints. The SystemVssConnection
	// endpoint's AccessToken is the job-scoped bearer token RenewJob must present:
	// the run service accepts the broker/registration OAuth token to *claim* a job
	// (acquirejob) but rejects that same token when *renewing* the job's lock,
	// returning 401 "Not authorized for this job" — so on a job that outlives the
	// initial ~10-minute lock TTL the lock is never renewed and GitHub recycles it
	// at exactly the TTL boundary (Q247). See JobAuthToken.
	Resources struct {
		Endpoints []ServiceEndpoint `json:"endpoints"`
	} `json:"resources"`
}

// JobAuthToken returns the job-scoped bearer token from the acquirejob response —
// the AccessToken authorization parameter of the SystemVssConnection endpoint — or
// "" when the response carries no such endpoint. This is the same token the real
// runner uses to renew a job lock (VssUtil.GetVssCredential over the message's
// SystemVssConnection endpoint); the listener passes it to RenewJob as
// RenewJobRequest.AuthToken so per-job renewal is authorized (Q247).
func (r *AcquireJobResponse) JobAuthToken() string {
	for i := range r.Resources.Endpoints {
		if !strings.EqualFold(r.Resources.Endpoints[i].Name, systemVssConnectionName) {
			continue
		}
		// Match the parameter key case-insensitively: it is a well-known constant
		// ("AccessToken") but a case quirk in the wire form must not silently drop
		// the token and re-trigger the Q247 401 loop.
		for k, v := range r.Resources.Endpoints[i].Authorization.Parameters {
			if strings.EqualFold(k, accessTokenParam) {
				return v
			}
		}
	}
	return ""
}

// RenewJobRequest is the request body for POST {run_service_url}/renewjob.
// Must be sent every 60 seconds after AcquireJob succeeds.
type RenewJobRequest struct {
	// PlanID comes from the AcquireJob response.
	PlanID string `json:"planId"`
	// JobID is RunnerJobRequestBody.RunnerRequestID.
	JobID string `json:"jobId"`
	// AuthToken is the job-scoped bearer token (AcquireJobResponse.JobAuthToken)
	// that authorizes this renewal. When non-empty, RenewJob presents it as the
	// Authorization header instead of Client.Token, because the run service rejects
	// the broker session token for per-job lock renewal with 401 "Not authorized
	// for this job" (Q247). Empty falls back to Client.Token. Never serialized into
	// the request body.
	AuthToken string `json:"-"`
}

// RenewJobResponse is returned by POST {run_service_url}/renewjob.
type RenewJobResponse struct {
	// LockedUntil is typically ~10 minutes from the time of renewal.
	LockedUntil time.Time `json:"lockedUntil"`
}
