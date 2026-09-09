#!/bin/sh
set -eu

# Read this container's own CSI-mounted secrets. The copilot sidecar mounts
# both tokens; the shell sidecar mounts only the workflow token.
WORKFLOW_GITHUB_TOKEN=""
if [ -r /secrets/workflow_github_token ]; then
    IFS= read -r WORKFLOW_GITHUB_TOKEN < /secrets/workflow_github_token
fi
if [ -z "$WORKFLOW_GITHUB_TOKEN" ] && [ "$ADAPTER_KIND" = "shell" ]; then
    echo "WORKFLOW_GITHUB_TOKEN is required via /secrets/workflow_github_token" >&2
    exit 1
fi

REVIEWER_GITHUB_TOKEN=""
if [ -r /secrets/reviewer_github_token ]; then
    IFS= read -r REVIEWER_GITHUB_TOKEN < /secrets/reviewer_github_token
fi

# Wait for the workflow-runner to publish the shared remote token.
until [ -r /data/.criteria-remote-token ]; do
    sleep 1
done
CRITERIA_REMOTE_TOKEN=$(cat /data/.criteria-remote-token)
export CRITERIA_REMOTE_TOKEN

if [ -z "${ADAPTER_KIND:-}" ]; then
    echo "ADAPTER_KIND must be set to shell or copilot" >&2
    exit 1
fi

export WORKFLOW_GITHUB_TOKEN
if [ -n "$REVIEWER_GITHUB_TOKEN" ]; then
    export REVIEWER_GITHUB_TOKEN
fi

# Resolve the pinned adapter digest from the workflow lockfile. The same digest
# is used as the adapter's identity over the remote handshake.
lockfile=/workflows/linear_intake_v1/.criteria.lock.hcl
digest=$(awk -v k="criteria-adapter-$ADAPTER_KIND" '
    $0 ~ "reference.*"k { in_entry=1 }
    in_entry && /resolved_digest/ { gsub(/[\" ]/, ""); sub(/^resolved_digest=sha256:/, ""); print; exit }
' "$lockfile")
if [ -z "$digest" ]; then
    echo "criteria adapter $ADAPTER_KIND not found in $lockfile" >&2
    exit 1
fi

binary="/home/criteria/.local/criteria/adapters/sha256-${digest}/criteria-adapter-${ADAPTER_KIND}"
if [ ! -x "$binary" ]; then
    echo "adapter binary not found: $binary" >&2
    exit 1
fi

# Shell steps run git/gh commands; set a generic bot identity and the gh
# credential helper so the adapter's own token is used for HTTPS clones.
if [ "$ADAPTER_KIND" = "shell" ]; then
    git config --global credential.https://github.com.helper '!gh auth git-credential'
    git config --global user.name "criteria-runner"
    git config --global user.email "criteria-runner@brokenbots.invalid"
fi

export CRITERIA_REMOTE_HOST=127.0.0.1:7778
export CRITERIA_REMOTE_DIGEST="sha256:$digest"

exec "$binary"
