package provisioner

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// eventTarget is a rungTarget that keeps the reasons of the Events recorded on it.
type eventTarget struct {
	rungTarget
	mu      sync.Mutex
	reasons []string
}

func (e *eventTarget) RecordEvent(_, reason, _, _ string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.reasons = append(e.reasons, reason)
}

// forkReplies is what forkGitHub answers: the run's event and head repository, and a
// status per call where the default is not wanted.
type forkReplies struct {
	event, headRepo string
	runStatus       int
	cancelStatus    int
	checkStatus     int
}

// forkGitHub is a fake GitHub REST API for RefuseForkJob: a run read, force-cancel, and
// check-run creation, answering per its forkReplies and recording every call.
type forkGitHub struct {
	forkReplies

	mu        sync.Mutex
	calls     []string
	checkBody map[string]any
}

func (f *forkGitHub) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.calls = append(f.calls, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/actions/runs/42"):
			if f.runStatus != 0 {
				w.WriteHeader(f.runStatus)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "queued", "event": f.event, "head_sha": "abc123",
				"head_repository": map[string]any{"full_name": f.headRepo},
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/actions/runs/42/force-cancel"):
			w.WriteHeader(orStatus(f.cancelStatus, http.StatusAccepted))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/check-runs"):
			body, _ := io.ReadAll(r.Body)
			f.mu.Lock()
			_ = json.Unmarshal(body, &f.checkBody)
			f.mu.Unlock()
			w.WriteHeader(orStatus(f.checkStatus, http.StatusCreated))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func orStatus(s, def int) int {
	if s == 0 {
		return def
	}
	return s
}

func (f *forkGitHub) called(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func TestRefuseForkJob(t *testing.T) {
	cases := []struct {
		name      string
		jobEvent  string
		gh        forkReplies
		noID      bool
		refused   bool
		held      bool // an error that is not a refusal: the caller holds and retries
		wantRead  bool
		wantCheck bool
		events    []string
	}{
		{name: "push is admitted without a read", jobEvent: "push"},
		{name: "pull_request_target is admitted without a read", jobEvent: "pull_request_target"},
		{name: "same-repository pull request is admitted, case-insensitively", jobEvent: "pull_request",
			gh: forkReplies{headRepo: "Acme/Widgets"}, wantRead: true},
		{name: "fork pull request is refused", jobEvent: "pull_request",
			gh: forkReplies{headRepo: "contributor/widgets"}, refused: true, wantRead: true, wantCheck: true,
			events: []string{"ForkJobRefused"}},
		{name: "fork pull_request_review is refused", jobEvent: "pull_request_review",
			gh: forkReplies{headRepo: "contributor/widgets"}, refused: true, wantRead: true, wantCheck: true,
			events: []string{"ForkJobRefused"}},
		{name: "fork pull_request_review_comment is refused", jobEvent: "pull_request_review_comment",
			gh: forkReplies{headRepo: "contributor/widgets"}, refused: true, wantRead: true, wantCheck: true,
			events: []string{"ForkJobRefused"}},
		{name: "empty head repository is refused", jobEvent: "pull_request",
			gh: forkReplies{headRepo: ""}, refused: true, wantRead: true, wantCheck: true,
			events: []string{"ForkJobRefused"}},
		{name: "no message event reads the run's: fork pull_request_target is admitted",
			gh: forkReplies{event: "pull_request_target", headRepo: "contributor/widgets"}, wantRead: true},
		{name: "no message event reads the run's: fork pull_request is refused",
			gh: forkReplies{event: "pull_request", headRepo: "contributor/widgets"}, refused: true, wantRead: true,
			wantCheck: true, events: []string{"ForkJobRefused"}},
		{name: "unreadable run is held, not admitted", jobEvent: "pull_request",
			gh: forkReplies{runStatus: http.StatusBadGateway}, held: true, wantRead: true},
		{name: "a failed cancel is held, not refused", jobEvent: "pull_request",
			gh: forkReplies{headRepo: "contributor/widgets", cancelStatus: http.StatusInternalServerError}, held: true, wantRead: true},
		{name: "a refused check run still refuses", jobEvent: "pull_request",
			gh: forkReplies{headRepo: "contributor/widgets", checkStatus: http.StatusForbidden}, refused: true, wantRead: true,
			wantCheck: true, events: []string{"ForkJobRefused", "ForkRefusalCheckRunSkipped"}},
		{name: "no run identity is refused without a call", jobEvent: "pull_request", noID: true, refused: true,
			events: []string{"ForkJobRefused"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gh := &forkGitHub{forkReplies: tc.gh}
			srv := httptest.NewServer(gh.handler())
			defer srv.Close()
			p := &Provisioner{
				TokenFunc:    func(context.Context) (string, error) { return "tok", nil },
				GitHubAPIURL: srv.URL,
				HTTPClient:   srv.Client(),
			}
			target := &eventTarget{}
			job := ScaleSetJob{JobID: "j1", Owner: "acme", Repository: "widgets", RunID: "42", EventName: tc.jobEvent}
			if tc.noID {
				job.Owner, job.Repository, job.RunID = "", "", ""
			}

			err := p.RefuseForkJob(context.Background(), target, job)

			switch {
			case tc.refused:
				if !IsForkJobRefused(err) {
					t.Fatalf("err = %v, want a refusal", err)
				}
				if !tc.noID && !gh.called("POST /repos/acme/widgets/actions/runs/42/force-cancel") {
					t.Errorf("refused without force-cancelling the run; calls %v", gh.calls)
				}
			case tc.held:
				if err == nil || IsForkJobRefused(err) {
					t.Fatalf("err = %v, want an error that is not a refusal", err)
				}
			default:
				if err != nil {
					t.Fatalf("err = %v, want the job admitted", err)
				}
				if gh.called("POST") {
					t.Errorf("an admitted job wrote to GitHub; calls %v", gh.calls)
				}
			}
			if got := gh.called("GET /repos/acme/widgets/actions/runs/42"); got != tc.wantRead {
				t.Errorf("run read = %v, want %v; calls %v", got, tc.wantRead, gh.calls)
			}
			if got := gh.called("POST /repos/acme/widgets/check-runs"); got != tc.wantCheck {
				t.Errorf("check run written = %v, want %v; calls %v", got, tc.wantCheck, gh.calls)
			}
			if tc.wantCheck && tc.gh.checkStatus == 0 {
				if gh.checkBody["head_sha"] != "abc123" || gh.checkBody["conclusion"] != "failure" {
					t.Errorf("check run body = %v, want head_sha abc123 and conclusion failure", gh.checkBody)
				}
				out, _ := gh.checkBody["output"].(map[string]any)
				if s, _ := out["summary"].(string); !strings.Contains(s, "forkPullRequests: Accept") || !strings.Contains(s, "ns/s") {
					t.Errorf("check run summary %q names neither the field nor the set", s)
				}
			}
			if strings.Join(target.reasons, ",") != strings.Join(tc.events, ",") {
				t.Errorf("events = %v, want %v", target.reasons, tc.events)
			}
		})
	}
}

func TestRefuseForkJobUnconfiguredEndpointIsHeld(t *testing.T) {
	p := &Provisioner{TokenFunc: func(context.Context) (string, error) { return "tok", nil }}
	err := p.RefuseForkJob(context.Background(), &eventTarget{},
		ScaleSetJob{JobID: "j1", Owner: "acme", Repository: "widgets", RunID: "42", EventName: "pull_request"})
	if err == nil || errors.Is(err, ErrForkJobRefused) {
		t.Fatalf("err = %v, want an unconfigured endpoint to hold the job rather than admit or refuse it", err)
	}
}
