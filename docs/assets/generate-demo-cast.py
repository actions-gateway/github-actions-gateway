#!/usr/bin/env python3
"""Hand-author an asciinema v2 cast for the GAG local-kind demo (Q193).

Every command and every line of output below is transcribed from a REAL run
captured under tmp/q193-demo/cap/ (kind cluster + real GitHub, 2026-07-03).
Timing is authored here for pacing; content is not invented.
"""
import json

WIDTH, HEIGHT = 92, 30
PROMPT = "[1;32m➜[0m [1;36mgag-demo[0m [1;34m$[0m "
DIM = "[38;5;245m"
RST = "[0m"
BOLD = "[1m"
GRN = "[32m"
YEL = "[33m"
CYN = "[36m"

events = []
t = 0.0

def emit(s):
    global t
    events.append([round(t, 3), "o", s])

def wait(dt):
    global t
    t += dt

def comment(lines, pause=1.0):
    for ln in lines:
        emit(f"{DIM}{ln}{RST}\r\n")
        wait(0.35)
    wait(pause)

def command(cmd, type_speed=0.028, pre=0.4, post=0.6):
    """Render the prompt, reveal the command, then Enter.

    The command is revealed whole (one keyframe) rather than char-by-char: a
    typing animation multiplies SVG text nodes ~one-per-char and bloats the
    asset several-fold for little gain on a docs page.
    """
    emit(PROMPT + cmd)
    wait(max(pre, 0.5))
    emit("\r\n")
    wait(post)

def output(text, chunk_pause=0.12, post=0.8):
    for ln in text.split("\n"):
        emit(ln + "\r\n")
        wait(chunk_pause)
    wait(post)

# ── Intro ──────────────────────────────────────────────────────────────────
comment([
    "# GitHub Actions Gateway — end-to-end demo on a local kind cluster",
    "# One GitHub job  →  one ephemeral worker pod  →  green on real GitHub.",
    "# Every line below is from a real run (kind + real GitHub, no fakes).",
], pause=1.4)

# ── 1. Cluster + platform ────────────────────────────────────────────────────
comment(["# 1) Stand up a kind cluster and install the platform (GMC)."], pause=0.8)

command("make e2e-cluster")
output(
"""Creating cluster \"actions-gateway-e2e\" ...
 ✓ Preparing nodes 📦 📦 📦
 ✓ Starting control-plane 🕹️
 ✓ Installing CNI 🔌
 ✓ Joining worker nodes 🚜
Set kubectl context to \"kind-actions-gateway-e2e\"""")

command("kubectl get nodes")
output(
"""NAME                                STATUS   ROLES           AGE   VERSION
actions-gateway-e2e-control-plane   Ready    control-plane   3m    v1.35.0
actions-gateway-e2e-worker          Ready    <none>          3m    v1.35.0
actions-gateway-e2e-worker2         Ready    <none>          3m    v1.35.0""")

command("make e2e-images   " + DIM + "# build gmc/agc/proxy/worker → local registry" + RST, post=0.4)
output("""#62 pushing manifest for 127.0.0.1:5000/gmc:e2e ... done
 ✓ gmc  ✓ agc  ✓ proxy  ✓ worker  ✓ wrapper  →  127.0.0.1:5000""", post=0.5)

command("helm upgrade --install actions-gateway charts/actions-gateway \\\n"
        "    -n gmc-system --create-namespace --set allowFloatingImageTags=true ...")
output(
"""Release \"actions-gateway\" does not exist. Installing it now.
NAME: actions-gateway
STATUS: deployed
NOTES: actions-gateway (Gateway Manager Controller) installed into \"gmc-system\".""")

command("kubectl get pods -n gmc-system")
output(
"""NAME                                      READY   STATUS    RESTARTS   AGE
gmc-controller-manager-675d44bd57-9phn4   1/1     Running   0          70s
gmc-controller-manager-675d44bd57-gmp8f   1/1     Running   0          63s""")

# ── 2. Onboard a tenant ──────────────────────────────────────────────────────
comment(["", "# 2) Onboard a tenant: namespace + quota, GitHub App secret, one CR."], pause=0.8)

command("kubectl create namespace team-a")
output("namespace/team-a created", post=0.3)
command("kubectl label ns team-a actions-gateway.github.com/tenant=true")
output("namespace/team-a labeled", post=0.3)

command("kubectl apply -f tenant-quota.yaml   " + DIM + "# ResourceQuota + LimitRange" + RST, post=0.3)
output(
"""resourcequota/team-a-quota created
limitrange/team-a-defaults created""", post=0.4)

command("kubectl create secret generic team-a-github-app -n team-a \\\n"
        "    --from-literal=appId=\"$APP_ID\" --from-literal=installationId=\"$INSTALL_ID\" \\\n"
        "    --from-file=privateKey=app.pem")
output("secret/team-a-github-app created", post=0.4)

comment(["  # ActionsGateway CR: 1 runner group, labels [e2e], completedPodTTL: 0s"], pause=0.4)
command("kubectl apply -f actionsgateway.yaml")
output("actionsgateway.actions-gateway.github.com/team-a-gateway created", post=0.6)

# ── 3. The GMC provisions everything; runners register with GitHub ───────────
comment(["", "# 3) The GMC provisions the AGC + egress proxy and registers runners."], pause=0.8)

command("kubectl get actionsgateway,runnergroup -n team-a")
output(
"""NAME                                                    PROXYREADY   READY   AGE
actionsgateway.../team-a-gateway                        1            True    2m

NAME                                            MAXLISTENERS  ACTIVESESSIONS  ACTIVEJOBS  READY
runnergroup.../team-a-gateway-e2e-6d8749c       2             2               0           True""")

command("gh api repos/$ORG/$REPO/actions/runners --jq '.runners[] | ...'")
output(
f"""team-a-gateway-e2e-6d8749c-0   {GRN}online{RST}   idle   e2e
team-a-gateway-e2e-6d8749c-1   {GRN}online{RST}   idle   e2e""", post=0.6)

comment(["  # Idle: the controller + proxy are up, but ZERO worker pods exist yet."], pause=0.5)
command("kubectl get pods -n team-a")
output(
"""NAME                                          READY   STATUS    RESTARTS   AGE
actions-gateway-controller-79ccdddbcc-9phzv   1/1     Running   0          2m
actions-gateway-proxy-5cfbb9584d-sjwjj        1/1     Running   0          2m""", post=1.0)

# ── 4. Fire a job — watch the ephemeral pod appear and vanish ────────────────
comment(["", "# 4) Trigger one workflow job on real GitHub and watch the pod lifecycle."], pause=0.8)

command("gh workflow run test-job.yml --repo $ORG/$REPO --ref main")
output("✓ Created workflow_dispatch event for test-job.yml at main", post=0.5)

command("kubectl get pods -n team-a -w", post=0.5)
# The worker pod materialises, runs, then is reaped (completedPodTTL: 0s).
emit("NAME                                                    READY   STATUS    AGE\r\n"); wait(0.3)
emit("actions-gateway-controller-79ccdddbcc-9phzv             1/1     Running   2m\r\n"); wait(0.2)
emit("actions-gateway-proxy-5cfbb9584d-sjwjj                  1/1     Running   2m\r\n"); wait(1.2)
emit(f"runner-...-6d8749c-b0587c8-47200999   0/1     {YEL}Pending{RST}             0s\r\n"); wait(1.4)
emit(f"runner-...-6d8749c-b0587c8-47200999   0/1     {YEL}ContainerCreating{RST}   1s\r\n"); wait(1.6)
emit(f"runner-...-6d8749c-b0587c8-47200999   1/1     {GRN}Running{RST}             3s   {DIM}← job executing{RST}\r\n"); wait(2.2)
emit(f"runner-...-6d8749c-b0587c8-47200999   0/1     {CYN}Completed{RST}           9s\r\n"); wait(1.2)
emit(f"runner-...-6d8749c-b0587c8-47200999   0/1     Terminating         9s   {DIM}← reaped on completion{RST}\r\n"); wait(1.4)
emit("[38;5;245m^C[0m\r\n"); wait(0.8)

# ── 5. Confirm success on GitHub, and the pod is gone ────────────────────────
comment(["", "# 5) The job is green on GitHub, and compute is back to zero."], pause=0.8)

command("gh run view 28687049624 --repo $ORG/$REPO")
output(
f"""{GRN}✓{RST} main Test self-hosted runner · 28687049624
Triggered via workflow_dispatch less than a minute ago

JOBS
{GRN}✓{RST} test in 8s (ID 85081558995)""", post=0.8)

command("kubectl get pods -n team-a")
output(
"""NAME                                          READY   STATUS    RESTARTS   AGE
actions-gateway-controller-79ccdddbcc-9phzv   1/1     Running   0          3m
actions-gateway-proxy-5cfbb9584d-sjwjj        1/1     Running   0          3m""", post=0.6)

comment([
    "  # The worker pod is gone — no idle compute between jobs.",
    "",
    f"{GRN}# job → ephemeral pod → green on real GitHub → scaled back to zero.{RST}",
], pause=2.0)

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
