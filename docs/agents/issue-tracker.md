# Issue tracker：GitHub

本项目的 issues 和 specs 存放在 GitHub 仓库 `fjsxy/monitor-in-hand`，所有操作使用 `gh` CLI。

执行写操作前，通过 `git remote -v` 确认当前仓库指向：

`https://github.com/fjsxy/monitor-in-hand`

如果 remote 缺失或指向其他仓库，停止操作，不得猜测目标仓库。

## Conventions

- 创建 Issue：`gh issue create --title "..." --body "..."`
- 查看 Issue：`gh issue view <number> --comments`
- 列出 Issues：`gh issue list --state open --json number,title,body,labels,comments`
- 评论：`gh issue comment <number> --body "..."`
- 添加标签：`gh issue edit <number> --add-label "..."`
- 移除标签：`gh issue edit <number> --remove-label "..."`
- 关闭：`gh issue close <number> --comment "..."`

多行 body 使用 heredoc。读取 Issue 时，同时获取 labels 和 comments，并按任务需要使用 `jq` 过滤。

## 实现前 E2E 评论

每个待实现 Issue/ticket 都执行「领取并回读 → 整理 E2E case → 发布评论并回读 → 实现」；直接手工实现与 Sandcastle 自动实现遵守同一前置条件。

可以提前整理并发布计划；正式领取后仍须回读，确认它覆盖当前需求后再复用。

1. 完整读取 Issue 正文、labels、comments、父规格与依赖，确认本期范围和领取状态。明确延期的 Issue 留在待办，不进入实现；#62、#63、#64 的延期决定继续生效。
2. 按 acceptance criteria 整理新增及复用的相关 E2E case。覆盖适用版本、正向与拒绝路径、关键不变量；注明每项验收由哪些 E2E 或其他测试层验证。复用场景保持已有 ID 和来源，新增场景使用当前 Issue 的 Issue-scoped ID。目标或验收边界不清楚时先讨论，确定后再继续。
3. 评论使用 `docs/testing/phase1-acceptance-matrix.md` 的 E2E 表头、列顺序与表格格式，填写全部字段；验收目的、执行步骤、执行预期分别独立填写。拟新增的测试入口明确注明尚未实现或执行。确无相关 E2E 时保留空表，并在 `## 无相关 E2E 的依据` 下解释原因与替代验证，不虚构场景。
4. 通过 `gh issue comment <number> --body-file <path>` 发布。回读确认评论 URL、正文、完整表格与当前需求一致后，才允许修改业务代码或测试；发布或回读失败时停在该步骤。
5. 重试或恢复实现时先检查已有计划；当前需求未变且评论仍满足格式时复用，需求改变后发布更新的计划。实现中发现验收范围需要变化时先更新评论。实际交付后，把已自动化场景同步到验收矩阵，保留相同 ID、目的、步骤与预期。

评论格式：

```markdown
## E2E

说明当前 Issue 的验收覆盖及明确排除范围；这些是实现前计划，不表示已经执行通过。

| 场景 ID | 来源 | 层级 | 目标版本 | 验收目的 | 执行步骤 | 执行预期 | 失败证据 | 测试入口 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
```

Sandcastle 由只读 planner 输出草稿，宿主校验格式、发布评论并回读成功后才启动 implementer。评论带当前 Issue 正文摘要，用于避免重试时重复发布及复用过期计划；规划阶段必须保持工作树与 HEAD 不变。

## Pull requests as a triage surface

**PRs as a request surface: no.**

GitHub Issues 和 Pull Requests 共用编号空间。遇到 `#42` 这类引用时，先运行 `gh pr view 42`，失败后再运行 `gh issue view 42`。

## Skill 语义

当 skill 要求“publish to the issue tracker”时，创建 GitHub Issue。

当 skill 要求“fetch the relevant ticket”时，运行：

`gh issue view <number> --comments`

## Wayfinding operations

`/wayfinder` 使用一个 map Issue 和多个 child Issues：

- **Map**：添加 `wayfinder:map` 标签，正文维护 Notes、Decisions-so-far 和 Fog。
- **Child ticket**：作为 GitHub sub-issue 关联到 map；不支持 sub-issue 时，使用 task list，并在 child 顶部写 `Part of #<map>`。
- **类型标签**：使用 `wayfinder:research`、`wayfinder:prototype`、`wayfinder:grilling` 或 `wayfinder:task`。
- **Blocking**：优先使用 GitHub native issue dependencies；不可用时，在 child 顶部写 `Blocked by: #<n>, #<n>`。
- **Frontier**：从 map 的未关闭 children 中排除存在未关闭 blocker 或已有 assignee 的 Issue，按 map 顺序选择第一个。
- **Claim**：运行 `gh issue edit <n> --add-assignee @me` 并回读。领取成功后按「实现前 E2E 评论」发布并验证计划，再开始实现。
- **Resolve**：添加结论评论、关闭 child，然后在 map 的 Decisions-so-far 中追加上下文链接。
