#!/bin/sh
set -eu

if [ -z "${ADAPTER_KIND:-}" ]; then
    echo "ADAPTER_KIND must be set to shell or copilot" >&2
    exit 1
fi
if [ -z "${CRITERIA_RUN_JOB_NAME:-}" ]; then
    echo "CRITERIA_RUN_JOB_NAME must be set" >&2
    exit 1
fi

run_dir_root="${CRITERIA_RUN_DIR_ROOT:-/data/.criteria/runs}"
run_dir="$run_dir_root/$CRITERIA_RUN_JOB_NAME"

# In per-scope mode the operator passes the connection metadata directly via
# the environment. When any value is missing we fall back to polling the
# per-run discovery directory written by the runner.
poll_file() {
    path="$1"
    until [ -r "$path" ] && [ -s "$path" ]; do
        sleep 1
    done
    cat "$path"
}

if [ -z "${CRITERIA_REMOTE_HOST:-}" ]; then
    CRITERIA_REMOTE_HOST=$(poll_file "$run_dir/host")
fi
if [ -z "${CRITERIA_REMOTE_TOKEN:-}" ]; then
    if [ -n "${CRITERIA_REMOTE_TOKEN_FILE:-}" ] && [ -r "$CRITERIA_REMOTE_TOKEN_FILE" ]; then
        CRITERIA_REMOTE_TOKEN=$(cat "$CRITERIA_REMOTE_TOKEN_FILE")
    else
        CRITERIA_REMOTE_TOKEN=$(poll_file "$run_dir/token")
    fi
fi
if [ -z "${CRITERIA_REMOTE_DIGEST:-}" ]; then
    # Per-scope pods carry the workflow's adapter instance name; prefer its
    # own pinned digest when the runner published one, so instances of the
    # same kind pinned to different versions resolve correctly (CRI-140).
    # The file is only used when it already exists: the runner publishes the
    # instance digests at startup, so a missing file means the instance has
    # no lockfile entry and the kind digest is the correct fallback.
    if [ -n "${CRITERIA_ADAPTER_NAME:-}" ] && [ -r "$run_dir/digest-$CRITERIA_ADAPTER_NAME" ]; then
        CRITERIA_REMOTE_DIGEST=$(poll_file "$run_dir/digest-$CRITERIA_ADAPTER_NAME")
    else
        CRITERIA_REMOTE_DIGEST=$(poll_file "$run_dir/digest-$ADAPTER_KIND")
    fi
fi

export CRITERIA_REMOTE_HOST
export CRITERIA_REMOTE_TOKEN
export CRITERIA_REMOTE_DIGEST

# Scope-tagged handshakes (CRI-116). The scope id/tag is empty in legacy
# run-duration mode.
if [ -n "${CRITERIA_SCOPE_ID:-}" ]; then
    export CRITERIA_SCOPE_ID
fi
if [ -n "${CRITERIA_SCOPE_TAG:-}" ]; then
    export CRITERIA_SCOPE_TAG
fi

# Shell steps run git/gh commands; set a generic bot identity and the gh
# credential helper so the adapter's own token is used for HTTPS clones.
if [ "$ADAPTER_KIND" = "shell" ]; then
    git config --global credential.https://github.com.helper '!gh auth git-credential'
    git config --global user.name "criteria-runner"
    git config --global user.email "criteria-runner@brokenbots.invalid"
fi

# The adapter image entrypoint is the remote runner phone-home binary. All
# credentials reach the adapter over the OpenSession SDK contract; this
# wrapper exports only the connection/digest metadata from discovery files
# or from the per-scope values supplied by the operator.
if command -v criteria-adapter-remote-runner > /dev/null 2>&1; then
    exec criteria-adapter-remote-runner
fi
exec /usr/local/bin/criteria-adapter-remote-runner
