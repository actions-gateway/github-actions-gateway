package provisioner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// A job GitHub cancelled at its time limit, re-run and passed, is counted as a
// failed one is, and any other cancelled job is not.

// annotationsStub answers a run's job list, and each check run's annotations.
type annotationsStub struct {
	mu   sync.Mutex
	jobs []map[string]any
	// answers is each check run's status and body, 200 and no annotation for the rest.
	answers map[string]annotationsAnswer
	lists   int
	read    []string
	// beforeRead runs, unlocked, as each annotations read arrives.
	beforeRead func(r *http.Request)
}

type annotationsAnswer struct {
	status int
	body   string
	err    error
	hang   bool
}

func (s *annotationsStub) RoundTrip(r *http.Request) (*http.Response, error) {
	w := httptest.NewRecorder()
	_, rest, annotations := strings.Cut(r.URL.Path, "/repos/org/repo/check-runs/")
	s.mu.Lock()
	if !annotations {
		defer s.mu.Unlock()
		s.lists++
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(s.jobs), "jobs": s.jobs})
		return w.Result(), nil
	}
	id, _ := strings.CutSuffix(rest, "/annotations")
	a, before := s.answers[id], s.beforeRead
	s.read = append(s.read, id+"?"+r.URL.RawQuery)
	s.mu.Unlock()
	if before != nil {
		before(r)
	}
	switch {
	case a.hang:
		<-r.Context().Done()
		return nil, r.Context().Err()
	case a.err != nil:
		return nil, a.err
	case a.status != 0:
		w.WriteHeader(a.status)
	}
	if a.body == "" {
		a.body = "[]"
	}
	_, _ = w.WriteString(a.body)
	return w.Result(), nil
}

// reads is the check runs whose annotations were read, in order.
func (s *annotationsStub) reads() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []string
	for _, r := range s.read {
		id, _, _ := strings.Cut(r, "?")
		ids = append(ids, id)
	}
	return ids
}

func (s *annotationsStub) listed() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lists
}

// annotation is a check run's answer holding one annotation per message.
func annotation(messages ...string) annotationsAnswer {
	var list []map[string]any
	for _, m := range messages {
		list = append(list, map[string]any{"path": ".github", "annotation_level": "failure", "message": m})
	}
	body, _ := json.Marshal(list)
	return annotationsAnswer{body: string(body)}
}

// capturedAnnotations is a captured answer: "timedOut" or "userCancelled".
func capturedAnnotations(t *testing.T, name string) annotationsAnswer {
	t.Helper()
	raw, err := os.ReadFile("testdata/check_run_annotations.json")
	require.NoError(t, err)
	var all map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &all))
	require.Contains(t, all, name)
	return annotationsAnswer{body: string(all[name])}
}

// concluded is a job as GitHub lists it in one attempt, with the id of its check run.
func concluded(attempt int, name, runner, conclusion string, id int64) map[string]any {
	j := listed(attempt, name, runner)
	j["conclusion"], j["id"] = conclusion, id
	return j
}

// cancelFixture is a run of one job "build", cancelled in attempt 1 (check run 101, on
// runner-1) and passed in attempt 2, whose check run 101 says it timed out.
func cancelFixture(t *testing.T) (client.Client, *Provisioner, *stubTarget, *annotationsStub) {
	t.Helper()
	c, p, target := flakyFixture(t)
	gh := &annotationsStub{
		jobs: []map[string]any{
			concluded(1, "build", "runner-1", "cancelled", 101),
			concluded(2, "build", "runner-2", "success", 102),
		},
		answers: map[string]annotationsAnswer{"101": capturedAnnotations(t, "timedOut")},
	}
	gh.serve(p)
	return c, p, target, gh
}

func (s *annotationsStub) serve(p *Provisioner) {
	p.GitHubAPIURL, p.HTTPClient = "https://github.test", &http.Client{Transport: s}
	p.TokenFunc = func(context.Context) (string, error) { return "tok", nil }
}

func counted(p *Provisioner) [2]float64 {
	return [2]float64{rerunCount(p, rerunTriggerOther), flaky(p, rerunTriggerOther)}
}

// TestTimedOut_AReRunThatPassesCounts is the whole of it: a job that timed out counts once
// its re-run passes, and one that a user cancelled does not.
func TestTimedOut_AReRunThatPassesCounts(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		answer annotationsAnswer
		want   float64
	}{
		{"timed out", capturedAnnotations(t, "timedOut"), 1},
		{"timed out, in the older words", annotation("The job running on runner GitHub Actions 2 has exceeded the maximum execution time of 360 minutes."), 1},
		{"timed out, listed after the job's own", annotation("unused variable", "The job has exceeded the maximum execution time of 5m0s"), 1},
		{"a user's cancel", capturedAnnotations(t, "userCancelled"), 0},
		{"a user's cancel of a job that says the words itself", annotation("subtest: The job has exceeded the maximum execution time of 1m0s"), 0},
		{"no annotation", annotationsAnswer{}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, p, target, gh := cancelFixture(t)
			gh.answers["101"] = tc.answer
			completeJob(ctx, t, p, target, buildJob(1), jobResultCanceled)
			assert.Zero(t, gh.listed(), "a cancel reads nothing")
			assert.Empty(t, gh.reads())
			completeJob(ctx, t, p, target, buildJob(2), jobResultSucceeded)

			assert.Equal(t, [2]float64{tc.want, tc.want}, counted(p))
			assert.Equal(t, 1, gh.listed())
			assert.Equal(t, []string{"101"}, gh.reads())
			assert.Equal(t, []string{"101?per_page=100"}, gh.read, "every annotation a job may have")
			if tc.want == 0 {
				assert.Empty(t, target.events)
			} else {
				require.Equal(t, []string{"FlakyJobDetected"}, target.events)
				assert.Contains(t, target.messages[0], "reached its time limit")
			}
			var cms corev1.ConfigMapList
			require.NoError(t, c.List(ctx, &cms))
			assert.Empty(t, cms.Items, "the hold is in memory alone")
		})
	}
}

// TestTimedOut_OnlyAPassedReRunIsRead: what never costs a read of the annotations.
func TestTimedOut_OnlyAPassedReRunIsRead(t *testing.T) {
	ctx := context.Background()
	t.Run("a cancel that is never re-run", func(t *testing.T) {
		_, p, target, gh := cancelFixture(t)
		completeJob(ctx, t, p, target, buildJob(1), jobResultCanceled)
		other := buildJob(2)
		other.RunID = "901"
		completeJob(ctx, t, p, target, other, jobResultSucceeded)
		assert.Zero(t, gh.listed())
		assert.Empty(t, gh.reads())
	})
	t.Run("a re-run that fails", func(t *testing.T) {
		_, p, target, gh := cancelFixture(t)
		completeJob(ctx, t, p, target, buildJob(1), jobResultCanceled)
		completeJob(ctx, t, p, target, buildJob(2), "failed")
		assert.Empty(t, gh.reads())
		assert.Equal(t, [2]float64{}, counted(p))
		assert.Contains(t, readFailures(t, p, target), "900/build", "and is recorded as any failure is")
	})
	t.Run("a cancel no runner started", func(t *testing.T) {
		_, p, target, gh := cancelFixture(t)
		job := buildJob(1)
		job.RunnerName = ""
		completeJob(ctx, t, p, target, job, jobResultCanceled)
		assert.Empty(t, p.cancelHolds.byJob)
		completeJob(ctx, t, p, target, buildJob(2), jobResultSucceeded)
		assert.Zero(t, gh.listed())
	})
	t.Run("a success that names no runner", func(t *testing.T) {
		_, p, target, gh := cancelFixture(t)
		completeJob(ctx, t, p, target, buildJob(1), jobResultCanceled)
		job := buildJob(2)
		job.RunnerName = ""
		completeJob(ctx, t, p, target, job, jobResultSucceeded)
		assert.Zero(t, gh.listed())
	})
	t.Run("a cancel a disruption claims, which is recorded instead", func(t *testing.T) {
		_, p, target, gh := cancelFixture(t)
		claimDisruption(t, p, target, "job-1", recoveryCauseEviction)
		completeJob(ctx, t, p, target, buildJob(1), jobResultCanceled)
		assert.Empty(t, p.cancelHolds.byJob)
		completeJob(ctx, t, p, target, buildJob(2), jobResultSucceeded)
		assert.Empty(t, gh.reads())
	})
	t.Run("the same run and name on another set", func(t *testing.T) {
		_, p, target, gh := cancelFixture(t)
		completeJob(ctx, t, p, target, buildJob(1), jobResultCanceled)
		completeJob(ctx, t, p, &stubTarget{key: client.ObjectKey{Namespace: "team-a", Name: "cpu"}}, buildJob(2), jobResultSucceeded)
		assert.Zero(t, gh.listed())
	})
	t.Run("a row GitHub does not list as cancelled", func(t *testing.T) {
		_, p, target, gh := cancelFixture(t)
		gh.jobs[0]["conclusion"] = "failure"
		completeJob(ctx, t, p, target, buildJob(1), jobResultCanceled)
		completeJob(ctx, t, p, target, buildJob(2), jobResultSucceeded)
		assert.Equal(t, 1, gh.listed())
		assert.Empty(t, gh.reads())
	})
}

// TestTimedOut_ATwinDoesNotSpendTheHold: a job that shares the cancelled one's name
// succeeds beside it, and the cancelled one's re-run still counts.
func TestTimedOut_ATwinDoesNotSpendTheHold(t *testing.T) {
	ctx := context.Background()
	_, p, target, gh := cancelFixture(t)
	rerun := gh.jobs[1]
	gh.jobs = []map[string]any{gh.jobs[0], concluded(1, "build", "twin", "success", 103)}
	completeJob(ctx, t, p, target, buildJob(1), jobResultCanceled)
	completeJob(ctx, t, p, target, twinJob("twin", "twin"), jobResultSucceeded)
	assert.Empty(t, gh.reads())
	assert.Equal(t, [2]float64{}, counted(p))

	gh.jobs = append(gh.jobs, rerun, concluded(2, "build", "twin", "success", 104))
	completeJob(ctx, t, p, target, buildJob(2), jobResultSucceeded)
	assert.Equal(t, []string{"101"}, gh.reads())
	assert.Equal(t, [2]float64{1, 1}, counted(p))
}

// TestTimedOut_ReadWhereItRan: an attempt that did not re-run the cancelled job lists it
// again under another id, whose check run is not the one read.
func TestTimedOut_ReadWhereItRan(t *testing.T) {
	ctx := context.Background()
	for name, order := range map[string]func([]map[string]any){
		"oldest attempt first": func([]map[string]any) {},
		"newest attempt first": slices.Reverse[[]map[string]any],
	} {
		t.Run(name, func(t *testing.T) {
			_, p, target, gh := cancelFixture(t)
			gh.jobs = []map[string]any{
				concluded(1, "build", "runner-1", "cancelled", 101),
				concluded(2, "build", "runner-1", "cancelled", 104),
				concluded(3, "build", "runner-3", "success", 105),
			}
			order(gh.jobs)
			completeJob(ctx, t, p, target, buildJob(1), jobResultCanceled)
			completeJob(ctx, t, p, target, buildJob(3), jobResultSucceeded)
			assert.Equal(t, []string{"101"}, gh.reads())
			assert.Equal(t, [2]float64{1, 1}, counted(p))
		})
	}
}

// TestTimedOut_BesideARecordedFailure: a failure of one attempt and a timeout of another
// make one count between them.
func TestTimedOut_BesideARecordedFailure(t *testing.T) {
	ctx := context.Background()
	results := func(first, second string) []map[string]any {
		return []map[string]any{
			concluded(1, "build", "runner-1", first, 101),
			concluded(2, "build", "runner-2", second, 102),
			concluded(3, "build", "runner-3", "success", 103),
		}
	}
	timedOut := capturedAnnotations(t, "timedOut")
	for _, tc := range []struct {
		name          string
		first, second string
		noRunner      bool
		wantReads     []string
		wantReruns    float64
		wantFlaky     float64
	}{
		{name: "fails, times out, passes", first: "failed", second: jobResultCanceled, wantReads: []string{"102"}, wantReruns: 1, wantFlaky: 1},
		{name: "times out, fails, passes", first: jobResultCanceled, second: "failed", wantReruns: 1, wantFlaky: 1},
		// A record without a runner pairs wherever one job held the name, so both match.
		{name: "fails on no runner, times out, passes", first: "failed", second: jobResultCanceled, noRunner: true, wantReruns: 1, wantFlaky: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, p, target, gh := cancelFixture(t)
			spelling := map[string]string{"failed": "failure", jobResultCanceled: "cancelled"}
			gh.jobs = results(spelling[tc.first], spelling[tc.second])
			gh.answers = map[string]annotationsAnswer{"101": timedOut, "102": timedOut}
			var log bytes.Buffer
			p.Log = slog.New(slog.NewTextHandler(&log, nil))
			first := buildJob(1)
			if tc.noRunner {
				first.RunnerName = ""
			}
			completeJob(ctx, t, p, target, first, tc.first)
			completeJob(ctx, t, p, target, buildJob(2), tc.second)
			completeJob(ctx, t, p, target, buildJob(3), jobResultSucceeded)
			assert.Equal(t, tc.wantReads, gh.reads())
			assert.Equal(t, [2]float64{tc.wantReruns, tc.wantFlaky}, counted(p))
			assert.NotContains(t, log.String(), "paired with nothing")
		})
	}
}

// TestTimedOut_ARecordFoundOnTheRetryIsTheCount: another replica writes the failure as the
// batch writes, so the second round pairs what the first round had read as a held cancel.
func TestTimedOut_ARecordFoundOnTheRetryIsTheCount(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		_, _, target, gh := cancelFixture(t)
		rival := false
		var p *Provisioner
		p = flakyProvisioner(fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if rival {
					rival = false
					l, cm, err := p.readRerunLedger(ctx, target)
					assert.NoError(t, err)
					l.Failures["900/build"] = &failureRecord{ExpiresAt: time.Now().Add(time.Hour), JobID: "job-1", Runner: "runner-1"}
					data, err := json.Marshal(l)
					assert.NoError(t, err)
					cm.Data[rerunLedgerDataKey] = string(data)
					assert.NoError(t, c.Update(ctx, cm))
				}
				return c.Update(ctx, obj, opts...)
			}}).Build())
		gh.serve(p)
		p.pairingDelay = defaultPairingDelay
		gh.jobs = append(gh.jobs, concluded(1, "unit", "unit-1", "failure", 301), concluded(2, "unit", "unit-2", "success", 302))
		unit := func(n int) ScaleSetJob {
			j := buildJob(n)
			j.JobName, j.JobID, j.RunnerName = "unit", fmt.Sprintf("unit-job-%d", n), fmt.Sprintf("unit-%d", n)
			return j
		}
		completeJob(ctx, t, p, target, buildJob(1), jobResultCanceled)
		completeJob(ctx, t, p, target, unit(1), "failed")
		_, err := p.RecordScaleSetJobCompleted(ctx, target, buildJob(2), jobResultSucceeded)
		require.NoError(t, err)
		rival = true
		completeJob(ctx, t, p, target, unit(2), jobResultSucceeded)
		assert.False(t, rival, "the rival write happened")
		assert.Equal(t, [2]float64{2, 2}, counted(p), "one for each job")
		assert.Empty(t, gh.reads())
	})
}

// TestTimedOut_QueuesWithoutARecord: the hold is enough to queue a success, with the
// failure index stale or the ledger unreadable.
func TestTimedOut_QueuesWithoutARecord(t *testing.T) {
	ctx := context.Background()
	t.Run("the index lists a record that is gone", func(t *testing.T) {
		_, p, target, gh := cancelFixture(t)
		completeJob(ctx, t, p, target, buildJob(1), jobResultCanceled)
		p.failureIndex.note(target.key, &rerunLedger{Failures: map[string]*failureRecord{"900/build": {}}})
		completeJob(ctx, t, p, target, buildJob(2), jobResultSucceeded)
		assert.Equal(t, []string{"101"}, gh.reads())
		assert.Equal(t, [2]float64{1, 1}, counted(p))
	})
	t.Run("the index knows of none, and the ledger is not read for it", func(t *testing.T) {
		var gets int
		c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if key.Name == scaleSetRerunLedgerConfigMapName("gpu") {
					gets++
				}
				return c.Get(ctx, key, obj, opts...)
			}}).Build()
		_, _, target, gh := cancelFixture(t)
		p := flakyProvisioner(c)
		gh.serve(p)
		p.failureIndex.note(target.key, &rerunLedger{})
		completeJob(ctx, t, p, target, buildJob(1), jobResultCanceled)
		assert.Zero(t, gets)
		completeJob(ctx, t, p, target, buildJob(2), jobResultSucceeded)
		assert.Equal(t, 1, gets, "the batch's, for the run's re-runs")
		assert.Equal(t, [2]float64{1, 1}, counted(p))
	})
	t.Run("the ledger cannot be read as the success completes", func(t *testing.T) {
		broken := errors.New("apiserver is away")
		fail := false
		c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if fail && key.Name == scaleSetRerunLedgerConfigMapName("gpu") {
					fail = false
					return broken
				}
				return c.Get(ctx, key, obj, opts...)
			}}).Build()
		_, _, target, gh := cancelFixture(t)
		p := flakyProvisioner(c)
		gh.serve(p)
		completeJob(ctx, t, p, target, buildJob(1), jobResultCanceled)
		fail = true
		done, err := p.RecordScaleSetJobCompleted(ctx, target, buildJob(2), jobResultSucceeded)
		require.ErrorIs(t, err, broken)
		<-done
		assert.Equal(t, [2]float64{1, 1}, counted(p))
	})
}

// TestTimedOut_ARedeliveredMessageCountsOnce: a re-created session delivers messages again.
func TestTimedOut_ARedeliveredMessageCountsOnce(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		answer string
		want   float64
	}{{"timed out", "timedOut", 1}, {"a user's cancel", "userCancelled", 0}} {
		t.Run(tc.name, func(t *testing.T) {
			_, p, target, gh := cancelFixture(t)
			gh.answers["101"] = capturedAnnotations(t, tc.answer)
			completeJob(ctx, t, p, target, buildJob(1), jobResultCanceled)
			completeJob(ctx, t, p, target, buildJob(1), jobResultCanceled)
			completeJob(ctx, t, p, target, buildJob(2), jobResultSucceeded)
			completeJob(ctx, t, p, target, buildJob(1), jobResultCanceled)
			completeJob(ctx, t, p, target, buildJob(2), jobResultSucceeded)
			assert.Equal(t, [2]float64{tc.want, tc.want}, counted(p))
			assert.Equal(t, []string{"101"}, gh.reads())
		})
	}
	t.Run("in one batch", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			_, p, target, _ := cancelFixture(t)
			p.pairingDelay = defaultPairingDelay
			completeJob(ctx, t, p, target, buildJob(1), jobResultCanceled)
			var waits []<-chan struct{}
			for range 2 {
				done, err := p.RecordScaleSetJobCompleted(ctx, target, buildJob(2), jobResultSucceeded)
				require.NoError(t, err)
				waits = append(waits, done)
			}
			for _, done := range waits {
				<-done
			}
			assert.Equal(t, [2]float64{1, 1}, counted(p))
		})
	})
}

// TestTimedOut_TheTriggerIsWhoeverReRanIt: GAG's re-run of another job carries a cancelled
// one with it, as it does a failed one.
func TestTimedOut_TheTriggerIsWhoeverReRanIt(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name        string
		attempt     string
		rerun       *rerunAttempt
		wantTrigger string
		wantFlaky   float64
	}{
		{"GAG's failed-jobs re-run of another job", "1", &rerunAttempt{Scope: rerunScopeFailedJobs, Cause: recoveryCauseDeletion, Jobs: []string{"lint"}}, rerunTriggerGAGCollateral, 1},
		{"GAG's re-run of another attempt", "2", &rerunAttempt{Scope: rerunScopeFailedJobs, Cause: recoveryCauseDeletion, Jobs: []string{"lint"}}, rerunTriggerOther, 1},
		{"GAG's re-run of this job for a disruption", "1", &rerunAttempt{Scope: rerunScopeFailedJobs, Cause: recoveryCauseEviction, Jobs: []string{"build"}}, rerunTriggerGAG, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, p, target, _ := cancelFixture(t)
			require.NoError(t, p.updateRerunLedger(ctx, target, func(l *rerunLedger, now time.Time) bool {
				l.Reruns["900"] = &rerunRecord{ExpiresAt: now.Add(time.Hour), Count: 1, Attempts: map[string]*rerunAttempt{tc.attempt: tc.rerun}}
				return true
			}))
			completeJob(ctx, t, p, target, buildJob(1), jobResultCanceled)
			completeJob(ctx, t, p, target, buildJob(2), jobResultSucceeded)
			assert.Equal(t, 1.0, rerunCount(p, tc.wantTrigger))
			assert.Equal(t, tc.wantFlaky, flaky(p, tc.wantTrigger))
			if tc.wantTrigger == rerunTriggerGAG {
				assert.Equal(t, 1.0, outcome(p, recoveryCauseEviction, jobResultSucceeded))
			}
		})
	}
}

// TestTimedOut_WhatEachAnswerDoes: two cancelled jobs are re-run and pass together, GitHub
// answers for the first as tc says, and for the second, where it is read, that it timed out.
func TestTimedOut_WhatEachAnswerDoes(t *testing.T) {
	const (
		secret = "a line of the job's own output"
		token  = "ghs_0123456789012345678901234567890123456789"
	)
	for _, tc := range []struct {
		name      string
		answer    annotationsAnswer
		wantReads []string
		wantEvent bool
		wantLog   string
	}{
		{name: "the App lacks the permission", answer: annotationsAnswer{status: 403, body: `{"message":"Resource not accessible by integration"}`},
			wantReads: []string{"101"}, wantEvent: true, wantLog: "403"},
		{name: "a rate limit", answer: annotationsAnswer{status: 403, body: `{"message":"API rate limit exceeded for installation"}`},
			wantReads: []string{"101"}, wantLog: "API rate limit exceeded"},
		{name: "a secondary rate limit", answer: annotationsAnswer{status: 429, body: `{"message":"slow down"}`},
			wantReads: []string{"101"}, wantLog: "slow down"},
		{name: "a check run that is gone", answer: annotationsAnswer{status: 404, body: `{"message":"Not Found"}`},
			wantReads: []string{"101", "201"}, wantLog: "404"},
		{name: "a server error", answer: annotationsAnswer{status: 502, body: "bad gateway"},
			wantReads: []string{"101", "201"}, wantLog: "bad gateway"},
		{name: "a server error that carries a token", answer: annotationsAnswer{status: 502, body: "bad gateway " + token},
			wantReads: []string{"101", "201"}, wantLog: "[REDACTED]"},
		{name: "a server error that says the words", answer: annotationsAnswer{status: 500, body: `{"message":"Resource not accessible by integration"}`},
			wantReads: []string{"101", "201"}, wantLog: "500"},
		{name: "an unreachable GitHub", answer: annotationsAnswer{err: errors.New("connection reset")},
			wantReads: []string{"101", "201"}, wantLog: "connection reset"},
		{name: "an answer that is not a list", answer: annotationsAnswer{body: `{"message":"` + secret + `"}`},
			wantReads: []string{"101", "201"}, wantLog: "decode"},
		{name: "another cause", answer: annotation(secret), wantReads: []string{"101", "201"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx := context.Background()
				_, p, target, gh := cancelFixture(t)
				p.pairingDelay = defaultPairingDelay
				var log bytes.Buffer
				p.Log = slog.New(slog.NewTextHandler(&log, &slog.HandlerOptions{Level: slog.LevelDebug}))
				gh.jobs = append(gh.jobs, concluded(1, "lint", "lint-1", "cancelled", 201), concluded(2, "lint", "lint-2", "success", 202))
				gh.answers = map[string]annotationsAnswer{"101": tc.answer, "201": capturedAnnotations(t, "timedOut")}
				lint := func(n int) ScaleSetJob {
					job := buildJob(n)
					job.JobName, job.JobID, job.RunnerName = "lint", fmt.Sprintf("lint-job-%d", n), fmt.Sprintf("lint-%d", n)
					return job
				}
				completeJob(ctx, t, p, target, buildJob(1), jobResultCanceled)
				completeJob(ctx, t, p, target, lint(1), jobResultCanceled)
				_, err := p.RecordScaleSetJobCompleted(ctx, target, buildJob(2), jobResultSucceeded)
				require.NoError(t, err)
				completeJob(ctx, t, p, target, lint(2), jobResultSucceeded)

				assert.Equal(t, tc.wantReads, gh.reads())
				want := float64(len(tc.wantReads) - 1)
				assert.Equal(t, [2]float64{want, want}, counted(p), "the second job alone, where it is read")
				settled := p.cancelHolds.find(target.key, "900/build", time.Now()) == nil
				assert.Equal(t, tc.answer.status == 0 && tc.answer.err == nil && tc.wantLog == "", settled, "only an answer settles the hold")
				if tc.wantEvent {
					require.Equal(t, []string{"CancelledJobUnread"}, target.events)
					assert.Contains(t, target.messages[0], "Checks: Read")
				} else {
					assert.NotContains(t, target.events, "CancelledJobUnread")
				}
				if tc.wantLog != "" {
					assert.Contains(t, log.String(), "level=WARN")
					assert.Contains(t, log.String(), tc.wantLog)
				} else {
					assert.NotContains(t, log.String(), "level=WARN")
				}
				assert.NotContains(t, log.String(), secret)
				assert.NotContains(t, log.String(), token)
				assert.NotContains(t, strings.Join(target.messages, "\n"), secret)
			})
		})
	}
}

// TestTimedOut_TheReadsAreBoundedAndLast: a recorded failure in the same batch is counted
// before the first read, no read holds the listener, and one that is never answered ends
// the batch's reads.
func TestTimedOut_TheReadsAreBoundedAndLast(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		_, _, target, gh := cancelFixture(t)
		const slowWrite = 10 * time.Second
		slow := false
		p := flakyProvisioner(fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if slow {
					time.Sleep(slowWrite)
				}
				return c.Update(ctx, obj, opts...)
			}}).Build())
		gh.serve(p)
		p.pairingDelay = defaultPairingDelay
		var log bytes.Buffer
		p.Log = slog.New(slog.NewTextHandler(&log, nil))
		gh.jobs = append(gh.jobs, concluded(1, "unit", "unit-1", "failure", 301), concluded(2, "unit", "unit-2", "success", 302),
			concluded(1, "lint", "lint-1", "cancelled", 201), concluded(2, "lint", "lint-2", "success", 202),
			concluded(1, "docs", "docs-1", "cancelled", 401), concluded(2, "docs", "docs-2", "success", 402))
		timedOut := capturedAnnotations(t, "timedOut")
		gh.answers = map[string]annotationsAnswer{"101": {hang: true}, "201": timedOut, "401": timedOut}
		var atRead [2]float64
		var budget time.Duration
		gh.beforeRead = func(r *http.Request) {
			atRead = counted(p)
			deadline, _ := r.Context().Deadline()
			budget = time.Until(deadline)
		}
		job := func(name string, n int) ScaleSetJob {
			j := buildJob(n)
			j.JobName, j.JobID, j.RunnerName = name, fmt.Sprintf("%s-job-%d", name, n), fmt.Sprintf("%s-%d", name, n)
			return j
		}
		completeJob(ctx, t, p, target, buildJob(1), jobResultCanceled)
		completeJob(ctx, t, p, target, job("lint", 1), jobResultCanceled)
		completeJob(ctx, t, p, target, job("docs", 1), jobResultCanceled)
		completeJob(ctx, t, p, target, job("unit", 1), "failed")
		slow = true
		start := time.Now()
		var done <-chan struct{}
		for _, j := range []ScaleSetJob{buildJob(2), job("lint", 2), job("docs", 2), job("unit", 2)} {
			var err error
			done, err = p.RecordScaleSetJobCompleted(ctx, target, j, jobResultSucceeded)
			require.NoError(t, err)
		}
		cancel()
		assert.Zero(t, time.Since(start), "the listener waits for no read")
		<-done
		assert.Equal(t, defaultPairingDelay+slowWrite+annotationsBudget, time.Since(start), "a listener that stopped does not end the reads")
		assert.Equal(t, annotationsBudget, budget, "which are bounded apart from the list and the write")
		assert.Equal(t, [2]float64{1, 1}, atRead, "the recorded failure, before the first read")
		assert.Equal(t, [2]float64{1, 1}, counted(p))
		assert.Equal(t, []string{"101"}, gh.reads())
		assert.Contains(t, log.String(), "unread=2")
	})
}

// TestTimedOut_AHundredReads is what a matrix cancelled and re-run together costs.
func TestTimedOut_AHundredReads(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		_, p, target, gh := cancelFixture(t)
		p.pairingDelay = defaultPairingDelay
		p.failureIndex.note(target.key, &rerunLedger{})
		gh.jobs, gh.answers = nil, nil
		leg := func(attempt, n int) ScaleSetJob {
			j := buildJob(attempt)
			j.JobName = "leg-" + strconv.Itoa(n)
			j.JobID, j.RunnerName = fmt.Sprintf("job-%d-%d", n, attempt), fmt.Sprintf("r-%d-%d", n, attempt)
			return j
		}
		for n := range 100 {
			gh.jobs = append(gh.jobs,
				concluded(1, leg(1, n).JobName, leg(1, n).RunnerName, "cancelled", int64(1000+n)),
				concluded(2, leg(2, n).JobName, leg(2, n).RunnerName, "success", int64(2000+n)))
			completeJob(ctx, t, p, target, leg(1, n), jobResultCanceled)
		}
		var done <-chan struct{}
		for n := range 100 {
			var err error
			done, err = p.RecordScaleSetJobCompleted(ctx, target, leg(2, n), jobResultSucceeded)
			require.NoError(t, err)
		}
		<-done
		assert.Equal(t, 1, gh.listed())
		assert.Len(t, gh.reads(), 100)
		assert.Equal(t, [2]float64{}, counted(p))
	})
}

// TestTimedOut_TheHoldIsTheOneTheSuccessSaw: a cancel that arrives while a success waits
// for its list does not take the hold's place in it.
func TestTimedOut_TheHoldIsTheOneTheSuccessSaw(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		_, p, target, gh := cancelFixture(t)
		p.pairingDelay = defaultPairingDelay
		completeJob(ctx, t, p, target, buildJob(1), jobResultCanceled)
		done, err := p.RecordScaleSetJobCompleted(ctx, target, buildJob(2), jobResultSucceeded)
		require.NoError(t, err)
		completeJob(ctx, t, p, target, buildJob(3), jobResultCanceled)
		<-done
		assert.Equal(t, []string{"101"}, gh.reads())
		assert.Equal(t, [2]float64{1, 1}, counted(p))
		h := p.cancelHolds.find(target.key, "900/build", time.Now())
		require.NotNil(t, h, "and is not settled by it")
		assert.Equal(t, "runner-3", h.runner)
	})
}

// TestCancelHolds_AreBounded: the oldest hold goes, a later attempt's takes the place of an
// earlier one's, and one past a failure record's life matches nothing.
func TestCancelHolds_AreBounded(t *testing.T) {
	set, now := types.NamespacedName{Namespace: "team-a", Name: "gpu"}, time.Now()
	var x cancelHolds
	hold := func(id string, age time.Duration) *cancelHold {
		return &cancelHold{jobID: id, runner: "runner-" + id, at: now.Add(-age)}
	}
	x.put(set, "1/build", hold("a", time.Hour))
	x.put(set, "1/build", hold("b", time.Minute))
	require.Equal(t, "b", x.find(set, "1/build", now).jobID)
	require.Nil(t, x.find(types.NamespacedName{Namespace: "team-b", Name: "gpu"}, "1/build", now))

	x.put(set, "2/build", hold("edge", failureRecordTTL))
	x.put(set, "3/build", hold("old", failureRecordTTL+time.Second))
	assert.NotNil(t, x.find(set, "2/build", now))
	assert.Nil(t, x.find(set, "3/build", now))

	for n := range maxCancelHolds {
		x.put(set, fmt.Sprintf("flood-%d/build", n), hold(strconv.Itoa(n), 0))
	}
	assert.Len(t, x.byJob, maxCancelHolds)
	assert.Nil(t, x.find(set, "1/build", now), "dropped for newer ones")
	assert.NotNil(t, x.find(set, "flood-0/build", now))
	assert.True(t, x.settle(x.find(set, "flood-0/build", now)))
	assert.Nil(t, x.find(set, "flood-0/build", now))
}

// TestTimedOut_ARestartLosesTheHold is the undercount the design accepts.
func TestTimedOut_ARestartLosesTheHold(t *testing.T) {
	ctx := context.Background()
	c, p, target, gh := cancelFixture(t)
	completeJob(ctx, t, p, target, buildJob(1), jobResultCanceled)
	restarted := flakyProvisioner(c)
	gh.serve(restarted)
	completeJob(ctx, t, restarted, target, buildJob(2), jobResultSucceeded)
	assert.Equal(t, [2]float64{}, counted(restarted))
	assert.Empty(t, gh.reads())
}
