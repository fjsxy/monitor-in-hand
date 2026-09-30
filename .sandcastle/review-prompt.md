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

阅读长文件时逐个、分段读取；搜索先限定目录和模式，再按需展开。不要在一次命令中串联多个长文件或回传大范围无界输出。

发现问题时直接在当前分支修复、运行聚焦测试，并至少运行 `make typecheck` 与 `make test HELM=helm`，然后提交修复。sandbox 不提供 Docker daemon，也不挂载宿主 Docker socket；调度器会在 review 完成后由宿主机运行 Kubernetes 1.28 E2E gate。必须明确记录未运行的 Docker/kind 验证，不能伪报通过。不要改变需求范围，不要修改或关闭 Issue，不要 push/创建 PR。

只有实现已达到可提 PR、本地可执行验证通过、CI-only 验收已接入 blocking CI，且没有未解决的阻断问题时，输出 `<promise>COMPLETE</promise>`。
