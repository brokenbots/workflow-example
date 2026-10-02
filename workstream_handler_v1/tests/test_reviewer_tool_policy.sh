#!/usr/bin/env bash
# Regression test for CRI-260: the pair-programming review step enforces the
# reviewer scope correction deterministically via allow_tools.
#
# The review step's allow_tools is deny-by-default, so every test/CI command
# (make ci, make test, go test, npm test, gh pr checks, gh run watch, and
# compounds like "make build && make ci") fails to match and is refused by
# policy, while the one allowed execution (build once + run the built binary)
# and all read-class commands still match. Also guards the developer step's
# unchanged ["*"] policy (developer responsibilities are not restricted) and
# the pr_reviewer_loop charter scoping (pr_review keeps ["*"] and is corrected
# at charter level because it needs gh read commands).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PAIR_DIR="${SCRIPT_DIR}/../workflows/pair_programming_loop"
PR_DIR="${SCRIPT_DIR}/../workflows/pr_reviewer_loop"
HANDLER_DIR="${SCRIPT_DIR}/.."
COMPILE_OUT="$(mktemp)"
ALLOWLIST="$(mktemp)"
trap 'rm -f "${COMPILE_OUT}" "${ALLOWLIST}"' EXIT

# glob2re translates a policy pattern into a POSIX ERE under the SHIPPED
# (#490) matcher semantics: a TRAILING bare '*' matches by slash-PERMISSIVE
# prefix (filepath.Match's '*' cannot cross '/' — that pre-#490 rule broke
# every slash-bearing segment under per-segment coverage), while interior
# '*' remain character-level non-slash wildcards against the remainder.
glob2re() {
    case "$1" in
        *\*)
            # trailing star: prefix match — escape literals, anchor head only
            printf '%s' "${1%\*}" | awk '
                { gsub(/\./, "\\.", $0); gsub(/\*/, "[^/]*", $0); print "^" $0 }
            '
            ;;
        *)
            printf '%s' "$1" | awk '
                { gsub(/\./, "\\.", $0); gsub(/\*/, "[^/]*", $0); print "^" $0 "$" }
            '
            ;;
    esac
}

# seg_matches TARGET: exits 0 when the whole-text target matches any compiled
# pattern (legacy whole-text semantics for a single segment).
seg_matches() {
    local target="$1" re pat
    while IFS= read -r pat; do
        re="$(glob2re "${pat}")"
        if printf '%s' "${target}" | grep -qE "${re}"; then
            return 0
        fi
    done <"${ALLOWLIST}"
    return 1
}

# kb57c_split_compound CMD: prints the segments of CMD split on unquoted
# && || ; | (newline), quote-aware — mirrors criteria segmentCompoundCommand.
# awk port of the original python3 implementation: CI runners ship python3,
# but minimal review/dev pods (Alpine, unprivileged) do not, and the helper
# otherwise silently degrades to whole-text matching, which makes compound
# negatives false-positive as permitted. Algorithm is identical to the
# python source (backslash escape outside quotes only, quote chars carried
# into the segment, empty segments dropped); commands are treated as one
# line, matching every caller in this suite.
kb57c_split_compound() {
    local cmd="$1"
    printf '%s\n' "${cmd}" | awk '
function flush() {
    gsub(/^[ \t]+|[ \t]+$/, "", buf)
    if (buf != "") print buf
    buf = ""
}
{
    n = length($0)
    q = ""
    esc = 0
    buf = ""
    i = 1
    while (i <= n) {
        c = substr($0, i, 1)
        if (esc)                  { buf = buf c; esc = 0; i++; continue }
        if (q == "" && c == "\\") { buf = buf c; esc = 1; i++; continue }
        if (q == "" && (c == "\"" || c == "\047")) { q = c; buf = buf c; i++; continue }
        if (q != "" && c == q)    { q = ""; buf = buf c; i++; continue }
        if (q == "") {
            pair = substr($0, i, 2)
            if (pair == "&&" || pair == "||") { flush(); i += 2; continue }
            if (c == ";" || c == "|")         { flush(); i++; continue }
        }
        buf = buf c; i++
    }
    flush()
}'
}

# allow_matches CMD: KB-57c semantics — a compound is permitted only when EVERY
# segment matches some allowlist pattern; a single command is permitted when
# any pattern matches whole text.
allow_matches() {
    local cmd="$1" segs n
    segs="$(kb57c_split_compound "${cmd}")"
    n="$(printf '%s' "${segs}" | grep -c . || true)"
    if [ "${n}" -le 1 ]; then
        seg_matches "shell:${cmd}"
        return $?
    fi
    # compound: every segment must match
    local line
    while IFS= read -r line; do
        [ -z "${line}" ] && continue
        if ! seg_matches "shell:${line}"; then
            return 1
        fi
    done <<<"${segs}"
    return 0
}

echo "==> Compiling pair_programming_loop..."
cd "${HANDLER_DIR}"
criteria compile "${PAIR_DIR}" >"${COMPILE_OUT}" 2>/dev/null

echo "==> Extracting review step allowlist..."
jq -r '.steps[] | select(.name == "review") | .allow_tools[]' "${COMPILE_OUT}" >"${ALLOWLIST}"
if [ ! -s "${ALLOWLIST}" ]; then
    echo "FAIL: review step has an empty allow_tools (deny-all); the reviewer could not work" >&2
    exit 1
fi

echo "==> Checking no universal pattern is present (would allow everything)..."
for universal in '*' 'shell' 'shell:*' 'shell*'; do
    if grep -qxF "${universal}" "${ALLOWLIST}"; then
        echo "FAIL: review allow_tools contains universal pattern '${universal}' (CRI-260)" >&2
        exit 1
    fi
done

echo "==> Checking bash-kind mirror of every read-only shell: pattern (copilot names its command tool bash)..."
while IFS= read -r pat; do
    case "$pat" in
        shell:*) ;;
        *) continue ;;
    esac
    mirror="bash:${pat#shell:}"
    if ! grep -qxF "${mirror}" "${ALLOWLIST}"; then
        echo "FAIL: review allow_tools lacks the bash-kind mirror of '${pat}' (CRI-260/KB-57)" >&2
        exit 1
    fi
done <"${ALLOWLIST}"

echo "==> Checking file-read tool kinds are allowed..."
grep -qE '^(read|read_file)$' "${ALLOWLIST}"

echo "==> Checking forbidden test/CI commands are denied by every pattern..."
for cmd in \
    "make ci" \
    "make test" \
    "make test ./pkg" \
    "make ci && make test" \
    "make build && make ci" \
    "make -C /repo ci" \
    "make ci 2>&1 | tee ci.log" \
    "go test ./..." \
    "go test ./internal/adapterhost" \
    "npm test" \
    "npm run test" \
    "gh pr checks 12" \
    "gh run watch 1234" \
    "gh run view 1 --log" \
    "bash -c 'make test'" \
    "sh -c 'make ci'" \
    "git -C /repo log --oneline -5" \
    "cd /repo && git log --oneline -5"; do
    if allow_matches "${cmd}"; then
        echo "FAIL: review allow_tools permits '${cmd}' (CRI-260 requires deterministic denial)" >&2
        exit 1
    fi
done

echo "==> Checking KB-61 deny shapes now match (run 747cf4ac review death)..."
for cmd in \
    "git diff origin/main...HEAD -- proto/criteria/v2/adapter.proto" \
    "git diff origin/main...HEAD -- criteria/v2/proto_test.go" \
    "git log --oneline -15" \
    "git ls-remote --tags origin" \
    "git ls-remote origin main CRI-201" \
    "git remote -v" \
    "git rev-parse HEAD origin/CRI-201" \
    "git show d7abb74 --stat" \
    "echo \"---STATUS---\""; do
    if ! allow_matches "${cmd}"; then
        echo "FAIL: review allow_tools denies KB-61 charter read '${cmd}' (scope gap: 5 denies burned the reviewer's turns -> missing finalize)" >&2
        exit 1
    fi
done

echo "==> Checking kb57-wave evidence-loop reads (CRI-205/206 review death run)..."
for cmd in \
    "find . -name '*.md' -not -path './.git/*' | head -50" \
    "find . -name '*.md' -not -path './.git/*'" \
    "find . -maxdepth 8 -type d" \
    "go env GOMODCACHE" \
    "ls /root/go/pkg/mod/github.com/brokenbots/"; do
    if ! allow_matches "${cmd}"; then
        echo "FAIL: review allow_tools denies evidence-loop read '${cmd}' (denies burned the reviewer's turn -> missing finalize)" >&2
        exit 1
    fi
done
# 'go' is allowed ONLY as build/env: go test / go vet stay denied
for cmd in "go test ./..." "go vet ./..."; do
    if allow_matches "${cmd}"; then
        echo "FAIL: review allow_tools permits non-build go verb '${cmd}'" >&2
        exit 1
    fi
done

# KB-74 (castle run 663ddd47 review-leg detour): the reviewer runs in a pod
# with no reachable network surface, and the charter now says so. Durable
# confirmation, fix direction 2: the deny-first allowlist must keep refusing
# url/web_fetch kinds and fetch verbs — a later grant would make the charter
# lie, so this also guards the boundary text and the gate from drifting
# apart. Kinds live as bare entries in the compiled allowlist.
for kind in 'url' 'web_fetch'; do
    if grep -qxF "${kind}" "${ALLOWLIST}"; then
        echo "FAIL: review allow_tools grants URL tool kind '${kind}' (KB-74 in-pod boundary: no network fetches)" >&2
        exit 1
    fi
done
if grep -qE 'curl|wget' "${ALLOWLIST}"; then
    echo "FAIL: review allow_tools contains a network-fetch verb entry (KB-74 in-pod boundary: review from the repo only)" >&2
    exit 1
fi
for cmd in \
    "curl -fsSL https://raw.githubusercontent.com/brokenbots/criteria/main/README.md" \
    "wget -qO- https://raw.githubusercontent.com/brokenbots/criteria/main/README.md" \
    "git ls-remote origin main && curl -s https://raw.githubusercontent.com/brokenbots/criteria/main/README.md"; do
    if allow_matches "${cmd}"; then
        echo "FAIL: review allow_tools permits network fetch '${cmd}' (KB-74: the last review death spent its budget on a denied URL fetch)" >&2
        exit 1
    fi
done

echo "==> Checking write/test/CI verbs stay denied inside compounds (engine evaluates whole text: the deny comes from the FIRST-segment pattern not matching)..."
for cmd in \
    "make test && git log --oneline" \
    "go test ./internal/adapterhost && git status" \
    "gh pr checks 22 && git diff --stat"; do
    if allow_matches "${cmd}"; then
        echo "FAIL: review allow_tools permits CI-compound '${cmd}'" >&2
        exit 1
    fi
done

echo "==> Checking git write verbs stay denied (any depth)..."
for cmd in \
    "git add -A" \
    "git commit -m wip" \
    "git push origin HEAD" \
    "git checkout -b test" \
    "git reset --hard origin/main" \
    "git diff origin/main...HEAD -- proto/x.proto && git push origin HEAD"; do
    if allow_matches "${cmd}"; then
        echo "FAIL: review allow_tools permits git write '${cmd}'" >&2
        exit 1
    fi
done

echo "==> Checking reviewer-prompt evidence reads are covered as single commands (KB-61)..."
for cmd in \
    "ls -la /data/intake/KB-61/" \
    "ls /data/intake/KB-61/workstreams" \
    "ls /data/intake/KB-61/workstreams/KB-61.md"; do
    if ! allow_matches "${cmd}"; then
        echo "FAIL: review allow_tools denies single read '${cmd}'" >&2
        exit 1
    fi
done

# KNOWN ENGINE BEHAVIOR (kb-57c, documented): the runtime matcher evaluates the WHOLE
# command text; a slash-free compound whose first segment matches an allow pattern
# ("git status && make ci") is NOT segmented, so its later segments are never checked.
# The reviewer charter forbids chaining and the run event stream makes violations
# visible; engine-side segmentation is the durable fix (ticketed separately).

echo "==> Checking allowed charter commands still match..."
for cmd in \
    "git status" \
    "git status --porcelain=v1" \
    "git --no-pager log --oneline -12" \
    "git --no-pager log origin/main..HEAD" \
    "git --no-pager diff origin/main...HEAD" \
    "git --no-pager status" \
    "git --no-pager show abc123" \
    "git diff" \
    "git diff origin/main...HEAD" \
    "git diff HEAD~1 HEAD -- src/main.go" \
    "git log --oneline -5" \
    "git log origin/main..HEAD" \
    "git show abc123" \
    "git show abc123:src/main.go" \
    "git rev-parse HEAD" \
    "git branch --show-current" \
    "make build" \
    "go build ./..." \
    "go build -o bin/app ./cmd/tool" \
    "./bin/tool --fixture tests/fixtures/in.json" \
    "./build/app --help" \
    "cat README.md" \
    "cat workstreams/KB-26/spec.md" \
    "grep -rn handler src/" \
    "rg pattern ./pkg" \
    "ls" \
    "ls workstreams" \
    "tail -n 50 run.log" \
    "jq .summary build/report.json"; do
    if ! allow_matches "${cmd}"; then
        echo "FAIL: review allow_tools denies charter command '${cmd}'" >&2
        exit 1
    fi
done

echo "==> Checking the develop step keeps its unrestricted policy..."
jq -e '.steps[] | select(.name == "develop") | .allow_tools == ["*"]' "${COMPILE_OUT}" >/dev/null

echo "==> Compiling pr_reviewer_loop (charter-level scoping)..."
PR_OUT="$(mktemp)"
trap 'rm -f "${COMPILE_OUT}" "${ALLOWLIST}" "${PR_OUT}"' EXIT
criteria compile "${PR_DIR}" >"${PR_OUT}" 2>/dev/null

echo "==> Checking pr_review keeps gh access (corrected at charter level, not by tool policy)..."
jq -e '.steps[] | select(.name == "pr_review") | .allow_tools == ["*"]' "${PR_OUT}" >/dev/null

echo "==> Checking pr_reviewer.md states the judgment charter without execution instructions..."
grep -q "judgment layer" "${HANDLER_DIR}/workflows/pr_reviewer_loop/agents/pr_reviewer.md"
grep -q "only execution authority" "${HANDLER_DIR}/workflows/pr_reviewer_loop/agents/pr_reviewer.md"
grep -q "Never run \`make test\`" "${HANDLER_DIR}/workflows/pr_reviewer_loop/agents/pr_reviewer.md"
for legacy in \
    "Run \`make test\`" \
    "verify for yourself with \`gh pr checks" \
    "Verify for yourself with \`gh pr checks"; do
    if grep -qF "${legacy}" "${HANDLER_DIR}/workflows/pr_reviewer_loop/agents/pr_reviewer.md"; then
        echo "FAIL: pr_reviewer.md still instructs the reviewer to run tests/CI: '${legacy}' (CRI-260)" >&2
        exit 1
    fi
done

echo "==> Checking pair-loop reviewer.md charter replaced the pre-authorized test runs..."
grep -q "judgment layer" "${HANDLER_DIR}/workflows/pair_programming_loop/agents/reviewer.md"
grep -q "only execution authority" "${HANDLER_DIR}/workflows/pair_programming_loop/agents/reviewer.md"
grep -q "built binary" "${HANDLER_DIR}/workflows/pair_programming_loop/agents/reviewer.md"
if grep -q "pre-authorized" "${HANDLER_DIR}/workflows/pair_programming_loop/agents/reviewer.md"; then
    echo "FAIL: reviewer.md still lists running tests/make targets as pre-authorized (CRI-260)" >&2
    exit 1
fi

echo "==> Checking KB-72: pair-loop reviewer adapter declares a bounded max_turns..."
# Compile-level: the reviewer adapter must wire the max_turns knob at all
# (compiled output carries adapter config key NAMES, not values).
jq -e '.adapters[] | select(.name == "reviewer") | (.config_keys // []) | index("max_turns") != null' "${COMPILE_OUT}" >/dev/null

# Value-level, from the variables source: pin the KB-71 bounded range,
# reject the pre-KB-72 unbounded 30 default, and keep the stated budget in
# the agents/reviewer.md turn-discipline rule in sync with the declared
# number (the charter states the same number the adapter enforces).
REVIEWER_BUDGET="$(awk '
    /^variable "reviewer"/ { in_reviewer = 1 }
    in_reviewer && /^}$/   { in_reviewer = 0; in_default = 0 }
    in_reviewer && /^    default = \{/ { in_default = 1 }
    in_reviewer && in_default && /^[[:space:]]*max_turns[[:space:]]*=/ {
        sub(/^[[:space:]]*max_turns[[:space:]]*=[[:space:]]*/, "");
        sub(/[^0-9].*$/, "");
        print;
        exit;
    }
' "${PAIR_DIR}/variables.chcl")"
if [ -z "${REVIEWER_BUDGET}" ]; then
    echo "FAIL: pair_programming_loop variables.chcl reviewer default carries no max_turns value (KB-72 turn budget)" >&2
    exit 1
fi
if [ "${REVIEWER_BUDGET}" -lt 12 ] || [ "${REVIEWER_BUDGET}" -gt 15 ]; then
    echo "FAIL: reviewer max_turns = ${REVIEWER_BUDGET}, outside the KB-71 bounded 12-15 review budget" >&2
    exit 1
fi
echo "   reviewer turn budget: ${REVIEWER_BUDGET} turns (bounded)"

echo "==> Checking KB-72: reviewer charter carries the turn-discipline rule with the stated budget..."
REVIEWER_MD="${HANDLER_DIR}/workflows/pair_programming_loop/agents/reviewer.md"
grep -q "Turn discipline: deliver the verdict" "${REVIEWER_MD}"
grep -qF "max_turns = ${REVIEWER_BUDGET}" "${REVIEWER_MD}"
grep -qF "even when the verdict is \`need_help\`" "${REVIEWER_MD}"
grep -qF 'Call `submit_outcome` with `approved`, `changes_requested`, or `need_help`' "${REVIEWER_MD}"
if grep -qF 'with approve,' "${REVIEWER_MD}"; then echo "FAIL: reviewer.md names an invalid outcome token 'approve' (pair-loop step declares 'approved')" >&2; exit 1; fi
grep -qF "A review that ends without a verdict is a failed review, not a longer review." "${REVIEWER_MD}"

echo "==> Checking KB-72: ported review charters carry the turn-discipline rule..."
for ported_md in \
    "${HANDLER_DIR}/workflows/pr_reviewer_loop/agents/pr_reviewer.md" \
    "${SCRIPT_DIR}/../../linear_triage_v1/agents/triage_reviewer.md" \
    "${SCRIPT_DIR}/../../linear_intake_v1/agents/triage_reviewer.md" \
    "${SCRIPT_DIR}/../../kanboard_triage_v1/agents/triage_reviewer.md"; do
    grep -q "Turn discipline: deliver the verdict" "${ported_md}" || {
        echo "FAIL: ${ported_md} lacks the KB-72 turn-discipline rule" >&2
        exit 1
    }
    grep -qF "A review that ends without a verdict is a failed review, not a longer review." "${ported_md}" || {
        echo "FAIL: ${ported_md} lacks the no-verdict-is-failed-review rule (KB-72)" >&2
        exit 1
    }
done
# pr_reviewer keeps a bounded budget too: its charter must not claim an
# unbounded turn window or instruct ignoring the cap.
if grep -q "unbounded" "${HANDLER_DIR}/workflows/pr_reviewer_loop/agents/pr_reviewer.md"; then
    echo "FAIL: pr_reviewer.md claims an unbounded review budget (KB-72)" >&2
    exit 1
fi

echo "==> Checking KB-74: reviewer charter carries the in-pod review boundary..."
# KB-74 (castle run 663ddd47): the review leg spent its budget on
# environment discovery — a git -C compound, find / for the absent SDK
# module cache, then a denied URL fetch — because the charter never named
# the boundary. The boundary section must name each unreachable surface,
# the in-repo alternative (the lock file), and the exact compound refusal,
# WITHOUT weakening the #125 deny-storm hard rule (asserted below).
grep -qF "## The in-pod review boundary: review the repo, not the environment" "${REVIEWER_MD}"
grep -qF "You are reviewing in a pod, and the environment is deliberately bounded" "${REVIEWER_MD}"
grep -qF "no \`url\`/\`web_fetch\` tool kind in your allowlist" "${REVIEWER_MD}"
grep -qF 'fetching a spec or doc from raw.githubusercontent.com' "${REVIEWER_MD}"
grep -qF "**No SDK/proto module cache.**" "${REVIEWER_MD}"
grep -qF "\`.criteria.lock.hcl\` is the artifact of record" "${REVIEWER_MD}"
grep -qF "Review from the repo diff and in-repo tests only." "${REVIEWER_MD}"
grep -qF "name it as the gap in your \`need_help\` reason" "${REVIEWER_MD}"
grep -qF "no matching allow_tools entry for every segment of the compound command" "${REVIEWER_MD}"
grep -qF "Run cd-relative plain git" "${REVIEWER_MD}"
grep -qF "never \`git -C <abs-path>" "${REVIEWER_MD}"
# No regression of the #125 deny-storm rule, and no instruction that turns
# a denied surface back into something the reviewer should try.
grep -qF "Do not chain commands" "${REVIEWER_MD}"
grep -qF "This is a hard rule, not a style preference." "${REVIEWER_MD}"
for bad in "run curl" "use curl" "curl -fsSL" "wget -qO-" "use web_fetch"; do
    if grep -qF "${bad}" "${REVIEWER_MD}"; then
        echo "FAIL: reviewer.md appears to instruct a network fetch: '${bad}' (KB-74 in-pod boundary: no network fetches)" >&2
        exit 1
    fi
done

echo "==> All checks passed."