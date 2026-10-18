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
// prompts; no comment parameters except the C17 probes (the empty comment on
// the require_comment arm, and the corrected-comment defect variant).
const MODES = {
  // C14: a schema-valid payload is accepted and routes the declared outcome.
  f1_valid: () => [
    submit({
      outcome: "approved",
      payload: {
        reason: "C14 valid payload: schema-valid payload routes the declared approved outcome with its projected outputs intact",
      },
    }),
  ],

  // C15/C16/D19: wrong-field-set and wrong-lane rejections (nothing
  // committed), then the schema-valid engineered rejection routes the
  // declared green lane.
  f2_reject: () => [
    submit({ outcome: "rejected", payload: { verdict: "wrong field set on purpose" } }),
    submit({
      outcome: "rejected",
      reason: "wrong lane on purpose: reason string outside the typed payload lane",
    }),
    submit({
      outcome: "rejected",
      payload: {
        reason: "D19 engineered rejection routed green: wrong-field-set and wrong-lane shapes were rejected loudly by the payload contract, then the declared rejected lane carried this verbatim reason",
      },
    }),
  ],

  // C17: empty adapter comment rejected loudly on the require_comment arm,
  // then the probe observation routes the green exit (the `observed` arm).
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
      payload: {
        reason: "C17 observed: the empty adapter comment on the require_comment arm was rejected loudly by the payload contract (payload_invalid, comment_missing)",
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
    }),
  ],

  // D21: the trailing-* allow band lets the shell tool call execute while
  // the read tool call is denied with a permission event; the recovered
  // denial keeps the turn's success.
  f6_permissions: () => [
    tool("bash", { command: "printf d21-shell-band-ok", description: "D21 band-matched shell call" }),
    tool("view", { path: "main.chcl" }),
    submit({
      outcome: "approved",
      reason: "D21 recovered denial keeps success: the trailing-* band allowed the shell tool call, the read tool call was denied with a permission event, and the step still finalized approved after recovering",
    }),
  ],

  // C18a/D22 + C18b: never-finalize stimuli — the turn budget exhausts and
  // the missing-finalize class (or, with no lanes mapped, the loud routing
  // error) is the designed shape.
  f7_budget: () => [plain("KB-277 C18a/D22 probe: this step never finalizes on purpose (turn budget must exhaust)")],
  f8_unmapped: () => [plain("KB-277 C18b probe: this step never finalizes on purpose (the synthetic failure outcome must hit the loud routing error)")],
};

function replyTo(body) {
  if (body === '{"x":1}') return {};
  let input = [];
  try {
    const parsed = JSON.parse(body);
    input = parsed.input || parsed.conversation || parsed.messages || [];
  } catch {
    return plain("stub: unparsable request body");
  }
  for (let i = input.length - 1; i >= 0; i--) {
    const it = input[i];
    const text = typeof it?.content === "string" ? it.content : JSON.stringify(it?.content ?? it);
    if (text.includes("Please create a detailed summary of the conversation so far")) {
      return plain(
        "<overview> KB-277 contracts harness fixture probe session running one fixed probe; submit_outcome is the only finalize path. " +
          "Earlier turns: the current fixture step asked for exactly the engineered submit_outcome/tool calls prescribed by its prompt; no other work is in flight.</overview> " +
          "Next: continue the current fixture step's engineered call sequence and finalize exactly once.",
      );
    }
  }
  const idx = calls.push(1) - 1;
  const seq = (MODES[mode] || (() => [submit({ outcome: "approved", payload: { reason: "fixture stub default: mode unknown" } })]))();
  process.stderr.write(`stub[${mode}] call ${idx + 1}\n`);
  const out = seq[Math.min(idx, seq.length - 1)];
  return Array.isArray(out) ? out : [out];
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