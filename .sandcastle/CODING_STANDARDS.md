# Coding Standards

## 原则

- 从 acceptance criteria 和领域不变量出发，修复根因，不做症状补丁。
- 保持变更局部、可解释；避免为未来可能性引入通用框架。
- 外部依赖、生成物和用户可观察行为必须有明确验证证据。

## Go

- 使用 `gofmt`，遵循现有 package 边界和命名。
- 错误必须保留可定位上下文，不静默吞掉。
- 固定行为优先放在纯逻辑 seam 中，再由 Kubernetes I/O 层调用。
- 保留已经建立的 CLI/API 兼容性；不兼容输入应显式拒绝。

## 测试

- 测试管理员可观察契约，不锁定私有实现细节。
- 新行为使用聚焦测试覆盖，bug fix 必须包含 regression test。
- 新增稳定场景 ID 使用 `docs/agents/development-standards.md` 规定的 Issue-scoped 格式；已有 ID 不重命名，不再分配全局递增编号。
- 不用固定长 sleep；异步行为使用 bounded eventual assertion。
- 完成前运行 `make typecheck` 和 `make test HELM=helm`。

## GitHub 生命周期

- 本地实现完成不等于 Issue 完成。
- implementer、reviewer 和 conflict resolver 不自行 push、merge 或关闭 Issue。
- merge 流程只合并 code-review 已完成且全部 CI checks 成功的 head，并固定使用 rebase merge；冲突解决产生新 head 后必须重新 review 和等待 CI。
- 由 PR 合并和远端 CI/验收状态决定 Issue 完成。
