# Sandcastle 接入说明

本目录把 GitHub `ready-for-agent` tickets 调度到独立的 Codex sandbox/worktree。它只处理 `/to-tickets` 生成的 ticket 结构，不把父规格 Issue 当作实现任务。

## 行为边界

- 从 Issue 的 `## Blocked by` 章节确定性计算 frontier，不调用模型重新猜依赖。
- 只选择无人认领或已经由当前 GitHub 用户认领的 ticket。
- 启动 agent 前串行执行 `gh issue edit <n> --add-assignee @me` 并回读验证。
- 每个 ticket 使用 `codex/issue-<n>` 独立分支和 Sandcastle worktree。
- 默认最多并行 3 个 ticket，每个 implementer 最多运行 3 次。
- implementer 和 reviewer 只在隔离 sandbox 中运行；sandbox 不挂载宿主 Docker socket，也不运行 DinD。
- 并行 implementer 和 reviewer 完成后，宿主机在对应 worktree 中进入串行 Kubernetes 1.28 E2E gate；通过后才 push 分支并创建 PR，随后按 Issue 编号进入串行 merge 队列。
- 本地 E2E gate 依次执行 `test-env-up`、`test-env-verify` 和 `e2e-kubestatemetrics`。失败时先保存 evidence，再由 sandbox 中的 repair agent 修复、重新 code review，并回到宿主机重跑；修复预算耗尽则不创建 PR。
- merge 队列等待 PR 的全部 CI checks 成功；CI 失败时基于失败 head 启动 repair agent，读取失败 logs/artifacts、提交并带 lease 推送，然后等待新一轮 CI，达到重试上限才阻塞队列。
- CI 通过后使用最新 `main` rebase 分支；如有冲突，启动 resolver 并重新 code-review，推送后再次等待 CI。
- 最终执行带 head commit 锁的 `rebase merge` 并回读 `MERGED` 状态；任一 PR 阻塞时停止队列，后续 PR 保持 open。
- 再次运行时会恢复带 Sandcastle 标记、分支和关闭 Issue 引用一致的 open PR，继续修复、等待或合并，不重复实现 ticket。
- 完成信号表示“实现已达到可提 PR”，不是 Issue 已完成；Issue 仅在 PR 通过 CI 并成功合并后关闭。
- 如果 agent 或 reviewer 未发出完成信号，保留分支，不创建 PR，命令以失败退出。

## 首次设置

先确认 Codex CLI 已使用 ChatGPT 订阅登录，并存在文件凭据缓存：

```bash
codex login status
test -f ~/.codex/auth.json
```

调度器不会要求 `OPENAI_API_KEY`。每个 sandbox 启动前，它会把 `~/.codex/auth.json` 复制到独立临时目录，只向该容器挂载临时副本，并在容器结束后删除；不会挂载整个 `~/.codex`，也不会把凭据写入仓库或镜像。

`auth.json` 等同密码。sandbox 内的 `agent` 用户为了运行 Codex 必须能读取该临时副本，因此只应对受信任的仓库与 ticket 运行本调度器。临时副本可写，以便 Codex 在长任务中刷新 token，但刷新不会改动宿主原始文件。

如果凭据文件不在默认位置，可复制 `.sandcastle/.env.example` 为 `.sandcastle/.env` 并设置 `CODEX_AUTH_FILE`。该文件已被忽略。`GH_TOKEN` 可以留空，调度器会读取当前 `gh auth` 登录。

安装依赖并构建固定版本的 sandbox image：

```bash
npm install
npm run sandcastle:check
npm run sandcastle:build
npm run sandcastle:smoke
```

`sandcastle:smoke` 不调用模型，但会访问一个只读 Issue 以验证 `gh` 凭据。它使用临时本地分支验证 ChatGPT 登录缓存、worktree 和 sandbox，并在宿主机验证 Kubernetes 1.28 test environment 与真实 `e2e-kubestatemetrics`；结束时清理 cluster、sandbox 和临时凭据。

## 运行一轮 frontier

确保当前位于干净且已同步远端的 `main` 分支，然后执行：

```bash
npm run sandcastle
```

调度器会拒绝非 `main` 分支、dirty worktree、detached HEAD 和非 `fjsxy/monitor-in-hand` remote。每次运行处理当前一轮 frontier，等待并合并本轮 PR，然后把本地 `main` fast-forward 到远端；blocker 关闭后再运行下一轮。

## 可选参数

可在 `.sandcastle/.env` 或进程环境中覆盖：

- `SANDCASTLE_MODEL`：默认 `gpt-5.6-sol`；reasoning effort 固定为 `high`。
- `SANDCASTLE_MAX_PARALLEL`：默认 `3`。
- `SANDCASTLE_MAX_ITERATIONS`：默认 `3`。
- `SANDCASTLE_MAX_CI_REPAIR_ATTEMPTS`：单个 PR 的 CI 失败自动修复次数，默认 `2`；设为 `0` 可关闭自动修复。
- `SANDCASTLE_MAX_LOCAL_E2E_REPAIR_ATTEMPTS`：PR 创建前本地 E2E 失败的自动修复次数，默认 `2`；设为 `0` 时首次失败即阻止发布。
- `SANDCASTLE_IDLE_TIMEOUT_SECONDS`：agent 连续没有 stdout 时的终止门限，默认 `1800` 秒。Codex 在处理较大 tool output 或执行 high reasoning 时可能长时间不输出；该门限不代表单条 shell 命令的 timeout。
- `SANDCASTLE_CI_TIMEOUT_MINUTES`：单次等待某个 PR CI 的最长时间，默认 `60`。
- `SANDCASTLE_CI_POLL_SECONDS`：CI 状态轮询间隔，默认 `15`。

sandbox 与宿主机职责由执行位置明确分开：agent 只能在 sandbox 中改代码和 code review；Docker、kind 和 Kubernetes E2E 只由宿主调度器在 Sandcastle worktree 的宿主路径执行。宿主机必须可用 `docker`、`kind`、`kubectl` 和 `helm`，且 Docker daemon 已启动。

本地 gate 只提前运行 Kubernetes 1.28 闭环，用于减少确定性失败进入 PR；GitHub CI 继续保留 Kubernetes 1.19.16 与 1.28.15 双版本权威 gate。所有本地 gate 串行执行，并通过 trap 在成功、失败或进程中断时运行 `test-env-down`；失败 evidence 会复制到宿主仓库的 `.sandcastle/logs/local-e2e-<TEST_RUN_ID>/`，避免随临时 worktree 删除。不得提交 `.sandcastle/logs/`、`artifacts/` 或任何 auth 文件。
