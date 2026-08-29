#!/usr/bin/env python3
"""check-registry-mirror-catalog-deny.py — every mirror instance must front its
registry with the catalog deny, and one port must run through the whole path
(Q1022).

Distribution 3.1.1 serves GET /v2/_catalog unconditionally and offers no setting
that closes it while leaving anonymous pulls working, so each mirror pod runs a
`catalog-deny` proxy on the admitted port and binds the registry to loopback
behind it. That posture is spread over five files that cannot see each other:

  base/deployment.yaml     the five instances, each needing a deny container and
                           a registry bound to loopback
  base/catalog-deny.cfg    the deny rule itself, and the loopback address it
                           forwards to
  base/kustomization.yaml  the generator that turns the config into the ConfigMap
                           the Deployments mount
  base/service.yaml        the port clients address
  base/networkpolicy.yaml  the port the mirror admits, restated once more by
  components/shared-tenants/kustomization.yaml, whose patch replaces the base's
                           ingress wholesale

A sixth instance added to the first without a deny container serves its catalog
to every tenant that can reach it, and nothing else in this repository would
notice: no CI job renders these manifests (Q1024), and the cluster battery that
would catch it needs a booked dogfood session. A registry quietly returned to
0.0.0.0 is the same hole by a different route — the proxy is still there and no
longer the only way in.

Exit: 0 the posture is whole, 1 it is not, 2 a read that could not be taken. An
empty extraction is a parser that stopped matching, and grading this green off
one is the failure this gate would otherwise become.
"""

import re
import sys
from pathlib import Path

BASE = Path("deploy/registry-mirror/base")
DEPLOYMENTS = BASE / "deployment.yaml"
DENY_CONFIG = BASE / "catalog-deny.cfg"
KUSTOMIZATION = BASE / "kustomization.yaml"
SERVICES = BASE / "service.yaml"
POLICIES = BASE / "networkpolicy.yaml"
SHARED = Path("deploy/registry-mirror/components/shared-tenants/kustomization.yaml")

REGISTRY_CONTAINER = "registry"
DENY_CONTAINER = "catalog-deny"
CONFIGMAP = "mirror-catalog-deny"
CATALOG_PATH = "/v2/_catalog"

# The mirror-side ingress policy. networkpolicy.yaml also holds the worker-side
# egress rule, whose ports are its own business.
INGRESS_POLICY = "registry-mirror-worker-access"

# The deny container probes its own health here rather than at /v2/, which is
# proxied through: a registry fault would otherwise fail the healthy proxy's
# probe. The path lives in two files and must be one string.
PROBE_RE = re.compile(r"^\s+path: (\S+)$", re.M)
MONITOR_RE = re.compile(r"^\s*monitor-uri (\S+)$", re.M)

NAME_RE = re.compile(r"^  name: (\S+)$", re.M)
KIND_RE = re.compile(r"^kind: (\S+)$", re.M)
CONTAINER_RE = re.compile(r"^        - name: (\S+)$", re.M)
PORT_RE = re.compile(r"^            - containerPort: (\d+)$", re.M)
HTTP_ADDR_RE = re.compile(r"^\s+- name: REGISTRY_HTTP_ADDR\n\s+value: (\S+)$", re.M)
VOLUME_CM_RE = re.compile(r"^          configMap:\n            name: (\S+)$", re.M)
TARGET_PORT_RE = re.compile(r"^      targetPort: (\d+)$", re.M)
POLICY_PORT_RE = re.compile(r"^\s+- protocol: TCP\n\s+port: (\d+)$", re.M)


class Refusal(Exception):
    """A read that could not be taken. Never a verdict on the posture."""


def read(path):
    try:
        return path.read_text()
    except OSError as exc:
        raise Refusal(f"cannot read {path}: {exc}") from exc


def documents(path):
    return [d for d in re.split(r"^---$", read(path), flags=re.M) if d.strip()]


def containers(doc):
    """Map container name -> its body text, splitting the containers list."""
    start = doc.find("      containers:\n")
    if start < 0:
        raise Refusal(f"{DEPLOYMENTS}: a Deployment with no containers list")
    body = doc[start:]
    end = re.search(r"^      volumes:$", body, re.M)
    body = body[: end.start()] if end else body
    found = {}
    marks = list(CONTAINER_RE.finditer(body))
    for i, mark in enumerate(marks):
        stop = marks[i + 1].start() if i + 1 < len(marks) else len(body)
        found[mark.group(1)] = body[mark.start(): stop]
    return found


def read_instances():
    """Map instance -> (deny port, registry loopback address, ConfigMap names)."""
    instances = {}
    for doc in documents(DEPLOYMENTS):
        if "Deployment" not in KIND_RE.findall(doc):
            continue
        names = NAME_RE.findall(doc)
        if not names:
            raise Refusal(f"{DEPLOYMENTS}: a Deployment with no metadata.name")
        instances[names[0]] = (containers(doc), VOLUME_CM_RE.findall(doc))
    if not instances:
        raise Refusal(f"{DEPLOYMENTS}: no mirror Deployments found")
    return instances


def policy_document(path, name):
    """The one YAML document of path declaring a policy called name.

    networkpolicy.yaml holds two independent policies, and their ports coincide
    by design rather than by constraint: the worker-side egress rule may
    legitimately gain a port that has nothing to do with the mirror. Reading the
    whole file would turn that change into a refusal naming a port the mirror
    never admitted.
    """
    docs = [d for d in documents(path) if re.search(rf"^  name: {re.escape(name)}$", d, re.M)]
    if len(docs) != 1:
        raise Refusal(f"{path}: expected exactly one policy named {name}, found {len(docs)}")
    return docs[0]


def sole(values, what, path):
    """The one value a set of restatements agrees on, or a refusal."""
    if not values:
        raise Refusal(f"{path}: no {what} found")
    if len(set(values)) != 1:
        raise Refusal(f"{path}: {what} disagree: {sorted(set(values))}")
    return values[0]


def main():
    try:
        instances = read_instances()
        config = read(DENY_CONFIG)
        kustomization = read(KUSTOMIZATION)
        service_port = sole(TARGET_PORT_RE.findall(read(SERVICES)), "targetPort", SERVICES)
        base_port = sole(POLICY_PORT_RE.findall(policy_document(POLICIES, INGRESS_POLICY)),
                         "admitted port", POLICIES)
        shared_port = sole(POLICY_PORT_RE.findall(read(SHARED)), "admitted port", SHARED)
    except Refusal as exc:
        print(f"REFUSED: {exc}", file=sys.stderr)
        return 2

    problems = []

    # Directives only. This file argues for its own rules at length in comments,
    # and every needle below appears in that prose as well, so matching the whole
    # text would grade a config whose rule had been deleted and whose comment
    # about it survived.
    directives = "\n".join(
        line for line in config.splitlines() if line.strip() and not line.lstrip().startswith("#")
    )

    # Read once, before the per-instance loop: every deny container must probe
    # the path this config answers itself. Off `directives` for consistency with
    # every other needle below, NOT because this one needs it: unlike the bare
    # substring the url_dec rule uses, this pattern is anchored, so a `#` before
    # it already breaks the match and the raw text would read the same. Measured
    # over the shipped file and four comment forms, both reads agree.
    monitor_paths = MONITOR_RE.findall(directives)
    monitor_path = monitor_paths[0] if len(monitor_paths) == 1 else None

    if base_port != service_port:
        problems.append(
            f"the Services target {service_port} and {POLICIES} admits {base_port}"
        )
    if shared_port != base_port:
        problems.append(
            f"{SHARED} admits {shared_port}, but the base admits {base_port}; its patch "
            "replaces the base's ingress wholesale, so the two must restate one port"
        )
    admitted = base_port

    loopbacks = set()
    for name, (found, volume_cms) in sorted(instances.items()):
        if DENY_CONTAINER not in found:
            problems.append(
                f"{name}: no {DENY_CONTAINER} container, so its {CATALOG_PATH} "
                "names every repository it has cached to anything that can reach it"
            )
            continue
        if REGISTRY_CONTAINER not in found:
            problems.append(f"{name}: no {REGISTRY_CONTAINER} container")
            continue

        deny_probes = set(PROBE_RE.findall(found[DENY_CONTAINER]))
        if monitor_path is not None and deny_probes != {monitor_path}:
            problems.append(
                f"{name}: the {DENY_CONTAINER} container probes "
                f"{sorted(deny_probes) or 'nothing'}, but {DENY_CONFIG.name} answers "
                f"{monitor_path} itself. A probe on a proxied path fails whenever the "
                "registry is down, so a healthy proxy restarts on a registry fault"
            )

        deny_ports = PORT_RE.findall(found[DENY_CONTAINER])
        if deny_ports != [admitted]:
            problems.append(
                f"{name}: the {DENY_CONTAINER} container declares {deny_ports or 'no port'}, "
                f"but {admitted} is the port the Services target and the policies admit"
            )

        addrs = HTTP_ADDR_RE.findall(found[REGISTRY_CONTAINER])
        if len(addrs) != 1:
            problems.append(
                f"{name}: the {REGISTRY_CONTAINER} container sets REGISTRY_HTTP_ADDR "
                f"{len(addrs)} times; without exactly one it listens on 0.0.0.0:5000 "
                "and the deny is one hop a client can skip"
            )
            continue
        addr = addrs[0]
        host, _, port = addr.rpartition(":")
        if host not in ("127.0.0.1", "[::1]"):
            problems.append(
                f"{name}: the registry binds {addr}, which is on the pod network; "
                "only loopback keeps the deny container the sole way in"
            )
        if port == admitted:
            problems.append(
                f"{name}: the registry binds {addr}, the same port the deny container "
                f"declares ({admitted}); one of the two would fail to bind"
            )
        loopbacks.add(port)

        registry_ports = PORT_RE.findall(found[REGISTRY_CONTAINER])
        if registry_ports != [port]:
            problems.append(
                f"{name}: the {REGISTRY_CONTAINER} container declares "
                f"{registry_ports or 'no port'} but listens on {port}"
            )

        if volume_cms != [CONFIGMAP]:
            problems.append(
                f"{name}: mounts ConfigMap {volume_cms or 'none'}, want [{CONFIGMAP!r}] — "
                "a deny container with no config does not start, and the pod never goes Ready"
            )

    if f"haproxy.cfg={DENY_CONFIG.name}" not in kustomization:
        problems.append(
            f"{KUSTOMIZATION}: no generator turning {DENY_CONFIG.name} into a ConfigMap, "
            "so the Deployments mount a name nothing renders"
        )
    if f"name: {CONFIGMAP}" not in kustomization:
        problems.append(f"{KUSTOMIZATION}: the generated ConfigMap is not named {CONFIGMAP}")

    if CATALOG_PATH not in directives:
        problems.append(
            f"{DENY_CONFIG}: carries no {CATALOG_PATH} rule, so it forwards the catalog "
            "like any other path"
        )
    elif not re.search(rf"^\s*acl \S+ path,url_dec .*{re.escape(CATALOG_PATH)}",
                       directives, re.M):
        problems.append(
            f"{DENY_CONFIG}: matches the raw path. Go decodes percent escapes before the "
            f"route is matched, so {CATALOG_PATH} is reachable as /v2/%5Fcatalog "
            "(measured); the rule must read the path through url_dec"
        )
    if len(monitor_paths) != 1:
        problems.append(
            f"{DENY_CONFIG}: declares {len(monitor_paths)} monitor-uri paths, want exactly "
            "one for the deny container to probe itself on"
        )
    if not re.search(rf"^\s*bind :{admitted}$", directives, re.M):
        problems.append(f"{DENY_CONFIG}: does not bind {admitted}, the admitted port")
    for port in sorted(loopbacks):
        if not re.search(rf"^\s*server \S+ 127\.0\.0\.1:{port}$", directives, re.M):
            problems.append(
                f"{DENY_CONFIG}: forwards nowhere the registry listens; the registries "
                f"bind 127.0.0.1:{port}"
            )

    if problems:
        print("the registry-mirror catalog deny is incomplete:", file=sys.stderr)
        for problem in problems:
            print(f"  - {problem}", file=sys.stderr)
        return 1

    print(
        f"the catalog deny fronts all {len(instances)} mirror instances on {admitted}, "
        f"each registry on loopback behind it: {', '.join(sorted(instances))}"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
