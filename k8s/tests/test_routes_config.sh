#!/usr/bin/env bash
set -euo pipefail

# Regression test for the routes ConfigMap (CRI-216). Validates the shipped
# example payload against the shipped JSON Schema (k8s/routes.schema.json),
# exercises the schema's fail-closed constraints with negative variants, and
# proves the ADR-0005 D1/D2 modes (image-only, url-only, url+image + expected
# pin) and the pvc/nfs/tmp volumes with env mapping and OpenBao/CSI secret
# name references are representable. Also asserts the example is secret-free.
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
printf '%s' "$manifest" | grep -q '^kind: ConfigMap$' || \
    fail "example is not a ConfigMap"
printf '%s' "$manifest" | grep -q '^  name: criteria-routes$' || \
    fail "example ConfigMap is not named criteria-routes"
printf '%s' "$manifest" | grep -q '^  namespace: criteria-jobs$' || \
    fail "example ConfigMap is not namespaced to criteria-jobs"
printf '%s' "$manifest" | grep -q '^  routes.json: |$' || \
    fail "example ConfigMap does not carry the routes.json data key"
printf '%s' "$manifest" | grep -q '^kind: Secret$' && \
    fail "example ConfigMap must not contain a Secret resource"
printf '%s' "$manifest" | grep -q 'secretKeyRef' && \
    fail "example ConfigMap uses secretKeyRef"
printf '%s' "$manifest" | grep -q 'secretRef' && \
    fail "example ConfigMap uses secretRef"
printf '%s' "$manifest" | grep -q 'envFrom:' && \
    fail "example ConfigMap uses envFrom"
printf '%s' "$manifest" | grep -q 'stringData' && \
    fail "example ConfigMap uses stringData"
printf '%s' "$manifest" | grep -Eq 'ghp_[A-Za-z0-9]|github_pat_|BEGIN (RSA |EC )?PRIVATE KEY' && \
    fail "example ConfigMap embeds credential material"

# ---------------------------------------------------------------- validation
python3 - "$SCHEMA" "$EXAMPLE" <<'PYEOF'
import copy
import json
import re
import sys

schema_path, example_path = sys.argv[1], sys.argv[2]

LABEL_RE = re.compile(r"^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$")
ENV_NAME_RE = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*$")
SECRET_KEY_RE = re.compile(r"^[A-Za-z0-9._-]+$")
NOSPACE_RE = re.compile(r"^\S+$")
MOUNT_RE = re.compile(r"^/")

WORKFLOW_KEYS = {"type", "namespace", "image", "url", "ref", "volumes", "secrets", "env"}
VOLUME_KEYS = {"name", "kind", "mountPath", "subPath", "readOnly", "claim", "server", "path", "sizeLimit", "env"}
SECRET_KEYS = {"name", "secretProviderClass", "mountPath", "env"}
ROUTE_KEYS = {"name", "workflow", "project", "tags", "tagMatch", "states"}
TOP_KEYS = {"apiVersion", "kind", "workflowLibrary", "routes"}
VOLUME_KINDS = ("pvc", "nfs", "tmp")
WORKFLOW_TYPES = ("url", "image")


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
        errs.append(f"{where}.kind {kind!r} must be one of pvc, nfs, tmp")
    mount = volume.get("mountPath")
    if "mountPath" in volume and (not isinstance(mount, str) or MOUNT_RE.match(mount) is None):
        errs.append(f"{where}.mountPath {mount!r} must be an absolute path")
    if "subPath" in volume and (not isinstance(volume["subPath"], str) or not volume["subPath"]):
        errs.append(f"{where}.subPath must be a non-empty string")
    if "readOnly" in volume and not isinstance(volume["readOnly"], bool):
        errs.append(f"{where}.readOnly must be a boolean")
    for key in ("claim", "server", "path", "sizeLimit"):
        if key in volume and (not isinstance(volume[key], str) or not volume[key]):
            errs.append(f"{where}.{key} must be a non-empty string")
    if "env" in volume:
        check_env(volume["env"], where, errs)
    if kind == "pvc":
        if "claim" not in volume:
            errs.append(f"{where}: pvc volume requires claim")
        for key in ("server", "path", "sizeLimit"):
            if key in volume:
                errs.append(f"{where}: pvc volume must not declare {key}")
    elif kind == "nfs":
        for key in ("server", "path"):
            if key not in volume:
                errs.append(f"{where}: nfs volume requires {key}")
        for key in ("claim", "sizeLimit"):
            if key in volume:
                errs.append(f"{where}: nfs volume must not declare {key}")
    elif kind == "tmp":
        for key in ("claim", "server", "path"):
            if key in volume:
                errs.append(f"{where}: tmp volume must not declare {key}")


def validate_workflow(name, workflow, where, errs):
    if not isinstance(workflow, dict):
        errs.append(f"{where} must be an object")
        return
    for key in ("type", "namespace"):
        if key not in workflow:
            errs.append(f"{where} is missing required field {key}")
    for key in workflow:
        if key not in WORKFLOW_KEYS:
            errs.append(f"{where} has unknown field {key!r}")
    wtype = workflow.get("type")
    if "type" in workflow and wtype not in WORKFLOW_TYPES:
        errs.append(f"{where}.type {wtype!r} must be one of url, image")
    namespace = workflow.get("namespace")
    if "namespace" in workflow and not is_label(namespace):
        errs.append(f"{where}.namespace {namespace!r} is not a DNS-1123 label")
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
    if wtype == "image":
        if "image" not in workflow:
            errs.append(f"{where}: type image requires image")
        for key in ("url", "ref"):
            if key in workflow:
                errs.append(f"{where}: type image must not declare {key} (ADR-0005 D2 fail-closed)")
    if wtype == "url" and "url" not in workflow:
        errs.append(f"{where}: type url requires url")


def validate_route(route, where, library, errs):
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
                validate_route(route, f"payload.routes[{i}]", library, errs)
    return errs


# --------------------------------------------------------- example variants
base = load_example(example_path)
LIB = "workflowLibrary"
BAKED = "linear-intake-v1"
URLWF = "linear-intake-url"


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
    ("route-without-states-defaults-triage", mutate(lambda d: d["routes"][0].pop("states"))),
    ("route-with-custom-states", mutate(lambda d: d["routes"][0].update(
        states=["Triage", "In Progress"]))),
    ("pvc-volume-as-nfs", mutate(lambda d: (
        d[LIB][BAKED]["volumes"][0].pop("claim"),
        d[LIB][BAKED]["volumes"][0].update(kind="nfs", server="nfs.internal", path="/export/data")))),
    ("volume-read-only", mutate(lambda d: d[LIB][BAKED]["volumes"][0].update(readOnly=True))),
    ("volume-subpath", mutate(lambda d: d[LIB][BAKED]["volumes"][0].update(subPath="intake"))),
    ("workflow-level-env", mutate(lambda d: d[LIB][BAKED].update(env={"ALLOW_DIRTY": "false"}))),
]

negative = [
    ("image-workflow-with-url", True, mutate(lambda d: d[LIB][BAKED].update(
        url="git::https://example.invalid/repo.git//wf"))),
    ("image-workflow-with-ref", True, mutate(lambda d: d[LIB][BAKED].update(ref="sha256:beef"))),
    ("image-workflow-without-image", True, mutate(lambda d: d[LIB][BAKED].pop("image"))),
    ("url-workflow-without-url", True, mutate(lambda d: d[LIB][URLWF].pop("url"))),
    ("workflow-unknown-type", True, mutate(lambda d: d[LIB][BAKED].update(type="baked"))),
    ("pvc-volume-without-claim", True, mutate(lambda d: d[LIB][BAKED]["volumes"][0].pop("claim"))),
    ("nfs-volume-without-server", True, mutate(lambda d: d[LIB][URLWF]["volumes"][2].pop("server"))),
    ("nfs-volume-without-path", True, mutate(lambda d: d[LIB][URLWF]["volumes"][2].pop("path"))),
    ("pvc-volume-with-sizelimit", True, mutate(lambda d: d[LIB][BAKED]["volumes"][0].update(sizeLimit="8Gi"))),
    ("volume-unknown-kind", True, mutate(lambda d: d[LIB][BAKED]["volumes"][0].update(kind="hostPath"))),
    ("volume-relative-mountpath", True, mutate(lambda d: d[LIB][BAKED]["volumes"][0].update(mountPath="data"))),
    ("volume-unknown-field", True, mutate(lambda d: d[LIB][BAKED]["volumes"][0].update(hostPath="/srv"))),
    ("unknown-top-level-field", True, mutate(lambda d: d.update(workflowLibraryTypo={}))),
    ("unknown-route-field", True, mutate(lambda d: d["routes"][0].update(workflowRef="x"))),
    ("route-empty-states", True, mutate(lambda d: d["routes"][0].update(states=[]))),
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