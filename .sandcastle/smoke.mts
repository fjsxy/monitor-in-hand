import { execFileSync } from "node:child_process";
import * as sandcastle from "@ai-hero/sandcastle";
import type { AgentProvider } from "@ai-hero/sandcastle";
import { resolveCodexAuthFile, stageCodexAuth } from "./auth.mts";
import { promptArgsForIssue } from "./orchestration.mts";
import { createIssueSandboxProvider } from "./sandbox.mts";

const branch = `codex/sandcastle-smoke-${process.pid}`;
const targetBranch = execFileSync("git", ["branch", "--show-current"], {
  encoding: "utf8",
}).trim();
const ghToken =
  process.env.GH_TOKEN ||
  execFileSync("gh", ["auth", "token"], {
    encoding: "utf8",
    stdio: ["ignore", "pipe", "pipe"],
  }).trim();
const stagedAuth = stageCodexAuth(
  resolveCodexAuthFile(process.env.CODEX_AUTH_FILE),
);
let sandbox: Awaited<ReturnType<typeof sandcastle.createSandbox>> | undefined;

const fakeAgent: AgentProvider = {
  name: "sandcastle-smoke",
  env: {},
  captureSessions: false,
  buildPrintCommand: ({ prompt }) => {
    const expectedDiff = `git diff ${targetBranch}...${branch}`;
    const expectedRebase = `rebase 到 \`${targetBranch}\` 时的冲突`;
    if (
      (!prompt.includes(expectedDiff) && !prompt.includes(expectedRebase)) ||
      prompt.includes("{{TARGET_BRANCH}}")
    ) {
      throw new Error("Sandcastle built-in prompt arguments were not substituted");
    }
    return { command: "printf '%s\\n' '<promise>COMPLETE</promise>'" };
  },
  parseStreamLine: (line) => [{ type: "text", text: line }],
};

function branchExists(): boolean {
  try {
    execFileSync("git", ["show-ref", "--verify", "--quiet", `refs/heads/${branch}`]);
    return true;
  } catch {
    return false;
  }
}

try {
  sandbox = await sandcastle.createSandbox({
    branch,
    sandbox: createIssueSandboxProvider(ghToken, stagedAuth.mount),
    hooks: {
      sandbox: {
        onSandboxReady: [
          {
            command:
              "codex login status && gh auth status && go version && helm version --short && codex --version",
          },
        ],
      },
    },
    copyToWorktree: [".agents"],
  });

  const result = await sandbox.exec(
    "codex login status 2>&1 && test \"$(gh issue view 51 --json number --jq .number)\" = 51 && git branch --show-current && id",
  );
  if (result.exitCode !== 0) {
    throw new Error(result.stderr || "sandbox smoke command failed");
  }

  const run = await sandbox.run({
    name: "prompt-argument-smoke",
    agent: fakeAgent,
    maxIterations: 1,
    promptFile: "./.sandcastle/review-prompt.md",
    promptArgs: promptArgsForIssue(
      { number: 0, title: "Sandcastle smoke" },
      branch,
    ),
  });
  if (!run.completionSignal) {
    throw new Error("Sandcastle prompt argument smoke did not complete");
  }

  const conflictRun = await sandbox.run({
    name: "merge-conflict-prompt-smoke",
    agent: fakeAgent,
    maxIterations: 1,
    promptFile: "./.sandcastle/merge-conflict-prompt.md",
    promptArgs: promptArgsForIssue(
      { number: 0, title: "Sandcastle smoke" },
      branch,
    ),
  });
  if (!conflictRun.completionSignal) {
    throw new Error("Sandcastle merge conflict prompt smoke did not complete");
  }

  process.stdout.write(result.stdout);
  console.log("Sandcastle smoke passed.");
} finally {
  try {
    if (sandbox) await sandbox.close();
    if (branchExists()) execFileSync("git", ["branch", "-D", branch]);
  } finally {
    stagedAuth.cleanup();
  }
}
