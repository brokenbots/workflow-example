// stub_systemone.mjs — deterministic System One decision-backend stub for the
// decision_demo_* runtime tests. Speaks the System One wire format over POST
// /v1/systemone (ADR-0013): request {model, state, questions}, response
// {model, answers, usage}.
//
// Answers are derived from the ticket state's subject so every scenario is
// driven purely by the test's --var inputs (no test-specific backend knobs):
//
//   subject contains "lowconf"      -> department confidence 0.20 (below route_conf_floor)
//   subject contains "urgent"       -> urgency noul answer "yes"
//   subject contains "critical"     -> severity score 2 ("high")
//   subject contains "billing"      -> department choice "billing"
//   subject contains "security"     -> department choice "security"
//   otherwise                        -> choice "technical", confidence 0.97,
//                                       urgency "no", score 0 ("low")
//
// The choice answer's confidence is 0.97 unless "lowconf" is present, so one
// backend covers both the auto-route and the confidence-gate paths.

import http from "node:http";

const port = Number(process.argv[2] ?? 11434);
const host = process.argv[3] ?? "127.0.0.1";

const server = http.createServer((req, res) => {
  let raw = "";
  req.on("data", (chunk) => (raw += chunk));
  req.on("end", () => {
    if (req.method !== "POST" || new URL(req.url, `http://${req.headers.host}`).pathname !== "/v1/systemone") {
      res.writeHead(404, { "content-type": "application/json" });
      res.end(JSON.stringify({ error: "stub only speaks POST /v1/systemone" }));
      return;
    }
    let request;
    try {
      request = JSON.parse(raw);
    } catch {
      res.writeHead(400, { "content-type": "application/json" });
      res.end(JSON.stringify({ error: "stub could not parse request body" }));
      return;
    }
    // state is a JSON string, object, or array — the stub only needs the
    // ticket subject, so tolerate both the string and object shapes.
    let state = request.state;
    if (typeof state === "string") state = JSON.parse(state);
    const subject = String(state?.subject ?? "");

    const choice = subject.includes("billing") ? "billing"
      : subject.includes("security") ? "security"
      : "technical";
    const confidence = subject.includes("lowconf") ? 0.2 : 0.97;
    const noul = subject.includes("urgent") ? "yes" : "no";
    const score = subject.includes("critical") ? 2 : 0;

    // Answers are positionally aligned with the request questions. The wire
    // contract (ADR-0013) strictly decodes every entry: choice answers
    // carry {id, choice, probabilities, [confidence]}, score answers carry
    // {id, score, legend, probabilities, [confidence]} with legend equal to
    // the chosen level, and noul answers carry {id, noul} with no
    // confidence. Choice answers for an undeclared option and probability
    // keys outside the declared criteria are rejected by the adapter.
    const answers = (request.questions ?? []).map((q) => {
      switch (q.type) {
        case "noul": return { id: q.id, noul };
        case "choice": {
          const options = Object.keys(q.criteria ?? {});
          const probs = {};
          let rest = 1 - confidence;
          for (const option of options) {
            probs[option.toLowerCase()] = option.toLowerCase() === choice ? confidence : rest / (options.length - 1);
          }
          return { id: q.id, choice, probabilities: probs, confidence };
        }
        case "score": {
          const levels = q.criteria ?? ["low"];
          return {
            id: q.id,
            score,
            legend: levels[score],
            probabilities: { [levels[score]]: 0.97, [levels[0]]: 0.03 },
            confidence: 0.97,
          };
        }
        default: return { id: q.id };
      }
    });

    res.writeHead(200, { "content-type": "application/json" });
    res.end(JSON.stringify({ model: `stub-${subject ? "echo" : "bare"}`, answers, usage: { tokens: 0 } }));
  });
});

server.listen(port, host, () => {
  console.log(`stub systemone listening on http://${host}:${port}/v1/systemone`);
});