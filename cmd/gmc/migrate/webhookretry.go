package main

import (
	"context"
	"io"
	"regexp"
	"time"
)

// webhookTransportErrorRe matches the apiserver's error text when it could not
// COMPLETE a call to an admission webhook — a transport-level failure (the webhook
// Service had no ready endpoint for the picked pod, its TLS listener was mid-restart,
// or the POST outran its per-call deadline) — as opposed to the webhook running and
// DENYING the request. The apiserver emits these phrases only for the unreachable
// case; a genuine rejection reads `admission webhook "…" denied the request: …` and
// matches none of them, so a denial is never retried.
//
// The signatures are deliberately identical to the e2e helper's
// (cmd/gmc/test/utils/resources.go, Q392) so the two lists cannot drift into
// disagreeing about what "transient" means.
var webhookTransportErrorRe = regexp.MustCompile(
	`(?i)failed calling webhook|failed to call webhook|context deadline exceeded|` +
		`connection refused|no route to host|no endpoints available`)

// Retry pacing. These are vars, not consts, only so tests can shrink them — nothing
// at runtime reassigns them.
var (
	// webhookRetryBudget bounds the total time spent riding out an unreachable
	// webhook. A persistent outage still surfaces the last transport error once it
	// elapses — this waits for the webhook to actually respond, it does not paper a
	// real outage over.
	webhookRetryBudget = 90 * time.Second
	// webhookRetryGap is the pause between attempts. A webhook endpoint that is
	// merely rolling out is typically back well inside one or two gaps.
	webhookRetryGap = 3 * time.Second
)

// retryOnTransientWebhookError runs op, retrying ONLY while the apiserver reports it
// could not REACH an admission webhook.
//
// Every v2 object gag-migrate creates passes through a validating webhook served by
// the GMC Deployment under `failurePolicy: Fail`, and the apiserver does not retry a
// webhook call of its own. So any transient unavailability of that endpoint — a
// rolling update, a node drain, an evicted pod, a cold TLS listener — aborts the
// create outright. That matters more here than almost anywhere else in the codebase
// because applyResult is a non-atomic sequence: a stall on the fourth object leaves
// the first three created, and the cluster-scoped ClusterRunnerTemplate among them is
// the one output that deleting the tenant namespace does not reclaim (§H.17.1).
// Riding out the blip keeps a one-shot migration from half-landing on a hiccup.
//
// It is NOT a blind retry: a clean call returns nil immediately, and a genuine
// webhook DENIAL (or any other error — AlreadyExists, Forbidden, a CEL rejection) is
// returned on the FIRST attempt so real failures fail fast and loudly instead of
// burning the budget. Progress is reported to stderr rather than stalling silently,
// so an operator watching an --apply can tell "waiting on a webhook" from "hung".
func retryOnTransientWebhookError(ctx context.Context, what string, stderr io.Writer, op func() error) error {
	deadline := time.Now().Add(webhookRetryBudget)
	attempts := 0
	for {
		err := op()
		if err == nil || !webhookTransportErrorRe.MatchString(err.Error()) {
			return err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return err
		}
		attempts++
		if attempts == 1 {
			fprintf(stderr, "admission webhook unreachable applying %s: %v\n", what, err)
			fprintf(stderr, "this is usually transient (webhook endpoint rolling out); retrying for up to %s\n",
				remaining.Truncate(time.Second))
		} else {
			fprintf(stderr, "  %s: still unreachable (attempt %d, %s left)\n",
				what, attempts, remaining.Truncate(time.Second))
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(webhookRetryGap):
		}
	}
}
