// Investigation I (Q793): is a runner-scale-set labels PATCH honoured?
//
// Set PROBE_LABELPATCH_TEST=true to run this scenario. It settles the one
// unknown Q726 left open: a scale set registers its labels at create, and
// nothing has ever measured whether PATCHing that list reaches the store.
// ARC never patches — it reuses an existing scale set untouched — so upstream
// carries no answer either, and Q712's group PATCH is stub-tested only.
//
// Until this runs, a reconciler that appends a label to a live RunnerSet has no
// evidence it would do anything at all.
//
//	installation token
//	  → POST {api}/orgs/{org}/actions/runners/registration-token
//	  → POST {api}/actions/runner-registration          (RemoteAuth hop)
//	  → POST {svc}/_apis/runtime/runnerscalesets        (throwaway, 2 labels)
//	  → PATCH/GET x4                                    (the arms below)
//	  → cleanup: DELETE session, DELETE scale set
//
// # The arms, and why they run in this order
//
//  0. create      — registers name + a base extra label. This is the CONTROL:
//     an appliance that drops extra labels at create (the GHES
//     <3.21 shape) makes every PATCH verdict below unreadable,
//     because a shortfall after a PATCH would be indistinguishable
//     from the create-time drop. The run stops INCONCLUSIVE there
//     rather than reporting a NOT-HONOURED it cannot support.
//  1. append      — PATCH a third label on. The question the row asks.
//  2. shrink      — PATCH back down to name + base. A reconciler that can add
//     but not remove diverges silently from the declared set, so
//     "honoured" has to mean both directions. Runs only when arm 1
//     landed: otherwise the set already carries what the shrink asks
//     for, and the arm would name an outcome nothing caused.
//  3. live session — repeat the append with a session open, then poll. Q726
//     recorded the behaviour against a set with a live session as
//     unknown, and a PATCH that invalidates the session would make
//     in-place reconcile cost a listener restart.
//  4. name omitted — PATCH a list whose first entry is NOT the scale set's name.
//     Label[0] is load-bearing identity (Q726), so what the service
//     does with a list that drops it decides whether the reconcile
//     path may ever send a caller-ordered list straight through.
//     Gated on arm 1 for the same reason as arm 2.
//
// # What the verdicts rest on
//
// Every verdict is read from an INDEPENDENT GET, never from the PATCH response
// body. A service is free to echo the request it was handed, so a PATCH that
// returns the labels asked for is consistent with a store that took none of
// them — the same distinction Investigation G draws between what the client made
// of a response and what the wire did.
//
// Required environment variables:
//
//	GITHUB_APP_ID              - GitHub App numeric ID
//	GITHUB_APP_PRIVATE_KEY     - Path to PEM file, or PEM literal
//	GITHUB_APP_INSTALLATION_ID - Installation ID for the target org
//	GITHUB_ORG_URL             - Org or repo URL (e.g. https://github.com/my-org)
//
// Optional:
//
//	PROBE_LABELPATCH_NAME       - Scale set name (default gag-q793-labelpatch).
//	PROBE_LABELPATCH_GROUP_NAME - Runner group (default Default).
//	PROBE_LABELPATCH_KEEP       - "true" leaves the scale set registered.
//
// Everything the probe creates is deleted on exit.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/actions-gateway/github-actions-gateway/githubapp"
	"github.com/actions-gateway/github-actions-gateway/scaleset"
)

// labelPatchConfig holds the parsed environment for the labels-PATCH scenario.
// Like the other scale-set scenarios it needs no broker URL and no pre-registered
// agent: the whole question lives in the _apis/runtime scale-set object.
type labelPatchConfig struct {
	AppID          int64
	InstallationID int64
	PrivateKeyPEM  []byte
	ConfigURL      string
	ScaleSetName   string
	GroupName      string
	// Keep leaves the scale set registered on exit, for a follow-up read by
	// hand. Off by default — a leaked scale set is reused by the next run,
	// whose create-arm control would then never execute.
	Keep bool
	// PollWindow bounds the live-session arm's single post-PATCH poll. It is
	// not waiting for a job: the poll is there to observe whether the session
	// still answers, so a short window is the whole point.
	PollWindow time.Duration
}

// parseLabelPatchConfig reads and validates the scenario environment from the
// injected getenv function (normally os.Getenv).
func parseLabelPatchConfig(getenv func(string) string) (labelPatchConfig, error) {
	var cfg labelPatchConfig

	appIDStr, err := mustEnv(getenv, "GITHUB_APP_ID")
	if err != nil {
		return labelPatchConfig{}, err
	}
	if _, err := fmt.Sscan(appIDStr, &cfg.AppID); err != nil {
		return labelPatchConfig{}, fmt.Errorf("parse GITHUB_APP_ID: %w", err)
	}
	installIDStr, err := mustEnv(getenv, "GITHUB_APP_INSTALLATION_ID")
	if err != nil {
		return labelPatchConfig{}, err
	}
	if _, err := fmt.Sscan(installIDStr, &cfg.InstallationID); err != nil {
		return labelPatchConfig{}, fmt.Errorf("parse GITHUB_APP_INSTALLATION_ID: %w", err)
	}
	pemValue, err := mustEnv(getenv, "GITHUB_APP_PRIVATE_KEY")
	if err != nil {
		return labelPatchConfig{}, err
	}
	cfg.PrivateKeyPEM, err = loadPEM(pemValue)
	if err != nil {
		return labelPatchConfig{}, fmt.Errorf("load GITHUB_APP_PRIVATE_KEY: %w", err)
	}
	cfg.ConfigURL, err = mustEnv(getenv, "GITHUB_ORG_URL")
	if err != nil {
		return labelPatchConfig{}, err
	}

	cfg.ScaleSetName = getenv("PROBE_LABELPATCH_NAME")
	if cfg.ScaleSetName == "" {
		cfg.ScaleSetName = "gag-q793-labelpatch"
	}
	cfg.GroupName = getenv("PROBE_LABELPATCH_GROUP_NAME")
	if cfg.GroupName == "" {
		cfg.GroupName = "Default"
	}
	cfg.Keep = getenv("PROBE_LABELPATCH_KEEP") == "true"
	if cfg.PollWindow, err = parseDurationEnv(getenv, "PROBE_LABELPATCH_POLL_WINDOW", 20*time.Second); err != nil {
		return labelPatchConfig{}, err
	}
	return cfg, nil
}

// The two extra labels the arms move on and off the scale set. Neither is a
// plausible runs-on target for anything real, so a stray dispatch cannot land on
// this throwaway set while the probe holds it.
const (
	labelPatchBase  = "gag-q793-base"
	labelPatchAdded = "gag-q793-added"
)

// labelPatchProbe carries the dependencies of the labels-PATCH scenario. As in
// every other scenario the calls go through the shipping scaleset.Client, so a
// live run is evidence about the code GAG ships rather than about a probe-local
// dialect — which is what makes the verdict usable by the reconciler this row
// goes on to build.
type labelPatchProbe struct {
	log    *slog.Logger
	cfg    labelPatchConfig
	client *scaleset.Client
}

// newLabelPatchProbe builds the scenario around a scaleset.Client wired to the
// probe's wire logger. hc and pollClient may be nil to take the library defaults.
func newLabelPatchProbe(logger *slog.Logger, cfg labelPatchConfig, provider githubapp.TokenProvider,
	apiBase string, hc, pollClient *http.Client) (*labelPatchProbe, error) {
	client, err := scaleset.New(scaleset.Config{
		TokenProvider: provider,
		ConfigURL:     cfg.ConfigURL,
		APIBase:       apiBase,
		HTTPClient:    hc,
		PollClient:    pollClient,
		Observer:      wireLog{log: logger, tag: "I"},
	})
	if err != nil {
		return nil, err
	}
	return &labelPatchProbe{log: logger, cfg: cfg, client: client}, nil
}

// runLabelPatchProbe is the Investigation I entry point wired from run().
func runLabelPatchProbe(ctx context.Context, logger *slog.Logger, cfg labelPatchConfig,
	provider githubapp.TokenProvider, apiBase string) error {
	p, err := newLabelPatchProbe(logger, cfg, provider, apiBase, nil, nil)
	if err != nil {
		return err
	}
	return p.run(ctx)
}

func (p *labelPatchProbe) run(ctx context.Context) error {
	if err := p.client.Connect(ctx); err != nil {
		return err
	}
	p.log.Info("INVESTIGATION-I: admin connection established")

	ss, err := p.createScaleSet(ctx)
	if err != nil {
		return err
	}
	if !p.cfg.Keep {
		// On a WithoutCancel context: a Ctrl-C mid-run should still clean up
		// rather than strand a registered scale set, whose survival would then
		// disarm the create-arm control on the next run.
		defer p.deleteScaleSet(context.WithoutCancel(ctx), ss.ID)
	}

	if !p.measureCreate(ss) {
		return nil
	}
	appended, err := p.measureAppend(ctx, ss)
	if err != nil {
		return err
	}
	// Arms 2 and 4 both read a label set they asked a PATCH to change. When arm 1
	// established that a labels PATCH changes nothing, neither can distinguish "the
	// service did what I asked" from "the set already looked like this and nothing
	// happened" — on a backend that ignores the field those two are the same bytes.
	// Arm 3 is not gated: it re-measures the append under a live session, which is
	// its own question, and its session half stands either way.
	if appended {
		if err := p.measureShrink(ctx, ss); err != nil {
			return err
		}
	} else {
		p.log.Warn("INVESTIGATION-I: arm 2 INCONCLUSIVE — arm 1 showed a labels PATCH changing " +
			"nothing, so a shrink cannot be observed: the set already carries the label set the " +
			"shrink asks for, and reporting HONOURED there would name an outcome nothing caused")
	}
	if err := p.measureUnderSession(ctx, ss); err != nil {
		return err
	}
	if !appended {
		p.log.Warn("INVESTIGATION-I: arm 4 INCONCLUSIVE — a PATCH that does not move labels " +
			"cannot drop the name label either, so the set surviving with its name intact is " +
			"evidence about arm 1's verdict rather than about omitting the name")
		return nil
	}
	return p.measureNameOmitted(ctx, ss)
}

// ── Arm 0: create (the control) ──────────────────────────────────────────────

// createScaleSet registers the throwaway set carrying the name label and one
// extra. It refuses to reuse an existing set: a set left over from an earlier run
// already carries whatever that run's last PATCH left, which is precisely the
// state the create-arm control has to rule out.
func (p *labelPatchProbe) createScaleSet(ctx context.Context) (*scaleset.RunnerScaleSet, error) {
	existing, err := p.client.GetRunnerScaleSetByName(ctx, p.cfg.ScaleSetName)
	if err != nil {
		return nil, fmt.Errorf("lookup scale set %q: %w", p.cfg.ScaleSetName, err)
	}
	if existing != nil {
		return nil, fmt.Errorf("scale set %q already exists (id %d) carrying labels %v: "+
			"its label set is whatever an earlier run left, so the create-arm control cannot "+
			"run and no PATCH verdict below would be readable; delete it and re-run",
			p.cfg.ScaleSetName, existing.ID, labelNames(existing.Labels))
	}
	groupID := 1
	if id, ok, gErr := p.client.ResolveRunnerGroup(ctx, p.cfg.GroupName); gErr != nil {
		p.log.Warn("INVESTIGATION-I: runnergroups lookup failed; falling back to group id 1", "error", gErr)
	} else if ok {
		groupID = id
	}
	ss, err := p.client.CreateRunnerScaleSet(ctx, scaleset.RunnerScaleSet{
		Name:          p.cfg.ScaleSetName,
		RunnerGroupID: groupID,
		Labels: []scaleset.Label{
			{Name: p.cfg.ScaleSetName, Type: "System"},
			{Name: labelPatchBase, Type: "System"},
		},
		RunnerSetting: &scaleset.RunnerSetting{Ephemeral: true},
	})
	if err != nil {
		return nil, fmt.Errorf("create scale set: %w", err)
	}
	p.log.Info("INVESTIGATION-I: scale set created",
		"id", ss.ID, "name", ss.Name, "runnerGroupId", ss.RunnerGroupID,
		"labels", labelNames(ss.Labels))
	return ss, nil
}

// measureCreate is the control every arm below rests on: it reports whether this
// backend honours an extra label AT CREATE, which Q726 already ships the
// reconciler-side detection for. It returns false when it does not — a backend
// that drops extras at create would answer every PATCH arm with a shortfall that
// says nothing about PATCH.
func (p *labelPatchProbe) measureCreate(ss *scaleset.RunnerScaleSet) bool {
	got := labelNames(ss.Labels)
	if !hasLabel(got, labelPatchBase) {
		p.log.Warn("INVESTIGATION-I: arm 0 INCONCLUSIVE — the create response does not carry the "+
			"extra label, so this backend drops extras before any PATCH is in play (the GHES "+
			"<3.21 shape Q726 records); every arm below would report a shortfall that is not "+
			"evidence about PATCH, so the run stops here",
			"asked", []string{p.cfg.ScaleSetName, labelPatchBase}, "registered", got)
		return false
	}
	p.log.Info("INVESTIGATION-I: arm 0 CONTROL-OK — the backend honours an extra label at create, "+
		"so a shortfall in any arm below is attributable to PATCH", "registered", got)
	return true
}

// ── Arm 1: append ────────────────────────────────────────────────────────────

// measureAppend reports whether the appended label reached the store. Its bool is
// the premise arms 2 and 4 rest on: a backend that ignores the labels field leaves
// them observing a set nothing touched.
func (p *labelPatchProbe) measureAppend(ctx context.Context, ss *scaleset.RunnerScaleSet) (bool, error) {
	want := []string{p.cfg.ScaleSetName, labelPatchBase, labelPatchAdded}
	got, err := p.patchThenRead(ctx, ss, want, "arm 1 (append)")
	if err != nil {
		return false, err
	}
	p.verdict("arm 1 APPEND", want, got)
	return got != nil && hasLabel(got, labelPatchAdded), nil
}

// ── Arm 2: shrink ────────────────────────────────────────────────────────────

func (p *labelPatchProbe) measureShrink(ctx context.Context, ss *scaleset.RunnerScaleSet) error {
	want := []string{p.cfg.ScaleSetName, labelPatchBase}
	got, err := p.patchThenRead(ctx, ss, want, "arm 2 (shrink)")
	if err != nil {
		return err
	}
	// A shrink is the one arm where a SUPERSET is the failure: the service
	// keeping the removed label means a reconciler can add and never remove,
	// which diverges from the declared set with nothing reporting it.
	if hasLabel(got, labelPatchAdded) {
		p.log.Warn("INVESTIGATION-I: arm 2 NOT-HONOURED (removal) — the label the previous arm "+
			"added survived a PATCH that omitted it, so labels are additive at this backend; a "+
			"reconciler could append but never retract, and a retracted label would keep matching",
			"asked", want, "registered", got)
		return nil
	}
	p.verdict("arm 2 SHRINK", want, got)
	return nil
}

// ── Arm 3: PATCH under a live session ────────────────────────────────────────

// measureUnderSession repeats the append with a session open, which is the state
// a running listener is always in. Q726 recorded this case as unknown, and it is
// the one that decides the cost of in-place reconcile: a PATCH that invalidates
// the session makes a label edit a listener restart rather than a wire call.
func (p *labelPatchProbe) measureUnderSession(ctx context.Context, ss *scaleset.RunnerScaleSet) error {
	sess, err := p.client.CreateSession(ctx, ss.ID, "gag-q793-probe")
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	defer p.deleteSession(context.WithoutCancel(ctx), ss.ID, sess.SessionID)
	p.log.Info("INVESTIGATION-I: arm 3 session open", "sessionId", sess.SessionID)

	want := []string{p.cfg.ScaleSetName, labelPatchBase, labelPatchAdded}
	got, err := p.patchThenRead(ctx, ss, want, "arm 3 (under session)")
	if err != nil {
		return err
	}
	p.verdict("arm 3 APPEND-UNDER-SESSION", want, got)

	// The session's own survival is the other half of this arm, and it is read
	// from the queue rather than inferred from the PATCH succeeding. A window
	// expiry is the expected outcome — nothing is queued — so it is the ERROR
	// case that carries the finding.
	pollCtx, cancel := context.WithTimeout(ctx, p.cfg.PollWindow)
	defer cancel()
	if _, err := p.client.GetMessage(pollCtx, sess, 1, 0); err != nil && pollCtx.Err() == nil {
		p.log.Warn("INVESTIGATION-I: arm 3 SESSION-BROKEN — the queue refused a poll after the "+
			"PATCH, so a labels PATCH disturbs a live session and in-place reconcile costs a "+
			"listener restart rather than one wire call", "error", err)
		return nil
	}
	p.log.Info("INVESTIGATION-I: arm 3 SESSION-SURVIVED — the queue still answered after the " +
		"PATCH, so a labels PATCH does not by itself invalidate a live session")
	return nil
}

// ── Arm 4: a list that omits the name label ──────────────────────────────────

// measureNameOmitted PATCHes a list whose first entry is not the scale set's own
// name. Q726 made runnerLabels[0] load-bearing identity, so the service's answer
// here decides whether a reconciler may pass a caller-ordered list straight
// through or must always compose the name in first.
//
// It runs last: whatever it leaves behind, nothing downstream reads.
func (p *labelPatchProbe) measureNameOmitted(ctx context.Context, ss *scaleset.RunnerScaleSet) error {
	want := []string{labelPatchBase, labelPatchAdded}
	got, err := p.patchThenRead(ctx, ss, want, "arm 4 (name omitted)")
	if err != nil {
		return err
	}
	after, err := p.client.GetRunnerScaleSet(ctx, ss.ID)
	if err != nil {
		return fmt.Errorf("arm 4 re-read scale set: %w", err)
	}
	switch {
	case after.Name != ss.Name:
		p.log.Warn("INVESTIGATION-I: arm 4 RENAMED — omitting the name label from the PATCH "+
			"changed the scale set's name, which orphans it from the listener that owns it; the "+
			"reconciler must compose the name label in first, unconditionally",
			"nameBefore", ss.Name, "nameAfter", after.Name, "registered", got)
	case !hasLabel(got, ss.Name):
		p.log.Warn("INVESTIGATION-I: arm 4 NAME-LABEL-DROPPED — the set kept its name but lost "+
			"the matching label, so every job targeting the scale-set name stops matching; the "+
			"reconciler must compose the name label in first, unconditionally",
			"name", after.Name, "registered", got)
	default:
		p.log.Info("INVESTIGATION-I: arm 4 NAME-LABEL-PRESERVED — the service reinstated the "+
			"name label the PATCH omitted, so a caller-ordered list cannot orphan the set; "+
			"composing the name in first is still what the reconciler does, now as belt-and-braces "+
			"rather than as the thing standing between an edit and an orphaned scale set",
			"asked", want, "registered", got)
	}
	return nil
}

// ── Shared mechanics ─────────────────────────────────────────────────────────

// patchThenRead sends one labels PATCH and returns the label names an INDEPENDENT
// GET reports afterwards.
//
// The GET is the whole point. A service may echo the body it was handed, so the
// PATCH response is consistent with a store that took nothing; both are logged,
// and only the GET feeds a verdict.
//
// RunnerGroupID is carried over explicitly because RunnerScaleSet.RunnerGroupID
// has no omitempty — a patch that left it zero would ask to move the set into
// group 0 while measuring something else entirely.
func (p *labelPatchProbe) patchThenRead(ctx context.Context, ss *scaleset.RunnerScaleSet,
	want []string, arm string) ([]string, error) {
	labels := make([]scaleset.Label, 0, len(want))
	for _, name := range want {
		labels = append(labels, scaleset.Label{Name: name, Type: "System"})
	}
	patched, err := p.client.UpdateRunnerScaleSet(ctx, ss.ID, scaleset.RunnerScaleSet{
		Name:          ss.Name,
		RunnerGroupID: ss.RunnerGroupID,
		Labels:        labels,
	})
	if err != nil {
		p.log.Warn("INVESTIGATION-I: "+arm+" PATCH-REFUSED — the service rejected the labels "+
			"PATCH outright, which is a NOT-HONOURED verdict with a reason attached",
			"asked", want, "error", err)
		return nil, nil
	}
	p.log.Info("INVESTIGATION-I: "+arm+" patch response (NOT the verdict — a service may echo "+
		"the request it was handed)", "asked", want, "echoed", labelNames(patched.Labels))

	fresh, err := p.client.GetRunnerScaleSet(ctx, ss.ID)
	if err != nil {
		return nil, fmt.Errorf("%s: re-read scale set: %w", arm, err)
	}
	got := labelNames(fresh.Labels)
	p.log.Info("INVESTIGATION-I: "+arm+" independent GET", "registered", got)
	return got, nil
}

// verdict reports one arm against the set it asked for. A nil got is the
// PATCH-REFUSED case, already reported at the point of refusal.
func (p *labelPatchProbe) verdict(arm string, want, got []string) {
	if got == nil {
		return
	}
	if missing := missingFrom(want, got); len(missing) > 0 {
		p.log.Warn("INVESTIGATION-I: "+arm+" NOT-HONOURED — the PATCH returned success and the "+
			"label set an independent GET reports is still short; a reconciler built on this "+
			"would report having reconciled while GitHub matched nothing new",
			"asked", want, "registered", got, "missing", missing)
		return
	}
	p.log.Info("INVESTIGATION-I: "+arm+" HONOURED — an independent GET reports every label the "+
		"PATCH asked for", "asked", want, "registered", got)
}

// deleteSession tears the arm-3 session down on a best-effort basis.
func (p *labelPatchProbe) deleteSession(ctx context.Context, scaleSetID int, sessionID string) {
	dCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := p.client.DeleteSession(dCtx, scaleSetID, sessionID); err != nil {
		p.log.Error("INVESTIGATION-I: delete session failed", "sessionId", sessionID, "error", err)
		return
	}
	p.log.Info("INVESTIGATION-I: session deleted", "sessionId", sessionID)
}

// deleteScaleSet removes the throwaway scale set.
func (p *labelPatchProbe) deleteScaleSet(ctx context.Context, scaleSetID int) {
	dCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := p.client.DeleteRunnerScaleSet(dCtx, scaleSetID); err != nil {
		p.log.Error("INVESTIGATION-I: delete scale set failed — it is still registered, and a "+
			"later run will refuse to start rather than reuse it; delete it by hand",
			"id", scaleSetID, "error", err)
		return
	}
	p.log.Info("INVESTIGATION-I: scale set deleted", "id", scaleSetID)
}

// labelNames projects the label names out of a wire label list, preserving order
// — order is itself part of what the arms observe, since label[0] names the set.
func labelNames(labels []scaleset.Label) []string {
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		out = append(out, l.Name)
	}
	return out
}

// hasLabel reports whether name is in got.
func hasLabel(got []string, name string) bool {
	for _, g := range got {
		if g == name {
			return true
		}
	}
	return false
}

// missingFrom returns the want entries absent from got, sorted so the log line is
// stable across runs.
func missingFrom(want, got []string) []string {
	var missing []string
	for _, w := range want {
		if !hasLabel(got, w) {
			missing = append(missing, w)
		}
	}
	sort.Strings(missing)
	return missing
}
