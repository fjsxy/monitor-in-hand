import assert from "node:assert/strict";
import test from "node:test";
import {
  classifyCI,
  mergeReviewedPullRequests,
  selectResumablePullRequests,
  type MergeOperations,
  type ReviewedPullRequest,
} from "./merge.mts";

function pullRequest(issue: number): ReviewedPullRequest {
  return {
    issue,
    number: issue + 100,
    branch: `codex/issue-${issue}`,
    url: `https://github.com/fjsxy/monitor-in-hand/pull/${issue + 100}`,
  };
}

test("按 Issue 编号串行等待 CI、更新基线并使用已检查的 head rebase merge", async () => {
  const events: string[] = [];
  const waits = new Map<number, number>();
  const operations: MergeOperations = {
    async waitForCI(pr) {
      const count = (waits.get(pr.number) ?? 0) + 1;
      waits.set(pr.number, count);
      events.push(`wait:${pr.issue}:${count}`);
      return `${pr.issue}-${count}`;
    },
    async rebaseOntoLatestBase(pr, checkedHeadOid) {
      events.push(`rebase:${pr.issue}:${checkedHeadOid}`);
      return pr.issue === 53 ? `${pr.issue}-2` : checkedHeadOid;
    },
    async mergeWithRebase(pr, checkedHeadOid) {
      events.push(`merge:${pr.issue}:${checkedHeadOid}`);
    },
    async verifyMerged(pr) {
      events.push(`verify:${pr.issue}`);
    },
  };

  const results = await mergeReviewedPullRequests(
    [pullRequest(56), pullRequest(53)],
    operations,
  );

  assert.deepEqual(results, [
    { issue: 53, pr: 153, status: "merged" },
    { issue: 56, pr: 156, status: "merged" },
  ]);
  assert.deepEqual(events, [
    "wait:53:1",
    "rebase:53:53-1",
    "wait:53:2",
    "merge:53:53-2",
    "verify:53",
    "wait:56:1",
    "rebase:56:56-1",
    "merge:56:56-1",
    "verify:56",
  ]);
});

test("任一 PR 阻塞时停止队列并保留后续 PR", async () => {
  const events: string[] = [];
  const operations: MergeOperations = {
    async waitForCI(pr) {
      events.push(`wait:${pr.issue}`);
      if (pr.issue === 53) throw new Error("CI failed");
      return `${pr.issue}-head`;
    },
    async rebaseOntoLatestBase() {
      throw new Error("不应执行");
    },
    async mergeWithRebase() {
      throw new Error("不应执行");
    },
    async verifyMerged() {
      throw new Error("不应执行");
    },
  };

  const results = await mergeReviewedPullRequests(
    [pullRequest(56), pullRequest(53)],
    operations,
  );

  assert.deepEqual(results, [
    { issue: 53, pr: 153, status: "blocked", detail: "CI failed" },
    {
      issue: 56,
      pr: 156,
      status: "not-attempted",
      detail: "前序 PR 未能合并",
    },
  ]);
  assert.deepEqual(events, ["wait:53"]);
});

test("CI 只有在存在检查且全部成功结束时才通过", () => {
  assert.equal(classifyCI([]), "pending");
  assert.equal(
    classifyCI([
      { status: "COMPLETED", conclusion: "SUCCESS" },
      { status: "COMPLETED", conclusion: "SKIPPED" },
    ]),
    "passed",
  );
  assert.equal(
    classifyCI([
      { status: "COMPLETED", conclusion: "SUCCESS" },
      { status: "IN_PROGRESS", conclusion: null },
    ]),
    "pending",
  );
  assert.equal(
    classifyCI([{ status: "COMPLETED", conclusion: "FAILURE" }]),
    "failed",
  );
  assert.equal(classifyCI([{ state: "ERROR" }]), "failed");
});

test("rebase 后 head 被再次替换时拒绝合并", async () => {
  let waitCount = 0;
  const operations: MergeOperations = {
    async waitForCI() {
      waitCount += 1;
      return waitCount === 1 ? "reviewed-head" : "unexpected-head";
    },
    async rebaseOntoLatestBase() {
      return "rebased-head";
    },
    async mergeWithRebase() {
      throw new Error("不应合并未经当前流程 rebase 的 head");
    },
    async verifyMerged() {
      throw new Error("不应执行");
    },
  };

  assert.deepEqual(
    await mergeReviewedPullRequests([pullRequest(53)], operations),
    [
      {
        issue: 53,
        pr: 153,
        status: "blocked",
        detail: "PR #153 在 rebase 后 head 已变化。",
      },
    ],
  );
});

test("只恢复带 Sandcastle 标记且 Issue 与关闭引用一致的 open PR", () => {
  assert.deepEqual(
    selectResumablePullRequests(
      [
        {
          number: 153,
          url: "https://github.com/fjsxy/monitor-in-hand/pull/153",
          headRefName: "codex/issue-53",
          baseRefName: "main",
          body: "由 Sandcastle 在隔离 worktree/sandbox 中实现并复核。",
          isDraft: false,
          closingIssuesReferences: [{ number: 53 }],
        },
        {
          number: 156,
          url: "https://example.test/156",
          headRefName: "codex/issue-56",
          baseRefName: "main",
          body: "人工创建",
          isDraft: false,
          closingIssuesReferences: [{ number: 56 }],
        },
      ],
      new Set([53, 56]),
      "main",
    ),
    [pullRequest(53)],
  );
});
