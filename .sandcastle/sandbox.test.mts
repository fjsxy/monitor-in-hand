import assert from "node:assert/strict";
import { resolve } from "node:path";
import test from "node:test";
import { createIssueSandboxProvider } from "./sandbox.mts";

test("GitHub token 在创建 Issue sandbox 时注入容器环境", () => {
  const ghToken = "test-github-token";
  const provider = createIssueSandboxProvider(ghToken, {
    hostPath: resolve("package.json"),
    sandboxPath: "/home/agent/.codex/auth.json",
  });

  assert.equal(provider.env.GH_TOKEN, ghToken);
});
