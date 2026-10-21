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
//      operator: drift there would silently change the admission contract);
//   5. every type=url workflowLibrary entry's binding (adapterImages,
//      volumes, secrets' env maps, env) satisfies its tree's interface
//      declaration under the same fail-closed rules the operator applies
//      (interfaceBindingApplies/validateInterfaceBinding, KB-310): four
//      refused bindings shipped because nothing ran these rules, so the
//      shipped example payload is now conformance-checked like a real CR;
//   6. the conformance rules themselves are exercised by two engineered
//      negative fixtures (an undeclared extra mount, a required secret
//      lane the binding does not resolve) and one positive conforming
//      fixture — a mirror that stops firing must fail loudly here, not
//      silently re-admit what the operator refuses.
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

// One grammar everywhere (KB-310 finding 5: the label and mountPath rules
// previously lived in four divergent copies). These are the Go definitions
// from criteria-k8s/internal/routes/routes.go (interfaceNameRe,
// interfacePathRe) held byte-identical; the parity block at the bottom of
// this file fails the build when any of the four surfaces drifts again.
const LABEL = new RegExp("^[A-Za-z][A-Za-z0-9_-]*$");
const PATH_RE = new RegExp("^/.+[^/]$");
const SECRET_ENV = /^[A-Z][A-Z0-9_]*$/;
const ENV_NAME = /^[A-Z][A-Z0-9_]*$/;
const SLOT_NAME = /^[a-z][a-zA-Z0-9_]*$/;

// labelViolation renders the violation message for a label-shaped name
// (tree workflow, adapter kind, mount name) or null when the name conforms.
function labelViolation(value) {
  if (typeof value !== "string" || !LABEL.test(value) || value.length > 128)
    return "must be a 1-128 char name";
  return null;
}

function vLabel(value, where, what) {
  const violation = labelViolation(value);
  if (violation) fail(where, `${what} ${violation} (got ${JSON.stringify(value)})`);
}

// mountPathViolation renders the violation message for a mountPath or null
// when the path conforms. The null-byte guard is defense in depth beyond
// the shared regexp (a NUL would sail through the regexes in every
// language and corrupt the mount target).
function mountPathViolation(value) {
  if (typeof value !== "string" || !PATH_RE.test(value) || value.includes("\0"))
    return "must be an absolute container path without trailing slash";
  return null;
}

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
      const pathViolation = mountPathViolation(m.mountPath);
      if (pathViolation)
        fail(where, `mounts[${i}].mountPath ${pathViolation}`);
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

// ── binding conformance: the shipped bindings must satisfy their own tree's
// interface (KB-310) ─────────────────────────────────────────────────────────
//
// bindingViolations is the JS mirror of criteria-k8s admission
// (internal/controller/interfacebinding.go validateInterfaceBinding): the
// two must stay rule-equivalent, and the messages name both sides — the
// tree (via the interface workflow) and the binding entry — so a refused
// entry is troubleshootable from the test output alone.

function bindingViolations(iface, binding) {
  const mounts = iface.mounts ?? [];
  const adapters = iface.adapters ?? [];
  const secretEnv = iface.secretEnv ?? [];
  const ifaceEnv = iface.env ?? [];
  const wfName = binding.name ?? "<unnamed binding>";
  const listOrNone = (names) => (names.length === 0 ? "[] (none)" : `[${names.join(" ")}]`);
  const problems = [];

  // Adapter kinds: interface-required kinds ⊆ binding adapterImages;
  // extra kinds (a binding supplying more than the tree uses) tolerated.
  const provided = Object.keys(binding.adapterImages ?? {}).sort();
  for (const kind of adapters)
    if (!(kind in (binding.adapterImages ?? {})))
      problems.push(`interface "${iface.workflow}" requires adapter kind "${kind}"; binding "${wfName}" adapterImages provide ${listOrNone(provided)} (declare the missing kind's image for this tree, KB-310)`);

  // Mounts: every binding volume must match an interface mount byte-exact
  // on mountPath with the declared kind, and every interface mount must be
  // satisfied by a binding volume.
  const ifacePaths = new Map(mounts.map((m) => [m.mountPath, m]));
  const bindingPaths = new Map();
  const volumeList = [];
  for (const v of binding.volumes ?? []) {
    bindingPaths.set(v.mountPath, v);
    volumeList.push(`${v.name} (${v.kind} at ${v.mountPath})`);
    const declared = ifacePaths.get(v.mountPath);
    if (!declared)
      problems.push(`binding "${wfName}" supplies volume "${v.name}" (${v.kind} at ${v.mountPath}) which interface "${iface.workflow}" does not require; interface mounts: ${listOrNone(mounts.map((m) => `${m.name} (${m.kind} at ${m.mountPath})`))} — unknown-slot, fail closed (the declaration is the supported surface, KB-310)`);
    else if (v.kind !== declared.kind)
      problems.push(`binding "${wfName}" volume "${v.name}" wants kind "${v.kind}" at ${v.mountPath}, interface "${iface.workflow}" declares "${declared.kind}" (binding volumes and interface mounts must agree on kind, KB-310)`);
  }
  for (const declared of mounts) {
    const v = bindingPaths.get(declared.mountPath);
    if (!v)
      problems.push(`interface "${iface.workflow}" requires mount "${declared.name}" (${declared.kind}) at "${declared.mountPath}"; binding "${wfName}" volumes: ${listOrNone(volumeList)} (mount paths must match byte-exact, KB-310)`);
    else if (v.kind !== declared.kind)
      problems.push(`interface "${iface.workflow}" requires mount "${declared.name}" as "${declared.kind}" at "${declared.mountPath}"; binding "${wfName}" volume at that path is "${v.kind}" (KB-310)`);
  }

  // Secret env lanes: binding secret env keys ⊆ interface secretEnv ∪ env;
  // every interface-required secret env lane must resolve from the
  // binding's secrets env maps. A secret with an empty env map contributes
  // no keys (the CSI file-mount lane).
  const resolved = new Map();
  const secretList = [];
  for (const s of binding.secrets ?? []) {
    secretList.push(s.name);
    for (const name of Object.keys(s.env ?? {})) {
      if (!secretEnv.includes(name) && !ifaceEnv.includes(name))
        problems.push(`binding "${wfName}" secret "${s.name}" carries env "${name}" which interface "${iface.workflow}" does not require; interface secretEnv: ${listOrNone(secretEnv)}, env: ${listOrNone(ifaceEnv)} (unknown-slot, fail closed, KB-310)`);
      resolved.set(name, s.name);
    }
  }
  for (const lane of secretEnv)
    if (!resolved.has(lane))
      problems.push(`interface "${iface.workflow}" requires secret env "${lane}"; binding "${wfName}" secrets (${listOrNone(secretList)}) carry no such env key (wire the lane in the binding secret's env map, KB-310)`);

  // Env: binding env keys must be declared as interface env.
  for (const name of Object.keys(binding.env ?? {}))
    if (!ifaceEnv.includes(name))
      problems.push(`binding "${wfName}" env key "${name}" is not declared by interface "${iface.workflow}" (interface env: ${listOrNone(ifaceEnv)}, secretEnv: ${listOrNone(secretEnv)}); unknown-slot, fail closed (KB-310)`);

  return problems;
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
    // Resolve the tree from the url itself (the operator parses the same
    // .git//<tree> segment): the embedded interface must be the addressed
    // tree's declaration, not another tree's.
    const urlTree = typeof wf.url === "string" ? (/\.git\/\/([A-Za-z0-9_-]+)/.exec(wf.url) ?? [])[1] : undefined;
    if (typeof tree === "string" && urlTree && urlTree !== tree)
      fail(`k8s/examples/routes-configmap.yaml`,
        `workflowLibrary.${name}.url addresses tree "${urlTree}" but its embedded interface declares workflow "${tree}" (one of the two points at the wrong tree)`);
    if (typeof tree === "string")
      for (const problem of bindingViolations(embedded, { ...wf, name }))
        fail(`k8s/examples/routes-configmap.yaml (workflowLibrary.${name})`, problem);
  }
  for (const tree of trees) {
    const embedded = Object.entries(wfLibrary).find(([, wf]) => wf.interface && wf.interface.workflow === tree);
    if (!embedded) warn(`${tree} interface is not referenced by the example routes payload (subworkflow-only tree or not yet routed)`);
  }
}

// ── self-test: the conformance mirror must stay sharp (KB-310) ──────────────
//
// A rule that stops firing is exactly what shipped four refused bindings
// past CI, so bindingViolations is exercised here against engineered
// bindings: two negatives whose violations must fire, and one positive
// that must stay clean.
{
  const selfIface = {
    workflow: "selftest_tree",
    adapters: ["shell"],
    mounts: [{ name: "data", kind: "pvc", mountPath: "/data" }],
    secretEnv: ["LINEAR_API_KEY"],
    env: [],
  };
  const expectViolation = (label, binding, needle) => {
    const problems = bindingViolations(selfIface, binding);
    if (!problems.some((p) => p.includes(needle)))
      fail("scripts/interface_validate.mjs (self-test)",
        `binding-rule self-test "${label}" did not fire (expected a violation containing ${JSON.stringify(needle)}; got ${JSON.stringify(problems)}) — this mirror and the operator's validateInterfaceBinding have diverged, KB-310`,
        "keep bindingViolations rule-equivalent to criteria-k8s validateInterfaceBinding");
  };
  // A binding that supplies a volume the tree never declares → unknown-slot.
  expectViolation("undeclared-extra-mount", {
    name: "selftest_binding",
    volumes: [
      { name: "data", kind: "pvc", mountPath: "/data" },
      { name: "scratch", kind: "tmp", mountPath: "/tmp/scratch" },
    ],
    secrets: [{ name: "linear-api-key", env: { LINEAR_API_KEY: "linear_api_key" } }],
    env: {},
  }, `volume "scratch" (tmp at /tmp/scratch) which interface "selftest_tree" does not require`);
  // A binding that does not resolve a required secret env lane.
  expectViolation("missing-secret-lane", {
    name: "selftest_binding",
    volumes: [{ name: "data", kind: "pvc", mountPath: "/data" }],
    secrets: [{ name: "github-tokens", env: { OTHER_ENV_KEY: "other_key" } }],
    env: {},
  }, `requires secret env "LINEAR_API_KEY"`);
  // A fully conforming binding produces no violations (an extra copilot
  // kind the tree does not declare must be tolerated).
  const conformance = bindingViolations(selfIface, {
    name: "selftest_binding",
    adapterImages: { shell: "registry/criteria-adapter-shell-peer:latest", copilot: "registry/criteria-adapter-copilot-peer:latest" },
    volumes: [{ name: "data", kind: "pvc", mountPath: "/data" }],
    secrets: [{ name: "linear-api-key", env: { LINEAR_API_KEY: "linear_api_key" } }],
    env: {},
  });
  if (conformance.length !== 0)
    fail("scripts/interface_validate.mjs (self-test)",
      `binding-rule self-test "conforming-binding" flagged a compliant binding: ${JSON.stringify(conformance)} — the mirror is stricter than the operator, KB-310`);

  // The consolidated grammars reject the previously divergent inputs
  // (KB-310 finding 5: leading digit, dot, trailing slash, /a).
  const grammar = (label, helper, value, expectFire) => {
    const violation = helper(value);
    if ((violation !== null) !== expectFire)
      fail("scripts/interface_validate.mjs (self-test)",
        `grammar self-test "${label}" (${JSON.stringify(value)}) ${violation === null ? "accepted" : "rejected"} — expected ${expectFire ? "rejection" : "acceptance"}; the consolidated name/mountPath grammar loosened or tightened unexpectedly`);
  };
  grammar("label leading-digit", labelViolation, "0foo", true);
  grammar("label dot", labelViolation, "a.b", true);
  grammar("label plain tree name", labelViolation, "kanboard_develop_v1", false);
  grammar("mountPath trailing-slash", mountPathViolation, "/data/", true);
  grammar("mountPath single-segment /a", mountPathViolation, "/a", true);
  grammar("mountPath nul byte", mountPathViolation, "/data\0", true);
  grammar("mountPath plain", mountPathViolation, "/data", false);
  grammar("mountPath deep", mountPathViolation, "/home/criteria/workspace", false);
}

// ── regex parity: one definition, four copies (KB-310 finding 5) ────────────
// The interface name and mountPath grammars are each spelled out in four
// places (Go operator, this validator, the shell test, the JSON schema).
// They drifted apart once; this leg fails if any copy stops being
// byte-identical to the Go definition in internal/routes/routes.go.
{
  const mjsSrc = readFileSync(fileURLToPath(import.meta.url), "utf8");
  const goSrc = readFileSync(join(repoRoot, "criteria-k8s", "internal", "routes", "routes.go"), "utf8");
  const shellSrc = readFileSync(join(repoRoot, "k8s", "tests", "test_routes_config.sh"), "utf8");
  const schema = JSON.parse(readFileSync(join(repoRoot, "k8s", "workflow-interface.schema.json"), "utf8"));
  const grab = (label, src, re, file) => {
    const m = src.match(re);
    if (!m)
      fail("regex parity (KB-310 finding 5)", `${label} not found in ${file} — the parity block's extraction regexp is stale vs a renamed or reformatted definition`);
    return m ? m[1] : null;
  };
  const goName = grab("interfaceNameRe", goSrc, /interfaceNameRe = mustCompile\(`([^`]+)`\)/, "criteria-k8s/internal/routes/routes.go");
  const goPath = grab("interfacePathRe", goSrc, /interfacePathRe = mustCompile\(`([^`]+)`\)/, "criteria-k8s/internal/routes/routes.go");
  const shName = grab("INTERFACE_NAME_RE", shellSrc, /INTERFACE_NAME_RE = re\.compile\(r"([^"]+)"\)/, "k8s/tests/test_routes_config.sh");
  const shPath = grab("MOUNT_PATH_RE", shellSrc, /MOUNT_PATH_RE = re\.compile\(r"([^"]+)"\)/, "k8s/tests/test_routes_config.sh");
  const mjsName = grab("LABEL", mjsSrc, /const LABEL = new RegExp\("([^"]+)"\)/, "scripts/interface_validate.mjs");
  const mjsPath = grab("PATH_RE", mjsSrc, /const PATH_RE = new RegExp\("([^"]+)"\)/, "scripts/interface_validate.mjs");
  const schemaName = schema && schema.definitions && schema.definitions.label && schema.definitions.label.pattern;
  const schemaPath = schema && schema.properties && schema.properties.mounts && schema.properties.mounts.items && schema.properties.mounts.items.properties && schema.properties.mounts.items.properties.mountPath && schema.properties.mounts.items.properties.mountPath.pattern;
  const parity = (what, copies) => {
    if (new Set(copies).size !== 1)
      fail("regex parity (KB-310 finding 5)", `${what} drifted: ${JSON.stringify(copies)} — consolidate on the Go definition in criteria-k8s/internal/routes/routes.go so all four agree byte-exactly`);
    else if (copies[0] === null || copies[0] === undefined)
      fail("regex parity (KB-310 finding 5)", `${what} extraction failed`);
    else
      console.log(`interface-contract: parity: ${what} identical across go/js/shell/schema (${copies[0]})`);
  };
  parity("interface name grammar", [goName, shName, mjsName, schemaName]);
  parity("mountPath grammar", [goPath, shPath, mjsPath, schemaPath]);
}

if (errors.length > 0) {
  console.error(`interface-contract: ${errors.length} violation(s):`);
  for (const e of errors) console.error(`  - ${e}`);
  process.exit(1);
}
console.log(`interface-contract: PASS (${trees.length} trees validated, interface declarations + example routes payload consistent)`);