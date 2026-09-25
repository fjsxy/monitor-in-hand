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
- **Claim**：运行 `gh issue edit <n> --add-assignee @me`。这是 session 的第一次写操作。
- **Resolve**：添加结论评论、关闭 child，然后在 map 的 Decisions-so-far 中追加上下文链接。
