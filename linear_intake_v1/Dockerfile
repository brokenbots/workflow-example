FROM golang:1.26.6-alpine AS criteria-builder

ARG CRITERIA_VERSION=v0.5.9
ARG CRITERIA_COMMIT=87c9b718bf532ab80c5ca8885aea80a91af8462e

RUN apk add --no-cache git
RUN git clone --filter=blob:none --branch "$CRITERIA_VERSION" \
        https://github.com/brokenbots/criteria.git /src/criteria \
    && test "$(git -C /src/criteria rev-parse HEAD)" = "$CRITERIA_COMMIT"
COPY linear_intake_v1/criteria-v0.5.9-sandbox-target.patch /tmp/criteria-sandbox.patch
RUN printf '\n' >> /tmp/criteria-sandbox.patch \
    && git -C /src/criteria apply --recount /tmp/criteria-sandbox.patch \
    && cd /src/criteria \
    && CGO_ENABLED=0 go build \
        -ldflags "-X github.com/brokenbots/criteria/workflow/version.Version=${CRITERIA_VERSION}" \
        -o /out/criteria ./cmd/criteria

FROM alpine:3.20
ARG TARGETARCH
ARG CRITERIA_VERSION=v0.5.9

USER root
RUN apk add --no-cache bash ca-certificates curl git github-cli go jq make \
    && addgroup -S -g 10001 criteria \
    && adduser -S -D -h /home/criteria -u 10001 -G criteria criteria \
    && case "$TARGETARCH" in \
        amd64) checksum=636d15a288339a1108de0079a1784debe39920530e999a83c29439ee74f4c8c6 ;; \
        arm64) checksum=3b650e2f856f608929aed0fde27d88fea3f4153ea7d4e9454b3250539b9e82fe ;; \
        *) echo "unsupported architecture: $TARGETARCH" >&2; exit 1 ;; \
    esac \
    && archive="criteria-${CRITERIA_VERSION}-linux-${TARGETARCH}.tar.gz" \
    && curl -fL "https://github.com/brokenbots/criteria/releases/download/${CRITERIA_VERSION}/${archive}" -o "/tmp/${archive}" \
    && echo "$checksum  /tmp/${archive}" | sha256sum -c - \
    && mkdir -p /tmp/criteria /home/criteria/.criteria/plugins \
    && tar -xzf "/tmp/${archive}" -C /tmp/criteria \
    && install -m 0755 /tmp/criteria/criteria /usr/local/bin/criteria \
    && install -m 0755 /tmp/criteria/criteria-adapter-* /home/criteria/.criteria/plugins/ \
    && rm -rf "/tmp/${archive}" /tmp/criteria

# Patch v0.5.9 Linux sandbox target resolution and allowed-network handling.
COPY --from=criteria-builder /out/criteria /usr/local/bin/criteria

ENV HOME=/home/criteria

COPY . /workflows
COPY linear_intake_v1/container-entrypoint.sh /usr/local/bin/linear-intake
RUN criteria adapter lock /workflows/linear_intake_v1 \
    && chmod 0755 /usr/local/bin/linear-intake \
    && mkdir -p /repo /data/intake /data/triage /home/criteria/.config/gh \
    && chown -R criteria:criteria /repo /data /home/criteria/.config /home/criteria/.criteria

ENV CRITERIA_SHELL_ALLOWED_PATHS=/repo:/data:/workflows

USER criteria
WORKDIR /workflows
ENTRYPOINT ["/usr/local/bin/linear-intake"]