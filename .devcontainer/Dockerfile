FROM golang:1.26-bookworm

ARG USERNAME=dev
ARG USER_UID=1000
ARG USER_GID=1000

# 基本工具 + sudo，讓非 root 使用者可以 apt update / apt install
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        sudo git curl ca-certificates make bash-completion less vim \
    && rm -rf /var/lib/apt/lists/*

# 建立 UID/GID 1000 的使用者，並給予免密碼 sudo
RUN groupadd --gid ${USER_GID} ${USERNAME} \
    && useradd --uid ${USER_UID} --gid ${USER_GID} -m -s /bin/bash ${USERNAME} \
    && echo "${USERNAME} ALL=(root) NOPASSWD:ALL" > /etc/sudoers.d/${USERNAME} \
    && chmod 0440 /etc/sudoers.d/${USERNAME}

# GOPATH 交給該使用者，go install 才不需要 root
ENV GOPATH=/home/${USERNAME}/go
ENV PATH=${GOPATH}/bin:/usr/local/go/bin:${PATH}
RUN mkdir -p ${GOPATH} && chown -R ${USER_UID}:${USER_GID} ${GOPATH}

USER ${USERNAME}

# 常用 Go 開發工具（VS Code Go extension 需要 gopls / dlv）
RUN go install golang.org/x/tools/gopls@latest \
    && go install github.com/go-delve/delve/cmd/dlv@latest \
    && go install honnef.co/go/tools/cmd/staticcheck@latest \
    && go install github.com/cweill/gotests/gotests@latest \
    && go install github.com/fatih/gomodifytags@latest \
    && go install github.com/josharian/impl@latest \
    && go clean -cache -modcache

# Claude Code（官方 native installer，裝在 ~/.local/bin）
# 設定/登入資訊放在 CLAUDE_CONFIG_DIR，並由 devcontainer.json 掛 volume 保存，rebuild 後不用重新登入
ENV CLAUDE_CONFIG_DIR=/home/${USERNAME}/.claude
ENV PATH=/home/${USERNAME}/.local/bin:${PATH}
RUN mkdir -p ${CLAUDE_CONFIG_DIR} \
    && curl -fsSL https://claude.ai/install.sh | bash

WORKDIR /workspace
