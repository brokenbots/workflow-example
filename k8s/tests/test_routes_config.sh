#!/usr/bin/env bash
set -euo pipefail

# Regression test for the routes ConfigMap (CRI-216). Validates the shipped
# example payload against the shipped JSON Schema (k8s/routes.schema.json),
# exercises the schema's fail-closed constraints with negative variants, and
# proves the ADR-0005 D1/D2 modes (image-only, url-only, url+image + expected
# pin) and the pvc/nfs/tmp volumes with env mapping and OpenBao/CSI secret
# name references are representable. Also asserts the example is secret-free
# and carries the KB-281 post-retirement shape: the retired linear estate
# (linear-intake-url / linear-triage-url / linear-develop-url objects and the
# criteria-triage / criteria-develop routes) is gone, the criteria project is
# cleanup-only, the kanboard routes fire the pinned kanboard_triage_v1 /
# kanboard_develop_v1 trees, the git-ref pin duality (url '?ref=' vs the
# separate "ref" key) agrees per entry, and the adapterImages pins agree
# three ways: shipped example == RECORDED_PIN_SET == live criteria-routes CM
# (the live leg skips when no kubectl/cluster is reachable, e.g. CI).
#
# KB-313 (ADR 307): namespace is optional on workflowLibrary entries and the
# global run-config surfaces (volumes/secrets/env/adapterImages) ride the
# two-revision gate — warned while the migration window is open, rejected
# once every projectRegistry entry is cutover: done (mirrored from
# criteria-k8s/internal/routes).
#
# Validation always runs a dependency-free structural validator mirroring the
# schema. When python's jsonschema module is available, the payload and every
# variant are additionally checked against the shipped schema itself, so the
# schema and the example cannot drift apart.

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SCHEMA="$REPO_ROOT/k8s/routes.schema.json"
EXAMPLE="$REPO_ROOT/k8s/examples/routes-configmap.yaml"

fail() {
    echo "FAIL: $1" >&2
    exit 1
}

command -v python3 >/dev/null 2>&1 || fail "python3 is required"

[ -f "$SCHEMA" ] || fail "k8s/routes.schema.json is missing"
[ -f "$EXAMPLE" ] || fail "k8s/examples/routes-configmap.yaml is missing"

python3 -m json.tool "$SCHEMA" >/dev/null 2>&1 || fail "k8s/routes.schema.json is not valid JSON"

manifest=$(cat "$EXAMPLE")
[ -n "$manifest" ] || fail "routes ConfigMap example is empty"

# ------------------------------------------------------------ manifest shape
grep -q <<< "$manifest" '^kind: ConfigMap$' || \
    fail "example is not a ConfigMap"
grep -q <<< "$manifest" '^  name: criteria-routes$' || \
    fail "example ConfigMap is not named criteria-routes"
grep -q <<< "$manifest" '^  namespace: criteria-jobs$' || \
    fail "example ConfigMap is not namespaced to criteria-jobs"
grep -q <<< "$manifest" '^  routes.json: |$' || \
    fail "example ConfigMap does not carry the routes.json data key"
grep -q <<< "$manifest" '^kind: Secret$' && \
    fail "example ConfigMap must not contain a Secret resource"
grep -q <<< "$manifest" 'secretKeyRef' && \
    fail "example ConfigMap uses secretKeyRef"
grep -q <<< "$manifest" 'secretRef' && \
    fail "example ConfigMap uses secretRef"
grep -q <<< "$manifest" 'envFrom:' && \
    fail "example ConfigMap uses envFrom"
grep -q <<< "$manifest" 'stringData' && \
    fail "example ConfigMap uses stringData"
printf '%s' "$manifest" | grep -Eq 'ghp_[A-Za-z0-9]|github_pat_|BEGIN (RSA |EC )?PRIVATE KEY' && \
    fail "example ConfigMap embeds credential material"

# ---------------------------------------------------------------- validation
python3 - "$SCHEMA" "$EXAMPLE" "$REPO_ROOT" <<'PYEOF'
import copy
import json
import os
import re
import shutil
import subprocess
import sys

schema_path, example_path, repo_root = sys.argv[1], sys.argv[2], sys.argv[3]

LABEL_RE = re.compile(r"^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$")
ENV_NAME_RE = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*$")
SECRET_KEY_RE = re.compile(r"^[A-Za-z0-9._-]+$")
NOSPACE_RE = re.compile(r"^\S+$")
MOUNT_RE = re.compile(r"^/")

WORKFLOW_KEYS = {"type", "namespace", "class", "image", "url", "ref", "volumes", "secrets", "env", "adapterImages", "interface"}
VOLUME_KEYS = {"name", "kind", "mountPath", "subPath", "readOnly", "claim", "server", "path", "sizeLimit", "secretName", "env"}
SECRET_KEYS = {"name", "secretProviderClass", "mountPath", "env"}
ROUTE_KEYS = {"name", "workflow", "project", "tags", "tagMatch", "states", "configRef"}
TOP_KEYS = {"apiVersion", "kind", "workflowLibrary", "configLibrary", "routes", "projectRegistry"}
# KB-311 (ADR 307): projectRegistry — the ONLY ticket->project routing table.
# Entries mirror routes.schema.json's projectEntry: closed key set, DNS-1123
# namespace, advisory owner/name repo members, cutover marker vocabulary of
# one value ("done"). Route projects must exist here when the key is present.
PROJECT_ENTRY_KEYS = {"namespace", "repos", "cutover"}
REPO_REF_RE = re.compile(r"^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$")
CONFIG_KEYS = {"buildCmd", "testCmd", "ciGateCmd", "baseBranch", "tags", "tagMatch"}
CONFIG_TAG_MATCHES = {"", "all", "any"}
VOLUME_KINDS = ("pvc", "nfs", "tmp", "k8s-secret")
WORKFLOW_TYPES = ("url", "image")
WORKFLOW_CLASSES = ("dev", "triage")

# KB-310 (ADR 307): an interface declaration rides every type=url workflow
# object; the inline mirror matches k8s/workflow-interface.schema.json (the
# schema of record). NAME_RE values mirror the interface schema's
# pattern-constrained fields; SLOT_RE is the config-slot name surface ("the
# tree decides what may be overridden" — camelCase slot names, lowercase
# first letter), and VAR_RE/ENV_RE are tree-variable and secret-env lanes.
INTERFACE_KEYS = {"apiVersion", "kind", "workflow", "description", "adapters", "mounts", "secretEnv", "env", "configSlots"}
MOUNT_KEYS = {"name", "mountPath", "kind"}
SLOT_KEYS = {"slot", "variable", "target", "description"}
INTERFACE_NAME_RE = re.compile(r"^[A-Za-z][A-Za-z0-9_-]*$")
MOUNT_PATH_RE = re.compile(r"^/.+[^/]$")  # two-segment minimum, no trailing slash — same grammar as routes.go interfacePathRe / interface_validate.mjs / the schema
MOUNT_KINDS = ("pvc", "tmp")
SECRET_ENV_RE = re.compile(r"^[A-Z][A-Z0-9_]*$")
SLOT_RE = re.compile(r"^[a-z][a-zA-Z0-9_]*$")
VAR_RE = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*$")


def load_example(path):
    lines = open(path, encoding="utf-8").read().splitlines()
    start = None
    for i, line in enumerate(lines):
        if line == "  routes.json: |":
            start = i + 1
            break
    assert start is not None, "routes.json data key not found in ConfigMap"
    block = []
    for line in lines[start:]:
        if line.strip() == "":
            block.append("")
            continue
        if not line.startswith("    "):
            break
        block.append(line[4:])
    payload = "\n".join(block)
    assert payload.strip().startswith("{"), "extracted routes.json payload is not JSON"
    return json.loads(payload)


def is_label(value):
    return isinstance(value, str) and LABEL_RE.match(value) is not None


def check_env(value, where, errs, value_re=None):
    if not isinstance(value, dict):
        errs.append(f"{where}.env must be an object")
        return
    for name, val in value.items():
        if not isinstance(name, str) or not ENV_NAME_RE.match(name):
            errs.append(f"{where}.env has invalid env var name {name!r}")
        if not isinstance(val, str):
            errs.append(f"{where}.env.{name} must be a string")
        elif value_re is not None and value_re.match(val) is None:
            errs.append(f"{where}.env.{name} value {val!r} is not a name reference")


def validate_secret(secret, where, errs):
    if not isinstance(secret, dict):
        errs.append(f"{where} must be an object")
        return
    for key in ("name", "secretProviderClass", "mountPath"):
        if key not in secret:
            errs.append(f"{where} is missing required field {key}")
    for key in secret:
        if key not in SECRET_KEYS:
            errs.append(f"{where} has unknown field {key!r}")
    name = secret.get("name")
    if "name" in secret and not is_label(name):
        errs.append(f"{where}.name {name!r} is not a DNS-1123 label")
    spc = secret.get("secretProviderClass")
    if "secretProviderClass" in secret and (not isinstance(spc, str) or not spc):
        errs.append(f"{where}.secretProviderClass must be a non-empty string")
    mount = secret.get("mountPath")
    if "mountPath" in secret and (not isinstance(mount, str) or MOUNT_RE.match(mount) is None):
        errs.append(f"{where}.mountPath {mount!r} must be an absolute path")
    if "env" in secret:
        check_env(secret["env"], where, errs, value_re=SECRET_KEY_RE)


def validate_volume(volume, where, errs):
    if not isinstance(volume, dict):
        errs.append(f"{where} must be an object")
        return
    for key in ("name", "kind", "mountPath"):
        if key not in volume:
            errs.append(f"{where} is missing required field {key}")
    for key in volume:
        if key not in VOLUME_KEYS:
            errs.append(f"{where} has unknown field {key!r}")
    name = volume.get("name")
    if "name" in volume and not is_label(name):
        errs.append(f"{where}.name {name!r} is not a DNS-1123 label")
    kind = volume.get("kind")
    if "kind" in volume and kind not in VOLUME_KINDS:
        errs.append(f"{where}.kind {kind!r} must be one of pvc, nfs, tmp, k8s-secret")
    mount = volume.get("mountPath")
    if "mountPath" in volume and (not isinstance(mount, str) or MOUNT_RE.match(mount) is None):
        errs.append(f"{where}.mountPath {mount!r} must be an absolute path")
    if "subPath" in volume and (not isinstance(volume["subPath"], str) or not volume["subPath"]):
        errs.append(f"{where}.subPath must be a non-empty string")
    if "readOnly" in volume and not isinstance(volume["readOnly"], bool):
        errs.append(f"{where}.readOnly must be a boolean")
    for key in ("claim", "server", "path", "sizeLimit", "secretName"):
        if key in volume and (not isinstance(volume[key], str) or not volume[key]):
            errs.append(f"{where}.{key} must be a non-empty string")
    if "env" in volume:
        check_env(volume["env"], where, errs)
    if kind == "pvc":
        if "claim" not in volume:
            errs.append(f"{where}: pvc volume requires claim")
        for key in ("server", "path", "sizeLimit", "secretName"):
            if key in volume:
                errs.append(f"{where}: pvc volume must not declare {key}")
    elif kind == "nfs":
        for key in ("server", "path"):
            if key not in volume:
                errs.append(f"{where}: nfs volume requires {key}")
        for key in ("claim", "sizeLimit", "secretName"):
            if key in volume:
                errs.append(f"{where}: nfs volume must not declare {key}")
    elif kind == "tmp":
        for key in ("claim", "server", "path", "secretName"):
            if key in volume:
                errs.append(f"{where}: tmp volume must not declare {key}")
    elif kind == "k8s-secret":
        # KB-7: a k8s-secret volume mounts a plain namespace Secret as
        # files at mountPath (e.g. /home/criteria/secrets); secretName is
        # its only backing field.
        if "secretName" not in volume:
            errs.append(f"{where}: k8s-secret volume requires secretName")
        for key in ("claim", "server", "path", "sizeLimit"):
            if key in volume:
                errs.append(f"{where}: k8s-secret volume must not declare {key}")


def validate_interface(iface, where, errs):
    if not isinstance(iface, dict):
        errs.append(f"{where} must be an object")
        return
    for key in ("apiVersion", "kind", "workflow"):
        if key not in iface:
            errs.append(f"{where} is missing required field {key}")
    for key in iface:
        if key not in INTERFACE_KEYS:
            errs.append(f"{where} has unknown field {key!r}")
    if iface.get("apiVersion") != "criteria.brokenbots.dev/v1":
        errs.append(f"{where}.apiVersion must be criteria.brokenbots.dev/v1")
    if iface.get("kind") != "WorkflowInterface":
        errs.append(f"{where}.kind must be WorkflowInterface")
    for key in ("workflow", "description"):
        if key in iface and (not isinstance(iface[key], str) or not iface[key]):
            errs.append(f"{where}.{key} must be a non-empty string")
    for key in ("adapters", "secretEnv", "env"):
        if key not in iface:
            continue
        names = iface[key]
        if not isinstance(names, list):
            errs.append(f"{where}.{key} must be an array")
            continue
        for i, val in enumerate(names):
            if not isinstance(val, str) or not val:
                errs.append(f"{where}.{key}[{i}] must be a non-empty string")
                continue
            if key == "adapters" and not INTERFACE_NAME_RE.match(val):
                errs.append(f"{where}.adapters[{i}] {val!r} is not an adapter kind name")
            if key in ("secretEnv", "env") and not SECRET_ENV_RE.match(val):
                errs.append(f"{where}.{key}[{i}] {val!r} is not an env name (UPPER_SNAKE)")
        if len(set(names)) != len(names):
            errs.append(f"{where}.{key} has duplicate entries")
    if "mounts" in iface:
        mounts = iface["mounts"]
        if not isinstance(mounts, list):
            errs.append(f"{where}.mounts must be an array")
        else:
            seen = set()
            for i, mount in enumerate(mounts):
                mw = f"{where}.mounts[{i}]"
                if not isinstance(mount, dict):
                    errs.append(f"{mw} must be an object")
                    continue
                for key in ("name", "mountPath", "kind"):
                    if key not in mount:
                        errs.append(f"{mw} is missing required field {key}")
                for key in mount:
                    if key not in MOUNT_KEYS:
                        errs.append(f"{mw} has unknown field {key!r}")
                mname = mount.get("name")
                if mname is None or not isinstance(mname, str) or not INTERFACE_NAME_RE.match(mname or ""):
                    errs.append(f"{mw}.name is not a mount name")
                elif mname in seen:
                    errs.append(f"{where}.mounts has duplicate name {mname!r}")
                seen.add(mname)
                for key in ("mountPath", "kind"):
                    if key == "mountPath" and ("mountPath" not in mount or MOUNT_PATH_RE.match(mount.get("mountPath", "")) is None):
                        errs.append(f"{mw}.mountPath must be an absolute path")
                    if key == "kind" and mount.get("kind") not in MOUNT_KINDS:
                        errs.append(f"{mw}.kind {mount.get('kind')!r} must be one of pvc, tmp")
    if "configSlots" in iface:
        slots = iface["configSlots"]
        if not isinstance(slots, list):
            errs.append(f"{where}.configSlots must be an array")
        else:
            seen = set()
            for i, slot in enumerate(slots):
                sw = f"{where}.configSlots[{i}]"
                if not isinstance(slot, dict):
                    errs.append(f"{sw} must be an object")
                    continue
                for key in ("slot", "variable", "target"):
                    if key not in slot:
                        errs.append(f"{sw} is missing required field {key}")
                    elif not isinstance(slot[key], str) or not slot[key]:
                        errs.append(f"{sw}.{key} must be a non-empty string")
                for key in slot:
                    if key not in SLOT_KEYS:
                        errs.append(f"{sw} has unknown field {key!r}")
                name = slot.get("slot")
                if name is None or SLOT_RE.match(name or "") is None:
                    errs.append(f"{sw}.slot {name!r} is not a slot name (lowercase-first camelCase)")
                elif name in seen:
                    errs.append(f"{where}.configSlots has duplicate slot {name!r}")
                seen.add(name)
                if "variable" in slot and VAR_RE.match(slot.get("variable", "")) is None:
                    errs.append(f"{sw}.variable {slot.get('variable')!r} is not a tree variable name")


def validate_workflow(name, workflow, where, errs):
    if not isinstance(workflow, dict):
        errs.append(f"{where} must be an object")
        return
    for key in ("type",):
        # KB-313 (ADR 307): namespace is optional — cutover-done projects run
        # in their project binding's namespace; a namespace-less entry's runs
        # resolve the run's own namespace.
        if key not in workflow:
            errs.append(f"{where} is missing required field {key}")
    for key in workflow:
        if key not in WORKFLOW_KEYS:
            errs.append(f"{where} has unknown field {key!r}")
    wtype = workflow.get("type")
    if "type" in workflow and wtype not in WORKFLOW_TYPES:
        errs.append(f"{where}.type {wtype!r} must be one of url, image")
    if "interface" in workflow:
        if wtype == "image":
            errs.append(f"{where}: type image must not declare interface (ADR-0005 D1; "
                        "the interface is the tree's, not the image's)")
        else:
            validate_interface(workflow["interface"], f"{where}.interface", errs)
    elif wtype == "url":
        errs.append(f"{where}: type url requires interface (KB-310 — every shared tree "
                    "declares the machine-readable contract the operator validates bindings against)")
    namespace = workflow.get("namespace")
    if "namespace" in workflow and not is_label(namespace):
        errs.append(f"{where}.namespace {namespace!r} is not a DNS-1123 label")
    wclass = workflow.get("class")
    if "class" in workflow and wclass not in WORKFLOW_CLASSES:
        errs.append(f"{where}.class {wclass!r} must be one of dev, triage")
    for key in ("image", "url", "ref"):
        if key in workflow and (not isinstance(workflow[key], str) or NOSPACE_RE.match(workflow[key]) is None):
            errs.append(f"{where}.{key} must be a non-blank string")
    if "volumes" in workflow:
        volumes = workflow["volumes"]
        if not isinstance(volumes, list):
            errs.append(f"{where}.volumes must be an array")
        else:
            seen = set()
            for i, volume in enumerate(volumes):
                vn = volume.get("name") if isinstance(volume, dict) else None
                if vn in seen:
                    errs.append(f"{where}.volumes has duplicate name {vn!r}")
                seen.add(vn)
                validate_volume(volume, f"{where}.volumes[{i}]", errs)
    if "secrets" in workflow:
        secrets = workflow["secrets"]
        if not isinstance(secrets, list):
            errs.append(f"{where}.secrets must be an array")
        else:
            seen = set()
            for i, secret in enumerate(secrets):
                sn = secret.get("name") if isinstance(secret, dict) else None
                if sn in seen:
                    errs.append(f"{where}.secrets has duplicate name {sn!r}")
                seen.add(sn)
                validate_secret(secret, f"{where}.secrets[{i}]", errs)
    if "env" in workflow:
        check_env(workflow["env"], where, errs)
    if "adapterImages" in workflow:
        # CRI-214 M14: per-adapter-kind image override, same surface as
        # volumes/secrets/env. Keys are adapter kinds (DNS-1123 labels,
        # matching the Go validator and the routes-schema/shell-test key
        # check); values are full image references (non-blank, no whitespace).
        images = workflow["adapterImages"]
        if not isinstance(images, dict):
            errs.append(f"{where}.adapterImages must be an object")
        else:
            for kind, ref in images.items():
                if not is_label(kind):
                    errs.append(f"{where}.adapterImages kind {kind!r} is not a DNS-1123 label")
                if not isinstance(ref, str) or NOSPACE_RE.match(ref) is None:
                    errs.append(f"{where}.adapterImages.{kind} must be a non-blank string")
    if wtype == "image":
        if "image" not in workflow:
            errs.append(f"{where}: type image requires image")
        for key in ("url", "ref"):
            if key in workflow:
                errs.append(f"{where}: type image must not declare {key} (ADR-0005 D2 fail-closed)")
    if wtype == "url" and "url" not in workflow:
        errs.append(f"{where}: type url requires url")


def validate_route(route, where, library, config_library, errs):
    if not isinstance(route, dict):
        errs.append(f"{where} must be an object")
        return
    for key in ("name", "workflow", "project"):
        if key not in route:
            errs.append(f"{where} is missing required field {key}")
    for key in route:
        if key not in ROUTE_KEYS:
            errs.append(f"{where} has unknown field {key!r}")
    for key in ("name", "workflow"):
        if key in route and not is_label(route[key]):
            errs.append(f"{where}.{key} {route[key]!r} is not a DNS-1123 label")
    project = route.get("project")
    if "project" in route and (not isinstance(project, str) or not project):
        errs.append(f"{where}.project must be a non-empty string")
    tag_match = route.get("tagMatch")
    if "tagMatch" in route and tag_match not in ("all", "any"):
        errs.append(f"{where}.tagMatch {tag_match!r} must be one of all, any")
    if "tags" in route:
        tags = route["tags"]
        if not isinstance(tags, list) or any(not isinstance(t, str) or not t for t in tags):
            errs.append(f"{where}.tags must be an array of non-empty strings")
        elif len(set(tags)) != len(tags):
            errs.append(f"{where}.tags must be unique")
    states = route.get("states")
    if "states" in route:
        if not isinstance(states, list) or any(not isinstance(s, str) or not s for s in states):
            errs.append(f"{where}.states must be an array of non-empty strings")
        elif not states:
            errs.append(f"{where}.states must not be empty")
        elif len(set(states)) != len(states):
            errs.append(f"{where}.states must be unique")
    workflow = route.get("workflow")
    if "workflow" in route and isinstance(library, dict) and workflow not in library:
        errs.append(f"{where}.workflow {workflow!r} is not in workflowLibrary")
    config_ref = route.get("configRef")
    if "configRef" in route:
        if not is_label(config_ref):
            errs.append(f"{where}.configRef {config_ref!r} is not a DNS-1123 label")
        elif isinstance(config_library, dict) and config_ref not in config_library:
            errs.append(f"{where}.configRef {config_ref!r} is not in configLibrary")


def validate_config(name, entry, where, errs):
    if not isinstance(entry, dict):
        errs.append(f"{where} must be an object")
        return
    if not is_label(name):
        errs.append(f"configLibrary key {name!r} is not a DNS-1123 label")
    for key in entry:
        if key not in CONFIG_KEYS:
            errs.append(f"{where} has unknown field {key!r}")
    for key in CONFIG_KEYS:
        if key not in entry:
            continue
        value = entry[key]
        if key == "tags":
            if not isinstance(value, list) or not value:
                errs.append(f"{where}.tags must be a non-empty list of tag names (empty means name-only selection; omit the field)")
                continue
            for tag in value:
                if not isinstance(tag, str) or not tag.strip():
                    errs.append(f"{where}.tags must carry non-empty tag names")
                    break
            continue
        if key == "tagMatch":
            if not isinstance(value, str) or value not in CONFIG_TAG_MATCHES:
                errs.append(f"{where}.tagMatch {value!r} must be one of: empty, all, any")
            continue
        if not isinstance(value, str) or not value.strip():
            errs.append(f"{where}.{key} must be a non-empty string")


def validate(doc):
    errs = []
    if not isinstance(doc, dict):
        return ["payload must be an object"]
    for key in ("apiVersion", "kind", "workflowLibrary", "routes"):
        if key not in doc:
            errs.append(f"payload is missing required field {key}")
    for key in doc:
        if key not in TOP_KEYS:
            errs.append(f"payload has unknown field {key!r}")
    if doc.get("apiVersion") != "criteria.brokenbots.dev/v1":
        errs.append("payload.apiVersion must be criteria.brokenbots.dev/v1")
    if doc.get("kind") != "Routes":
        errs.append("payload.kind must be Routes")
    library = doc.get("workflowLibrary")
    if "workflowLibrary" in doc:
        if not isinstance(library, dict) or not library:
            errs.append("payload.workflowLibrary must be a non-empty object")
        else:
            for name, workflow in library.items():
                if not is_label(name):
                    errs.append(f"payload.workflowLibrary key {name!r} is not a DNS-1123 label")
                validate_workflow(name, workflow, f"payload.workflowLibrary.{name}", errs)
    config_library = doc.get("configLibrary")
    if "configLibrary" in doc:
        if not isinstance(config_library, dict):
            errs.append("payload.configLibrary must be an object")
        else:
            for name, entry in config_library.items():
                validate_config(name, entry, f"payload.configLibrary.{name}", errs)
    routes = doc.get("routes")
    if "routes" in doc:
        if not isinstance(routes, list) or not routes:
            errs.append("payload.routes must be a non-empty array")
        else:
            seen = set()
            for i, route in enumerate(routes):
                rn = route.get("name") if isinstance(route, dict) else None
                if rn in seen:
                    errs.append(f"payload.routes has duplicate name {rn!r}")
                seen.add(rn)
                validate_route(route, f"payload.routes[{i}]", library, config_library, errs)
    registry = doc.get("projectRegistry")
    if "projectRegistry" in doc:
        validate_registry(registry, errs)
        if isinstance(registry, dict):
            for i, route in enumerate(routes if isinstance(routes, list) else []):
                if isinstance(route, dict) and route.get("project") not in registry:
                    errs.append(f"payload.routes[{i}].project {route.get('project')!r} is not in projectRegistry (the registry is the only ticket->project routing table, KB-311)")
        # KB-313 (ADR 307): the global CM is a thin catalog — a post-cutover
        # payload (the registry is non-empty and every entry cutover: done)
        # that still carries global run-config (volumes/secrets/env/
        # adapterImages) on a workflowLibrary entry is rejected. This mirrors
        # criteria-k8s/internal/routes' two-revision hard edge; pre-cutover
        # payloads keep the surfaces with a warning on the Go side.
        if isinstance(registry, dict) and registry \
                and not [p for p, e in registry.items()
                         if not (isinstance(e, dict) and e.get("cutover") == "done")]:
            for name, workflow in (library or {}).items():
                if not isinstance(workflow, dict):
                    continue
                present = [k for k in ("volumes", "secrets", "env", "adapterImages") if k in workflow]
                if present:
                    errs.append(f"payload.workflowLibrary.{name} still carries global run-config {', '.join(present)}; post-cutover the global CM is workflow catalog + projectRegistry + routes only (KB-313, ADR 307)")
    return errs


def validate_registry(registry, errs):
    # KB-311 (ADR 307): the project registry — closed key sets, DNS-1123
    # namespaces, advisory owner/name repo members, cutover vocabulary of
    # exactly one marker ("done"). Same rules as routes.schema.json's
    # projectEntry and interface_validate.mjs's registryViolations mirror.
    if not isinstance(registry, dict) or not registry:
        errs.append("payload.projectRegistry must be a non-empty object of project name -> entry")
        return
    for project, entry in registry.items():
        if not isinstance(project, str) or re.search(r"\S", project) is None:
            errs.append("payload.projectRegistry has a blank project name")
            continue
        if not isinstance(entry, dict):
            errs.append(f"payload.projectRegistry.{project} must be an object")
            continue
        for key in entry:
            if key not in PROJECT_ENTRY_KEYS:
                errs.append(f"payload.projectRegistry.{project} has unknown field {key!r}")
        namespace = entry.get("namespace")
        if "namespace" not in entry or not is_label(namespace):
            errs.append(f"payload.projectRegistry.{project}.namespace must be a DNS-1123 label")
        repos = entry.get("repos")
        if repos is not None and (not isinstance(repos, list) or any(
                not isinstance(r, str) or REPO_REF_RE.match(r) is None for r in repos)):
            errs.append(f"payload.projectRegistry.{project}.repos must be an array of owner/name repository references")
        cutover = entry.get("cutover")
        if "cutover" in entry and cutover not in ("done",):
            errs.append(f"payload.projectRegistry.{project}.cutover {cutover!r} must be \"done\" or absent (binding-first, deletion second)")


# --------------------------------------------------------- example variants
base = load_example(example_path)
LIB = "workflowLibrary"
BAKED = "linear-intake-v1"
URLWF = "kanboard-develop-url"


def mutate(fn):
    doc = copy.deepcopy(base)
    fn(doc)
    return doc


positive = [
    ("url-workflow-with-image-and-ref", mutate(lambda d: d[LIB][URLWF].update(
        image="localhost:5000/linear-intake-remote:dev",
        ref="0123456789abcdef0123456789abcdef01234567"))),
    ("route-with-tag-subset-all", mutate(lambda d: d["routes"][0].update(
        tags=["intake", "triage"], tagMatch="all"))),
    ("route-with-tag-subset-any", mutate(lambda d: d["routes"][0].update(
        tags=["intake"], tagMatch="any"))),
    ("route-with-default-tagmatch", mutate(lambda d: d["routes"][0].update(tags=["intake"]))),
    ("workflow-class-dev", mutate(lambda d: d[LIB][BAKED].update(**{"class": "dev"}))),
    ("workflow-class-triage", mutate(lambda d: d[LIB][URLWF].update(**{"class": "triage"}))),
    ("route-without-states-defaults-triage", mutate(lambda d: d["routes"][0].pop("states"))),
    ("route-with-custom-states", mutate(lambda d: d["routes"][0].update(
        states=["Triage", "In Progress"]))),
    ("pvc-volume-as-nfs", mutate(lambda d: (
        d[LIB][BAKED]["volumes"][0].pop("claim"),
        d[LIB][BAKED]["volumes"][0].update(kind="nfs", server="nfs.internal", path="/export/data")))),
    ("volume-read-only", mutate(lambda d: d[LIB][BAKED]["volumes"][0].update(readOnly=True))),
    ("volume-subpath", mutate(lambda d: d[LIB][BAKED]["volumes"][0].update(subPath="intake"))),
    ("k8s-secret-volume", mutate(lambda d: d[LIB][URLWF]["volumes"].insert(
        2, {"name": "wf-tokens", "kind": "k8s-secret", "secretName": "github-tokens",
            "mountPath": "/home/criteria/secrets", "readOnly": True,
            "env": {"WORKFLOW_GITHUB_TOKEN": "/home/criteria/secrets/workflow_github_token"}}))),
    ("workflow-level-env", mutate(lambda d: d[LIB][BAKED].update(env={"ALLOW_DIRTY": "false"}))),
    ("workflow-adapter-images-override", mutate(lambda d: d[LIB][BAKED].update(
        adapterImages={"shell": "localhost:5000/criteria-adapter-shell:k8s-0.5.4-2",
                       "copilot": "localhost:5000/criteria-adapter-copilot@sha256:" + "0" * 64}))),
]

# KB-103: configLibrary + per-route configRef — the explicit route
# binding and a bare (name-only) config entry are both valid.


def kb103_add_route_configref(d):
    d["configLibrary"]["acme-repo"] = {"buildCmd": "scripts/build.sh", "testCmd": "make test"}
    d["routes"][0].update(configRef="acme-repo")


def kb103_add_empty_config(d):
    d["configLibrary"]["acme-repo"] = {}


def kb103_add_gate_only_config(d):
    d["configLibrary"]["acme-repo"] = {"ciGateCmd": "make ci && make vuln-scan"}


positive.extend([
    ("config-library-with-route-configref", mutate(kb103_add_route_configref)),
    ("config-library-entry-empty", mutate(kb103_add_empty_config)),
    ("config-library-gate-only", mutate(kb103_add_gate_only_config)),
])

# KB-154: tag-group config entries — tags + tagMatch + baseBranch are all
# valid shapes of the config schema (release-branch targeting rides a
# configs-label-group/ticket-tag overlay, and baseBranch needs no
# whitespace to be a git ref).
def kb154_add_tag_group(d):
    d["configLibrary"]["acme-repo-v060"] = {
        "buildCmd": "scripts/build.sh", "baseBranch": "v0.6.0-release", "tags": ["v0.6.0"],
    }


def kb154_add_matching_tagmatch(d):
    d["configLibrary"]["acme-repo-beta"] = {
        "baseBranch": "beta-release", "tags": ["beta", "v0.6.0"], "tagMatch": "any",
    }


def kb154_add_empty_tagmatch(d):
    d["configLibrary"]["acme-repo-main"] = {"baseBranch": "release-test", "tags": ["v0.6.0"], "tagMatch": ""}


def kb154_add_basebranch_only(d):
    d["configLibrary"]["acme-repo-release"] = {"baseBranch": "release"}


positive.extend([
    ("config-library-tag-group-entry", mutate(kb154_add_tag_group)),
    ("config-library-tag-match-any", mutate(kb154_add_matching_tagmatch)),
    ("config-library-tag-match-empty", mutate(kb154_add_empty_tagmatch)),
    ("config-library-basebranch-only", mutate(kb154_add_basebranch_only)),
])

negative = [
    ("image-workflow-with-url", True, mutate(lambda d: d[LIB][BAKED].update(
        url="git::https://example.invalid/repo.git//wf"))),
    ("image-workflow-with-ref", True, mutate(lambda d: d[LIB][BAKED].update(ref="sha256:beef"))),
    ("image-workflow-without-image", True, mutate(lambda d: d[LIB][BAKED].pop("image"))),
    ("url-workflow-without-url", True, mutate(lambda d: d[LIB][URLWF].pop("url"))),
    ("workflow-unknown-type", True, mutate(lambda d: d[LIB][BAKED].update(type="baked"))),
    ("workflow-unknown-class", True, mutate(lambda d: d[LIB][BAKED].update(**{"class": "concurrent"}))),
    ("pvc-volume-without-claim", True, mutate(lambda d: d[LIB][BAKED]["volumes"][0].pop("claim"))),
    ("nfs-volume-without-server", True, mutate(lambda d: d[LIB][URLWF]["volumes"].insert(
        2, {"name": "wf-cache", "kind": "nfs", "path": "/export/workflow-cache", "mountPath": "/mnt/workflow-cache", "readOnly": True}))),
    ("nfs-volume-without-path", True, mutate(lambda d: d[LIB][URLWF]["volumes"].insert(
        2, {"name": "wf-cache", "kind": "nfs", "server": "nfs.internal", "mountPath": "/mnt/workflow-cache", "readOnly": True}))),
    ("pvc-volume-with-sizelimit", True, mutate(lambda d: d[LIB][BAKED]["volumes"][0].update(sizeLimit="8Gi"))),
    ("k8s-secret-volume-without-secretname", True, mutate(lambda d: d[LIB][URLWF]["volumes"].insert(
        2, {"name": "wf-tokens", "kind": "k8s-secret", "mountPath": "/home/criteria/secrets", "readOnly": True}))),
    ("k8s-secret-volume-with-claim", True, mutate(lambda d: d[LIB][URLWF]["volumes"].insert(
        2, {"name": "wf-tokens", "kind": "k8s-secret", "secretName": "github-tokens", "claim": "criteria-data",
            "mountPath": "/home/criteria/secrets", "readOnly": True}))),
    ("pvc-volume-with-secretname", True, mutate(lambda d: d[LIB][BAKED]["volumes"][0].update(secretName="github-tokens"))),
    ("volume-unknown-kind", True, mutate(lambda d: d[LIB][BAKED]["volumes"][0].update(kind="hostPath"))),
    ("volume-relative-mountpath", True, mutate(lambda d: d[LIB][BAKED]["volumes"][0].update(mountPath="data"))),
    ("volume-unknown-field", True, mutate(lambda d: d[LIB][BAKED]["volumes"][0].update(hostPath="/srv"))),
    ("unknown-top-level-field", True, mutate(lambda d: d.update(workflowLibraryTypo={}))),
    ("unknown-route-field", True, mutate(lambda d: d["routes"][0].update(workflowRef="x"))),
    ("adapter-images-bad-kind", True, mutate(lambda d: d[LIB][BAKED].update(
        adapterImages={"Shell": "localhost:5000/criteria-adapter-shell:k8s-0.5.4-2"}))),
    ("adapter-images-blank-ref", True, mutate(lambda d: d[LIB][BAKED].update(
        adapterImages={"shell": " "}))),
    ("route-empty-states", True, mutate(lambda d: d["routes"][0].update(states=[]))),
    ("route-null-states", True, mutate(lambda d: d["routes"][0].update(states=None))),
    ("route-duplicate-states", True, mutate(lambda d: d["routes"][0].update(states=["Triage", "Triage"]))),
    ("route-bad-tagmatch", True, mutate(lambda d: d["routes"][0].update(tagMatch="some"))),
    ("route-dangling-workflow", False, mutate(lambda d: d["routes"][0].update(workflow="does-not-exist"))),
    ("bad-namespace", True, mutate(lambda d: d[LIB][BAKED].update(namespace="Criteria Jobs"))),
    ("secret-unknown-field", True, mutate(lambda d: d[LIB][BAKED]["secrets"][0].update(secretKeyRef="linear"))),
    ("secret-env-value-with-slash", True, mutate(lambda d: d[LIB][BAKED]["secrets"][0]["env"].update(
        LINEAR_API_KEY="criteria/data/linear"))),
    ("volume-env-value-not-string", True, mutate(lambda d: d[LIB][BAKED]["volumes"][0]["env"].update(
        CRITERIA_RUN_DIR_ROOT=42))),
]

# KB-103: configLibrary fail-closed variants.


def kb103_set_unknown_field(d):
    d["configLibrary"]["acme-repo"] = {"image": "repo-image:latest"}


def kb103_set_blank_cmd(d):
    d["configLibrary"]["acme-repo"] = {"buildCmd": "   "}


def kb103_add_bad_key(d):
    d["configLibrary"]["Acme Repo"] = {"buildCmd": "scripts/build.sh"}


def kb103_dangling_configref(d):
    d["routes"][0].update(configRef="other-repo")


def kb103_bad_configref_label(d):
    d["routes"][0].update(configRef="Acme Repo")


negative.extend([
    ("config-unknown-field", True, mutate(kb103_set_unknown_field)),
    ("config-blank-cmd", True, mutate(kb103_set_blank_cmd)),
    ("config-bad-key-name", True, mutate(kb103_add_bad_key)),
    ("route-dangling-configref", False, mutate(kb103_dangling_configref)),
    ("route-bad-configref-label", True, mutate(kb103_bad_configref_label)),
])

# KB-154: tag-group fail-closed variants.
def kb154_bad_tagmatch(d):
    d["configLibrary"]["acme-repo-v060"] = {"baseBranch": "v0.6.0-release", "tags": ["v0.6.0"], "tagMatch": "some"}


def kb154_empty_tags(d):
    d["configLibrary"]["acme-repo-v060"] = {"baseBranch": "v0.6.0-release", "tags": []}


def kb154_nonlist_tags(d):
    d["configLibrary"]["acme-repo-v060"] = {"baseBranch": "v0.6.0-release", "tags": "v0.6.0"}


def kb154_blank_tag_name(d):
    d["configLibrary"]["acme-repo-v060"] = {"baseBranch": "v0.6.0-release", "tags": ["   "]}


def kb154_blank_basebranch(d):
    d["configLibrary"]["acme-repo-v060"] = {"baseBranch": "   "}


negative.extend([
    ("config-bad-tagmatch", True, mutate(kb154_bad_tagmatch)),
    ("config-empty-tags", True, mutate(kb154_empty_tags)),
    ("config-nonlist-tags", True, mutate(kb154_nonlist_tags)),
    ("config-blank-tag-name", True, mutate(kb154_blank_tag_name)),
    ("config-blank-basebranch", True, mutate(kb154_blank_basebranch)),
])

# KB-310 (ADR 307): interface fail-closed variants. A type=url workflow
# without an interface, an interface with an unknown field, a mount with a
# bad kind, a slot with a bad name, and an image workflow declaring an
# interface all reject — the declaration is the admission contract and must
# be shape-complete before the operator trusts it.
def kb310_drop_interface(d):
    d[LIB][URLWF].pop("interface")


def kb310_interface_unknown_field(d):
    d[LIB][URLWF]["interface"]["slots"] = []


def kb310_mount_bad_kind(d):
    d[LIB][URLWF]["interface"]["mounts"][0]["kind"] = "hostPath"


def kb310_slot_bad_name(d):
    d[LIB][URLWF]["interface"]["configSlots"].append(
        {"slot": "BaseBranch", "variable": "base_branch", "target": "branch_manager/checkout"})


def kb310_slot_unknown_field(d):
    d[LIB][URLWF]["interface"]["configSlots"][0]["default"] = "main"


def kb310_image_with_interface(d):
    d[LIB][BAKED]["interface"] = {"apiVersion": "criteria.brokenbots.dev/v1", "kind": "WorkflowInterface", "workflow": BAKED}


negative.extend([
    ("url-workflow-missing-interface", True, mutate(kb310_drop_interface)),
    ("interface-unknown-field", True, mutate(kb310_interface_unknown_field)),
    ("interface-mount-bad-kind", True, mutate(kb310_mount_bad_kind)),
    ("interface-slot-bad-name", True, mutate(kb310_slot_bad_name)),
    ("interface-slot-unknown-field", True, mutate(kb310_slot_unknown_field)),
    ("image-workflow-with-interface", True, mutate(kb310_image_with_interface)),
])

# KB-311 (ADR 307): project-registry fail-closed variants. The registry is
# the ticket->project routing table, so a bad entry poisons every ticket it
# routes: unknown fields, namespace labels, repo refs, the cutover
# vocabulary, and routes selecting projects the registry does not carry all
# reject. A pre-cutover entry (no cutover marker) stays legal —
# binding-first, deletion second.
def kb311_registry_unknown_field(d):
    d["projectRegistry"]["Criteria Go Dev"]["binding"] = {}


def kb311_registry_bad_namespace(d):
    d["projectRegistry"]["Criteria Go Dev"]["namespace"] = "Bad_NS"


def kb311_registry_bad_repo(d):
    d["projectRegistry"]["Criteria Go Dev"]["repos"] = ["missing-slash"]


def kb311_registry_bad_cutover(d):
    d["projectRegistry"]["Criteria Go Dev"]["cutover"] = "half done"


def kb311_registry_blank_project(d):
    d["projectRegistry"]["   "] = {"namespace": "demo-ns"}


def kb311_registry_missing_namespace(d):
    d["projectRegistry"]["Criteria Go Dev"].pop("namespace")


def kb311_route_unregistered_project(d):
    d["routes"][0]["project"] = "Unregistered Project"


def kb311_registry_entry_without_cutover(d):
    d["projectRegistry"]["Criteria Go Dev"].pop("cutover")


negative.extend([
    ("registry-unknown-field", True, mutate(kb311_registry_unknown_field)),
    ("registry-bad-namespace", True, mutate(kb311_registry_bad_namespace)),
    ("registry-bad-repo", True, mutate(kb311_registry_bad_repo)),
    ("registry-bad-cutover", True, mutate(kb311_registry_bad_cutover)),
    ("registry-blank-project", True, mutate(kb311_registry_blank_project)),
    ("registry-missing-namespace", True, mutate(kb311_registry_missing_namespace)),
    ("route-unregistered-project", False, mutate(kb311_route_unregistered_project)),
])

positive.extend([
    ("registry-entry-without-cutover", mutate(kb311_registry_entry_without_cutover)),
])

# KB-313 (ADR 307): two-revision gate on the global run-config surfaces
# (volumes/secrets/env/adapterImages). While the migration window is open —
# at least one registry entry without the cutover: done marker — the
# surfaces stay legal on workflowLibrary entries (that is the shipped
# example itself); post-cutover they are rejected, mirroring
# criteria-k8s/internal/routes' ValidationError. namespace is OPTIONAL on
# workflowLibrary entries: cutover-done projects run in their project
# binding's namespace, so the global CM stops owning run placement.
def kb313_all_done(d):
    for entry in d["projectRegistry"].values():
        entry["cutover"] = "done"


def kb313_only_env_remains(d):
    # the shipped payload carries no top-level env on workflowLibrary
    # entries, so "env remains" means one entry gains one
    kb313_all_done(d)
    d[LIB][URLWF]["env"] = {"RUNNER_NOTE": "stale"}


def kb313_leave_only_adapterimages(d):
    kb313_all_done(d)
    for workflow in d[LIB].values():
        workflow.pop("env", None)
        workflow.pop("volumes", None)
        workflow.pop("secrets", None)


def kb313_leave_only_adapterimages(d):
    kb313_all_done(d)
    for workflow in d[LIB].values():
        workflow.pop("env", None)
        workflow.pop("volumes", None)
        workflow.pop("secrets", None)


def kb313_postcutover_clean(d):
    kb313_all_done(d)
    for workflow in d[LIB].values():
        workflow.pop("env", None)
        workflow.pop("adapterImages", None)
        workflow.pop("volumes", None)
        workflow.pop("secrets", None)


def kb313_namespace_optional(d):
    d[LIB][URLWF].pop("namespace")


def kb313_window_half_done(d):
    # "Criteria Go Dev" keeps its done marker; the rest stay un-cut — the
    # window is open, so the payload-wide stale surfaces stay legal.
    for project, entry in d["projectRegistry"].items():
        if project != "Criteria Go Dev":
            entry.pop("cutover", None)


negative.extend([
    ("postcutover-stale-global-surfaces", True, mutate(kb313_all_done)),
    ("postcutover-stale-env-remains", True, mutate(kb313_only_env_remains)),
    ("postcutover-stale-adapterimages-remains", True, mutate(kb313_leave_only_adapterimages)),
])

positive.extend([
    ("postcutover-clean", mutate(kb313_postcutover_clean)),
    ("namespace-optional-workflow-entry", mutate(kb313_namespace_optional)),
    ("migration-window-half-done-registry", mutate(kb313_window_half_done)),
])

failures = []


def check(label, doc, expect_ok, schema_fails=None):
    errs = validate(doc)
    if expect_ok and errs:
        failures.append(f"{label}: expected valid, structural validator reported: {'; '.join(errs)}")
    if not expect_ok and not errs:
        failures.append(f"{label}: expected invalid, structural validator accepted it")


for label, doc in positive:
    check(label, doc, True)
for label, schema_fails, doc in negative:
    check(label, doc, False)

# Secret-free scan: no string in the payload may look like credential material.
CRED_PATTERNS = [
    re.compile(r"(?i)(ghp|gho|ghu|ghs|ghr|github_pat)_[A-Za-z0-9_-]{16,}"),
    re.compile(r"(?i)\bsk-[A-Za-z0-9_-]{20,}"),
    re.compile(r"-----BEGIN"),
    re.compile(r"(?i)\b(AKIA|ASIA)[0-9A-Z]{16}\b"),
    re.compile(r"(?i)\bbearer\s+[A-Za-z0-9._-]{20,}"),
    re.compile(r"(?i)(password|passwd|api[_-]?key|private[_-]?key)\s*[:=]\s*\S+"),
]


def walk_strings(node, where, hits):
    if isinstance(node, str):
        for pattern in CRED_PATTERNS:
            if pattern.search(node):
                hits.append(f"{where}: looks like credential material")
                return
        if len(node) >= 32 and re.search(r"[A-Z]", node) and re.search(r"[a-z]", node) \
                and re.search(r"[0-9]", node) and " " not in node and "/" not in node \
                and ":" not in node and "-" not in node and "." not in node and "_" not in node:
            hits.append(f"{where}: opaque high-entropy value")
    elif isinstance(node, dict):
        for key, value in node.items():
            walk_strings(key, where, hits)
            walk_strings(value, f"{where}.{key}", hits)
    elif isinstance(node, list):
        for i, value in enumerate(node):
            walk_strings(value, f"{where}[{i}]", hits)


cred_hits = []
walk_strings(base, "payload", cred_hits)
if cred_hits:
    failures.append(f"example payload is not secret-free: {'; '.join(cred_hits)}")

# -------------------------------------------------- KB-281: shipped example
# shape — the linear estate (the criteria project's split triage/develop
# routes firing the linear_* trees) is RETIRED: the criteria project is
# cleanup-only, the kanboard routes cover their own watcher's project and
# states with full ADR-0005 D7 commit pins, and the retired linear-* url
# objects must not return.
FULL_SHA = re.compile(r"^[0-9a-f]{40}$")


def shipped_err(cond, message):
    if not cond:
        failures.append(message)


shipped_err(len(base["routes"]) == 10,
            f"shipped example routes = {len(base['routes'])}, want the CRI-310 cleanup route, "
            "the two kanboard routes plus the KB-311 acceptance pair (kanboard-develop-go/"
            "kanboard-develop-ts: two projects, same tree, different bindings), and the "
            "KB-277/KB-278/KB-279/KB-280/KB-276 harness legs "
            "(contracts/composition/failures/fleet-smoke/wire; the criteria split routes are retired, KB-281)")
route_names = {r.get("name") for r in base["routes"] if isinstance(r, dict)}
shipped_err(route_names == {"criteria-cleanup", "kanboard-triage", "kanboard-develop",
                            "kanboard-develop-go", "kanboard-develop-ts",
                            "harness-contracts", "harness-composition", "harness-failures",
                            "harness-fleet-smoke", "harness-wire"},
            f"shipped example route names = {sorted(route_names)}, want cleanup + kanboard pair"
            " + KB-311 acceptance pair + harness-contracts + harness-composition"
            " + harness-failures + harness-fleet-smoke + harness-wire")
for retired_route in ("criteria-triage", "criteria-develop"):
    shipped_err(retired_route not in route_names,
                f"shipped example: the retired {retired_route} route returned (KB-281)")
cleanup_route = next((r for r in base["routes"] if r.get("name") == "criteria-cleanup"), None)
kanboard_triage_route = next((r for r in base["routes"] if r.get("name") == "kanboard-triage"), None)
kanboard_develop_route = next((r for r in base["routes"] if r.get("name") == "kanboard-develop"), None)
harness_contracts_route = next((r for r in base["routes"] if r.get("name") == "harness-contracts"), None)
harness_composition_route = next((r for r in base["routes"] if r.get("name") == "harness-composition"), None)
harness_failures_route = next((r for r in base["routes"] if r.get("name") == "harness-failures"), None)
harness_fleet_smoke_route = next((r for r in base["routes"] if r.get("name") == "harness-fleet-smoke"), None)
harness_wire_route = next((r for r in base["routes"] if r.get("name") == "harness-wire"), None)
shipped_err(cleanup_route is not None, "shipped example: routes missing the criteria-cleanup route (CRI-310)")
shipped_err(kanboard_triage_route is not None,
            "shipped example: routes missing the kanboard-triage route (dual-source coexistence)")
shipped_err(kanboard_develop_route is not None,
            "shipped example: routes missing the kanboard-develop route (dual-source coexistence)")
shipped_err(all(r.get("project") == "Criteria K8s Workflow Runner"
                for r in base["routes"] if r.get("name", "").startswith("criteria-")),
            "shipped example: every criteria-* route must target the criteria project")

# KB-281 absence regressions: the retired linear url objects and any route
# firing them must not return.
for retired_wf in ("linear-intake-url", "linear-triage-url", "linear-develop-url"):
    shipped_err(retired_wf not in base[LIB],
                f"shipped example: workflowLibrary still carries the retired {retired_wf} object (KB-281)")
for r in base["routes"]:
    wf_name = r.get("workflow")
    shipped_err(not str(wf_name).startswith("linear-"),
                f"shipped example: route {r.get('name')} still fires the retired linear workflow {wf_name!r} (KB-281)")

# KB-103: the shipped example carries a per-repo configLibrary — but the
# configRef binding lived on the retired criteria-develop route, so no
# surviving route binds a config entry today.
config_library = base.get("configLibrary")
shipped_err(isinstance(config_library, dict) and bool(config_library),
            "shipped example: configLibrary must be a non-empty object")
if isinstance(config_library, dict):
    for name, entry in config_library.items():
        validate_config(name, entry, f"shipped example: configLibrary.{name}", failures)
shipped_err(all("configRef" not in r for r in base["routes"]),
            "shipped example: a surviving route still binds configRef (KB-103's binding rode the retired criteria-develop route)")

# CRI-310 (validation run C): the dirty label is a routing surface. The
# cleanup route selects on the watcher's sticky criteria-dirty tag for the
# criteria project's Ready for Development state. (KB-281: the tag-less
# general develop route it used to out-prioritize is retired, so a
# dirty-label-free ticket matches nothing and the watcher fails closed.)
if cleanup_route is not None:
    if cleanup_route.get("workflow") != "ticket-cleanup-url":
        failures.append("shipped example: criteria-cleanup must fire ticket-cleanup-url")
    if cleanup_route.get("tags") != ["criteria-dirty"]:
        failures.append(f"shipped example: criteria-cleanup tags = {cleanup_route.get('tags')}, want [criteria-dirty]")
    if cleanup_route.get("states") != ["Ready for Development"]:
        failures.append(
            f"shipped example: criteria-cleanup states = {cleanup_route.get('states')}, want [Ready for Development]"
        )
    if "tagMatch" in cleanup_route:
        failures.append("shipped example: criteria-cleanup must keep the tagMatch=all default")

# Dual-source coexistence: the kanboard routes fire their own watcher's
# project and states (Kanboard Tickets / Backlog and Ready), url-only, with
# the routes-test asserting the library objects elsewhere.
for route, wf_name, states in (
        (kanboard_triage_route, "kanboard-triage-url", ["Backlog"]),
        (kanboard_develop_route, "kanboard-develop-url", ["Ready"]),
):
    if route is None:
        continue
    if route.get("workflow") != wf_name:
        failures.append(f"shipped example: {route.get('name')} must fire {wf_name}")
    if route.get("project") != "Kanboard Tickets":
        failures.append(f"shipped example: {route.get('name')} must target the Kanboard Tickets project")
    if route.get("states") != states:
        failures.append(f"shipped example: {route.get('name')} states = {route.get('states')}, want {states}")

# KB-277 (series ARMED): the harness-contracts route fires the KB-254 harness
# series' contracts leg ON DAVE'S GO within the green-run plan. Its selector
# must never overlap the plain development lanes: tagMatch=all on
# harness-validation + harness-contracts (criteria-cleanup selects the same
# state but the different criteria-dirty tag; the kanboard routes select the
# kanboard project), so an untagged development card matches nothing here.
if harness_contracts_route is None:
    failures.append("shipped example: routes missing the harness-contracts route (KB-277)")
else:
    if harness_contracts_route.get("workflow") != "harness-contracts-url":
        failures.append("shipped example: harness-contracts must fire harness-contracts-url")
    if harness_contracts_route.get("project") != "Criteria K8s Workflow Runner":
        failures.append("shipped example: harness-contracts must target the criteria project")
    if harness_contracts_route.get("tags") != ["harness-validation", "harness-contracts"]:
        failures.append(f"shipped example: harness-contracts tags = {harness_contracts_route.get('tags')}, "
                        "want [harness-validation, harness-contracts] (the dave-go tag gate)")
    if harness_contracts_route.get("states") != ["Ready for Development"]:
        failures.append(f"shipped example: harness-contracts states = {harness_contracts_route.get('states')}, "
                        "want [Ready for Development]")
    if "tagMatch" in harness_contracts_route:
        failures.append("shipped example: harness-contracts must keep the tagMatch=all default")
# KB-276 (series ARMED): the harness-wire route fires the KB-254 harness
# series' wire leg ON DAVE'S GO within the green-run plan. Its selector must
# never overlap the plain development lanes: tagMatch=all on
# harness-validation + harness-wire (criteria-cleanup selects the same state
# but the different criteria-dirty tag; the kanboard routes select the
# kanboard project), so an untagged development card matches nothing here.
if harness_wire_route is None:
    failures.append("shipped example: routes missing the harness-wire route (KB-276)")
else:
    if harness_wire_route.get("workflow") != "harness-wire-url":
        failures.append("shipped example: harness-wire must fire harness-wire-url")
    if harness_wire_route.get("project") != "Criteria K8s Workflow Runner":
        failures.append("shipped example: harness-wire must target the criteria project")
    if harness_wire_route.get("tags") != ["harness-validation", "harness-wire"]:
        failures.append(f"shipped example: harness-wire tags = {harness_wire_route.get('tags')}, "
                        "want [harness-validation, harness-wire] (the dave-go tag gate)")
    if harness_wire_route.get("states") != ["Ready for Development"]:
        failures.append(f"shipped example: harness-wire states = {harness_wire_route.get('states')}, "
                        "want [Ready for Development]")
    if "tagMatch" in harness_wire_route:
        failures.append("shipped example: harness-wire must keep the tagMatch=all default")

# (KB-281: the linear-triage-url / linear-develop-url object assertions are
# gone with the objects themselves — the absence regressions above fail fast
# if the retired linear estate ever resurfaces in the example payload.)

# KB-278 (series ARMED): the harness-composition route fires the KB-254
# harness series' composition leg (harness_composition_v1) with the same
# gating discipline as the contracts leg: tagMatch=all on harness-validation
# + harness-composition over the criteria project's Ready for Development
# state — never overlapping the plain development lanes.
if harness_composition_route is None:
    failures.append("shipped example: routes missing the harness-composition route (KB-278)")
else:
    if harness_composition_route.get("workflow") != "harness-composition-url":
        failures.append("shipped example: harness-composition must fire harness-composition-url")
    if harness_composition_route.get("project") != "Criteria K8s Workflow Runner":
        failures.append("shipped example: harness-composition must target the criteria project")
    if harness_composition_route.get("tags") != ["harness-validation", "harness-composition"]:
        failures.append(f"shipped example: harness-composition tags = {harness_composition_route.get('tags')}, "
                        "want [harness-validation, harness-composition] (the dave-go tag gate)")
    if harness_composition_route.get("states") != ["Ready for Development"]:
        failures.append(f"shipped example: harness-composition states = {harness_composition_route.get('states')}, "
                        "want [Ready for Development]")
    if "tagMatch" in harness_composition_route:
        failures.append("shipped example: harness-composition must keep the tagMatch=all default")

# KB-279 (series leg armed on dave's go): the harness-failures route fires
# the KB-254 harness series' failures leg (harness_failures_v1) with the
# same gating discipline as the composition leg: tagMatch=all on
# harness-validation + harness-failures over the criteria project's Ready
# for Development state.
if harness_failures_route is None:
    failures.append("shipped example: routes missing the harness-failures route (KB-279)")
else:
    if harness_failures_route.get("workflow") != "harness-failures-url":
        failures.append("shipped example: harness-failures must fire harness-failures-url")
    if harness_failures_route.get("project") != "Criteria K8s Workflow Runner":
        failures.append("shipped example: harness-failures must target the criteria project")
    if harness_failures_route.get("tags") != ["harness-validation", "harness-failures"]:
        failures.append(f"shipped example: harness-failures tags = {harness_failures_route.get('tags')}, "
                        "want [harness-validation, harness-failures] (the dave-go tag gate)")
    if harness_failures_route.get("states") != ["Ready for Development"]:
        failures.append(f"shipped example: harness-failures states = {harness_failures_route.get('states')}, "
                        "want [Ready for Development]")
    if "tagMatch" in harness_failures_route:
        failures.append("shipped example: harness-failures must keep the tagMatch=all default")

# KB-280 (the fast-feedback canary): the harness-fleet-smoke route fires the
# series' fleet-smoke leg (harness_fleet_smoke_v1 — the ~90s deterministic
# pre-leg gate) with the same gating discipline as the other harness legs:
# tagMatch=all on harness-validation + harness-fleet-smoke over the criteria
# project's Ready for Development state.
if harness_fleet_smoke_route is None:
    failures.append("shipped example: routes missing the harness-fleet-smoke route (KB-280)")
else:
    if harness_fleet_smoke_route.get("workflow") != "harness-fleet-smoke-url":
        failures.append("shipped example: harness-fleet-smoke must fire harness-fleet-smoke-url")
    if harness_fleet_smoke_route.get("project") != "Criteria K8s Workflow Runner":
        failures.append("shipped example: harness-fleet-smoke must target the criteria project")
    if harness_fleet_smoke_route.get("tags") != ["harness-validation", "harness-fleet-smoke"]:
        failures.append(f"shipped example: harness-fleet-smoke tags = {harness_fleet_smoke_route.get('tags')}, "
                        "want [harness-validation, harness-fleet-smoke] (the dave-go tag gate)")
    if harness_fleet_smoke_route.get("states") != ["Ready for Development"]:
        failures.append(f"shipped example: harness-fleet-smoke states = {harness_fleet_smoke_route.get('states')}, "
                        "want [Ready for Development]")
    if "tagMatch" in harness_fleet_smoke_route:
        failures.append("shipped example: harness-fleet-smoke must keep the tagMatch=all default")

# CRI-310: ticket-cleanup-url — the dirty-label cleanup workflow object
# (url-only, on criteria-base). Validation run C corrected two assumptions
# from the original landing: (1) the pinless fetch fails to resolve the
# subtree ("resolve git ref HEAD ... exit status 128"), so the object pins
# to the tree's squash-merge commit a84ef55 per the CRI-241/CRI-243
# follow-up convention; (2) the criteria-base entrypoint unconditionally
# requires WORKFLOW_GITHUB_TOKEN via /home/criteria/secrets (repo-clone
# init rejects the pod without it), so the object must bind the
# github-tokens CSI surface even though the cleanup pass itself performs
# zero git operations. linear-api-key rides at
# /home/criteria/linear-secrets — the runner script's hardcoded file:
# OriginRef path (lstat-checked before any step executes).
cleanup_wf = base[LIB].get("ticket-cleanup-url")
if not isinstance(cleanup_wf, dict):
    failures.append("shipped example: workflowLibrary missing the ticket-cleanup-url object (CRI-310)")
else:
    if cleanup_wf.get("type") != "url" or "url" not in cleanup_wf:
        failures.append("shipped example: ticket-cleanup-url must be a url workflow")
    if not str(cleanup_wf.get("url", "")).startswith(
            "git::https://github.com/brokenbots/workflow-example.git//ticket_cleanup_v1"):
        failures.append("shipped example: ticket-cleanup-url must point at the ticket_cleanup_v1 subtree")
    if not cleanup_wf.get("ref"):
        failures.append(
            "shipped example: ticket-cleanup-url must pin the ticket_cleanup_v1 squash-merge commit"
            " (CRI-241/CRI-243 follow-up re-pin convention; pinless HEAD resolution fails on the subtree)"
        )
    if cleanup_wf.get("class") != "triage":
        failures.append("shipped example: ticket-cleanup-url must declare class=triage (CRI-242 read-only admission)")
    if "image" in cleanup_wf:
        failures.append("shipped example: ticket-cleanup-url must be url-only (no process image)")
    cleanup_secrets = cleanup_wf.get("secrets")
    names = [s.get("name") for s in cleanup_secrets] if isinstance(cleanup_secrets, list) else []
    if set(cleanup_secrets and [s.get("name") for s in cleanup_secrets] or []) != {"linear-api-key", "github-tokens"}:
        failures.append("shipped example: ticket-cleanup-url must bind exactly linear-api-key + github-tokens"
                        " (the base entrypoint requires WORKFLOW_GITHUB_TOKEN; validation run C)")
    for s in cleanup_secrets if isinstance(cleanup_secrets, list) else []:
        if s.get("name") == "linear-api-key" and s.get("mountPath") != "/home/criteria/linear-secrets":
            failures.append("shipped example: ticket-cleanup-url linear-api-key mountPath must be"
                            " /home/criteria/linear-secrets (the runner's file: OriginRef path)")

# KB-310 (ADR 307): the shipped example declares the machine-readable
# workflow interface. Every type=url object embeds the byte-identical
# interface.json of the tree its url resolves to (the operator stamps it
# onto the run CR and validates the project binding against it BEFORE job
# creation), type=image objects carry none, and the shipped configLibrary /
# secret env surfaces stay within what the referenced trees honor — the
# example must admit, fail closed must not fire on the golden payload, and
# a trim (unknown-slot regression) breaks the test before a run does.
url_interfaces = []
for wf_name, wf in base[LIB].items():
    if wf.get("type") != "url":
        if "interface" in wf:
            failures.append(f"shipped example: {wf_name} is type=image and must not declare interface")
        continue
    iface = wf.get("interface")
    if not isinstance(iface, dict):
        failures.append(f"shipped example: {wf_name} must embed interface (KB-310)")
        continue
    m = re.search(r"\.git//([A-Za-z0-9_-]+)", wf.get("url", ""))
    if m is None:
        failures.append(f"shipped example: {wf_name} url does not resolve to a repo tree")
        continue
    tree = m.group(1)
    if iface.get("workflow") != tree:
        failures.append(f"shipped example: {wf_name} interface.workflow {iface.get('workflow')!r}"
                        f" must name the url's tree {tree!r}")
        continue
    tree_iface_path = os.path.join(repo_root, tree, "interface.json")
    if not os.path.exists(tree_iface_path):
        failures.append(f"shipped example: {wf_name} names tree {tree} but {tree}/interface.json"
                        " is missing from the repo")
        continue
    with open(tree_iface_path, encoding="utf-8") as fh:
        tree_iface = json.load(fh)
    if iface != tree_iface:
        failures.append(f"shipped example: {wf_name} embedded interface diverges from"
                        f" {tree}/interface.json (regenerate the copy; the stamps must match)")
    url_interfaces.append((wf_name, tree, iface))

# Spot checks: the tree decides the override surface, so the shipped
# configLibrary entries must stay within what the trees declaring them
# honor (the 4 platform-core keys map to tree variables; a key whose tree
# variable the tree does not declare is an unknown-slot binding).
develop_iface = next((i for n, t, i in url_interfaces if n == "kanboard-develop-url"), None)
if develop_iface is not None:
    honored = {s.get("slot") for s in develop_iface.get("configSlots", [])}
    if honored != {"baseBranch", "ciGateCmd"}:
        failures.append(f"shipped example: kanboard_develop_v1 honors {sorted(honored)},"
                        " want {baseBranch, ciGateCmd} — re-trim configLibrary when this changes")

cfg = base.get("configLibrary", {})
if "criteria-repo" in cfg:
    failures.append("shipped example: configLibrary.criteria-repo must not return (the"
                    " ticket_cleanup_v1 tree honors no config slots; the ciGateCmd entry"
                    " was an unknown-slot binding)")
for repo_name in ("castle", "workflow-example"):
    entry = cfg.get(repo_name)
    if entry is None:
        failures.append(f"shipped example: configLibrary missing {repo_name} (KB-270/KB-273)")
    else:
        extra = sorted(set(entry) - {"ciGateCmd"})
        if extra:
            failures.append(f"shipped example: configLibrary.{repo_name} declares {extra}"
                            " but kanboard_develop_v1 honors only ciGateCmd+baseBranch —"
                            " unknown-slot keys were trimmed (KB-310); never re-add them")
        if not entry.get("ciGateCmd"):
            failures.append(f"shipped example: configLibrary.{repo_name} must keep ciGateCmd (KB-273)")

# Secret env lanes: binding-side env surfaces must not exceed the trees'
# declared secretEnv lanes (fail-closed trim regressions).
triage_wf = base[LIB].get("kanboard-triage-url")
if isinstance(triage_wf, dict):
    for s in triage_wf.get("secrets", []):
        if s.get("name") == "github-tokens" and s.get("env") != {"WORKFLOW_GITHUB_TOKEN": "workflow_github_token"}:
            failures.append(f"shipped example: kanboard-triage-url github-tokens env ="
                            f" {s.get('env')!r}, want only WORKFLOW_GITHUB_TOKEN (the"
                            " kanboard_triage_v1 tree declares no reviewer lane)")
cleanup_wf = base[LIB].get("ticket-cleanup-url")
if isinstance(cleanup_wf, dict):
    for s in cleanup_wf.get("secrets", []):
        if s.get("name") == "github-tokens" and "env" in s:
            failures.append(f"shipped example: ticket-cleanup-url github-tokens must not map env"
                            f" keys ({sorted(s['env'])} — the ticket_cleanup_v1 tree declares no"
                            " secret env; the CSI lane mount stays for the base entrypoint)")

# KB-11 smoke regression: the kanboard dual-source objects carried no pin
# assertions here while their ref stayed at 308315c (the #94 re-pin) after
# the develop tree's getTaskTags fix (#106) landed — the develop route kept
# fetching the pre-fix tree and the run captured the -32602 "Too many
# arguments" error body as ticket.json's tags. Both objects now pin the
# current main tree carrying the #106 fix; the assertions below mirror the
# ticket-cleanup-url pin assertions (KB-281: the retired linear-triage-url /
# linear-develop-url blocks are gone — these are the subtree pin guards for
# all shipped kanboard objects) so the next subtree change
# fails here until its re-pin lands in the same change.
for kb_name, kb_subtree, kb_class, kb_pin in (
        ("kanboard-triage-url", "kanboard_triage_v1", "triage", "63535719e37750301a8b7cc9be2c74b2d18c847e"),
        ("kanboard-develop-url", "kanboard_develop_v1", None, "63535719e37750301a8b7cc9be2c74b2d18c847e"),
):
    kb_wf = base[LIB].get(kb_name)
    if not isinstance(kb_wf, dict):
        failures.append(f"shipped example: workflowLibrary missing the {kb_name} object (dual-source coexistence)")
        continue
    if not str(kb_wf.get("url", "")).startswith(
            f"git::https://github.com/brokenbots/workflow-example.git//{kb_subtree}?ref={kb_pin}"):
        failures.append(f"shipped example: {kb_name} must point at the {kb_subtree} subtree pinned to"
                        " the current main HEAD (KB-57c reviewer policy wave; re-pin convention: main HEAD):"
                        " the stale 308315c pin kept the pre-#106 fetch_ticket live)")
    if not FULL_SHA.match(kb_wf.get("ref") or ""):
        failures.append(f"shipped example: {kb_name} ref must be a pinned 40-hex commit SHA (D7)")
    if kb_wf.get("ref") != kb_pin:
        failures.append(f"shipped example: {kb_name} ref must pin the re-pin commit {kb_pin[:7]}"
                        " (any kanboard_* subtree change must re-pin in the same change)")
    if kb_wf.get("class") != kb_class:
        failures.append(f"shipped example: {kb_name} class = {kb_wf.get('class')}, want {kb_class!r}")
    if "image" in kb_wf:
        failures.append(f"shipped example: {kb_name} must be url-only (no process image)")

# KB-277: the harness_contracts_v1 tree object — url-only, class=triage,
# shipped PINLESS pre-close (the ADR-0005 D7 follow-up convention per
# decision-demo-url precedent: a subtree cannot pin its own landing commit,
# so KB-277 re-pins ref/?ref= to the squash-merge commit at the card's
# KB-230: adapter-image pin state. The shipped example is the deploy source
# for the live criteria-routes CM (make apply-routes applies it wholesale),
# so its adapterImages pins must agree three ways: example == RECORDED_PIN_SET
# (below) == live CM. The KB-216 fleet leg repinned live copilot-peer
# everywhere to k8s-0.5.16 (verified 2026-10-09) while this file lagged at
# 0.5.15 — exactly the drift class both legs below fail on. Version drift of
# one pin family is caught per kind present; the objects that declare no
# adapterImages (the retained linear-intake-v1 image object and the
# decision-demo-url placeholder, KB-205/KB-281) skip the check.
RECORDED_PIN_SET = {
    "shell": "localhost:5000/criteria-adapter-shell-peer:k8s-0.5.56-1",
    "copilot": "localhost:5000/criteria-adapter-copilot-peer:k8s-0.5.56-1",
}


def check_adapter_pins(where, library):
    for wf_name, wf_obj in library.items():
        images = wf_obj.get("adapterImages") if isinstance(wf_obj, dict) else None
        if not isinstance(images, dict):
            continue
        for kind, image in images.items():
            recorded = RECORDED_PIN_SET.get(kind)
            if recorded is None:
                failures.append(
                    f"{where}: {wf_name} adapterImages pins kind {kind!r} with no recorded pin"
                    " — record the fleet pin in RECORDED_PIN_SET in k8s/tests/test_routes_config.sh")
            elif image != recorded:
                failures.append(
                    f"{where}: {wf_name} adapterImages.{kind} = {image!r}, want the recorded pin"
                    f" {recorded!r} (three-way pin agreement: example == recorded pin set == live"
                    " criteria-routes CM — bump the whole pin family in the same change)")


check_adapter_pins("shipped example", base[LIB])
# KB-276: the harness_wire_v1 tree object — url-only, class=triage, shipped
# PINLESS pre-close (the ADR-0005 D7 follow-up convention per
# decision-demo-url precedent: a subtree cannot pin its own landing commit,
# so KB-276 re-pins ref/?ref= to the squash-merge commit at the card's
# close). The github-tokens binding is a name reference (CRI-214 section
# 3.8); the tree's only secret var is workflow_github_token, bound on the
# copilot member, and every other member stays secret-free. adapterImages
# pin the recorded fleet pin set verbatim (three-way pin agreement).
harness_contracts_wf = base[LIB].get("harness-contracts-url")
if not isinstance(harness_contracts_wf, dict):
    failures.append("shipped example: workflowLibrary missing the harness-contracts-url object (KB-277)")
else:
    if harness_contracts_wf.get("type") != "url" or "url" not in harness_contracts_wf:
        failures.append("shipped example: harness-contracts-url must be a url workflow")
    if not str(harness_contracts_wf.get("url", "")).startswith(
            "git::https://github.com/brokenbots/workflow-example.git//harness_contracts_v1"):
        failures.append("shipped example: harness-contracts-url must point at the harness_contracts_v1 subtree")
    if str(harness_contracts_wf.get("url", "")).endswith("?ref=") or not str(harness_contracts_wf.get("url", "")).endswith("//harness_contracts_v1"):
        failures.append("shipped example: harness-contracts-url must be url-only and PINLESS pre-close"
                        " (the KB-277 follow-up re-pins ref/?ref= to the squash-merge commit at card close)")
    if harness_contracts_wf.get("class") != "triage":
        failures.append("shipped example: harness-contracts-url must declare class=triage (CRI-242 read-only admission)")
    if "image" in harness_contracts_wf:
        failures.append("shipped example: harness-contracts-url must be url-only (no process image)")
    hc_secrets = harness_contracts_wf.get("secrets")
    hc_secret_names = [s.get("name") for s in hc_secrets] if isinstance(hc_secrets, list) else []
    if hc_secret_names != ["github-tokens"]:
        failures.append("shipped example: harness-contracts-url must bind exactly the github-tokens CSI surface"
                        " (the copilot member's workflow_github_token only; name references, CRI-214 section 3.8)")
    for s in hc_secrets if isinstance(hc_secrets, list) else []:
        if s.get("env") != {"WORKFLOW_GITHUB_TOKEN": "workflow_github_token"}:
            failures.append("shipped example: harness-contracts-url github-tokens env must bind"
                            " WORKFLOW_GITHUB_TOKEN=workflow_github_token (name reference only)")
    hc_volumes = harness_contracts_wf.get("volumes")
    hc_volume_names = [v.get("name") for v in hc_volumes] if isinstance(hc_volumes, list) else []
    if hc_volume_names != ["data", "repo", "scratch"]:
        failures.append(f"shipped example: harness-contracts-url volumes = {hc_volume_names},"
                        " want [data, repo, scratch] (the KB-241 peer-pod surface)")

# KB-278: the harness_composition_v1 tree object — url-only, class=triage,
# shipped PINLESS pre-close, same convention as the contracts leg (the
# KB-278 follow-up re-pins ref/?ref= to the squash-merge commit at the
# card's close). No binding secret: the tree interface declares no secret
# lanes (a shipped WORKFLOW_GITHUB_TOKEN lane would refuse admission as
# unknown-slot; the F36 secret-free root stands — KB-310 review), and the
# recorded fleet pin set verbatim, shared with the other harness legs.
harness_composition_wf = base[LIB].get("harness-composition-url")
if not isinstance(harness_composition_wf, dict):
    failures.append("shipped example: workflowLibrary missing the harness-composition-url object (KB-278)")
else:
    if harness_composition_wf.get("type") != "url" or "url" not in harness_composition_wf:
        failures.append("shipped example: harness-composition-url must be a url workflow")
    if not str(harness_composition_wf.get("url", "")).startswith(
            "git::https://github.com/brokenbots/workflow-example.git//harness_composition_v1"):
        failures.append("shipped example: harness-composition-url must point at the harness_composition_v1 subtree")
    if str(harness_composition_wf.get("url", "")).endswith("?ref=") or not str(harness_composition_wf.get("url", "")).endswith("//harness_composition_v1"):
        failures.append("shipped example: harness-composition-url must be url-only and PINLESS pre-close"
                        " (the KB-278 follow-up re-pins ref/?ref= to the squash-merge commit at card close)")
    if harness_composition_wf.get("class") != "triage":
        failures.append("shipped example: harness-composition-url must declare class=triage (CRI-242 read-only admission)")
    if "image" in harness_composition_wf:
        failures.append("shipped example: harness-composition-url must be url-only (no process image)")
    hcomp_secrets = harness_composition_wf.get("secrets")
    hcomp_secret_names = [s.get("name") for s in hcomp_secrets] if isinstance(hcomp_secrets, list) else []
    if hcomp_secret_names != []:
        failures.append(f"shipped example: harness-composition-url must bind no secret"
                        f" (the tree's interface declares no secret lanes; a"
                        f" WORKFLOW_GITHUB_TOKEN binding is undeclared there and rejects"
                        f" admission as unknown-slot — KB-310 review, got {hcomp_secret_names})")
    for s in hcomp_secrets if isinstance(hcomp_secrets, list) else []:
        if "env" in s:
            failures.append("shipped example: harness-composition-url binds no secret lane,"
                            " so no secret may carry an env map (KB-310 review)")
    hcomp_volumes = harness_composition_wf.get("volumes")
    hcomp_volume_names = [v.get("name") for v in hcomp_volumes] if isinstance(hcomp_volumes, list) else []
    if hcomp_volume_names != ["data", "repo", "scratch"]:
        failures.append(f"shipped example: harness-composition-url volumes = {hcomp_volume_names},"
                        " want [data, repo, scratch] (the KB-241 peer-pod surface)")

# KB-279: the harness_failures_v1 tree object — url-only, class=triage,
# shipped PINLESS pre-close (same convention as the composition leg; the
# KB-279 follow-up re-pins ref/?ref= to the squash-merge commit at the
# card's close). No binding secret: the tree interface declares no secret
# lanes (a shipped WORKFLOW_GITHUB_TOKEN lane would refuse admission as
# unknown-slot; the F36 secret-free root stands — KB-310 review), and the
# recorded fleet pin set verbatim, shared with the other harness legs.
harness_failures_wf = base[LIB].get("harness-failures-url")
if not isinstance(harness_failures_wf, dict):
    failures.append("shipped example: workflowLibrary missing the harness-failures-url object (KB-279)")
else:
    if harness_failures_wf.get("type") != "url" or "url" not in harness_failures_wf:
        failures.append("shipped example: harness-failures-url must be a url workflow")
    if not str(harness_failures_wf.get("url", "")).startswith(
            "git::https://github.com/brokenbots/workflow-example.git//harness_failures_v1"):
        failures.append("shipped example: harness-failures-url must point at the harness_failures_v1 subtree")
    if str(harness_failures_wf.get("url", "")).endswith("?ref=") or not str(harness_failures_wf.get("url", "")).endswith("//harness_failures_v1"):
        failures.append("shipped example: harness-failures-url must be url-only and PINLESS pre-close"
                        " (the KB-279 follow-up re-pins ref/?ref= to the squash-merge commit at card close)")
    if harness_failures_wf.get("class") != "triage":
        failures.append("shipped example: harness-failures-url must declare class=triage (CRI-242 read-only admission)")
    if "image" in harness_failures_wf:
        failures.append("shipped example: harness-failures-url must be url-only (no process image)")
    hfail_secrets = harness_failures_wf.get("secrets")
    hfail_secret_names = [s.get("name") for s in hfail_secrets] if isinstance(hfail_secrets, list) else []
    if hfail_secret_names != []:
        failures.append(f"shipped example: harness-failures-url must bind no secret"
                        f" (the tree's interface declares no secret lanes; a"
                        f" WORKFLOW_GITHUB_TOKEN binding is undeclared there and rejects"
                        f" admission as unknown-slot — KB-310 review, got {hfail_secret_names})")
    for s in hfail_secrets if isinstance(hfail_secrets, list) else []:
        if "env" in s:
            failures.append("shipped example: harness-failures-url binds no secret lane,"
                            " so no secret may carry an env map (KB-310 review)")
    hfail_volumes = harness_failures_wf.get("volumes")
    hfail_volume_names = [v.get("name") for v in hfail_volumes] if isinstance(hfail_volumes, list) else []
    if hfail_volume_names != ["data", "repo", "scratch"]:
        failures.append(f"shipped example: harness-failures-url volumes = {hfail_volume_names},"
                        " want [data, repo, scratch] (the KB-241 peer-pod surface)")

# KB-280: the harness_fleet_smoke_v1 tree object — url-only, class=triage,
# shipped PINLESS pre-close (same convention as the other harness legs). The
# canary is the no-agent slice: no copilot member, so adapterImages is
# SHELL-ONLY, and it binds no secret: the tree interface declares no secret
# lanes (a shipped WORKFLOW_GITHUB_TOKEN lane would refuse admission as
# unknown-slot; the F36 secret-free root stands — KB-310 review).
harness_fleet_smoke_wf = base[LIB].get("harness-fleet-smoke-url")
if not isinstance(harness_fleet_smoke_wf, dict):
    failures.append("shipped example: workflowLibrary missing the harness-fleet-smoke-url object (KB-280)")
else:
    if harness_fleet_smoke_wf.get("type") != "url" or "url" not in harness_fleet_smoke_wf:
        failures.append("shipped example: harness-fleet-smoke-url must be a url workflow")
    if not str(harness_fleet_smoke_wf.get("url", "")).startswith(
            "git::https://github.com/brokenbots/workflow-example.git//harness_fleet_smoke_v1"):
        failures.append("shipped example: harness-fleet-smoke-url must point at the harness_fleet_smoke_v1 subtree")
    if str(harness_fleet_smoke_wf.get("url", "")).endswith("?ref=") or not str(harness_fleet_smoke_wf.get("url", "")).endswith("//harness_fleet_smoke_v1"):
        failures.append("shipped example: harness-fleet-smoke-url must be url-only and PINLESS pre-close")
    if harness_fleet_smoke_wf.get("class") != "triage":
        failures.append("shipped example: harness-fleet-smoke-url must declare class=triage (CRI-242 read-only admission)")
    if "image" in harness_fleet_smoke_wf:
        failures.append("shipped example: harness-fleet-smoke-url must be url-only (no process image)")
    hfsm_images = harness_fleet_smoke_wf.get("adapterImages")
    if not isinstance(hfsm_images, dict) or set(hfsm_images.keys()) != {"shell"}:
        failures.append("shipped example: harness-fleet-smoke-url must pin the Shell adapter ONLY"
                        " (the canary is the no-agent slice — no copilot identity to image, F36)")
    hfsm_secrets = harness_fleet_smoke_wf.get("secrets")
    hfsm_secret_names = [s.get("name") for s in hfsm_secrets] if isinstance(hfsm_secrets, list) else []
    if hfsm_secret_names != []:
        failures.append(f"shipped example: harness-fleet-smoke-url must bind no secret"
                        f" (the tree's interface declares no secret lanes; a"
                        f" WORKFLOW_GITHUB_TOKEN binding is undeclared there and rejects"
                        f" admission as unknown-slot — KB-310 review, got {hfsm_secret_names})")
    for s in hfsm_secrets if isinstance(hfsm_secrets, list) else []:
        if "env" in s:
            failures.append("shipped example: harness-fleet-smoke-url binds no secret lane,"
                            " so no secret may carry an env map (KB-310 review)")
    hfsm_volumes = harness_fleet_smoke_wf.get("volumes")
    hfsm_volume_names = [v.get("name") for v in hfsm_volumes] if isinstance(hfsm_volumes, list) else []
    if hfsm_volume_names != ["data", "repo", "scratch"]:
        failures.append(f"shipped example: harness-fleet-smoke-url volumes = {hfsm_volume_names},"
                        " want [data, repo, scratch] (the KB-241 peer-pod surface)")
harness_wf = base[LIB].get("harness-wire-url")
if not isinstance(harness_wf, dict):
    failures.append("shipped example: workflowLibrary missing the harness-wire-url object (KB-276)")
else:
    if harness_wf.get("type") != "url" or "url" not in harness_wf:
        failures.append("shipped example: harness-wire-url must be a url workflow")
    if not str(harness_wf.get("url", "")).startswith(
            "git::https://github.com/brokenbots/workflow-example.git//harness_wire_v1"):
        failures.append("shipped example: harness-wire-url must point at the harness_wire_v1 subtree")
    if str(harness_wf.get("url", "")).endswith("?ref=") or not str(harness_wf.get("url", "")).endswith("//harness_wire_v1"):
        failures.append("shipped example: harness-wire-url ships pinless pre-close (ADR-0005 D7 follow-up: "
                        "KB-276 re-pins ref/?ref= to the landing squash-merge commit at the card's close)")
    if harness_wf.get("ref"):
        failures.append("shipped example: harness-wire-url must be pinless before the KB-276 close re-pin "
                        "(the follow-up change flips this assertion together with the pin)")
    if harness_wf.get("class") != "triage":
        failures.append(f"shipped example: harness-wire-url class = {harness_wf.get('class')!r}, want 'triage'")
    if "image" in harness_wf:
        failures.append("shipped example: harness-wire-url must be url-only (no process image)")
    harness_secrets = harness_wf.get("secrets") if isinstance(harness_wf.get("secrets"), list) else []
    if {s.get("name") for s in harness_secrets} != {"github-tokens"}:
        failures.append("shipped example: harness-wire-url must bind exactly github-tokens "
                        "(WORKFLOW_GITHUB_TOKEN by name reference; the tree is otherwise secret-free)")
    harness_vols = harness_wf.get("volumes") if isinstance(harness_wf.get("volumes"), list) else []
    if [v.get("name") for v in harness_vols] != ["data", "repo", "scratch"]:
        failures.append("shipped example: harness-wire-url volumes must be data + repo + scratch "
                        "(the criteria runner shape the other url objects declare)")
    if harness_wf.get("adapterImages") != RECORDED_PIN_SET:
        failures.append(f"shipped example: harness-wire-url adapterImages = {harness_wf.get('adapterImages')!r}, "
                        "want the recorded pin set verbatim (three-way pin agreement)")

# Pin-duality cross-check (the KB-11/KB-94 gotcha recorded in both headers):
# a workflow ref is ONE pin recorded twice — the url's '?ref=' query and the
# separate "ref" key. The go loader never compares them and only the url
# query reaches the fetch, so a half-bump silently resolves a different
# tree than the "ref" key advertises. The per-entry SHA assertions above
# catch half-bumps on the objects they cover; this general cross-check
# covers every entry, including ones whose assertions pin only one side
# (ticket-cleanup-url's url prefix stops before the '?ref=').
for wf_name, wf_obj in base[LIB].items():
    if not isinstance(wf_obj, dict) or wf_obj.get("type") != "url" or not isinstance(wf_obj.get("url"), str):
        continue
    m = re.search(r"\?ref=([0-9a-f]{40})$", wf_obj["url"])
    url_ref = m.group(1) if m else None
    key_ref = wf_obj.get("ref") or None
    if url_ref != key_ref:
        failures.append(
            f"shipped example: {wf_name} pin duality mismatch: url ?ref="
            f"{url_ref if url_ref else '(absent)'} vs \"ref\" {key_ref if key_ref else '(absent)'}"
            " — one pin, two carriers, bumped together in the same change")

# Third leg of the three-way agreement (recorded pins == live CM): CI has no
# kubectl/cluster, but on an operator box (make test) the live criteria-routes
# CM is reachable — assert its adapterImages carry the same recorded set so
# live-only or example-only drift in either direction fails here. Skipped
# (not passed) when the CM cannot be reached.
kubectl = shutil.which("kubectl")
live_cm_json = None
if kubectl:
    try:
        proc = subprocess.run(
            [kubectl, "-n", "criteria-jobs", "get", "configmap", "criteria-routes", "-o", "json"],
            capture_output=True, text=True, timeout=30)
        if proc.returncode == 0:
            live_cm_json = proc.stdout
    except Exception:
        live_cm_json = None
if live_cm_json is None:
    print("live criteria-routes CM unreachable; skipping the recorded-pins==live leg")
else:
    try:
        cm = json.loads(live_cm_json)
        live_payload = json.loads(cm["data"]["routes.json"])
        live_library = live_payload["workflowLibrary"]
    except (KeyError, TypeError, ValueError):
        failures.append("live criteria-routes CM: data['routes.json'] missing or unparsable"
                        " — the live ConfigMap must carry the same routes.json payload"
                        " shape as the example (make apply-routes)")
        live_library = None
    if live_library is not None:
        if not isinstance(live_library, dict):
            failures.append("live criteria-routes CM: workflowLibrary is not an object")
        else:
            check_adapter_pins("live criteria-routes CM", live_library)

# ------------------------------------------------------------ schema checks
schema = json.load(open(schema_path, encoding="utf-8"))
try:
    import jsonschema

    validator_cls = jsonschema.Draft7Validator
    validator_cls.check_schema(schema)
    validator = validator_cls(schema)
    for label, doc in positive:
        errors = sorted(validator.iter_errors(doc), key=lambda e: list(e.path))
        if errors:
            failures.append(f"{label}: expected valid against shipped schema, got: "
                            + "; ".join(e.message for e in errors[:3]))
    for label, schema_fails, doc in negative:
        errors = list(validator.iter_errors(doc))
        if schema_fails and not errors:
            failures.append(f"{label}: expected schema violation, shipped schema accepted it")
        if not schema_fails and errors:
            failures.append(f"{label}: shipped schema rejected a valid document: "
                            + "; ".join(e.message for e in errors[:3]))
    print("jsonschema: validated against shipped k8s/routes.schema.json (draft-07)")
except ImportError:
    print("jsonschema module not available; ran the structural validator only")

if failures:
    print("FAIL:", file=sys.stderr)
    for failure in failures:
        print(f"  - {failure}", file=sys.stderr)
    sys.exit(1)

print("PASS: routes payload validates (schema + structural), "
      f"{len(positive)} positive and {len(negative)} negative variants behave as declared")
PYEOF

echo "PASS: k8s/examples/routes-configmap.yaml validates against k8s/routes.schema.json"