#!/usr/bin/env python3
"""Hand-author an asciinema v2 cast for the GAG local-kind demo (Q193).

Every command and every line of output below is transcribed from a REAL run
captured under tmp/q193-demo/cap/ (kind cluster + real GitHub, 2026-07-03).
Timing is authored here for pacing; content is not invented.

Audience: platform/DevOps engineers *and* the managers/directors who sign off.
So each step is preceded by a plain-English narration line (what is happening and
why it matters), and the pacing is deliberately slow enough to read.
"""
import json

WIDTH, HEIGHT = 100, 32
E = "\x1b"
PROMPT = f"{E}[1;32m➜{E}[0m {E}[1;36mgag-demo{E}[0m {E}[1;34m${E}[0m "
DIM = f"{E}[38;5;245m"      # incidental inline comments
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

def say(lines, pause=1.7):
    """Plain-English narration. Rendered as bright-cyan `#` comment lines and
    given generous read time — this is the story a non-expert follows."""
    for ln in lines:
        emit(f"{SAY}{ln}{RST}\r\n")
        wait(0.42)
    wait(pause)

def blank(pause=0.4):
    emit("\r\n")
    wait(pause)

def command(cmd, pre=0.5, post=0.75):
    """Render the prompt, reveal the whole command (one keyframe), then Enter.

    Revealed whole rather than typed char-by-char: a typing animation multiplies
    SVG text nodes ~one-per-char and bloats the asset for little gain on a docs
    page. Continuation lines in a multi-line command use "\\\n" in the source;
    convert the bare LF to CRLF so the terminal returns the cursor to column 0 (a
    bare LF only moves down a row, leaving the continuation indented to the prompt
    width)."""
    emit(PROMPT + cmd.replace("\n", "\r\n"))
    wait(pre)
    emit("\r\n")
    wait(post)

def output(text, chunk_pause=0.12, post=1.0):
    for ln in text.split("\n"):
        emit(ln + "\r\n")
        wait(chunk_pause)
    wait(post)

def watch_row(name, ready, status, age, annot="", color=""):
    """Format one `kubectl get pods -w` row with fixed visible-width columns so
    ANSI colour codes (zero display width) don't break alignment, and nothing
    wraps at WIDTH columns."""
    seg = (f"{color}{status}{RST}" if color else status) + " " * (18 - len(status))
    line = f"{name:<45}{ready:<8}{seg}{age:<5}"
    if annot:
        line += f"{DIM}{annot}{RST}"
    return line

RULE = "─" * 74

# ── Title card ───────────────────────────────────────────────────────────────
say([
    f"# {RULE}",
    "#  GitHub Actions Gateway (GAG)",
    "#  Self-hosted GitHub Actions runners on Kubernetes — with zero idle compute.",
    f"# {RULE}",
], pause=2.2)
say([
    "#  In this short demo, on a cluster running on a laptop:",
    "#    a single real GitHub job runs in a pod that exists ONLY while the job runs,",
    "#    then disappears. No runner sits idle waiting. That is the whole idea.",
], pause=2.4)
blank()

# ── STEP 1 — cluster + platform ──────────────────────────────────────────────
say([
    "#  STEP 1 — Create a throwaway Kubernetes cluster and install GAG.",
    "#  (kind runs a real cluster locally; GAG installs as a single controller.)",
])
command("make e2e-cluster")
output(
"""Creating cluster \"actions-gateway-e2e\" ...
 ✓ Preparing nodes 📦 📦 📦
 ✓ Starting control-plane 🕹️
 ✓ Installing CNI 🔌
 ✓ Joining worker nodes 🚜
Set kubectl context to \"kind-actions-gateway-e2e\"""")

say(["#  The cluster is up — one control-plane node and two workers."], pause=1.6)
command("kubectl get nodes")
output(
"""NAME                                STATUS   ROLES           AGE   VERSION
actions-gateway-e2e-control-plane   Ready    control-plane   3m    v1.35.0
actions-gateway-e2e-worker          Ready    <none>          3m    v1.35.0
actions-gateway-e2e-worker2         Ready    <none>          3m    v1.35.0""")

say([
    "#  Now install the platform itself — the Gateway Manager Controller —",
    "#  with one Helm command. This is the only thing installed cluster-wide.",
])
command("make e2e-images   " + DIM + "# build the gmc/agc/proxy/worker images" + RST, post=0.6)
output("""#62 pushing manifest for 127.0.0.1:5000/gmc:e2e ... done
 ✓ gmc  ✓ agc  ✓ proxy  ✓ worker  ✓ wrapper  →  127.0.0.1:5000""", post=1.0)
command("helm upgrade --install actions-gateway charts/actions-gateway \\\n"
        "    -n gmc-system --create-namespace ...")
output(
"""Release \"actions-gateway\" does not exist. Installing it now.
NAME: actions-gateway
STATUS: deployed""")

say(["#  One controller now manages runners for every team on this cluster."], pause=1.6)
command("kubectl get pods -n gmc-system")
output(
"""NAME                                      READY   STATUS    RESTARTS   AGE
gmc-controller-manager-675d44bd57-9phn4   1/1     Running   0          70s
gmc-controller-manager-675d44bd57-gmp8f   1/1     Running   0          63s""")
blank()

# ── STEP 2 — onboard a tenant ────────────────────────────────────────────────
say([
    "#  STEP 2 — Onboard a team, \"team-a\".",
    "#  A whole team's runner setup is a namespace, a budget, a secret, and ONE resource.",
])
command("kubectl create namespace team-a")
output("namespace/team-a created", post=0.5)
command("kubectl label ns team-a actions-gateway.github.com/tenant=true")
output("namespace/team-a labeled", post=0.8)

say(["#  Give the team a compute budget the platform owns — the team can't exceed it."], pause=1.6)
command("kubectl apply -f tenant-quota.yaml   " + DIM + "# ResourceQuota + LimitRange" + RST, post=0.5)
output(
"""resourcequota/team-a-quota created
limitrange/team-a-defaults created""", post=1.0)

say(["#  Add the team's GitHub App credentials as a Secret (read from a file, never inline)."], pause=1.6)
command("kubectl create secret generic team-a-github-app -n team-a \\\n"
        "    --from-literal=appId=\"$APP_ID\" --from-literal=installationId=\"$INSTALL_ID\" \\\n"
        "    --from-file=privateKey=app.pem")
output("secret/team-a-github-app created", post=1.0)

say([
    "#  Finally, ONE resource describes the team's runners: labels, size, and",
    "#  \"completedPodTTL: 0s\" — delete each job's pod the instant it finishes.",
])
command("kubectl apply -f actionsgateway.yaml")
output("actionsgateway.actions-gateway.github.com/team-a-gateway created", post=1.2)
blank()

# ── STEP 3 — GAG provisions; runners register; nothing idle ──────────────────
say([
    "#  GAG reacts automatically: it stands up the team's runner controller and",
    "#  egress proxy, and registers self-hosted runners with GitHub.",
])
command("kubectl get actionsgateway,runnergroup -n team-a")
output(
f"""NAME                                                    PROXYREADY   READY   AGE
actionsgateway.../team-a-gateway                        1            {GRN}True{RST}    2m

NAME                                            MAXLISTENERS  ACTIVESESSIONS  ACTIVEJOBS  READY
runnergroup.../team-a-gateway-e2e-6d8749c       2             2               0           {GRN}True{RST}""")

say(["#  GitHub now sees the team's runners as online and idle:"], pause=1.4)
command("gh api repos/$ORG/$REPO/actions/runners --jq '.runners[] | ...'")
output(
f"""team-a-gateway-e2e-6d8749c-0   {GRN}online{RST}   idle   e2e
team-a-gateway-e2e-6d8749c-1   {GRN}online{RST}   idle   e2e""", post=1.4)

say([
    "#  The important part — look at the team's pods right now:",
    f"#  the controller and proxy are running, but there are {SAY}ZERO job pods.{RST}",
    "#  An idle team costs nothing: no reserved runners, no idle GPUs.",
])
command("kubectl get pods -n team-a")
output(
"""NAME                                          READY   STATUS    RESTARTS   AGE
actions-gateway-controller-79ccdddbcc-9phzv   1/1     Running   0          2m
actions-gateway-proxy-5cfbb9584d-sjwjj        1/1     Running   0          2m""", post=2.4)
blank()

# ── STEP 4 — run a job: the pod appears, runs, and is reaped ──────────────────
say([
    "#  STEP 3 — Trigger ONE real GitHub Actions job and watch what happens.",
])
command("gh workflow run test-job.yml --repo $ORG/$REPO --ref main")
output(f"{GRN}✓{RST} Created workflow_dispatch event for test-job.yml at main", post=1.0)

say([
    "#  GAG picks up the job and creates a pod JUST for it. We'll watch the pods live:",
])
command("kubectl get pods -n team-a -w", post=0.7)
RUNNER = "runner-…-6d8749c-b0587c8-47200999"
emit(watch_row("NAME", "READY", "STATUS", "AGE") + "\r\n"); wait(0.4)
emit(watch_row("actions-gateway-controller-79ccdddbcc-9phzv", "1/1", "Running", "2m") + "\r\n"); wait(0.3)
emit(watch_row("actions-gateway-proxy-5cfbb9584d-sjwjj", "1/1", "Running", "2m") + "\r\n"); wait(1.6)
emit(watch_row(RUNNER, "0/1", "Pending", "0s", "← new pod for the job", YEL) + "\r\n"); wait(2.0)
emit(watch_row(RUNNER, "0/1", "ContainerCreating", "1s", color=YEL) + "\r\n"); wait(2.0)
emit(watch_row(RUNNER, "1/1", "Running", "3s", "← the job is executing", GRN) + "\r\n"); wait(2.6)
emit(watch_row(RUNNER, "0/1", "Completed", "9s", "← job finished", CYN) + "\r\n"); wait(2.0)
emit(watch_row(RUNNER, "0/1", "Terminating", "9s", "← pod deleted instantly") + "\r\n"); wait(2.2)
emit(f"{DIM}^C{RST}\r\n"); wait(0.9)

say([
    "#  That is the whole pitch: one job → one short-lived pod → gone.",
    "#  You pay for compute only during the ~8 seconds the job actually ran.",
], pause=2.4)
blank()

# ── STEP 5 — confirm green on GitHub, and back to zero ───────────────────────
say(["#  Meanwhile, on GitHub, the job is green:"], pause=1.4)
command("gh run view 28687049624 --repo $ORG/$REPO")
output(
f"""{GRN}✓{RST} main Test self-hosted runner · 28687049624
Triggered via workflow_dispatch less than a minute ago

JOBS
{GRN}✓{RST} test in 8s (ID 85081558995)""", post=1.8)

say(["#  And the team's namespace is back to just the controller and proxy — no job pods."], pause=1.6)
command("kubectl get pods -n team-a")
output(
"""NAME                                          READY   STATUS    RESTARTS   AGE
actions-gateway-controller-79ccdddbcc-9phzv   1/1     Running   0          3m
actions-gateway-proxy-5cfbb9584d-sjwjj        1/1     Running   0          3m""", post=2.0)
blank()

# ── Closing card ─────────────────────────────────────────────────────────────
say([
    f"# {RULE}",
    "#  One job  →  one short-lived pod  →  green on GitHub  →  back to zero.",
    "#  No idle runners. No idle GPUs. Runners scale to zero by default.",
    f"# {RULE}",
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
