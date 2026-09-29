# 任务

复核分支 `{{BRANCH}}` 对 GitHub Issue #{{TASK_ID}}（{{ISSUE_TITLE}}）的实现。

完整读取：

- `gh issue view {{TASK_ID}} --comments`
- 根目录 `AGENTS.md`
- `.agents/skills/code-review/SKILL.md`
- `.sandcastle/CODING_STANDARDS.md`

检查 `git diff {{TARGET_BRANCH}}...{{BRANCH}}` 和该分支新增 commits，分别回答：

1. **Spec**：每项 acceptance criterion 是否有实现与验证证据。
2. **Standards**：是否符合仓库规则、领域决策和代码质量要求。

特别检查新增稳定测试场景是否使用包含 `GH{{TASK_ID}}` 的 Issue-scoped ID，且没有重命名已有 ID 或继续分配全局递增编号。

发现问题时直接在当前分支修复、运行聚焦测试，并至少运行 `make typecheck` 与 `make test HELM=helm`，然后提交修复。sandbox 不挂载宿主 Docker socket；需要 kind/Docker 的验收必须检查其是否被接入 PR 的 blocking CI，不能伪报本地通过。不要改变需求范围，不要修改或关闭 Issue，不要 push/创建 PR。

只有实现已达到可提 PR、本地可执行验证通过、CI-only 验收已接入 blocking CI，且没有未解决的阻断问题时，输出 `<promise>COMPLETE</promise>`。
