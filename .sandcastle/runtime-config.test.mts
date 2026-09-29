import assert from "node:assert/strict";
import test from "node:test";
import {
  resolveAgentIdleTimeoutSeconds,
  resolveMaxCIRepairAttempts,
  withAgentIdleTimeout,
} from "./runtime-config.mts";

test("agent idle timeout 默认覆盖 high reasoning 的长静默窗口", () => {
  assert.equal(resolveAgentIdleTimeoutSeconds({}, {}), 1_800);
});

test("agent idle timeout 支持进程环境覆盖并拒绝非正整数", () => {
  assert.equal(
    resolveAgentIdleTimeoutSeconds(
      { SANDCASTLE_IDLE_TIMEOUT_SECONDS: "2400" },
      { SANDCASTLE_IDLE_TIMEOUT_SECONDS: "2100" },
    ),
    2_400,
  );
  assert.equal(
    resolveAgentIdleTimeoutSeconds(
      { SANDCASTLE_IDLE_TIMEOUT_SECONDS: "invalid" },
      {},
    ),
    1_800,
  );
});

test("所有 agent run options 注入同一 idle timeout", () => {
  assert.deepEqual(withAgentIdleTimeout(1_800, { maxIterations: 3 }), {
    maxIterations: 3,
    idleTimeoutSeconds: 1_800,
  });
});

test("CI 自动修复默认最多两次，并支持进程环境覆盖", () => {
  assert.equal(resolveMaxCIRepairAttempts({}, {}), 2);
  assert.equal(
    resolveMaxCIRepairAttempts(
      { SANDCASTLE_MAX_CI_REPAIR_ATTEMPTS: "4" },
      { SANDCASTLE_MAX_CI_REPAIR_ATTEMPTS: "3" },
    ),
    4,
  );
  assert.equal(
    resolveMaxCIRepairAttempts(
      { SANDCASTLE_MAX_CI_REPAIR_ATTEMPTS: "invalid" },
      {},
    ),
    2,
  );
  assert.equal(
    resolveMaxCIRepairAttempts(
      { SANDCASTLE_MAX_CI_REPAIR_ATTEMPTS: "0" },
      {},
    ),
    0,
  );
});
