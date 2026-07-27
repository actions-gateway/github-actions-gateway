package scaleset_test

import (
	"encoding/json"
	"testing"

	"github.com/actions-gateway/github-actions-gateway/scaleset"
)

func TestMessage_Jobs(t *testing.T) {
	body, _ := json.Marshal([]scaleset.JobMessage{
		{MessageType: scaleset.MessageTypeJobAvailable, RunnerRequestID: 10},
		{MessageType: scaleset.MessageTypeJobAssigned, JobID: "j-1"},
	})
	msg := scaleset.RunnerScaleSetMessage{Body: string(body)}
	jobs, err := msg.Jobs()
	if err != nil {
		t.Fatalf("Jobs: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("len(jobs) = %d, want 2", len(jobs))
	}

	// Empty body → no entries, no error.
	empty := scaleset.RunnerScaleSetMessage{}
	if got, err := empty.Jobs(); err != nil || got != nil {
		t.Errorf("empty Jobs = %v, %v; want nil, nil", got, err)
	}

	// Malformed body surfaces an error.
	if _, err := (&scaleset.RunnerScaleSetMessage{Body: "not-json"}).Jobs(); err == nil {
		t.Error("malformed body should error")
	}
}

// TestJobMessage_RunIdentity decodes a JobAssigned body in the wire shape the official
// actions/scaleset client models (JobMessageBase embedded in every job message) and
// checks the run identity comes out whole. It is the decode half of Q417: without
// ownerName/repositoryName/workflowRunId there is nothing for scale-set eviction
// recovery to re-run, so the field names and types are load-bearing, not cosmetic.
//
// The body is written as raw JSON rather than marshalled from JobMessage so a renamed
// or retyped field fails the test instead of silently round-tripping.
func TestJobMessage_RunIdentity(t *testing.T) {
	const body = `[{
		"messageType": "JobAssigned",
		"runnerRequestId": 0,
		"jobId": "8f3c1e2a-0000-4b1a-9c3d-1a2b3c4d5e6f",
		"ownerName": "myorg",
		"repositoryName": "myrepo",
		"workflowRunId": 17654321,
		"jobDisplayName": "build (ubuntu-latest)",
		"jobWorkflowRef": "myorg/myrepo/.github/workflows/ci.yml@refs/heads/main",
		"eventName": "push",
		"scaleSetAssignTime": "2026-07-26T12:00:00Z"
	}]`

	jobs, err := (&scaleset.RunnerScaleSetMessage{Body: body}).Jobs()
	if err != nil {
		t.Fatalf("Jobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("len(jobs) = %d, want 1", len(jobs))
	}

	owner, repo, runID, ok := jobs[0].RunIdentity()
	if !ok {
		t.Fatalf("RunIdentity ok = false, want true (owner=%q repo=%q runID=%q)", owner, repo, runID)
	}
	if owner != "myorg" || repo != "myrepo" || runID != "17654321" {
		t.Errorf("RunIdentity = %q/%q run %q, want myorg/myrepo run 17654321", owner, repo, runID)
	}
	if got := jobs[0].JobDisplayName; got != "build (ubuntu-latest)" {
		t.Errorf("JobDisplayName = %q", got)
	}
}

// TestJobMessage_RunIdentityIncomplete pins the degrade contract: any missing piece
// yields ok=false and no partial triple. A run is addressed by all three fields, so a
// caller must never be handed two of them and left to improvise the third — an empty
// owner or a run_id of 0 would produce a rerun request against the wrong thing.
func TestJobMessage_RunIdentityIncomplete(t *testing.T) {
	tests := map[string]scaleset.JobMessage{
		"nothing":       {MessageType: scaleset.MessageTypeJobAssigned, JobID: "j"},
		"no owner":      {RepositoryName: "myrepo", WorkflowRunID: 7},
		"no repository": {OwnerName: "myorg", WorkflowRunID: 7},
		"no run id":     {OwnerName: "myorg", RepositoryName: "myrepo"},
	}
	for name, msg := range tests {
		t.Run(name, func(t *testing.T) {
			owner, repo, runID, ok := msg.RunIdentity()
			if ok {
				t.Fatal("RunIdentity ok = true, want false")
			}
			if owner != "" || repo != "" || runID != "" {
				t.Errorf("incomplete identity leaked %q/%q run %q; want all empty", owner, repo, runID)
			}
		})
	}
}

func TestAvailableJobIDsAndAssignedJobs(t *testing.T) {
	jobs := []scaleset.JobMessage{
		{MessageType: scaleset.MessageTypeJobAvailable, RunnerRequestID: 1},
		{MessageType: scaleset.MessageTypeJobAvailable, RunnerRequestID: 2},
		{MessageType: scaleset.MessageTypeJobAssigned, JobID: "a"},
		{MessageType: scaleset.MessageTypeJobStarted, RunnerName: "r1"},
		{MessageType: scaleset.MessageTypeJobCompleted, Result: "succeeded"},
	}
	ids := scaleset.AvailableJobIDs(jobs)
	if len(ids) != 2 || ids[0] != 1 || ids[1] != 2 {
		t.Errorf("AvailableJobIDs = %v, want [1 2]", ids)
	}
	assigned := scaleset.AssignedJobs(jobs)
	if len(assigned) != 1 || assigned[0].JobID != "a" {
		t.Errorf("AssignedJobs = %v, want one JobAssigned", assigned)
	}

	// The auto-assign case: no JobAvailable, JobAssigned is authoritative.
	auto := []scaleset.JobMessage{{MessageType: scaleset.MessageTypeJobAssigned, JobID: "z"}}
	if got := scaleset.AvailableJobIDs(auto); got != nil {
		t.Errorf("auto-assign AvailableJobIDs = %v, want nil", got)
	}
	if got := scaleset.AssignedJobs(auto); len(got) != 1 {
		t.Errorf("auto-assign AssignedJobs = %v, want one", got)
	}
}
