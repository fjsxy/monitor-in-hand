import { execFileSync, spawnSync } from "node:child_process";
import * as sandcastle from "@ai-hero/sandcastle";
import { stageCodexAuth } from "./auth.mts";
import {
  classifyCI,
  PullRequestCIFailedError,
  type MergeOperations,
  type ReviewedPullRequest,
  type StatusCheck,
} from "./merge.mts";
import {
  ciRepairPromptArgs,
  promptArgsForIssue,
  type IssueRecord,
} from "./orchestration.mts";
import { createIssueSandboxProvider } from "./sandbox.mts";
import { withAgentIdleTimeout } from "./runtime-config.mts";

const repoRoot = process.cwd();

function command(commandName: string, args: string[]): string {
  return execFileSync(commandName, args, {
    cwd: repoRoot,
    encoding: "utf8",
    stdio: ["ignore", "pipe", "pipe"],
  }).trim();
}

function tryCommand(
  commandName: string,
  args: string[],
): { ok: boolean; stdout: string; stderr: string } {
  const result = spawnSync(commandName, args, {
    cwd: repoRoot,
    encoding: "utf8",
    stdio: ["ignore", "pipe", "pipe"],
  });
  if (result.error) throw result.error;
  return {
    ok: result.status === 0,
    stdout: result.stdout.trim(),
    stderr: result.stderr.trim(),
  };
}

function delay(milliseconds: number): Promise<void> {
  return new Promise((resolveDelay) => setTimeout(resolveDelay, milliseconds));
}

function shellQuote(value: string): string {
  return `'${value.replaceAll("'", `'"'"'`)}'`;
}

type GitHubStatusCheck = StatusCheck & {
  name?: string;
  context?: string;
};

export interface PullRequestSnapshot {
  number: number;
  state: "OPEN" | "CLOSED" | "MERGED";
  isDraft: boolean;
  headRefName: string;
  headRefOid: string;
  baseRefName: string;
  statusCheckRollup: GitHubStatusCheck[];
}

export interface CIRepairAgentInput {
  pr: ReviewedPullRequest;
  issue: IssueRecord;
  failure: PullRequestCIFailedError;
  attempt: number;
}

export interface CIRepairDependencies {
  fetchPullRequest(number: number): PullRequestSnapshot;
  runCIRepairAgent(input: CIRepairAgentInput): Promise<{
    repairedHeadOid: string;
  }>;
  pushRepairedBranch(
    pr: ReviewedPullRequest,
    expectedHeadOid: string,
  ): void;
  delayBeforeRefetch(): Promise<void>;
}

const REPAIR_PUSH_VISIBILITY_ATTEMPTS = 30;

function fetchPullRequest(number: number): PullRequestSnapshot {
  return JSON.parse(
    command("gh", [
      "pr",
      "view",
      String(number),
      "--json",
      "number,state,isDraft,headRefName,headRefOid,baseRefName,statusCheckRollup",
    ]),
  ) as PullRequestSnapshot;
}

function failedCheckNames(checks: GitHubStatusCheck[]): string[] {
  return checks
    .filter((check) => classifyCI([check]) === "failed")
    .map((check) => check.name || check.context || "unknown-check");
}

async function waitForPullRequestCI(
  pr: ReviewedPullRequest,
  timeoutMs: number,
  pollMs: number,
): Promise<string> {
  const deadline = Date.now() + timeoutMs;
  let lastProgress = "";

  while (Date.now() < deadline) {
    const snapshot = fetchPullRequest(pr.number);
    if (snapshot.state !== "OPEN") {
      throw new Error(`PR #${pr.number} 不再处于 OPEN 状态：${snapshot.state}`);
    }
    if (snapshot.isDraft) throw new Error(`PR #${pr.number} 仍是 draft。`);
    if (snapshot.headRefName !== pr.branch) {
      throw new Error(
        `PR #${pr.number} head 分支不匹配：${snapshot.headRefName}`,
      );
    }

    const state = classifyCI(snapshot.statusCheckRollup);
    const progress = `${snapshot.headRefOid}:${state}:${snapshot.statusCheckRollup.length}`;
    if (progress !== lastProgress) {
      console.log(
        `PR #${pr.number} CI ${state}（${snapshot.statusCheckRollup.length} checks，head ${snapshot.headRefOid.slice(0, 12)}）。`,
      );
      lastProgress = progress;
    }
    if (state === "passed") return snapshot.headRefOid;
    if (state === "failed") {
      throw new PullRequestCIFailedError(
        pr.number,
        snapshot.headRefOid,
        failedCheckNames(snapshot.statusCheckRollup),
      );
    }
    await delay(pollMs);
  }

  throw new Error(`PR #${pr.number} 等待 CI 超时。`);
}

async function runCIRepairAgent(
  input: CIRepairAgentInput,
  options: {
    codexAuthFile: string;
    ghToken: string;
    agent: ReturnType<typeof sandcastle.codex>;
    maxIterations: number;
    idleTimeoutSeconds: number;
  },
): Promise<{ repairedHeadOid: string }> {
  const stagedAuth = stageCodexAuth(options.codexAuthFile);
  let sandbox: Awaited<ReturnType<typeof sandcastle.createSandbox>> | undefined;
  let repairedHeadOid: string | undefined;
  let preservedWorktreePath: string | undefined;

  try {
    sandbox = await sandcastle.createSandbox({
      branch: input.pr.branch,
      sandbox: createIssueSandboxProvider(options.ghToken, stagedAuth.mount),
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

    const currentHead = await sandbox.exec("git rev-parse HEAD");
    if (
      currentHead.exitCode !== 0 ||
      currentHead.stdout.trim() !== input.failure.headOid
    ) {
      throw new Error(
        `PR #${input.pr.number} repair 前本地 head 与失败 head 不一致。`,
      );
    }

    const repair = await sandbox.run(
      withAgentIdleTimeout(options.idleTimeoutSeconds, {
        name: `issue-${input.pr.issue}-ci-repair-${input.attempt}`,
        agent: options.agent,
        maxIterations: options.maxIterations,
        promptFile: "./.sandcastle/ci-repair-prompt.md",
        promptArgs: ciRepairPromptArgs(
          input.issue,
          input.pr.branch,
          input.pr.number,
          input.failure.headOid,
          input.failure.failedChecks,
          input.attempt,
        ),
      }),
    );
    if (!repair.completionSignal) {
      throw new Error(
        `PR #${input.pr.number} repair agent 未发出完成信号。`,
      );
    }
    if (repair.commits.length === 0) {
      throw new Error(`PR #${input.pr.number} repair agent 没有产生 commit。`);
    }

    const verify = await sandbox.exec(
      'test -z "$(git status --porcelain)" && git rev-parse HEAD',
    );
    if (verify.exitCode !== 0 || !verify.stdout.trim()) {
      throw new Error(`PR #${input.pr.number} repair 后工作区不干净。`);
    }
    repairedHeadOid = verify.stdout.trim();
    if (repairedHeadOid === input.failure.headOid) {
      throw new Error(`PR #${input.pr.number} repair 后 head 未变化。`);
    }
  } finally {
    if (sandbox) {
      const closeResult = await sandbox.close();
      preservedWorktreePath = closeResult.preservedWorktreePath;
      if (preservedWorktreePath) {
        console.warn(
          `PR #${input.pr.number} repair worktree 已保留：${preservedWorktreePath}`,
        );
      }
    }
    stagedAuth.cleanup();
  }

  if (!repairedHeadOid || preservedWorktreePath) {
    throw new Error(`PR #${input.pr.number} repair 结果未达到可推送状态。`);
  }
  return { repairedHeadOid };
}

async function resolveRebaseWithAgent(
  pr: ReviewedPullRequest,
  issue: IssueRecord,
  baseBranch: string,
  checkedHeadOid: string,
  codexAuthFile: string,
  ghToken: string,
  agent: ReturnType<typeof sandcastle.codex>,
  maxIterations: number,
  idleTimeoutSeconds: number,
): Promise<string> {
  const stagedAuth = stageCodexAuth(codexAuthFile);
  let sandbox: Awaited<ReturnType<typeof sandcastle.createSandbox>> | undefined;
  let rebasedHeadOid: string | undefined;
  let preservedWorktreePath: string | undefined;

  try {
    sandbox = await sandcastle.createSandbox({
      branch: pr.branch,
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

    const fetchResult = await sandbox.exec(
      `git fetch --prune origin ${shellQuote(baseBranch)}`,
    );
    if (fetchResult.exitCode !== 0) {
      throw new Error(fetchResult.stderr || `PR #${pr.number} fetch 失败。`);
    }

    const currentHead = await sandbox.exec("git rev-parse HEAD");
    if (currentHead.exitCode !== 0 || currentHead.stdout.trim() !== checkedHeadOid) {
      throw new Error(`PR #${pr.number} 在 rebase 前 head 已变化，拒绝覆盖。`);
    }

    const rebase = await sandbox.exec(
      `GIT_EDITOR=true git rebase ${shellQuote(`origin/${baseBranch}`)}`,
      { onLine: (line) => console.log(`[PR #${pr.number} rebase] ${line}`) },
    );
    if (rebase.exitCode !== 0) {
      const unmerged = await sandbox.exec(
        "git diff --name-only --diff-filter=U",
      );
      if (unmerged.exitCode !== 0 || !unmerged.stdout.trim()) {
        throw new Error(
          rebase.stderr || `PR #${pr.number} rebase 失败且没有可解析的冲突。`,
        );
      }

      console.log(
        `PR #${pr.number} 存在冲突，启动 resolver：${unmerged.stdout.trim().replaceAll("\n", ", ")}`,
      );
      const resolution = await sandbox.run(
        withAgentIdleTimeout(idleTimeoutSeconds, {
          name: `issue-${pr.issue}-merge-conflict`,
          agent,
          maxIterations,
          promptFile: "./.sandcastle/merge-conflict-prompt.md",
          promptArgs: promptArgsForIssue(issue, pr.branch),
        }),
      );
      if (!resolution.completionSignal) {
        throw new Error(`PR #${pr.number} resolver 未完成冲突解决。`);
      }

      const review = await sandbox.run(
        withAgentIdleTimeout(idleTimeoutSeconds, {
          name: `issue-${pr.issue}-post-rebase-review`,
          agent,
          maxIterations: 1,
          promptFile: "./.sandcastle/review-prompt.md",
          promptArgs: promptArgsForIssue(issue, pr.branch),
        }),
      );
      if (!review.completionSignal) {
        throw new Error(`PR #${pr.number} 冲突解决后的 code-review 未通过。`);
      }
    }

    const verify = await sandbox.exec(
      [
        'test -z "$(git diff --name-only --diff-filter=U)"',
        'test ! -d "$(git rev-parse --git-path rebase-merge)"',
        'test ! -d "$(git rev-parse --git-path rebase-apply)"',
        `git merge-base --is-ancestor ${shellQuote(`origin/${baseBranch}`)} HEAD`,
        'test -z "$(git status --porcelain)"',
      ].join(" && "),
    );
    if (verify.exitCode !== 0) {
      throw new Error(
        verify.stderr || `PR #${pr.number} rebase 后不满足干净、无冲突不变量。`,
      );
    }

    const head = await sandbox.exec("git rev-parse HEAD");
    if (head.exitCode !== 0 || !head.stdout.trim()) {
      throw new Error(head.stderr || `PR #${pr.number} 无法读取 rebased head。`);
    }
    rebasedHeadOid = head.stdout.trim();
  } finally {
    if (sandbox) {
      const closeResult = await sandbox.close();
      preservedWorktreePath = closeResult.preservedWorktreePath;
      if (preservedWorktreePath) {
        console.warn(
          `PR #${pr.number} merge worktree 已保留：${preservedWorktreePath}`,
        );
      }
    }
    stagedAuth.cleanup();
  }

  if (!rebasedHeadOid || preservedWorktreePath) {
    throw new Error(`PR #${pr.number} rebase 结果未达到可推送状态。`);
  }

  command("git", [
    "push",
    `--force-with-lease=refs/heads/${pr.branch}:${checkedHeadOid}`,
    "origin",
    `${pr.branch}:refs/heads/${pr.branch}`,
  ]);
  const pushed = fetchPullRequest(pr.number);
  if (pushed.headRefOid !== rebasedHeadOid) {
    throw new Error(`PR #${pr.number} rebase 推送后 head 回读不一致。`);
  }
  return rebasedHeadOid;
}

export function createGitHubMergeOperations(
  options: {
    baseBranch: string;
    codexAuthFile: string;
    ghToken: string;
    agent: ReturnType<typeof sandcastle.codex>;
    maxIterations: number;
    maxCIRepairAttempts: number;
    idleTimeoutSeconds: number;
    ciTimeoutMs: number;
    ciPollMs: number;
    issuesByNumber: Map<number, IssueRecord>;
  },
  dependencies?: CIRepairDependencies,
): MergeOperations {
  const repairDependencies: CIRepairDependencies = dependencies ?? {
    fetchPullRequest,
    runCIRepairAgent: (input) =>
      runCIRepairAgent(input, {
        codexAuthFile: options.codexAuthFile,
        ghToken: options.ghToken,
        agent: options.agent,
        maxIterations: options.maxIterations,
        idleTimeoutSeconds: options.idleTimeoutSeconds,
      }),
    pushRepairedBranch(pr, expectedHeadOid) {
      command("git", [
        "push",
        `--force-with-lease=refs/heads/${pr.branch}:${expectedHeadOid}`,
        "origin",
        `${pr.branch}:refs/heads/${pr.branch}`,
      ]);
    },
    delayBeforeRefetch: () => delay(1_000),
  };

  return {
    maxCIRepairAttempts: options.maxCIRepairAttempts,
    waitForCI: (pr) =>
      waitForPullRequestCI(pr, options.ciTimeoutMs, options.ciPollMs),

    async repairFailedCI(pr, failure, attempt) {
      const issue = options.issuesByNumber.get(pr.issue);
      if (!issue) throw new Error(`找不到 PR #${pr.number} 对应的 Issue。`);
      if (failure.pullRequestNumber !== pr.number) {
        throw new Error(`PR #${pr.number} 的 CI failure 身份不匹配。`);
      }

      const assertFailedHeadUnchanged = () => {
        const snapshot = repairDependencies.fetchPullRequest(pr.number);
        if (
          snapshot.state !== "OPEN" ||
          snapshot.isDraft ||
          snapshot.baseRefName !== options.baseBranch ||
          snapshot.headRefName !== pr.branch ||
          snapshot.headRefOid !== failure.headOid
        ) {
          throw new Error(`PR #${pr.number} repair 前状态或 head 已变化。`);
        }
      };

      assertFailedHeadUnchanged();
      console.log(
        `PR #${pr.number} 启动第 ${attempt}/${options.maxCIRepairAttempts} 次 CI 自动修复：${failure.failedChecks.join(", ")}`,
      );
      const result = await repairDependencies.runCIRepairAgent({
        pr,
        issue,
        failure,
        attempt,
      });
      assertFailedHeadUnchanged();
      repairDependencies.pushRepairedBranch(pr, failure.headOid);

      let published: PullRequestSnapshot | undefined;
      for (
        let visibilityAttempt = 1;
        visibilityAttempt <= REPAIR_PUSH_VISIBILITY_ATTEMPTS;
        visibilityAttempt += 1
      ) {
        const snapshot = repairDependencies.fetchPullRequest(pr.number);
        if (
          snapshot.state === "OPEN" &&
          snapshot.headRefOid === result.repairedHeadOid
        ) {
          published = snapshot;
          break;
        }
        if (
          snapshot.state !== "OPEN" ||
          snapshot.headRefOid !== failure.headOid
        ) {
          throw new Error(`PR #${pr.number} repair 推送后 head 回读不一致。`);
        }
        if (visibilityAttempt < REPAIR_PUSH_VISIBILITY_ATTEMPTS) {
          await repairDependencies.delayBeforeRefetch();
        }
      }
      if (!published) {
        throw new Error(`PR #${pr.number} repair 推送后等待 head 可见超时。`);
      }
      console.log(
        `PR #${pr.number} repair 已推送 head ${published.headRefOid.slice(0, 12)}，等待新一轮 CI。`,
      );
      return published.headRefOid;
    },

    async rebaseOntoLatestBase(pr, checkedHeadOid) {
      const before = fetchPullRequest(pr.number);
      if (
        before.state !== "OPEN" ||
        before.isDraft ||
        before.baseRefName !== options.baseBranch ||
        before.headRefName !== pr.branch ||
        before.headRefOid !== checkedHeadOid
      ) {
        throw new Error(`PR #${pr.number} rebase 前状态或 head 已变化。`);
      }

      const update = tryCommand("gh", [
        "pr",
        "update-branch",
        String(pr.number),
        "--rebase",
      ]);
      if (update.ok) {
        const updated = fetchPullRequest(pr.number);
        console.log(
          `PR #${pr.number} 已基于最新 ${options.baseBranch} 更新。`,
        );
        return updated.headRefOid;
      }

      const afterFailure = fetchPullRequest(pr.number);
      if (afterFailure.headRefOid !== checkedHeadOid) {
        throw new Error(`PR #${pr.number} update-branch 失败后 head 已变化。`);
      }
      const issue = options.issuesByNumber.get(pr.issue);
      if (!issue) throw new Error(`找不到 PR #${pr.number} 对应的 Issue。`);
      console.warn(
        `PR #${pr.number} 无法由 GitHub 自动 rebase，转入冲突解决流程：${update.stderr || update.stdout}`,
      );
      return resolveRebaseWithAgent(
        pr,
        issue,
        options.baseBranch,
        checkedHeadOid,
        options.codexAuthFile,
        options.ghToken,
        options.agent,
        options.maxIterations,
        options.idleTimeoutSeconds,
      );
    },

    async mergeWithRebase(pr, checkedHeadOid) {
      const snapshot = fetchPullRequest(pr.number);
      if (
        snapshot.state !== "OPEN" ||
        snapshot.isDraft ||
        snapshot.headRefOid !== checkedHeadOid ||
        classifyCI(snapshot.statusCheckRollup) !== "passed"
      ) {
        throw new Error(`PR #${pr.number} merge 前状态、head 或 CI 已变化。`);
      }
      command("gh", [
        "pr",
        "merge",
        String(pr.number),
        "--rebase",
        "--delete-branch",
        "--match-head-commit",
        checkedHeadOid,
      ]);
    },

    async verifyMerged(pr) {
      const snapshot = fetchPullRequest(pr.number);
      if (snapshot.state !== "MERGED") {
        throw new Error(`PR #${pr.number} merge 后状态为 ${snapshot.state}。`);
      }
      command("git", ["fetch", "origin", options.baseBranch]);
      command("git", ["merge", "--ff-only", `origin/${options.baseBranch}`]);
      console.log(
        `PR #${pr.number} 已 rebase merge，local ${options.baseBranch} 已同步。`,
      );
    },
  };
}
