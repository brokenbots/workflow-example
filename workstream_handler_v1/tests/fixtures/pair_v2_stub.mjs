#!/usr/bin/env node
// pair_v2_stub.mjs — the scripted model-provider stub for the pair_loop_v2
// ROUTING simulator (workstream_handler_v1/tests/test_pair_loop_v2_routing.sh).
// KB-277's contracts_stub.mjs mechanics, re-keyed for the pair-v2 step graph:
// marker-based per-call dispatch on the serialized conversation input, the
// KB-277 committed-finalize-tail rule, compaction answered with plain text,
// the full responses-API wrapper shape, and the KB-263 payload-only lane.
//
// Markers (appended into the tree COPY's prompt templates by sim_lib.sh):
//   "PAIR-V2 SIM planner probe"          -> serve the plan JSON (PLAN_JSON env)
//   "PAIR-V2 SIM part brief p<pN>"       -> part chain: ONE real committed
//                                           shell edit, then submit part_ready
//   "PAIR-V2 SIM remediation probe p<pN>"-> submit remediated
// Env: STUB_PORT, PLAN_JSON, SIM_SEED, MARKER_*.

import http from "node:http";

const port = parseInt(process.env.STUB_PORT || "0", 10);
const planJson = process.env.PLAN_JSON || "{}";
const markerPlanner = process.env.MARKER_PLANNER || "PAIR-V2 SIM planner probe";
const markerPart = process.env.MARKER_PART || "PAIR-V2 SIM part brief p";
const markerRem = process.env.MARKER_REM || "PAIR-V2 SIM remediation probe p";
const simSeed = process.env.SIM_SEED || "";

let callIdx = 0;

const submit = (args) => ({
  id: "fc" + callIdx,
  type: "function_call",
  call_id: "c" + callIdx,
  name: "submit_outcome",
  arguments: JSON.stringify(args),
});
const bashCall = (command) => ({
  id: "ft" + callIdx,
  type: "function_call",
  call_id: "t" + callIdx,
  name: "bash",
  arguments: JSON.stringify({ command }),
});
const plain = (text) => ({
  id: "fm" + callIdx,
  type: "message",
  role: "assistant",
  content: [{ type: "output_text", text }],
});

// The part's DEVELOPER work: a real edit + real commit in the sim worktree so
// the tree's own verify script observes branch on KB-386SIM, HEAD != entry_sha
// (committed work), clean tree. No push/fetch/ls-remote/clone anywhere.
const partEdit = (n) =>
  `cd ${simSeed}/worktree && printf 'sim marker p${n}\\n' >> src/f${n}.go && ` +
  `git add -A && git -c user.email=sim@pairv2.invalid -c user.name=sim commit -qm "sim part p${n}"`;

// Marker state per slot: the part chain serves [edit, submit_ready] on the
// FIRST two calls that carry the slot's marker (the committed-tail stray calls
// in between are filtered by the tail rule), exactly like KB-277's counters.
let markerState = { key: null, within: -1 };

function serveStep(marker, slotN) {
  const within = markerState.within;
  if (within === 0) {
    return bashCall(partEdit(slotN));
  }
  return submit({
    outcome: "part_ready",
    payload: { reason: `pair-v2 sim: part p${slotN} committed (edit+ready scripted)` },
  });
}

function replyTo(body) {
  if (body === '{"x":1}') return [plain("{}")];
  let input = [];
  try {
    const parsed = JSON.parse(body);
    input = parsed.input || parsed.conversation || parsed.messages || [];
  } catch {
    return [plain("stub: unparsable request body")];
  }

  // Compaction: plain text, never a tool call (a tool call wedges it).
  for (let i = input.length - 1; i >= 0; i--) {
    const it = input[i];
    const text = typeof it?.content === "string" ? it.content : JSON.stringify(it?.content ?? it);
    if (text.includes("Please create a detailed summary of the conversation so far")) {
      return [plain("pair-v2 sim: compaction summary elided; continue the engineered sequence.")];
    }
  }

  const blob = JSON.stringify(input);

  // KB-277's committed-finalize rule: the LAST item is the tool result of an
  // already-committed submit_outcome — answer plain so the per-step counters
  // stay exact.
  const lastIt = input[input.length - 1];
  if (lastIt && typeof lastIt === "object") {
    if (lastIt.type === "function_call_output" || lastIt.role === "tool") {
      const out =
        typeof lastIt.output === "string"
          ? lastIt.output
          : JSON.stringify(lastIt.content ?? lastIt.output ?? "");
      const callId = lastIt.call_id ?? lastIt.tool_call_id;
      const call = input.find(
        (x) => x && typeof x === "object" && x.type === "function_call" && x.call_id === callId,
      );
      if (out.includes("recorded successfully") && (!call || call.name === "submit_outcome")) {
        return [plain("pair-v2 sim: the finalize was committed; the turn is complete.")];
      }
    }
  }

  // Marker dispatch: LAST marker occurrence in the serialized input names the
  // active call.
  const posRem = blob.lastIndexOf(markerRem);
  const posPart = blob.lastIndexOf(markerPart);
  const posPlan = blob.lastIndexOf(markerPlanner);
  const best = Math.max(posRem, posPart, posPlan);
  if (best < 0) {
    return [plain("pair-v2 sim: no marker recognized; never finalizing this call")];
  }

  const idx = callIdx++;
  let slotN = null;
  let marker = null;
  if (best === posRem) {
    marker = markerRem;
    slotN = Number(blob.slice(posRem).match(/(\d)/)?.[1] ?? "1");
    return [
      submit({
        outcome: "remediated",
        payload: { reason: `pair-v2 sim: remediated p${slotN} (scripted)` },
      }),
    ];
  }
  if (best === posPart) {
    marker = markerPart;
    slotN = Number(blob.slice(posPart).match(/(\d)/)?.[1] ?? "1");
    const key = marker + slotN;
    if (markerState.key !== key) {
      markerState = { key, within: -1 };
    }
    markerState.within++;
    if (markerState.within === 0) {
      process.stderr.write(`stub[part p${slotN}] call ${idx + 1}: edit+commit\n`);
      return [bashCall(partEdit(slotN))];
    }
    process.stderr.write(`stub[part p${slotN}] call ${idx + 1}: submit part_ready\n`);
    return [
      submit({
        outcome: "part_ready",
        payload: {
          reason: `pair-v2 sim: part p${slotN} done (scripted; edit committed in the prior call)`,
        },
      }),
    ];
  }
  // planner
  process.stderr.write(`stub[planner] call ${idx + 1}: submit parts_ready\n`);
  return [submit({ outcome: "parts_ready", payload: { reason: planJson } })];
}

const server = http.createServer((req, res) => {
  let body = "";
  req.on("data", (c) => (body += c));
  req.on("end", () => {
    const output = replyTo(body);
    res.writeHead(200, { "content-type": "application/json" });
    res.end(
      JSON.stringify({
        id: "resp_" + Math.random(),
        object: "response",
        status: "completed",
        model: "stub",
        output,
        output_text: "",
        usage: { input_tokens: 1, output_tokens: 2 },
      }),
    );
  });
});

server.listen(port, "127.0.0.1", () =>
  process.stderr.write(`pair-v2 sim stub up on ${port}\n`),
);
