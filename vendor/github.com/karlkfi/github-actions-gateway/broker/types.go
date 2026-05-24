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

import "time"

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
	RunnerOS       string `json:"runnerOS"`      // e.g. "Linux"
	BillingOwnerID string `json:"billingOwnerId"`
}

// AcquireJobResponse is the parsed response from POST {run_service_url}/acquirejob.
// The AGC only extracts PlanID for renewal; the full raw response bytes are
// stored alongside it and forwarded opaquely to the worker pod.
// PlanID is populated by AcquireJob from the x-plan-id response header
// (preferred) or .plan.planId in the body (fallback).
type AcquireJobResponse struct {
	Plan struct {
		PlanID string `json:"planId"`
	} `json:"plan"`
}

// RenewJobRequest is the request body for POST {run_service_url}/renewjob.
// Must be sent every 60 seconds after AcquireJob succeeds.
type RenewJobRequest struct {
	// PlanID comes from the AcquireJob response.
	PlanID string `json:"planId"`
	// JobID is RunnerJobRequestBody.RunnerRequestID.
	JobID string `json:"jobId"`
}

// RenewJobResponse is returned by POST {run_service_url}/renewjob.
type RenewJobResponse struct {
	// LockedUntil is typically ~10 minutes from the time of renewal.
	LockedUntil time.Time `json:"lockedUntil"`
}
