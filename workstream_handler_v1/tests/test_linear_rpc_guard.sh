#!/usr/bin/env bash
# Regression test for KB-231: set_linear_review_state must fail
# deterministically on a dead Linear API — an empty rc-0 body or an HTML
# outage page each surface the named transport fatal with the failing stage,
# never a deferred jq "invalid JSON text" error.
#
# Mocks the Linear GraphQL endpoint with a node server that answers every
# request with the configured outage body.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="${SCRIPT_DIR}/../scripts/set_linear_review_state.sh.tftpl"

ok() { echo "ok: $1"; }
fail() { echo "FAIL: $1" >&2; FAILURES=$((FAILURES + 1)); }
FAILURES=0
require_equal() {
    if [ "$1" = "$2" ]; then ok "$3"; else fail "$3: got '$1', want '$2'"; fi
}

WORK_DIR="$(mktemp -d)"
trap 'kill "$MOCK_PID" 2>/dev/null; rm -rf "${WORK_DIR}"' EXIT

# The mock answers every request with the configured outage body.
MOCK="${WORK_DIR}/mock_linear.js"
cat > "$MOCK" <<'JS'
const http = require('http');
const fs = require('fs');
const cfgFile = process.argv[2];
const server = http.createServer((req, res) => {
    let body = '';
    req.on('data', (chunk) => { body += chunk; });
    req.on('end', () => {
        const cfg = JSON.parse(fs.readFileSync(cfgFile, 'utf8'));
        if (cfg.body_mode === 'empty') {
            res.writeHead(200, { 'Content-Length': 0 });
            res.end();
            return;
        }
        const html = '<html><body>302 Found. Object moved.</body></html>';
        res.writeHead(200, { 'Content-Type': 'text/html', 'Content-Length': Buffer.byteLength(html) });
        res.end(html);
    });
});
server.listen(0, '127.0.0.1', () => { console.log(server.address().port); });
JS

MOCK_CFG="${WORK_DIR}/mock_config.json"
MOCK_LOG="${WORK_DIR}/mock.log"
printf '{"body_mode":"empty"}' > "$MOCK_CFG"
node "$MOCK" "$MOCK_CFG" >"$MOCK_LOG" 2>&1 &
MOCK_PID=$!
PORT=""
tries=0
until [ -n "$PORT" ] || [ "$tries" -ge 50 ]; do
    PORT="$(head -n1 "$MOCK_LOG" 2>/dev/null || true)"
    tries=$((tries + 1))
    [ -n "$PORT" ] || sleep 0.1
done
if [ -z "$PORT" ]; then
    echo "FAIL: mock Linear server did not start: $(cat "$MOCK_LOG")" >&2
    exit 1
fi

# Criteria renders the shellquoted criteria_value_N header lines; here the
# values are substituted directly. The Linear endpoint is pointed at the
# harness mock.
sed -e "s,{{ .criteria_value_1 | shellquote }},'CRI-32',g" \
    -e "s,{{ .criteria_value_2 | shellquote }},'In Review',g" \
    -e "s,https://api.linear.app/graphql,http://127.0.0.1:$PORT/graphql,g" \
    "$SCRIPT" > "$WORK_DIR/case.sh"
chmod +x "$WORK_DIR/case.sh"

for mode in empty html302; do
    jq -n --arg mode "$mode" '{body_mode: $mode}' > "$MOCK_CFG"
    err="$(LINEAR_API_KEY=harness-key bash "$WORK_DIR/case.sh" 2>&1)" && rc=0 || rc=$?
    if [ "$mode" = empty ]; then want="linear rpc transport: issue fetch (empty response)"; else want="linear rpc transport: issue fetch (non-JSON-RPC body)"; fi
    if [ "$rc" -ne 0 ] \
        && printf '%s' "$err" | grep -qF "$want" \
        && ! printf '%s' "$err" | grep -qF "invalid JSON text"; then
        ok "an $mode body fails set_linear_review_state with the issue fetch named fatal"
    else
        fail "an $mode body must fail with '$want' (rc=$rc out=$err)"
    fi
done

require_equal "$FAILURES" 0 "KB-231 handler linear guard verified"