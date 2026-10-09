#!/usr/bin/env node
// Mechanical outcome-reason flow rule (KB-217).
//
// Any workflow-tree outcome arm whose adapter `reason` (or adapter outputs)
// transitively renders into another step's input/prompt — or into a
// subworkflow call input or a workflow output export — must declare
// `require_comment = true` on that arm, so the runtime outcome contract
// rejects empty reason payloads. Validation FAILS with file, step, arm and
// the fix hint when the declaration is missing.
//
// The rule is fully mechanical: no per-tree lists, no allowlists. Trees are
// discovered by walking the repo for main.chcl files (paths containing a
// `tests` segment — test fixtures — are excluded). The dependency is derived
// by a dataflow analysis over the tree source:
//
//   source  = an expression inside an outcome/default arm referencing
//             `output.<field>` (the executing adapter's reason/declared
//             outputs), or any expression referencing `steps.<S>.reason` /
//             `steps.<S>.outputs.<field>` (a sibling step's adapter reason)
//   flow    = arm-local `write { target = ..., value = ... }` bindings:
//             var-level taint propagated to fixpoint, so chains like
//             reason -> data.internal.a -> var.b are traced; the flag always
//             attributes back to the arm that wrote the reason in
//   sinks   = step `input` expressions, subworkflow `input = {...}` call
//             literals (data passed into a child tree's rendered input) and
//             workflow `output` blocks (reason exported across a tree
//             boundary)
//
// Cross-file note: a reason crossing a tree boundary always crosses at one of
// the two sink kinds in the producing file — a subworkflow call input in the
// consuming parent, or a workflow output in the producer — so per-file
// analysis suffices and the flag is always attributed at the producing arm
// (the root cause).
//
// Unknown/lexer-hostile HCL constructs (heredocs, template directives,
// unterminated literals) fail closed with a hard error instead of a silent
// wrong parse.
//
// Usage: node tree-outcome-flow.mjs [--root <dir>] [--quiet]
// Exit 0: no violations. Exit 1: violations (listed with fix hints).
// Exit 2: setup/parse error.

import fs from "node:fs";
import path from "node:path";
import process from "node:process";

const USAGE = "usage: node tree-outcome-flow.mjs [--root <dir>] [--quiet]";

function fail(msg) {
  process.stderr.write(`tree-outcome-flow: ${msg}\n`);
  if (process.env.TOF_DEBUG === "1" && debugCtx) {
    const { tokens, pos, file } = debugCtx;
    const lo = Math.max(0, pos - 4), hi2 = Math.min(tokens.length, pos + 6);
    for (let k = lo; k < hi2; k++) {
      const t = tokens[k];
      process.stderr.write(`    tok[${k}]${k === pos ? " <<<" : "    "} L${t.line} ${t.type} ${JSON.stringify(t.raw ?? t.value)}\n`);
    }
    void file;
  }
  process.exit(2);
}
// Parse debug context (tok window on failure), set while parsing.
let debugCtx = null;

function parseArgs(argv) {
  const args = { root: null, quiet: false };
  for (let i = 0; i < argv.length; i++) {
    if (argv[i] === "--root") args.root = argv[++i];
    else if (argv[i] === "--quiet") args.quiet = true;
    else fail(`${USAGE} (unexpected argument: ${argv[i]})`);
  }
  return args;
}

// ---------------------------------------------------------------------------
// Tokenizer.
// ---------------------------------------------------------------------------

const T_IDENT = "ident";
const T_STRING = "string";
const T_PUNCT = "punct";
const T_NL = "newline";

// String tokens carry `interpIdents`: identifier chains found inside their
// `${...}` interpolations (scanned with the same brace-aware loop, so nested
// strings and nested braces do not truncate them).
export function tokenize(src, file) {
  const tokens = [];
  const n = src.length;
  let i = 0;
  let line = 1;

  // Scans a quoted string starting at src[i] === '"'. Leaves i just past the
  // closing quote; pushes one string token carrying interpIdents. Raw
  // newlines inside a quoted string are rejected (this tree set uses neither
  // heredocs nor multiline strings; fail closed instead of misparsing).
  function scanQuoted() {
    const startLine = line;
    const startIdx = i;
    i++;
    const interpIdents = [];
    while (i < n) {
      const c = src[i];
      if (c === "\\") {
        if (i + 1 < n && src[i + 1] === "$") i += 2; // escaped "${" -> literal
        else if (i + 1 < n && src[i + 1] === "\n") fail(`${file}:${startLine}: unterminated string literal`);
        else i += 2;
        continue;
      }
      if (c === "%" && src[i + 1] === "{") {
        fail(`${file}:${startLine}: unsupported HCL template directive (%{...}%) — extend the lexer instead of guessing`);
      }
      if (c === "$" && src[i + 1] === "{") {
        i += 2;
        const bodyStart = i;
        let depth = 1;
        while (i < n && depth > 0) {
          const d = src[i];
          if (d === "{") depth++;
          else if (d === "}") depth--;
          else if (d === '"') {
            scanQuoted(); // nested string inside the interpolation
            continue;
          } else if (d === "\n") line++;
          i++;
        }
        if (depth !== 0) fail(`${file}:${startLine}: unterminated interpolation`);
        const body = src.slice(bodyStart, i - 1);
        for (const m of body.matchAll(/[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)+/g)) {
          interpIdents.push(m[0]);
        }
        continue;
      }
      if (c === '"') {
        i++;
        tokens.push({ type: T_STRING, value: "<string>", raw: src.slice(startIdx, i), line: startLine, interpIdents });
        return;
      }
      if (c === "\n") fail(`${file}:${startLine}: unterminated string literal`);
      i++;
    }
    fail(`${file}:${startLine}: unterminated string literal`);
  }

  while (i < n) {
    const c = src[i];
    if (c === "\n") {
      tokens.push({ type: T_NL, value: "\n", line });
      line++;
      i++;
      continue;
    }
    if (c === " " || c === "\t" || c === "\r") {
      i++;
      continue;
    }
    if (c === "#") {
      while (i < n && src[i] !== "\n") i++;
      continue;
    }
    if (c === "/" && src[i + 1] === "/") {
      while (i < n && src[i] !== "\n") i++;
      continue;
    }
    if (c === "/" && src[i + 1] === "*") {
      const startLine = line;
      const end = src.indexOf("*/", i + 2);
      if (end === -1) fail(`${file}:${startLine}: unterminated block comment`);
      for (let k = i; k < end; k++) if (src[k] === "\n") line++;
      i = end + 2;
      continue;
    }
    if (c === '"') {
      scanQuoted();
      continue;
    }
    if (/[A-Za-z_]/.test(c)) {
      let j = i;
      while (j < n && /[A-Za-z0-9_.]/.test(src[j])) j++;
      if (src[j] === "<" && src[j + 1] === "<") fail(`${file}:${line}: unsupported HCL construct (heredoc)`);
      tokens.push({ type: T_IDENT, value: src.slice(i, j), line });
      i = j;
      continue;
    }
    if ("{}()[]=,.?:!&|<>+-*/%".includes(c)) {
      if (c === "<" && src[i + 1] === "<") fail(`${file}:${line}: unsupported HCL construct (heredoc)`);
      tokens.push({ type: T_PUNCT, value: c, line });
      i++;
      continue;
    }
    if (/[0-9]/.test(c)) {
      let j = i;
      while (j < n && /[0-9a-zA-Z_.]/.test(src[j])) j++;
      tokens.push({ type: T_IDENT, value: src.slice(i, j), line });
      i = j;
      continue;
    }
    fail(`${file}:${line}: unexpected character ${JSON.stringify(c)}`);
  }
  return tokens;
}

// ---------------------------------------------------------------------------
// Block-tree parser.
// ---------------------------------------------------------------------------

// Block: { kind, name, line, attrs: [{key, tokens, line}], blocks: Block[] }
// Attribute values are token lists captured up to a depth-0 newline, so
// multi-line object literals survive intact.
const OPEN = new Set(["{", "(", "["]);
const CLOSE = new Map([["}", "{"], [")", "("], ["]", "["]]);

export function parseTokens(tokens, file) {
  let pos = 0;
  debugCtx = { tokens, get pos() { return pos; }, file };
  const at = (k) => tokens[pos + k];
  const skipNls = () => {
    while (at(0)?.type === T_NL) pos++;
  };
  const dequote = (h) => (h.type === T_STRING ? h.raw.slice(1, -1) : h.value);
  const describe = (hdrs, from) => {
    void hdrs; void from;
    const list = hdrs.slice(from).filter((h) => h.type !== T_NL);
    return list.map((h) => h.raw ?? h.value).join(" ");
  };

  function captureAttrValue(k) {
    // k points at "="; captures value tokens up to a depth-0 newline or the
    // closing brace of the enclosing block.
    const out = [];
    let depth = 0;
    let k2 = k + 1;
    while (tokens[k2]) {
      const t = tokens[k2];
      if (t.type === T_NL && depth === 0) break;
      if (t.type === T_PUNCT && OPEN.has(t.value)) depth++;
      if (t.type === T_PUNCT && CLOSE.has(t.value)) {
        if (depth === 0) break;
        depth--;
      }
      out.push(t);
      k2++;
    }
    return { tokens: out, next: k2 };
  }

  function parseHeader() {
    // Collects ident/string header tokens up to `{` or `=`, then dispatches
    // to a block or an attribute. Returns true if a construct was consumed.
    const startTok = at(0);
    const header = [];
    let k = pos;
    while (tokens[k] && tokens[k].type !== T_NL && !(tokens[k].type === T_PUNCT && (tokens[k].value === "{" || tokens[k].value === "="))) {
      header.push(tokens[k]);
      k++;
    }
    const nt = tokens[k];
    if (!nt || nt.type === T_NL) {
      if (header.length === 0) return false;
      fail(`${file}:${startTok.line}: malformed header near ${describe(header, 0)}`);
    }
    if (nt.type === T_PUNCT && nt.value === "{") {
      if (header.length === 0) fail(`${file}:${nt.line}: block without a header`);
      pos = k + 1;
      const child = parseBody("}");
      body.blocks.push({ kind: header[0].value, name: header[1] ? dequote(header[1]) : null, line: header[0].line, ...child });
      return true;
    }
    if (nt.type === T_PUNCT && nt.value === "=") {
      if (header.length === 0) fail(`${file}:${startTok.line}: attribute without a key`);
      const value = captureAttrValue(k);
      body.attrs.push({ key: header[header.length - 1].value, tokens: value.tokens, line: startTok.line, header: header.map((h) => h.raw ?? h.value) });
      pos = value.next;
      return true;
    }
    return false;
  }

  let body = { attrs: [], blocks: [] };
  function parseBody(closer) {
    const b = { attrs: [], blocks: [] };
    const saved = body;
    body = b;
    while (true) {
      skipNls();
      const t = at(0);
      if (!t) fail(`${file}: unexpected EOF inside block`);
      if (t.type === T_PUNCT && t.value === closer) {
        pos++;
        body = saved;
        return b;
      }
      if (t.type === T_PUNCT && (t.value === "}" || t.value === ")")) fail(`${file}:${t.line}: unexpected closing ${t.value}`);
      if (t.type === T_PUNCT) fail(`${file}:${t.line}: unexpected ${JSON.stringify(t.value)} in block body`);
      if (!parseHeader()) fail(`${file}:${t.line}: unexpected token ${JSON.stringify(t.raw ?? t.value)} in block body`);
    }
  }

  const root = body; // note: parseBody swaps `body`; root stays the outer one
  while (true) {
    skipNls();
    const t = at(0);
    if (!t) return root;
    if (t.type === T_PUNCT && t.value === "{") {
      pos++;
      const child = parseBody("}");
      root.blocks.push({ kind: "anonymous", name: null, line: t.line, ...child });
      continue;
    }
    if (t.type !== T_IDENT && t.type !== T_STRING) {
      fail(`${file}:${t.line}: unexpected top-level token ${JSON.stringify(t.raw ?? t.value)}`);
    }
    if (!parseHeader()) fail(`${file}:${t.line}: unexpected top-level token ${JSON.stringify(t.raw ?? t.value)}`);
  }
}

// ---------------------------------------------------------------------------
// Reference classification.
// ---------------------------------------------------------------------------

// {kind:"reason-self"} — `output.reason`: the executing arm's adapter reason
// {kind:"reason-step", step} — `steps.<S>.reason` / `steps.<S>.outputs.reason`
// {kind:"var", ns, name} — taint-carrying variable (var.x / data.internal.x / local.x)
// {kind:"opaque"} — cross-file subworkflow passthrough; not modelled
// null — not a data reference.
//
// Adapter output FIELDS other than `.reason` (e.g. `output.stdout`) are
// structured data validated by the arm's `schema`, not by the comment
// contract — require_comment gates the adapter's submitted reason only, so
// they are deliberately not reason sources here.
export function classifyRef(dotted) {
  const seg = dotted.split(".");
  if (seg[0] === "output" && seg[1] === "reason") return { kind: "reason-self" };
  if (seg[0] === "steps" && seg.length >= 3 && seg[2] === "reason") {
    return { kind: "reason-step", step: seg[1] };
  }
  if (seg[0] === "steps" && seg.length >= 4 && seg[2] === "outputs" && seg[3] === "reason") {
    return { kind: "reason-step", step: seg[1] };
  }
  // Write targets and reads address the same slot with an optional trailing
  // `.value` (data.internal.X.value vs data.internal.X) — normalize both.
  if (seg[0] === "var" && seg.length >= 2) return { kind: "var", ns: "var", name: seg.slice(1).join(".").replace(/\.value$/, "") };
  if (seg[0] === "data" && seg[1] === "internal" && seg.length >= 3) {
    return { kind: "var", ns: "data", name: seg.slice(2).join(".").replace(/\.value$/, "") };
  }
  if (seg[0] === "local" && seg.length >= 2) return { kind: "var", ns: "local", name: seg.slice(1).join(".").replace(/\.value$/, "") };
  if (seg[0] === "subworkflow") return { kind: "opaque" };
  return null;
}

// Dotted identifiers referenced by an expression token list, including
// interpolations embedded in string literals.
export function collectRefs(tokens) {
  const refs = [];
  for (const tok of tokens) {
    if (tok.type === T_IDENT) refs.push(tok.value);
    else if (tok.type === T_STRING) refs.push(...tok.interpIdents);
  }
  return refs;
}

const varKey = (ns, name) => `${ns}:${name}`;
// Write targets of the form data.internal.<X>.value address the same storage
// slot as reads of data.internal.<X>.
function normalizeVar(c) {
  return { ns: c.ns, name: c.name.replace(/\.value$/, "") };
}

// ---------------------------------------------------------------------------
// Per-file flow analysis.
// ---------------------------------------------------------------------------

function analyzeFile(treeFile) {
  const src = fs.readFileSync(treeFile, "utf8");
  const tokens = tokenize(src, treeFile);
  const root = parseTokens(tokens, treeFile);
  const violations = [];
  const seenKeys = new Set();

  const collectArms = (stepBlock) => stepBlock.blocks.filter((b) => b.kind === "outcome" || b.kind === "default");

  // ---- index steps -------------------------------------------------------
  const stepsByLeaf = new Map(); // leaf step name -> [{path, block, arms}]
  const allSteps = [];
  const walkSteps = (block, prefix) => {
    for (const b of block.blocks) {
      if (b.kind === "step") {
        const p = prefix ? `${prefix}.${b.name}` : (b.name ?? "?");
        const entry = { path: p, block: b, arms: collectArms(b) };
        allSteps.push(entry);
        const leaf = p.split(".").pop();
        if (!stepsByLeaf.has(leaf)) stepsByLeaf.set(leaf, []);
        stepsByLeaf.get(leaf).push(entry);
      }
      const nextPrefix = b.kind === "step" ? (prefix ? `${prefix}.${b.name}` : b.name) : prefix;
      walkSteps(b, nextPrefix);
    }
  };
  walkSteps(root, "");

  const armDesc = (arm, stepPath) => `${arm.kind} "${arm.name ?? "?"}" of step "${stepPath}"`;

  // ---- collect reason writes ---------------------------------------------
  // producers[vKey] = Set of producer descriptors:
  //   {type:"arm", arm, stepPath}          — this arm's own adapter reason
  //   {type:"step-arms", step}             — sibling step's adapter reason
  const producers = new Map();
  const writeEntries = [];

  const gatherWrites = (block, prefix) => {
    for (const b of block.blocks) {
      if (b.kind === "step") {
        const p = prefix ? `${prefix}.${b.name}` : b.name;
        for (const arm of collectArms(b)) {
          for (const w of arm.blocks) {
            if (w.kind === "write") writeEntries.push({ block: w, producer: { type: "arm", arm, stepPath: p } });
          }
        }
      }
      gatherWrites(b, b.kind === "step" ? (prefix ? `${prefix}.${b.name}` : b.name) : prefix);
    }
  };
  gatherWrites(root, "");

  function classifyWriteTarget(w) {
    const t = w.attrs.find((a) => a.key === "target");
    if (!t) return null;
    for (const ref of collectRefs(t.tokens)) {
      const c = classifyRef(ref);
      if (c?.kind === "var") return normalizeVar(c);
    }
    return null;
  }

  for (let pass = 0; pass < 25; pass++) {
    let changed = false;
    for (const { block: w, producer } of writeEntries) {
      const target = classifyWriteTarget(w);
      if (!target) continue;
      const valueTokens = w.attrs.find((a) => a.key === "value")?.tokens ?? [];
      const got = new Set();
      let any = false;
      for (const ref of collectRefs(valueTokens)) {
        const c = classifyRef(ref);
        if (!c) continue;
        if (c.kind === "reason-self") {
          got.add(producer);
          any = true;
        } else if (c.kind === "reason-step") {
          got.add({ type: "step-arms", step: c.step });
          any = true;
        } else if (c.kind === "var") {
          const prior = producers.get(varKey(c.ns, c.name));
          if (prior) {
            for (const p of prior) got.add(p);
            any = true;
          }
        }
      }
      if (!any) continue;
      const key = varKey(target.ns, target.name);
      const cur = producers.get(key) ?? new Set();
      const before = cur.size;
      for (const p of got) cur.add(p);
      if (cur.size > before) {
        producers.set(key, cur);
        changed = true;
      }
    }
    if (!changed) break;
    if (pass === 24) fail(`${treeFile}: write dependency graph did not settle`);
  }

  // ---- sinks ---------------------------------------------------------------
  const analyzeSinkRefs = (tokens, label) => {
    const set = new Map();
    const add = (p) => set.set(JSON.stringify(p), p);
    for (const ref of collectRefs(tokens)) {
      const c = classifyRef(ref);
      if (!c) continue;
      if (c.kind === "reason-self") {
        // Rare: a rendered input referencing the step's own prior reason
        // (loop/repair renders). Flagged against the step's own arms.
        add({ type: "sink-self", label });
      } else if (c.kind === "reason-step") {
        const st = stepsByLeaf.get(c.step) ?? [];
        if (!st.length) fail(`${treeFile}: ${label} references unknown step "${c.step}"`);
        for (const s of st) for (const arm of s.arms) add({ type: "arm", arm, stepPath: s.path });
      } else if (c.kind === "var") {
        const prior = producers.get(varKey(c.ns, c.name));
        if (prior) {
          for (const p of prior) add({ ...p, label });
          // Opaque passthrough already excluded upstream; unmodelled refs
          // contribute nothing here.
        }
      }
    }
    return { set };
  };

  const flattenBlock = (b) => {
    const out = [];
    for (const a of b.attrs) out.push(...a.tokens);
    for (const c of b.blocks) out.push(...flattenBlock(c));
    return out;
  };

  const walkSinks = (block, prefix) => {
    for (const b of block.blocks) {
      if (b.kind === "step") {
        const p = prefix ? `${prefix}.${b.name}` : b.name;
        const sinkHere = (label, tok) => {
          const { set } = analyzeSinkRefs(tok, label);
          if (set.size) flagSink(set, label, b);
        };
        const inputAttr = b.attrs.find((a) => a.key === "input");
        if (inputAttr) sinkHere(`input of step "${p}"`, inputAttr.tokens);
        const inputBlock = b.blocks.find((bb) => bb.kind === "input");
        if (inputBlock) sinkHere(`input of step "${p}"`, flattenBlock(inputBlock));
        walkSinks(b, p);
        continue;
      }
      if (b.kind === "subworkflow") {
        const inputAttr = b.attrs.find((a) => a.key === "input");
        if (inputAttr) {
          const label = `input of subworkflow call "${b.name}"`;
          const { set } = analyzeSinkRefs(inputAttr.tokens, label);
          if (set.size) flagSink(set, label, null);
        }
        continue;
      }
      if (b.kind === "output") {
        const valueAttr = b.attrs.find((a) => a.key === "value");
        if (valueAttr) {
          const label = `workflow output "${b.name}"`;
          const { set } = analyzeSinkRefs(valueAttr.tokens, label);
          if (set.size) flagSink(set, label, null);
        }
        continue;
      }
      walkSinks(b, prefix);
    }
  };

  function flagSink(set, label, stepBlock) {
    const arms = [];
    for (const p of set.values()) {
      if (p.type === "sink-self") {
        for (const arm of collectArms(stepBlock)) arms.push({ arm, stepPath: stepBlock.name ?? "?" });
      } else if (p.type === "arm") {
        arms.push({ arm: p.arm, stepPath: p.stepPath });
      } else if (p.type === "step-arms") {
        const st = stepsByLeaf.get(p.step) ?? [];
        if (!st.length) fail(`${treeFile}: sink "${label}" references unknown step "${p.step}"`);
        for (const s of st) for (const arm of s.arms) arms.push({ arm, stepPath: s.path });
      }
    }
    for (const { arm, stepPath } of arms) {
      const key = `${treeFile}:${arm.line}:${arm.name}:${label}`;
      if (seenKeys.has(key)) continue;
      seenKeys.add(key);
      violations.push({ file: treeFile, line: arm.line, desc: armDesc(arm, stepPath), sink: label, arm });
    }
  }

  walkSinks(root, "");

  return violations.sort((a, b) => a.line - b.line || a.desc.localeCompare(b.desc));
}

// ---------------------------------------------------------------------------
// Main.
// ---------------------------------------------------------------------------

function hasRequireComment(arm) {
  const attr = arm.attrs.find((a) => a.key === "require_comment");
  if (!attr) return false;
  return attr.tokens.some((t) => t.type === T_IDENT && t.value === "true");
}

function main() {
  const args = parseArgs(process.argv.slice(2));
  const defaultRoot = path.resolve(path.dirname(new URL(import.meta.url).pathname), "..");
  const rootDir = path.resolve(args.root ?? defaultRoot);

  const treeFiles = [];
  const walkFs = (dir) => {
    for (const e of fs.readdirSync(dir, { withFileTypes: true })) {
      if (e.isDirectory()) {
        walkFs(path.join(dir, e.name));
        continue;
      }
      if (e.name !== "main.chcl") continue;
      const p = path.join(dir, e.name);
      if (path.relative(rootDir, p).split(path.sep).includes("tests")) continue;
      treeFiles.push(p);
    }
  };
  walkFs(rootDir);
  treeFiles.sort();
  if (!treeFiles.length) fail(`no main.chcl files found under ${rootDir}`);

  const violations = [];
  for (const f of treeFiles) {
    violations.push(...analyzeFile(f));
  }

  const report = [];
  for (const v of violations) {
    if (hasRequireComment(v.arm)) continue; // compliant arm
    report.push(`  ${v.file}:${v.line}: ${v.desc} renders its adapter reason into ${v.sink}`);
    report.push(`    fix: add "require_comment = true" to that ${v.arm.kind} arm (a schema alone does not satisfy the rule)`);
  }

  if (report.length === 0) {
    if (!args.quiet) console.log(`outcome-reason flow rule: OK (${treeFiles.length} tree files, 0 violations)`);
    process.exit(0);
  }
  console.error(`outcome-reason flow rule: ${violations.filter((v) => !hasRequireComment(v.arm)).length} violation(s) across ${treeFiles.length} tree files`);
  for (const line of report) console.error(line);
  process.exit(1);
}

// Only auto-run when executed directly; imports (tests, debug harnesses) stay
// side-effect free.
const invoked = process.argv[1] ? path.resolve(process.argv[1]) : "";
const selfPath = new URL(import.meta.url).pathname;
if (invoked && invoked === selfPath) {
  main();
}