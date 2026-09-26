import assert from "node:assert/strict";
import {
  existsSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  statSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { resolveCodexAuthFile, stageCodexAuth } from "./auth.mts";

test("ChatGPT 登录缓存只通过权限收紧的临时副本挂载", () => {
  const sourceDir = mkdtempSync(join(tmpdir(), "monitor-op-auth-source-"));
  const sourceFile = join(sourceDir, "auth.json");
  writeFileSync(sourceFile, '{"token":"test-only"}', { mode: 0o600 });

  try {
    const resolved = resolveCodexAuthFile(sourceFile);
    const staged = stageCodexAuth(resolved);

    try {
      assert.equal(staged.mount.sandboxPath, "/home/agent/.codex/auth.json");
      assert.equal(readFileSync(staged.mount.hostPath, "utf8"), '{"token":"test-only"}');
      assert.equal(statSync(staged.mount.hostPath).mode & 0o777, 0o600);
    } finally {
      const stagedFile = staged.mount.hostPath;
      staged.cleanup();
      assert.equal(existsSync(stagedFile), false);
    }
  } finally {
    rmSync(sourceDir, { recursive: true, force: true });
  }
});

test("缺少 ChatGPT 登录缓存时拒绝启动", () => {
  assert.throws(
    () => resolveCodexAuthFile(join(tmpdir(), "missing-codex-auth.json")),
    /找不到 Codex ChatGPT 登录缓存/,
  );
});
