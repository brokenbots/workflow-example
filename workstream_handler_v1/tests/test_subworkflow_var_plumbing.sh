#!/usr/bin/env bash
# Regression test for the KB-96 launch failure: subworkflow var plumbing.
#
# pair_programming_loop push_checkpoint referenced var.base_branch, but
# pair_programming_loop/variables.chcl declares no such variable and neither
# workstream_handler_v1 subworkflow block (pair_programming_loop,
# pair_programming_loop_feedback) passed base_branch in its input map.
# criteria validate/compile check subworkflow input expressions lazily (the
# run-time child variable lookup is what exploded), so a green-CI wfex commit
# shipped a latent fault that crashed KB-96's run the first time
# push_checkpoint was entered ("This object does not have an attribute named
# \"base_branch\""). KB-95's run never entered push_checkpoint (its develop
# turn ended ready_for_review), which is how the fault survived a merge.
#
# Validates:
#   1. every var.X referenced in pair_programming_loop .chcl files is either
#      declared in its variables.chcl or passed by BOTH parent subworkflow
#      input maps (var usages inside .tftpl scripts are template
#      interpolation, out of scope here);
#   2. the two pairing sites pass base_branch explicitly (the pinned
#      regression);
#   3. the child declares variable "base_branch" so the plumb-in is
#      meaningful.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
HANDLER_DIR="${SCRIPT_DIR}/.."

python3 - "${HANDLER_DIR}" <<'PYCHECK'
import json, os, re, sys

handler = sys.argv[1]
main_chcl = os.path.join(handler, "main.chcl")
src = open(main_chcl).read()


def fail(msg):
    print(f"FAIL: {msg}", file=sys.stderr)
    sys.exit(1)


# Extract every subworkflow block: key, source dir, and input-map text.
blocks = []
for m in re.finditer(r'subworkflow\s+"([^"]+)"\s*\{(.*?)\n\}', src, re.S):
    key, body = m.group(1), m.group(2)
    srcm = re.search(r'source\s*=\s*"([^"]+)"', body)
    # input map: text between the first "input = {" and the line that closes
    # the block ("}" at column 0) — greedy enough for these maps.
    inpm = re.search(r'input\s*=\s*\{(.*)', body, re.S)
    blocks.append({
        "key": key,
        "source": srcm.group(1) if srcm else "",
        "input": inpm.group(1) if inpm else "",
    })

if len(blocks) < 3:
    fail(f"expected >=3 subworkflow blocks in {main_chcl}, parsed {len(blocks)}")

pairs = [b for b in blocks if b["source"].endswith("pair_programming_loop")]
if len(pairs) != 2:
    fail(f"expected exactly 2 pair_programming_loop subworkflow blocks, got {len(pairs)}")

# 1) var plumbing completeness for the pair loop child.
child_dir = os.path.join(handler, "workflows", "pair_programming_loop")
declared = set()
vars_chcl = os.path.join(child_dir, "variables.chcl")
if os.path.isfile(vars_chcl):
    declared = set(re.findall(r'variable\s+"([^"]+)"', open(vars_chcl).read()))
if "base_branch" not in declared:
    fail('pair_programming_loop/variables.chcl does not declare variable "base_branch"')

used = set()
for root, _dirs, files in os.walk(child_dir):
    for f in files:
        if not f.endswith(".chcl"):
            continue
        text = open(os.path.join(root, f)).read()
        used |= set(re.findall(r'var\.([a-zA-Z_][a-zA-Z0-9_]*)', text))

for b in pairs:
    for name in sorted(used):
        if name in declared:
            continue
        # passed by the parent? bare "<name> =" inside the input map
        if re.search(rf'^\s*{re.escape(name)}\s*=', b["input"], re.M):
            continue
        fail(f'pair subworkflow "{b["key"]}": var.{name} used in {child_dir} .chcl files '
             f'but not declared in its variables.chcl and not passed in the '
             f'"{b["key"]}" input map')

# 2) explicit pin: both pair blocks carry base_branch.
for b in pairs:
    if "base_branch" not in b["input"]:
        fail(f'pair subworkflow "{b["key"]}" does not pass base_branch in its input map')

print("PASS: pair_programming_loop var plumbing complete "
      "(base_branch declared and passed by both parent subworkflow blocks)")
PYCHECK