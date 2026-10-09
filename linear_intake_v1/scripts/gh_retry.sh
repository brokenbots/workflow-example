# gh_retry.sh.tftpl — shared bounded-backoff retry layer for GitHub API surfaces (KB-219).
#
# Rendered into step scripts via templatefile (or file() in the entrypoint tree)
# before any gh call is made. One copy per tree keeps each tree self-contained:
# workstream_handler_v1/scripts (canonical), devops_triage_v1/scripts and
# linear_intake_v1/scripts are vendored copies of this file.
#
# Contract (KB-219, dave ruling 2026-10-08):
#   * Retries ONLY transient transport errors (TLS handshake timeout,
#     connection reset/refused/timed out, DNS failure, 5xx, rate limit).
#     Verdict-shaped results are never retried: an rc 0 with any body passes
#     through on the first attempt, and non-transient failures (4xx, gh rc 8
#     "no checks") return immediately, preserving pending/failure semantics.
#   * Bounded: GH_RETRY_MAX_ATTEMPTS (default 4) with GH_RETRY_BACKOFF_SECONDS
#     (default "15 30 60", last value repeats for later attempts) and a
#     cumulative GH_RETRY_SCRIPT_SLEEP_BUDGET (default 105s) shared by every
#     wrapper call in one rendered script, so a step's wall clock extends by
#     at most ~105s. Exhaustion returns the last rc — callers keep their
#     honest failure route (awaiting_human), no infinite loops.
#   * attempt#= + error class go to stderr per attempt as run evidence for
#     the run log / castle stream. Step stdout stays reserved for
#     protocol-shaped output; teardown_worktree merges the streams itself.
#   * Capture sites that previously used `2>&1` call gh_retry_merged instead:
#     on a terminal result the helper folds stderr into stdout so the captured
#     value keeps the same shape it had before this wrapper existed.
#
# Per-call knobs (plain env, POSIX-sh semantics): GH_RETRY_MAX_ATTEMPTS,
# GH_RETRY_BACKOFF_SECONDS, GH_RETRY_SCRIPT_SLEEP_BUDGET. Globals after a
# call: GH_RETRY_LAST_ERR, GH_RETRY_ATTEMPTS, GH_RETRY_SLEEP_USED (running
# sleep total across calls in this script).

gh_retry_is_transient() {
    # Classifies combined command output as a transient transport error.
    # Sets GH_RETRY_ERR_CLASS and returns 0 when transient, else returns 1.
    GH_RETRY_ERR_CLASS=""
    _grt_text=$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]')
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
        *"http 5"*|*"503"*|*"502"*|*"504"*|*"internal server error"*|*"bad gateway"*|*"service unavailable"*|*"gateway timeout"*|*"requested url returned error"*|*"api error"*|*"server error"*)
            GH_RETRY_ERR_CLASS="http_5xx" ;;
        *"rate limit"*|*"abuse detection"*|*"secondary rate limit"*)
            GH_RETRY_ERR_CLASS="rate_limit" ;;
        *)
            return 1 ;;
    esac
    return 0
}

gh_retry_log() {
    # Run evidence channel: stderr, never the captured protocol stream.
    printf 'gh_retry: %s\n' "$1" >&2
}

gh_retry_run() {
    # Internal engine. First arg is the emission mode ("stream" | "merged"),
    # the rest is the command to run.
    GH_RETRY_SLEEP_USED=${GH_RETRY_SLEEP_USED:-0}
    GH_RETRY_SEQ=${GH_RETRY_SEQ:-0}
    [ $# -gt 0 ] || { gh_retry_log "attempt=1/0 result=pass_through class=non_transient label=none (no command given)"; return 2; }
    _gr_mode=$1
    shift
    # If the caller passed `gh` as the command, the sourced shell's gh alias
    # would resolve to our own function and nest a second engine. Rebuild the
    # argv with the pre-alias binary path so exactly one engine wraps the call.
    if [ "$1" = "gh" ]; then
        shift
        set -- "$_GH_RETRY_GH_BIN" "$@"
    fi
    _gr_attempts=${GH_RETRY_MAX_ATTEMPTS:-4}
    case "$_gr_attempts" in ''|*[!0-9]*) _gr_attempts=4 ;; esac
    [ "$_gr_attempts" -ge 1 ] || _gr_attempts=1
    _gr_backoff=${GH_RETRY_BACKOFF_SECONDS:-"15 30 60"}
    _gr_budget=${GH_RETRY_SCRIPT_SLEEP_BUDGET:-105}
    case "$_gr_budget" in ''|*[!0-9]*) _gr_budget=105 ;; esac
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
        [ -n "$_gr_wait" ] || _gr_wait=15
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
_GH_RETRY_GH_BIN=$(command -v gh 2>/dev/null)
[ -n "$_GH_RETRY_GH_BIN" ] || _GH_RETRY_GH_BIN=gh
gh() { gh_retry_run stream "$_GH_RETRY_GH_BIN" "$@"; }
