#!/usr/bin/env node
// Mechanical compile-expectations guard.
//
// docs/compile-expectations.md is the compile-expectations surface of
// record: per tree, the compile expectation and the engine version band it
// applies inside. This guard fails closed (exit 1) when the matrix is
// incomplete or self-inconsistent: a missing/duplicate tree row, a row with
// a malformed expectation or band cell, a tree directory that does not
// exist, an installed engine outside a row's band, or a live
// `criteria validate` of the row's tree contradicting the declared
// expectation. The harness_contracts_v1 row is the row of record —
// dropping or altering it fails closed (its regression shape: the probe
// tree must stay compile-compliant under the 0.5.57+ outcome-completeness
// verifier, and the engine re-derivation trap must fail loudly, never
// silently).
//
// The engine binary resolves from CRITERIA_BIN, then /usr/local/bin/criteria
// (the ONE-engine stamp the Makefile and CI install).
//
// Usage: node compile_matrix.mjs [--root <dir>] [--quiet]
// Exit 0: matrix sound. Exit 1: violations (listed). Exit 2: setup error.

import { spawnSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import process from "node:process";

const USAGE = "usage: node compile_matrix.mjs [--root <dir>] [--quiet]";
const DOC_REL = path.join("docs", "compile-expectations.md");

// The row of record: the tree the KB-367 regression pins, its expectation,
// and the band it applies inside. Both band edges are REQUIRED — an open
// ceiling would silently accept 0.6.x engines instead of failing the gate
// for the re-derivation.
const REQUIRED_ROW = {
  tree: "harness_contracts_v1",
  expectation: "ok",
  floor: "0.5.57",
  ceiling: "0.6.0",
};

function setupFail(msg) {
  process.stderr.write(`compile-matrix: ${msg}\n`);
  process.exit(2);
}

const problems = [];

// --- args ----------------------------------------------------------------

const argv = process.argv.slice(2);
let root = null;
let quiet = false;
for (let i = 0; i < argv.length; i++) {
  const a = argv[i];
  if (a === "--root") {
    if (i + 1 >= argv.length) setupFail(USAGE);
    root = argv[++i];
  } else if (a === "--quiet") {
    quiet = true;
  } else {
    setupFail(USAGE);
  }
}
if (!root) {
  root = path.resolve(path.dirname(fs.realpathSync(process.argv[1])), "..");
}

// --- load ----------------------------------------------------------------

const docPath = path.join(root, DOC_REL);
let doc;
try {
  doc = fs.readFileSync(docPath, "utf8");
} catch (e) {
  setupFail(`cannot read ${docPath}: ${e.message}`);
}

// --- engine version ------------------------------------------------------

const engineBin =
  process.env.CRITERIA_BIN && process.env.CRITERIA_BIN.trim() !== ""
    ? process.env.CRITERIA_BIN
    : "/usr/local/bin/criteria";

function engineVersion() {
  const r = spawnSync(engineBin, ["version"], { encoding: "latin1", timeout: 30_000 });
  const out = `${r.stdout ?? ""}${r.stderr ?? ""}`;
  const m = out.match(/v?(\d+)\.(\d+)\.(\d+)/);
  if (!m || r.status !== 0) return null;
  return [Number(m[1]), Number(m[2]), Number(m[3])];
}

function tuple(v) {
  return v.split(".").map((n) => Number(n));
}

function cmp(a, b) {
  for (let i = 0; i < 3; i++) {
    if (a[i] !== b[i]) return a[i] < b[i] ? -1 : 1;
  }
  return 0;
}

const engine = engineVersion();
if (!engine) {
  setupFail(`cannot determine the installed engine version (${engineBin} version)`);
}
const engineLabel = `${engine[0]}.${engine[1]}.${engine[2]}`;

// --- rows: `| `tree` | `expectation` | band | shape | carrier |` -----------

const rowRe = /^\| `([a-zA-Z0-9_-]+)` \| `([a-zA-Z]+)` \| `([^`]+)` \|/;
const rowsByTree = new Map(); // tree -> {expectation, band, line}
const duplicateTrees = new Set();
let malformedRows = 0;

for (const line of doc.split("\n")) {
  if (!line.startsWith("| `")) continue;
  const m = line.match(rowRe);
  const cells = line.split("|");
  if (!m || cells.length !== 7) {
    malformedRows++;
    continue;
  }
  const [_, tree, expectation, band] = m;
  if (rowsByTree.has(tree)) duplicateTrees.add(tree);
  else rowsByTree.set(tree, { expectation, band, line });
}

if (malformedRows > 0) {
  problems.push(
    `${malformedRows} malformed tree row(s): every row needs exactly ` +
    "five cells — tree, expectation (`ok`/`fail`), `>=floor, <ceiling` band, compiled shape, runtime carrier",
  );
}

// --- band parsing (fail closed on unparseable/open bands) ----------------

// The band cell is a comma-separated list of exact-version tokens, each
// either `>=X.Y.Z` (inclusive floor) or `<X.Y.Z` (exclusive ceiling). Both
// edges are required: an open ceiling would silently accept the next engine
// line instead of failing the gate for the re-derivation.
function parseBand(band) {
  let floor = null;
  let ceiling = null;
  for (const token of band.split(",").map((t) => t.trim())) {
    let m = token.match(/^>=(\d+\.\d+\.\d+)$/);
    if (m) {
      floor = m[1];
      continue;
    }
    m = token.match(/^<(\d+\.\d+\.\d+)$/);
    if (m) {
      ceiling = m[1];
      continue;
    }
    return { error: `band token \`${token}\` is not \`>=X.Y.Z\` or \`<X.Y.Z\`` };
  }
  if (!floor) return { error: "no `>=X.Y.Z` floor" };
  if (!ceiling) return { error: "no `<X.Y.Z` ceiling (an open band would silently accept the next engine line)" };
  return { floor, ceiling };
}

// --- assertions ----------------------------------------------------------

if (rowsByTree.size === 0 && malformedRows === 0) {
  problems.push("the matrix declares no tree rows — a compile-expectations matrix of record cannot be empty");
}

for (const tree of rowsByTree.keys()) {
  const row = rowsByTree.get(tree);
  const treeDir = path.join(root, tree);
  if (!fs.existsSync(treeDir) || !fs.statSync(treeDir).isDirectory()) {
    problems.push(`row for \`${tree}\` names a tree directory that does not exist`);
  }
  if (row.expectation !== "ok" && row.expectation !== "fail") {
    problems.push(`row for \`${tree}\`: expectation \`${row.expectation}\` is not \`ok\` or \`fail\``);
  }
  const band = parseBand(row.band);
  if (band.error) {
    problems.push(`row for \`${tree}\`: ${band.error} in band \`${row.band}\``);
    continue;
  }
  const installed = engine;
  if (cmp(tuple(band.floor), installed) > 0 || cmp(installed, tuple(band.ceiling)) >= 0) {
    problems.push(
      `row for \`${tree}\`: installed engine ${engineLabel} is outside the row's band \`${row.band}\`` +
      " — the expectation was derived inside this band and must be re-derived, never carried across",
    );
    continue;
  }
  // The declared expectation is asserted live against the installed engine.
  const r = spawnSync(engineBin, ["validate", tree], {
    cwd: root,
    encoding: "latin1",
    timeout: 300_000,
  });
  const compiledOk = r.status === 0;
  const declaredOk = row.expectation === "ok";
  if (compiledOk !== declaredOk) {
    problems.push(
      `row for \`${tree}\`: expectation \`${row.expectation}\` contradicts the observed compile result ` +
      `(validate exited ${r.status ?? "crashed"}: ${`${r.stdout ?? ""}${r.stderr ?? ""}`.trim().split("\n").pop() ?? ""})`,
    );
  }
}

for (const tree of duplicateTrees) {
  problems.push(`duplicate tree row for \`${tree}\` (a tree is asserted exactly once)`);
}

// The row of record must exist verbatim in tree, expectation and band —
// its regression shape (compile green under the 0.5.57 band, loud on the
// next engine line) is what the guard is for.
const required = rowsByTree.get(REQUIRED_ROW.tree);
if (!required) {
  problems.push(
    `the row of record for \`${REQUIRED_ROW.tree}\` is missing — ` +
    `dropping the row silently retires its regression shape`,
  );
} else {
  if (required.expectation !== REQUIRED_ROW.expectation) {
    problems.push(
      `the row of record's expectation is \`${required.expectation}\`, expected \`${REQUIRED_ROW.expectation}\``,
    );
  }
  const band = parseBand(required.band);
  if (band.error || band.floor !== REQUIRED_ROW.floor || band.ceiling !== REQUIRED_ROW.ceiling) {
    problems.push(
      `the row of record's band is \`${required.band}\`, expected \`${REQUIRED_ROW.floor}, ${REQUIRED_ROW.ceiling}\``,
    );
  }
}

// --- report --------------------------------------------------------------

if (problems.length > 0) {
  process.stderr.write(`compile-matrix: ${docPath}: ${problems.length} violation(s) on engine ${engineLabel}:\n`);
  for (const p of problems) process.stderr.write(`  - ${p}\n`);
  process.stderr.write("compile-matrix: fix the compile-expectations matrix of record; the guard reads it as its spec.\n");
  process.exit(1);
}
if (!quiet) {
  process.stdout.write(
    `compile-matrix: ${docPath} sound on engine ${engineLabel}: ` +
    `${rowsByTree.size} tree row(s) — ${[...rowsByTree.keys()].join(", ")} — ` +
    `every expectation asserted live against the installed engine\n`,
  );
}
