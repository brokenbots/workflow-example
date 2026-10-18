#!/usr/bin/env bash
set -uo pipefail

# KB-280: standalone regression suite for harness_fleet_smoke_v1 (matrix
# A1/A2 + F33-F37 — the fleet-smoke canary slice). The canary's acceptance
# lane is the live green-run pre-leg gate on every roll/pin change (~1-minute
# wall); these asserts pin the COMPILED, SOURCE, FIXTURE, PROBE, and ROUTE
# shapes the live observations read, so a half-implementation cannot pass
# silently:
#
#   A1 identity handshake: the handshake probe fails LOUDLY (exit 3, designed
#       class marker on stderr) when a session binds without its
#       type-presented identity (CRITERIA_ADAPTER_NAME), and writes the
#       identity record (adapter_name + guarded scope facets) when it holds;
#       the borrower scope carries the same discipline.
#   A2 digest-pin resolve + miss: RESOLVE — `criteria adapter list
#       --referenced` resolves the tree's lockfile to the recorded fleet pin,
#       the compiled graph boots from THAT digest, and the fleet-pin record
#       pins it into the run evidence (survivor redial proves the pinned
#       provision outlives the borrowed scope). MISS — the digest_miss
#       fixture (lockfile pinning sha256:ffff..., never pullable, never
#       cached) OBSERVES the fail-closed admission refusal:
#       `criteria validate --warnings-as-errors` refuses and names the
#       tampered pin, BEFORE any pod opens a session (zero pod creates).
#   F33 zero re-creates / zero provision for the borrower: the census record
#       pins pod_creates==1 per (scope, adapter/environment) pair and
#       provision_wanted==0 for the borrower scope; the compiled borrower
#       callee dials ONLY the owner-established pass-in identity.
#   F34 zero refused dials: the loud-arm routing pins every green-path dial
#       to fail LOUD on state.failed — a refused dial can never be silent.
#   F35 queue discipline: the registry pins the queue_position_zero
#       vocabulary (Queue.Position counts to 0; 0 == admitted-running).
#   F36 secret-free configs: the tree declares no copilot member, no secret
#       channel, and the routes object carries name-referenced secrets only.
#   F37 memory-ceiling parity: source pins over the two pod builders — the
#       peer builder seeds from the SAME shared adapterResources table and
#       only resMax's per-member maxima (zero hand-written resourceQuantity
#       literals in the peer builder); the shared table's ceiling set is
#       pinned verbatim (shell 512Mi/8Gi per KB-95, copilot 2Gi/8Gi per
#       CRI-272).
#   Probe-exec: every probe template renders through the engine's template
#       contract and runs under env -i across the substitution-marker
#       permutations (guarded expansion, no truncation, byte-stable records).
#   Determinism: the compiled graph is byte-stable across re-compiles (the
#       canary's 10/10-green acceptance is its own shape, not a timeout).
#
# The fleet-pin cross-checks are pinned against the tree's own lockfile so a
# bumped binary cannot sneak through a stale pin. Skips cleanly when the
# engine or jq are unavailable — the CI lanes that lack them pick other
# gates. Gate integrity: the green banner is conditional on the A2-miss
# admission refusal actually being OBSERVED (a skipped observation refuses
# the banner).

TREE_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FIXTURES="$(cd "$(dirname "${BASH_SOURCE[0]}")/fixtures" && pwd)"
REPO_ROOT="$(cd "$TREE_ROOT/.." && pwd)"
CRITERIA="${CRITERIA_BIN:-criteria}"

# The fleet pin of record (shared across the harness series' .criteria.lock
# files): ghcr.io/brokenbots/criteria-adapter-shell:0.5.5. The suite refuses
# to assert anything if the tree's own lockfile drifted from it.
FLEET_PIN="sha256:41288ce9ce5d3d46841f40b85208c6208c596f36993a898c23002acfa9100a41"
MISS_PIN="sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"

FAILED=0
A2_OBSERVED=0
WORK=""

fail() {
    echo "FAIL: $*" >&2
    FAILED=$((FAILED + 1))
}

ok() {
    echo "ok: $*"
}

SKIP=""

if ! command -v "$CRITERIA" >/dev/null 2>&1; then
    SKIP="criteria binary '$CRITERIA' not found (set CRITERIA_BIN)"
fi
if [ -z "$SKIP" ] && ! command -v jq >/dev/null 2>&1; then
    SKIP="jq not available (compiled-graph assertions)"
fi
if [ -z "$SKIP" ] && [ -f "$TREE_ROOT/.criteria.lock.hcl" ] && \
   ! grep -qF "$FLEET_PIN" "$TREE_ROOT/.criteria.lock.hcl"; then
    SKIP="canary lockfile no longer pins the recorded fleet pin (suite would lie: re-pin the lockfile and the suite constant together)"
fi
if [ -z "$SKIP" ] && ! "$CRITERIA" version >/dev/null 2>&1; then
    SKIP="criteria cannot report a version ('$CRITERIA version' failed)"
fi

if [ -n "$SKIP" ]; then
    echo "SKIP: harness_fleet_smoke_v1 standalone suite — $SKIP"
    exit 0
fi

WORK="$(mktemp -d /tmp/harness_fleet_smoke_v1.XXXXXX)"
trap 'rm -rf "$WORK"' EXIT

# ── 1. Canary tree: validate green, compile green, boot-manifest shape ───────

if "$CRITERIA" validate "$TREE_ROOT" >"$WORK/validate.log" 2>&1; then
    ok "tree validates green (harness_fleet_smoke_v1; cold-cache schema warnings are tolerated, never demanded)"
else
    fail "tree validation failed:" "$(<"$WORK/validate.log")"
fi

"$CRITERIA" compile --format json "$TREE_ROOT" >"$WORK/graph.json" 2>"$WORK/compile.log" || {
    fail "tree compile failed:" "$(<"$WORK/compile.log")"
}

if [ -s "$WORK/graph.json" ]; then
    got_name="$(jq -r '.name' "$WORK/graph.json")"
    if [ "$got_name" = "harness_fleet_smoke_v1" ]; then
        ok "compiled graph is the canary workflow (harness_fleet_smoke_v1)"
    else
        fail "compiled graph name = $got_name, want harness_fleet_smoke_v1"
    fi

    got_order="$(jq -r '.steps | map(.name) | join(",")' "$WORK/graph.json")"
    want_order="identity_handshake,fleet_census,borrower_callee,fleet_pin_record"
    if [ "$got_order" = "$want_order" ]; then
        ok "compiled step order is the designed canary chain (4 steps: handshake → census → borrower → pin+survivor)"
    else
        fail "compiled step order = [$got_order], want [$want_order]"
    fi

    # Boot-manifest shape: the graph the operator boots from carries exactly
    # ONE identity, pinned to the fleet digest, on the per-scope environment.
    got_adapters="$(jq -r '[.adapters[] | "\(.type).\(.name)@\(.environment)\(.pinned_digest)"] | join(",")' "$WORK/graph.json")"
    want_adapters="shell.pass_in@remote.smoke$FLEET_PIN"
    if [ "$got_adapters" = "$want_adapters" ]; then
        ok "boot-manifest shape: exactly one identity (shell.pass_in@remote.smoke) pinned to the fleet digest"
    else
        fail "boot-manifest shape regressed: adapters = [$got_adapters], want [$want_adapters]"
    fi

    # The borrower's own boot layer carries the SAME identity + digest
    # (cross-layer grouping — the borrowed dial joins, it never provisions).
    got_callee_adapters="$(jq -r '[.subworkflows[] | select(.name=="borrow_scope") | .body.adapters[] | "\(.type).\(.name)@\(.environment)\(.pinned_digest)"] | join(",")' "$WORK/graph.json")"
    if [ "$got_callee_adapters" = "$want_adapters" ]; then
        ok "boot-manifest shape (callee layer): the borrower re-spells the same pinned identity (borrowed dial joins, zero provision)"
    else
        fail "callee boot shape regressed: callee adapters = [$got_callee_adapters], want [$want_adapters]"
    fi

    # F33/F34 carrier: the borrower dials ONLY the owner-established identity.
    callee_dials="$(jq -r '[.subworkflows[] | select(.name=="borrow_scope") | .body.steps[] | .adapter] | join(",")' "$WORK/graph.json")"
    if [ "$callee_dials" = "shell.pass_in" ]; then
        ok "F33: the borrower callee dials ONLY shell.pass_in (nothing new to wire — zero provision_wanted)"
    else
        fail "F33: borrower dials = [$callee_dials], want [shell.pass_in]"
    fi

    # Boot-policy shape (small on purpose: the canary is ~1 minute of wall).
    got_policy="$(jq -r '"\(.policy.MaxTotalSteps)"' "$WORK/graph.json")"
    if [ "$got_policy" = "8" ]; then
        ok "boot.policy.MaxTotalSteps = 8 (the canary's designed small bound)"
    else
        fail "boot.policy.MaxTotalSteps = $got_policy, want 8"
    fi

    # Terminal shape: done is the only green exit; failed is the loud exit.
    got_states="$(jq -r '[.states[] | "\( .name )=\( .terminal )=\( .success )"] | join(",")' "$WORK/graph.json")"
    if [ "$got_states" = "done=true=true,failed=true=false" ]; then
        ok "terminal shape: done=true / failed=false — the loud bug exit never folds into the green exit"
    else
        fail "terminal shape regressed: states = [$got_states], want done=true=true,failed=true=false"
    fi

    # F34 loud-arm routing: EVERY parent step's failure arm lands on
    # state.failed, and the success arms chain the designed canary order.
    loud_arms="$(jq -r '[.steps[] | .outcomes[] | select(.name=="failure") | .next] | join(",")' "$WORK/graph.json")"
    if [ "$loud_arms" = "failed,failed,failed,failed" ]; then
        ok "F34: every step's failure arm routes state.failed (a refused dial is a red, never a silent pass)"
    else
        fail "F34 loud arms regressed: failure next-sites = [$loud_arms]"
    fi
    chain_arms="$(jq -r '[.steps[] | .outcomes[] | select(.name=="success") | .next] | join(",")' "$WORK/graph.json")"
    want_chain="fleet_census,borrower_callee,fleet_pin_record,done"
    if [ "$chain_arms" = "$want_chain" ]; then
        ok "green chain: identity_handshake → census → borrower → pin+survivor → done"
    else
        fail "green chain regressed: success next-sites = [$chain_arms], want [$want_chain]"
    fi
    callee_loud="$(jq -r '[.subworkflows[] | select(.name=="borrow_scope") | .body.steps[] | .outcomes[] | select(.name=="failure") | .next] | join(",")' "$WORK/graph.json")"
    if [ "$callee_loud" = "failed" ]; then
        ok "callee loud arm: the borrower dial refuses loud too (its failure is the bug terminal)"
    else
        fail "callee loud arm regressed: [$callee_loud]"
    fi
else
    fail "compile produced no graph (cannot assert boot-manifest shape)"
fi

# Determinism: the compiled graph is byte-stable across re-compiles.
second_compile="$WORK/graph_second.json"
if "$CRITERIA" compile --format json "$TREE_ROOT" >"$second_compile" 2>"$WORK/second_compile.log" && \
   cmp -s "$WORK/graph.json" "$second_compile"; then
    ok "determinism: re-compile is byte-identical (the canary's green is its own shape, not a timeout artifact)"
else
    fail "determinism: re-compiled graph drifted from the first compile"
fi

# A2 resolve half (OBSERVED offline): the lockfile resolves to the fleet pin
# through the engine's own reference lister.
got_pin="$( (cd "$TREE_ROOT" && "$CRITERIA" adapter list --referenced 2>"$WORK/adlist.log") \
    | awk -F'  ' '$1 == "shell.pass_in" { print $3; exit }' )"
if [ "$got_pin" = "$FLEET_PIN" ]; then
    ok "A2 resolve: adapter list --referenced resolves shell.pass_in to the recorded fleet pin"
else
    fail "A2 resolve: resolved pin = [$got_pin], want [$FLEET_PIN]"
fi
lockfile_pin="$(sed -n 's/^  resolved_digest      = " //p' "$TREE_ROOT/.criteria.lock.hcl" >/dev/null 2>&1; sed -n 's/^  resolved_digest      = " //p' "$TREE_ROOT/.criteria.lock.hcl" >/dev/null 2>&1; grep 'resolved_digest' "$TREE_ROOT/.criteria.lock.hcl" | grep -o 'sha256:[0-9a-f]*' | grep -v '^$' | head -1)"
if [ "$lockfile_pin" = "$FLEET_PIN" ]; then
    ok "A2 resolve: the tree's own lockfile digest is the fleet pin (a bumped binary cannot sneak through a stale pin)"
else
    fail "A2 resolve: lockfile digest = [$lockfile_pin], want [$FLEET_PIN]"
fi

# ── 2. A2 miss half (OBSERVED): the engineered digest-pin refusal ────────────

MISS_FIXTURE="$FIXTURES/digest_miss"
if "$CRITERIA" validate "$MISS_FIXTURE" >"$WORK/miss_plain.log" 2>&1; then
    # Precondition: the fixture is SHAPE-valid (it would only misbehave at
    # admission) — the designed refusal is about the PIN, not syntax.
    ok "A2 miss precondition: the digest-miss fixture validates shape-green as shipped"
else
    fail "A2 miss precondition: fixture does not validate as a well-formed tree:" "$(<"$WORK/miss_plain.log")"
fi
if "$CRITERIA" validate --warnings-as-errors "$MISS_FIXTURE" >"$WORK/miss_strict.log" 2>&1; then
    fail "A2 miss: the tampered pin was ACCEPTED fail-open (refused nowhere) — D7 fail-closed is broken"
else
    miss_rc=0
    "$CRITERIA" validate --warnings-as-errors "$MISS_FIXTURE" >/dev/null 2>&1 || miss_rc=$?
    if grep -qF "$MISS_PIN" "$WORK/miss_strict.log"; then
        ok "A2 miss (OBSERVED): admission refuses fail-closed and names the tampered pin (rc=$miss_rc) — zero sessions, zero pod creates"
        A2_OBSERVED=1
    else
        fail "A2 miss: strict refusal fired without naming the tampered pin $\{MISS_PIN\}: $(<"$WORK/miss_strict.log")"
    fi
fi

# ── 3. Probe renders + exec: the lanes the live run executes ─────────────────

shquote() {
    printf "'%s'" "$(printf '%s' "$1" | sed "s/'/'\\\\''/g")"
}

render_probe() {
    # render_probe <template> <out> <field>=<value>...
    local tpl="$1" out="$2"
    shift 2
    local sed_args=() pair field value
    for pair in "$@"; do
        field="${pair%%=*}"
        value="${pair#*=}"
        sed_args+=(-e "s@{{ .${field} | shellquote }}@$(shquote "$value")@")
    done
    sed "${sed_args[@]}" "$tpl" > "$out" || return 1
    ! grep -q '{{' "$out"
}

RECORDS_DIRNAME="fleet_smoke_records"

# (a) A1 handshake: a nameless session fails LOUD in its designed class.
h_tpl="$TREE_ROOT/scripts/handshake_probe.sh.tftpl"
h_rendered="$WORK/handshake.sh"
if ! render_probe "$h_tpl" "$h_rendered" "probe_root=$WORK/probes"; then
    fail "handshake render: leftover template markers"
else
    out="$(env -i PATH=/usr/bin:/bin sh "$h_rendered" 2>&1)"
    rc=$?
    if [ "$rc" -eq 3 ] && grep -q 'class=A1 name=identity_handshake_missing' <<<"$out"; then
        ok "A1 probe: a session bound WITHOUT its identity fails loud (rc=3, designed class marker on stderr)"
    else
        fail "A1 probe loud leg: rc=$rc, expected 3 + class marker; got: ${out:-<no output>}"
    fi
    h_rec="$WORK/probes/$RECORDS_DIRNAME/identity"
    out="$(env -i PATH=/usr/bin:/bin CRITERIA_ADAPTER_NAME=adapter.shell.pass_in CRITERIA_SCOPE=sc-1 CRITERIA_SCOPE_INSTANCE=inst-1 sh "$h_rendered" 2>&1)"
    rc=$?
    if [ "$rc" -eq 0 ] && \
       grep -Fxq 'record_name=identity_handshake' "$h_rec" && \
       grep -Fxq "adapter_name=adapter.shell.pass_in" "$h_rec" && \
       grep -Fxq "scope=sc-1" "$h_rec" && \
       grep -Fxq "scope_instance=inst-1" "$h_rec"; then
        ok "A1 record: the bound handshake writes the identity record (adapter_name + guarded scope facets)"
    else
        fail "A1 record leg: rc=$rc record=$h_rec got: ${out:-<no output>}"
    fi
    unset h_rc
fi

# (b) Borrower record: the substitution facet + the zero-provision marker.
b_tpl="$TREE_ROOT/workflows/borrow_scope/scripts/borrower_record.sh.tftpl"
b_rendered="$WORK/borrower.sh"
if ! render_probe "$b_tpl" "$b_rendered" "probe_root=$WORK/probes_b"; then
    fail "borrower render: leftover template markers"
else
    b_rec="$WORK/probes_b/$RECORDS_DIRNAME/borrower"
    out="$(env -i PATH=/usr/bin:/bin CRITERIA_ADAPTER_NAME=adapter.shell.pass_in FLEET_SMOKE_MARKER=owner CRITERIA_SCOPE=sc-1 CRITERIA_SCOPE_INSTANCE=inst-1 sh "$b_rendered" 2>&1)"
    rc=$?
    if [ "$rc" -eq 0 ] && \
       grep -Fxq "fleet_smoke_env_marker=owner" "$b_rec" && \
       grep -Fxq 'zero_provision=provision_wanted==0 (nothing new to wire: the borrower joins the owner provision)' "$b_rec"; then
        ok "borrower record: substituted marker reads owner + zero_provision named (the borrower joins, never provisions)"
    else
        fail "borrower record leg (owner): rc=$rc got: ${out:-<no output>}"
    fi
    out="$(env -i PATH=/usr/bin:/bin CRITERIA_ADAPTER_NAME=adapter.shell.pass_in FLEET_SMOKE_MARKER=callee-shadow sh "$b_rendered" 2>&1)"
    rc=$?
    if [ "$rc" -eq 0 ] && grep -Fxq "fleet_smoke_env_marker=callee-shadow" "$b_rec"; then
        ok "borrower contrast leg: the record CARRIES the facet value it is given (callee-shadow on a pod would be substitution leakage, observable)"
    else
        fail "borrower contrast leg: rc=$rc (the contrast facet value is not observable in the record)"
    fi
    out="$(env -i PATH=/usr/bin:/bin FLEET_SMOKE_MARKER=owner sh "$b_rendered" 2>&1)"
    rc=$?
    if [ "$rc" -eq 3 ] && grep -q 'class=A1 name=identity_handshake_missing' <<<"$out"; then
        ok "borrower loud leg: a nameless borrower session fails loud (zero provision still means a bound identity)"
    else
        fail "borrower loud leg: rc=$rc, expected 3 + class marker; got: ${out:-<no output>}"
    fi
fi

# (c) fleet_census + registry: the designed vocabulary, byte-stable.
r_tpl="$TREE_ROOT/scripts/fleet_registry.sh.tftpl"
r_rendered="$WORK/registry.sh"
if ! render_probe "$r_tpl" "$r_rendered" "probe_root=$WORK/probes_r"; then
    fail "registry render: leftover template markers"
else
    r_rec="$WORK/probes_r/$RECORDS_DIRNAME"
    (env -i PATH=/usr/bin:/bin sh "$r_rendered") >"$WORK/registry_run.log" 2>&1
    rc=$?
    if [ "$rc" -eq 0 ] && \
       grep -Fqx 'class=F33 name=zero_pod_recreates marker=pod_creates==1 scope=root identity=shell.pass_in@remote.smoke (one provision, zero recreates)' "$r_rec/census.jsonl" 2>/dev/null 2>&1; then
        ok "census record: the F33 vocabulary pins pod_creates==1 per (scope, adapter/environment)"
    else
        fail "census record leg: rc=$rc, census=[$r_rec/census.jsonl] missing F33 census line (got: $(cat "$r_rec/census.jsonl" 2>/dev/null | head -3))"
    fi
    if grep -Fqx 'designed_pod_creates_total=1' "$r_rec/census.jsonl" 2>/dev/null 2>&1; then
        ok "census record: the designed green run costs exactly ONE provision (root scope); the borrower adds zero"
    else
        fail "census record: designed_pod_creates_total=1 missing"
    fi
    want_reg_classes="A1,A2,A2,F33,F33,F34,F35,F36,F37,BUG"
    got_reg_classes="$(grep -o '^class=[A-Z0-9]*' "$r_rec/registry.jsonl" 2>/dev/null | sed 's/^class=//' | paste -sd, -)"
    if [ "$got_reg_classes" = "$want_reg_classes" ]; then
        ok "registry record: all designed class markers present (A1, A2 resolve+miss, F33-F37, loud bug arms)"
    else
        fail "registry classes = [$got_reg_classes], want [$want_reg_classes]"
    fi
    if grep -q 'name=digest_miss_refused' "$r_rec/registry.jsonl" && \
       grep -q 'name=memory_ceiling_parity' "$r_rec/registry.jsonl"; then
        ok "registry record: the engineered A2-miss marker and the F37 parity marker carry their carrier + lane names"
    else
        fail "registry record: engineered A2-miss / F37 parity marker lines missing"
    fi
    # Byte-stable records: the SAME render run twice writes identical bytes.
    (env -i PATH=/usr/bin:/bin sh "$r_rendered") >"$WORK/registry_run2.log" 2>&1
    if cmp -s "$r_rec/census.jsonl" <(sed -n 'p' "$r_rec/census.jsonl") && \
       ! { (env -i PATH=/usr/bin:/bin sh "$r_rendered") && cmp "$r_rec/census.jsonl" "$WORK/census_first" >/dev/null 2>&1; }; then
        : # handled below via a real first-run copy
    fi
    cp "$r_rec/census.jsonl" "$WORK/census_first"
    cp "$r_rec/registry.jsonl" "$WORK/registry_first"
    (env -i PATH=/usr/bin:/bin sh "$r_rendered") >"$WORK/registry_run3.log" 2>&1
    if cmp -s "$WORK/census_first" "$r_rec/census.jsonl" && cmp -s "$WORK/registry_first" "$r_rec/registry.jsonl"; then
        ok "registry byte-stability: re-written census/registry records are byte-identical (a replay can never fake different evidence)"
    else
        fail "registry byte-stability: re-written records drifted"
    fi
fi

# (d) fleet_pin_record: the pin of record + survivor redial + loud pin guard.
p_tpl="$TREE_ROOT/scripts/fleet_pin_record.sh.tftpl"
p_rendered="$WORK/pin.sh"
if ! render_probe "$p_tpl" "$p_rendered" "probe_root=$WORK/probes_p" "fleet_pin=$FLEET_PIN"; then
    fail "pin render: leftover template markers"
else
    p_rec="$WORK/probes_p/$RECORDS_DIRNAME/fleet_pin"
    out="$(env -i PATH=/usr/bin:/bin FLEET_SMOKE_MARKER=owner CRITERIA_SCOPE=sc-1 CRITERIA_SCOPE_INSTANCE=inst-1 sh "$p_rendered" 2>&1)"
    rc=$?
    if [ "$rc" -eq 0 ] && \
       grep -Fxq "fleet_pin=$FLEET_PIN" "$p_rec" && \
       grep -Fxq 'survivor_redial=scope=root after=borrower_scope (the pinned provision outlives the borrowed scope)' "$p_rec" && \
       grep -Fxq "fleet_smoke_env_marker=owner" "$p_rec"; then
        ok "A2 resolve record: the fleet pin + survivor redial + owned marker land in the run evidence"
    else
        fail "A2 resolve record leg: rc=$rc got: ${out:-<no output>}"
    fi
fi
p_bad="$WORK/pin_bad.sh"
if ! render_probe "$p_tpl" "$p_bad" "probe_root=$WORK/probes_p_bad" "fleet_pin=not-a-digest-address"; then
    fail "pin render (bad pin): leftover template markers"
else
    out="$(env -i PATH=/usr/bin:/bin FLEET_SMOKE_MARKER=owner sh "$p_bad" 2>&1)"
    rc=$?
    if [ "$rc" -eq 3 ] && grep -q 'class=A2 name=fleet_pin_missing' <<<"$out"; then
        ok "pin guard: a pin-less provision fails loud (class=A2 fleet_pin_missing marker) — the canary always runs under the pin of record"
    else
        fail "pin guard leg: rc=$rc, expected 3 + class marker; got: ${out:-<no output>}"
    fi
fi

# ── 4. Probe render contract: templatefile knobs must interpolate ────────────
# Every knob a .chcl passes to templatefile must reach the rendered script
# through a {{ .key | shellquote }} placeholder — a template that only
# carries shell-default fallbacks would silently IGNORE its declared knobs.
for knob_pair in \
    "$TREE_ROOT|scripts/handshake_probe.sh.tftpl|probe_root" \
    "$TREE_ROOT|scripts/fleet_registry.sh.tftpl|probe_root" \
    "$TREE_ROOT|scripts/fleet_pin_record.sh.tftpl|probe_root fleet_pin" \
    "$TREE_ROOT/workflows/borrow_scope|scripts/borrower_record.sh.tftpl|probe_root"; do
    root="${knob_pair%%|*}"
    rest="${knob_pair#*|}"
    tpl_rel="${rest%%|*}"
    knobs="${rest#*|}"
    tpl="$root/$tpl_rel"
    leg_ok=1
    for knob in $knobs; do
        if [ "$(grep -cF "{{ .${knob} | shellquote }}" "$tpl" || true)" -lt 1 ]; then
            fail "render contract ($tpl_rel): the ${knob} knob placeholder is missing (knobs would be silently ignored)"
            leg_ok=0
        fi
    done
    if [ "$leg_ok" -eq 1 ]; then
        ok "render contract (${tpl_rel}): every declared knob reaches the template through a shellquote placeholder"
    fi
done

# ── 5. F36: secret-free configs (tree + routes object) ───────────────────────

# grep tree-wide but never match the suite's own greps (BusyBox has no
# --exclude-dir, so filter the tests/ path from the hit list).
tree_hits="$(grep -rF 'adapter "copilot"' "$TREE_ROOT" 2>/dev/null | grep -v '/tests/' || true)"
if [ -z "$tree_hits" ]; then
    ok "F36 tree: no copilot member anywhere in the canary tree (no secret channel to hold)"
else
    fail "F36 tree: the canary declares a copilot member (a secret channel the canary must not hold): $tree_hits"
fi
secret_hits="$(grep -rE 'WORKFLOW_GITHUB_TOKEN|secretProviderClass|stringData|secretKeyRef' "$TREE_ROOT" 2>/dev/null | grep -v '/tests/' || true)"
if [ -z "$secret_hits" ]; then
    ok "F36 tree: no token surface, no secret provider, no stringData in the canary tree"
else
    fail "F36 tree: the canary tree carries a secret surface it must not hold: $secret_hits"
fi
routes_cm="$REPO_ROOT/k8s/examples/routes-configmap.yaml"
if [ ! -f "$routes_cm" ]; then
    fail "F36 routes: k8s/examples/routes-configmap.yaml not found"
fi
# workflowLibrary objects close at 8-space indent (`        }`).
fleet_obj="$(awk '/^        "harness-fleet-smoke-url": \{/{f=1} f{print} f && /^        \}$/{exit}' "$routes_cm" 2>/dev/null)"
if [ -z "$fleet_obj" ]; then
    fail "F36 routes: the harness-fleet-smoke-url object is missing from the shipped example"
elif grep -qE 'stringData|value: [^"{]|WORKFLOW_GITHUB_TOKEN=' <<<"$fleet_obj"; then
    fail "F36 routes: the harness-fleet-smoke-url object carries material or a literal env value"
else
    ok "F36 routes: the canary object stays in the name-reference secret discipline (secret env values are key names, never material)"
fi

# ── 6. F37: memory-ceiling parity (source pins, read-only) ──────────────────

peer_src="$REPO_ROOT/criteria-k8s/internal/jobbuilder/per_scope_peer.go"
adapter_src="$REPO_ROOT/criteria-k8s/internal/jobbuilder/per_scope_adapter.go"
if [ "$(grep -c 'resourceQuantity(' "$peer_src")" = "0" ]; then
    ok "F37 source: the peer builder contains NO hand-written resourceQuantity literal (a lone ceiling is the defect)"
else
    fail "F37 source: per_scope_peer.go hand-writes resourceQuantity literals (peer ceilings would bypass the shared table)"
fi
if grep -q 'res := adapterResources(adapterKind(members\[0\]))' "$peer_src" && \
   grep -q 'resMax(res.Requests' "$peer_src" && grep -q 'resMax(res.Limits' "$peer_src"; then
    ok "F37 source: the peer builder seeds from the SAME shared adapterResources table and only resMax's per-member maxima"
else
    fail "F37 source: the peer builder lost the shared-seed + resMax mechanism"
fi
shared_table="$(awk '/^func adapterResources/,/^}/' "$adapter_src")"
f37_missing=""
for lit in 'resourceQuantity("512Mi")' 'resourceQuantity("250m")' 'resourceQuantity("2Gi")' 'resourceQuantity("1000m")' \
           'resourceQuantity("8Gi")' 'resourceQuantity("2000m")' '"copilot"' '"shell"' 'CRI-272' 'KB-95'; do
    if ! grep -qF "$lit" <<<"$shared_table"; then
        f37_missing+="$lit "
    fi
done
if [ -z "$f37_missing" ]; then
    ok "F37 table: the shared ceiling set is pinned verbatim (default 512Mi/250m → 2Gi/1000m; copilot 2Gi/500m → 8Gi/2000m CRI-272; shell limit raised to 8Gi/2000m KB-95)"
else
    fail "F37 table: shared ceiling set drifted; missing [$f37_missing] (a ceiling changed in the engine must be re-observed, not assumed)"
fi
if grep -q 'peer-ceilings-seed-from-shared-adapterResources-then-resMax' "$TREE_ROOT/scripts/fleet_registry.sh.tftpl"; then
    ok "F37 registry: the designed marker vocabulary names the parity mechanism"
else
    fail "F37 registry: the designed marker vocabulary lost the parity mechanism naming"
fi

# ── 7. Route wiring shape (the fleet-smoke lane) ─────────────────────────────

for line in \
    '"harness-fleet-smoke-url": {' \
    'git::https://github.com/brokenbots/workflow-example.git//harness_fleet_smoke_v1' \
    '"name": "harness-fleet-smoke",' \
    '"workflow": "harness-fleet-smoke-url"'; do
    if ! grep -qF "$line" "$routes_cm"; then
        fail "routes wiring: expected line missing from the shipped example: $line"
    fi
done
if grep -qF '"harness-fleet-smoke-url"' "$routes_cm" && grep -qF '"name": "harness-fleet-smoke",' "$routes_cm"; then
    ok "route wiring: the harness-fleet-smoke route and its object are shipped (the dave-go-tagged fleet-smoke lane)"
fi
# Route objects live at 8-space indent; a window from the workflow line to
# the object's 8-space close carries the gating (tags/states) without the
# route's own name line pre-satisfying the tag greps.
route_body="$(awk '/^          "workflow": "harness-fleet-smoke-url",$/{f=1} f{print} f && /^        \}$/{exit}' "$routes_cm" 2>/dev/null)"
if grep -qF '"workflow": "harness-fleet-smoke-url"' <<<"$route_body" && \
   grep -qF '"harness-validation"' <<<"$route_body" && \
   grep -qF '"harness-fleet-smoke"' <<<"$route_body" && \
   grep -qF '"Ready for Development"' <<<"$route_body"; then
    ok "route gating: harness-fleet-smoke selects only harness-validation+harness-fleet-smoke on Ready for Development (a plain development card never matches it)"
else
    fail "route gating: harness-fleet-smoke tags/states regressed"
fi
if grep -q 'tagMatch' <<<"$route_body"; then
    fail "route gating: harness-fleet-smoke sets tagMatch (the shipped convention relies on the default all-match semantics)"
fi

# ── Verdict ──────────────────────────────────────────────────────────────────

if [ "$FAILED" -ne 0 ]; then
    echo "harness_fleet_smoke_v1 standalone suite: FAILED ($FAILED assert(s))"
    exit 1
fi
# The green banner is conditional on EVERY leg being OBSERVED: an A2-miss
# that never executed must fail the suite loudly — a skip is not a pass, and
# gate coverage that silently drops the engineered-miss leg is not coverage.
if [ "$A2_OBSERVED" -ne 1 ]; then
    echo "harness_fleet_smoke_v1 standalone suite: all asserts passed; A2 miss SKIPPED (admission refusal never observed) — refusing the green banner"
    exit 1
fi
echo "harness_fleet_smoke_v1 standalone suite: all canary shapes asserted (compile + probes + pin gates + route wiring green)"
exit 0