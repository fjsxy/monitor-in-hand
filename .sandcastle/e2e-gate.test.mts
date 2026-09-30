import assert from "node:assert/strict";
import test from "node:test";
import {
  createSerialE2EGate,
  ensureLocalE2E,
  localE2EGateCommand,
  runLocalE2EGate,
} from "./e2e-gate.mts";

test("本地 E2E gate 使用 1.28、保存失败 evidence 并无条件清理集群", () => {
  const command = localE2EGateCommand({
    testRunId: "sc-51-abc12345",
    logPath: ".sandcastle/logs/local-e2e-sc-51-abc12345.log",
    archivePath: "/repo/.sandcastle/logs/local-e2e-sc-51-abc12345",
  });

  assert.match(command, /KUBERNETES_VERSION=1\.28/);
  assert.match(command, /TEST_RUN_ID=sc-51-abc12345/);
  assert.match(command, /make test-env-up/);
  assert.match(command, /make test-env-verify/);
  assert.match(command, /make e2e-kubestatemetrics HELM=helm/);
  assert.match(command, /make test-env-evidence/);
  assert.match(command, /TEST_ENV_EVIDENCE_DIR/);
  assert.match(command, /artifacts\/e2e\/\$\{TEST_RUN_ID\}-1\.28/);
  assert.match(command, /gate\.log/);
  assert.match(command, /make test-env-down/);
  assert.match(command, /trap cleanup EXIT INT TERM/);
});

test("并发提交的本地 E2E gate 严格串行执行", async () => {
  const gate = createSerialE2EGate();
  const events: string[] = [];
  let releaseFirst: (() => void) | undefined;
  const firstCanFinish = new Promise<void>((resolve) => {
    releaseFirst = resolve;
  });

  const first = gate.run(async () => {
    events.push("first:start");
    await firstCanFinish;
    events.push("first:end");
    return "first";
  });
  const second = gate.run(async () => {
    events.push("second:start");
    events.push("second:end");
    return "second";
  });

  await Promise.resolve();
  assert.deepEqual(events, ["first:start"]);
  releaseFirst!();
  assert.deepEqual(await Promise.all([first, second]), ["first", "second"]);
  assert.deepEqual(events, [
    "first:start",
    "first:end",
    "second:start",
    "second:end",
  ]);
});

test("前一个本地 E2E gate 失败后仍允许后一个执行", async () => {
  const gate = createSerialE2EGate();
  const events: string[] = [];

  const failed = gate.run(async () => {
    events.push("failed");
    throw new Error("boom");
  });
  const succeeded = gate.run(async () => {
    events.push("succeeded");
    return 42;
  });

  await assert.rejects(failed, /boom/);
  assert.equal(await succeeded, 42);
  assert.deepEqual(events, ["failed", "succeeded"]);
});

test("本地 E2E 首次失败后执行 sandbox 修复、复核并重跑 gate", async () => {
  const events: string[] = [];
  let gateAttempt = 0;

  await ensureLocalE2E(
    {
      async runGate() {
        gateAttempt += 1;
        events.push(`gate:${gateAttempt}`);
        return {
          exitCode: gateAttempt === 1 ? 1 : 0,
          stdout: "",
          stderr: gateAttempt === 1 ? "failed" : "",
          logPath: ".sandcastle/logs/local-e2e.log",
          archivePath: "/repo/.sandcastle/logs/local-e2e",
        };
      },
      async repairFailure(_failure, attempt) {
        events.push(`repair:${attempt}`);
      },
      async reviewRepair(attempt) {
        events.push(`review:${attempt}`);
      },
    },
    2,
  );

  assert.deepEqual(events, ["gate:1", "repair:1", "review:1", "gate:2"]);
});

test("本地 E2E 修复预算耗尽后阻止发布", async () => {
  const events: string[] = [];

  await assert.rejects(
    ensureLocalE2E(
      {
        async runGate() {
          events.push("gate");
          return {
            exitCode: 1,
            stdout: "",
            stderr: "still failing",
            logPath: ".sandcastle/logs/local-e2e.log",
            archivePath: "/repo/.sandcastle/logs/local-e2e",
          };
        },
        async repairFailure(_failure, attempt) {
          events.push(`repair:${attempt}`);
        },
        async reviewRepair(attempt) {
          events.push(`review:${attempt}`);
        },
      },
      1,
    ),
    /still failing/,
  );
  assert.deepEqual(events, ["gate", "repair:1", "review:1", "gate"]);
});

test("本地 E2E 在 sandbox worktree 的宿主路径执行并返回日志位置", async () => {
  let received: { command: string; args: string[]; cwd: string } | undefined;
  let streamed = "";
  const result = await runLocalE2EGate(
    "/tmp/issue-51-worktree",
    "sc-51-abc12345",
    (line) => {
      streamed = line;
    },
    async (options) => {
      received = {
        command: options.command,
        args: options.args,
        cwd: options.cwd,
      };
      options.onLine?.("gate output");
      return { exitCode: 1, stdout: "tail", stderr: "failure" };
    },
  );

  assert.equal(received?.command, "bash");
  assert.equal(received?.args[0], "-c");
  assert.match(received?.args[1] ?? "", /make e2e-kubestatemetrics/);
  assert.equal(received?.cwd, "/tmp/issue-51-worktree");
  assert.equal(streamed, "gate output");
  assert.deepEqual(result, {
    exitCode: 1,
    stdout: "tail",
    stderr: "failure",
    logPath: ".sandcastle/logs/local-e2e-sc-51-abc12345.log",
    archivePath: `${process.cwd()}/.sandcastle/logs/local-e2e-sc-51-abc12345`,
  });
});
