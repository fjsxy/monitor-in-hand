FROM golang:1.22.12-bookworm AS go-toolchain

FROM node:22-bookworm

ARG AGENT_UID=1000
ARG AGENT_GID=1000
ARG CODEX_VERSION=0.157.1
ARG HELM_VERSION=v3.22.0

COPY --from=go-toolchain /usr/local/go /usr/local/go

ENV PATH="/usr/local/go/bin:${PATH}"

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    curl \
    git \
    jq \
    make \
    && rm -rf /var/lib/apt/lists/*

RUN curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
      | dd of=/usr/share/keyrings/githubcli-archive-keyring.gpg \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
      > /etc/apt/sources.list.d/github-cli.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends gh \
    && rm -rf /var/lib/apt/lists/*

RUN set -eu; \
    case "$(dpkg --print-architecture)" in \
      amd64) helm_arch=amd64 ;; \
      arm64) helm_arch=arm64 ;; \
      *) echo "unsupported architecture: $(dpkg --print-architecture)" >&2; exit 1 ;; \
    esac; \
    curl -fsSL "https://get.helm.sh/helm-${HELM_VERSION}-linux-${helm_arch}.tar.gz" \
      | tar -xz -C /tmp; \
    install "/tmp/linux-${helm_arch}/helm" /usr/local/bin/helm; \
    rm -r "/tmp/linux-${helm_arch}"

RUN npm install -g "@openai/codex@${CODEX_VERSION}"

RUN groupmod -o -g "${AGENT_GID}" node \
    && usermod -o -u "${AGENT_UID}" -g "${AGENT_GID}" -d /home/agent -m -l agent node \
    && mkdir -p /home/agent/.codex \
    && chown -R "${AGENT_UID}:${AGENT_GID}" /home/agent/.codex

USER ${AGENT_UID}:${AGENT_GID}
WORKDIR /home/agent

ENTRYPOINT ["sleep", "infinity"]
