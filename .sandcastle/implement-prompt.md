# 任务

实现 GitHub Issue #{{TASK_ID}}：{{ISSUE_TITLE}}。

先运行 `gh issue view {{TASK_ID}} --comments`，完整读取正文、labels 和 comments。只处理这个 Issue，不顺带实现其他 ticket。

当前分支是 `{{BRANCH}}`。

# 项目规则

1. 完整阅读根目录 `AGENTS.md`。
2. 完整阅读 `.agents/skills/implement/SKILL.md` 并遵循其流程。
3. 按需使用 `.agents/skills/tdd/SKILL.md`；从可观察行为和根因出发，不写临时补丁。
4. 如果仓库存在 `.codegraph/`，理解或定位代码时先使用 CodeGraph。
5. 保留现有兼容接口，除非 Issue 明确要求破坏性变更。
6. 阅读长文件时逐个、分段读取；搜索先限定目录和模式，再按需展开。不要在一次命令中串联多个长文件或回传大范围无界输出。

# 实现与验证

- 先理解 acceptance criteria、相关 ADR/`CONTEXT.md` 和当前实现。
- 一次完成一个可验证的 red-green slice。
- 新增稳定测试场景时使用包含 `GH{{TASK_ID}}` 的 Issue-scoped ID；不得扫描现有最大编号后分配全局递增 ID，也不得重命名已有 ID。完整格式遵循 `docs/agents/development-standards.md`。
- 经常运行聚焦测试；提交前至少运行 `make typecheck` 和 `make test HELM=helm`。
- sandbox 不挂载宿主 Docker socket。需要 kind/Docker 的验收必须落实为 PR 中的 blocking CI job；本地明确记录未运行，不能伪报通过。
- 不得通过删除、skip、放宽断言或吞掉错误让测试变绿。
- 不修改 Issue assignee/labels，不关闭 Issue，不 push，不创建或合并 PR；这些由宿主调度器负责。

# 交付

- 将完整实现和测试提交到当前分支。
- commit message 简洁说明行为变化并引用 `#{{TASK_ID}}`。
- 如果不能完成，在最终输出中说明已验证事实、剩余 blocker 和当前分支状态，不要写入 Issue，也不要输出完成信号。
- 只有实现已达到可提 PR、所有本地可执行验证通过，并且任何 CI-only 验收都已接入 blocking CI 时，输出 `<promise>COMPLETE</promise>`。
