import assert from "node:assert/strict";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { createServer } from "node:http";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { ensureLocalE2E } from "./e2e-gate.mts";
import type { E2EPlanComment, E2EPlanOperations } from "./e2e-plan.mts";
import { publishNotifiedPullRequest } from "./github-pr.mts";
import { runNotifiedImplementation } from "./milestones.mts";
import { mergeReviewedPullRequests, PullRequestCIFailedError, type MergeOperations, type ReviewedPullRequest } from "./merge.mts";
import { createFeishuNotifications, milestoneLabels, observeMilestone, resolveFeishuConfig, type MilestoneResult } from "./notifications.mts";

const issue = { number: 85, body: "通知六个节点", title: "节点通知" };
const header = "| 场景 ID | 来源 | 层级 | 目标版本 | 前置条件 | 验收目的 | 执行步骤 | 执行预期 | 失败证据 | 测试入口 |";
const plan = ["## E2E", header, "| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |", "", "## 无相关 E2E 的依据", "", "仅改变宿主通知，使用 Node 集成测试验证。"].join("\n");
const pr: ReviewedPullRequest = { issue: 85, number: 185, branch: "codex/issue-85", url: "https://github.com/fjsxy/monitor-in-hand/pull/185" };
const complete = { completionSignal: "COMPLETE", commits: ["commit"] };
const webhook = "https://open.feishu.cn/open-apis/bot/v2/hook/fake-hook-token";

function planning(events: string[], failure?: string): E2EPlanOperations {
  let comment: E2EPlanComment;
  return {
    async listComments() { return []; },
    async draftPlan() { return plan; },
    async publishComment(body) {
      events.push("publish-plan");
      comment = { id: 1, body, url: "https://github.com/fjsxy/monitor-in-hand/issues/85#issuecomment-1" };
      return comment;
    },
    async readComment() {
      events.push("read-plan");
      if (failure) throw new Error(failure);
      return comment;
    },
  };
}

function publishing(events: string[], snapshot = {}) {
  return {
    existingPullRequest() { return undefined; },
    command(name: string, args: string[]) {
      events.push(`${name}:${args.slice(0, 2).join(":")}`);
      if (args[1] === "view") return JSON.stringify({ number: pr.number, url: pr.url, state: "OPEN", isDraft: false, headRefName: pr.branch, baseRefName: "main", ...snapshot });
      return "";
    },
  };
}

function merging(events: string[], overrides: Partial<MergeOperations> = {}): MergeOperations {
  return {
    async waitForCI() { events.push("ci"); return "head-final"; },
    async rebaseOntoLatestBase(_pr, head) { events.push("rebase"); return head; },
    async mergeWithRebase() { events.push("merge"); },
    async verifyMerged() { events.push("read-merged"); },
    ...overrides,
  };
}

test("UT-NOTIFY-GH85-001 配置关闭、六类范围、去重与凭据脱敏", async (t) => {
  // 准备独立日志与时钟，保证测试不会访问真实飞书。
  const directory = mkdtempSync(join(tmpdir(), "sc-notify-"));
  t.after(() => rmSync(directory, { recursive: true, force: true }));
  assert.deepEqual(resolveFeishuConfig({ SANDCASTLE_FEISHU_WEBHOOK: " env ", SANDCASTLE_FEISHU_SECRET: " env-secret " }, { SANDCASTLE_FEISHU_WEBHOOK: "local", SANDCASTLE_FEISHU_SECRET: "local-secret" }), { webhook: "env", secret: "env-secret" });
  assert.deepEqual(resolveFeishuConfig({}, { SANDCASTLE_FEISHU_WEBHOOK: webhook }), { webhook, secret: undefined });
  const disabled = createFeishuNotifications({ logDirectory: directory }, { fetch: async () => { throw new Error("不应发送"); } });
  disabled.notify({ issue: 85, stage: "merge", status: "succeeded", durationMs: 0 });
  assert.deepEqual(await disabled.flush(), { accepted: 0, failed: 0 });

  const messages: string[] = [];
  let clock = 0;
  const notifications = createFeishuNotifications({ webhook, secret: "fake-sign-secret", secrets: ["fake-gh-secret"], logDirectory: directory }, {
    now: () => clock, sleep: async (ms) => { clock += ms; },
    fetch: async (_url, request) => {
      messages.push(String(request?.body));
      return Response.json({ code: 0, msg: "success" });
    },
  });
  // 执行同一节点重复、越界和含凭据的上报，验证可观察输出。
  const result: MilestoneResult = { issue: 85, stage: "merge", status: "succeeded", durationMs: 1000, detail: `${webhook} fake-hook-token fake-sign-secret fake-gh-secret ghp_fakecredential password=secret-value` };
  notifications.notify(result);
  notifications.notify({ ...result, status: "failed" });
  notifications.notify({ ...result, stage: "review" as MilestoneResult["stage"] });
  assert.deepEqual(await notifications.flush(), { accepted: 1, failed: 0 });
  const body = JSON.parse(messages[0]!);
  assert.equal(body.msg_type, "text");
  assert.match(body.content.text, /Sandcastle #85/);
  assert.doesNotMatch(body.content.text, /fake-hook-token|fake-sign-secret|fake-gh-secret|ghp_fakecredential|secret-value/);
  assert.doesNotMatch(readFileSync(notifications.logPath, "utf8"), /fake-hook-token|fake-sign-secret|fake-gh-secret|ghp_fakecredential|secret-value/);
  assert.equal(Object.keys(milestoneLabels).length, 6);
});

test("IT-NOTIFY-GH85-001 六个成功结果均在真实验收边界之后发送", async () => {
  // 准备 GitHub、implementer、本地 gate 和 merge 的受控边界。
  const events: string[] = [];
  const notifications: MilestoneResult[] = [];
  const notify = (result: MilestoneResult) => { notifications.push(result); events.push(`notify:${result.stage}`); };
  await runNotifiedImplementation(issue, plan, planning(events), async () => { events.push("implement"); return complete; }, notify);
  await observeMilestone(notify, 85, "local-e2e", () => ensureLocalE2E({
    async runGate() { events.push("gate"); return { exitCode: 0, stdout: "", stderr: "", logPath: "gate.log", archivePath: "logs" }; },
    async repairFailure() { throw new Error("不应修复"); }, async reviewRepair() { throw new Error("不应评审"); },
  }, 2));
  const published = await publishNotifiedPullRequest(issue, pr.branch, "main", publishing(events), notify);
  const merged = await mergeReviewedPullRequests([published], merging(events), notify);

  // 检查事件顺序与链接，而不是只检查六个字符串是否存在。
  assert.deepEqual(events, ["publish-plan", "read-plan", "notify:e2e-plan", "implement", "notify:implementation", "gate", "notify:local-e2e", "git:push:--set-upstream", "gh:pr:create", "gh:pr:view", "notify:pull-request", "ci", "rebase", "notify:pr-ci", "merge", "read-merged", "notify:merge"]);
  assert.deepEqual(notifications.map(({ stage, status }) => [stage, status]), Object.keys(milestoneLabels).map((stage) => [stage, "succeeded"]));
  assert.equal(notifications[0]!.url?.includes("issuecomment"), true);
  assert.equal(notifications[3]!.url, pr.url);
  assert.equal(notifications[4]!.headOid, "head-final");
  assert.equal(merged[0]!.status, "merged");
});

test("IT-NOTIFY-GH85-002 指定节点失败独立报告，未执行及其他节点不通知", async (t) => {
  await t.test("计划回读失败阻止实现", async () => {
    const notifications: MilestoneResult[] = [];
    await assert.rejects(runNotifiedImplementation(issue, plan, planning([], "回读失败"), async () => { throw new Error("不应实现"); }, (r) => notifications.push(r)), /回读失败/);
    assert.deepEqual(notifications.map((r) => [r.stage, r.status]), [["e2e-plan", "failed"]]);
  });
  await t.test("实现缺少信号、commit 或抛错不误报计划失败", async () => {
    for (const outcome of [{ completionSignal: undefined, commits: [] }, { completionSignal: "COMPLETE", commits: [] }, new Error("agent 超时")]) {
      const notifications: MilestoneResult[] = [];
      const operation = runNotifiedImplementation(issue, plan, planning([]), async () => { if (outcome instanceof Error) throw outcome; return outcome; }, (r) => notifications.push(r));
      if (outcome instanceof Error) await assert.rejects(operation, /超时/); else assert.equal(await operation, outcome);
      assert.deepEqual(notifications.map((r) => [r.stage, r.status]), [["e2e-plan", "succeeded"], ["implementation", "failed"]]);
    }
  });
  await t.test("本地 gate 失败保留原始错误", async () => {
    const notifications: MilestoneResult[] = [];
    const failure = new Error("本地 E2E 失败");
    await assert.rejects(observeMilestone((r) => notifications.push(r), 85, "local-e2e", () => { throw failure; }), (error) => error === failure);
    assert.equal(notifications[0]!.status, "failed");
  });
  await t.test("PR 回读拒绝错误状态与分支", async () => {
    for (const snapshot of [{ state: "CLOSED" }, { isDraft: true }, { headRefName: "other" }, { baseRefName: "other" }, { url: "" }, { number: 0 }]) {
      const notifications: MilestoneResult[] = [];
      await assert.rejects(publishNotifiedPullRequest(issue, pr.branch, "main", publishing([], snapshot), (r) => notifications.push(r)));
      assert.deepEqual(notifications.map((r) => [r.stage, r.status]), [["pull-request", "failed"]]);
    }
  });
  await t.test("CI 和合并失败停队列；后续未执行的 PR 没有通知", async () => {
    for (const failAt of ["waitForCI", "mergeWithRebase", "verifyMerged"] as const) {
      const notifications: MilestoneResult[] = [];
      const results = await mergeReviewedPullRequests([pr, { ...pr, issue: 86, number: 186 }], merging([], { [failAt]: async () => { throw new Error("拒绝"); } }), (r) => notifications.push(r));
      assert.deepEqual(results.map((r) => r.status), ["blocked", "not-attempted"]);
      assert.deepEqual(notifications.map((r) => [r.stage, r.status]), failAt === "waitForCI" ? [["pr-ci", "failed"]] : [["pr-ci", "succeeded"], ["merge", "failed"]]);
    }
  });
  await t.test("rebase 失败不伪造合并节点；通知回调失败不阻断业务", async () => {
    const notifications: MilestoneResult[] = [];
    await mergeReviewedPullRequests([pr], merging([], { async rebaseOntoLatestBase() { throw new Error("resolver 失败"); } }), (r) => notifications.push(r));
    assert.deepEqual(notifications, []);
    const results = await mergeReviewedPullRequests([pr], merging([]), () => { throw new Error("通知故障"); });
    assert.equal(results[0]!.status, "merged");
  });
  await t.test("远端已确认合并后本地同步失败，合并通知仍然成功", async () => {
    const notifications: MilestoneResult[] = [];
    const results = await mergeReviewedPullRequests([pr], merging([], { async synchronizeBase() { throw new Error("本地同步失败"); } }), (r) => notifications.push(r));
    assert.equal(results[0]!.status, "blocked");
    assert.deepEqual(notifications.map((r) => [r.stage, r.status]), [["pr-ci", "succeeded"], ["merge", "succeeded"]]);
  });
});

test("IT-NOTIFY-GH85-003 本地和 CI 修复不单独通知，最终 CI 对应 rebase 后 head", async () => {
  const notifications: MilestoneResult[] = [];
  const notify = (result: MilestoneResult) => { notifications.push(result); };
  let gates = 0;
  await observeMilestone(notify, 85, "local-e2e", () => ensureLocalE2E({
    async runGate() { return { exitCode: gates++ ? 0 : 1, stdout: "", stderr: "失败", logPath: "gate.log", archivePath: "logs" }; },
    async repairFailure() {}, async reviewRepair() {},
  }, 2));
  let waits = 0;
  const results = await mergeReviewedPullRequests([pr], merging([], {
    maxCIRepairAttempts: 1,
    async waitForCI() {
      waits += 1;
      if (waits === 1) throw new PullRequestCIFailedError(185, "original", ["quality"]);
      return waits === 2 ? "repaired" : "rebased";
    },
    async repairFailedCI() { return "repaired"; },
    async rebaseOntoLatestBase() { return "rebased"; },
    async mergeWithRebase(_pr, head) { assert.equal(head, "rebased"); },
  }), notify);
  assert.equal(results[0]!.status, "merged");
  assert.deepEqual(notifications.map((r) => r.stage), ["local-e2e", "pr-ci", "merge"]);
  assert.equal(notifications[1]!.headOid, "rebased");

  // rebase 后 CI 最终失败时，不能提前发送旧 head 的成功通知。
  notifications.length = 0;
  waits = 0;
  await mergeReviewedPullRequests([pr], merging([], {
    async waitForCI() { if (waits++) throw new PullRequestCIFailedError(185, "rebased", ["quality"]); return "original"; },
    async rebaseOntoLatestBase() { return "rebased"; },
  }), notify);
  assert.deepEqual(notifications.map((r) => [r.stage, r.status]), [["pr-ci", "failed"]]);
});

test("IT-NOTIFY-GH85-004 并行事件的 HTTP 限速、业务码、超时和故障隔离", async (t) => {
  const directory = mkdtempSync(join(tmpdir(), "sc-notify-http-"));
  t.after(() => rmSync(directory, { recursive: true, force: true }));
  const requests: { time: number; body: string }[] = [];
  let clock = 0;
  // 使用真实本地 HTTP 响应，验证 JSON 业务码；仅将固定生产 URL 转向测试服务器。
  const server = createServer((req, res) => {
    let body = "";
    req.on("data", (data) => { body += data; });
    req.on("end", () => {
      requests.push({ time: clock, body });
      res.setHeader("Content-Type", "application/json");
      res.end(JSON.stringify(requests.length === 1 ? { code: 11232, msg: `暂时失败 ${webhook}` } : requests.length === 2 ? { StatusCode: 0, StatusMessage: "success" } : { code: 0, msg: "success" }));
    });
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  t.after(() => { server.closeAllConnections(); server.close(); });
  const address = server.address();
  assert.ok(address && typeof address !== "string");
  const notifications = createFeishuNotifications({ webhook, logDirectory: directory }, {
    now: () => clock, sleep: async (ms) => { clock += ms; },
    fetch: (url, request) => {
      assert.equal(url, webhook);
      assert.equal(request?.redirect, "error");
      const payload = JSON.parse(String(request?.body));
      assert.equal(payload.msg_type, "text");
      assert.match(payload.content.text, /Sandcastle #85/);
      assert.equal(payload.sign, undefined);
      assert.equal(payload.timestamp, undefined);
      return fetch(`http://127.0.0.1:${address.port}/send`, request);
    },
  });
  for (const stage of Object.keys(milestoneLabels) as MilestoneResult["stage"][]) notifications.notify({ issue: 85, stage, status: "succeeded", durationMs: 0 });
  assert.deepEqual(await notifications.flush(), { accepted: 6, failed: 0 });
  assert.equal(requests.length, 7);
  assert.ok(requests.every((r, index) => index === 0 || r.time - requests[index - 1]!.time >= 1000));
  assert.doesNotMatch(readFileSync(notifications.logPath, "utf8"), /fake-hook-token/);

  // 请求必须能被超时信号终止；重试预算耗尽仍不能改变已成功的业务结果。
  let attempts = 0;
  const broken = createFeishuNotifications({ webhook, logDirectory: directory }, {
    now: () => clock, sleep: async (ms) => { clock += ms; }, requestTimeoutMs: 5,
    fetch: async (_url, request) => {
      attempts += 1;
      return new Promise<Response>((_resolve, reject) => request!.signal!.addEventListener("abort", () => reject(new Error(`超时 ${webhook}`)), { once: true }));
    },
  });
  const business = await observeMilestone(broken.notify, 86, "merge", () => "业务成功");
  // AbortSignal 的内部定时器不保持 Node 进程存活，测试用有界活跃句柄模拟真实网络请求。
  const keepAlive = setTimeout(() => {}, 1000);
  try { assert.deepEqual(await broken.flush(), { accepted: 0, failed: 1 }); } finally { clearTimeout(keepAlive); }
  assert.equal(business, "业务成功");
  assert.equal(attempts, 3);
  assert.match(readFileSync(broken.logPath, "utf8"), /"state":"failed"/);
  assert.doesNotMatch(readFileSync(broken.logPath, "utf8"), /fake-hook-token/);
});

test("UT-NOTIFY-GH85-002 飞书签名、地址限制与消息提醒控制", async (t) => {
  const directory = mkdtempSync(join(tmpdir(), "sc-notify-feishu-"));
  t.after(() => rmSync(directory, { recursive: true, force: true }));

  await t.test("每次重试更新签名，通知内容不会触发 at 标签", async () => {
    // 使用官方示例的时间戳和 demo 密钥；期望值由独立 Python HMAC 实现计算。
    const bodies: { timestamp: string; sign: string; content: { text: string } }[] = [];
    let clock = 1599360473000;
    const notifications = createFeishuNotifications({ webhook, secret: "demo", logDirectory: directory }, {
      now: () => clock, sleep: async (ms) => { clock += ms; },
      fetch: async (_url, request) => {
        const body = String(request?.body);
        assert.ok(Buffer.byteLength(body) < 20 * 1024);
        bodies.push(JSON.parse(body));
        return Response.json(bodies.length === 1 ? { code: 11232 } : { code: 0 });
      },
    });
    notifications.notify({ issue: 85, stage: "merge", status: "failed", durationMs: 0, detail: '<at user_id="all">所有人</at> ' + "\0".repeat(5000), url: webhook });
    assert.deepEqual(await notifications.flush(), { accepted: 1, failed: 0 });
    assert.equal(bodies[0]!.timestamp, "1599360473");
    assert.equal(bodies[0]!.sign, "l1N0gAcBjdwBvGm1xMjOF0XSyaLRpR7tuO5dHfhAYc8=");
    assert.equal(bodies[1]!.timestamp, "1599360474");
    assert.notEqual(bodies[1]!.sign, bodies[0]!.sign);
    assert.doesNotMatch(bodies[0]!.content.text, /<at|demo|fake-hook-token/);
    assert.match(bodies[0]!.content.text, /＜at user_id=/);
    assert.doesNotMatch(readFileSync(notifications.logPath, "utf8"), /demo|fake-hook-token/);
  });

  await t.test("拒绝非官方 HTTPS v2 hook，不将配置错误升级为任务错误", async () => {
    for (const url of ["http://open.feishu.cn/open-apis/bot/v2/hook/fake-hook-token", "https://open.feishu.cn.evil.example/open-apis/bot/v2/hook/fake-hook-token", "https://open.feishu.cn/open-apis/bot/v2/hook/fake-hook-token?query=1", "https://open.feishu.cn/open-apis/bot/v2/hook/fake-hook-token#fragment", "https://open.feishu.cn/open-apis/bot/hook/fake-hook-token", "fake-hook-token"]) {
      let requests = 0;
      const notifications = createFeishuNotifications({ webhook: url, logDirectory: directory }, { fetch: async () => { requests++; throw new Error("不应发送"); } });
      assert.equal(await observeMilestone(notifications.notify, 85, "merge", () => "业务成功"), "业务成功");
      assert.deepEqual(await notifications.flush(), { accepted: 0, failed: 1 });
      assert.equal(requests, 0);
      assert.doesNotMatch(readFileSync(notifications.logPath, "utf8"), /fake-hook-token/);
    }
  });

  await t.test("HTTP 成功仍须有明确的成功业务码，错误消息不得泄漏凭据", async () => {
    // 同时给出旧成功字段与新错误字段时，以新 code 为准；缺失或字符串业务码均不能确认成功。
    for (const body of [{ code: 19021, StatusCode: 0, msg: `${webhook} demo` }, {}, null, { code: "0" }]) {
      let clock = 0;
      let requests = 0;
      const notifications = createFeishuNotifications({ webhook, secret: "demo", logDirectory: directory }, {
        now: () => clock, sleep: async (ms) => { clock += ms; },
        fetch: async () => { requests++; return Response.json(body); },
      });
      notifications.sendTest();
      assert.deepEqual(await notifications.flush(), { accepted: 0, failed: 1 });
      assert.equal(requests, 3);
      assert.doesNotMatch(readFileSync(notifications.logPath, "utf8"), /fake-hook-token|demo/);
    }
  });
});
