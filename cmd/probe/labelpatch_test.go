package main

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/scaleset/scalesetstub"
	"github.com/actions-gateway/github-actions-gateway/scaleset/scalesettest"
)

// What these tests can and cannot establish. The stub answers a labels PATCH the way
// scalesetstub.LabelPatchMode is set to answer it, so a green suite here says the
// scenario drives the protocol correctly and reads its own evidence correctly. It
// cannot say which mode the Actions Service is in — that is the whole question, and
// only the live run answers it.
//
// What they do protect is the reasoning a live run cannot re-check: that a verdict
// comes from the independent GET rather than the PATCH response, that the create-arm
// control stops the run when a shortfall would be unattributable, and that a
// removal that silently does not happen is reported as a distinct failure from an
// append that does not happen.

func labelPatchEnv(overrides map[string]string) func(string) string {
	env := map[string]string{
		"GITHUB_APP_ID":              "42",
		"GITHUB_APP_INSTALLATION_ID": "99",
		"GITHUB_APP_PRIVATE_KEY":     "-----BEGIN TEST PEM-----\nnot-a-key\n-----END TEST PEM-----",
		"GITHUB_ORG_URL":             "https://github.com/test-org",
	}
	for k, v := range overrides {
		env[k] = v
	}
	return func(k string) string { return env[k] }
}

// newLabelPatchProbeForTest builds the scenario against the stub. Its log is captured
// because the verdicts ARE log lines: that is where the evidence has to be checked.
func newLabelPatchProbeForTest(t *testing.T, srv *scalesettest.Server, w *strings.Builder) *labelPatchProbe {
	t.Helper()
	p, err := newLabelPatchProbe(
		slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug})),
		labelPatchConfig{
			ConfigURL:    "https://github.com/test-org",
			ScaleSetName: "gag-q793-labelpatch",
			GroupName:    "Default",
			PollWindow:   200 * time.Millisecond,
		},
		staticTokenProvider{token: installToken},
		srv.URL,
		srv.HTTPClient(),
		srv.HTTPClient(),
	)
	if err != nil {
		t.Fatalf("newLabelPatchProbe: %v", err)
	}
	if err := p.client.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	return p
}

// runAgainstMode drives the whole scenario against a stub in the given mode and
// returns the log the verdicts were written to.
func runAgainstMode(t *testing.T, mode scalesetstub.LabelPatchMode) string {
	t.Helper()
	srv := scalesettest.New()
	defer srv.Close()
	srv.SetPollTimeout(100 * time.Millisecond)
	srv.SetLabelPatchMode(mode)

	var log strings.Builder
	p := newLabelPatchProbeForTest(t, srv, &log)
	if err := p.run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	return log.String()
}

func TestLabelPatchHonoured(t *testing.T) {
	log := runAgainstMode(t, scalesetstub.LabelPatchHonour)

	for _, want := range []string{
		"arm 0 CONTROL-OK",
		"arm 1 APPEND HONOURED",
		"arm 2 SHRINK HONOURED",
		"arm 3 APPEND-UNDER-SESSION HONOURED",
		"arm 3 SESSION-SURVIVED",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("log missing %q\n--- log ---\n%s", want, log)
		}
	}
	if strings.Contains(log, "NOT-HONOURED") {
		t.Errorf("a honouring backend produced a NOT-HONOURED verdict\n--- log ---\n%s", log)
	}
}

// TestLabelPatchIgnoredReportsNotHonoured is the direction that matters most for the
// row: a service that answers 200 and stores nothing must not read as success.
func TestLabelPatchIgnoredReportsNotHonoured(t *testing.T) {
	log := runAgainstMode(t, scalesetstub.LabelPatchIgnore)

	if !strings.Contains(log, "arm 0 CONTROL-OK") {
		t.Fatalf("the control must still pass — the create path is untouched by the patch mode\n--- log ---\n%s", log)
	}
	if !strings.Contains(log, "arm 1 APPEND NOT-HONOURED") {
		t.Errorf("a PATCH that stored nothing did not produce a NOT-HONOURED verdict\n--- log ---\n%s", log)
	}
	if !strings.Contains(log, `missing=[`+labelPatchAdded) {
		t.Errorf("the verdict does not name the label that never arrived\n--- log ---\n%s", log)
	}
	// Arms 2 and 4 ask a PATCH to change a set and then read it. With arm 1 showing
	// that a PATCH changes nothing, the set they read still equals what they asked
	// for — so a verdict there would report an outcome nothing caused. This is the
	// defect the first live run surfaced.
	if !strings.Contains(log, "arm 2 INCONCLUSIVE") {
		t.Errorf("arm 2 reported a verdict on a set no PATCH moved\n--- log ---\n%s", log)
	}
	if strings.Contains(log, "arm 2 SHRINK HONOURED") {
		t.Errorf("arm 2 called a shrink honoured when the set merely already matched"+
			"\n--- log ---\n%s", log)
	}
	if !strings.Contains(log, "arm 4 INCONCLUSIVE") {
		t.Errorf("arm 4 reported a verdict on a set no PATCH moved\n--- log ---\n%s", log)
	}
	if strings.Contains(log, "NAME-LABEL-PRESERVED") {
		t.Errorf("arm 4 credited the service with reinstating a label it never removed"+
			"\n--- log ---\n%s", log)
	}
	// Arm 3 is deliberately NOT gated: it re-measures the append under a live
	// session, and its session half is readable either way.
	if !strings.Contains(log, "arm 3 APPEND-UNDER-SESSION NOT-HONOURED") {
		t.Errorf("arm 3 was skipped; it asks its own question\n--- log ---\n%s", log)
	}
}

// TestLabelPatchEchoIsNotMistakenForHonoured is what makes the independent GET
// load-bearing rather than decorative. A service echoing its input is byte-identical
// to one that stored it, from the PATCH response alone — so a probe reading that
// response would report HONOURED here, which is the wrong answer.
func TestLabelPatchEchoIsNotMistakenForHonoured(t *testing.T) {
	log := runAgainstMode(t, scalesetstub.LabelPatchEcho)

	if !strings.Contains(log, "arm 1 APPEND NOT-HONOURED") {
		t.Errorf("an echoing backend was read as honouring the patch — the verdict is coming "+
			"from the PATCH response rather than the independent GET\n--- log ---\n%s", log)
	}
	// The echoed set is still reported, as the evidence the verdict is set against.
	if !strings.Contains(log, `echoed="[gag-q793-labelpatch `+labelPatchBase+" "+labelPatchAdded+`]"`) {
		t.Errorf("the echoed label set is not in the log, so the verdict has nothing to contrast "+
			"the GET against\n--- log ---\n%s", log)
	}
}

// TestLabelPatchAdditiveReportsRemovalFailure separates the two ways a reconciler can
// diverge: a backend that takes an append but drops a retraction leaves a label
// matching jobs after the RunnerSet stopped declaring it, which an append-only verdict
// would call success.
func TestLabelPatchAdditiveReportsRemovalFailure(t *testing.T) {
	log := runAgainstMode(t, scalesetstub.LabelPatchAdditive)

	if !strings.Contains(log, "arm 1 APPEND HONOURED") {
		t.Errorf("an additive backend takes an append; that arm should pass\n--- log ---\n%s", log)
	}
	if !strings.Contains(log, "arm 2 NOT-HONOURED (removal)") {
		t.Errorf("an additive backend kept a retracted label and that was not reported\n--- log ---\n%s", log)
	}
}

func TestLabelPatchRefusedIsAVerdictNotAnError(t *testing.T) {
	log := runAgainstMode(t, scalesetstub.LabelPatchRefuse)

	if !strings.Contains(log, "arm 1 (append) PATCH-REFUSED") {
		t.Errorf("a refused PATCH was not reported as a verdict\n--- log ---\n%s", log)
	}
}

// TestLabelPatchCreateControlStopsTheRun is the precondition assert: on a backend that
// drops extra labels at create, every PATCH arm would report a shortfall that is not
// evidence about PATCH, so no arm may run.
func TestLabelPatchCreateControlStopsTheRun(t *testing.T) {
	srv := scalesettest.New()
	defer srv.Close()
	srv.SetPollTimeout(100 * time.Millisecond)
	srv.DropExtraScaleSetLabels(true)
	// Honour is the mode that would produce the most convincing false green: the
	// PATCH machinery works, and only the create-time drop makes the arms unreadable.
	srv.SetLabelPatchMode(scalesetstub.LabelPatchHonour)

	var log strings.Builder
	p := newLabelPatchProbeForTest(t, srv, &log)
	if err := p.run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	out := log.String()

	if !strings.Contains(out, "arm 0 INCONCLUSIVE") {
		t.Fatalf("the create-arm control did not fire\n--- log ---\n%s", out)
	}
	for _, forbidden := range []string{"arm 1", "arm 2", "arm 3", "arm 4"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("%s ran after an INCONCLUSIVE control, so the run reports a verdict it "+
				"cannot support\n--- log ---\n%s", forbidden, out)
		}
	}
}

// TestLabelPatchRefusesAnExistingScaleSet keeps the control armed across runs: a set
// left behind by an earlier run already carries that run's last PATCH, so reusing it
// would skip the create arm entirely.
func TestLabelPatchRefusesAnExistingScaleSet(t *testing.T) {
	srv := scalesettest.New()
	defer srv.Close()
	id := srv.AddScaleSet("gag-q793-labelpatch", 1)
	srv.SetScaleSetLabels(id, "gag-q793-labelpatch", labelPatchBase, labelPatchAdded)

	var log strings.Builder
	p := newLabelPatchProbeForTest(t, srv, &log)
	err := p.run(context.Background())
	if err == nil {
		t.Fatal("run reused an existing scale set instead of refusing it")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error does not name the cause: %v", err)
	}
}

func TestParseLabelPatchConfigDefaults(t *testing.T) {
	cfg, err := parseLabelPatchConfig(labelPatchEnv(nil))
	if err != nil {
		t.Fatalf("parseLabelPatchConfig: %v", err)
	}
	if cfg.ScaleSetName != "gag-q793-labelpatch" {
		t.Errorf("ScaleSetName = %q", cfg.ScaleSetName)
	}
	if cfg.GroupName != "Default" {
		t.Errorf("GroupName = %q", cfg.GroupName)
	}
	if cfg.Keep {
		t.Error("Keep defaults true; a leaked scale set disarms the next run's create control")
	}
	if cfg.PollWindow != 20*time.Second {
		t.Errorf("PollWindow = %v", cfg.PollWindow)
	}
}

func TestParseLabelPatchConfigRequiresOrgURL(t *testing.T) {
	env := labelPatchEnv(map[string]string{"GITHUB_ORG_URL": ""})
	if _, err := parseLabelPatchConfig(env); err == nil {
		t.Fatal("parseLabelPatchConfig accepted a missing GITHUB_ORG_URL")
	}
}
