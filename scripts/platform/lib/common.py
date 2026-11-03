"""Shared plumbing for the platform/ setup tooling.

Design rules (docs/jobs-setup.md):
- stdlib only; no third-party deps.
- Components never import each other's logic; they may import lib/*.
- Deterministic: same inputs -> same actions; every write is behind an apply flag.
- All discovered live values (tokens, IPs, etags) come from the cluster/APIs
  at runtime, never hardcoded.
"""

import json
import os
import subprocess
import sys
import urllib.error
import urllib.request
import base64
import time

PLATFORM_ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))  # scripts/platform
REPO_ROOT = os.path.dirname(os.path.dirname(PLATFORM_ROOT))                  # workflow-example

DEFAULT_KUBECONFIG = os.path.expanduser("~/.kube/config")
# Castle Connect-RPC over h2c via the k8s Service. Resolved per-call via
# kubectl; kept as fallback for doc examples.
CASTLE_SVC_FALLBACK = "http://10.43.233.98:8080"
KANBOARD_RPC_FALLBACK = "http://catch.internal.nullop.io/kanboard/jsonrpc.php"


class Skip(Exception):
    """A precondition was not met and the step legitimately does nothing."""


class Gate(Exception):
    """A deterministic gate failed. Never auto-corrected; listed and reported."""


# ---------------------------------------------------------------- subprocess

def run(cmd, check=True, timeout=120, env=None, cwd=None):
    """Run a command, return stdout. Raises Gate on nonzero exit when check."""
    e = dict(os.environ)
    e.setdefault("KUBECONFIG", DEFAULT_KUBECONFIG)
    if env:
        e.update(env)
    try:
        r = subprocess.run(cmd, capture_output=True, text=True, timeout=timeout, env=e, cwd=cwd)
    except FileNotFoundError:
        raise Gate(f"command not found: {cmd[0]}")
    except subprocess.TimeoutExpired:
        raise Gate(f"command timed out after {timeout}s: {' '.join(cmd)}")
    if check and r.returncode != 0:
        raise Gate(f"command failed rc={r.returncode}: {' '.join(cmd)}\n  stderr: {r.stderr.strip()[:500]}")
    return r.stdout


def kubectl(*args, check=True, timeout=120):
    return run(["kubectl", *args], check=check, timeout=timeout)


# ---------------------------------------------------------------- state IO

def write_state(outdir, name, obj):
    os.makedirs(outdir, exist_ok=True)
    p = os.path.join(outdir, name)
    with open(p, "w") as f:
        json.dump(obj, f, indent=2, sort_keys=True)
    return p


def read_state(outdir, name):
    p = os.path.join(outdir, name)
    if not os.path.exists(p):
        raise Gate(f"state file missing: {p} (run the upstream component first)")
    with open(p) as f:
        return json.load(f)


# ---------------------------------------------------------------- HTTP

def http_post_json(url, body, headers=None, timeout=30, auth_basic=None):
    """POST JSON, return parsed JSON. Connect-RPC h2c endpoints work with
    plain urllib on this host (cleartext HTTP/1.1 to the Service IP)."""
    data = json.dumps(body).encode()
    hdrs = {"Content-Type": "application/json"}
    if headers:
        hdrs.update(headers)
    if auth_basic:
        tok = base64.b64encode(auth_basic.encode()).decode()
        hdrs["Authorization"] = "Basic " + tok
    req = urllib.request.Request(url, data=data, headers=hdrs, method="POST")
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            raw = resp.read().decode()
            status = resp.status
    except urllib.error.HTTPError as e:
        raw = e.read().decode()
        status = e.code
    if not raw.strip():
        return {"_status": status}
    try:
        out = json.loads(raw)
    except json.JSONDecodeError:
        raise Gate(f"non-JSON response from {url} (status {status}): {raw[:200]}")
    out["_status"] = status
    return out


# ---------------------------------------------------------------- discovery

def kanboard_token_from_pod():
    """The kanboard app token, sourced from the LIVE pod config (the CM copy
    historically diverges)."""
    out = kubectl("-n", "kanboard", "exec", "deploy/kanboard", "--",
                  "sed", "-n",
                  r"s/.*API_AUTHENTICATION_TOKEN', '\([^']*\)'.*/\1/p",
                  "/var/www/app/config.php", check=True, timeout=60)
    tok = out.strip()
    if not tok:
        raise Gate("kanboard token empty from live pod config.php")
    return tok


def kanboard_rpc(method, params, token=None, url=None):
    """Kanboard JSON-RPC. Param arity is per-method and STRICT in this build:
    getTask/getTaskTags take task_id ONLY (extra args = -32602)."""
    if token is None:
        token = kanboard_token_from_pod()
    if url is None:
        url = os.environ.get("KANBOARD_RPC_URL", KANBOARD_RPC_FALLBACK)
    # retry once on a malformed/empty body (transient php-fpm hiccups)
    for attempt in (1, 2):
        out = http_post_json(url, {"jsonrpc": "2.0", "method": method, "id": 1,
                                   "params": params},
                             auth_basic=f"jsonrpc:{token}", timeout=30)
        if "error" in out and out["error"].get("code") != 0:
            err = out["error"]
            raise Gate(f"kanboard {method}: {err.get('message')} (code {err.get('code')}) params={json.dumps(params)[:200]}")
        if "result" in out:
            return out["result"]
        if attempt == 1:
            continue
    raise Gate(f"kanboard {method}: no result and no error in response")


def castle_svc_url():
    """Resolve castle's ClusterIP at runtime (host has no cluster DNS)."""
    ip = kubectl("-n", "criteria-jobs", "get", "svc", "castle",
                 "-o", "jsonpath={.spec.clusterIP}", timeout=30).strip()
    port = kubectl("-n", "criteria-jobs", "get", "svc", "castle",
                   "-o", "jsonpath={.spec.ports[0].port}", timeout=30).strip()
    if not ip:
        raise Gate("castle svc not found in criteria-jobs")
    return f"http://{ip}:{port or 8080}"


def openbao_svc_url():
    ip = kubectl("-n", "default", "get", "svc", "openbao",
                 "-o", "jsonpath={.spec.clusterIP}", timeout=30).strip()
    if not ip:
        raise Gate("openbao svc not found in default")
    return f"http://{ip}:8200"


def openbao_criteria_kv():
    """Read the criteria/linear KV map (kanboard URL/token, gh tokens, castle
    token) through OpenBao's kubernetes auth using the criteria-runner SA.
    Returns the whole secret map."""
    sa_token = kubectl("-n", "criteria-jobs", "create", "token",
                       "criteria-runner", "--duration=10m", timeout=60).strip()
    login = http_post_json(openbao_svc_url() + "/v1/auth/kubernetes/login",
                           {"role": "criteria-jobs", "jwt": sa_token}, timeout=30)
    vt = (login.get("auth") or {}).get("client_token")
    if not vt:
        raise Gate(f"openbao kubernetes login failed: {json.dumps(login)[:300]}")
    secret = http_post_json(openbao_svc_url() + "/v1/criteria/data/linear", {},
                            headers={"X-Vault-Token": vt}, timeout=30)
    data = (secret.get("data") or {}).get("data") or {}
    if not data:
        raise Gate("openbao criteria/data/linear empty")
    return data


def k8s_secret_key(ns, secret, key):
    """Read one key from a k8s secret (base64-decoded)."""
    import base64
    raw = kubectl("-n", ns, "get", "secret", secret, "-o",
                  f"jsonpath={{.data.{key}}}", timeout=30)
    if not raw:
        raise Gate(f"secret {ns}/{secret} key {key} empty or missing")
    return base64.b64decode(raw).decode()


# ---------------------------------------------------------------- reporting

class Stage:
    """Per-stage printer + gate collector for one-pass failure reporting."""
    def __init__(self, name, apply_mode):
        self.name = name
        self.apply = apply_mode
        self.t0 = time.time()

    def ok(self, msg):
        print(f"  [{self.name}] ok: {msg}")

    def change(self, msg):
        print(f"  [{self.name}] {'CHANGED: ' if self.apply else 'WOULD: '}{msg}")

    def fail(self, msg):
        raise Gate(f"[{self.name}] {msg}")


def report(plan_name, stages, failures, apply_mode):
    print()
    if failures:
        print(f"{plan_name}: {len(failures)} gate failure(s) — ALL listed, fix then rerun:")
        for i, f in enumerate(failures, 1):
            print(f"  {i}. {f}")
        print(f"mode: {'APPLY' if apply_mode else 'DRY-RUN'} (nothing further this pass)")
        return 1
    print(f"{plan_name}: all stages {'applied' if apply_mode else 'verified (dry-run clean)'} "
          f"({stages} stage(s))")
    return 0
