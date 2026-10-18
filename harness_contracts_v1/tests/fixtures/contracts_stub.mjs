// Scripted model-provider stub for the harness_contracts_v1 standalone
// regression fixtures (KB-277). One stub process serves ONE fixture run: the
// test script starts it on a free port, pins the shimmed adapter config to
// it (provider_wire_api = "responses"), and asserts the run events against
// the shapes the fixture's mode engineers. The stub never reads the repo —
// the sequences below are the fixed prompts' engineered submission chains,
// transcribed verbatim so the CLI's conversation sees exactly what the
// step prompts prescribe.
//
// Wire notes (proven against copilot adapter 0.5.15 over the local shim):
//   - a request body of {"x":1} is the adapter's health probe; answer {}.
//   - conversation items arrive in `input`; a CLI auto-compaction request is
//     a user item containing "Please create a detailed summary of the
//     conversation so far" and MUST be answered with plain output text — a
//     tool call wedges the compaction and strands the step prompt.
//   - a scripted reply is a function_call item (submit_outcome or a real
//     tool). The adapter echoes tool results back into `input`, so fixture
//     sequence position is tracked per-process by call counters.
//
// full_chain mode (the shipped main.chcl run) dispatches BY STEP MARKER
// instead of a global counter: rejection retries and permission denials add
// extra requests per step, so concatenated indices drift. Each step prompt
// opens with a unique "KB-277 Cnn probe" marker; the stub takes the LAST
// marker present in the serialized input (conversation history carries the
// earlier steps' prompts) and counts requests per marker, resetting on step
// change.
//
// Committed-tail rule (proven against the shipped chain): once a step's
// submit_outcome call is COMMITTED ("recorded successfully" tool result),
// the CLI's agentic loop calls the model one more time BEFORE the next
// step's prompt is appended — the turn boundary is locked. Answering that
// stray call with a submission would land it on the NEXT step's contracts
// while its marker is still absent from the input, so any request whose
// tail is a committed submit_outcome result is answered with plain text
// (never another submission) and never advances the per-step counters.
import http from "node:http";

const mode = process.env.STUB_MODE || "f1_valid";
const calls = []; // invocation index per fixture (compaction excluded)

const submit = (args) => ({
  id: "fc" + calls.length,
  type: "function_call",
  call_id: "c" + calls.length,
  name: "submit_outcome",
  arguments: JSON.stringify(args),
});
const tool = (name, args) => ({
  id: "ft" + calls.length,
  type: "function_call",
  call_id: "t" + calls.length,
  name,
  arguments: JSON.stringify(args),
});
const plain = (text) => ({
  id: "fm" + calls.length,
  type: "message",
  role: "assistant",
  content: [{ type: "output_text", text }],
});

// The engineered submission chains. Call order matches the fixed step
// prompts, transcribed verbatim — including the prescribed comment
// parameter on the probes whose prompts carry one (C14, C13, D21, the C17
// empty-comment probe, and the C17 corrected-comment defect variant).
//
// NOTE ON THE DUAL-LANE SUBMITS (payload + identical top-level reason):
// the engine projects ONLY the top-level reason field into the arm's output
// (output.reason); payload fields are not projected (payload is not an
// output key), so a payload-only submission leaves output.reason "" and the
// echo_verdict records would render empty. The record-carrying submissions
// therefore carry BOTH lanes with identical text. This does not weaken the
// lane separation the fixtures prove: the wrong-lane probe (f2 item 2) is
// reason-ONLY on a contracted step and is rejected loudly, and the C13
// contrast (f5) proves the reason lane accepted exactly on the step that
// declares no schema.
const MODES = {
  // C14: a schema-valid payload is accepted and routes the declared outcome.
  // The submission carries BOTH lanes with DISTINCT content: the engine
  // projects ONLY the top-level reason into the arm outputs (payload body
  // fields are dropped — the pinned projection gap, filed back to the engine
  // carrier), so the c14_reason record must carry the top-level lane content
  // and must NOT carry the payload lane content — the lanes are discriminated,
  // not echoed. Both texts avoid the lowercase character the engine's
  // projection pipeline redacts (see the pinned defect on this row).
  f1_valid: () => [
    submit({
      outcome: "approved",
      reason:
        "C14 projected outputs: the top-level reason lane is the one lane the engine projects — these characters flow verbatim through output.reason into the c14_reason run-state record",
      payload: {
        reason:
          "C14 payload lane: schema-valid payload body — the pinned engine projection gap drops payload body fields, they never surface in the arm outputs",
      },
      comment: "C14 valid payload accepted",
    }),
  ],

  // C15/C16/D19: wrong-field-set and wrong-lane rejections (nothing
  // committed), then the schema-valid engineered rejection routes the
  // declared green lane. The final submission carries both lanes (payload
  // valid and identical top-level reason) so the engine projects the D19
  // reason into the verdict record sink.
  f2_reject: () => [
    submit({ outcome: "rejected", payload: { verdict: "wrong field set on purpose" } }),
    submit({
      outcome: "rejected",
      reason: "wrong lane on purpose: reason string outside the typed payload lane",
    }),
    submit({
      outcome: "rejected",
      reason:
        "D19 engineered rejection routed green: wrong-field-set and wrong-lane shapes were rejected loudly by the payload contract, then the declared rejected lane carried this verbatim reason",
      payload: {
        reason:
          "D19 engineered rejection routed green: wrong-field-set and wrong-lane shapes were rejected loudly by the payload contract, then the declared rejected lane carried this verbatim reason",
      },
    }),
  ],

  // C17: empty adapter comment rejected loudly on the require_comment arm,
  // then the probe observation routes the green exit (the `observed` arm).
  // The observed submission carries both lanes so the c17_comment record
  // renders the observation text.
  f3_comment: () => [
    submit({
      outcome: "approved",
      payload: {
        reason: "C17 comment contract payload (this submission carries an empty adapter comment and must be rejected)",
      },
      comment: "",
    }),
    submit({
      outcome: "observed",
      reason:
        "C17 observed: the empty adapter comment on the require_comment arm was rejected loudly by the payload contract (payload_invalid, comment_missing)",
      payload: {
        reason:
          "C17 observed: the empty adapter comment on the require_comment arm was rejected loudly by the payload contract (payload_invalid, comment_missing)",
      },
    }),
  ],

  // C17 defect evidence: the corrected non-empty comment resubmission on the
  // require_comment arm. The adapter-side check accepts it but the adapter
  // drops the comment before the FinalizeOutcome RPC, so the engine-side
  // require_comment check sees an empty comment and the step fails LOUD with
  // missing_comment (asserted by the fixture as the documented 0.5.15 defect
  // shape; KB-249 retires require_comment in v0.6.0).
  f3b_comment_defect: () => [
    submit({
      outcome: "approved",
      payload: {
        reason: "C17 comment contract payload (this submission carries an empty adapter comment and must be rejected)",
      },
      comment: "",
    }),
    submit({
      outcome: "approved",
      payload: {
        reason: "C17 corrected-comment defect probe: this non-empty adapter comment is accepted by the adapter-side check but dropped before the engine-side require_comment validation",
      },
      comment: "C17 corrected non-empty adapter comment (defect probe)",
    }),
  ],

  // C13 carrier fired for real: the undeclared arm (declared in the tree
  // with no schema, so no outcome_contracts entry) fails LOUD.
  f4_undeclared: () => [
    submit({
      outcome: "rejected",
      reason: "C13 undeclared-arm probe: submitting the arm with no schema on a contracted step",
    }),
  ],

  // C13 contrast: the same top-level reason shape accepted on the step that
  // declares no schema at all.
  f5_uncontracted: () => [
    submit({
      outcome: "approved",
      reason: "C13 uncontracted lane: the reason-form submission is accepted on a step with no declared schema — the same top-level reason shape is rejected loudly on all contracted steps",
      comment: "C13 uncontracted reason lane",
    }),
  ],

  // D21: the trailing-* band grants the shell tool call first-match-wins
  // (the first matching band wins per the permission event's pattern echo)
  // while the read tool call is denied with a permission event naming the
  // tool; the recovered denial keeps the turn's success. The bash command
  // writes the same side-effect record the fixed prompt engineers — RELATIVE
  // path, because the CLI bash tool's working directory is the probe root
  // the shim points the shell environment at.

  f6c_compound: () => [
    tool("bash", {
      command: "printf d21c-first-segment-granted > ./d21c_seg1_record && ls /d21c_no_such_dir",
      description: "D21 compound segmentation probe: second segment matches no band",
    }),
    tool("bash", {
      command: "printf d21c-segment-a > ./d21c_seg2_record && printf d21c-segment-b > ./d21c_seg3_record",
      description: "D21 compound all-matching probe",
    }),
    submit({
      outcome: "approved",
      payload: {
        reason:
          "D21 compound segmentation: the compound command was denied when one segment matched no allow band (per-segment matching, one allowed segment never rescues a denied one), the all-matching compound was granted by the kind-prefixed trailing band, and the step still finalized approved after recovering",
      },
      comment: "D21 compound segmentation probe",
    }),
  ],
  f6d_kindprefix: () => [
    tool("bash", {
      command: "printf d21d-kind-prefixed-band > ./d21d_record",
      description: "D21 kind-prefixed band probe",
    }),
    tool("view", { path: "main.chcl" }),
    submit({
      outcome: "approved",
      payload: {
        reason:
          "D21 kind-prefixed bands: the grant echoes which band won (kind-prefixed argument form versus adapter namespace form), the unmatched call was denied, and the step finalized approved after recovering",
      },
      comment: "D21 kind-prefix probe",
    }),
  ],
  f6_permissions: () => [
    tool("bash", {
      command: "printf d21-shell-band-ok > ./d21_shell_band_record",
      description: "D21 band-matched shell call",
    }),
    tool("view", { path: "main.chcl" }),
    submit({
      outcome: "approved",
      reason:
        "D21 recovered denial keeps success: the trailing-* band granted the shell tool call first-match-wins (pattern s* echoed in the permission.granted event), the read tool call was denied with a permission.denied event naming the tool, and the step still finalized approved after recovering",
      payload: {
        reason:
          "D21 recovered denial keeps success: the trailing-* band granted the shell tool call first-match-wins (pattern s* echoed in the permission.granted event), the read tool call was denied with a permission.denied event naming the tool, and the step still finalized approved after recovering",
      },
      comment: "D21 permission band probe",
    }),
  ],

  // C18a/D22 + C18b: never-finalize stimuli — the turn budget exhausts and
  // the missing-finalize class (or, with no lanes mapped, the loud routing
  // error) is the designed shape.
  f7_budget: () => [plain("KB-277 C18a/D22 probe: this step never finalizes on purpose (turn budget must exhaust)")],
  // C18 default mapping: never-finalize on a step with an explicit
  // `outcome "default"` arm — the synthetic unmapped failure name routes the
  // default lane green (the mapped-by-default contrast to f8_unmapped).
  f8b_default: () => [plain("KB-277 C18 default-lane probe: this step never finalizes on purpose (the synthetic failure outcome must route the default arm)")],
  f8_unmapped: () => [plain("KB-277 C18b probe: this step never finalizes on purpose (the synthetic failure outcome must hit the loud routing error)")],
};

// Step markers -> per-step dispatch mode for full_chain (the shipped-tree
// run). Every step prompt in the shipped agents/*.md.tftpl templates opens
// with one of these marker phrases; conversation history keeps the earlier
// steps' prompts around, so the LAST marker occurrence in the serialized
// input names the active step.
const MARKER_STEPS = [
  ["KB-277 C14 probe", "f1_valid"],
  ["KB-277 C15/C16/D19 probe", "f2_reject"],
  ["KB-277 C17 probe", "f3_comment"],
  ["KB-277 C13 probe", "f5_uncontracted"],

  ["KB-277 D21 compound-segmentation probe", "f6c_compound"],
  ["KB-277 D21 kind/adapter namespace probe", "f6d_kindprefix"],
  ["KB-277 D21 probe", "f6_permissions"],
  ["KB-277 C18a/D22 probe", "f7_budget"],
  ["KB-277 C18 default-lane probe", "f8b_default"],
  ["KB-277 C18b probe", "f8_unmapped"],
];
const PLAIN_NEVER_FINALIZE = () =>
  plain("KB-277 never-finalize probe: this step never finalizes on purpose (turn budget must exhaust)");

// True when the LAST conversation item is the tool result of a submit_outcome
// call that was already committed ("recorded successfully") — the stray
// post-commit model call the CLI makes before the next step's prompt is
// appended. Anything else (a fresh step prompt, rejection feedback, a real
// tool result) is normal dispatch flow.
function committedFinalizeTail(input) {
  const last = input[input.length - 1];
  if (!last || typeof last !== "object") return false;
  if (last.type !== "function_call_output" && last.role !== "tool") return false;
  const callId = last.call_id ?? last.tool_call_id;
  const out = typeof last.output === "string" ? last.output : JSON.stringify(last.content ?? last.output ?? "");
  if (!out.includes("recorded successfully")) return false;
  const call = input.find(
    (it) => it && typeof it === "object" && it.type === "function_call" && it.call_id === callId,
  );
  return !call || call.name === "submit_outcome";
}

// full_chain dispatch state: the active step marker and the number of
// scriptable requests it has seen; resets on marker change.
let chainStep = null;
let chainCount = 0;

function serve(modeName, idx) {
  const gen = MODES[modeName];
  if (!gen) {
    process.stderr.write(`stub[${mode}] mode unknown: ${modeName}\n`);
    return [plain("stub: fixture mode unknown")];
  }
  const seq = gen();
  const out = seq[Math.min(idx, seq.length - 1)];
  return Array.isArray(out) ? out : [out];
}

function replyTo(body) {
  if (body === '{"x":1}') return {};
  let input = [];
  try {
    const parsed = JSON.parse(body);
    input = parsed.input || parsed.conversation || parsed.messages || [];
  } catch {
    return [plain("stub: unparsable request body")];
  }
  for (let i = input.length - 1; i >= 0; i--) {
    const it = input[i];
    const text = typeof it?.content === "string" ? it.content : JSON.stringify(it?.content ?? it);
    if (text.includes("Please create a detailed summary of the conversation so far")) {
      return [
        plain(
          "<overview> KB-277 contracts harness fixture probe session running one fixed probe; submit_outcome is the only finalize path. " +
            "Earlier turns: the current fixture step asked for exactly the engineered submit_outcome/tool calls prescribed by its prompt; no other work is in flight.</overview> " +
            "Next: continue the current fixture step's engineered call sequence and finalize exactly once.",
        ),
      ];
    }
  }
  const idx = calls.push(1) - 1;
  if (committedFinalizeTail(input)) {
    process.stderr.write(`stub[${mode}] call ${idx + 1}: committed submit_outcome tail — plain end-of-turn\n`);
    return [
      plain("KB-277 stub: the finalize call was already committed; the turn is complete — no further submissions."),
    ];
  }
  if (mode !== "full_chain") {
    process.stderr.write(`stub[${mode}] call ${idx + 1}\n`);
    return serve(mode, idx);
  }
  // full_chain: marker-based per-step dispatch.
  const blob = JSON.stringify(input);
  let best = -1;
  let step = null;
  for (const [marker, stepMode] of MARKER_STEPS) {
    const pos = blob.lastIndexOf(marker);
    if (pos > best) {
      best = pos;
      step = stepMode;
    }
  }
  if (!step) {
    process.stderr.write(`stub[full_chain] call ${idx + 1}: no step marker in input — plain never-finalize\n`);
    return [PLAIN_NEVER_FINALIZE()];
  }
  if (step !== chainStep) {
    chainStep = step;
    chainCount = 0;
  }
  const within = chainCount++;
  process.stderr.write(`stub[full_chain] step ${step} call ${within + 1} (global ${idx + 1})\n`);
  return serve(step, within);
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

const port = parseInt(process.env.STUB_PORT || "8971", 10);
server.listen(port, "127.0.0.1", () => process.stderr.write(`contracts fixture stub up on ${port} (${mode})\n`));