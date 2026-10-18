// Shims the tree's adapters.chcl for local fixture runs (KB-277 standalone
// regression suite): points the copilot adapter at the scripted stub and the
// remote environment at a local shell environment. Transforms a COPY of the
// tree only — the shipped adapters.chcl in the repo tree is never touched.
//
//   argv[1] = source adapters.chcl (the fixture copy's file)
//   argv[2] = destination adapters.chcl (overwrites in place in the copy)
//   STUBPORT = port the fixture stub is listening on
//   PROBEROOT = working directory for the shimmed shell environment
import fs from "node:fs";

const [, , src, dst] = process.argv;
const stubPort = process.env.STUBPORT;
const probeRoot = process.env.PROBEROOT;
if (!src || !dst || !stubPort || !probeRoot) {
  console.error("usage: node adapters_shim.mjs <src.chcl> <dst.chcl>  (env STUBPORT, PROBEROOT)");
  process.exit(2);
}

let s = fs.readFileSync(src, "utf8");
s = s.replace(/environment = remote\.probe_members/g, "environment = shell.shimlocal");
s = s.replace(
  /model         = "claude-sonnet-4\.5"/,
  'model         = "stub-model"\n' +
    '        provider_base_url = "http://127.0.0.1:' + stubPort + '/v1"\n' +
    '        provider_wire_api = "responses"',
);
s += '\nenvironment "shell" "shimlocal" {\n    working_directory = "' + probeRoot + '"\n}\n';
fs.writeFileSync(dst, s);