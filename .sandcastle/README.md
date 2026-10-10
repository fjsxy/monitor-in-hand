# Sandcastle 接入说明

本目录把 GitHub `ready-for-agent` tickets 调度到独立的 Codex sandbox/worktree。它只处理 `/to-tickets` 生成的 ticket 结构，不把父规格 Issue 当作实现任务。

## 行为边界

- 从 Issue 的 `## Blocked by` 章节确定性计算 frontier，不调用模型重新猜依赖。
- 只选择无人认领或已经由当前 GitHub 用户认领的 ticket。
- 启动 agent 前串行执行 `gh issue edit <n> --add-assignee @me` 并回读验证。
- 领取后先运行只读 E2E planner，按验收矩阵格式整理相关 case；宿主校验草稿、发布 Issue 评论并回读一致后才启动 implementer。规划修改工作树、格式不完整、发布失败或回读失败均停止实现。当前 Issue 正文未变时复用已发布的有效计划，需求变化后重新整理。
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

### 飞书节点通知

链路为 `Sandcastle → 飞书群自定义机器人 Webhook → 群消息`。直接调用飞书接口，不经过第三方推送平台，不需要购买推送会员、注册开发者应用或为推送另行办理实名认证。消息发送到机器人所在的群。

按[飞书官方指南](https://open.feishu.cn/document/client-docs/bot-v3/add-custom-bot)完成一次配置：

1. 在飞书中准备一个用于接收通知的群，并开启该群的消息提醒。
2. 使用 **Mac/Windows 飞书桌面客户端**打开该群，进入群设置 → 群机器人 → 添加机器人 → 自定义机器人，名称填写 `Sandcastle`，复制生成的 Webhook 地址。手机 App 只能添加应用机器人，不能创建自定义机器人；机器人创建后，手机可正常接收群通知。客户端支持范围见[飞书帮助中心](https://www.feishu.cn/hc/zh-CN/articles/360024984973-在群组中使用机器人)。
3. 推荐在机器人安全设置中开启签名校验，复制签名密钥并保存；若使用关键词校验，关键词设置为 `Sandcastle`（每条通知都包含）。
4. 将以下配置加入被 Git 忽略的 `.sandcastle/.env`。已有文件只追加或修改这两项，保留其余设置；若文件不存在，可先复制 `.sandcastle/.env.example`。

```dotenv
SANDCASTLE_FEISHU_WEBHOOK=https://open.feishu.cn/open-apis/bot/v2/hook/你的机器人令牌
# 仅当机器人开启签名校验时填写，否则留空。
SANDCASTLE_FEISHU_SECRET=你的签名密钥
```

也可通过进程环境设置同名变量，进程环境优先。未配置 Webhook 时不发送通知，也不生成通知日志。仅允许官方 HTTPS v2 hook 地址。Webhook 和签名密钥仅由宿主调度器使用，不传入 sandbox，不打印或提交到 Git。此前的 `SANDCASTLE_PUSHPLUS_TOKEN` 不再使用。

自动通知只覆盖以下六类节点的最终成功或失败：

自动化验证范围见[节点通知验收矩阵](acceptance-matrix.md)。

| 节点 | 成功判定 |
| --- | --- |
| E2E 计划发布 | 评论发布或复用后，正文、URL 和场景格式已完整回读验证 |
| 实现完成 | implementer 发出完成信号且产生 commit |
| 本地 E2E | 现有串行 gate 及有界修复流程最终通过 |
| PR 创建完成 | push/create 或复用后，远端 PR 回读为正确分支、目标分支且 OPEN、非 draft |
| PR CI | 修复和 rebase 结束后，待合并 head 的全部 CI checks 通过 |
| 完成合并 | rebase merge 后，远端 PR 回读为 MERGED |

领取、评审、repair、rebase、本地 main 同步与整轮汇总不单独通知，未执行的节点也不发送结果。中间的 CI/本地 E2E 失败由既有有界修复流程处理，只通知最终结果。若 rebase 本身失败，流程停止，不虚构最终 CI 或合并结果。实现完成消息只表示实现节点结束；合并消息依据 PR 状态，不推测 Issue 是否关闭。

每条消息包含 Issue 编号、节点、成功/失败、时间、耗时，以及可用的结果摘要、评论/PR 链接和 head。同一轮、同一 Issue、同一节点只入队一次；恢复已有 PR 时仅通知本轮实际执行的 CI/合并节点。

并行 Issue 共用一个发送队列，所有请求（包括重试）间隔至少 1 秒。单次请求超时 10 秒，最多尝试 3 次；推送失败只记录日志，不改变任务、E2E gate 或合并队列的结果。退出前等待当前通知队列结束。发送记录位于 `.sandcastle/logs/notifications-<runId>.jsonl`，其中 `queued` 表示排队，`accepted` 表示飞书接受请求，`retry`/`failed` 表示发送故障；异常终止留下的记录不会自动重放。

飞书的 `code=0`（兼容旧响应 `StatusCode=0`）仅表示接受请求，不能证明手机收到。首次配置后单独运行：

```bash
npm run sandcastle:notify:test
```

该命令只发送一条明确标记为测试的消息，请在机器人所在的飞书群确认收到。任务通知内容只发送脱敏后的短摘要和链接，不附原始 agent stdout，也不把结果中的 at 标签解释成群提醒。HTTP 响应不确定时重试可能造成重复消息，入队去重不代表飞书端严格一次送达。

根据[飞书官方限制](https://open.feishu.cn/document/client-docs/bot-v3/add-custom-bot)，单租户单自定义机器人最多 100 次/分钟、5 次/秒，请求体不能超过 20 KB；发送摘要的截断和队列限速按此设置。不要让其他脚本共用该机器人大量发送消息。

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
