export interface ReviewedPullRequest {
  issue: number;
  number: number;
  branch: string;
  url: string;
}

export interface PullRequestCandidate {
  number: number;
  url: string;
  headRefName: string;
  baseRefName: string;
  body: string;
  isDraft: boolean;
  closingIssuesReferences: Array<{ number: number }>;
}

const SANDCASTLE_PR_MARKER =
  "由 Sandcastle 在隔离 worktree/sandbox 中实现并复核。";

export function selectResumablePullRequests(
  candidates: PullRequestCandidate[],
  eligibleIssues: ReadonlySet<number>,
  baseBranch: string,
): ReviewedPullRequest[] {
  const result: ReviewedPullRequest[] = [];
  for (const candidate of candidates) {
    const branchMatch = /^codex\/issue-([1-9][0-9]*)$/.exec(
      candidate.headRefName,
    );
    if (!branchMatch) continue;
    const issue = Number(branchMatch[1]);
    if (
      candidate.isDraft ||
      candidate.baseRefName !== baseBranch ||
      !candidate.body.includes(SANDCASTLE_PR_MARKER) ||
      !eligibleIssues.has(issue) ||
      !candidate.closingIssuesReferences.some(
        (reference) => reference.number === issue,
      )
    ) {
      continue;
    }
    result.push({
      issue,
      number: candidate.number,
      branch: candidate.headRefName,
      url: candidate.url,
    });
  }
  return result.sort((left, right) => left.issue - right.issue);
}

export interface MergeOperations {
  waitForCI(pr: ReviewedPullRequest): Promise<string>;
  maxCIRepairAttempts?: number;
  repairFailedCI?(
    pr: ReviewedPullRequest,
    failure: PullRequestCIFailedError,
    attempt: number,
  ): Promise<string>;
  rebaseOntoLatestBase(
    pr: ReviewedPullRequest,
    checkedHeadOid: string,
  ): Promise<string>;
  mergeWithRebase(
    pr: ReviewedPullRequest,
    checkedHeadOid: string,
  ): Promise<void>;
  verifyMerged(pr: ReviewedPullRequest): Promise<void>;
}

export interface MergeResult {
  issue: number;
  pr: number;
  status: "merged" | "blocked" | "not-attempted";
  detail?: string;
}

export type StatusCheck =
  | { status: string; conclusion: string | null }
  | { state: string };

export type CIState = "pending" | "passed" | "failed";

export class PullRequestCIFailedError extends Error {
  constructor(
    readonly pullRequestNumber: number,
    readonly headOid: string,
    readonly failedChecks: string[],
  ) {
    super(
      `PR #${pullRequestNumber} CI 失败：${failedChecks.join(", ")}`,
    );
    this.name = "PullRequestCIFailedError";
  }
}

export function classifyCI(checks: StatusCheck[]): CIState {
  if (checks.length === 0) return "pending";

  let pending = false;
  for (const check of checks) {
    if ("status" in check) {
      if (check.status !== "COMPLETED") {
        pending = true;
        continue;
      }
      if (!new Set(["SUCCESS", "NEUTRAL", "SKIPPED"]).has(check.conclusion ?? "")) {
        return "failed";
      }
      continue;
    }

    if (new Set(["PENDING", "EXPECTED"]).has(check.state)) {
      pending = true;
    } else if (check.state !== "SUCCESS") {
      return "failed";
    }
  }
  return pending ? "pending" : "passed";
}

async function waitForCIWithRepair(
  pr: ReviewedPullRequest,
  operations: MergeOperations,
  repairBudget: { attempts: number },
  initialExpectedHeadOid?: string,
): Promise<string> {
  let expectedHeadOid = initialExpectedHeadOid;
  while (true) {
    try {
      const checkedHeadOid = await operations.waitForCI(pr);
      if (expectedHeadOid && checkedHeadOid !== expectedHeadOid) {
        throw new Error(`PR #${pr.number} 等待 CI 期间 head 已变化。`);
      }
      return checkedHeadOid;
    } catch (error) {
      if (
        !(error instanceof PullRequestCIFailedError) ||
        !operations.repairFailedCI ||
        repairBudget.attempts >= (operations.maxCIRepairAttempts ?? 0)
      ) {
        throw error;
      }
      if (expectedHeadOid && error.headOid !== expectedHeadOid) {
        throw new Error(`PR #${pr.number} CI 失败前 head 已变化。`);
      }
      repairBudget.attempts += 1;
      expectedHeadOid = await operations.repairFailedCI(
        pr,
        error,
        repairBudget.attempts,
      );
    }
  }
}

export async function mergeReviewedPullRequests(
  pullRequests: ReviewedPullRequest[],
  operations: MergeOperations,
): Promise<MergeResult[]> {
  const ordered = [...pullRequests].sort(
    (left, right) => left.issue - right.issue,
  );
  const results: MergeResult[] = [];

  for (const [index, pr] of ordered.entries()) {
    try {
      const repairBudget = { attempts: 0 };
      const checkedHeadOid = await waitForCIWithRepair(
        pr,
        operations,
        repairBudget,
      );
      const rebasedHeadOid = await operations.rebaseOntoLatestBase(
        pr,
        checkedHeadOid,
      );
      let mergeHeadOid = checkedHeadOid;
      if (rebasedHeadOid !== checkedHeadOid) {
        mergeHeadOid = await waitForCIWithRepair(
          pr,
          operations,
          repairBudget,
          rebasedHeadOid,
        );
      }
      await operations.mergeWithRebase(pr, mergeHeadOid);
      await operations.verifyMerged(pr);
      results.push({ issue: pr.issue, pr: pr.number, status: "merged" });
    } catch (error) {
      results.push({
        issue: pr.issue,
        pr: pr.number,
        status: "blocked",
        detail: error instanceof Error ? error.message : String(error),
      });
      for (const pending of ordered.slice(index + 1)) {
        results.push({
          issue: pending.issue,
          pr: pending.number,
          status: "not-attempted",
          detail: "前序 PR 未能合并",
        });
      }
      break;
    }
  }

  return results;
}
