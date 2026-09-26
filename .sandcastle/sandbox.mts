import { docker } from "@ai-hero/sandcastle/sandboxes/docker";
import type { StagedCodexAuth } from "./auth.mts";

const IMAGE_NAME = "sandcastle:monitor-op";

export function createIssueSandboxProvider(
  ghToken: string,
  authMount: StagedCodexAuth["mount"],
) {
  return docker({
    imageName: IMAGE_NAME,
    mounts: [authMount],
    env: { GH_TOKEN: ghToken },
  });
}
