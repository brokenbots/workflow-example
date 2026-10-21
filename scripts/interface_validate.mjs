#!/usr/bin/env node
// Workflow interface declaration contract (KB-310, ADR 307).
//
// Every shared workflow tree in this repo carries a machine-readable
// interface.json at its root declaring what it requires from a run binding
// (adapter kinds, mount paths, secret env lanes) and the config it honors
// from a project's per-repo ConfigMap (config slots). The criteria-k8s
// operator validates a binding against this declaration BEFORE job creation;
// this leg keeps the declarations honest:
//
//   1. every tree root with a main.chcl has an interface.json, structurally
//      valid against k8s/workflow-interface.schema.json (a dependency-free
//      validator mirroring the schema, same discipline as
//      k8s/tests/test_routes_config.sh);
//   2. the declared `workflow` matches the tree directory;
//   3. every config-slot `variable` is a variable the tree actually declares
//      in its .chcl sources (a slot pointing at an undeclared variable would
//      silently deliver nothing);
//   4. every type=url workflowLibrary entry in the shipped example routes
//      ConfigMap embeds an interface copy, the copy is structurally valid,
//      and when it names a repo tree the copy is byte-identical to that
//      tree's interface.json (the routes CM is the delivery vehicle to the
//      operator: drift there would silently change the admission contract).
//
// Engineered failure fixtures (paths carrying a `tests` segment) are
// excluded from discovery: they exercise the engine, not bindings, and
// intentionally declare no interface. Image-mode entries (linear-intake-v1)
// carry no interface by design (no repo tree exists for them).
//
// Exit 0: no violations. Exit 1: violations listed with file and fix hint.

import { readdirSync, statSync, readFileSync, existsSync } from "node:fs";
import { join, basename, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const repoRoot = join(dirname(fileURLToPath(import.meta.url)), "..");
const errors = [];
const warn = (msg) => console.warn(`interface-contract: WARN: ${msg}`);
const fail = (file, msg, hint) =>
  errors.push(`${file}: ${msg}${hint ? `\n  fix: ${hint}` : ""}`);

// ── structural validation mirroring k8s/workflow-interface.schema.json ──────

const LABEL = /^[A-Za-z0-9][A-Za-z0-9._-]*$/;
const SECRET_ENV = /^[A-Z][A-Z0-9_]*$/;
const ENV_NAME = /^[A-Z][A-Z0-9_]*$/;
const SLOT_NAME = /^[a-z][a-zA-Z0-9_]*$/;

// The platform slot surface (KB-310): the config keys a project binding
// may carry and the tree variable each is delivered as through the KB-103
// per-run config rendering. A declared slot outside this map has no
// delivery vehicle and is rejected by both this script and the operator.
const CONFIG_SLOT_VARIABLES = {
  buildCmd: "build_cmd",
  testCmd: "test_cmd",
  ciGateCmd: "ci_gate_cmd",
  baseBranch: "base_branch",
};

function vLabel(value, where, what) {
  if (typeof value !== "string" || !LABEL.test(value) || value.length > 128)
    fail(where, `${what} must be a 1-128 char name (got ${JSON.stringify(value)})`);
}

function validateInterface(iface, where) {
  if (typeof iface !== "object" || iface === null || Array.isArray(iface)) {
    fail(where, "interface must be a JSON object");
    return;
  }
  const allowed = ["apiVersion", "kind", "workflow", "description", "adapters", "mounts", "secretEnv", "env", "configSlots"];
  for (const key of Object.keys(iface))
    if (!allowed.includes(key)) fail(where, `unknown field "${key}" (schema: additionalProperties false)`);
  if (iface.apiVersion !== "criteria.brokenbots.dev/v1") fail(where, `apiVersion must be "criteria.brokenbots.dev/v1"`);
  if (iface.kind !== "WorkflowInterface") fail(where, `kind must be "WorkflowInterface"`);
  vLabel(iface.workflow, where, "workflow");
  if (typeof iface.description !== "undefined" && typeof iface.description !== "string")
    fail(where, "description must be a string");
  const arr = (v, name, what) => {
    if (typeof v === "undefined" || v === null) return [];
    if (!Array.isArray(v)) {
      fail(where, `${name} must be an array`);
      return null;
    }
    return v;
  };
  const unique = (list, name, file, keyFn) => {
    const seen = new Set();
    for (const item of list) {
      const k = keyFn(item);
      if (seen.has(k)) fail(file, `duplicate ${name} entry ${JSON.stringify(k)} (uniqueItems)`);
      seen.add(k);
    }
  };

  const adapters = arr(iface.adapters, "adapters", where);
  if (adapters) {
    adapters.forEach((a) => vLabel(a, where, "adapters[]"));
    unique(adapters, "adapter", where, (a) => a);
  }
  const mounts = arr(iface.mounts, "mounts", where);
  if (mounts) {
    mounts.forEach((m, i) => {
      if (typeof m !== "object" || m === null)
        return fail(where, `mounts[${i}] must be an object`);
      for (const k of Object.keys(m))
        if (!["name", "mountPath", "kind"].includes(k))
          fail(where, `mounts[${i}]: unknown field "${k}"`);
      vLabel(m.name, where, `mounts[${i}].name`);
      if (typeof m.mountPath !== "string" || !m.mountPath.startsWith("/") || m.mountPath.endsWith("/") || m.mountPath.includes("\0"))
        fail(where, `mounts[${i}].mountPath must be an absolute container path without trailing slash`);
      if (!["pvc", "tmp"].includes(m.kind))
        fail(where, `mounts[${i}].kind must be "pvc" or "tmp" (KB-310 scope, got ${JSON.stringify(m.kind)})`);
    });
    unique(mounts.filter((m) => m && typeof m === "object"), "mount", where, (m) => `${m.mountPath}|${m.kind}`);
    unique(mounts.filter((m) => m && typeof m === "object"), "mount name", where, (m) => m.name);
  }
  const secretEnv = arr(iface.secretEnv, "secretEnv", where);
  if (secretEnv) {
    secretEnv.forEach((s) => {
      if (typeof s !== "string" || !SECRET_ENV.test(s) || s.length > 128)
        fail(where, `secretEnv[] must be a SCREAMING_SNAKE env name (got ${JSON.stringify(s)})`);
    });
    unique(secretEnv, "secretEnv", where, (s) => s);
  }
  const env = arr(iface.env, "env", where);
  if (env) {
    env.forEach((e) => {
      if (typeof e !== "string" || !ENV_NAME.test(e) || e.length > 128)
        fail(where, `env[] must be a SCREAMING_SNAKE env name (got ${JSON.stringify(e)})`);
    });
    unique(env, "env", where, (e) => e);
  }
  const slots = arr(iface.configSlots, "configSlots", where);
  if (slots) {
    slots.forEach((s, i) => {
      if (typeof s !== "object" || s === null)
        return fail(where, `configSlots[${i}] must be an object`);
      for (const k of Object.keys(s))
        if (!["slot", "variable", "target", "description"].includes(k))
          fail(where, `configSlots[${i}]: unknown field "${k}"`);
      if (typeof s.slot !== "string" || !SLOT_NAME.test(s.slot) || s.slot.length > 128)
        fail(where, `configSlots[${i}].slot must be a camelCase ConfigMap data key (got ${JSON.stringify(s.slot)})`);
      if (!["buildCmd", "testCmd", "ciGateCmd", "baseBranch"].includes(s.slot))
        fail(where, `configSlots[${i}].slot "${s.slot}" is not a platform config key (declare only buildCmd, testCmd, ciGateCmd, or baseBranch — the keys a project binding can carry, KB-310)`);
      if (CONFIG_SLOT_VARIABLES[s.slot] && s.variable !== CONFIG_SLOT_VARIABLES[s.slot])
        fail(where, `configSlots[${i}] (${s.slot}).variable must be "${CONFIG_SLOT_VARIABLES[s.slot]}" (the KB-103 render delivers the value as that tree variable, KB-310)`);
      vLabel(s.variable, where, `configSlots[${i}].variable`);
      if (typeof s.target !== "string" || s.target.length === 0 || s.target.length > 256)
        fail(where, `configSlots[${i}].target must name the step/agent the slot lands in`);
      if (typeof s.description !== "undefined" && typeof s.description !== "string")
        fail(where, `configSlots[${i}].description must be a string`);
    });
    const valid = slots.filter((s) => s && typeof s === "object");
    unique(valid, "slot", where, (s) => s.slot);
    unique(valid, "slot variable", where, (s) => s.variable);
  }
}

// ── tree discovery (same discipline as tree-outcome-flow.mjs) ───────────────

function discoverTrees() {
  const trees = [];
  for (const entry of readdirSync(repoRoot, { withFileTypes: true })) {
    if (!entry.isDirectory() || entry.name.startsWith(".") || entry.name === "criteria-k8s" ||
        entry.name === "criteria-base" || entry.name === "k8s" || entry.name === "scripts" ||
        entry.name === "charts" || entry.name === "docs") continue;
    if (existsSync(join(repoRoot, entry.name, "main.chcl"))) trees.push(entry.name);
  }
  return trees.sort();
}

// ── tree variables: collect declared variable "<name>" across .chcl files ──

function declaredVariables(treeDir) {
  const vars = new Set();
  const walk = (dir) => {
    for (const entry of readdirSync(dir, { withFileTypes: true })) {
      const p = join(dir, entry.name);
      if (entry.isDirectory()) {
        if (entry.name === "tests") continue; // fixtures carry no contract
        walk(p);
      } else if (entry.name.endsWith(".chcl")) {
        const src = readFileSync(p, "utf8");
        for (const m of src.matchAll(/(?:^|\n)\s*variable\s+"([^"]+)"\s*\{/g)) vars.add(m[1]);
      }
    }
  };
  walk(treeDir);
  return vars;
}

// ── example routes ConfigMap: extract the routes.json data key ─────────────

function exampleRoutesPayload() {
  const example = join(repoRoot, "k8s", "examples", "routes-configmap.yaml");
  const src = readFileSync(example, "utf8");
  const start = src.indexOf("  routes.json: |");
  if (start === -1) fail(example, "no 'routes.json: |' data key found");
  let out = [];
  for (const line of src.slice(start).split("\n").slice(1)) {
    if (line.trim() === "---") break;
    if (/^  [a-z]/.test(line)) break;
    if (line.startsWith("    ") || line.trim() === "") out.push(line.slice(4));
  }
  return JSON.parse(out.join("\n"));
}

// ── main ─────────────────────────────────────────────────────────────────────

const trees = discoverTrees();
if (trees.length === 0) fail("repo root", "no workflow trees discovered (unexpected)");

for (const tree of trees) {
  const file = join(repoRoot, tree, "interface.json");
  if (!existsSync(file)) {
    fail(file, "tree carries no interface.json", `declare the tree's interface (k8s/workflow-interface.schema.json) so admission can validate bindings against it`);
    continue;
  }
  let iface;
  try {
    iface = JSON.parse(readFileSync(file, "utf8"));
  } catch (e) {
    fail(file, `invalid JSON: ${e.message}`);
    continue;
  }
  validateInterface(iface, file);
  if (iface && iface.workflow && iface.workflow !== tree) {
    fail(file, `workflow "${iface.workflow}" does not match tree directory "${tree}"`);
    continue; // slot crosscheck would attribute to the wrong tree
  }
  const vars = declaredVariables(join(repoRoot, tree));
  for (const slot of iface.configSlots ?? []) {
    if (!vars.has(slot.variable))
      fail(file, `config slot ${slot.slot} points at variable "${slot.variable}" which the tree does not declare`,
        `declare variable "${slot.variable}" in the tree (or point the slot at an existing variable)`);
  }
}

// Example routes payload: every type=url entry embeds a valid interface copy
let payload;
try {
  payload = exampleRoutesPayload();
} catch (e) {
  fail("k8s/examples/routes-configmap.yaml", `cannot parse routes.json data key: ${e.message}`);
  payload = null;
}
if (payload) {
  const wfLibrary = payload.workflowLibrary ?? {};
  for (const [name, wf] of Object.entries(wfLibrary)) {
    if (wf.type !== "url") continue; // image-mode entries carry no repo tree
    const embedded = wf.interface;
    if (!embedded) {
      fail("k8s/examples/routes-configmap.yaml",
        `workflowLibrary.${name} (type=url) carries no embedded interface`,
        `inline the referenced tree's interface.json so the operator stamps it onto the run CR`);
      continue;
    }
    validateInterface(embedded, `k8s/examples/routes-configmap.yaml (workflowLibrary.${name}.interface)`);
    const tree = embedded.workflow;
    if (typeof tree === "string" && trees.includes(tree)) {
      const onDisk = readFileSync(join(repoRoot, tree, "interface.json"), "utf8");
      if (JSON.stringify(JSON.parse(onDisk)) !== JSON.stringify(embedded))
        fail(`k8s/examples/routes-configmap.yaml`, `workflowLibrary.${name}.interface drifts from ${tree}/interface.json`,
          `copy the tree's interface.json verbatim into the routes payload entry`);
    }
  }
  for (const tree of trees) {
    const embedded = Object.entries(wfLibrary).find(([, wf]) => wf.interface && wf.interface.workflow === tree);
    if (!embedded) warn(`${tree} interface is not referenced by the example routes payload (subworkflow-only tree or not yet routed)`);
  }
}

if (errors.length > 0) {
  console.error(`interface-contract: ${errors.length} violation(s):`);
  for (const e of errors) console.error(`  - ${e}`);
  process.exit(1);
}
console.log(`interface-contract: PASS (${trees.length} trees validated, interface declarations + example routes payload consistent)`);