import {
  chmodSync,
  copyFileSync,
  existsSync,
  mkdtempSync,
  realpathSync,
  rmSync,
  statSync,
} from "node:fs";
import { homedir, tmpdir } from "node:os";
import { join } from "node:path";

const TEMP_PREFIX = "monitor-op-codex-auth-";

export interface StagedCodexAuth {
  mount: {
    hostPath: string;
    sandboxPath: string;
  };
  cleanup: () => void;
}

export function resolveCodexAuthFile(override?: string): string {
  const candidate = override || join(homedir(), ".codex", "auth.json");
  if (!existsSync(candidate) || !statSync(candidate).isFile()) {
    throw new Error(
      `找不到 Codex ChatGPT 登录缓存：${candidate}。请先在宿主机运行 codex login。`,
    );
  }
  return realpathSync(candidate);
}

export function stageCodexAuth(sourceFile: string): StagedCodexAuth {
  const authDir = mkdtempSync(join(tmpdir(), TEMP_PREFIX));
  const stagedFile = join(authDir, "auth.json");
  copyFileSync(sourceFile, stagedFile);
  chmodSync(stagedFile, 0o600);

  return {
    mount: {
      hostPath: stagedFile,
      sandboxPath: "/home/agent/.codex/auth.json",
    },
    cleanup: () => rmSync(authDir, { recursive: true, force: true }),
  };
}
