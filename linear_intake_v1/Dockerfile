FROM golang:1.26.6-bookworm
ARG TARGETARCH
ARG CRITERIA_VERSION=v0.5.11
ARG COPILOT_VERSION=1.0.82
ARG GH_VERSION=2.98.0
ARG LINEAR_CLI_VERSION=0.0.5

ENV CGO_ENABLED=0

USER root
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        bash \
        build-essential \
        ca-certificates \
        curl \
        fd-find \
        git \
        git-lfs \
        jq \
        less \
        make \
        nodejs \
        npm \
        openssh-client \
        procps \
        python3 \
        ripgrep \
        shellcheck \
        unzip \
        zip \
    && rm -rf /var/lib/apt/lists/* \
    && ln -sf /bin/bash /bin/sh \
    && npm install --global \
        "@github/copilot@${COPILOT_VERSION}" \
        "@linear/cli@${LINEAR_CLI_VERSION}" \
    && ln -s /usr/local/bin/lin /usr/local/bin/linear \
    && npm cache clean --force \
    && groupadd --system --gid 10001 criteria \
    && useradd --system --uid 10001 --gid criteria --home-dir /home/criteria --create-home criteria \
    && gh_archive="gh_${GH_VERSION}_linux_${TARGETARCH}.tar.gz" \
    && curl -fL "https://github.com/cli/cli/releases/download/v${GH_VERSION}/${gh_archive}" -o "/tmp/${gh_archive}" \
    && curl -fL "https://github.com/cli/cli/releases/download/v${GH_VERSION}/gh_${GH_VERSION}_checksums.txt" -o /tmp/gh-checksums.txt \
    && (cd /tmp && grep " ${gh_archive}$" gh-checksums.txt | sha256sum -c -) \
    && tar -xzf "/tmp/${gh_archive}" -C /tmp \
    && install -m 0755 "/tmp/gh_${GH_VERSION}_linux_${TARGETARCH}/bin/gh" /usr/local/bin/gh \
    && rm -rf "/tmp/${gh_archive}" /tmp/gh-checksums.txt "/tmp/gh_${GH_VERSION}_linux_${TARGETARCH}" \
    && archive="criteria-${CRITERIA_VERSION}-linux-${TARGETARCH}.tar.gz" \
    && curl -fL "https://github.com/brokenbots/criteria/releases/download/${CRITERIA_VERSION}/${archive}" -o "/tmp/${archive}" \
    && curl -fL "https://github.com/brokenbots/criteria/releases/download/${CRITERIA_VERSION}/SHA256SUMS" -o /tmp/criteria-checksums.txt \
    && (cd /tmp && grep " ${archive}$" criteria-checksums.txt | sha256sum -c -) \
    && mkdir -p /tmp/criteria /home/criteria/.local/criteria/adapters \
    && tar -xzf "/tmp/${archive}" -C /tmp/criteria \
    && install -m 0755 /tmp/criteria/criteria /usr/local/bin/criteria \
    && install -m 0755 /tmp/criteria/criteria-adapter-* /home/criteria/.local/criteria/adapters/ \
    && rm -rf "/tmp/${archive}" /tmp/criteria-checksums.txt /tmp/criteria

ENV HOME=/home/criteria \
    CRITERIA_HOME=/home/criteria/.local/criteria

COPY . /workflows
COPY linear_intake_v1/container-entrypoint.sh /usr/local/bin/linear-intake
RUN criteria adapter lock /workflows/linear_intake_v1 \
    && chmod 0755 /usr/local/bin/linear-intake \
    && mkdir -p /repo /data/intake /data/triage /home/criteria/.config/gh \
    && chown -R criteria:criteria /repo /data /home/criteria/.config /home/criteria/.local/criteria

ENV CRITERIA_SHELL_ALLOWED_PATHS=/repo:/data:/workflows

USER criteria
WORKDIR /workflows
ENTRYPOINT ["/usr/local/bin/linear-intake"]