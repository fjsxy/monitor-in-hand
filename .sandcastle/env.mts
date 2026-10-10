import { existsSync, readFileSync } from "node:fs";
import { resolve } from "node:path";

export function readLocalEnv(repoRoot = process.cwd()): Record<string, string> {
  const envPath = resolve(repoRoot, ".sandcastle/.env");
  if (!existsSync(envPath)) return {};
  return Object.fromEntries(
    readFileSync(envPath, "utf8")
      .split("\n")
      .map((line) => line.trim())
      .filter((line) => line && !line.startsWith("#") && line.includes("="))
      .map((line) => {
        const separator = line.indexOf("=");
        return [line.slice(0, separator).trim(), line.slice(separator + 1).trim().replace(/^(['"])(.*)\1$/, "$2")];
      }),
  );
}
