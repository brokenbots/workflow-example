#!/usr/bin/env bash
# Positive-path sanity: a checkpoint verdict with a PUSHED branch flows
# criteria_value_2 (base_branch) into the verifier and verify_checkpoint_pushed
# exits 0 — i.e. the freshly plumbed base_branch renders into a real shell run
# without the var-resolution crash and without breaking benign/fatal shapes.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORKFLOW_DIR="${SCRIPT_DIR}/../workflows/pair_programming_loop"
TEMPLATE="${WORKFLOW_DIR}/scripts/verify_checkpoint_pushed.sh.tftpl"
WORK_DIR="$(mktemp -d)"
trap 'rm -rf "${WORK_DIR}"' EXIT
fail() { echo "FAIL: $1" >&2; exit 1; }

command -v python3 >/dev/null || fail "python3 required"
command -v criterion >/dev/null 2>&1 || true

# Render the .tftpl with both criteria values exactly as the step passes them
# (branch = simulated pushed branch name, base = 'main').
SHELLQUOTE() { python3 - "$1" <<'PY'
import shlex, sys
print(shlex.quote(sys.argv[1]))
PY
}
BRANCH="basebranch-sanity"
BASE="main"
python3 - "$BRANCH" "$BASE" "$TEMPLATE" > "${WORK_DIR}/verify.sh" <<'PY'
import re, shlex, sys
branch, base, tpl = sys.argv[1], sys.argv[2], sys.argv[3]
text = open(tpl).read()
text = re.sub(r'\{\{\s*\.criteria_value_1\s*\|\s*shellquote\s*\}\}', shlex.quote(branch), text)
text = re.sub(r'\{\{\s*\.criteria_value_2\s*\|\s*shellquote\s*\}\}', shlex.quote(base), text)
if 'criteria_value_2' in text and 'criteria_value_2=' not in text.split('set -euo')[0]:
    sys.exit("template lost the criteria_value_2 render")
sys.stdout.write(text)
PY
chmod +x "${WORK_DIR}/verify.sh"

# Build a deterministic origin/HEAD shape: HEAD has 1 commit beyond base,
# branch IS on the remote, heads match -> exit 0 (pushed-checkpoint path).
git init -q "${WORK_DIR}/repo"
cd "${WORK_DIR}/repo"
git config user.email t@t; git config user.name t
git commit -q --allow-empty -m base
git branch -q main 2>/dev/null || true
git checkout -q -b "${BRANCH}"
git commit -q --allow-empty -m wip
# local file remote: push the branch
git init -q --bare "${WORK_DIR}/origin.git"
git remote add origin "${WORK_DIR}/origin.git"
git push -q origin "${BRANCH}"
HEAD_SHA="$(git rev-parse HEAD)"
REMOTE_SHA="$(git ls-remote origin "refs/heads/${BRANCH}" | awk '{print $1}')"
[ "${HEAD_SHA}" = "${REMOTE_SHA}" ] || fail "fixture broken: head mismatch"

OUT="$(bash "${WORK_DIR}/verify.sh")" || fail "verify exit non-zero on pushed branch (stdout: ${OUT})"
echo "PASS: pushed-checkpoint path renders base_branch and exits 0 (${OUT})"