package listener_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/listener"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/runnercore"
	"github.com/actions-gateway/github-actions-gateway/broker"
)

// TestListener_AdmitOutcome asserts the outcome handleJob reports back to the
// admission gate, which is what decides whether the scale-up token the rate rung
// took is refunded (Q972). A delivery that never asks for a worker pod must report
// AdmitAborted — above all the Q260 dedup loser, where N fanned-out siblings would
// otherwise spend N tokens for the single pod their winner creates, cutting the ramp
// to maxPerSecond/N under exactly the burst spec.scaleUp exists to smooth.
//
// The gate itself is stubbed: the token arithmetic behind these outcomes is pinned
// in the provisioner's own tests, and only this tier can say which outcome each
// return path reports.
func TestListener_AdmitOutcome(t *testing.T) {
	tests := []struct {
		name string
		// setup installs the path under test on the broker mux and the config.
		setup func(t *testing.T, mux *brokerMux, cfg *listener.Config)
		want  runnercore.AdmitOutcome
	}{
		{
			name: "dedup loser refunds its token",
			setup: func(_ *testing.T, mux *brokerMux, cfg *listener.Config) {
				mux.SetAcquire(func(w http.ResponseWriter, _ *http.Request) { defaultAcquireJob(w) })
				// A sibling already owns this planID, so this delivery provisions nothing.
				cfg.ClaimJob = func(string, listener.SiblingDelivery) listener.ClaimResult {
					return listener.ClaimResult{}
				}
				cfg.JobHandler = func(context.Context, string, string, []byte, string) (broker.TaskResult, error) {
					t.Error("JobHandler must not run for a deduplicated delivery")
					return "", nil
				}
			},
			want: runnercore.AdmitAborted,
		},
		{
			name: "failed acquire refunds its token",
			setup: func(_ *testing.T, mux *brokerMux, cfg *listener.Config) {
				mux.SetAcquire(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusInternalServerError)
				})
				cfg.JobHandler = func(context.Context, string, string, []byte, string) (broker.TaskResult, error) {
					t.Error("JobHandler must not run when AcquireJob failed")
					return "", nil
				}
			},
			want: runnercore.AdmitAborted,
		},
		{
			name: "provisioned job keeps its token",
			setup: func(_ *testing.T, mux *brokerMux, cfg *listener.Config) {
				mux.SetAcquire(func(w http.ResponseWriter, _ *http.Request) { defaultAcquireJob(w) })
				cfg.JobHandler = func(context.Context, string, string, []byte, string) (broker.TaskResult, error) {
					return broker.TaskResultSucceeded, nil
				}
			},
			want: runnercore.AdmitProvisioned,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oauthSrv := oauthStub()
			mux := &brokerMux{}
			brokerSrv := httptest.NewServer(mux)

			var delivered atomic.Bool
			mux.SetGetMessage(func(w http.ResponseWriter, _ *http.Request) {
				if !delivered.Swap(true) {
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(jobMsgWithURL(brokerSrv.URL))
					return
				}
				w.WriteHeader(http.StatusAccepted)
			})

			cfg := makeCfg(t, oauthSrv, brokerSrv)
			cfg.Metrics = newTestMetrics()
			cfg.IsLastPoller = func() bool { return true }

			// Record what the gate is told. Unlimited capacity: the reservation is not
			// what this test is about.
			var released atomic.Bool
			var got atomic.Int64
			cfg.Admit = func(context.Context) (func(runnercore.AdmitOutcome), bool, string) {
				return func(outcome runnercore.AdmitOutcome) {
					got.Store(int64(outcome))
					released.Store(true)
				}, true, ""
			}
			tt.setup(t, mux, &cfg)

			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			done := runAndWait(ctx, cfg)

			require.Eventually(t, released.Load, 2*time.Second, 10*time.Millisecond,
				"the admission release was never called")
			assert.Equal(t, tt.want, runnercore.AdmitOutcome(got.Load()))

			cancel()
			<-done
			closeHTTP(oauthSrv)
			closeHTTP(brokerSrv)
			goleak.VerifyNone(t)
		})
	}
}
