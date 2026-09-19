# syntax=docker/dockerfile:1

# Minimal criteria base image for source-mode runs (CRI-230, ADR-0005 D1/D3/D4).
#
# Carries only the criteria binary built from criteria main, git, and
# ca-certificates: no baked /workflows tree and no node/gh/jq toolchain. The
# entry point fetches the CriteriaRun workflow source through the merged
# fetcher (git/archive) and applies it from the CRITERIA_HOME cache with
# fail-closed pin enforcement (CRI-226 --workflow-ref).
#
# The baked workflow image (linear_intake_v1) is unchanged and stays in use for
# image-only mode; wiring this image into the operator is CRI-231.
#
# Do NOT bump the pinned commit blindly: it must be criteria main at
# eae01816c4a2da88449833a233d1b3f6a099bef1 (the merged CRI-215..236 chain:
# remote sources, origin metadata, pin enforcement, cascade slugs, cache
# index, environment identity emission, and the accept_token provision
# emission CRI-237's wire delivery reads) or later.

ARG ALPINE_VERSION=3.20
ARG GO_VERSION=1.26.6
ARG CRITERIA_COMMIT=eae01816c4a2da88449833a233d1b3f6a099bef1
ARG BUILDPLATFORM=linux/amd64

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS builder
ARG CRITERIA_COMMIT
ARG TARGETARCH
RUN apk add --no-cache ca-certificates git
# Pin the exact source commit: the build fails closed if the checkout does not
# resolve to CRITERIA_COMMIT, so the binary can only come from criteria main at
# the reviewed commit or its declared replacement.
RUN git clone --filter=blob:none https://github.com/brokenbots/criteria.git /src
WORKDIR /src
RUN git -c advice.detachedHead=false checkout --quiet "${CRITERIA_COMMIT}" \
 && test "$(git rev-parse HEAD)" = "${CRITERIA_COMMIT}"
# CRITERIA_VERSION is REQUIRED and must be the exact criteria release tag the
# CRITERIA_COMMIT sha corresponds to (e.g. v0.5.25). The image tag mirrors it
# (v0.1-<tag>), possibly patched ahead for container-only fixes. There is
# deliberately NO git-describe fallback: a git-describe string (vX.Y.Z-N-gSHA)
# is a semver PRERELEASE, which the engine's criteria_version gate rejects
# against stable lower bounds - and an image whose engine version is ambiguous
# must not ship to the cluster at all.
ARG CRITERIA_VERSION=""
RUN test -n "${CRITERIA_VERSION}" || { echo "CRITERIA_VERSION is required (the exact criteria release tag, e.g. v0.5.25)"; exit 1; } \
 && CGO_ENABLED=0 GOARCH="${TARGETARCH}" go build -trimpath \
    -ldflags "-X github.com/brokenbots/criteria/workflow/version.Version=${CRITERIA_VERSION}" \
    -o /out/criteria ./cmd/criteria

FROM alpine:${ALPINE_VERSION}
RUN apk add --no-cache ca-certificates git
COPY --from=builder /out/criteria /usr/local/bin/criteria
COPY entrypoint.sh /usr/local/bin/criteria-base-entrypoint
RUN chmod 0755 /usr/local/bin/criteria /usr/local/bin/criteria-base-entrypoint \
 && addgroup -S -g 10001 criteria \
 && adduser -S -u 10001 -G criteria -h /home/criteria criteria \
 && mkdir -p /data/criteria \
 && chown criteria:criteria /data /data/criteria
# The runtime uid is fixed at 10001, but fetched sources may arrive via
# PVC/CSI mounts owned by a different uid, which git refuses as "dubious
# ownership". Tree integrity does not rely on git's ownership model: the
# fetched content must match the caller-declared pin (CRI-226) before
# anything runs, so the ownership check is the only thing being relaxed.
RUN git config --system --add safe.directory '*'
ENV HOME=/home/criteria \
    CRITERIA_HOME=/data/criteria
USER 10001
ENTRYPOINT ["/usr/local/bin/criteria-base-entrypoint"]