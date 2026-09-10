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

run_dir="/data/.criteria/runs/$CRITERIA_RUN_JOB_NAME"

# Poll the per-run discovery directory written by the runner. Adapters mount
# /data RW to the same shared PVC, so the runner can publish the dial
# address, bearer token, and pinned digest without any CSI volumes or
# credential env vars in the adapter pod spec.
poll_file() {
    path="$1"
    until [ -r "$path" ] && [ -s "$path" ]; do
        sleep 1
    done
    cat "$path"
}

CRITERIA_REMOTE_HOST=$(poll_file "$run_dir/host")
CRITERIA_REMOTE_TOKEN=$(poll_file "$run_dir/token")
CRITERIA_REMOTE_DIGEST=$(poll_file "$run_dir/digest-$ADAPTER_KIND")

export CRITERIA_REMOTE_HOST
export CRITERIA_REMOTE_TOKEN
export CRITERIA_REMOTE_DIGEST

# Shell steps run git/gh commands; set a generic bot identity and the gh
# credential helper so the adapter's own token is used for HTTPS clones.
if [ "$ADAPTER_KIND" = "shell" ]; then
    git config --global credential.https://github.com.helper '!gh auth git-credential'
    git config --global user.name "criteria-runner"
    git config --global user.email "criteria-runner@brokenbots.invalid"
fi

# The adapter image entrypoint is the remote runner phone-home binary. All
# credentials reach the adapter over the OpenSession SDK contract; this
# wrapper exports only the connection/digest metadata from discovery files.
exec /usr/local/bin/criteria-adapter-remote-runner
