#!/usr/bin/env python3
"""check-registry-mirror-wiring.py — the mirror set and the job's wiring to it
must name the same endpoints (Q408 Phase 3).

Three files hold one fact between them, and none of them can see the others:

  deploy/registry-mirror/base/deployment.yaml   the instances, each with the one
                                                upstream its proxy mode fetches
  deploy/registry-mirror/base/service.yaml      the ClusterIPs clients address
  deploy/dogfood-e2e/overlays/kata/mirror-wiring.yaml
                                                the tenant ConfigMap, holding
                                                dockerd's daemon.json and the
                                                <upstream>=<mirror> map the ref
                                                rewriting reads

A sixth upstream added to the first without the third leaves that upstream's
pulls going direct. Under the Phase 3 posture — open egress still in place —
that is GREEN: the run passes, the mirror is simply unused, and the only reading
that would have caught it is scripts/dogfood/e2e-mirror-hits.sh on a booked
dogfood session. Phase 4 turns the same drift into a failed e2e run on another
booked session. Both are expensive ways to learn about a typo, hence a gate.

The upstream a mirror serves is read from REGISTRY_PROXY_REMOTEURL rather than
inferred from the instance name, so `ghcr.io=mirror-quay-io...` fails here
rather than at pull time. Docker Hub is the one instance whose remote URL is not
its upstream host (registry-1.docker.io is the v2 API host), and that is spelt
out below rather than pattern-matched.

Exit: 0 the three agree, 1 they do not, 2 a read that could not be taken — an
empty extraction is a parser that stopped matching, and grading a wiring green
off one is the failure this gate would otherwise become.
"""

import json
import re
import sys
from pathlib import Path

MIRROR_DEPLOYMENTS = Path("deploy/registry-mirror/base/deployment.yaml")
MIRROR_SERVICES = Path("deploy/registry-mirror/base/service.yaml")
WIRING = Path("deploy/dogfood-e2e/overlays/kata/mirror-wiring.yaml")

PORT = "5000"
CLUSTER_DOMAIN = "svc.cluster.local"

# The one instance whose proxy.remoteurl host is not its upstream: Docker Hub's
# v2 API is served from registry-1.docker.io, while every ref names docker.io.
REMOTE_HOST_ALIASES = {"registry-1.docker.io": "docker.io"}

NAME_RE = re.compile(r"^  name: (\S+)$", re.M)
NAMESPACE_RE = re.compile(r"^  namespace: (\S+)$", re.M)
KIND_RE = re.compile(r"^kind: (\S+)$", re.M)
REMOTEURL_RE = re.compile(r"^\s+- name: REGISTRY_PROXY_REMOTEURL\n\s+value: (\S+)$", re.M)


class Refusal(Exception):
    """A read that could not be taken. Never a verdict on the wiring."""


def documents(path):
    """Yield the YAML documents of path as raw text, comments and all."""
    try:
        text = path.read_text()
    except OSError as exc:
        raise Refusal(f"cannot read {path}: {exc}") from exc
    return [d for d in re.split(r"^---$", text, flags=re.M) if d.strip()]


def one(matches, what, path):
    if len(matches) != 1:
        raise Refusal(f"{path}: expected exactly one {what} per document, found {len(matches)}")
    return matches[0]


def read_instances():
    """Map instance name -> (namespace, upstream host), from the Deployments."""
    instances = {}
    for doc in documents(MIRROR_DEPLOYMENTS):
        kinds = KIND_RE.findall(doc)
        if "Deployment" not in kinds:
            continue
        name = one(NAME_RE.findall(doc), "metadata.name", MIRROR_DEPLOYMENTS)
        namespace = one(NAMESPACE_RE.findall(doc), "metadata.namespace", MIRROR_DEPLOYMENTS)
        url = one(REMOTEURL_RE.findall(doc), "REGISTRY_PROXY_REMOTEURL", MIRROR_DEPLOYMENTS)
        host = url.split("://", 1)[-1].rstrip("/")
        instances[name] = (namespace, REMOTE_HOST_ALIASES.get(host, host))
    if not instances:
        raise Refusal(f"{MIRROR_DEPLOYMENTS}: no mirror Deployments found")
    return instances


def read_services():
    names = set()
    for doc in documents(MIRROR_SERVICES):
        if "Service" not in KIND_RE.findall(doc):
            continue
        names.add(one(NAME_RE.findall(doc), "metadata.name", MIRROR_SERVICES))
    if not names:
        raise Refusal(f"{MIRROR_SERVICES}: no mirror Services found")
    return names


def read_block(text, key):
    """Return the body of a `  <key>: |`/`>-` block scalar, lines joined as the
    scalar's own style joins them: literal keeps newlines, folded uses spaces."""
    match = re.search(rf"^  {re.escape(key)}: ([|>][+-]?)\n", text, re.M)
    if not match:
        raise Refusal(f"{WIRING}: no block scalar named {key}")
    style = match.group(1)[0]
    lines = []
    for line in text[match.end():].splitlines():
        if line.strip() and not line.startswith("    "):
            break
        lines.append(line[4:])
    joiner = "\n" if style == "|" else " "
    body = joiner.join(line for line in lines if line.strip())
    if not body:
        raise Refusal(f"{WIRING}: block scalar {key} is empty")
    return body


def read_wiring():
    try:
        text = WIRING.read_text()
    except OSError as exc:
        raise Refusal(f"cannot read {WIRING}: {exc}") from exc

    pairs = {}
    for entry in read_block(text, "registry-mirrors").split():
        if "=" not in entry:
            raise Refusal(f"{WIRING}: registry-mirrors entry '{entry}' is not <upstream>=<endpoint>")
        upstream, endpoint = entry.split("=", 1)
        pairs[upstream] = endpoint

    try:
        daemon = json.loads(read_block(text, "daemon.json"))
    except json.JSONDecodeError as exc:
        raise Refusal(f"{WIRING}: daemon.json is not valid JSON: {exc}") from exc
    return pairs, daemon


def main():
    try:
        instances = read_instances()
        services = read_services()
        pairs, daemon = read_wiring()
    except Refusal as exc:
        print(f"REFUSED: {exc}", file=sys.stderr)
        return 2

    problems = []

    for name in sorted(set(instances) - services):
        problems.append(f"{name}: a Deployment with no Service of the same name")
    for name in sorted(services - set(instances)):
        problems.append(f"{name}: a Service with no Deployment of the same name")

    expected = {}
    for name, (namespace, upstream) in sorted(instances.items()):
        want_name = "mirror-" + upstream.replace(".", "-")
        if name != want_name:
            problems.append(
                f"{name}: proxies {upstream}, so it must be named {want_name} "
                "(the ConfigMap and the validation batteries derive one from the other)"
            )
        expected[upstream] = f"{name}.{namespace}.{CLUSTER_DOMAIN}:{PORT}"

    for upstream in sorted(set(expected) - set(pairs)):
        problems.append(
            f"{upstream}: served by a mirror instance but absent from the ConfigMap's "
            "registry-mirrors map, so the job pulls it direct"
        )
    for upstream in sorted(set(pairs) - set(expected)):
        problems.append(
            f"{upstream}: named in the ConfigMap's registry-mirrors map with no mirror "
            "instance behind it, so every pull of it fails"
        )
    for upstream in sorted(set(expected) & set(pairs)):
        if pairs[upstream] != expected[upstream]:
            problems.append(
                f"{upstream}: the map points at {pairs[upstream]}, "
                f"but its instance is at {expected[upstream]}"
            )

    endpoints = sorted(expected.values())
    insecure = sorted(daemon.get("insecure-registries", []))
    if insecure != endpoints:
        problems.append(
            "daemon.json insecure-registries must list every mirror endpoint "
            "(transport is plain HTTP, which dockerd refuses otherwise): "
            f"have {insecure}, want {endpoints}"
        )

    hub = expected.get("docker.io")
    want_mirrors = [f"http://{hub}"] if hub else []
    if daemon.get("registry-mirrors", []) != want_mirrors:
        problems.append(
            "daemon.json registry-mirrors must name the docker.io instance and only it "
            "(dockerd mirrors Hub alone; the other upstreams ride the ref rewrite): "
            f"have {daemon.get('registry-mirrors', [])}, want {want_mirrors}"
        )

    if problems:
        print("registry-mirror wiring is inconsistent:", file=sys.stderr)
        for problem in problems:
            print(f"  - {problem}", file=sys.stderr)
        print(
            "\nThe three files that must agree:\n"
            f"  {MIRROR_DEPLOYMENTS}\n  {MIRROR_SERVICES}\n  {WIRING}",
            file=sys.stderr,
        )
        return 1

    print(f"registry-mirror wiring is consistent over {len(expected)} upstreams: "
          + ", ".join(sorted(expected)))
    return 0


if __name__ == "__main__":
    sys.exit(main())
