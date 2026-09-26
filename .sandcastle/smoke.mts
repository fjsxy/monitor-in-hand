import { execFileSync } from "node:child_process";
import * as sandcastle from "@ai-hero/sandcastle";
import { docker } from "@ai-hero/sandcastle/sandboxes/docker";
import { resolveCodexAuthFile, stageCodexAuth } from "./auth.mts";

const branch = `codex/sandcastle-smoke-${process.pid}`;
const stagedAuth = stageCodexAuth(
  resolveCodexAuthFile(process.env.CODEX_AUTH_FILE),
);
let sandbox: Awaited<ReturnType<typeof sandcastle.createSandbox>> | undefined;

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
    sandbox: docker({
      imageName: "sandcastle:monitor-op",
      mounts: [stagedAuth.mount],
    }),
    hooks: {
      sandbox: {
        onSandboxReady: [
          {
            command:
              "codex login status && go version && helm version --short && codex --version",
          },
        ],
      },
    },
    copyToWorktree: [".agents"],
  });

  const result = await sandbox.exec(
    "codex login status 2>&1 && git branch --show-current && id",
  );
  if (result.exitCode !== 0) {
    throw new Error(result.stderr || "sandbox smoke command failed");
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
