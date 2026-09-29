import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import {
  branchForIssue,
  parseBlockerNumbers,
  promptArgsForIssue,
  selectFrontier,
  type IssueRecord,
} from "./orchestration.mts";

function issue(
  number: number,
  options: Partial<IssueRecord> = {},
): IssueRecord {
  return {
    number,
    title: `Ticket ${number}`,
    body: [
      "## What to build",
      "交付一个可验证行为。",
      "## Acceptance criteria",
      "- [ ] 行为可验证",
      "## Blocked by",
      "None (can start immediately).",
    ].join("\n\n"),
    state: "OPEN",
    labels: [{ name: "ready-for-agent" }],
    assignees: [],
    ...options,
  };
}

test("只解析 Blocked by 章节中的 issue 引用", () => {
  const body = [
    "## Parent",
    "#48",
    "## What to build",
    "交付行为。",
    "## Acceptance criteria",
    "- [ ] 完成 #999 的兼容检查",
    "## Blocked by",
    "- #51 基础能力",
    "- #52 测试环境",
    "- #51 重复引用",
    "## Notes",
    "另见 #77",
  ].join("\n\n");

  assert.deepEqual(parseBlockerNumbers(body), [51, 52]);
});

test("frontier 只包含 blocker 已关闭且无人或由当前用户认领的 ticket", () => {
  const issues = [
    issue(1),
    issue(2, {
      body: issue(2).body.replace(
        "None (can start immediately).",
        "- #1 基础能力",
      ),
    }),
    issue(3, { assignees: [{ login: "fjsxy" }] }),
    issue(4, { assignees: [{ login: "someone-else" }] }),
    issue(5, { labels: [] }),
  ];

  assert.deepEqual(
    selectFrontier(issues, "fjsxy", 3).map((item) => item.number),
    [1, 3],
  );

  issues[0]!.state = "CLOSED";
  assert.deepEqual(
    selectFrontier(issues, "fjsxy", 3).map((item) => item.number),
    [2, 3],
  );
});

test("父规格 issue 不会被误当成 agent ticket", () => {
  const parent = issue(48, {
    body: "## User Stories\n\n1. 作为管理员，我希望……",
  });
  assert.deepEqual(selectFrontier([parent], "fjsxy", 3), []);
});

test("分支名稳定且使用项目要求的 codex 前缀", () => {
  assert.equal(branchForIssue(51), "codex/issue-51");
});

test("prompt 模板中的变量都有调度参数", () => {
  const args = promptArgsForIssue(issue(51), "codex/issue-51");
  const builtInArgs = new Set(["SOURCE_BRANCH", "TARGET_BRANCH"]);

  for (const path of [
    ".sandcastle/implement-prompt.md",
    ".sandcastle/review-prompt.md",
    ".sandcastle/merge-conflict-prompt.md",
  ]) {
    const template = readFileSync(path, "utf8");
    const variables = [
      ...new Set(
        [...template.matchAll(/\{\{([A-Z0-9_]+)\}\}/g)].map(
          (match) => match[1]!,
        ),
      ),
    ];

    assert.deepEqual(
      variables.filter(
        (variable) => !builtInArgs.has(variable) && !(variable in args),
      ),
      [],
      `${path} 存在未提供的模板变量`,
    );
  }
});

test("promptArgs 不覆盖 Sandcastle 内置的 TARGET_BRANCH", () => {
  const args = promptArgsForIssue(issue(51), "codex/issue-51");
  assert.equal("TARGET_BRANCH" in args, false);
});
