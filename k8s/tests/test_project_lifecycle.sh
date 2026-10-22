#!/usr/bin/env bash
# Per-project namespace cutover regression test (KB-312, ADR 307).
#
# Pins the go-domain acceptance leg (the card that ships the cutover
# TEMPLATE): the rendered per-project namespace target state
# (k8s/projects/criteria-go-dev.yaml) must be byte-stable, structurally
# sound, and consistent with the KB-311 surfaces it builds on
# (binding schema of record, projectRegistry cutover marker, per-ns PVC
# shape). The workflow-level legs assert the registry/route behavior the
# cutover leaves behind: the route keeps routing to the project, and the
# GLOBAL workflow library carries NO go-domain binding data (the post-
# cutover platform-namespace shrink), plus the checksum the operator stamps
# at admission matches the binding payload the namespace serves.
#
# Live-cluster legs (apply/verify/triage/retention) need kubectl and a
# cluster; they are exercised by the operator, not here. This test runs
# offline; the only soft dependency is python3+pyyaml for the YAML parse
# leg (skipped with a loud notice when absent, like the other python-needing
# k8s tests).
set -euo pipefail

cd "$(dirname "$0")/../.."
lifecycle="k8s/project-lifecycle.sh"
examples="k8s/examples"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

fail() { echo "FAIL: ${*}" >&2; exit 1; }
pass() { echo "ok: ${*}"; }

[ -x "$lifecycle" ] || fail "$lifecycle missing or not executable"

# 1) Deterministic render: same inputs -> byte-identical output, and the
#    committed target state is exactly what the renderer produces (drift gate).
"$lifecycle" render >"$tmp/once.yaml" 2>"$tmp/render.stderr"
"$lifecycle" render >"$tmp/twice.yaml" 2>/dev/null
cmp "$tmp/once.yaml" "$tmp/twice.yaml" || fail "render is not deterministic"
cmp "$tmp/once.yaml" k8s/projects/criteria-go-dev.yaml \
  || fail "committed k8s/projects/criteria-go-dev.yaml drifted from the renderer; re-run: $lifecycle render --namespace criteria-go-dev --output k8s/projects/criteria-go-dev.yaml"
pass "deterministic render matches committed target state"

# 2) Every document in the rendered target state must parse as YAML
#    (python3+pyyaml; skipped loudly when unavailable on the runner).
if command -v python3 >/dev/null 2>&1 && python3 -c 'import yaml' >/dev/null 2>&1; then
  awk '/^---$/{n++; next} {print > ("'"$tmp"'/doc." (n + 1))}' "$tmp/once.yaml"
  for f in "$tmp"/doc.*; do
    python3 - "$f" <<'P'
import sys, yaml
docs = list(yaml.safe_load_all(open(sys.argv[1])))
assert docs, sys.argv[1]
assert all(isinstance(d, dict) for d in docs), sys.argv[1]
P
  done
  n_docs="$(find "$tmp" -name 'doc.*' | wc -l)"
  echo "ok: ${n_docs} documents parse as YAML (python3)"
else
  echo "SKIP yaml parse leg: python3+pyyaml unavailable on this runner (CI covers it)"
fi

# 3) Namespace-level shape: project labels, namespaced-only RBAC, refusals.
excerpt_doc() { # kind -> ALL docs of that kind, from each doc's first kind line onward
  awk -v kind="$2" '
    BEGIN { kind_line = "kind: " kind }
    /^---$/ { next }
    /^kind: / && $0 == kind_line { inb = 1 }
    /^kind: / && inb && $0 != kind_line { inb = 0 }
    inb { print }
  ' "$1"
}
grep -q '^kind: Namespace$' "$tmp/once.yaml" || fail "no Namespace document rendered"
ns_doc="$(excerpt_doc "$tmp/once.yaml" Namespace)"
grep -q 'criteria.brokenbots.dev/project: criteria-go-dev' <<<"$ns_doc" \
  || fail "namespace label criteria.brokenbots.dev/project missing"
grep -q 'project: criteria-go-dev' <<<"$ns_doc" \
  || fail "namespace label project missing (both labels per the card)"
pass "namespace labels present"

if grep -q '^kind: ClusterRole' "$tmp/once.yaml" || grep -q '^kind: ClusterRoleBinding' "$tmp/once.yaml"; then
  fail "render carries a cluster-scoped RBAC kind; per-project RBAC is namespaced ONLY (ADR 307)"
fi
pass "namespaced-only RBAC (no ClusterRole/ClusterRoleBinding)"

# RoleBindings: operator in-ns + cross-namespace platform intake subject.
rb_doc="$(excerpt_doc "$tmp/once.yaml" RoleBinding)"
grep -q 'name: criteria-kanboard-watcher-intake' <<<"$rb_doc" \
  || fail "missing the platform intake RoleBinding (kanboard-watcher stamps post-cutover runs into the project ns)"
grep -q '  namespace: criteria-jobs' <<<"$rb_doc" \
  || fail "the intake RoleBinding does not bind the platform SA namespace (criteria-jobs)"
grep -q '  namespace: criteria-go-dev' <<<"$rb_doc" \
  || fail "no in-project RoleBinding subject for the project operator SA"
pass "intake write path: platform watcher SA bound into the project namespace"

# criteria-runner SA must exist: the OpenBao role is bounded to it.
grep -q '^kind: ServiceAccount$' "$tmp/once.yaml" || fail "no ServiceAccount documents"
grep -q 'name: criteria-runner$' "$tmp/once.yaml" || fail "no criteria-runner SA (OpenBao role target)"
pass "service accounts present (operator + OpenBao-bound runner)"

# 4) The operator Deployment must carry the per-project shape.
deploy_awk='
/^kind: Deployment$/{d=1}
d&&/^        image: localhost:5000\/criteria-k8s:dev$/{img=1}
d&&/name: CRITERIA_NAMESPACE/{n=1}
d&&/value: criteria-go-dev$/{ns=1}
d&&/name: ADAPTER_REGISTRY/{r=1}
d&&/value: localhost:5000$/{reg=1}
d&&/castle\.criteria-jobs\.svc\.cluster\.local:8080/{c=1}
d&&/name: CRITERIA_BASE_IMAGE/{b=1}
d&&/value: localhost:5000\/criteria-base:dev$/{bi=1}
d&&/^            memory: 64Mi$/{mreq=1}
d&&/^            memory: 256Mi$/{mlim=1}
d&&/^            cpu: 50m$/{creq=1}
d&&/^            cpu: 500m$/{clim=1}
d&&/name: CR_RETENTION_NAMESPACES/{ret_listed=1}
d&&/name: CR_RETENTION_DAYS$/{rdays=1}
END{exit !(d&&img&&n&&ns&&r&&reg&&c&&b&&bi&&mreq&&mlim&&creq&&clim&&!ret_listed&&rdays)}'
awk "$deploy_awk" "$tmp/once.yaml" \
  || fail "operator Deployment shape wrong (namespaced cache env, shared adapter registry, cross-ns castle FQDN, base image, 64Mi/256Mi + 50m/500m, and INERT retention — no CR_RETENTION_NAMESPACES)"
grep -q 'value: 0s' "$tmp/once.yaml" || fail "retention sweep not disabled at render"
pass "operator Deployment: namespaced cache, shared registry, cross-ns castle, inert retention, card resource shape"

# PVC claim shape (KB-312): criteria-data + criteria-repo per ns, local-path.
# The operator mounts criteria-data itself; criteria-repo is consumed by the
# binding's repo volume claim at run time (jobbuilder), so only criteria-data
# needs a deployment-side claimName reference.
for claim in criteria-data criteria-repo; do
  grep -q '  name: '"${claim}"'$' "$tmp/once.yaml" || fail "no ${claim} PVC rendered"
done
grep -q 'claimName: criteria-data$' "$tmp/once.yaml" || fail "operator does not mount the project data PVC"
grep -q '  storageClassName: local-path$' "$tmp/once.yaml" \
  || fail "project PVCs must be local-path (host-affinity same-host rule, docs/pod-topology.md)"
pass "project PVCs: criteria-data + criteria-repo on local-path (same-host rule)"

# SPC wiring: openbao provider, per-ns roleName, no NEW credential paths.
spc_count="$(grep -c '^kind: SecretProviderClass$' "$tmp/once.yaml")"
[ "$spc_count" = "3" ] || fail "expected 3 SecretProviderClasses (kanboard/cpilot/castle-token), got ${spc_count}"
grep -q '    roleName: criteria-go-dev$' "$tmp/once.yaml" || fail "SPCs do not use the per-ns OpenBao roleName"
if grep -q 'secretPath: criteria/' "$tmp/once.yaml"; then
  if grep 'secretPath: criteria/' "$tmp/once.yaml" | grep -qv 'criteria/data/linear\|criteria/metadata/linear'; then
    fail "new OpenBao credential paths introduced; cutover reuses existing paths only (no new credentials, KB-312)"
  fi
else
  fail "no OpenBao secretPath wiring in the render"
fi
pass "SPC wiring: per-ns OpenBao role over the existing credential paths"

# 5) The binding CM is spliced verbatim from the project's binding example:
#    payload JSON equality from example and committed render, and the
#    checksum stamped in the render header matches the payload's canonical
#    hash (recomputed through the script's own mirror of the operator's
#    admission algorithm; the Go gate in criteria-k8s/internal/binding pins
#    that algorithm against the shipped example independently).
payload_of() { awk '/^[ ]*binding.json: \|$/{b=1;next} b&&(/^[ \t]/||$0==""){sub(/^[ \t]*/,"",$0);print;next} b{exit}' "$1"; }
jq -e . < <(payload_of "$examples/binding-criteria-go-dev.yaml") >/dev/null || fail "binding example payload is not JSON"
cmp <(payload_of "$examples/binding-criteria-go-dev.yaml") <(payload_of k8s/projects/criteria-go-dev.yaml) \
  || fail "rendered binding CM payload differs from the binding example (the splice is verbatim)"
checksum="$(./k8s/project-lifecycle.sh binding-checksum --namespace criteria-go-dev)"
[ -n "${checksum}" ] || fail "binding-checksum produced no digest"
grep -q "${checksum}" k8s/projects/criteria-go-dev.yaml \
  || fail "rendered target state does not stamp the payload's canonical checksum (${checksum}) — the operator stamps this at admission"
[ "$(echo -n "$checksum" | wc -c)" = "64" ] || fail "checksum is not sha256 hex"
pass "binding CM: verbatim splice, canonical checksum matches render stamp"

# 6) The binding's PVC claims follow the KB-312 per-ns shape.
claims="$(jq -r '.volumes[] | select(.kind=="pvc") | .claim' < <(payload_of "$examples/binding-criteria-go-dev.yaml"))"
echo "$claims" | grep -qx 'criteria-data' || fail "binding data claim is not criteria-data (per-ns PVC shape)"
echo "$claims" | grep -qx 'criteria-repo' || fail "binding repo claim is not criteria-repo (per-ns PVC shape)"
echo "$claims" | grep -q 'criteria-go-dev-' && fail "binding carries project-suffixed claim names; cutovers unify on per-ns names"
pass "binding claims resolve the per-ns PVCs (criteria-data, criteria-repo)"

# 7) Registry/route behavior the cutover leaves behind (offline mirror of
#    verify's global-CM legs against the routes example of record).
routes_json="$(awk '/^[ ]*routes.json: \|$/{b=1;next} b&&(/^[ \t]/||$0==""){sub(/^[ \t]*/,"",$0);print;next} b{exit}' "$examples/routes-configmap.yaml")"
[ -n "$routes_json" ] || fail "routes example carries no routes.json"
jq -e --arg ns criteria-go-dev \
  '.projectRegistry | to_entries[] | select(.value.namespace == $ns) | .value.cutover == "done"' \
  >/dev/null <<<"$routes_json" || fail "registry marker for the go domain is not cutover:done"
jq -e '.routes[] | select(.workflow=="kanboard-develop-url" and (.project=="Criteria Go Dev"))' \
  >/dev/null <<<"$routes_json" || fail "the go route no longer routes to Criteria Go Dev (the registry entry is routing metadata and stays)"
library_json="$(jq -Sc '.workflowLibrary' <<<"$routes_json")"
if echo "$library_json" | grep -q 'criteria-go-dev'; then
  fail "global workflowLibrary carries criteria-go-dev binding data; post-cutover the project ns owns it"
fi
if echo "$library_json" | grep -q 'peer-go'; then
  fail "global workflowLibrary carries go-domain peer images; project-owned go-dev images live in the binding, not the platform CM"
fi
pass "routes: registry entry + route intact, workflowLibrary carries no go-domain binding data"

# 8) Refusals: the tool fails closed on the platform ns and unknown projects.
if "$lifecycle" render --namespace criteria-jobs >/dev/null 2>&1; then
  fail "render allowed the platform namespace criteria-jobs as a project namespace"
fi
if "$lifecycle" render --namespace no-such-project 2>/dev/null >/dev/null; then
  fail "render allowed a namespace with no binding example (cutovers are binding-first)"
fi
pass "fail-closed refusals (platform ns, unknown project)"

echo "PASS: KB-312 project lifecycle regression test (${lifecycle}, k8s/projects/criteria-go-dev.yaml, binding checksum ${checksum})"