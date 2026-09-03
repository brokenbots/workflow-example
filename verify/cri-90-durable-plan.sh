#!/usr/bin/env bash
# Manual verification script for CRI-90: durable QA plan persistence and resume.
# Exercises the actual script templates used by qa_triage_v1 and linear_intake_v1,
# and verifies that both workflows compile.
set -euo pipefail

cd "$(dirname "$0")/.."

echo "==> Compiling target workflows"
criteria compile qa_triage_v1 >/dev/null
criteria compile linear_intake_v1 >/dev/null
echo "    compile: ok"

REPO_ROOT="$(pwd)"
export REPO_ROOT

python3 - <<'PY'
import os
import re
import shlex
import shutil
import subprocess
import tempfile

repo = os.environ["REPO_ROOT"]


def render(template_path, values):
    with open(template_path, "r", encoding="utf-8") as fh:
        text = fh.read()

    def repl(match):
        n = int(match.group(1))
        key = match.group(2)
        val = values.get(key, "")
        return f"criteria_value_{n}={shlex.quote(val)}"

    return re.sub(
        r'criteria_value_(\d+)={{ \.(\w+) \| shellquote }}',
        repl,
        text,
    )


def run_script(rendered, cwd=None):
    result = subprocess.run(
        ["bash", "-c", rendered],
        cwd=cwd,
        capture_output=True,
        text=True,
        check=False,
    )
    if result.returncode != 0:
        print("STDERR:", result.stderr)
        print("STDOUT:", result.stdout)
        raise RuntimeError("script exited non-zero")
    return result.stdout.strip()


tmp = tempfile.mkdtemp(prefix="cri90-verify-")
try:
    intake = os.path.join(tmp, "intake")
    triage = os.path.join(tmp, "triage")
    ticket_dir = os.path.join(intake, "CRI-90")
    run_dir = os.path.join(triage, "CRI-90")
    evidence_dir = os.path.join(run_dir, "evidence")
    os.makedirs(ticket_dir)
    os.makedirs(evidence_dir)

    plan_path = os.path.join(evidence_dir, "plan.md")
    with open(plan_path, "w", encoding="utf-8") as fh:
        fh.write("# Approved reproduction plan\n1. Build the project.\n2. Run the reproduction.\n")

    durable = os.path.join(ticket_dir, "approved-plan.md")

    # Persist the approved plan to durable storage.
    rendered = render(
        os.path.join(repo, "qa_triage_v1/scripts/persist_approved_plan.sh.tftpl"),
        {"run_dir": run_dir, "approved_plan_file": durable, "intake_root": intake},
    )
    out = run_script(rendered)
    assert out == durable, f"unexpected persist output: {out}"
    assert os.path.exists(durable), "approved plan was not persisted"
    with open(durable, "r", encoding="utf-8") as fh:
        content = fh.read()
    assert "Approved reproduction plan" in content, "persisted plan contents mismatch"

    # Load the approved plan on a simulated retry.
    os.remove(plan_path)
    rendered = render(
        os.path.join(repo, "qa_triage_v1/scripts/load_approved_plan.sh.tftpl"),
        {"run_dir": run_dir, "approved_plan_file": durable, "intake_root": intake},
    )
    out = run_script(rendered)
    assert out == "true", f"load did not detect persisted plan: {out}"
    assert os.path.exists(plan_path), "plan was not restored into run evidence"
    with open(plan_path, "r", encoding="utf-8") as fh:
        content = fh.read()
    assert "Approved reproduction plan" in content, "restored plan contents mismatch"

    # linear_intake_v1 detects the durable plan before classification.
    rendered = render(
        os.path.join(repo, "linear_intake_v1/scripts/check_approved_plan.sh.tftpl"),
        {"intake_root": intake, "ticket_id": "CRI-90"},
    )
    out = run_script(rendered)
    assert out == "true", f"check_approved_plan did not detect plan: {out}"

    # Removing the durable plan causes both checks to report false.
    os.remove(durable)
    rendered = render(
        os.path.join(repo, "qa_triage_v1/scripts/load_approved_plan.sh.tftpl"),
        {"run_dir": run_dir, "approved_plan_file": durable, "intake_root": intake},
    )
    out = run_script(rendered)
    assert out == "false", f"load should return false when plan missing: {out}"

    rendered = render(
        os.path.join(repo, "linear_intake_v1/scripts/check_approved_plan.sh.tftpl"),
        {"intake_root": intake, "ticket_id": "CRI-90"},
    )
    out = run_script(rendered)
    assert out == "false", f"check_approved_plan should return false when plan missing: {out}"

    print("CRI-90 durable-plan persistence/resume verification: PASS")
finally:
    shutil.rmtree(tmp)
PY
