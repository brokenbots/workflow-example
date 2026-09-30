#!/usr/bin/env bash
set -euo pipefail

# CRI-234/CRI-237 deploy-pairing guard for the criteria build input (fail
# closed).
#
# The per-(scope,environment) co-location reads the environment_type /
# environment_name keys the engine publishes on adapter lifecycle events
# from criteria commit fc95449 (CRI-233) onward. Older runners emit no
# environment identity, so every provision takes the env-less per-adapter
# fallback and the co-location never activates - silently, with all tests
# still green, because only this build input pairs the operator with a
# runner that emits the keys.
#
# CRI-237 wire token delivery additionally consumes the accept_token key
# the engine carries on provision_wanted payload.data from criteria commit
# eae0181 (CRI-236) onward. Older runners deliver the token only through
# engine-rotated files on the shared volume; the operator's wire delivery
# and the container-local CRITERIA_HOME of source-mode runs pair with a
# runner that emits the token on the event.
#
# The criteria-base image builds the rolled runner from CRITERIA_COMMIT,
# so the pin must be exactly the audited commit:
#
#   - a pin predating eae0181 ships a runner that never emits accept_token,
#     leaving the wire delivery inert and the container-local engine state
#     dangling the legacy token_ref paths;
#   - a pin beyond eae0181 ships an emitted contract that has not been
#     audited against the operator's event parser.
#
# Audited to b2d0b66 (v0.5.32, 2026-09-22, operator): proto delta vs eae0181 is
# purely ADDITIVE - new WorkflowGraphs message + oneof arm workflow_graphs = 37
# (CRI-278/298/299: best-effort metadata event; no existing field renumbered or
# removed; adapter event contract untouched). Castle ingest + run-viewer
# consumer verified live. criteria-base builds the engine, not the SDK, so the
# additive events.proto does not affect adapter wire v2.
#
# Re-audited to 77b6e0ed (v0.5.36, 2026-09-30, KB-57 rollout): proto delta vs
# b2d0b66 is purely ADDITIVE - events.proto oneof arms 38-42 (AgentPromptInjected,
# CheckpointPointer, RunPaused/RunResumed, ...) plus new messages in
# criteria.proto/peer.proto/server.proto; zero removed or renumbered fields;
# adapter wire v2 untouched (criteria-base builds the engine, not the SDK).
# Castle ingest is envelope-verbatim (no payload-kind switch) - consumer-safe.
#
# To move off 77b6e0ed, re-audit the emitted adapter event contract, then
# update AUDITED_COMMIT here and the equality checks in
# k8s/tests/test_criteria_base.sh and criteria-base/tests/smoke_test.sh in
# the same change.
#
# Image-mode runs keep the baked workflow image (linear_intake_v1) built
# from a released criteria tarball; no criteria release contains eae0181,
# so image-mode pairing is a coordinator decision and is NOT guarded here.
# It is documented in linear_intake_v1/Dockerfile and the Makefile deploy
# targets.

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
DOCKERFILE="$REPO_ROOT/criteria-base/Dockerfile"
AUDITED_COMMIT="77b6e0ed06dd9c025fd03b691c0b7faffa0f2489"

fail() {
    echo "FAIL: $1" >&2
    exit 1
}

[ -f "$DOCKERFILE" ] || fail "criteria-base/Dockerfile missing"

pin="$(awk -F= '/^ARG CRITERIA_COMMIT=/ {print $2}' "$DOCKERFILE" | tr -d '[:space:]')"
[ -n "$pin" ] || fail "Dockerfile does not pin CRITERIA_COMMIT"

if [ "$pin" != "$AUDITED_COMMIT" ]; then
    fail "criteria build input $pin is not the audited commit $AUDITED_COMMIT: a pin predating eae0181 never emits accept_token, so the CRI-237 wire token delivery stays inert and source-mode runs would dangle the legacy token_ref paths with a container-local CRITERIA_HOME, and a pin beyond eae0181 carries an unaudited event contract; re-audit the emitted contract, then update AUDITED_COMMIT in k8s/tests/test_criteria_base_pin.sh (and the equality checks in k8s/tests/test_criteria_base.sh and criteria-base/tests/smoke_test.sh) in the same change"
fi

echo "PASS: criteria-base pins the audited criteria commit $AUDITED_COMMIT (CRI-234/CRI-237 deploy pairing)"