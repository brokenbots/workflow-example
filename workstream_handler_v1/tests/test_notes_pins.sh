#!/usr/bin/env bash
set -euo pipefail

# Regression test for KB-215 (CRI-321 stack validation): NOTES.md must stay in
# lockstep with the tree's real adapter pins. The release wave that rolled the
# lockfiles to shell 0.5.4 / copilot 0.5.15 left NOTES.md documenting the
# previous pins (0.5.2 / kimi-k2.7 / glm-5.2) and nothing noticed — the doc's
# whole job is to tell the next person what provisions. Nothing here is
# hardcoded to the current wave: the expected values are derived from the
# sources of truth (the lockfile and the adapter/variable defaults), so the
# test keeps guarding the doc through future pin waves:
#
#   1. every adapter in .criteria.lock.hcl has a Pins-table row carrying the
#      same version and resolved digest, and the table has no extra rows the
#      lock does not resolve;
#   2. every model pin in the Model-pins table matches the model default in
#      the variable/adapter block that actually runs the role;
#   3. the "ollama pull" line lists every distinct model the table references,
#      so host-provisioning instructions stay complete.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TREE_ROOT="${SCRIPT_DIR}/.."
FAILED=0

ok() {
    echo "ok: $1"
}

fail() {
    FAILED=$((FAILED + 1))
    echo "FAIL: $1" >&2
}

command -v awk >/dev/null 2>&1 || { echo "FAIL: awk not found" >&2; exit 1; }

NOTES="${TREE_ROOT}/NOTES.md"
LOCK="${TREE_ROOT}/.criteria.lock.hcl"

for f in "$NOTES" "$LOCK"; do
    [ -f "$f" ] || { echo "FAIL: missing $f" >&2; exit 1; }
done

# Strip surrounding whitespace/backticks and print.
trim_cell() {
    awk '{ gsub(/`/, ""); gsub(/^[ \t]+|[ \t]+$/, ""); print }'
}

# ── 1. Pins table vs the lockfile ────────────────────────────────────────────

# Derive "source<TAB>version<TAB>digest", unique by source, from the lock. The
# lock repeats a source once per consumer (shell has three); repeats must agree.
lock_rows="$(awk -F'"' '
    /^adapter /     { inb = 1; src = ""; ver = ""; dig = "" }
    inb && /^  source_url\s*=/      { src = $2 }
    inb && /^  version\s*=/         { ver = $2 }
    inb && /^  resolved_digest\s*=/ { dig = $2 }
    /^\}/ && inb {
        if (seen[src] != "" && seen[src] != ver "@" dig) {
            print "lockfile repeats " src " with inconsistent version/digest" | "cat >&2"
            exit 1
        }
        if (seen[src] == "") { seen[src] = ver "@" dig; print src "\t" ver "\t" dig }
        inb = 0
    }' "$LOCK")"

[ -n "$lock_rows" ] || { echo "FAIL: could not parse any adapter block from $LOCK" >&2; exit 1; }

pins_section="$(awk '/^## Pins/,/^## Model pins/' "$NOTES")"

# Every lock adapter must have a NOTES row with the same version and digest.
for src in $(printf '%s\n' "$lock_rows" | awk -F'\t' '{print $1}'); do
    want_version="$(printf '%s\n' "$lock_rows" | awk -F'\t' -v s="$src" '$1 == s { print $2 }')"
    want_digest="$(printf '%s\n' "$lock_rows" | awk -F'\t' -v s="$src" '$1 == s { print $3 }')"
    name="$(basename "$src")"

    row="$(printf '%s\n' "$pins_section" | grep -F "$name")"
    [ -n "$row" ] || { fail "pins table has no row for $name (lock pins $want_version)"; continue; }

    got_version="$(printf '%s\n' "$row" | awk -F'|' '{ print $4 }' | trim_cell)"
    got_digest="$(printf '%s\n' "$row" | awk -F'|' '{ print $5 }' | trim_cell)"
    [ "$got_version" = "$want_version" ] \
        || fail "pins table $name version '$got_version' != lock version '$want_version'"
    [ "$got_digest" = "$want_digest" ] \
        || fail "pins table $name digest '$got_digest' != lock digest '$want_digest'"
    ok "pins table $name matches lock ($want_version @ ${want_digest#sha256:})"
done

# No stale rows: every NOTES pins row must name a source the lock resolves.
# The lock lists source_url as https://github.com/<org>/<name>; NOTES rows
# reference the ghcr.io/<org>/<name> OCI path — compare by repository name.
lock_sources="$(printf '%s\n' "$lock_rows" | awk -F'\t' '{ print $1 }')"
stale_rows="$(printf '%s\n' "$pins_section" | awk -F'|' '
    /^\| adapter/ { next }
    /^\|--/       { next }
    /^\|/ {
        label = $2; src = $3
        gsub(/^[ \t]+|[ \t]+$/, "", label)
        gsub(/`/, "", src);    gsub(/^[ \t]+|[ \t]+$/, "", src)
        if (label != "" && src != "") print src
    }' | sort -u | while read -r src; do
        base="$(basename "$src")"
        printf '%s\n' "$lock_sources" | grep -F "/$base" >/dev/null 2>&1 || echo "$base"
    done)"

[ -z "$stale_rows" ] \
    || fail "pins table rows not resolved by the lockfile: $stale_rows"
[ -z "$stale_rows" ] && ok "no stale rows in the pins table"

# ── 2. Model-pins table vs the real defaults ────────────────────────────────

# Print the model default inside the variable declaration block of $2 in $1.
# The match requires a quoted value: the object() type block also carries a
# bare `model = string` member that must not be picked up.
model_from_variable() {
    awk -v var="$2" '
        $0 ~ ("^variable \"" var "\" \\{") { inb = 1 }
        inb && $0 ~ /^[ \t]*model[ \t]*=[ \t]*\"/ {
            sub(/^[ \t]*model[ \t]*=[ \t]*\"/, ""); sub(/\".*/, "")
            print
            exit
        }
        inb && $0 ~ /^\}/ { exit }
    ' "$1"
}

# The coordinator's model lives on the copilot adapter block in the root
# adapters.chcl rather than on a variable default.
model_from_coordinator_block() {
    awk '
        /^adapter "copilot" "coordinator" \{/ { inb = 1 }
        inb && $0 ~ /^[ \t]*model[ \t]*=[ \t]*\"/ {
            sub(/^[ \t]*model[ \t]*=[ \t]*\"/, ""); sub(/\".*/, "")
            print
            exit
        }
        inb && $0 ~ /^\}/ { exit }
    ' "$1"
}

models_section="$(awk '/^## Model pins/,/^## Git push credentials/' "$NOTES")"
REFERENCED_MODELS=""

check_role() {
    label="$1" file="$2" block="$3" kind="${4:-}"
    if [ "$kind" = "adapter" ]; then
        want="$(model_from_coordinator_block "$file")"
    else
        want="$(model_from_variable "$file" "$block")"
    fi
    if [ -z "$want" ]; then
        fail "could not derive model for $label from $file ($block)"
        return
    fi

    row="$(printf '%s\n' "$models_section" | grep -F "$label")"
    if [ -z "$row" ]; then
        fail "model-pins table has no row for '$label'"
        return
    fi

    # The model is the first token of the third cell: "`m:cloud` (Ollama ...)".
    got="$(printf '%s\n' "$row" | awk -F'|' '{ print $4 }' | trim_cell | awk '{ print $1 }')"
    if [ "$got" != "$want" ]; then
        fail "model-pins table $label says '$got' but $file defines '$want'"
    else
        case ",${REFERENCED_MODELS}," in
            *",${want},"*) ;;
            *) REFERENCED_MODELS="${REFERENCED_MODELS} ${want}" ;;
        esac
        ok "model-pins table $label matches source ($want)"
    fi
}

check_role "developer (pair loop)" "$TREE_ROOT/workflows/pair_programming_loop/variables.chcl" developer
check_role "reviewer (pair loop)"  "$TREE_ROOT/workflows/pair_programming_loop/variables.chcl" reviewer
check_role "PR reviewer"           "$TREE_ROOT/workflows/pr_reviewer_loop/variables.chcl"      pr_reviewer
check_role "branch repair"         "$TREE_ROOT/workflows/branch_manager/variables.chcl"        repair_agent
check_role "coordinator (root)"    "$TREE_ROOT/adapters.chcl"                                  coordinator adapter

# ── 3. The ollama pull line must cover every referenced model ───────────────

pull_line="$(awk '/pull the cloud models with/ { getline; print }' "$NOTES")"
[ -n "$pull_line" ] \
    || { echo "FAIL: could not find the 'pull the cloud models with' line in NOTES.md" >&2; exit 1; }

for m in $REFERENCED_MODELS; do
    case "$pull_line" in
        *"$m"*) ok "pull line lists $m" ;;
        *) fail "pull line does not list $m: '$pull_line'" ;;
    esac
done

if [ "$FAILED" -ne 0 ]; then
    echo "test_notes_pins: $FAILED failure(s)" >&2
    exit 1
fi
echo "test_notes_pins: all checks passed"