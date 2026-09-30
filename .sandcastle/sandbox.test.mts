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

test("Issue sandbox 不获得宿主 Docker 或远程 daemon 访问能力", () => {
  const provider = createIssueSandboxProvider("test-github-token", {
    hostPath: resolve("package.json"),
    sandboxPath: "/home/agent/.codex/auth.json",
  }) as typeof createIssueSandboxProvider extends (...args: never[]) => infer Result
    ? Result & {
        mounts?: ReadonlyArray<{ hostPath: string; sandboxPath: string }>;
        network?: string | readonly string[];
      }
    : never;

  assert.equal("DOCKER_HOST" in provider.env, false);
  assert.equal(provider.network, undefined);
  assert.equal(
    (provider.mounts ?? []).some(
      (mount) =>
        mount.hostPath.includes("docker.sock") ||
        mount.sandboxPath.includes("docker.sock"),
    ),
    false,
  );
});
