import type { IssueRecord } from "./orchestration.mts";
import type { ReviewedPullRequest } from "./merge.mts";
import { observeMilestone, type MilestoneSink } from "./notifications.mts";

export interface PullRequestPublishOperations {
  command(name: string, args: string[]): string;
  existingPullRequest(branch: string): string | undefined;
}

export async function publishNotifiedPullRequest(
  issue: Pick<IssueRecord, "number" | "title">,
  branch: string,
  baseBranch: string,
  operations: PullRequestPublishOperations,
  notify?: MilestoneSink,
): Promise<ReviewedPullRequest> {
  // PR 通知必须经过 push/create 和远端回读；不能以 create 命令退出作为完成依据。
  return observeMilestone(notify, issue.number, "pull-request", () => {
    const { command, existingPullRequest } = operations;
    command("git", ["push", "--set-upstream", "origin", branch]);

    const existing = existingPullRequest(branch);
    if (!existing) {
      command("gh", [
        "pr",
        "create",
        "--base",
        baseBranch,
        "--head",
        branch,
        "--title",
        issue.title,
        "--body",
        [
          `Closes #${issue.number}`,
          "",
          "由 Sandcastle 在隔离 worktree/sandbox 中实现并复核。",
          "Issue 仅在 PR 通过 CI 并由串行 merge 流程以 rebase 方式合并后关闭。",
        ].join("\n"),
      ]);
    }

    const published = JSON.parse(
      command("gh", [
        "pr",
        "view",
        branch,
        "--json",
        "number,url,headRefName,baseRefName,state,isDraft",
      ]),
    ) as {
      number: number;
      url: string;
      headRefName: string;
      baseRefName: string;
      state: string;
      isDraft: boolean;
    };
    if (
      !Number.isInteger(published.number) || published.number <= 0 || !published.url ||
      published.state !== "OPEN" ||
      published.isDraft ||
      published.headRefName !== branch ||
      published.baseRefName !== baseBranch
    ) {
      throw new Error(`PR #${published.number} 状态或分支与 merge 前置条件不符。`);
    }
    return {
      issue: issue.number,
      number: published.number,
      branch,
      url: published.url,
    };
  }, (pr) => ({ url: pr.url }));
}
