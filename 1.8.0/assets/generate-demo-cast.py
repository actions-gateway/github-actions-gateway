#!/usr/bin/env python3
"""Hand-author an asciinema v2 cast for the GAG local-kind demo (Q193).

Every command and every line of output below is transcribed from a REAL run
captured under tmp/q193-demo/cap/ (kind cluster + real GitHub, 2026-07-03).
Timing is authored here for pacing; content is not invented.

Design: a *value-first* recording, not a tutorial. As few words as possible —
two short captions — and the payoff (a worker pod appears for one job, runs, and
is deleted) is on screen within a few seconds. Short inline annotations on the
`kubectl -w` rows do the narrating; each key frame dwells long enough to read.
The full install/onboard steps live as text on the demo page.
"""
import json

WIDTH, HEIGHT = 100, 27
E = "\x1b"
PROMPT = f"{E}[1;32m➜{E}[0m {E}[1;36mgag-demo{E}[0m {E}[1;34m${E}[0m "
DIM = f"{E}[38;5;245m"      # inline annotations
SAY = f"{E}[1;38;5;45m"     # the (few) caption lines
RST = f"{E}[0m"
GRN = f"{E}[32m"
YEL = f"{E}[33m"
CYN = f"{E}[36m"

events = []
t = 0.0

def emit(s):
    global t
    events.append([round(t, 3), "o", s])

def wait(dt):
    global t
    t += dt

def say(line, pause=2.0):
    emit(f"{SAY}{line}{RST}\r\n")
    wait(pause)

def blank(pause=0.4):
    """A visual separator between command blocks — one empty line."""
    emit("\r\n")
    wait(pause)

def command(cmd, pre=0.5, post=0.7):
    """Reveal the prompt + whole command, then Enter. Continuation lines use
    "\\\n" in the source; convert the bare LF to CRLF so a wrapped command
    returns the cursor to column 0 rather than indenting to the prompt width."""
    emit(PROMPT + cmd.replace("\n", "\r\n"))
    wait(pre)
    emit("\r\n")
    wait(post)

def output(text, chunk_pause=0.1, post=1.0):
    for ln in text.split("\n"):
        emit(ln + "\r\n")
        wait(chunk_pause)
    wait(post)

def watch_row(name, ready, status, age, annot="", color=""):
    """One `kubectl get pods -w` row with fixed visible-width columns so ANSI
    colour codes (zero display width) don't break alignment and nothing wraps."""
    seg = (f"{color}{status}{RST}" if color else status) + " " * (18 - len(status))
    line = f"{name:<45}{ready:<8}{seg}{age:<5}"
    if annot:
        line += f"{DIM}{annot}{RST}"
    return line

# ── One caption, then straight to the job ────────────────────────────────────
say("#  GitHub Actions Gateway — trigger one CI job, watch the pod:", pause=2.2)
command("gh workflow run test-job.yml --repo $ORG/$REPO --ref main")
output(f"{DIM}https://github.com/$ORG/$REPO/actions/runs/28687049624{RST}", post=0.7)
blank()

# ── The payoff: one `-w` shows idle → pod appears, runs, and is deleted ───────
command("kubectl get pods -n team-a -w", post=0.6)
RUNNER = "runner-…-6d8749c-b0587c8-47200999"
emit(watch_row("NAME", "READY", "STATUS", "AGE") + "\r\n"); wait(0.3)
emit(watch_row("actions-gateway-controller-79ccdddbcc-9phzv", "1/1", "Running", "9m") + "\r\n"); wait(0.25)
emit(watch_row("actions-gateway-proxy-5cfbb9584d-sjwjj", "1/1", "Running", "9m", "← no runner pods yet") + "\r\n"); wait(2.4)
emit(watch_row(RUNNER, "0/1", "Pending", "0s", "← new pod for the job", YEL) + "\r\n"); wait(2.2)
emit(watch_row(RUNNER, "0/1", "ContainerCreating", "1s", color=YEL) + "\r\n"); wait(1.6)
emit(watch_row(RUNNER, "1/1", "Running", "3s", "← the job runs here", GRN) + "\r\n"); wait(2.8)
emit(watch_row(RUNNER, "0/1", "Completed", "9s", "← job done", CYN) + "\r\n"); wait(1.8)
emit(watch_row(RUNNER, "0/1", "Terminating", "9s", "← deleted, back to idle") + "\r\n"); wait(2.8)
emit(f"{DIM}^C{RST}\r\n"); wait(1.2)
blank()

# ── Proof + one-line payoff ──────────────────────────────────────────────────
say("#  Green on GitHub — it really ran (8s):", pause=1.6)
command("gh run view 28687049624 --repo $ORG/$REPO")
output(
f"""{GRN}✓{RST} main Test self-hosted runner · 28687049624
Triggered via workflow_dispatch less than a minute ago

JOBS
{GRN}✓{RST} test in 8s (ID 85081558995)""", post=2.0)
blank()

# The closing frame is the whole story (idle → pod lifecycle → green → payoff),
# so hold it a good while before the loop repeats — a short hold buries the lead
# and is frustrating if you were mid-read when it restarts.
say("#  One job → one short-lived pod → gone. No idle runners, no idle GPUs.", pause=7.0)

# svg-term ends the animation at the last event's timestamp, so without a
# trailing event the final caption would flash at the loop boundary with no dwell.
# Emit a no-op at the post-pause time to hold the closing frame on screen.
emit("")

# ── Write cast ───────────────────────────────────────────────────────────────
header = {
    "version": 2,
    "width": WIDTH,
    "height": HEIGHT,
    "timestamp": 0,
    "env": {"TERM": "xterm-256color", "SHELL": "/bin/zsh"},
    "title": "GitHub Actions Gateway — local kind demo",
}
with open("demo-local-kind.cast", "w") as f:
    f.write(json.dumps(header) + "\n")
    for ev in events:
        f.write(json.dumps(ev, ensure_ascii=False) + "\n")
print(f"wrote {len(events)} events, duration {events[-1][0]:.1f}s")
