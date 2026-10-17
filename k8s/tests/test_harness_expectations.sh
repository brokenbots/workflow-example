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
tampered_fail() { # <desc> <sed-expr> <expected output needle>
    local desc="$1" sedexpr="$2" needle="$3"
    local tmp out
    tmp="$(mktemp -d)"
    mkdir -p "${tmp}/docs"
    cp "${REPO_ROOT}/${DOC_REL}" "${tmp}/docs/harness-expectations.md"
    sed -i "${sedexpr}" "${tmp}/docs/harness-expectations.md"
    if ! out="$(node "${GUARD}" --root "${tmp}" --quiet 2>&1)"; then
        echo "${out}" | grep -qF "${needle}" ||
            fail "${desc}: guard failed but without the expected finding '${needle}'; got: ${out}"
    else
        fail "${desc}: guard accepted a doctored matrix (must fail closed); got: ${out}"
    fi
    rm -rf "${tmp}"
    echo "PASS: ${desc} -> guard failed closed with the expected finding"
}

# 2. Missing item row.
tampered_fail "missing item row" '/^\| \*\*E30\*\* /d' 'missing item row for **E30**'

# 3. Lost slice assignment.
tampered_fail "lost slice assignment" 's/`E31`, //' 'harness/failures) does not assign'

# 4. Unpinning a KNOWN GAP (G11/G12 must stay pinned with their engine carrier).
tampered_fail "unpinned KNOWN GAP marker" 's/KNOWN GAP/KNOWN LOSS/g' 'must be pinned as a KNOWN GAP'
tampered_fail "lost engine-carrier filing rule" 's/files back to the engine carrier/files back elsewhere/g' 'must state its pinned engine carrier'

# 5. Out-of-band item id + row-count receipt: renaming A6 to H1 must be
#    flagged as unknown (the id space is bounded); duplicating A6 must be
#    flagged as a duplicate AND trip the exact row-count check (50 vs 49).
tampered_fail "out-of-band item id" 's/^\| \*\*A6\*\* /| **H1** /' 'unexpected item id **H1**'
tampered_fail "observed row count" 's/^\(| \*\*A6\*\* .*\)/\1\n\1/' 'observed item row count is 50'
