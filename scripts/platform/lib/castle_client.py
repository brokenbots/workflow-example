"""Castle Connect-RPC client (criteria.v1 ServerService + castle.v1
ProjectService) over plain HTTP to the Service IP.

Credential classes (verified 10-25/26):
- orchestrator token (secret criteria-castle-orchestrator / CASTLE_ORCHESTRATOR_TOKEN):
  project administration + run reads + archive; NOT agent-owned procedures.
- console session (ConsoleService/Login from secret criteria-console-auth):
  console-scoped writes that the orchestrator token is denied.
This client carries whatever token the caller passes; components decide.
"""

import json
import os

from lib import common


def _headers(token):
    h = {}
    if token:
        h["Authorization"] = f"Bearer {token}"
    return h


def castle_rpc(base_url, service_method, body, token=None, timeout=30, expected=(200,)):
    """POST /<package>.<Service>/<Method> ConnectRPC (JSON). Returns parsed
    body; raises Gate on unexpected status."""
    url = base_url.rstrip("/") + "/" + service_method
    out = common.http_post_json(url, body, headers=_headers(token), timeout=timeout)
    st = out.get("_status")
    if st not in expected:
        # castle Connect errors carry {code, message}
        msg = out.get("message") or json.dumps(out)[:300]
        raise common.Gate(f"castle {service_method}: http {st} body {msg}")
    return out


def orchestrator_token():
    return common.k8s_secret_key("criteria-jobs", "criteria-castle-orchestrator",
                                 "CASTLE_ORCHESTRATOR_TOKEN")


def console_login(base_url):
    """Console session via castle-auth secret keys (criteria-console-auth)."""
    user = common.k8s_secret_key("criteria-jobs", "criteria-console-auth", "CASTLE_CONSOLE_USER")
    pw = common.k8s_secret_key("criteria-jobs", "criteria-console-auth", "CASTLE_CONSOLE_PASSWORD")
    out = castle_rpc(base_url, "castle.v1.ConsoleService/Login",
                     {"username": user, "password": pw}, token=None)
    tok = out.get("sessionToken")
    if not tok:
        raise common.Gate("console login returned no sessionToken")
    return tok


# ---- ProjectService (console-session or orchestrator token as caller decides)

def get_project(base, token, project_id):
    return castle_rpc(base, "castle.v1.ProjectService/GetProject",
                      {"projectId": project_id}, token=token,
                      expected=(200, 404))


def project_exists(base, token, project_id):
    return get_project(base, token, project_id).get("_status") == 200


def list_projects(base, token):
    out = castle_rpc(base, "castle.v1.ProjectService/ListProjects", {}, token=token)
    return out.get("projects") or []


def create_project(base, token, project_id, membership_policy="PROJECT_MEMBERSHIP_POLICY_OPEN"):
    body = {"project": {"id": project_id, "membershipPolicy": membership_policy}}
    return castle_rpc(base, "castle.v1.ProjectService/CreateProject", body, token=token,
                      expected=(200, 409))


def list_project_members(base, token, project_id):
    out = castle_rpc(base, "castle.v1.ProjectService/ListProjectMembers",
                     {"projectId": project_id}, token=token,
                     expected=(200, 404))
    return out.get("members") or []


def add_project_member(base, token, project_id, member_id, kind="PROJECT_MEMBER_KIND_AI"):
    return castle_rpc(base, "castle.v1.ProjectService/AddProjectMember",
                      {"projectId": project_id, "memberId": member_id, "kind": kind},
                      token=token)


def get_project_config(base, token, project_id):
    """returns {document: <decoded JSON text>|'', etag, status}.
    Connect JSON encodes bytes fields as base64 — decode with strict validation
    so a plain-JSON server response also works."""
    import base64
    out = castle_rpc(base, "castle.v1.ProjectService/GetProjectConfig",
                     {"projectId": project_id}, token=token, expected=(200, 404))
    raw = out.get("document") or ""
    text = ""
    if raw:
        if raw.lstrip().startswith(b"{") if isinstance(raw, bytes) else raw.lstrip().startswith("{"):
            text = raw.decode() if isinstance(raw, bytes) else raw
        else:
            pad = raw + "=" * (-len(raw) % 4)
            text = base64.b64decode(pad).decode()
    return {"document": text, "etag": out.get("etag", ""), "status": out.get("_status")}


def set_project_config(base, token, project_id, document_text, expected_etag):
    body = {"projectId": project_id, "document": document_text.encode().decode("latin-1"),
            "expectedEtag": expected_etag}
    # bytes on the Connect JSON wire: base64 is standard, but castle decodes
    # bytes fields from base64 or plain strings; send as unicode passthrough
    import base64
    body["document"] = base64.b64encode(document_text.encode()).decode()
    return castle_rpc(base, "castle.v1.ProjectService/SetProjectConfig", body, token=token,
                      expected=(200, 409))


def get_project_settings(base, token, project_id):
    return castle_rpc(base, "castle.v1.ProjectService/GetProjectSettings",
                      {"projectId": project_id}, token=token, expected=(200, 404))


def set_project_settings(base, token, project_id, settings, expected_etag):
    body = {"projectId": project_id, "settings": settings, "expectedEtag": expected_etag}
    return castle_rpc(base, "castle.v1.ProjectService/SetProjectSettings", body, token=token,
                      expected=(200, 409))


def list_workflow_catalog(base, token, project_id):
    out = castle_rpc(base, "castle.v1.ProjectService/ListWorkflowCatalog",
                     {"projectId": project_id}, token=token, expected=(200, 404))
    return out.get("entries") or []
