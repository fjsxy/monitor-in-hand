import { runWithE2EPlan, type E2EPlanComment, type E2EPlanOperations } from "./e2e-plan.mts";
import type { IssueRecord } from "./orchestration.mts";
import { observeMilestone, reportMilestone, type MilestoneSink } from "./notifications.mts";

// 计划与实现分别记录终态；implement 抛错时不能误报为计划发布失败。
export async function runNotifiedImplementation<T extends { completionSignal?: unknown; commits: readonly unknown[] }>(
  issue: Pick<IssueRecord, "number" | "body">,
  matrix: string,
  operations: E2EPlanOperations,
  implement: (comment: E2EPlanComment) => Promise<T>,
  notify?: MilestoneSink,
): Promise<T> {
  const started = Date.now();
  let verified = false;
  try {
    return await runWithE2EPlan(issue, matrix, operations, async (comment) => {
      verified = true;
      reportMilestone(notify, { issue: issue.number, stage: "e2e-plan", status: "succeeded", durationMs: Date.now() - started, url: comment.url });
      return observeMilestone(notify, issue.number, "implementation", () => implement(comment), (result) => {
        if (!result.completionSignal) return { status: "failed", detail: "implementer 未发出完成信号，分支保留但不会创建 PR" };
        if (!result.commits.length) return { status: "failed", detail: "implementer 报告完成但没有产生 commit" };
        return { detail: `产生 ${result.commits.length} 个 commit；等待后续验收与合并。` };
      });
    });
  } catch (error) {
    if (!verified) reportMilestone(notify, { issue: issue.number, stage: "e2e-plan", status: "failed", durationMs: Date.now() - started, detail: String(error) });
    throw error;
  }
}
