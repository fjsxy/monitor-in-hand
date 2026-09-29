import assert from "node:assert/strict";
import test from "node:test";
import { createGitHubMergeOperations } from "./github-merge.mts";
import {
  PullRequestCIFailedError,
  type ReviewedPullRequest,
} from "./merge.mts";

const pullRequest: ReviewedPullRequest = {
  issue: 56,
  number: 72,
  branch: "codex/issue-56",
  url: "https://github.com/fjsxy/monitor-in-hand/pull/72",
};

function snapshot(headRefOid: string) {
  return {
    number: 72,
    state: "OPEN" as const,
    isDraft: false,
    headRefName: "codex/issue-56",
    headRefOid,
    baseRefName: "main",
    statusCheckRollup: [],
  };
}

test("GitHub CI repair 锁定失败 head，运行 agent，带 lease 推送并回读新 head", async () => {
  const events: string[] = [];
  const snapshots = [snapshot("failed-head"), snapshot("failed-head"), snapshot("repaired-head")];
  const operations = createGitHubMergeOperations(
    {
      baseBranch: "main",
      codexAuthFile: "/tmp/auth.json",
      ghToken: "test-token",
      agent: null as never,
      maxIterations: 3,
      maxCIRepairAttempts: 2,
      idleTimeoutSeconds: 1_800,
      ciTimeoutMs: 60_000,
      ciPollMs: 1_000,
      issuesByNumber: new Map([
        [
          56,
          {
            number: 56,
            title: "交付分片模式",
            body: "",
            state: "OPEN",
            labels: [{ name: "ready-for-agent" }],
            assignees: [{ login: "fjsxy" }],
          },
        ],
      ]),
    },
    {
      fetchPullRequest() {
        events.push("fetch");
        return snapshots.shift()!;
      },
      async runCIRepairAgent(input) {
        events.push(`agent:${input.attempt}:${input.failure.headOid}`);
        return { repairedHeadOid: "repaired-head" };
      },
      pushRepairedBranch(pr, expectedHeadOid) {
        events.push(`push:${pr.branch}:${expectedHeadOid}`);
      },
    },
  );

  const repairedHeadOid = await operations.repairFailedCI!(
    pullRequest,
    new PullRequestCIFailedError(72, "failed-head", ["test-environment"]),
    1,
  );

  assert.equal(repairedHeadOid, "repaired-head");
  assert.equal(operations.maxCIRepairAttempts, 2);
  assert.deepEqual(events, [
    "fetch",
    "agent:1:failed-head",
    "fetch",
    "push:codex/issue-56:failed-head",
    "fetch",
  ]);
});
