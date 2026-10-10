# Sandcastle 节点通知验收矩阵

来源：[Issue #85](https://github.com/fjsxy/monitor-in-hand/issues/85)。这些场景通过 `npm run sandcastle:check` 自动验证，不改变一期 Operator/Kubernetes 验收范围；不计入 `docs/testing/phase1-acceptance-matrix.md` 的 Go/Kubernetes 场景集合。

全部测试使用受控 GitHub、sandbox、gate 操作及 mock HTTP，不调用真实飞书、不创建或合并真实 PR。首次配置后的群消息送达需另行执行 `npm run sandcastle:notify:test`，并由接收端确认。

| 场景 ID | 来源 | 层级 | 目标版本 | 前置条件 | 验收目的 | 执行步骤 | 执行预期 | 失败证据 | 测试入口 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| `UT-NOTIFY-GH85-001` | #85 | Node 单元 | 宿主 Node.js | 临时日志目录、mock 时钟与 fetch | 验证配置关闭、六类范围、去重与凭据脱敏 | 1. 提供进程及本地配置，执行无 Webhook 上报；<br>2. 上报含 Webhook、路径令牌、Secret、GitHub 凭据的结果；<br>3. 重复及越界上报并读取请求与日志 | 进程配置优先；无 Webhook 不发送；只包含六类节点，同轮同 Issue 同节点只入队一次；消息和日志不包含凭据 | Node assertion、临时 JSONL | `.sandcastle/notifications.test.mts`；`npm run sandcastle:check` |
| `IT-NOTIFY-GH85-001` | #85 | Node 集成 | 宿主 Node.js | fake GitHub、implementer、gate 与 merge operations | 验证六个真实成功边界及顺序 | 1. 完成计划发布和回读；<br>2. 完成实现、gate、PR 发布与回读；<br>3. 执行 CI、rebase、merge 与 MERGED 回读，核对通知 | 恰有六类成功通知；计划在回读后、实现在完成信号和 commit 后、PR 在正确 OPEN/非 draft 回读后、CI 对应最终 head、合并在 MERGED 回读后上报 | Node assertion、事件顺序 | `.sandcastle/notifications.test.mts`；`npm run sandcastle:check` |
| `IT-NOTIFY-GH85-002` | #85 | Node 集成 | 宿主 Node.js | 受控失败操作与通知接收器 | 验证失败不会伪造未执行节点或改变业务结果 | 1. 注入计划回读、实现、gate、PR、CI 和 merge 错误；<br>2. 注入 rebase、通知回调及合并后本地同步错误；<br>3. 检查通知和队列结果 | 仅指定节点报告最终失败；未执行节点不通知；rebase 失败不伪造 CI/merge 结果；通知错误不改变业务结果；远端已 MERGED 后本地同步失败仍保留合并成功通知 | Node assertion、事件与队列状态 | `.sandcastle/notifications.test.mts`；`npm run sandcastle:check` |
| `IT-NOTIFY-GH85-003` | #85 | Node 集成 | 宿主 Node.js | fake 本地 E2E 修复与 CI/rebase operations | 验证修复收敛及最终 CI head | 1. 让本地 E2E 首次失败后修复通过；<br>2. CI 失败后修复，再 rebase 改变 head；<br>3. 分别模拟最终 CI 通过或耗尽预算失败 | 不通知中间修复；本地 E2E 和 CI 各只报告最终结果；CI 成功对应 rebase 后 head，最终失败不提前报告旧 head 成功 | Node assertion、最终 head 与节点记录 | `.sandcastle/notifications.test.mts`；`npm run sandcastle:check` |
| `IT-NOTIFY-GH85-004` | #85 | Node HTTP 集成 | 宿主 Node.js | 本地 HTTP server、mock 时钟、超时信号 | 验证飞书协议、限速、业务码、超时与故障隔离 | 1. 提交并行的六类节点，将官方 URL 转向本地 HTTP server；<br>2. 首次返回限流业务码，后续分别返回旧/新成功响应；<br>3. 模拟请求超时，核对原业务结果 | 使用 text payload、禁止重定向且未配置 Secret 时不带签名；七次请求间隔均至少 1 秒；成功码必须为 0；超时最多尝试三次；通知失败不改变业务成功且日志脱敏 | Node assertion、请求时间与 JSONL | `.sandcastle/notifications.test.mts`；`npm run sandcastle:check` |
| `UT-NOTIFY-GH85-002` | #85 | Node 单元 | 宿主 Node.js | 固定签名向量、mock fetch、临时日志目录 | 验证可选签名、官方地址限制及消息提醒控制 | 1. 固定官方示例 timestamp 和 demo Secret，首次限流后重试；<br>2. 提供错误协议、域名、版本、query 和 fragment；<br>3. 注入 at 标签、超长内容和错误/缺失业务码 | 签名匹配独立 HMAC 向量且每次重试更新时间戳；非法 URL 不调用 fetch；新业务错误不能被旧成功码覆盖；缺失或字符串成功码不接受；payload 小于 20 KB，at 标签不触发提醒，凭据不泄漏 | Node assertion、请求 payload 与 JSONL | `.sandcastle/notifications.test.mts`；`npm run sandcastle:check` |
