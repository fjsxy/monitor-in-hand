# 任务

解决分支 `{{BRANCH}}`（GitHub Issue #{{TASK_ID}}：{{ISSUE_TITLE}}）rebase 到 `{{TARGET_BRANCH}}` 时的冲突。

当前 worktree 已处于正在进行的 rebase 中。完整阅读：

- 根目录 `AGENTS.md`
- `.agents/skills/resolving-merge-conflicts/SKILL.md`
- `gh issue view {{TASK_ID}} --comments`
- 冲突两侧代码、测试和相关领域文档

# 约束

- 阅读长文件时逐个、分段读取；搜索先限定目录和模式，再按需展开。不要在一次命令中串联多个长文件或回传大范围无界输出。
- 追溯双方意图并保留仍然成立的行为，不机械选择 ours/theirs，不通过删除测试、放宽断言或恢复旧实现消除冲突。
- 不执行 `git rebase --abort`，不重新开始 rebase，不 merge `{{TARGET_BRANCH}}`。
- 逐个解决冲突并 `git add`；使用 `GIT_EDITOR=true git rebase --continue` 推进，直到 rebase 完成。
- rebase 完成后运行受影响的聚焦测试，并至少运行 `make typecheck` 与 `make test HELM=helm`。
- 不 push、不合并 PR、不修改或关闭 Issue；宿主 merge 流程负责这些写操作。

只有 rebase 已完成、worktree 干净、所有冲突已解决且本地可执行验证通过时，输出 `<promise>COMPLETE</promise>`。
