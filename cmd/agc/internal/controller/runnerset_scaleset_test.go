package controller

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/actions-gateway/github-actions-gateway/scaleset"
)

// TestScaleSetStubConfigURL pins the fake-GitHub re-point, whose whole job is to swap
// the https scheme a gateway is FORCED to declare (CRD pattern + webhook) for the
// plaintext one the stub serves. The org (or owner/repo) path must survive: the client
// derives the runners REST prefix from it and rejects a pathless config URL outright,
// so dropping the path would wedge the bootstrap rather than merely misaddress it.
//
// The negative cases are the load-bearing ones. The rewrite applies only to a gateway
// already naming the stub's host:port; anything else is left alone, so a gateway
// deliberately pointed at an unreachable host still fails to bootstrap on a cluster
// where the stub env is set. The first shipped form rewrote every gateway, which
// silently started the listener E2E_AGC_ScaleSetRecovery requires to stay down and
// turned that spec red in CI.
//
// The API base is deliberately not asserted here: it is derived from whatever config
// URL this returns, by the same githubapp.DeriveAPIBaseURL the production path uses
// (Q506), so the GHES rule lives in exactly one place.
func TestScaleSetStubConfigURL(t *testing.T) {
	const stub = "http://fakegithub.e2e-infra.svc.cluster.local:8080"
	cases := []struct {
		name      string
		stubBase  string
		githubURL string
		want      string
		wantOK    bool
	}{
		{
			name:      "stub host: scheme swapped, org path preserved",
			stubBase:  stub,
			githubURL: "https://fakegithub.e2e-infra.svc.cluster.local:8080/e2e-org",
			want:      stub + "/e2e-org",
			wantOK:    true,
		},
		{
			name:      "stub host: owner/repo path preserved",
			stubBase:  "http://stub:8080",
			githubURL: "https://stub:8080/acme/widgets",
			want:      "http://stub:8080/acme/widgets",
			wantOK:    true,
		},
		{
			name:      "different host is left alone",
			stubBase:  stub,
			githubURL: "https://github.com/acme",
			wantOK:    false,
		},
		{
			// The shape E2E_AGC_ScaleSetRecovery relies on: a host that cannot be
			// reached, which must stay unreachable.
			name:      "unresolvable host is left alone",
			stubBase:  stub,
			githubURL: "https://ghes.invalid/ssrecorg",
			wantOK:    false,
		},
		{
			name:      "same host on a different port is left alone",
			stubBase:  stub,
			githubURL: "https://fakegithub.e2e-infra.svc.cluster.local:9/ssrecorg",
			wantOK:    false,
		},
		{
			name:      "no stub configured (production)",
			stubBase:  "",
			githubURL: "https://github.com/acme",
			wantOK:    false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := scaleSetStubConfigURL(tc.stubBase, tc.githubURL)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if got != tc.want {
				t.Errorf("configURL = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestScaleSetSessionHeld_MatchesThroughTheStartWrap pins the error chain the rollout
// path depends on. A predecessor's session makes CreateSession answer 409, and the
// reconciler must recognise that through Listener.Start's own wrap so the retry stays a
// short flat wait rather than the work queue's backoff — which climbs past the teardown
// it is waiting on and idles the set with the session already free. A wrap that stopped
// using %w would restore that failure silently, so the match is asserted through one.
func TestScaleSetSessionHeld_MatchesThroughTheStartWrap(t *testing.T) {
	conflict := fmt.Errorf("scalesetlistener: open session for %q: %w", "linux",
		&scaleset.SessionConflictError{StatusCode: http.StatusConflict})
	if !scaleSetSessionHeld(conflict) {
		t.Error("a wrapped SessionConflictError must be recognised as a held session")
	}

	// The failures that must keep the loud path: they are not a predecessor finishing.
	for name, err := range map[string]error{
		"unrelated": errors.New("dial tcp: connection refused"),
		"nil":       nil,
		"other conflict": fmt.Errorf("generate jit config: %w",
			&scaleset.RunnerNameConflictError{StatusCode: http.StatusConflict}),
	} {
		if scaleSetSessionHeld(err) {
			t.Errorf("%s must not be treated as a held session", name)
		}
	}
}

// TestResolveRunnerGroupName pins the GitHub-side boundary's resolution chain (Q712):
// the set's own spec.runnerGroup, else its gateway's defaultRunnerGroup, else empty
// (GitHub's default group). It mirrors templateRef/proxyRef inheritance, so a tenant
// declares the group once on the gateway and a set overrides it only when it means to.
func TestResolveRunnerGroupName(t *testing.T) {
	cases := []struct {
		name       string
		set        string
		gateway    string
		nilGateway bool
		want       string
	}{
		{name: "set wins over gateway", set: "tenant-a-gpu", gateway: "tenant-a", want: "tenant-a-gpu"},
		{name: "unset set inherits the gateway", gateway: "tenant-a", want: "tenant-a"},
		{name: "both unset means GitHub's default group", want: ""},
		{name: "set alone", set: "tenant-a-gpu", want: "tenant-a-gpu"},
		// The gateway is resolved before this runs on the production path, but a nil
		// must not panic the listener start it feeds.
		{name: "no gateway resolved", set: "tenant-a-gpu", nilGateway: true, want: "tenant-a-gpu"},
		{name: "no gateway resolved, nothing declared", nilGateway: true, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rs := &v2alpha1.RunnerSet{Spec: v2alpha1.RunnerSetSpec{RunnerGroup: tc.set}}
			var gw *v2alpha1.ActionsGateway
			if !tc.nilGateway {
				gw = &v2alpha1.ActionsGateway{
					Spec: v2alpha1.ActionsGatewaySpec{DefaultRunnerGroup: tc.gateway},
				}
			}
			if got := resolveRunnerGroupName(rs, gw); got != tc.want {
				t.Errorf("resolveRunnerGroupName = %q, want %q", got, tc.want)
			}
		})
	}
}
