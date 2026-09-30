import { spawn } from "node:child_process";
import { resolve } from "node:path";

interface GateOptions {
  testRunId: string;
  logPath: string;
  archivePath: string;
}

export interface HostProcessOptions {
  command: string;
  args: string[];
  cwd: string;
  onLine?: (line: string) => void;
}

export type HostProcessRunner = (
  options: HostProcessOptions,
) => Promise<{ exitCode: number; stdout: string; stderr: string }>;

export interface SerialE2EGate {
  run<T>(operation: () => Promise<T>): Promise<T>;
}

export interface LocalE2EGateResult {
  exitCode: number;
  stdout: string;
  stderr: string;
  logPath: string;
  archivePath: string;
}

export interface LocalE2EOperations {
  runGate(): Promise<LocalE2EGateResult>;
  repairFailure(failure: LocalE2EGateResult, attempt: number): Promise<void>;
  reviewRepair(attempt: number): Promise<void>;
}

function assertDNSLabel(value: string): void {
  if (
    value.length > 32 ||
    !/^[a-z0-9](?:[-a-z0-9]*[a-z0-9])?$/.test(value)
  ) {
    throw new Error(`本地 E2E TEST_RUN_ID 不是合法 DNS label：${value}`);
  }
}

function shellQuote(value: string): string {
  return `'${value.replaceAll("'", `'"'"'`)}'`;
}

export function localE2EGateCommand(options: GateOptions): string {
  assertDNSLabel(options.testRunId);
  const logPath = shellQuote(options.logPath);
  const archivePath = shellQuote(options.archivePath);
  const evidencePath = shellQuote(`${options.archivePath}/test-env`);
  const e2eArchivePath = shellQuote(`${options.archivePath}/e2e`);
  const archivedLogPath = shellQuote(`${options.archivePath}/gate.log`);

  return [
    "set -Eeuo pipefail",
    `export KUBERNETES_VERSION=1.28`,
    `export TEST_RUN_ID=${options.testRunId}`,
    `mkdir -p $(dirname ${logPath})`,
    `exec > >(tee ${logPath}) 2>&1`,
    "cleanup() {",
    "  status=$?",
    "  trap - EXIT INT TERM",
    "  if (( status != 0 )); then",
    `    mkdir -p ${archivePath}`,
    `    TEST_ENV_EVIDENCE_DIR=${evidencePath} make test-env-evidence || true`,
    `    cp ${logPath} ${archivedLogPath} || true`,
    '    if [[ -d "artifacts/e2e/${TEST_RUN_ID}-1.28" ]]; then',
    `      cp -R "artifacts/e2e/\${TEST_RUN_ID}-1.28" ${e2eArchivePath} || true`,
    "    fi",
    "  fi",
    "  cleanup_status=0",
    "  make test-env-down || cleanup_status=$?",
    "  if (( status == 0 && cleanup_status != 0 )); then status=$cleanup_status; fi",
    "  exit $status",
    "}",
    "trap cleanup EXIT INT TERM",
    "docker info",
    "make test-env-up",
    "make test-env-verify",
    "make e2e-kubestatemetrics HELM=helm",
  ].join("\n");
}

function appendTail(current: string, chunk: string): string {
  const combined = current + chunk;
  return combined.length <= 64 * 1024 ? combined : combined.slice(-64 * 1024);
}

const runHostProcess: HostProcessRunner = (options) =>
  new Promise((resolve, reject) => {
    const child = spawn(options.command, options.args, {
      cwd: options.cwd,
      env: process.env,
      stdio: ["ignore", "pipe", "pipe"],
    });
    let stdout = "";
    let stderr = "";
    let lineBuffer = "";

    const streamLines = (chunk: string): void => {
      if (!options.onLine) return;
      lineBuffer += chunk;
      const lines = lineBuffer.split("\n");
      lineBuffer = lines.pop() ?? "";
      for (const line of lines) options.onLine(line);
    };

    child.stdout.on("data", (chunk: Buffer) => {
      const text = chunk.toString();
      stdout = appendTail(stdout, text);
      streamLines(text);
    });
    child.stderr.on("data", (chunk: Buffer) => {
      const text = chunk.toString();
      stderr = appendTail(stderr, text);
      streamLines(text);
    });
    child.on("error", reject);
    child.on("close", (code) => {
      if (lineBuffer && options.onLine) options.onLine(lineBuffer);
      resolve({ exitCode: code ?? 1, stdout, stderr });
    });
  });

export async function runLocalE2EGate(
  worktreePath: string,
  testRunId: string,
  onLine?: (line: string) => void,
  runner: HostProcessRunner = runHostProcess,
): Promise<LocalE2EGateResult> {
  const logPath = `.sandcastle/logs/local-e2e-${testRunId}.log`;
  const archivePath = resolve(
    `.sandcastle/logs/local-e2e-${testRunId}`,
  );
  const script = localE2EGateCommand({ testRunId, logPath, archivePath });
  const result = await runner({
    command: "bash",
    args: ["-c", script],
    cwd: worktreePath,
    onLine,
  });
  return { ...result, logPath, archivePath };
}

export function createSerialE2EGate(): SerialE2EGate {
  let tail = Promise.resolve<void>(undefined);

  return {
    run<T>(operation: () => Promise<T>): Promise<T> {
      const result = tail.then(operation, operation);
      tail = result.then(
        () => undefined,
        () => undefined,
      );
      return result;
    },
  };
}

export async function ensureLocalE2E(
  operations: LocalE2EOperations,
  maxRepairAttempts: number,
): Promise<void> {
  let repairAttempt = 0;

  while (true) {
    const result = await operations.runGate();
    if (result.exitCode === 0) return;

    if (repairAttempt >= maxRepairAttempts) {
      const detail =
        result.stderr.trim() ||
        result.stdout.trim() ||
        `详见 ${result.logPath}`;
      throw new Error(
        `本地 E2E gate 失败：${detail}\n证据已归档：${result.archivePath}`,
      );
    }

    repairAttempt += 1;
    await operations.repairFailure(result, repairAttempt);
    await operations.reviewRepair(repairAttempt);
  }
}
