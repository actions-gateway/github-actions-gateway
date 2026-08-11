package scalesetlistener_test

import (
	"context"
	"testing"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/scalesetlistener"
	"github.com/actions-gateway/github-actions-gateway/scaleset/scalesettest"
	"github.com/stretchr/testify/assert"
)

// Q726 — a runner set's runnerLabels are the scale set's runs-on match targets, all of
// them, and the Listener reports what the server actually kept rather than what it
// asked for.

func extraLabels(labels ...string) func(*scalesetlistener.Config) {
	return func(c *scalesetlistener.Config) { c.ExtraLabels = labels }
}

// TestListener_RegistersEveryDeclaredLabel is the feature: a create carries the name
// label plus every extra one, so `runs-on: [linux, gpu]` has something to match.
func TestListener_RegistersEveryDeclaredLabel(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	l, _ := startListenerFunc(t, srv, fixedCapacity(1), noopProvision, nil,
		extraLabels("gpu", "private-network"))

	assert.Equal(t, []string{"linux", "gpu", "private-network"}, l.Status().RegisteredLabels,
		"the name label must come first, then each extra label in order")
}

// TestListener_SingleLabelSetIsUnchanged pins the compatibility claim the whole design
// rests on: a set with no extra labels registers exactly the one label this tier has
// always registered, so nothing already at GitHub re-registers.
func TestListener_SingleLabelSetIsUnchanged(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	l, _ := startListenerFunc(t, srv, fixedCapacity(1), noopProvision, nil)

	assert.Equal(t, []string{"linux"}, l.Status().RegisteredLabels)
}

// TestListener_DuplicateLabelRegisteredOnce covers a label repeated in runnerLabels —
// including one that repeats the name. The CRD does not enforce uniqueness within the
// list, so the duplicate reaches here, and sending it twice would ask GitHub to hold
// one match target twice.
func TestListener_DuplicateLabelRegisteredOnce(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	l, _ := startListenerFunc(t, srv, fixedCapacity(1), noopProvision, nil,
		extraLabels("gpu", "linux", "gpu"))

	assert.Equal(t, []string{"linux", "gpu"}, l.Status().RegisteredLabels,
		"a repeated label — the name among them — must be registered once")
}

// TestListener_ReportsLabelsTheServerDropped is the GHES appliance below 3.21 without
// DistributedTask.AllowRunnerScaleSetCustomLabels: the create answers 200 having kept
// only the name label. Nothing errors, so the shortfall is observable only by reading
// the labels back — which is what makes reporting them the whole point.
func TestListener_ReportsLabelsTheServerDropped(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)
	srv.DropExtraScaleSetLabels(true)

	l, _ := startListenerFunc(t, srv, fixedCapacity(1), noopProvision, nil,
		extraLabels("gpu", "private-network"))

	assert.Equal(t, []string{"linux"}, l.Status().RegisteredLabels,
		"the Listener must report the server's answer, not the request it sent")
}

// TestListener_ReusedScaleSetReportsItsOwnLabels is the drift half: a scale set that
// already exists is reused untouched, so a label appended to the runner set since it
// was created is not on it. The listener reports the labels the existing set carries
// rather than assuming its own ask took effect.
func TestListener_ReusedScaleSetReportsItsOwnLabels(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	// The scale set an earlier generation registered, carrying the labels declared
	// then.
	ssID := srv.AddScaleSet("linux", 1)
	srv.SetScaleSetLabels(ssID, "linux", "gpu")

	// This generation declares one more. The scale set already exists under the same
	// name, so it is reused untouched and the new label never reaches GitHub.
	l, _ := startListenerFunc(t, srv, fixedCapacity(1), noopProvision, nil,
		extraLabels("gpu", "arm64"))
	assert.Equal(t, []string{"linux", "gpu"}, l.Status().RegisteredLabels,
		"a reused scale set must report the labels it has, not the ones now asked for")
}

// TestListener_NoLabelsInResponseIsNotAShortfall guards the discriminator the reporting
// rests on. A scale set always carries at least its name label, because the name IS a
// label — so a response with none cannot be a true state and must be read as "this
// response did not carry labels". Reading it as a state would report every declared
// label missing on a set that is working, and the read routes that omit them are not
// something this repo has measured.
func TestListener_NoLabelsInResponseIsNotAShortfall(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	// AddScaleSet models one a previous run left behind: registered, with no labels
	// recorded against it, exactly as a response that omits them looks from here.
	srv.AddScaleSet("linux", 1)

	l, _ := startListenerFunc(t, srv, fixedCapacity(1), noopProvision, nil, extraLabels("gpu"))

	assert.Nil(t, l.Status().RegisteredLabels,
		"a label-less response is no observation, and must not be published as one")
}

// noopProvision satisfies the Listener's required ProvisionFunc for tests that never
// enqueue a job.
func noopProvision(context.Context, scalesetlistener.Job) error { return nil }
