#!/usr/bin/env bash
# KB-311 (ADR 307): per-project binding ConfigMap regression test.
#
# The shipped k8s/examples/binding-*.yaml files are the schema of record for
# the project binding surface: the COMPLETE execution config a project
# namespace serves to the operator at admission (images/env/volumes/secrets/
# config — nothing else; unknown keys fail closed). The legs:
#   1. manifest shape guards (ConfigMap named criteria-binding in the project
#      namespace, binding.json data key, no secret material delivery — the
#      binding carries SecretProviderClass REFERENCES, never secretKeyRef);
#   2. structural validation of every binding payload against
#      k8s/criteria-binding.schema.json (mirrors routes.go's validators —
#      fail-closed unknown keys at every level);
#   3. registry connection via the projectRegistry in the routes payload
#      (namespace -> project, cutover: done) and route-project integrity;
#   4. interface conformance for the workflow tree the project's routes
#      select (KB-310 with the KB-311 project-env rule: declared env is a
#      requirement the binding must cover; extra env is legal);
#   5. the acceptance pair: two projects (criteria/go-dev and
#      typescript-sdk) execute the SAME workflow tree with DIFFERENT
#      images+env — reuse by binding, identical tree digest.
# The registry side of the routes payload is guarded by
# k8s/tests/test_routes_config.sh; this file re-checks the registry shape
# only as far as the binding examples need it to resolve.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
BINDING_SCHEMA="$REPO_ROOT/k8s/criteria-binding.schema.json"
ROUTES_SCHEMA="$REPO_ROOT/k8s/routes.schema.json"
ROUTES_EXAMPLE="$REPO_ROOT/k8s/examples/routes-configmap.yaml"

fail() {
    echo "FAIL: $1" >&2
    exit 1
}

for file in "$BINDING_SCHEMA" "$ROUTES_SCHEMA" "$ROUTES_EXAMPLE"; do
    [ -f "$file" ] || fail "required file missing: $file"
done

# ------------------------------------------------- binding manifest guards
shopt -s nullglob
binding_files=("$REPO_ROOT"/k8s/examples/binding-*.yaml)
shopt -u nullglob
[ ${#binding_files[@]} -ge 2 ] || fail "the acceptance pair needs at least two k8s/examples/binding-*.yaml ConfigMaps (one per cutover project)"

for binding in "${binding_files[@]}"; do
    manifest=$(cat "$binding")
    [ -n "$manifest" ] || fail "$binding is empty"
    grep -q <<<"$manifest" '^kind: ConfigMap$' || fail "$binding is not a ConfigMap"
    grep -q <<<"$manifest" '^  name: criteria-binding$' || fail "$binding is not named criteria-binding (the operator reads the project binding from a fixed name, KB-311)"
    grep -Eq <<<"$manifest" '^  namespace: [a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$' || fail "$binding does not declare a project metadata.namespace"
    grep -q <<<"$manifest" '^  binding.json: |$' || fail "$binding does not carry the binding.json data key"
    grep -q <<<"$manifest" 'secretKeyRef' && fail "$binding uses secretKeyRef (the binding carries SecretProviderClass references, not k8s Secret material)"
    grep -q <<<"$manifest" 'secretRef' && fail "$binding uses secretRef"
    grep -q <<<"$manifest" 'envFrom:' && fail "$binding uses envFrom"
    grep -q <<<"$manifest" 'stringData' && fail "$binding uses stringData"
    grep -q <<<"$manifest" 'kind: SecretProviderClass' && fail "$binding must not embed a SecretProviderClass resource (per-project credentials are referenced, never defined here)"
    printf '%s' "$manifest" | grep -Eq 'ghp_[A-Za-z0-9]|github_pat_|BEGIN (RSA |EC )?PRIVATE KEY' && fail "$binding embeds credential material"
done

# ------------------------------------------------------------- validation
python3 - "$BINDING_SCHEMA" "$ROUTES_SCHEMA" "$ROUTES_EXAMPLE" "$REPO_ROOT/k8s/examples" <<'PYEOF'
import copy
import json
import os
import re
import sys

binding_schema_path, routes_schema_path, routes_example_path, examples_dir = (
    sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4])

LABEL_RE = re.compile(r"^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$")
ENV_NAME_RE = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*$")
SECRET_KEY_RE = re.compile(r"^[A-Za-z0-9._-]+$")
NOSPACE_RE = re.compile(r"^\S+$")
MOUNT_RE = re.compile(r"^/")
REPO_RE = re.compile(r"^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$")

BINDING_KEYS = {"apiVersion", "kind", "images", "env", "volumes", "secrets", "config"}
VOLUME_KEYS = {"name", "kind", "mountPath", "claim", "server", "path", "sizeLimit", "secretName", "env"}
SECRET_KEYS = {"name", "secretProviderClass", "mountPath", "env"}
CONFIG_KEYS = {"buildCmd", "testCmd", "ciGateCmd", "baseBranch"}
PROJECT_ENTRY_KEYS = {"namespace", "repos", "cutover"}
VOLUME_KINDS = ("pvc", "nfs", "tmp", "k8s-secret")
CUTOVER_STATES = {"done"}

failures = []


def load_data_key(path, key):
    lines = open(path, encoding="utf-8").read().splitlines()
    for i, line in enumerate(lines):
        if line == f"  {key}: |":
            start = i + 1
            block = []
            for line in lines[start:]:
                if line.strip() == "":
                    block.append("")
                    continue
                if not line.startswith("    "):
                    break
                block.append(line[4:])
            payload = "\n".join(block)
            assert payload.strip().startswith("{"), f"extracted {key} payload is not JSON"
            return json.loads(payload)
    raise AssertionError(f"{key} data key not found in {path}")


def manifest_namespace(path):
    for line in open(path, encoding="utf-8").read().splitlines():
        if line.startswith("  namespace: "):
            return line[len("  namespace: "):].strip()
    raise AssertionError(f"namespace not found in {path}")


# -------------------------------------------------------- structural rules
# Mirrors routes.go's validators (labelRe, envNameRe, secretKeyRe, the
# per-kind volume exclusions, the SPC secret shape) — the same rules
# k8s/criteria-binding.schema.json declares, so a payload must clear all
# three gates (schema, this mirror, the operator's admission fail-closed
# parse) to be shippable.


def check_env(env, where, errs, value_re=None):
    if not isinstance(env, dict):
        errs.append(f"{where}.env must be an object")
        return
    for name, value in env.items():
        if not isinstance(name, str) or ENV_NAME_RE.match(name) is None:
            errs.append(f"{where}.env has invalid env var name {name!r}")
        if not isinstance(value, str) or value == "":
            errs.append(f"{where}.env.{name} must be a non-empty string")
            continue
        if value_re is not None and value_re.match(value) is None:
            errs.append(f"{where}.env.{name} maps to {value!r} which is not a valid key name")


def validate_volume(volume, where, errs):
    if not isinstance(volume, dict):
        errs.append(f"{where} must be an object")
        return
    for key in volume:
        if key not in VOLUME_KEYS:
            errs.append(f"{where} has unknown field {key!r}")
    for key in ("name", "kind", "mountPath"):
        if key not in volume:
            errs.append(f"{where} is missing required field {key}")
    name = volume.get("name")
    if "name" in volume and (not isinstance(name, str) or LABEL_RE.match(name) is None):
        errs.append(f"{where}.name {name!r} is not a DNS-1123 label")
    mount = volume.get("mountPath")
    if "mountPath" in volume and (not isinstance(mount, str) or MOUNT_RE.match(mount) is None):
        errs.append(f"{where}.mountPath {mount!r} must be an absolute path")
    if "env" in volume:
        check_env(volume["env"], where, errs)
    kind = volume.get("kind")
    if kind not in VOLUME_KINDS:
        errs.append(f"{where}.kind must be one of {', '.join(VOLUME_KINDS)}, got {kind!r}")
        return
    if kind == "pvc":
        if volume.get("claim", "") == "":
            errs.append(f"{where}: kind=pvc requires a claim")
        for cross in ("server", "path", "sizeLimit", "secretName"):
            if volume.get(cross):
                errs.append(f"{where}: kind=pvc must not declare {cross}")
    elif kind == "nfs":
        if volume.get("server", "") == "" or volume.get("path", "") == "":
            errs.append(f"{where}: kind=nfs requires server and path")
        for cross in ("claim", "sizeLimit", "secretName"):
            if volume.get(cross):
                errs.append(f"{where}: kind=nfs must not declare {cross}")
    elif kind == "tmp":
        for cross in ("claim", "server", "path", "secretName"):
            if volume.get(cross):
                errs.append(f"{where}: kind=tmp must not declare {cross}")
    elif kind == "k8s-secret":
        if volume.get("secretName", "") == "":
            errs.append(f"{where}: kind=k8s-secret requires a secretName")
        for cross in ("claim", "server", "path", "sizeLimit"):
            if volume.get(cross):
                errs.append(f"{where}: kind=k8s-secret must not declare {cross}")


def validate_secret(secret, where, errs):
    if not isinstance(secret, dict):
        errs.append(f"{where} must be an object")
        return
    for key in secret:
        if key not in SECRET_KEYS:
            errs.append(f"{where} has unknown field {key!r}")
    for key in ("name", "secretProviderClass", "mountPath"):
        if key not in secret:
            errs.append(f"{where} is missing required field {key}")
    name = secret.get("name")
    if "name" in secret and (not isinstance(name, str) or LABEL_RE.match(name) is None):
        errs.append(f"{where}.name {name!r} is not a DNS-1123 label")
    spc = secret.get("secretProviderClass")
    if "secretProviderClass" in secret and (not isinstance(spc, str) or spc == ""):
        errs.append(f"{where}.secretProviderClass must be a non-empty string (the reference to the project's OWN SecretProviderClass, KB-311)")
    mount = secret.get("mountPath")
    if "mountPath" in secret and (not isinstance(mount, str) or MOUNT_RE.match(mount) is None):
        errs.append(f"{where}.mountPath {mount!r} must be an absolute path")
    if "env" in secret:
        check_env(secret["env"], where, errs, value_re=SECRET_KEY_RE)


def validate_binding(doc, label):
    errs = []
    if not isinstance(doc, dict):
        errs.append("binding must be a JSON object")
        return errs
    for key in doc:
        if key not in BINDING_KEYS:
            errs.append(f"unknown top-level binding key {key!r} — the binding is images/env/volumes/secrets/config and NOTHING else (fail closed, KB-311)")
    for key in BINDING_KEYS:
        if key not in doc:
            errs.append(f"binding is missing required key {key!r}")
    if doc.get("apiVersion") != "criteria.brokenbots.dev/v1":
        errs.append(f"apiVersion must be \"criteria.brokenbots.dev/v1\"")
    if doc.get("kind") != "CriteriaBinding":
        errs.append(f"kind must be \"CriteriaBinding\"")
    images = doc.get("images")
    if not isinstance(images, dict):
        errs.append("images must be an object of adapter kind -> image reference")
    else:
        for kind, ref in images.items():
            if not isinstance(kind, str) or LABEL_RE.match(kind) is None:
                errs.append(f"images has invalid adapter kind {kind!r}")
            if not isinstance(ref, str) or not ref or NOSPACE_RE.match(ref) is None:
                errs.append(f"images.{kind} must be a non-empty image reference without whitespace")
    if "env" in doc:
        env = doc["env"]
        if not isinstance(env, dict):
            errs.append("env must be an object of name -> value")
        else:
            for name, value in env.items():
                if not isinstance(name, str) or ENV_NAME_RE.match(name) is None:
                    errs.append(f"env has invalid env var name {name!r}")
                if not isinstance(value, str) or value == "":
                    errs.append(f"env.{name} must be a non-empty string")
    volumes = doc.get("volumes")
    if not isinstance(volumes, list):
        errs.append("volumes must be an array")
    else:
        seen = set()
        for i, volume in enumerate(volumes):
            validate_volume(volume, f"volumes[{i}]", errs)
            if isinstance(volume, dict) and isinstance(volume.get("name"), str):
                if volume["name"] in seen:
                    errs.append(f"volumes[{i}].name {volume['name']!r} is declared twice")
                seen.add(volume["name"])
    secrets = doc.get("secrets")
    if not isinstance(secrets, list):
        errs.append("secrets must be an array")
    else:
        seen = set()
        for i, secret in enumerate(secrets):
            validate_secret(secret, f"secrets[{i}]", errs)
            if isinstance(secret, dict) and isinstance(secret.get("name"), str):
                if secret["name"] in seen:
                    errs.append(f"secrets[{i}].name {secret['name']!r} is declared twice")
                seen.add(secret["name"])
    config = doc.get("config")
    if not isinstance(config, dict):
        errs.append("config must be an object of config slot -> value")
    else:
        for slot, value in config.items():
            if slot not in CONFIG_KEYS:
                errs.append(f"config slot {slot!r} is not a declared config slot (buildCmd/testCmd/ciGateCmd/baseBranch) — unknown slot, fail closed")
            if not isinstance(value, str) or value == "":
                errs.append(f"config.{slot} must be a non-empty string")
    return errs


# ------------------------------------------------------------ registry side
# The same projectRegistry shape the route schema declares; routes must only
# select projects the registry actually carries (the registry is the ONLY
# ticket->project routing table, KB-311).
def validate_registry(registry, routes):
    errs = []
    if not isinstance(registry, dict) or not registry:
        errs.append("projectRegistry must be a non-empty object of project name -> project entry")
        return errs
    for project, entry in registry.items():
        if not isinstance(project, str) or project.strip() == "":
            errs.append("projectRegistry has a blank project name")
            continue
        if not isinstance(entry, dict):
            errs.append(f"projectRegistry.{project} must be an object")
            continue
        for key in entry:
            if key not in PROJECT_ENTRY_KEYS:
                errs.append(f"projectRegistry.{project} has unknown field {key!r}")
        ns = entry.get("namespace")
        if "namespace" not in entry or not isinstance(ns, str) or LABEL_RE.match(ns) is None:
            errs.append(f"projectRegistry.{project}.namespace must be a DNS-1123 label")
        repos = entry.get("repos")
        if repos is not None and (not isinstance(repos, list) or \
                any(not isinstance(r, str) or REPO_RE.match(r) is None for r in repos)):
            errs.append(f"projectRegistry.{project}.repos must be an array of owner/name repository references")
        cutover = entry.get("cutover")
        if "cutover" in entry and cutover not in CUTOVER_STATES:
            errs.append(f"projectRegistry.{project}.cutover {cutover!r} is not one of {sorted(CUTOVER_STATES)} (binding-first, deletion second)")
    for route in routes:
        if isinstance(route, dict) and route.get("project") not in registry:
            errs.append(f"route {route.get('name')!r} selects project {route.get('project')!r} which the projectRegistry does not carry")
    return errs


# -------------------------------------------------- interface conformance
def binding_conformance(binding, iface, label):
    """Python mirror of scripts/interface_validate.mjs bindingViolations in
    its KB-311 projectEnv mode (and of criteria-k8s admission's
    validateInterfaceBinding): adapters/mounts/secretEnv follow the
    interface's string shapes, project env extras stay legal (KB-311 ADR
    307), and unused/undeclared entries fail closed. Keep the three
    rule-equivalent — a divergence here is a false PASS on the contract."""
    errs = []
    wf = iface.get("workflow") or "<unknown workflow>"
    volumes = [v for v in binding.get("volumes", []) if isinstance(v, dict)]
    mounts = [m for m in iface.get("mounts", []) if isinstance(m, dict)]

    def list_or_none(names):
        return "[] (none)" if not names else "[" + " ".join(names) + "]"

    def volume_desc(v):
        return f"{v.get('name')} ({v.get('kind')} at {v.get('mountPath')})"

    # Mounts: binding volumes must match interface mounts byte-exact on
    # mountPath with the declared kind, and every interface mount must be
    # supplied; a volume no mount declares fails closed.
    iface_paths = {m.get("mountPath"): m for m in mounts}
    volume_paths = {v.get("mountPath"): v for v in volumes}
    for volume in volumes:
        declared = iface_paths.get(volume.get("mountPath"))
        if declared is None:
            errs.append(f"{label}: binding supplies volume {volume.get('name')!r} ({volume.get('kind')!r} at {volume.get('mountPath')!r}) "
                        f"which interface {wf!r} does not require; interface mounts: {list_or_none([volume_desc(m) for m in mounts])} "
                        f"(unknown mount, fail closed — the declaration is the supported surface)")
        elif volume.get("kind") != declared.get("kind"):
            errs.append(f"{label}: binding volume {volume.get('name')!r} wants kind {volume.get('kind')!r} at {volume.get('mountPath')!r}; "
                        f"interface {wf!r} declares {declared.get('kind')!r} (binding volumes and interface mounts must agree on kind)")
    for declared in mounts:
        volume = volume_paths.get(declared.get("mountPath"))
        if volume is None:
            errs.append(f"{label}: interface {wf!r} requires mount {declared.get('name')!r} ({declared.get('kind')!r}) at {declared.get('mountPath')!r}; "
                        f"binding volumes {list_or_none([volume_desc(v) for v in volumes])} (mount paths must match byte-exact)")

    # Adapter kinds: interface-required kinds ⊆ binding images (the
    # interface declares kind STRINGS); extra images tolerated.
    images = binding.get("images") or {}
    for kind in iface.get("adapters", []):
        if kind not in images:
            errs.append(f"{label}: interface {wf!r} requires adapter kind {kind!r}; binding images provide {list_or_none(sorted(images))}")

    # Secret env lanes (string names, KB-310): the union of binding secret
    # env-map KEYS is the resolution surface — every interface secretEnv
    # name must resolve from it and every carried key must be declared
    # (secretEnv ∪ env). A secret with no env map contributes nothing (the
    # CSI file-mount lane); SPC/mountPath references stay structural
    # (validate_secret), not lane-matched.
    declared_secret_lanes = iface.get("secretEnv", [])
    declared_env = iface.get("env", [])
    secret_names = []
    resolved = {}
    for secret in (s for s in binding.get("secrets", []) if isinstance(s, dict)):
        secret_names.append(secret.get("name"))
        for key in (secret.get("env") or {}):
            if key not in declared_secret_lanes and key not in declared_env:
                errs.append(f"{label}: binding secret {secret.get('name')!r} carries env {key!r} which interface {wf!r} does not require; "
                            f"interface secretEnv: {list_or_none(declared_secret_lanes)}, env: {list_or_none(declared_env)} (unknown slot, fail closed)")
            resolved[key] = secret.get("name")
    for lane in declared_secret_lanes:
        if lane not in resolved:
            errs.append(f"{label}: interface {wf!r} requires secret env {lane!r}; binding secrets {list_or_none(secret_names)} "
                        f"carry no such env key (wire the lane in the binding secret's env map)")

    # Project env (KB-311, ADR 307): every env name the interface declares
    # must be covered by the project binding's env; extra keys are the
    # project's own execution config (tool paths, GOFLAGS, npm proxies ...)
    # and stay legal — the KB-311 divergence from KB-310 global mode.
    env = binding.get("env") or {}
    for name in declared_env:
        if name not in env:
            errs.append(f"{label}: interface {wf!r} requires env {name!r}; binding env carries {list_or_none(sorted(env))} "
                        f"(project env must cover the tree's declared env, KB-311)")

    declared_slots = {s.get("slot") for s in iface.get("configSlots", []) if isinstance(s, dict)}
    for slot in (binding.get("config") or {}):
        if slot not in declared_slots:
            errs.append(f"{label}: binding config slot {slot!r} is not declared by the tree's configSlots (unknown slot, fail closed)")
    return errs


# ------------------------------------------------------------------- load
binding_files = sorted(
    os.path.join(examples_dir, name) for name in os.listdir(examples_dir)
    if name.startswith("binding-") and name.endswith(".yaml"))
if len(binding_files) < 2:
    failures.append(f"only {len(binding_files)} binding example(s) shipped — the acceptance pair needs two cutover projects")

routes_payload = load_data_key(routes_example_path, "routes.json")
registry = routes_payload.get("projectRegistry")
registry_errs = validate_registry(registry, routes_payload.get("routes", []))
failures.extend(f"routes example projectRegistry: {err}" for err in registry_errs)

by_namespace = {}
for project, entry in (registry or {}).items():
    if isinstance(entry, dict) and isinstance(entry.get("namespace"), str):
        by_namespace.setdefault(entry["namespace"], []).append((project, entry))

resolutions = []
for path in binding_files:
    name = os.path.basename(path)
    ns = manifest_namespace(path)
    binding = load_data_key(path, "binding.json")
    errs = validate_binding(copy.deepcopy(binding), name)
    failures.extend(f"{name} (binding.json): {err}" for err in errs)

    projects = by_namespace.get(ns, [])
    if len(projects) != 1:
        failures.append(f"{name}: namespace {ns!r} maps to {len(projects)} registry projects (expected exactly one)")
        continue
    project, entry = projects[0]
    if entry.get("cutover") != "done":
        failures.append(f"{name}: project {project!r} is not cutover: done — the binding CM is served only after cutover (KB-311)")
    ns_routes = [r for r in routes_payload.get("routes", []) if r.get("project") == project]
    if not ns_routes:
        failures.append(f"{name}: project {project!r} has no routes in the routes payload")
        continue
    trees = {routes_payload["workflowLibrary"].get(r.get("workflow"), {}).get("url") or "" for r in ns_routes}
    if len(trees) != 1:
        failures.append(f"{name}: project {project!r} routes select {len(trees)} distinct workflow trees; the example binding declares one tree's execution config")
        continue
    workflow = routes_payload["workflowLibrary"].get(ns_routes[0].get("workflow"))
    iface = (workflow or {}).get("interface")
    if not isinstance(iface, dict):
        failures.append(f"{name}: project {project!r}'s workflow carries no embedded interface to conform to")
        continue
    failures.extend([f"{name}: {err}" for err in binding_conformance(binding, iface, name)])
    resolutions.append({"file": name, "project": project, "binding": binding,
                        "tree": json.dumps({"url": workflow.get("url"), "ref": workflow.get("ref")}, sort_keys=True)})

# ------------------------------------------------ acceptance (reuse-by-binding)
if len(resolutions) >= 2:
    if len({r["tree"] for r in resolutions}) != 1:
        failures.append(f"binding examples resolve to {len({r['tree'] for r in resolutions})} distinct trees — the acceptance demo is ONE tree executed by several projects")
    if len({r["project"] for r in resolutions}) != len(resolutions):
        failures.append("binding examples resolve to the same project through the registry; the acceptance pair needs distinct projects")
    for i in range(1, len(resolutions)):
        a, b = resolutions[i - 1], resolutions[i]
        if json.dumps(a["binding"].get("images"), sort_keys=True) == json.dumps(b["binding"].get("images"), sort_keys=True):
            failures.append(f"{a['file']} and {b['file']} ship identical images — the acceptance demo requires different execution images per project")
        if json.dumps(a["binding"].get("env"), sort_keys=True) == json.dumps(b["binding"].get("env"), sort_keys=True):
            failures.append(f"{a['file']} and {b['file']} ship identical env — the acceptance demo requires different project env per project")
    print(f"acceptance: {len(resolutions)} binding examples resolve to ONE tree (identical digest) with pairwise-different images+env")

# ------------------------------------------------------------------ negatives
good = load_data_key(binding_files[0], "binding.json")
_negatives = []

def expect_reject(label, mutate, needle):
    _negatives.append(label)
    mutated = copy.deepcopy(good)
    mutate(mutated)
    errs = validate_binding(mutated, label)
    if not any(needle in err for err in errs):
        failures.append(f"{label}: expected a binding violation containing {needle!r}, got {errs[:2]}")

expect_reject("binding/unknown-top-key", lambda d: d.update({"workflowLibrary": {}}), "unknown top-level")
expect_reject("binding/missing-required", lambda d: d.pop("env"), "missing required key")
expect_reject("binding/api-version", lambda d: d.update({"apiVersion": "v1"}), "apiVersion")
expect_reject("binding/kind", lambda d: d.update({"kind": "criteria.example/v1"}), "kind")
expect_reject("binding/image-blank", lambda d: d["images"].update({"shell": " "}), "without whitespace")
expect_reject("binding/image-kind-label", lambda d: d["images"].update({"Bad_Kind": "img"}), "adapter kind")
expect_reject("binding/env-name", lambda d: d["env"].update({"1BAD": "x"}), "invalid env var name")
expect_reject("binding/env-empty", lambda d: d["env"].update({"GOFLAGS": ""}), "non-empty string")
expect_reject("binding/volume-unknown-field", lambda d: d["volumes"][0].update({"nodeSelector": "x"}), "unknown field")
expect_reject("binding/volume-missing-kind", lambda d: d["volumes"][0].pop("kind"), "missing required field")
expect_reject("binding/volume-kind", lambda d: d["volumes"][0].update({"kind": "ceph"}), "kind must be one of")
expect_reject("binding/volume-pvc-no-claim", lambda d: d["volumes"][0].pop("claim"), "kind=pvc requires a claim")
expect_reject("binding/volume-pvc-cross-kind", lambda d: d["volumes"][0].update({"sizeLimit": "8Gi"}), "must not declare")
expect_reject("binding/volume-mount-path", lambda d: d["volumes"][0].update({"mountPath": "data"}), "absolute path")
expect_reject("binding/volume-name-label", lambda d: d["volumes"][0].update({"name": "Bad_Name"}), "DNS-1123 label")
expect_reject("binding/secret-unknown-field", lambda d: d["secrets"][0].update({"stringData": {}}), "unknown field")
expect_reject("binding/secret-no-spc", lambda d: d["secrets"][0].pop("secretProviderClass"), "missing required field")
expect_reject("binding/secret-env-key", lambda d: d["secrets"][0]["env"].update({"KANBOARD_APP_TOKEN": "sec/ret"}), "not a valid key name")
expect_reject("binding/secret-env-name", lambda d: d["secrets"][0]["env"].update({"with space": "kanboard_app_token"}), "invalid env var name")
expect_reject("binding/config-unknown-slot", lambda d: d["config"].update({"imageTag": "v9"}), "not a declared config slot")
expect_reject("binding/config-blank", lambda d: d["config"].update({"ciGateCmd": ""}), "non-empty string")
expect_reject("binding/volumes-not-array", lambda d: d.update({"volumes": {"data": {}}}), "must be an array")
expect_reject("binding/secrets-not-array", lambda d: d.update({"secrets": {"kanboard": {}}}), "must be an array")
expect_reject("binding/images-not-object", lambda d: d.update({"images": ["shell"]}), "images must be an object")

def expect_registry_reject(label, mutate, needle):
    _negatives.append(label)
    mutated = copy.deepcopy(registry)
    mutate(mutated)
    errs = validate_registry(mutated, routes_payload.get("routes", []))
    if not any(needle in err for err in errs):
        failures.append(f"{label}: expected a registry violation containing {needle!r}, got {errs[:2]}")

expect_registry_reject("registry/unknown-field", lambda r: list(r.values())[0].update({"images": {}}), "unknown field")
expect_registry_reject("registry/blank-project", lambda r: r.update({" ": {"namespace": "demo-ns", "repos": ["o/n"]}}), "blank project name")
expect_registry_reject("registry/bad-namespace", lambda r: list(r.values())[0].update({"namespace": "Bad_NS"}), "DNS-1123 label")
expect_registry_reject("registry/bad-repo", lambda r: list(r.values())[0].update({"repos": ["missing-slash"]}), "owner/name")
expect_registry_reject("registry/bad-cutover", lambda r: list(r.values())[0].update({"cutover": "half done"}), "cutover")

# ------------------------------------------------------------ schema checks
schemas = {"binding": json.load(open(binding_schema_path, encoding="utf-8")),
           "routes": json.load(open(routes_schema_path, encoding="utf-8"))}
try:
    import jsonschema

    jsonschema.Draft7Validator.check_schema(schemas["binding"])
    jsonschema.Draft7Validator.check_schema(schemas["routes"])
    binding_validator = jsonschema.Draft7Validator(schemas["binding"])
    for path in binding_files:
        doc = load_data_key(path, "binding.json")
        errors = list(binding_validator.iter_errors(doc))
        if errors:
            failures.append(f"{os.path.basename(path)}: expected valid against the shipped binding schema, got: "
                            + "; ".join(e.message for e in errors[:3]))
    reg_validator = jsonschema.Draft7Validator(schemas["routes"])
    reg_errors = list(reg_validator.iter_errors(routes_payload))
    if reg_errors:
        failures.append("routes example (routes.json incl. projectRegistry): expected valid against the shipped routes schema, got: "
                        + "; ".join(e.message for e in reg_errors[:3]))
    print("jsonschema: binding payloads + projectRegistry validated against the shipped draft-07 schemas")
except ImportError:
    print("jsonschema module not available; ran the structural validator only")

if failures:
    print("FAIL:", file=sys.stderr)
    for failure in failures:
        print(f"  - {failure}", file=sys.stderr)
    sys.exit(1)

print("PASS: binding ConfigMaps validate (schema + structural + registry + interface conformance + acceptance), "
      f"{len(binding_files)} examples and {len(_negatives)} negative variants behave as declared")
PYEOF

echo "PASS: per-project binding ConfigMaps validate against k8s/criteria-binding.schema.json (KB-311)"