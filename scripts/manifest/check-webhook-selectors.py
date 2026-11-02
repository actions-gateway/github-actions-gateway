#!/usr/bin/env python3
"""check-webhook-selectors.py — every query selecting the GMC's admission
webhooks by name must name exactly the webhooks the chart ships (Q1267).

The apiserver's apiserver_admission_webhook_rejection_count carries no group or
resource label, only the webhook's `name`, and `.kb.io` is kubebuilder's default
suffix for every operator's webhooks. So the ActionsGatewayWebhookRejections
alert and the security dashboard's denied series select ours with an explicit
alternation, `name=~"v(actionsgateway-v2|...)\\.kb\\.io"`, that nothing tied to
the webhooks themselves. Q1150 renamed the five actions-gateway.com webhooks
from -v2alpha1 to -v2, and both queries kept the old names through v1.9.0 and
v1.10.0: a v2 webhook's denials matched nothing, so the alert could not fire
and the panel read as no webhook ever refusing.

This gate reads the webhook names from controller-gen's output and holds every
selector to them as a set, in both directions:

  cmd/gmc/config/webhook/manifests.yaml   the names (the chart's webhook.yaml
                                          is generated from it and gated by
                                          chart-webhook-check)
  deploy/monitoring/**, docs/operations/*.md
                                          every name=~ selector on the rejection
                                          counter, wherever it is written
  deploy/monitoring/preview/exporter.py   the synthetic exporter's WEBHOOKS,
                                          which the screenshot's denied series
                                          is drawn from

Every `name` matcher on the counter is judged, not only `=~`. A `=~` selector is
read as `v(<alt>|<alt>...)\\.kb\\.io` and must name the shipped set exactly; a
`=` selector must name one shipped webhook; `!=`, `!~` and any other `=~` shape
are refused rather than judged, so a rewrite cannot pass by being unparseable.

Exit: 0 every selector names the shipped set, 1 one does not, 2 a read that
could not be taken — an empty extraction is a parser that stopped matching.
"""

import json
import re
import sys
from pathlib import Path

WEBHOOKS = Path("cmd/gmc/config/webhook/manifests.yaml")
SITE_GLOBS = ("deploy/monitoring/**/*.json", "deploy/monitoring/**/*.yaml",
              "docs/operations/*.md")
EXPORTER = Path("deploy/monitoring/preview/exporter.py")

METRIC = "apiserver_admission_webhook_rejection_count"
# The name matcher inside the metric's braces: its operator, and its value as a
# PromQL string literal.
SELECTOR_RE = re.compile(METRIC + r'\{[^}]*?\bname\s*(=~|!~|!=|=)\s*"((?:[^"\\]|\\.)*)"')
SHAPE_RE = re.compile(r"v\(([a-z0-9|-]+)\)\\\.kb\\\.io")
EXPORTER_NAME_RE = re.compile(r'"(v[a-z0-9-]+\.kb\.io)"')


class Refusal(Exception):
    """A read that could not be taken. Never a verdict on the selectors."""


def shipped_names():
    names = set()
    for block in re.split(r"^- ", WEBHOOKS.read_text(), flags=re.M)[1:]:
        m = re.search(r"^  name: (\S+)$", block, re.M)
        if m:
            names.add(m.group(1))
    if not names:
        raise Refusal(f"{WEBHOOKS}: no webhook names extracted")
    return names


def strings(path):
    """Yield the text of a site that a PromQL expression can sit in."""
    text = path.read_text()
    if path.suffix == ".json":
        def walk(node):
            if isinstance(node, dict):
                for v in node.values():
                    yield from walk(v)
            elif isinstance(node, list):
                for v in node:
                    yield from walk(v)
            elif isinstance(node, str):
                yield node
        yield from walk(json.loads(text))
    elif path.suffix == ".md":
        # A pipe inside a table cell is written \|.
        yield text.replace("\\|", "|")
    else:
        yield text


def selectors():
    """Yield (path, operator, decoded value) for every name matcher on the counter."""
    for pattern in SITE_GLOBS:
        for path in sorted(Path(".").glob(pattern)):
            for s in strings(path):
                for m in SELECTOR_RE.finditer(s):
                    yield path, m.group(1), re.sub(r"\\(.)", r"\1", m.group(2))


def judge(where, named, shipped, problems):
    for name in sorted(shipped - named):
        problems.append(f"{where}: does not match {name}, which the chart ships")
    for name in sorted(named - shipped):
        problems.append(f"{where}: names {name}, which the chart does not ship")


def main():
    shipped = shipped_names()
    problems, checked = [], 0
    for path, op, value in selectors():
        checked += 1
        if op == "=":
            if value not in shipped:
                problems.append(f"{path}: names {value}, which the chart does not ship")
            continue
        shape = SHAPE_RE.fullmatch(value) if op == "=~" else None
        if not shape:
            raise Refusal(f"{path}: selector name{op}{value!r} is not name=~\"v(<alt>|...)\\.kb\\.io\" or name=\"<webhook>\"")
        judge(path, {f"v{alt}.kb.io" for alt in shape.group(1).split("|")}, shipped, problems)
    if not checked:
        raise Refusal(f"no {METRIC} name selector under {', '.join(SITE_GLOBS)}")

    exported = set(EXPORTER_NAME_RE.findall(EXPORTER.read_text()))
    if not exported:
        raise Refusal(f"{EXPORTER}: no webhook names extracted")
    judge(EXPORTER, exported, shipped, problems)

    if problems:
        print("webhook-name selectors disagree with the webhooks the chart ships:", file=sys.stderr)
        for problem in problems:
            print(f"  - {problem}", file=sys.stderr)
        print(
            "\nA name the selector misses is a webhook whose denials the alert and the "
            "dashboard cannot see.\nEdit each selector to the names in "
            f"{WEBHOOKS}, and the preview exporter's WEBHOOKS to match.",
            file=sys.stderr,
        )
        return 1

    print(f"webhook-name selectors match the shipped webhooks: "
          f"{checked} selectors and the preview exporter over {len(shipped)} webhooks")
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Refusal as err:
        print(f"check-webhook-selectors: cannot read: {err}", file=sys.stderr)
        sys.exit(2)
