"""Routes-payload and ProjectWorkflowConfig record operations.

Shared by setup_project (registry entries) and register_job (catalog+routes).
One place for the shape constants of record (castle/internal/confighome +
k8s/routes.schema.json + k8s/criteria-binding.schema.json).

The routes payload here is the routes.json CONTENT (not the ConfigMap
wrapper, which has its own metadata.renderedFrom stamp managed by
routes-sync.sh — never write renderedFrom from these tools).
"""


SLOT_VARIABLES = {  # KB-310 config-slot surface (castle confighome record.go)
    "buildCmd": "build_cmd",
    "testCmd": "test_cmd",
    "ciGateCmd": "ci_gate_cmd",
    "baseBranch": "base_branch",
}

ROUTE_KEYS = {"name", "workflow", "project", "states", "tags", "configRef"}

def empty_routes_payload():
    return {
        "apiVersion": "criteria.brokenbots.dev/v1",
        "kind": "Routes",
        "workflowLibrary": {},
        "configLibrary": {},
        "projectRegistry": {},
        "routes": [],
    }


def find_registry_entry(routes, project_name):
    return (routes.get("projectRegistry") or {}).get(project_name)


def add_registry_entry(routes, project_name, namespace, repos, cutover=None):
    entry = {"namespace": namespace, "repos": list(repos)}
    if cutover:
        entry["cutover"] = cutover
    routes.setdefault("projectRegistry", {})[project_name] = entry
    return entry


def route_exists(routes, route_name):
    return any(r.get("name") == route_name for r in routes.get("routes", []))


def add_route(routes, route_name, workflow_key, project_name, states, tags=None,
              config_ref=None):
    if route_exists(routes, route_name):
        raise ValueError(f"route {route_name} already exists")
    rt = {"name": route_name, "workflow": workflow_key, "project": project_name,
          "states": list(states)}
    if tags:
        rt["tags"] = list(tags)
    if config_ref:
        rt["configRef"] = config_ref
    routes.setdefault("routes", []).append(rt)
    return rt


def upsert_catalog_entry(routes, workflow_key, entry):
    """entry = a workflowLibrary object (url/ref/class/namespace/volumes/
    secrets/adapterImages/interface). Returns the (possibly replaced) entry."""
    lib = routes.setdefault("workflowLibrary", {})
    if workflow_key in lib:
        raise ValueError(
            f"workflowLibrary entry {workflow_key} already exists — "
            "editing a live job entry is a re-render of its tree by name, not an upsert; aborting")
    lib[workflow_key] = entry
    return entry


def upsert_config_library(routes, config_key, ci_gate_cmd):
    lib = routes.setdefault("configLibrary", {})
    lib[config_key] = {"ciGateCmd": ci_gate_cmd}
    return lib[config_key]


# ---------------- ProjectWorkflowConfig record helpers (castle config home)

def record_for_project(registry_name, namespace, repos):
    return {
        "apiVersion": "criteria.brokenbots.dev/v1",
        "kind": "ProjectWorkflowConfig",
        "registryName": registry_name,
        "namespace": namespace,
        "repos": list(repos),
        "catalog": {},
        "routes": [],
        "configLibrary": {},
        "binding": None,
    }


def extract_data_json_block(manifest_text, key):
    """Extract a YAML 'data:\n  <key>: |' block scalars payload as text."""
    lines = manifest_text.split("\n")
    out = []
    in_data = False
    in_block = False
    base_indent = ""
    keyline = f"{key}: |"
    for l in lines:
        if not in_data:
            if l.strip() == "data:":
                in_data = True
            continue
        if not in_block:
            s = l.strip()
            if s == keyline or (s.startswith(key + ":") and s.endswith("|")):
                in_block = True
                base_indent = l[: len(l) - len(l.lstrip())]
                continue
            if l.strip() and not l.startswith(" "):
                in_data = False  # next top-level doc/object
            continue
        # in block
        if l.strip() == "" :
            out.append("")
            continue
        indent = l[: len(l) - len(l.lstrip())]
        if len(indent) <= len(base_indent):
            break
        out.append(l[len(base_indent) + 2 if False else len(base_indent) + 0:])
    return "\n".join(out).rstrip() + "\n"
