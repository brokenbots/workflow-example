#!/usr/bin/env node
// pair_v2_shim.mjs — shims the pair_loop_v2 tree COPY's adapters.chcl for the
// routing simulator (KB-277 adapters_shim lineage): points every copilot
// adapter at the scripted stub and BOTH environments (the remote per-scope
// worktree env AND the neutralized upstream_sync's simroot) at local shell
// environments. Transforms the copy only — the shipped adapters.chcl is never
// touched.
//
//   argv[1] = source adapters.chcl  argv[2] = destination  (in place in copy)
//   STUB_PORT = stub port; SIMROOT = scenario root (worktree/intake below it)
import fs from "node:fs";

const [, , src, dst] = process.argv;
const stubPort = process.env.STUB_PORT;
const simRoot = process.env.SIMROOT;
if (!src || !dst || !stubPort || !simRoot) {
  console.error("usage: node pair_v2_shim.mjs <src> <dst> (env STUB_PORT, SIMROOT)");
  process.exit(2);
}

let s = fs.readFileSync(src, "utf8");

// Every copilot adapter points at the stub (model irrelevant to the shim; the
// adapter CLI requires a model name — keep the configured one, override the
// provider only). provider_base_url in the tree is a var expression; the
// OVERRIDE is applied by pinning the vars on apply (SIM_ vars) — but the
// robust path is config-level: rewrite each provider_base_url expression to
// the stub URL so no var plumbing is needed.
s = s.replace(/provider_base_url *= *var\.[^\n]*/g,
  `provider_base_url = "http://127.0.0.1:${stubPort}/v1"`);
s = s.replace(/provider_base_url *= *"[^"]*"/g,
  `provider_base_url = "http://127.0.0.1:${stubPort}/v1"`);

// The remote per-scope worktree env -> a local shell env on the sim worktree.
s = s.replace(/environment = remote\.worktree/g, "environment = shell.sim_worktree");

// Declare the local shell environments (worktree for the body, simroot for
// the neutralized upstream_sync subworkflow adapter "shim" which references
// shell.simroot in the sim's own adapters.chcl).
s += `
environment "shell" "sim_worktree" {
    working_directory = "${simRoot}/worktree"
}
`;
fs.writeFileSync(dst, s);
