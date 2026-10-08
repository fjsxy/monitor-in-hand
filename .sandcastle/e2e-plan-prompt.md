# 任务

为已认领的 GitHub Issue #{{TASK_ID}}（{{ISSUE_TITLE}}）整理实现前 E2E case 评论草稿。

这是实现前的只读规划阶段。读取 Issue 正文、labels、comments，相关父规格、依赖、`AGENTS.md`、`docs/agents/issue-tracker.md`、`docs/agents/development-standards.md`、`docs/testing/phase1-acceptance-matrix.md` 和相关测试源码。仓库存在 `.codegraph/` 时先使用 CodeGraph。

按 `docs/agents/issue-tracker.md` 的「实现前 E2E 评论」规则逐项核对 acceptance criteria，整理新增及复用的相关 E2E case，覆盖适用版本、正向路径、拒绝路径和关键不变量。只为当前 Issue 分配新 ID，复用场景保持已有 ID 和来源。#62、#63、#64 的延期范围继续明确排除。

草稿包含 `## E2E`，其表头、列顺序和 Markdown 表格格式与验收矩阵 E2E 部分完全一致；每个字段填写具体内容，执行步骤按动作顺序排列，执行预期可判定通过或失败。测试入口可以是计划新增的 Ginkgo 入口，须明确尚未实现或执行。确无相关 E2E 时保留空表，并在 `## 无相关 E2E 的依据` 下说明原因及替代验证。

只读取代码并输出草稿，保持工作树与 HEAD 不变；评论由宿主发布，规划阶段不提交或修改任何文件，也不操作 GitHub。不能确定验收目标时报告具体缺口，不输出就绪信号。

完成格式与需求覆盖检查后，在最终输出中给出：

<e2e-plan>
完整中文 Markdown 评论草稿
</e2e-plan>

随后输出 `<promise>E2E_PLAN_READY</promise>`。此信号只表示草稿就绪，宿主发布并回读评论成功后才启动实现。
