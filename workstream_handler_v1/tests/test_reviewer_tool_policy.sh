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

# glob2re translates a filepath.Match-style pattern (only '*' wildcards are
# used in the review allowlist) into a POSIX ERE, preserving the engine rule
# that '*' does not cross '/'. Escapes '.' (the only regex metacharacter the
# allowlist patterns contain); other metacharacters are not expected here.
glob2re() {
    printf '%s' "$1" | awk '
        function glob2re(g) {
            gsub(/\./, "\\.", g)
            gsub(/\*/, "[^/]*", g)
            return "^" g "$"
        }
        { print glob2re($0) }
    '
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
kb57c_split_compound() {
    python3 - "$1" <<'PYEOF'
import sys
text = sys.argv[1]
out, buf, i, q = [], [], 0, None
esc = False
while i < len(text):
    c = text[i]
    if esc is True:
        buf.append(c); esc = False; i += 1; continue
    if c == "\\" and q is None:
        buf.append(c); esc = True; i += 1; continue
    if q is None and c in "'\"":
        q = c; buf.append(c); i += 1; continue
    if q is not None and c == q:
        q = None; buf.append(c); i += 1; continue
    if q is None:
        two = text[i:i+2]
        if two in ("&&", "||"):
            out.append("".join(buf).strip()); buf = []; i += 2; continue
        if c in ";|":
            out.append("".join(buf).strip()); buf = []; i += 1; continue
    buf.append(c); i += 1
out.append("".join(buf).strip())
print("\n".join(s for s in out if s))
PYEOF
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

echo "==> All checks passed."