#!/usr/bin/env python3
"""Hand-author an asciinema v2 cast for the GAG local-kind demo (Q193).

Every command and every line of output below is transcribed from a REAL run
captured under tmp/q193-demo/cap/ (kind cluster + real GitHub, 2026-07-03).
Timing is authored here for pacing; content is not invented.

This is a *value-first* recording, not a tutorial: it assumes GAG is already
installed and shows only the payoff — a real job runs in a pod that exists only
while the job runs, then disappears. The full install/onboard steps live as text
on the demo page ("Reproduce it yourself"), which is where a tutorial belongs.
"""
import json

WIDTH, HEIGHT = 100, 26
E = "\x1b"
PROMPT = f"{E}[1;32m➜{E}[0m {E}[1;36mgag-demo{E}[0m {E}[1;34m${E}[0m "
DIM = f"{E}[38;5;245m"      # incidental inline comments / annotations
SAY = f"{E}[1;38;5;45m"     # narration — the plain-English "voiceover"
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

def say(lines, pause=1.5):
    """Plain-English narration in bright cyan `#` lines, with read time."""
    for ln in lines:
        emit(f"{SAY}{ln}{RST}\r\n")
        wait(0.4)
    wait(pause)

def blank(pause=0.3):
    emit("\r\n")
    wait(pause)

def command(cmd, pre=0.5, post=0.7):
    """Reveal the prompt + whole command, then Enter. Continuation lines use
    "\\\n" in the source; convert the bare LF to CRLF so a wrapped command
    returns the cursor to column 0 instead of indenting to the prompt width."""
    emit(PROMPT + cmd.replace("\n", "\r\n"))
    wait(pre)
    emit("\r\n")
    wait(post)

def output(text, chunk_pause=0.1, post=0.9):
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

# ── Hook: what this is, in one line ──────────────────────────────────────────
say([
    "#  GitHub Actions Gateway — self-hosted CI runners on Kubernetes,",
    "#  with zero idle compute. Here's the whole idea in 30 seconds:",
], pause=2.2)

# ── Beat 1: idle — no runner pods exist ──────────────────────────────────────
say(["#  GAG is installed and idle. How many runner pods are running? None:"])
command("kubectl get pods -n team-a")
output(
"""NAME                                          READY   STATUS    RESTARTS   AGE
actions-gateway-controller-79ccdddbcc-9phzv   1/1     Running   0          9m
actions-gateway-proxy-5cfbb9584d-sjwjj        1/1     Running   0          9m""")
say([
    "#  Just a lightweight controller and proxy. No runner sits idle —",
    "#  an idle team costs nothing: no reserved pods, no idle GPUs.",
], pause=2.0)
blank()

# ── Beat 2: one job → a pod appears, runs, and is deleted ────────────────────
say(["#  Now trigger ONE real GitHub Actions job:"])
command("gh workflow run test-job.yml --repo $ORG/$REPO --ref main")
output(f"{DIM}https://github.com/$ORG/$REPO/actions/runs/28687049624{RST}", post=0.8)

say(["#  GAG spins up a pod JUST for that job — watch it appear, run, and vanish:"])
command("kubectl get pods -n team-a -w", post=0.7)
RUNNER = "runner-…-6d8749c-b0587c8-47200999"
emit(watch_row("NAME", "READY", "STATUS", "AGE") + "\r\n"); wait(0.35)
emit(watch_row("actions-gateway-controller-79ccdddbcc-9phzv", "1/1", "Running", "9m") + "\r\n"); wait(0.25)
emit(watch_row("actions-gateway-proxy-5cfbb9584d-sjwjj", "1/1", "Running", "9m") + "\r\n"); wait(1.4)
emit(watch_row(RUNNER, "0/1", "Pending", "0s", "← new pod for the job", YEL) + "\r\n"); wait(1.6)
emit(watch_row(RUNNER, "0/1", "ContainerCreating", "1s", color=YEL) + "\r\n"); wait(1.6)
emit(watch_row(RUNNER, "1/1", "Running", "3s", "← the job is executing", GRN) + "\r\n"); wait(2.2)
emit(watch_row(RUNNER, "0/1", "Completed", "9s", "← job finished", CYN) + "\r\n"); wait(1.6)
emit(watch_row(RUNNER, "0/1", "Terminating", "9s", "← pod deleted instantly") + "\r\n"); wait(1.8)
emit(f"{DIM}^C{RST}\r\n"); wait(0.9)
blank()

# ── Beat 3: green on GitHub, back to zero ────────────────────────────────────
say(["#  The job is green on GitHub — it really ran (~8 seconds):"])
command("gh run view 28687049624 --repo $ORG/$REPO")
output(
f"""{GRN}✓{RST} main Test self-hosted runner · 28687049624
Triggered via workflow_dispatch less than a minute ago

JOBS
{GRN}✓{RST} test in 8s (ID 85081558995)""", post=1.4)

say([
    "#  One job → one short-lived pod → green → back to zero.",
    "#  You pay for compute only while a job runs. That's GAG.",
], pause=3.0)

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
