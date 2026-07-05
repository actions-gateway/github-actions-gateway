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
