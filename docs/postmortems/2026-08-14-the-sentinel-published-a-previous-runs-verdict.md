# 2026-08-14: the release sentinel published a previous run's verdict

The watcher for the pre-GA dogfood validation reported **PASSED** for a release candidate that had not started, reading a verdict five days old and five versions stale, and closed with an instruction to report the result.

It was caught because the report named the wrong tag.

## Impact

None realized, and the near-miss is the reason this is written down.

`validate-release.sh` is the gate that decides whether a release candidate may be promoted to a stable tag; [release.md](../operations/release.md#validate-the-release-candidate-on-dogfood) calls a red result a stop-ship.
A false PASS on it authorises exactly the promotion the gate exists to prevent, and a stable tag is not a quiet artifact: it deploys the docs site wholesale, moves the `stable` alias, and a broken chart publish on one reddens every subsequent pull request.

Nothing was promoted.
The gate itself was healthy throughout and later passed on its own merits.

## What the defect was

`progress_init` empties the phase stream and removes the rendered status object.
It was called after `quota_preflight` and `settle_e2e_lane`, on a stated rationale: a run that aborts during preflight should leave no half-stream for a renderer to mistake for an in-flight gate.

That rationale is sound and the placement inverted it.
Preflight is not brief — the settle wait alone runs to `E2E_WAIT_TIMEOUT`, 1800 seconds by default, and on the run before this one it burned 645 seconds before failing.
For that entire window the **previous** run's complete stream sat on disk, terminal event included, and every reader rendered it as the current run's state.

Two readers, not one.
The sentinel reported the stale verdict, and `release-status.sh` rendered the same spent object when asked directly where the gate was.
A fix at either reader would have left the other one lying.

## Timeline

**2026-08-09.** A `v1.4.0-rc.2` validation run completes and passes, leaving `tmp/release-validation-progress.jsonl`, its rendered status object, and a stall marker on disk.
Nothing cleans them up; nothing is supposed to.

**2026-08-14, first attempt.** A `v1.5.0-rc.1` gate is launched.
It fails in preflight on an unrelated defect ([#1498](https://github.com/actions-gateway/github-actions-gateway/pull/1498)) after 645 seconds, having written no event of its own.

**2026-08-14, second attempt.** The gate is relaunched and enters the settle wait.
It has again written nothing, because it is still in preflight.

**2026-08-14.** The sentinel is launched against it.
It reads the 2026-08-09 stream, finds that run's terminal `passed` event, and exits reporting `Gate: passed`, `RC: v1.4.0-rc.2`, elapsed `101h53m`, run URL `owner/repo/actions/runs/42` — the placeholder from its own test fixtures.
Its closing line is `Next action: report the result to the operator. Do NOT relaunch this watcher.`

**2026-08-14.** The mismatch between the reported tag and the tag under validation is noticed before the verdict is relayed.
The gate's own log shows it still in the settle wait at `30s/1800s`; `record-launch.sh --list` shows the process alive; both stream files are dated five days earlier.

**2026-08-14.** The stale files are archived under `v1.4.0-rc.2` names rather than deleted, since the 1.4 record is cited in that release's plan doc.
The sentinel is relaunched against an empty stream and goes correctly silent.

**2026-08-14.** [#1500](https://github.com/actions-gateway/github-actions-gateway/pull/1500) moves the reset before preflight.

## Contributing factors

**A spent stream is indistinguishable from a current one.** No record carries the run it belongs to in a way any reader checks.
The RC tag is present in the stream and rendered into the report, and nothing compares it to the run being watched.

**The window was assumed to be short.** Placing the reset after preflight is only safe if preflight is brief.
It is the opposite: it holds the two longest waits the gate has before it spends anything.

**One stale file fed two independent readers.** The sentinel and `release-status.sh` share the stream and the same blind spot, so the defect presented twice and could have been "fixed" once.

**The report contained its own refutation, three times over.** A tag from a different release line, an elapsed time of 101 hours, and a placeholder run id are each individually impossible for a live run.
All three were rendered, none was checked, and the report still closed with an instruction to act on the verdict.

**A verdict-shaped output invites relay rather than inspection.** The sentinel exists so an hour-long gate does not need watching; the value it offers is not having to look.
That is precisely what makes a wrong verdict from it expensive.

## Action items

**Mitigative, shipped.** [#1500](https://github.com/actions-gateway/github-actions-gateway/pull/1500) empties the stream before preflight instead of after it, so no reader can meet a spent run's terminal event.
The reset is skipped only when the target's lease is `held`, which is the concurrent-gate case the lease refuses moments later anyway.
The regression test reproduces the published false verdict: deleting the mechanism takes the assertions red with `got 'passed'` and `got 'v1.4.0-rc.2'`.

**Preventative: the class, not the instance.** The general shape is a reader trusting a file without checking it belongs to this run, and the fix landed at the writer for that reason: the ambiguous state stops existing, so every reader benefits, including ones not yet written.
A reader-side identity check was considered and rejected as the primary fix, because it repairs the tool that failed loudly and leaves the quieter one unchanged.

**Preventative: the reporting discipline.** An explanation offered on the strength of a plausible-looking tool output is a claim like any other.
The rule that this session's `stalled` event was "a defect" was made the same way and was also wrong; both are recorded in [testing.md](../development/testing.md#the-status-you-report-is-a-claim-too).

## What this does not cover

The gate still has no retry on a transient API failure, which cost the first attempt of this same release candidate; that is tracked separately.
And a run whose job log GitHub will not serve still produces no heartbeat for the whole leg, which is what made a quiet 22-minute stretch indistinguishable from a stalled one on the run that eventually passed.
