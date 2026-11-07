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

node - "$HANDLER_DIR" <<'JSCHECK'
const fs = require("fs");
const path = require("path");

const handler = process.argv[2];
const mainChcl = path.join(handler, "main.chcl");
const src = fs.readFileSync(mainChcl, "utf8");

function fail(msg) {
    console.error(`FAIL: ${msg}`);
    process.exit(1);
}

// Extract every subworkflow block: key, source dir, and input-map text.
const blocks = [];
const blockRe = /subworkflow\s+"([^"]+)"\s*\{([\s\S]*?)\n\}/g;
let m;
while ((m = blockRe.exec(src)) !== null) {
    const [, key, body] = m;
    const srcm = body.match(/source\s*=\s*"([^"]+)"/);
    // input map: text between the first "input = {" and the line that closes
    // the block ("}" at column 0) — greedy enough for these maps.
    const inpm = body.match(/input\s*=\s*\{([\s\S]*)/);
    blocks.push({
        key,
        source: srcm ? srcm[1] : "",
        input: inpm ? inpm[1] : "",
    });
}

if (blocks.length < 3) {
    fail(`expected >=3 subworkflow blocks in ${mainChcl}, parsed ${blocks.length}`);
}

const pairs = blocks.filter((b) => b.source.endsWith("pair_programming_loop"));
if (pairs.length !== 2) {
    fail(`expected exactly 2 pair_programming_loop subworkflow blocks, got ${pairs.length}`);
}

// 1) var plumbing completeness for the pair loop child.
const childDir = path.join(handler, "workflows", "pair_programming_loop");
let declared = new Set();
const varsChcl = path.join(childDir, "variables.chcl");
if (fs.existsSync(varsChcl)) {
    declared = new Set(
        [...fs.readFileSync(varsChcl, "utf8").matchAll(/variable\s+"([^"]+)"/g)].map((x) => x[1]),
    );
}
if (!declared.has("base_branch")) {
    fail('pair_programming_loop/variables.chcl does not declare variable "base_branch"');
}

const used = new Set();
(function walk(dir) {
    for (const e of fs.readdirSync(dir, { withFileTypes: true })) {
        const p = path.join(dir, e.name);
        if (e.isDirectory()) {
            walk(p);
        } else if (e.name.endsWith(".chcl")) {
            for (const u of fs.readFileSync(p, "utf8").matchAll(/var\.([a-zA-Z_][a-zA-Z0-9_]*)/g)) {
                used.add(u[1]);
            }
        }
    }
})(childDir);

for (const b of pairs) {
    for (const name of [...used].sort()) {
        if (declared.has(name)) continue;
        // passed by the parent? bare "<name> =" inside the input map
        if (new RegExp(`^\\s*${name}\\s*=`, "m").test(b.input)) continue;
        fail(`pair subworkflow "${b.key}": var.${name} used in ${childDir} .chcl files `
            + `but not declared in its variables.chcl and not passed in the `
            + `"${b.key}" input map`);
    }
}

// 2) explicit pin: both pair blocks carry base_branch.
for (const b of pairs) {
    if (!b.input.includes("base_branch")) {
        fail(`pair subworkflow "${b.key}" does not pass base_branch in its input map`);
    }
}

console.log("PASS: pair_programming_loop var plumbing complete "
    + "(base_branch declared and passed by both parent subworkflow blocks)");
JSCHECK