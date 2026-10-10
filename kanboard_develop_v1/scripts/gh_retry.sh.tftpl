# gh_retry.sh.tftpl — shared bounded-backoff retry layer for GitHub API surfaces (KB-219).
#
# Rendered into step scripts via templatefile (or file() in the entrypoint tree)
# before any gh call is made. One copy per tree keeps each tree self-contained:
# this file is vendored VERBATIM into the scripts/ dir of devops_triage_v1, the
# Linear intake tree, kanboard_develop_v1 and kanboard_triage_v1; the canonical
# copy lives beside the comment's home tree (workstream_handler_v1). All copies
# are compared byte-for-byte by the tree suites; edit one, sync all.
#
# Contract (KB-219, dave ruling 2026-10-08):
#   * Retries ONLY transient transport errors (TLS handshake timeout,
#     connection reset/refused/timed out, DNS failure, 5xx, rate limit).
#     Verdict-shaped results are never retried: an rc 0 with any body passes
#     through on the first attempt, and non-transient failures (4xx, gh rc 8
#     "no checks") return immediately, preserving pending/failure semantics.
#   * Bounded: GH_RETRY_MAX_ATTEMPTS (default 4) with GH_RETRY_BACKOFF_SECONDS
#     (default "30 60 120", last value repeats for later attempts) and a
#     cumulative GH_RETRY_SCRIPT_SLEEP_BUDGET (default 210s) shared by every
#     wrapper call in one rendered script, so a step's wall clock extends by
#     at most ~210s. Exhaustion returns the last rc — callers keep their
#     honest failure route (awaiting_human), no infinite loops. The schedule
#     spans a 60-90s transport blackout (KB-48/KB-225 evidence): attempts at
#     t0/t30/t90/t210 all land inside it, so a single-shot immediate retry
#     (the KB-48 era one, same-window, always failed) is no longer the only
#     second chance.
#   * HTTP 4xx are NEVER retried: they are auth/not-found-shaped client
#     errors and must surface the caller's named failure immediately. curl -f
#     reports them as "The requested URL returned error: 4xx" and that text
#     classifies as http_4xx_client_error (the KB-219 shape sent it to
#     http_5xx and needlessly retried). 429 stays transient (rate_limit).
#   * attempt#= + error class go to stderr per attempt as run evidence for
#     the run log / castle stream. Step stdout stays reserved for
#     protocol-shaped output; teardown_worktree merges the streams itself.
#   * Capture sites that previously used `2>&1` call gh_retry_merged instead:
#     on a terminal result the helper folds stderr into stdout so the captured
#     value keeps the same shape it had before this wrapper existed.
#   * Terminal results are echoed to ${TMPDIR:-/tmp}/gh_retry-last.$$ (one
#     line "exhausted=<0|1> class=<name> attempts=<n>", rewritten after every
#     terminal result of this shell's pid) so a command-substitution subshell
#     can report exhaustion back to the parent shell — env vars set inside
#     $(...) never survive the fork. Steps use this with
#     gh_retry_transport_verdict below to re-emit a schedule exhaustion as a
#     protocol verdict instead of dying under `set -e` with no evidence.
#
# Per-call knobs (plain env, POSIX-sh semantics): GH_RETRY_MAX_ATTEMPTS,
# GH_RETRY_BACKOFF_SECONDS, GH_RETRY_SCRIPT_SLEEP_BUDGET. Globals after a
# call: GH_RETRY_LAST_ERR, GH_RETRY_ATTEMPTS, GH_RETRY_ERR_CLASS (set when
# transient, cleared otherwise), GH_RETRY_EXHAUSTED ("1" only on schedule
# exhaustion), GH_RETRY_SLEEP_USED (running sleep total across calls in this
# script).

gh_retry_is_transient() {
    # Classifies combined command output as a transient transport error.
    # Sets GH_RETRY_ERR_CLASS and returns 0 when transient, else returns 1.
    # `tr` is an optional dependency: minimal-PATH shells (busy k8s probe
    # shells) may not have it, and a missing tr must never crash the
    # classification or leak "not found" noise into the evidence channel.
    # Without tr the raw text is classified as-is: lower/mixed-case
    # spellings still match, all-upper text conservatively reads as
    # non-transient (pass-through, no retry).
    GH_RETRY_ERR_CLASS=""
    _grt_text=$1
    if [ -x "${_GH_RETRY_TR_BIN}" ]; then
        _grt_text=$(printf '%s' "$_grt_text" | "$_GH_RETRY_TR_BIN" '[:upper:]' '[:lower:]' 2>/dev/null)
    fi
    case "$_grt_text" in
        *"tls handshake"*|*"handshake timeout"*|*"ssl connect"*|*"handshake timed out"*|*"tlsv1"*|*"ssl3"*|*"ssl_read"*|*"ssl_connect_error"*|*"ssl peer certificate"*)
            GH_RETRY_ERR_CLASS="tls_handshake_timeout" ;;
        *"connection reset"*|*"broken pipe"*)
            GH_RETRY_ERR_CLASS="connection_reset" ;;
        *"connection refused"*)
            GH_RETRY_ERR_CLASS="connection_refused" ;;
        *"connection timed out"*|*"i/o timeout"*|*"request timed out"*|*"timed out"*|*"failed to connect"*|*"unable to connect"*)
            GH_RETRY_ERR_CLASS="connect_timeout" ;;
        *"could not resolve host"*|*"name resolution"*|*"no address associated"*)
            GH_RETRY_ERR_CLASS="dns_failure" ;;
        *"no route to host"*|*"network is unreachable"*|*"network unreachable"*)
            GH_RETRY_ERR_CLASS="network_unreachable" ;;
        *"unexpected eof"*|*"early eof"*|*"empty reply"*|*"rpc failed"*|*"http/2 stream"*)
            GH_RETRY_ERR_CLASS="connection_eof" ;;
        # KB-235: rate limit before the generic HTTP branches — GitHub's 403
        # secondary-limit text ("API rate limit exceeded") and curl's 429
        # ("The requested URL returned error: 429", "too many requests") are
        # transient even though their codes are 4xx-shaped.
        *"rate limit"*|*"abuse detection"*|*"too many requests"*|*"returned error: 429"*|*"http 429"*)
            GH_RETRY_ERR_CLASS="rate_limit" ;;
        # KB-235: client errors are never retried. curl -f reports them as
        # "(22) The requested URL returned error: 4xx" — the KB-219 shape
        # classified that text as http_5xx and needlessly backed off before
        # surfacing the failure; the digit now routes the class. gh's named
        # auth/not-found texts are covered too.
        *"http 400"*|*"http 401"*|*"http 403"*|*"http 404"*|*"http 405"*|*"http 409"*|*"http 422"*|*"http 4"*|*"returned error: 4"*|*"not found"*|*"bad credentials"*|*"unauthorized"*|*"unauthorised"*|*"forbidden"*|*"access denied"*|*"resource not accessible"*)
            return 1 ;;
        *"http 5"*|*"503"*|*"502"*|*"504"*|*"internal server error"*|*"bad gateway"*|*"service unavailable"*|*"gateway timeout"*|*"api error"*|*"server error"*)
            GH_RETRY_ERR_CLASS="http_5xx" ;;
        *)
            return 1 ;;
    esac
    return 0
}

gh_retry_log() {
    # Run evidence channel: stderr, never the captured protocol stream.
    printf 'gh_retry: %s\n' "$1" >&2
}

gh_retry_record_terminal() {
    # Cross-subshell terminal signal (KB-235): env assignments made inside
    # $(...) never reach the parent shell, so the engine also echoes the last
    # terminal result of this shell's pid to a fixed state file, rewritten on
    # EVERY terminal result (success, pass-through and exhaustion alike — a
    # fresh win must clear a stale exhaustion). Steps read it via
    # gh_retry_transport_dead / gh_retry_transport_verdict after a failed
    # capture. Best-effort: an unwritable file degrades to the honest-failure
    # route, never an error.
    printf '%s\n' "exhausted=$1 class=${2:-none} attempts=${3:-0}" \
        > "${TMPDIR:-/tmp}/gh_retry-last.$$" 2>/dev/null || true
}

gh_retry_transport_dead() {
    # True when this shell's most recent terminal wrapper result was a
    # schedule exhaustion on a transient transport error. Sets
    # GH_RETRY_DEAD_CLASS and GH_RETRY_DEAD_ATTEMPTS when true. Reads the
    # state file (see gh_retry_record_terminal) — plain POSIX, no external
    # tools, so minimal-PATH shells keep using it.
    _grtd_line=""
    read -r _grtd_line < "${TMPDIR:-/tmp}/gh_retry-last.$$" 2>/dev/null || return 1
    case "$_grtd_line" in
        *exhausted=1*)
            GH_RETRY_DEAD_CLASS=unknown
            GH_RETRY_DEAD_ATTEMPTS=unknown
            for _grtd_field in $_grtd_line; do
                case "$_grtd_field" in
                    class=*) GH_RETRY_DEAD_CLASS=${_grtd_field#class=} ;;
                    attempts=*) GH_RETRY_DEAD_ATTEMPTS=${_grtd_field#attempts=} ;;
                esac
            done
            return 0 ;;
    esac
    return 1
}

gh_retry_transport_verdict() {
    # Step-side guard for transport-prone capture sites (KB-235):
    #   value=$(gh ...) || gh_retry_transport_verdict "$?" "$pr" "<stage>"
    # If the last wrapper call exhausted its bounded schedule on a transient
    # transport error, re-emit the death as the named protocol verdict
    # (status:transport_failed ... evidence lines) and exit 0, so a status
    # route can park the run on the named diagnostic with its verdict state
    # intact instead of reporting failed (KB-48/KB-225). Any other failure
    # exits the ORIGINAL rc — the caller's honest failure route (KB-24)
    # stays in charge. Exits inside the caller's shell: use only at sites
    # whose sole failure handling is this guard.
    [ $# -ge 3 ] || { echo "gh_retry_transport_verdict needs rc, pr and stage" >&2; exit 2; }
    _grtv_rc=$1
    _grtv_pr=$2
    _grtv_stage=$3
    if gh_retry_transport_dead; then
        echo "status:transport_failed"
        echo "pr_number=$_grtv_pr"
        echo "transport_class=${GH_RETRY_DEAD_CLASS:-unknown}"
        echo "transport_attempts=${GH_RETRY_DEAD_ATTEMPTS:-unknown}"
        echo "transport_stage=$_grtv_stage"
        exit 0
    fi
    exit "$_grtv_rc"
}

gh_retry_run() {
    # Internal engine. First arg is the emission mode ("stream" | "merged"),
    # the rest is the command to run.
    GH_RETRY_SLEEP_USED=${GH_RETRY_SLEEP_USED:-0}
    GH_RETRY_SEQ=${GH_RETRY_SEQ:-0}
    GH_RETRY_EXHAUSTED=0
    GH_RETRY_ERR_CLASS=""
    # Must hold with the mode still in argv, so a bare single-arg call
    # (just "stream"/"merged", e.g. `gh_retry` with no command) is rejected
    # here instead of shifting into a post-shift `$1` read that dies under
    # set -u.
    [ $# -ge 2 ] || { gh_retry_log "attempt=1/0 result=pass_through class=non_transient label=none (no command given)"; gh_retry_record_terminal 0 non_transient 1; return 2; }
    _gr_mode=$1
    shift
    # If the caller passed `gh` as the command, the sourced shell's gh alias
    # would resolve to our own function and nest a second engine. Rebuild the
    # argv with the pre-alias binary path so exactly one engine wraps the call.
    if [ "$1" = "gh" ]; then
        shift
        if [ -x "${_GH_RETRY_GH_BIN}" ]; then
            # Pre-alias absolute path: exec directly, exactly one engine wrap.
            set -- "$_GH_RETRY_GH_BIN" "$@"
        else
            # gh was absent on PATH at source time: resolve via `command`,
            # which suppresses the gh() shadow, so a missing binary fails
            # honestly instead of recursing into the shadow forever.
            set -- command "$_GH_RETRY_GH_BIN" "$@"
        fi
    fi
    _gr_attempts=${GH_RETRY_MAX_ATTEMPTS:-4}
    case "$_gr_attempts" in ''|*[!0-9]*) _gr_attempts=4 ;; esac
    [ "$_gr_attempts" -ge 1 ] || _gr_attempts=1
    _gr_backoff=${GH_RETRY_BACKOFF_SECONDS:-"30 60 120"}
    _gr_budget=${GH_RETRY_SCRIPT_SLEEP_BUDGET:-210}
    case "$_gr_budget" in ''|*[!0-9]*) _gr_budget=210 ;; esac
    _gr_label=$1
    [ $# -ge 2 ] && _gr_label="$_gr_label $2"
    # Stable run-evidence label: resolve to the basename so labels like
    # "gh api" don't embed the resolved binary's full path (which varies by
    # stub vs. image and rots log-greps).
    case "$_gr_label" in */*) _gr_label=${_gr_label##*/} ;; esac
    _gr_attempt=0
    _gr_out=""
    _gr_err=""
    _gr_rc=0
    while :; do
        _gr_attempt=$((_gr_attempt + 1))
        GH_RETRY_ATTEMPTS=$_gr_attempt
        GH_RETRY_SEQ=$((GH_RETRY_SEQ + 1))
        _gr_errfile="${TMPDIR:-/tmp}/gh_retry-stderr.$$.${GH_RETRY_SEQ}.${_gr_attempt}"
        if _gr_out="$("$@" 2>"$_gr_errfile")"; then
            _gr_rc=0
        else
            _gr_rc=$?
        fi
        _gr_err=$(cat "$_gr_errfile" 2>/dev/null || true)
        rm -f "$_gr_errfile"
        GH_RETRY_LAST_ERR=$_gr_err
        if [ "$_gr_rc" -eq 0 ]; then
            # Verdict-shaped success: pass through on the first attempt.
            [ "$_gr_attempt" -gt 1 ] && gh_retry_log "$_gr_label attempt=$_gr_attempt/$_gr_attempts result=success class=none"
            gh_retry_record_terminal 0 none "$_gr_attempt"
            printf '%s' "$_gr_out"
            if [ "$_gr_mode" = "merged" ] && [ -n "$_gr_err" ]; then
                printf '%s' "$_gr_err" >&1
            else
                [ -n "$_gr_err" ] && printf '%s' "$_gr_err" >&2
            fi
            return 0
        fi
        if gh_retry_is_transient "$_gr_err"; then :
        elif gh_retry_is_transient "$_gr_out"; then :
        else
            # Non-transient (4xx, gh rc 8 "no checks", verdict-shaped bodies):
            # never retried — the caller's verdict logic stays in charge.
            gh_retry_log "$_gr_label attempt=$_gr_attempt/$_gr_attempts result=pass_through class=non_transient"
            gh_retry_record_terminal 0 non_transient "$_gr_attempt"
            printf '%s' "$_gr_out"
            if [ "$_gr_mode" = "merged" ]; then
                [ -n "$_gr_err" ] && printf '%s' "$_gr_err" >&1
            else
                [ -n "$_gr_err" ] && printf '%s' "$_gr_err" >&2
            fi
            return "$_gr_rc"
        fi
        if [ "$_gr_attempt" -ge "$_gr_attempts" ]; then
            gh_retry_log "$_gr_label attempt=$_gr_attempt/$_gr_attempts result=exhausted class=$GH_RETRY_ERR_CLASS"
            GH_RETRY_EXHAUSTED=1
            gh_retry_record_terminal 1 "$GH_RETRY_ERR_CLASS" "$_gr_attempt"
            printf '%s' "$_gr_out"
            if [ "$_gr_mode" = "merged" ]; then
                [ -n "$_gr_err" ] && printf '%s' "$_gr_err" >&1
            else
                [ -n "$_gr_err" ] && printf '%s' "$_gr_err" >&2
            fi
            return "$_gr_rc"
        fi
        _gr_wait=""
        _gr_seen=0
        for _gr_s in $_gr_backoff; do
            case "$_gr_s" in *[!0-9]*) continue ;; esac
            _gr_seen=$((_gr_seen + 1))
            _gr_wait="$_gr_s"
            [ "$_gr_seen" -ge "$_gr_attempt" ] && break
        done
        [ -n "$_gr_wait" ] || _gr_wait=30
        # Cumulative clamp: sleep up to the remaining shared script budget,
        # never past it. An exhausted or negative allowance collapses to 0.
        _gr_allowance=$((_gr_budget - GH_RETRY_SLEEP_USED))
        [ "$_gr_allowance" -le 0 ] && _gr_allowance=0
        [ "$_gr_wait" -le "$_gr_allowance" ] || _gr_wait=$_gr_allowance
        [ "$_gr_wait" -ge 0 ] || _gr_wait=0
        if [ "$_gr_wait" -gt 0 ]; then
            sleep "$_gr_wait"
            GH_RETRY_SLEEP_USED=$((GH_RETRY_SLEEP_USED + _gr_wait))
        fi
        gh_retry_log "$_gr_label attempt=$_gr_attempt/$_gr_attempts result=retry class=$GH_RETRY_ERR_CLASS wait=${_gr_wait}s"
    done
}

gh_retry() {
    # Explicit entrypoint for non-gh commands (e.g. git push over https).
    gh_retry_run stream "$@"
}

gh_retry_merged() {
    # Capture-site entrypoint: replaces `$(cmd 2>&1)` so terminal stderr stays
    # inside the captured value while attempt logs keep going to stderr.
    gh_retry_run merged "$@"
}

# Drop-in shadow: wraps /usr/bin/gh in the retry engine without rewriting
# call sites. gh_retry_run receives the resolved binary path so the shadow
# cannot recurse into itself.
_GH_RETRY_GH_BIN=$(command -v gh 2>/dev/null || true)
[ -n "$_GH_RETRY_GH_BIN" ] || _GH_RETRY_GH_BIN=gh
# Optional classification dependency (see gh_retry_is_transient): resolved
# once at source time, and stays unset when tr is absent so the engine
# never execs a missing tool.
_GH_RETRY_TR_BIN=$(command -v tr 2>/dev/null || true)
gh() { gh_retry_run stream "$_GH_RETRY_GH_BIN" "$@"; }
