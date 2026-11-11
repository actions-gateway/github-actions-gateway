package scaleset_test

import (
	"strings"
	"testing"

	"github.com/actions-gateway/github-actions-gateway/scaleset/scalesettest"
)

// The runnerscalesets name filter is the Actions service's, and whether it is exact
// is unmeasured (Q1325). The AGC adopts whatever this lookup returns, sessions on it
// and PATCHes its runner group, so a looser filter must never hand it a set under
// another name. Each case widens the stub's filter and asserts what the client makes
// of the answer.

func TestGetRunnerScaleSetByName_IgnoresALooserFilterMatch(t *testing.T) {
	srv := scalesettest.New()
	defer srv.Close()
	srv.SetScaleSetNameMatch(strings.HasPrefix)
	srv.AddScaleSet("ci-linux", 1)
	ctx := testContext(t)
	c := newClient(t, srv, nil)
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	got, err := c.GetRunnerScaleSetByName(ctx, "ci")
	if err != nil || got != nil {
		t.Fatalf("GetRunnerScaleSetByName(ci) = %+v, %v; want nil, nil: a set named ci-linux is not the set named ci", got, err)
	}
}

func TestGetRunnerScaleSetByName_PicksTheExactMatchAmongLooserOnes(t *testing.T) {
	srv := scalesettest.New()
	defer srv.Close()
	srv.SetScaleSetNameMatch(strings.HasPrefix)
	srv.AddScaleSet("ci-linux", 1) // lower id, so the stub returns it first
	want := srv.AddScaleSet("ci", 1)
	ctx := testContext(t)
	c := newClient(t, srv, nil)
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	got, err := c.GetRunnerScaleSetByName(ctx, "ci")
	if err != nil || got == nil || got.ID != want || got.Name != "ci" {
		t.Fatalf("GetRunnerScaleSetByName(ci) = %+v, %v; want the set named ci (id %d)", got, err, want)
	}
}

// A set differing only by case is refused rather than adopted or shadowed. Adopting it
// would drive another controller's scale set; creating beside it would register a
// second set GitHub's case-insensitive runs-on matching cannot tell apart.
func TestGetRunnerScaleSetByName_RefusesACaseOnlyMatch(t *testing.T) {
	srv := scalesettest.New()
	defer srv.Close()
	srv.SetScaleSetNameMatch(strings.EqualFold)
	srv.AddScaleSet("ci", 1)
	ctx := testContext(t)
	c := newClient(t, srv, nil)
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	got, err := c.GetRunnerScaleSetByName(ctx, "CI")
	if err == nil {
		t.Fatalf("GetRunnerScaleSetByName(CI) = %+v, nil; want an error naming the case-only match", got)
	}
	if !strings.Contains(err.Error(), `"ci"`) {
		t.Errorf("error %q must name the set it refused to adopt", err)
	}
}

func TestGetRunnerScaleSetByName_RefusesTwoExactMatches(t *testing.T) {
	srv := scalesettest.New()
	defer srv.Close()
	srv.AddScaleSet("ci", 1)
	srv.AddScaleSet("ci", 2)
	ctx := testContext(t)
	c := newClient(t, srv, nil)
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	got, err := c.GetRunnerScaleSetByName(ctx, "ci")
	if err == nil {
		t.Fatalf("GetRunnerScaleSetByName(ci) = %+v, nil; want an error: two sets share the name and the lookup cannot say which is this one", got)
	}
}

// A case-only neighbour does not stop the set that has the exact name: refusing it
// would let anyone who registers "CI" stop a working "ci" from starting.
func TestGetRunnerScaleSetByName_ExactMatchWinsOverACaseOnlyOne(t *testing.T) {
	srv := scalesettest.New()
	defer srv.Close()
	srv.SetScaleSetNameMatch(strings.EqualFold)
	srv.AddScaleSet("CI", 1)
	want := srv.AddScaleSet("ci", 1)
	ctx := testContext(t)
	c := newClient(t, srv, nil)
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	got, err := c.GetRunnerScaleSetByName(ctx, "ci")
	if err != nil || got == nil || got.ID != want {
		t.Fatalf("GetRunnerScaleSetByName(ci) = %+v, %v; want the set named ci (id %d)", got, err, want)
	}
}
