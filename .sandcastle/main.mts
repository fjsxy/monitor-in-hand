import { execFileSync } from "node:child_process";
import { randomUUID } from "node:crypto";
import { existsSync, readFileSync } from "node:fs";
import { resolve } from "node:path";
import * as sandcastle from "@ai-hero/sandcastle";
import { resolveCodexAuthFile, stageCodexAuth } from "./auth.mts";
import {
  branchForIssue,
  localE2ERepairPromptArgs,
  isAgentTicket,
  promptArgsForIssue,
  selectFrontier,
  type IssueRecord,
} from "./orchestration.mts";
import {
  mergeReviewedPullRequests,
  selectResumablePullRequests,
  type PullRequestCandidate,
  type ReviewedPullRequest,
} from "./merge.mts";
import { createGitHubMergeOperations } from "./github-merge.mts";
import { createIssueSandboxProvider } from "./sandbox.mts";
import {
  assertE2EPlanningReadOnly,
  createGitHubE2EPlanOperations,
  extractE2EPlan,
  runWithE2EPlan,
} from "./e2e-plan.mts";
import {
  createSerialE2EGate,
  ensureLocalE2E,
  runLocalE2EGate,
  type SerialE2EGate,
} from "./e2e-gate.mts";
import {
  resolveAgentIdleTimeoutSeconds,
  resolveMaxCIRepairAttempts,
  resolveMaxLocalE2ERepairAttempts,
  withAgentIdleTimeout,
} from "./runtime-config.mts";

const EXPECTED_REMOTE = "https://github.com/fjsxy/monitor-in-hand";
const READY_LABEL = "ready-for-agent";
const repoRoot = process.cwd();

function command(commandName: string, args: string[]): string {
  return execFileSync(commandName, args, {
    cwd: repoRoot,
    encoding: "utf8",
    stdio: ["ignore", "pipe", "pipe"],
  }).trim();
}

function readLocalEnv(): Record<string, string> {
  const envPath = resolve(repoRoot, ".sandcastle/.env");
  if (!existsSync(envPath)) return {};

  return Object.fromEntries(
    readFileSync(envPath, "utf8")
      .split("\n")
      .map((line) => line.trim())
      .filter((line) => line && !line.startsWith("#") && line.includes("="))
      .map((line) => {
        const separator = line.indexOf("=");
        const key = line.slice(0, separator).trim();
        const value = line.slice(separator + 1).trim().replace(/^(['"])(.*)\1$/, "$2");
        return [key, value];
      }),
  );
}

function positiveInteger(value: string | undefined, fallback: number): number {
  const parsed = Number(value);
  return Number.isInteger(parsed) && parsed > 0 ? parsed : fallback;
}

function assertPreconditions(): {
  baseBranch: string;
  codexAuthFile: string;
  currentUser: string;
  ghToken: string;
} {
  if (resolve(repoRoot) !== resolve(command("git", ["rev-parse", "--show-toplevel"]))) {
    throw new Error("请从仓库根目录运行 npm run sandcastle。 ");
  }

  const remote = command("git", ["remote", "get-url", "origin"])
    .replace(/\.git$/, "");
  if (remote !== EXPECTED_REMOTE) {
    throw new Error(`origin 不匹配，拒绝执行 GitHub 写操作：${remote}`);
  }

  if (command("git", ["status", "--porcelain"])) {
    throw new Error("工作区不干净；请先提交或暂存现有修改，再运行 Sandcastle。");
  }

  const baseBranch = command("git", ["branch", "--show-current"]);
  if (!baseBranch) throw new Error("当前处于 detached HEAD，无法创建 ticket 分支。");
  if (baseBranch !== "main") {
    throw new Error(`必须从 main 运行 Sandcastle，当前分支是 ${baseBranch}。`);
  }
  command("git", ["fetch", "origin", "main"]);
  command("git", ["merge", "--ff-only", "origin/main"]);
  if (
    command("git", ["rev-parse", "HEAD"]) !==
    command("git", ["rev-parse", "origin/main"])
  ) {
    throw new Error("本地 main 含有未推送提交，拒绝启动自动 merge 流程。");
  }

  const localEnv = readLocalEnv();
  const codexAuthFile = resolveCodexAuthFile(
    process.env.CODEX_AUTH_FILE || localEnv.CODEX_AUTH_FILE,
  );

  const ghToken =
    process.env.GH_TOKEN || localEnv.GH_TOKEN || command("gh", ["auth", "token"]);
  const currentUser = command("gh", ["api", "user", "--jq", ".login"]);

  return { baseBranch, codexAuthFile, currentUser, ghToken };
}

function fetchIssues(): IssueRecord[] {
  return JSON.parse(
    command("gh", [
      "issue",
      "list",
      "--state",
      "all",
      "--limit",
      "1000",
      "--json",
      "number,title,body,state,labels,assignees",
    ]),
  ) as IssueRecord[];
}

function existingPullRequest(branch: string): string | undefined {
  const url = command("gh", [
    "pr",
    "list",
    "--state",
    "open",
    "--head",
    branch,
    "--json",
    "url",
    "--jq",
    ".[0].url // \"\"",
  ]);
  return url || undefined;
}

function fetchResumablePullRequests(
  issues: IssueRecord[],
  currentUser: string,
  baseBranch: string,
): ReviewedPullRequest[] {
  const eligibleIssues = new Set(
    issues
      .filter(isAgentTicket)
      .filter(
        (issue) =>
          issue.assignees.length > 0 &&
          issue.assignees.every((assignee) => assignee.login === currentUser),
      )
      .map((issue) => issue.number),
  );
  const candidates = JSON.parse(
    command("gh", [
      "pr",
      "list",
      "--state",
      "open",
      "--base",
      baseBranch,
      "--limit",
      "1000",
      "--json",
      "number,url,headRefName,baseRefName,body,isDraft,closingIssuesReferences",
    ]),
  ) as PullRequestCandidate[];
  return selectResumablePullRequests(candidates, eligibleIssues, baseBranch);
}

function claimIssue(issue: IssueRecord, currentUser: string): void {
  const assignees = JSON.parse(
    command("gh", ["issue", "view", String(issue.number), "--json", "assignees"]),
  ).assignees as Array<{ login: string }>;

  if (assignees.some((assignee) => assignee.login !== currentUser)) {
    throw new Error(`#${issue.number} 已由其他用户认领。`);
  }

  if (assignees.length === 0) {
    command("gh", [
      "issue",
      "edit",
      String(issue.number),
      "--add-assignee",
      "@me",
    ]);
  }

  const verified = JSON.parse(
    command("gh", ["issue", "view", String(issue.number), "--json", "assignees"]),
  ).assignees as Array<{ login: string }>;
  if (!verified.some((assignee) => assignee.login === currentUser)) {
    throw new Error(`#${issue.number} 认领回读失败。`);
  }
}

function publishPullRequest(
  issue: IssueRecord,
  branch: string,
  baseBranch: string,
): ReviewedPullRequest {
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
}

async function executeIssue(
  issue: IssueRecord,
  baseBranch: string,
  codexAuthFile: string,
  ghToken: string,
  agent: ReturnType<typeof sandcastle.codex>,
  maxIterations: number,
  idleTimeoutSeconds: number,
  localE2EGate: SerialE2EGate,
  maxLocalE2ERepairAttempts: number,
): Promise<{
  issue: number;
  status: string;
  detail?: string;
  pullRequest?: ReviewedPullRequest;
}> {
  const branch = branchForIssue(issue.number);
  const promptArgs = promptArgsForIssue(issue, branch);
  const stagedAuth = stageCodexAuth(codexAuthFile);

  try {
    const testRunId = `sc-${issue.number}-${randomUUID().slice(0, 8)}`;
    const sandbox = await sandcastle.createSandbox({
      branch,
      sandbox: createIssueSandboxProvider(ghToken, stagedAuth.mount),
      hooks: {
        sandbox: {
          onSandboxReady: [
            {
              command:
                "codex login status && gh auth status && gh auth setup-git && go mod download",
              timeoutMs: 300_000,
            },
          ],
        },
      },
      copyToWorktree: [".agents"],
    });

    try {
      // 领取后重新读取当前范围；E2E 计划先由只读 planner 整理，再由宿主发布和回读。
      const claimedIssue = JSON.parse(command("gh", [
        "issue", "view", String(issue.number), "--json", "number,title,body,state,labels,assignees",
      ])) as IssueRecord;
      if (!isAgentTicket(claimedIssue) || claimedIssue.assignees.length === 0) {
        throw new Error(`#${issue.number} 已退出 ready-for-agent 实现范围或领取已被撤销。`);
      }
      const matrix = readFileSync(resolve(sandbox.worktreePath, "docs/testing/phase1-acceptance-matrix.md"), "utf8");
      const implementation = await runWithE2EPlan(
        claimedIssue,
        matrix,
        createGitHubE2EPlanOperations(issue.number, repoRoot, async () => {
          const git = (args: string[]) => execFileSync("git", args, {
            cwd: sandbox.worktreePath, encoding: "utf8",
          }).trim();
          const beforeHead = git(["rev-parse", "HEAD"]);
          const planning = await sandbox.run(withAgentIdleTimeout(idleTimeoutSeconds, {
            name: `issue-${issue.number}-e2e-plan`,
            agent,
            maxIterations: 1,
            promptFile: "./.sandcastle/e2e-plan-prompt.md",
            promptArgs,
            completionSignal: "<promise>E2E_PLAN_READY</promise>",
          }));
          assertE2EPlanningReadOnly(
            planning.commits, beforeHead,
            git(["rev-parse", "HEAD"]), git(["status", "--porcelain"]),
          );
          if (!planning.completionSignal) throw new Error(`#${issue.number} E2E 规划未完成。`);
          return extractE2EPlan(planning.stdout);
        }),
        async (comment) => {
          const currentIssue = JSON.parse(command("gh", [
            "issue", "view", String(issue.number), "--json", "number,title,body,state,labels,assignees",
          ])) as IssueRecord;
          if (!isAgentTicket(currentIssue) || currentIssue.body !== claimedIssue.body ||
              currentIssue.assignees.length !== claimedIssue.assignees.length ||
              currentIssue.assignees.some((assignee) => !claimedIssue.assignees.some((owner) => owner.login === assignee.login))) {
            throw new Error(`#${issue.number} 的范围或领取状态在 E2E 规划期间发生变化，停止实现。`);
          }
          console.log(`#${issue.number} 实现前 E2E 计划已发布并回读：${comment.url}`);
          return sandbox.run(withAgentIdleTimeout(idleTimeoutSeconds, {
            name: `issue-${issue.number}-implement`,
            agent,
            maxIterations,
            promptFile: "./.sandcastle/implement-prompt.md",
            promptArgs,
          }));
        },
      );

      if (!implementation.completionSignal) {
        return {
          issue: issue.number,
          status: "incomplete",
          detail: "implementer 未发出完成信号，分支保留但不会创建 PR",
        };
      }

      if (implementation.commits.length === 0) {
        return {
          issue: issue.number,
          status: "no-commits",
          detail: "implementer 报告完成但没有产生 commit",
        };
      }

      const review = await sandbox.run(
        withAgentIdleTimeout(idleTimeoutSeconds, {
          name: `issue-${issue.number}-review`,
          agent,
          maxIterations: 1,
          promptFile: "./.sandcastle/review-prompt.md",
          promptArgs,
        }),
      );

      if (!review.completionSignal) {
        return {
          issue: issue.number,
          status: "review-incomplete",
          detail: "reviewer 未发出完成信号，分支保留但不会创建 PR",
        };
      }

      // agent 只在 sandbox 内实现和评审；宿主串行运行 Docker/kind E2E，避免向 sandbox 暴露容器控制面。
      await localE2EGate.run(() =>
        ensureLocalE2E(
          {
            runGate: () =>
              runLocalE2EGate(sandbox.worktreePath, testRunId, (line) => {
                console.log(`#${issue.number} [本地 E2E] ${line}`);
              }),
            repairFailure: async (failure, attempt) => {
              console.warn(
                `#${issue.number} 本地 E2E 失败，启动第 ${attempt}/${maxLocalE2ERepairAttempts} 次修复；worktree 日志：${failure.logPath}；宿主归档：${failure.archivePath}`,
              );
              const repair = await sandbox.run(
                withAgentIdleTimeout(idleTimeoutSeconds, {
                  name: `issue-${issue.number}-local-e2e-repair-${attempt}`,
                  agent,
                  maxIterations,
                  promptFile: "./.sandcastle/local-e2e-repair-prompt.md",
                  promptArgs: localE2ERepairPromptArgs(
                    issue,
                    branch,
                    testRunId,
                    failure.logPath,
                    attempt,
                  ),
                }),
              );
              if (!repair.completionSignal) {
                throw new Error(
                  `#${issue.number} 本地 E2E repair agent 未发出完成信号。`,
                );
              }
              if (repair.commits.length === 0) {
                throw new Error(
                  `#${issue.number} 本地 E2E repair agent 没有产生 commit。`,
                );
              }
            },
            reviewRepair: async (attempt) => {
              const repairedReview = await sandbox.run(
                withAgentIdleTimeout(idleTimeoutSeconds, {
                  name: `issue-${issue.number}-local-e2e-review-${attempt}`,
                  agent,
                  maxIterations: 1,
                  promptFile: "./.sandcastle/review-prompt.md",
                  promptArgs,
                }),
              );
              if (!repairedReview.completionSignal) {
                throw new Error(
                  `#${issue.number} 本地 E2E 修复后的 code review 未通过。`,
                );
              }
            },
          },
          maxLocalE2ERepairAttempts,
        ),
      );
    } finally {
      const closeResult = await sandbox.close();
      if (closeResult.preservedWorktreePath) {
        console.warn(
          `#${issue.number} 存在未提交修改，worktree 已保留：${closeResult.preservedWorktreePath}`,
        );
      }
    }
  } finally {
    stagedAuth.cleanup();
  }

  const pullRequest = publishPullRequest(issue, branch, baseBranch);
  return {
    issue: issue.number,
    status: "pull-request",
    detail: pullRequest.url,
    pullRequest,
  };
}

async function main(): Promise<void> {
  const { baseBranch, codexAuthFile, currentUser, ghToken } = assertPreconditions();
  const localEnv = readLocalEnv();
  const model =
    process.env.SANDCASTLE_MODEL ||
    localEnv.SANDCASTLE_MODEL ||
    "gpt-5.6-sol";
  const maxParallel = positiveInteger(
    process.env.SANDCASTLE_MAX_PARALLEL || localEnv.SANDCASTLE_MAX_PARALLEL,
    3,
  );
  const maxIterations = positiveInteger(
    process.env.SANDCASTLE_MAX_ITERATIONS || localEnv.SANDCASTLE_MAX_ITERATIONS,
    3,
  );
  const idleTimeoutSeconds = resolveAgentIdleTimeoutSeconds(
    process.env,
    localEnv,
  );
  const ciTimeoutMinutes = positiveInteger(
    process.env.SANDCASTLE_CI_TIMEOUT_MINUTES ||
      localEnv.SANDCASTLE_CI_TIMEOUT_MINUTES,
    60,
  );
  const ciPollSeconds = positiveInteger(
    process.env.SANDCASTLE_CI_POLL_SECONDS || localEnv.SANDCASTLE_CI_POLL_SECONDS,
    15,
  );
  const maxCIRepairAttempts = resolveMaxCIRepairAttempts(
    process.env,
    localEnv,
  );
  const maxLocalE2ERepairAttempts = resolveMaxLocalE2ERepairAttempts(
    process.env,
    localEnv,
  );

  const issues = fetchIssues();
  const resumablePullRequests = fetchResumablePullRequests(
    issues,
    currentUser,
    baseBranch,
  );
  const frontier = selectFrontier(issues, currentUser, maxParallel).filter(
    (issue) => !existingPullRequest(branchForIssue(issue.number)),
  );

  if (frontier.length === 0 && resumablePullRequests.length === 0) {
    console.log(`没有可执行的 ${READY_LABEL} frontier ticket。`);
    return;
  }

  if (frontier.length > 0) {
    console.log(
      `本轮将在 ${baseBranch} 基础上并行处理：${frontier
        .map((issue) => `#${issue.number}`)
        .join(", ")}`,
    );
  }
  if (resumablePullRequests.length > 0) {
    console.log(
      `恢复已有 Sandcastle PR：${resumablePullRequests
        .map((pr) => `#${pr.number}`)
        .join(", ")}`,
    );
  }

  const claimed: IssueRecord[] = [];
  for (const issue of frontier) {
    try {
      claimIssue(issue, currentUser);
      claimed.push(issue);
      console.log(`已认领 #${issue.number}。`);
    } catch (error) {
      console.warn(`#${issue.number} 认领失败，跳过：${String(error)}`);
    }
  }

  if (claimed.length === 0 && resumablePullRequests.length === 0) {
    throw new Error("本轮 frontier ticket 均未能成功认领。");
  }

  const agent = sandcastle.codex(model, { effort: "high" });
  const localE2EGate = createSerialE2EGate();

  const settled = await Promise.allSettled(
    claimed.map((issue) =>
      executeIssue(
        issue,
        baseBranch,
        codexAuthFile,
        ghToken,
        agent,
        maxIterations,
        idleTimeoutSeconds,
        localE2EGate,
        maxLocalE2ERepairAttempts,
      ),
    ),
  );

  let failed = false;
  const reviewedPullRequests: ReviewedPullRequest[] = [
    ...resumablePullRequests,
  ];
  for (const [index, result] of settled.entries()) {
    const issue = claimed[index]!;
    if (result.status === "rejected") {
      failed = true;
      console.error(`#${issue.number}: failed — ${String(result.reason)}`);
      continue;
    }
    if (result.value.status !== "pull-request") failed = true;
    if (result.value.pullRequest) {
      if (
        !reviewedPullRequests.some(
          (pr) => pr.number === result.value.pullRequest!.number,
        )
      ) {
        reviewedPullRequests.push(result.value.pullRequest);
      }
    }
    console.log(
      `#${issue.number}: ${result.value.status}${
        result.value.detail ? ` — ${result.value.detail}` : ""
      }`,
    );
  }

  if (reviewedPullRequests.length > 0) {
    console.log(
      `进入串行 merge 阶段：${reviewedPullRequests
        .sort((left, right) => left.issue - right.issue)
        .map((pr) => `PR #${pr.number}`)
        .join(", ")}`,
    );
    const mergeResults = await mergeReviewedPullRequests(
      reviewedPullRequests,
      createGitHubMergeOperations({
        baseBranch,
        codexAuthFile,
        ghToken,
        agent,
        maxIterations,
        maxCIRepairAttempts,
        idleTimeoutSeconds,
        ciTimeoutMs: ciTimeoutMinutes * 60_000,
        ciPollMs: ciPollSeconds * 1_000,
        issuesByNumber: new Map(issues.map((issue) => [issue.number, issue])),
      }),
    );
    for (const result of mergeResults) {
      if (result.status !== "merged") failed = true;
      console.log(
        `#${result.issue} / PR #${result.pr}: ${result.status}${
          result.detail ? ` — ${result.detail}` : ""
        }`,
      );
    }
  }

  if (failed) process.exitCode = 1;
}

await main();
