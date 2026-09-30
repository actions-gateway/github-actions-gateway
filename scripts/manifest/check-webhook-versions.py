#!/usr/bin/env python3
"""check-webhook-versions.py — every admission webhook rule must name an API
version that one of this repo's CustomResourceDefinitions serves (Q1068).

A rule whose apiVersions name only versions nobody serves matches no request,
and an unmatched rule is not a rejection: `failurePolicy: Fail` never fires,
because the webhook is never called. Admission validation for that kind stops
without an error anywhere. The apiserver's default `matchPolicy: Equivalent`
routes a request at any served version to a version the rule names, which is
how a `v2alpha1`-only rule validates `v2beta1` writes today; it has nothing to
route to once the named version is gone.

That is exactly what `v2.0.0` does to the `actions-gateway.com` rules: it
removes `v2alpha1` and `v2beta1`, and each rule's version comes from a
`+kubebuilder:webhook:...versions=` marker that nothing ties to the CRD's served
set. Deleting the API package forces the handler's Go type to change; nothing
forces the marker to. So this gate reads both generated outputs and holds them
to each other:

  cmd/gmc/config/webhook/manifests.yaml   the controller-gen webhook rules
                                          (the chart's webhook.yaml is generated
                                          from it and gated by chart-webhook-check)
  api/config/crd/*.yaml                   the actions-gateway.com CRDs
  cmd/*/config/crd/bases/*.yaml           the actions-gateway.github.com CRDs

A rule naming a group or resource no CRD here defines fails too: removing a
whole CRD leaves its webhook in exactly the same state as removing one version.

Exit: 0 every rule matches a served version, 1 one does not, 2 a read that
could not be taken — an empty extraction is a parser that stopped matching, and
passing the rules off one is the failure this gate would otherwise become.
"""

import re
import sys
from pathlib import Path

WEBHOOKS = Path("cmd/gmc/config/webhook/manifests.yaml")
CRD_GLOBS = ("api/config/crd/*.yaml", "cmd/*/config/crd/bases/*.yaml")

# controller-gen's CRD layout: spec keys at two spaces, a versions item's own
# keys at four. The schema nests far deeper, so these anchors cannot match inside it.
GROUP_RE = re.compile(r"^  group: (\S+)$", re.M)
PLURAL_RE = re.compile(r"^    plural: (\S+)$", re.M)
VERSION_ITEM_RE = re.compile(r"^  - ", re.M)
VERSION_NAME_RE = re.compile(r"^    name: (\S+)$", re.M)
SERVED_RE = re.compile(r"^    served: (true|false)$", re.M)


class Refusal(Exception):
    """A read that could not be taken. Never a verdict on the rules."""


def served_versions(path):
    """Return ((group, plural), {version: served}) for one CRD file."""
    text = path.read_text()
    group, plural = GROUP_RE.search(text), PLURAL_RE.search(text)
    head, sep, versions = text.partition("\n  versions:\n")
    if not (group and plural and sep):
        raise Refusal(f"{path}: no spec.group, spec.names.plural or spec.versions")
    served = {}
    for item in VERSION_ITEM_RE.split(versions)[1:]:
        name, flag = VERSION_NAME_RE.search(item), SERVED_RE.search(item)
        if not (name and flag):
            raise Refusal(f"{path}: a spec.versions entry with no name or served field")
        served[name.group(1)] = flag.group(1) == "true"
    if not served:
        raise Refusal(f"{path}: spec.versions lists no version")
    return (group.group(1), plural.group(1)), served


def list_block(block, key, indent):
    """Return the '- item' entries under `key:` at the given indent."""
    m = re.search(rf"^{indent}{key}:\n((?:{indent}- \S+\n)+)", block, re.M)
    if not m:
        return None
    return re.findall(rf"^{indent}- (\S+)$", m.group(1), re.M)


def webhook_rules(path):
    """Yield (webhook name, group, resource, versions) for every rule."""
    text = path.read_text()
    found = 0
    for block in re.split(r"^- ", text, flags=re.M)[1:]:
        name = re.search(r"^  name: (\S+)$", block, re.M)
        rules = block.partition("\n  rules:\n")[2]
        if not (name and rules):
            raise Refusal(f"{path}: a webhook with no name or rules")
        for rule in re.split(r"^  - ", rules, flags=re.M)[1:]:
            # A rule's first key rides on the '- ' line, so put its indent back.
            rule = "    " + rule
            groups = list_block(rule, "apiGroups", "    ")
            versions = list_block(rule, "apiVersions", "    ")
            resources = list_block(rule, "resources", "    ")
            if not (groups and versions and resources):
                raise Refusal(f"{path}: {name.group(1)}: a rule missing apiGroups, apiVersions or resources")
            for group in groups:
                for resource in resources:
                    found += 1
                    yield name.group(1), group, resource, versions
    if not found:
        raise Refusal(f"{path}: no webhook rules extracted")


def main():
    crds = {}
    for pattern in CRD_GLOBS:
        for path in sorted(Path(".").glob(pattern)):
            key, served = served_versions(path)
            crds[key] = served
    if not crds:
        raise Refusal(f"no CRDs under {', '.join(CRD_GLOBS)}")

    problems, checked = [], 0
    for name, group, resource, versions in webhook_rules(WEBHOOKS):
        checked += 1
        served = crds.get((group, resource))
        if served is None:
            problems.append(f"{name}: names {resource}.{group}, which no CRD in this repo defines")
            continue
        live = [v for v in versions if v == "*" or served.get(v)]
        if not live:
            have = ", ".join(v for v, on in served.items() if on) or "none"
            problems.append(
                f"{name}: names {resource}.{group} at {', '.join(versions)}, "
                f"none of which the CRD serves (served: {have})"
            )

    if problems:
        print("admission webhook rules name versions nothing serves:", file=sys.stderr)
        for problem in problems:
            print(f"  - {problem}", file=sys.stderr)
        print(
            "\nSuch a rule matches no request, so the webhook is never called and its "
            "validation stops silently.\nPoint the handler and its +kubebuilder:webhook "
            f"versions= marker at a served version, then run `make manifests`.\n"
            f"Rules: {WEBHOOKS}",
            file=sys.stderr,
        )
        return 1

    print(f"webhook rules match a served version: {checked} rules over {len(crds)} CRDs")
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Refusal as err:
        print(f"check-webhook-versions: cannot read: {err}", file=sys.stderr)
        sys.exit(2)
