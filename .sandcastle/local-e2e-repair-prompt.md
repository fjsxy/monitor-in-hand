# 本地 E2E 修复：Issue #{{TASK_ID}} — {{ISSUE_TITLE}}

当前分支 `{{BRANCH}}` 已完成实现和 code review，但宿主机 E2E gate 第 {{REPAIR_ATTEMPT}} 次运行失败。

## 证据

- test run：`{{TEST_RUN_ID}}`
- 完整日志：`{{LOCAL_E2E_LOG_PATH}}`
- 失败 evidence：`artifacts/test-env/{{TEST_RUN_ID}}-1.28/`
- E2E 报告：`artifacts/e2e/{{TEST_RUN_ID}}-1.28/`

## 要求

1. 先读失败日志、JUnit/report 和 evidence，定位根因；不要猜测。
2. 只修复 Issue #{{TASK_ID}} 范围内、会导致同一本地 E2E 失败的问题。
3. 通过公开接口补充或修正测试，遵守 `docs/agents/development-standards.md`。
4. sandbox 内不提供 Docker daemon；使用已有 evidence 建立不依赖 Docker 的最小反馈回路，运行聚焦测试以及 `make typecheck`，然后提交修复。
5. 不 push、不创建或修改 PR/Issue；调度器会重新 code review，并在宿主机重跑 E2E gate。

完成后输出 `<promise>COMPLETE</promise>`；证据不足或无法安全修复时不要输出完成信号。
