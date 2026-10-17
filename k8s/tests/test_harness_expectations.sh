#!/usr/bin/env bash
# Regression test for the KB-254 harness-expectations matrix of record (the
# harness series anchor, cards 276-280).
#
# docs/harness-expectations.md is the validation surface the series reads as
# its spec: 37 base items (A1-F37) + 12 composition items (G1-G12), the series
# map (card -> tree -> slice), the D20/D23 standing-develop-leg carriers, the
# two pinned KNOWN GAPS (G11/G12 with their engine carriers), and the
# conventions of record the legs land against. The guard
# (scripts/harness_matrix.mjs, wired into `make validate`) fails closed on
# drift; this test keeps the guard itself honest:
#   1. repo-wide pass: the committed matrix is complete and consistent;
#   2. negative: removing an item row must fail closed with the item named;
#   3. negative: losing a series-map slice assignment must fail closed with
#      the card named;
#   4. negative: unpinning a KNOWN GAP must fail closed.
#   5. negative: an out-of-band item row id must be flagged (the id space is
#      bounded) and the observed row count must be checked.
set -euo pipefail

SCRIPT_DIR="$(cd "${BASH_SOURCE[0]%/*}" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
GUARD="${REPO_ROOT}/scripts/harness_matrix.mjs"
DOC_REL="docs/harness-expectations.md"

fail() { echo "FAIL: $1" >&2; exit 1; }

command -v node >/dev/null 2>&1 || fail "node is required to run the harness-matrix guard"
[ -f "${GUARD}" ] || fail "guard missing: ${GUARD}"
[ -f "${REPO_ROOT}/${DOC_REL}" ] || fail "matrix of record missing: ${DOC_REL}"

# 1. Repo-wide pass: the committed matrix satisfies the guard.
if ! out="$(node "${GUARD}" --root "${REPO_ROOT}" 2>&1)"; then
    echo "${out}" >&2
    fail "committed matrix of record should pass the guard"
fi
echo "${out}" | grep -q "49/49 items (37 base + 12 G)" ||
    fail "guard pass output must name the 49-item count: got: ${out}"
echo "PASS: committed harness-expectations matrix is complete and consistent (49 items, 5 cards, 2 gaps)"

# Negative checks run the guard against a doctored COPY of the doc in a temp
# root, never the committed file.
#
# Two tamper mechanisms:
#   - tampered_fail_sed: literal sed edits (no anchors, no \| alternation
#     traps — GNU and BusyBox both treat \| as alternation, which globally
#     corrupts the doc and makes the fixture vacuous).
#   - tampered_fail_line: surgical single-line edits done in node (node is
#     already required by the guard), dialect-free by construction. The
#     row-shape tampers use bold markers (**E30**, **A6**) that appear ONLY
#     on their item rows — series-map slices use single-backtick cells.
tampered_fail_sed() { # <desc> <sed-expr> <expected output needle>
    local desc="$1" sedexpr="$2" needle="$3"
    local tmp
    tmp="$(mktemp -d)"
    mkdir -p "${tmp}/docs"
    cp "${REPO_ROOT}/${DOC_REL}" "${tmp}/docs/harness-expectations.md"
    sed -i "${sedexpr}" "${tmp}/docs/harness-expectations.md"
    guard_must_fail "${desc}" "${tmp}" "${needle}"
    echo "PASS: ${desc} -> guard failed closed with the expected finding"
}

tampered_fail_line() { # <desc> <op> <marker> <replacement> <expected needle>
    local desc="$1" op="$2" marker="$3" repl="$4" needle="$5"
    local tmp
    tmp="$(mktemp -d)"
    mkdir -p "${tmp}/docs"
    cp "${REPO_ROOT}/${DOC_REL}" "${tmp}/docs/harness-expectations.md"
    node -e '
        const fs = require("fs");
        const [, file, op, marker, repl] = process.argv;
        const lines = fs.readFileSync(file, "utf8").split("\n");
        const at = lines.findIndex((l) => l.includes(marker));
        if (at === -1) process.exit(3);
        if (op === "delete-line") lines.splice(at, 1);
        else if (op === "duplicate-line") lines.splice(at, 0, lines[at]);
        else if (op === "replace-first-marker") lines[at] = lines[at].replace(marker, repl);
        else process.exit(3);
        fs.writeFileSync(file, lines.join("\n"));
    ' "${tmp}/docs/harness-expectations.md" "${op}" "${marker}" "${repl}" ||
        fail "${desc}: tamper marker '${marker}' not found (needle: ${needle})"
    guard_must_fail "${desc}" "${tmp}" "${needle}"
    echo "PASS: ${desc} -> guard failed closed with the expected finding"
}

guard_must_fail() { # <desc> <tmp-root> <expected output needle>
    local desc="$1" tmp="$2" needle="$3"
    local out
    if out="$(node "${GUARD}" --root "${tmp}" --quiet 2>&1)"; then
        rm -rf "${tmp}"
        fail "${desc}: guard accepted a doctored matrix (must fail closed); got: ${out}"
    fi
    echo "${out}" | grep -qF "${needle}" ||
        { rm -rf "${tmp}"; fail "${desc}: guard failed but without the expected finding '${needle}'; got: ${out}"; }
    rm -rf "${tmp}"
}

# 2. Missing item row (single-row deletion).
tampered_fail_line "missing item row" "delete-line" "**E30** " "" "missing item row for **E30**"

# 3. Lost slice assignment.
tampered_fail_sed "lost slice assignment" "s/\`E31\`, //" 'harness/failures) does not assign'

# 4. Unpinning a KNOWN GAP (G11/G12 must stay pinned with their engine carrier).
tampered_fail_sed "unpinned KNOWN GAP marker" 's/KNOWN GAP/KNOWN LOSS/g' 'must be pinned as a KNOWN GAP'
tampered_fail_sed "lost engine-carrier filing rule" 's/files back to the engine carrier/files back elsewhere/g' 'must state its pinned engine carrier'

# 5. Out-of-band item id + row-count receipt: renaming A6 to H1 must be
#    flagged as unknown (the id space is bounded); duplicating A6 must be
#    flagged as a duplicate AND trip the exact row-count check (50 vs 49).
tampered_fail_line "out-of-band item id" "replace-first-marker" "**A6**" "**H1**" "unexpected item id **H1**"
tampered_fail_line "observed row count" "duplicate-line" "**A6** " "" "observed item row count is 50"
