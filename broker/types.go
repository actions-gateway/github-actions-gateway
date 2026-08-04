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
	// Resources carries the job's service endpoints, including the
	// SystemVssConnection endpoint whose AccessToken is the job-scoped bearer
	// token for per-job operations — see JobAuthToken (Q247).
	Resources struct {
		Endpoints []ServiceEndpoint `json:"endpoints"`
	} `json:"resources"`
}

// JobAuthToken returns the job-scoped bearer token from the acquirejob response —
// the AccessToken authorization parameter of the SystemVssConnection endpoint — or
// "" when the response carries no such endpoint. The run service accepts the
// broker session token to *claim* a job but rejects it for per-job operations
// (renewjob, completejob) with 401 "Not authorized for this job", so callers
// must present this token instead, as the real runner does (Q247).
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
	// AuthToken authorizes this renewal — the job-scoped token, see
	// AcquireJobResponse.JobAuthToken (Q247). Empty falls back to Client.Token.
	// Never serialized into the request body.
	AuthToken string `json:"-"`
}

// RenewJobResponse is returned by POST {run_service_url}/renewjob.
type RenewJobResponse struct {
	// LockedUntil is typically ~10 minutes from the time of renewal.
	LockedUntil time.Time `json:"lockedUntil"`
}

// TaskResult is a job's terminal result as reported to
// POST {run_service_url}/completejob. The values mirror the runner SDK's
// TaskResult enum (Microsoft.TeamFoundation.DistributedTask.WebApi.TaskResult).
// The AGC itself sends the winner's pod-phase proxy when it releases a job
// assignment nothing else will report — a deduped sibling delivery (Q260 Option A)
// or its own delivery when the worker was removed before it ran (Q628, which sends
// TaskResultAbandoned). The remaining values are defined for wire fidelity.
//
// Wire format live-confirmed 2026-08-04: the run service accepted this lowercase
// camelCase serialization with a 204 (Investigation H, the Q645 probe). The AGC's
// completejob call stays gated off by default for a measured *semantic* reason
// instead: completing an acquired-but-never-run delivery with abandoned concluded
// the whole run as SUCCESS one second later — a job that never executed reports
// green (the Q645 findings). Remedy tracked as Q676.
type TaskResult string

const (
	// TaskResultSucceeded is a job that ran to a successful conclusion.
	TaskResultSucceeded TaskResult = "succeeded"
	// TaskResultSucceededWithIssues is a job that succeeded but logged warnings.
	TaskResultSucceededWithIssues TaskResult = "succeededWithIssues"
	// TaskResultFailed is a job that ran to a failed conclusion.
	TaskResultFailed TaskResult = "failed"
	// TaskResultCanceled is a job that was cancelled before or during execution.
	TaskResultCanceled TaskResult = "canceled"
	// TaskResultSkipped is a job assignment the runner acquired but did not
	// execute. The AGC reports this for a deduplicated duplicate delivery (the
	// Q260 loser): honest (acquired, ran nothing) and the smallest blast radius if
	// the run service maps a delivery's completion onto the whole job.
	TaskResultSkipped TaskResult = "skipped"
	// TaskResultAbandoned is a job the runner gave up without a conclusion. The AGC
	// reports it for a delivery whose worker pod was removed before any container
	// ran — an unschedulable worker reaped at spec.pendingPodDeadline, say: the
	// assignment is real and must be released, but no step executed, so neither
	// succeeded nor failed describes it (Q628).
	TaskResultAbandoned TaskResult = "abandoned"
)

// CompleteJobRequest is the request body for POST {run_service_url}/completejob.
// A runner sends it to report a job's terminal result. In GAG the worker pod's
// runner binary makes this call for a job it actually ran; the AGC itself sends it
// for an assignment it acquired but never ran — a deduplicated duplicate delivery
// (Q260 follow-up) and a delivery whose worker pod was removed before it started
// (Q628). Measured caveat: for the winner's own (sole) delivery this call does not
// release the job back to the queue — it concludes the run immediately, as success
// when the result is abandoned (Q645 Investigation H; remedy Q676). The deduped-
// sibling case, where the winner still reports the real result, is unmeasured.
//
// JobID is the delivery's own RunnerRequestID — distinct per sibling under GitHub's
// broker fan-out — so, under the per-delivery lock model the renew path relies on
// (Q247), completing it resolves only this phantom assignment and not the winner's.
type CompleteJobRequest struct {
	// PlanID comes from the acquirejob response (AcquireJobResponse.Plan.PlanID).
	PlanID string `json:"planId"`
	// JobID is RunnerJobRequestBody.RunnerRequestID — this delivery's own id.
	JobID string `json:"jobId"`
	// Result is the terminal result reported for the job assignment.
	Result TaskResult `json:"result"`
	// AuthToken authorizes this call — the job-scoped token, see
	// AcquireJobResponse.JobAuthToken (Q247). Empty falls back to Client.Token.
	// Never serialized into the request body.
	AuthToken string `json:"-"`
}
