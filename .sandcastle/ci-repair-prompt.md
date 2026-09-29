# 任务

修复 PR #{{PR_NUMBER}}（GitHub Issue #{{TASK_ID}}：{{ISSUE_TITLE}}）在 head `{{FAILED_HEAD_OID}}` 上的 CI 失败。

这是第 {{REPAIR_ATTEMPT}} 次自动修复。失败 checks：{{FAILED_CHECKS}}。

当前分支是 `{{BRANCH}}`。

# 项目规则

1. 完整阅读根目录 `AGENTS.md`、`docs/agents/development-standards.md` 和 `.agents/skills/diagnosing-bugs/SKILL.md`。
2. 如果仓库存在 `.codegraph/`，理解或定位代码时先使用 CodeGraph。
3. 只修复这次 CI 暴露的根因，不扩大 Issue 范围，不通过删除、skip、放宽断言或吞掉错误让测试变绿。
4. 不修改 Issue、PR、assignee 或 labels，不 push，不 merge；宿主调度器负责发布和后续 CI。

# 诊断与修复

1. 先确认 `git rev-parse HEAD` 等于 `{{FAILED_HEAD_OID}}`，否则停止，避免覆盖并发更新。
2. 使用 `gh pr view {{PR_NUMBER}}`、`gh pr checks {{PR_NUMBER}}` 和 `gh run view <run-id> --log-failed` 获取失败证据；需要时用 `gh run download <run-id> --dir /tmp/<isolated-dir>` 把 artifacts 下载到仓库外的临时目录。
3. 建立能捕获同一失败的最小反馈回路，追到根因；若 sandbox 无法运行 kind/Docker，使用 CI logs、JUnit、evidence artifacts 和不依赖 Docker 的最小测试复现，不伪报 E2E 通过。
4. 按 red → green 修复并运行聚焦测试；提交前至少运行 `make typecheck` 和 `make test HELM=helm`。
5. 将修复和回归测试提交到当前分支，commit message 说明根因并引用 `#{{TASK_ID}}`。

# 完成条件

- 失败证据已被解释，修改针对根因，并有可执行回归验证。
- 工作区干净，至少产生一个新的 commit。
- 不包含临时下载的 CI artifacts、凭据或调试残留。

只有满足全部条件时输出 `<promise>COMPLETE</promise>`。如果 CI 是外部基础设施故障、无法形成安全修复或不能产生 commit，说明 blocker，且不要输出完成信号。
