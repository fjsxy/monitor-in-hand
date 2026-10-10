import { createHmac, randomUUID } from "node:crypto";
import { appendFileSync, mkdirSync } from "node:fs";
import { resolve } from "node:path";
import { setTimeout as sleep } from "node:timers/promises";

export const milestoneLabels = {
  "e2e-plan": "E2E 计划发布",
  implementation: "实现完成",
  "local-e2e": "本地 E2E",
  "pull-request": "PR 创建完成",
  "pr-ci": "PR CI",
  merge: "完成合并",
} as const;

export type Milestone = keyof typeof milestoneLabels;
export interface MilestoneResult {
  issue: number;
  stage: Milestone;
  status: "succeeded" | "failed";
  durationMs: number;
  detail?: string;
  url?: string;
  headOid?: string;
}
export type MilestoneSink = (result: MilestoneResult) => void;

// 业务节点先完成自己的校验，再上报结果；通知故障不能变成业务失败。
export function reportMilestone(sink: MilestoneSink | undefined, result: MilestoneResult): void {
  try { sink?.(result); } catch { console.warn("节点通知上报失败，任务继续执行。"); }
}

export async function observeMilestone<T>(
  sink: MilestoneSink | undefined,
  issue: number,
  stage: Milestone,
  operation: () => Promise<T> | T,
  describe: (result: T) => Partial<Pick<MilestoneResult, "status" | "detail" | "url" | "headOid">> = () => ({}),
): Promise<T> {
  const started = Date.now();
  try {
    const value = await operation();
    reportMilestone(sink, { issue, stage, status: "succeeded", durationMs: Date.now() - started, ...describe(value) });
    return value;
  } catch (error) {
    reportMilestone(sink, { issue, stage, status: "failed", durationMs: Date.now() - started, detail: String(error) });
    throw error;
  }
}

export function resolveFeishuConfig(env: Record<string, string | undefined>, localEnv: Record<string, string>) {
  return {
    webhook: (env.SANDCASTLE_FEISHU_WEBHOOK || localEnv.SANDCASTLE_FEISHU_WEBHOOK)?.trim() || undefined,
    secret: (env.SANDCASTLE_FEISHU_SECRET || localEnv.SANDCASTLE_FEISHU_SECRET)?.trim() || undefined,
  };
}

interface NotificationOptions {
  webhook?: string;
  secret?: string;
  logDirectory: string;
  secrets?: string[];
}
interface NotificationDependencies {
  fetch?: typeof fetch;
  now?: () => number;
  sleep?: (ms: number) => Promise<unknown>;
  requestTimeoutMs?: number;
}

export function createFeishuNotifications(options: NotificationOptions, dependencies: NotificationDependencies = {}) {
  const webhook = options.webhook?.trim();
  const validWebhook = Boolean(webhook && /^https:\/\/open\.feishu\.cn\/open-apis\/bot\/v2\/hook\/[A-Za-z0-9-]+$/.test(webhook));
  const now = dependencies.now ?? Date.now;
  const pause = dependencies.sleep ?? sleep;
  const request = dependencies.fetch ?? fetch;
  const runId = randomUUID();
  const logPath = resolve(options.logDirectory, `notifications-${runId}.jsonl`);
  const hookToken = webhook?.match(/\/hook\/([^/?#]+)/)?.[1];
  const secrets = [webhook, hookToken, options.secret, ...(options.secrets ?? [])].filter((value): value is string => Boolean(value));
  const seen = new Set<string>();
  let tail = Promise.resolve();
  let lastRequestAt = -Infinity;
  let accepted = 0;
  let failed = 0;

  // 只发送短摘要和链接；剔除已知凭据及常见认证字段，原始 stdout 不作为通知附件。
  function redact(value: string): string {
    let clean = value;
    for (const secret of secrets) clean = clean.replaceAll(secret, "[REDACTED]");
    return clean
      .replace(/https:\/\/open\.feishu\.cn\/open-apis\/bot\/v2\/hook\/[^\s"'<>]+/gi, "[REDACTED]")
      .replace(/\b(?:gh[pousr]_[A-Za-z0-9_]+|github_pat_[A-Za-z0-9_]+)\b/g, "[REDACTED]")
      .replace(/((?:token|secret|password|api[_-]?key|authorization|access_token|refresh_token)\s*["']?\s*[:=]\s*["']?)(?:Bearer\s+)?[^\s,"'}&]+/gi, "$1[REDACTED]");
  }

  function record(value: object): void {
    try {
      mkdirSync(options.logDirectory, { recursive: true, mode: 0o700 });
      appendFileSync(logPath, JSON.stringify({ runId, time: new Date(now()).toISOString(), ...value }) + "\n", { mode: 0o600 });
    } catch {
      console.warn("飞书通知记录写入失败，任务继续执行。");
    }
  }

  function enqueue(id: string, title: string, content: string): void {
    if (!webhook || seen.has(id)) return;
    seen.add(id);
    // 使用纯文本短摘要；将标签括号转为全角，避免 agent 结果中的 at 标签触发群提醒。
    const text = `${redact(title).slice(0, 100)}\n\n${redact(content).slice(0, 3000)}`.replaceAll("<", "＜").replaceAll(">", "＞");
    const payload = { msg_type: "text", content: { text } };
    record({ id, state: "queued", ...payload });

    if (!validWebhook) {
      failed += 1;
      record({ id, state: "failed", detail: "SANDCASTLE_FEISHU_WEBHOOK 必须是飞书官方 HTTPS v2 hook 地址" });
      console.warn(`飞书 Webhook 配置无效，未发送通知；记录：${logPath}`);
      return;
    }

    // 并行 Issue 共用宿主队列；含重试的请求间隔至少 1 秒，低于飞书 100 次/分钟、5 次/秒的限制。
    tail = tail.then(async () => {
      for (let attempt = 1; attempt <= 3; attempt += 1) {
        try {
          const waitMs = Math.max(0, lastRequestAt + 1000 - now());
          if (waitMs) await pause(waitMs);
          lastRequestAt = now();
          // 飞书将 timestamp 和密钥拼成 HMAC key，对空消息签名；每次重试使用新的时间戳。
          const timestamp = String(Math.floor(now() / 1000));
          const signed = options.secret ? { timestamp, sign: createHmac("sha256", `${timestamp}\n${options.secret}`).digest("base64") } : {};
          const response = await request(webhook, {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ ...payload, ...signed }),
            redirect: "error",
            signal: AbortSignal.timeout(dependencies.requestTimeoutMs ?? 10_000),
          });
          if (!response.ok) throw new Error(`飞书 HTTP ${response.status}`);
          const body = await response.json() as { code?: number; msg?: string; StatusCode?: number; StatusMessage?: string };
          const code = body?.code ?? body?.StatusCode;
          if (code !== 0) throw new Error(`飞书业务码 ${code ?? "缺失"}：${body?.msg ?? body?.StatusMessage ?? "请求未被接受"}`);
          accepted += 1;
          // code=0 仅表示服务端接受请求；不能把它记录成手机已收到。
          record({ id, state: "accepted", attempt });
          return;
        } catch (error) {
          const detail = redact(String(error)).slice(0, 500);
          record({ id, state: attempt === 3 ? "failed" : "retry", attempt, detail });
          if (attempt === 3) {
            failed += 1;
            console.warn(`飞书通知发送失败（${id}）：${detail}；记录：${logPath}`);
          }
        }
      }
    }).catch(() => {
      failed += 1;
      record({ id, state: "failed", detail: "通知队列异常" });
      console.warn("飞书通知队列异常，任务继续执行。");
    });
  }

  const notify: MilestoneSink = (result) => {
    if (!Object.hasOwn(milestoneLabels, result.stage)) return;
    const label = milestoneLabels[result.stage];
    const status = result.status === "succeeded" ? "成功" : "失败";
    enqueue(`${result.issue}:${result.stage}`, `Sandcastle #${result.issue}｜${label}｜${status}`, [
      `Issue：#${result.issue}`,
      `节点：${label}`,
      `结果：${status}`,
      `时间：${new Date(now()).toISOString()}`,
      `耗时：${(result.durationMs / 1000).toFixed(1)} 秒`,
      result.headOid ? `head：${result.headOid}` : "",
      result.detail ? `说明：${redact(result.detail).slice(0, 1500)}` : "",
      result.url ? `链接：${result.url}` : "",
    ].filter(Boolean).join("\n\n"));
  };

  return {
    notify,
    enabled: Boolean(webhook),
    logPath,
    sendTest() {
      enqueue("manual-test", "Sandcastle 飞书通知测试", "这是一条手动发送的测试消息。自动任务仅通知 E2E 计划发布、实现完成、本地 E2E、PR 创建、PR CI 和完成合并六类节点。");
    },
    async flush() { await tail; return { accepted, failed }; },
  };
}
