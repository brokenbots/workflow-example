#!/usr/bin/env node
// Mechanical matrix-of-record guard (KB-254 — the harness series anchor).
//
// docs/harness-expectations.md is the validation surface of record for the
// workflow test harness series (cards 276-280): 37 base items (A1-A6,
// B7-B12, C13-C18, D19-D23, E24-E32, F33-F37) plus 12 subworkflow/composition
// items (G1-G12). This guard fails closed when the matrix is incomplete or
// self-inconsistent: a missing/duplicate/unexpected item id, a wrong count,
// a series-map slice assignment missing an expected id, an item with no
// owning card, a KNOWN GAP without its pinned engine-carrier marker, or a
// conventions fact dropped from the of-record sections — plus a malformed
// item row (empty assertion/carrier cells). Fully mechanical: the doc is
// parsed, never hand-listed here, so the matrix cannot drift silently.
//
// Usage: node harness_matrix.mjs [--root <dir>] [--quiet]
// Exit 0: matrix sound. Exit 1: violations (listed). Exit 2: setup error.

import fs from "node:fs";
import path from "node:path";
import process from "node:process";

const USAGE = "usage: node harness_matrix.mjs [--root <dir>] [--quiet]";
const DOC_REL = path.join("docs", "harness-expectations.md");

function setupFail(msg) {
  process.stderr.write(`harness-matrix: ${msg}\n`);
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

const lines = doc.split("\n");

// --- expected item id space (37 base + 12 composition) -------------------

const RANGE = { A: [1, 6], B: [7, 12], C: [13, 18], D: [19, 23], E: [24, 32], F: [33, 37], G: [1, 12] };
const BASE_IDS = [];
for (const letter of ["A", "B", "C", "D", "E", "F"]) {
  for (let n = RANGE[letter][0]; n <= RANGE[letter][1]; n++) BASE_IDS.push(`${letter}${n}`);
}
const G_IDS = [];
for (let n = RANGE.G[0]; n <= RANGE.G[1]; n++) G_IDS.push(`G${n}`);
const EXPECTED_IDS = [...BASE_IDS, ...G_IDS];
if (EXPECTED_IDS.length !== 49) setupFail(`internal: expected id space is ${EXPECTED_IDS.length}, not 49`);

// --- item rows: `| **A1** | assertion | carrier |` ------------------------

const rowRe = /^\| \*\*([A-Z]\d+)\*\*/;
const itemRows = new Map(); // id -> line (first occurrence)
const observedIds = new Set(); // every distinct id the row shape matched, expected or not
const duplicateIds = new Set();
const unknownIds = new Set();
const malformedRows = new Set();
let parsedRowCount = 0;

for (const line of lines) {
  const m = line.match(rowRe);
  if (!m) continue;
  parsedRowCount++;
  const id = m[1];
  observedIds.add(id);
  if (!EXPECTED_IDS.includes(id)) unknownIds.add(id);
  if (itemRows.has(id)) duplicateIds.add(id);
  else itemRows.set(id, line);
  // 3-column item table: cells[1]=id, [2]=assertion, [3]=stimulus/carrier.
  const cells = line.split("|");
  const assertion = cells[2]?.trim() ?? "";
  const carrier = cells[3]?.trim() ?? "";
  if (assertion.length < 20 || carrier.length < 20) malformedRows.add(id);
}

if (parsedRowCount !== EXPECTED_IDS.length) {
  problems.push(
    `observed item row count is ${parsedRowCount}, expected exactly ${EXPECTED_IDS.length} ` +
    `(id space is bounded: ${DOC_REL} declares 37 base + 12 G items)`,
  );
}

for (const id of EXPECTED_IDS) {
  if (!itemRows.has(id)) problems.push(`missing item row for **${id}** in ${DOC_REL}`);
}
for (const id of duplicateIds) {
  problems.push(`duplicate item row for **${id}** (item ids must appear exactly once as a table-row marker)`);
}
for (const id of unknownIds) {
  problems.push(`unexpected item id **${id}** (id space is bounded: ${DOC_REL} declares 37 base + 12 G items)`);
}
for (const id of malformedRows) {
  problems.push(`item row **${id}** has an empty/underspecified assertion or carrier cell (every item needs a stated carrier)`);
}

// --- series map: card / title / tree / slice ------------------------------

const sectionSlice = (title) => {
  const start = lines.findIndex((l) => l.trim() === title);
  if (start === -1) return null;
  let end = start + 1;
  while (end < lines.length && !lines[end].startsWith("## ")) end++;
  return lines.slice(start, end).join("\n");
};

const seriesSection = sectionSlice("## Series map");
if (!seriesSection) problems.push("missing `## Series map` section");

const CARDS = [
  {
    id: "276", title: "harness/wire", tree: "harness_wire_v1",
    ids: ["A1", "A2", "A3", "A4", "A5", "A6", "B7", "B8", "B9", "B10", "B11", "B12"],
  },
  {
    id: "277", title: "harness/contracts", tree: "harness_contracts_v1",
    ids: ["C13", "C14", "C15", "C16", "C17", "C18", "D19", "D21", "D22"],
  },
  { id: "278", title: "harness/composition", tree: "harness_composition_v1", ids: G_IDS },
  {
    id: "279", title: "harness/failures", tree: "harness_failures_v1",
    ids: [...Array.from({ length: 9 }, (_, i) => `E${24 + i}`), "G12"],
  },
  {
    id: "280", title: "harness/fleet-smoke", tree: "harness_fleet_smoke_v1",
    ids: ["A1", "A2", "F33", "F34", "F35", "F36", "F37"],
  },
];

const cardSliceCell = new Map(); // card id -> slice cell text
if (seriesSection) {
  for (const line of seriesSection.split("\n")) {
    const cells = line.split("|").map((c) => c.trim());
    if (cells.length < 5) continue;
    if (!/^\d{3}$/.test(cells[1])) continue;
    cardSliceCell.set(cells[1], cells[4] ?? "");
    const expectedCard = CARDS.find((c) => c.id === cells[1]);
    if (!expectedCard) {
      problems.push(`series map lists card ${cells[1]} which is not a filed harness card (${CARDS.map((c) => c.id).join(", ")})`);
      continue;
    }
    if (cells[2].replace(/`/g, "") !== expectedCard.title) {
      problems.push(`series map row ${cells[1]}: title is \`${cells[2]}\`, expected \`${expectedCard.title}\``);
    }
    if (cells[3].replace(/`/g, "") !== expectedCard.tree) {
      problems.push(`series map row ${cells[1]}: tree is \`${cells[3]}\`, expected \`${expectedCard.tree}\``);
    }
  }
  for (const card of CARDS) {
    if (!cardSliceCell.has(card.id)) {
      problems.push(`series map is missing card ${card.id} (${card.title});`);
      continue;
    }
    const slice = cardSliceCell.get(card.id);
    for (const id of card.ids) {
      if (!slice.includes(`\`${id}\``)) {
        problems.push(`series map slice for card ${card.id} (${card.title}) does not assign \`${id}\``);
      }
    }
  }
}

// every item is owned: by a card slice, or (D20/D23) by the standing
// develop leg — which must be stated, never dropped silently.
const LEG_CARRIED = ["D20", "D23"];
for (const id of EXPECTED_IDS) {
  if (duplicatedOrFailed(itemRows, id)) continue;
  const ownedByCard = CARDS.some((c) => cardSliceCell.get(c.id)?.includes(`\`${id}\``));
  if (ownedByCard) continue;
  const row = itemRows.get(id) ?? "";
  if (LEG_CARRIED.includes(id) && row.includes("standing develop leg") && row.includes("not a harness tree")) {
    continue;
  }
  if (!LEG_CARRIED.includes(id)) {
    problems.push(`item **${id}** is assigned to no card slice and carries no standing-develop-leg marker`);
  } else {
    problems.push(`item **${id}** is not owned by any card and is missing its \`(standing develop leg, not a harness tree)\` marker`);
  }
}

function duplicatedOrFailed(rows, id) {
  return duplicateIds.has(id) || unknownIds.has(id) || !rows.has(id);
}

// --- known gaps pinned (composition G11/G12) ------------------------------

for (const id of ["G11", "G12"]) {
  const row = itemRows.get(id) ?? "";
  if (duplicatedOrFailed(itemRows, id)) continue;
  if (!row.includes("KNOWN GAP")) {
    problems.push(`item **${id}** must be pinned as a KNOWN GAP (it is a known defect of the engine, never silently fixed by a harness card)`);
  }
  if (!row.includes("engine carrier") || !row.includes("files back to the engine carrier")) {
    problems.push(`item **${id}** must state its pinned engine carrier and that a fix files back to the engine carrier, never to a harness card`);
  }
}

// --- conventions of record -----------------------------------------------

const FACTS = [
  ["route key naming", "harness-<leg>-url"],
  ["route entry naming", "route name `harness-<leg>`"],
  ["validation tag category", "harness-validation"],
  ["validation tag dispatch", "harness-<leg>"],
  ["queue class stamp", 'class = "triage"'],
  ["acceptance rule", "expectation slice observed in run events"],
  ["defect-filing rule", "Defects file back to their own carriers"],
  ["develop leg remains the gate", "REMAINS the acceptance gate"],
  ["subtree re-pin rule", "ADR-0005"],
  ["canary runs before develop legs", "BEFORE any develop leg"],
];
for (const [fact, needle] of FACTS) {
  if (!doc.includes(needle)) {
    problems.push(`conventions of record lost the ${fact} fact (expected the phrase \`${needle}\` in ${DOC_REL})`);
  }
}
for (const card of CARDS) {
  if (!doc.includes(card.tree)) {
    problems.push(`conventions of record lost the declared tree name \`${card.tree}\``);
  }
}
// the carrier vocabulary items A2/A4/A6/F-item assertions depend on:
for (const carrierKey of ["provision_wanted", "release", "status.conditions", "status.Queue.Position"]) {
  if (!doc.includes(carrierKey)) {
    problems.push(`run-event carrier vocabulary lost \`${carrierKey}\``);
  }
}

// --- report ----------------------------------------------------------------

if (problems.length > 0) {
  process.stderr.write(`harness-matrix: ${docPath}: ${problems.length} violation(s):\n`);
  for (const p of problems) process.stderr.write(`  - ${p}\n`);
  process.stderr.write("harness-matrix: fix the matrix of record; the harness series reads it as its spec.\n");
  process.exit(1);
}
if (!quiet) {
  process.stdout.write(
    `harness-matrix: ${docPath} sound: 49/49 items (37 base + 12 G), ` +
    `${CARDS.map((c) => `${c.id}=${c.title}`).join(", ")}, 2 standing-leg carriers (D20, D23), 2 pinned KNOWN GAPS (G11, G12)\n`,
  );
}