FROM golang:1.26-bookworm

ARG USERNAME=dev
ARG USER_UID=1000
ARG USER_GID=1000

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        sudo git curl ca-certificates make bash-completion less vim \
    && rm -rf /var/lib/apt/lists/*

RUN groupadd --gid ${USER_GID} ${USERNAME} \
    && useradd --uid ${USER_UID} --gid ${USER_GID} -m -s /bin/bash ${USERNAME} \
    && echo "${USERNAME} ALL=(root) NOPASSWD:ALL" > /etc/sudoers.d/${USERNAME} \
    && chmod 0440 /etc/sudoers.d/${USERNAME}

ENV GOPATH=/home/${USERNAME}/go
ENV PATH=${GOPATH}/bin:/usr/local/go/bin:${PATH}
RUN mkdir -p ${GOPATH} && chown -R ${USER_UID}:${USER_GID} ${GOPATH}

USER ${USERNAME}

RUN go install golang.org/x/tools/gopls@latest \
    && go install github.com/go-delve/delve/cmd/dlv@latest \
    && go install honnef.co/go/tools/cmd/staticcheck@latest \
    && go install github.com/cweill/gotests/gotests@latest \
    && go install github.com/fatih/gomodifytags@latest \
    && go install github.com/josharian/impl@latest \
    && go clean -cache -modcache

ENV CLAUDE_CONFIG_DIR=/home/${USERNAME}/.claude
ENV PATH=/home/${USERNAME}/.local/bin:${PATH}
RUN mkdir -p ${CLAUDE_CONFIG_DIR} \
    && curl -fsSL https://claude.ai/install.sh | bash

WORKDIR /workspace
