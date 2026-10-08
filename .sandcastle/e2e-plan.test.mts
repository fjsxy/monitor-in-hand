import assert from "node:assert/strict";
import { existsSync, readFileSync } from "node:fs";
import test from "node:test";
import {
  assertE2EPlanningReadOnly,
  createGitHubE2EPlanOperations,
  e2ePlanMarker,
  extractE2EPlan,
  runWithE2EPlan,
  validateE2EPlan,
  type E2EPlanComment,
  type E2EPlanOperations,
} from "./e2e-plan.mts";

const header = "| 场景 ID | 来源 | 层级 | 目标版本 | 验收目的 | 执行步骤 | 执行预期 | 失败证据 | 测试入口 |";
const separator = "| --- | --- | --- | --- | --- | --- | --- | --- | --- |";
const cells = ["`E2E-119-GATE-GH65-001` / `E2E-128-GATE-GH65-001`", "#65", "kind E2E", "1.19、1.28", "证明阻断 gate 不漏报", "1. 执行场景；<br>2. 检查报告", "所有必选 ID 有通过结果", "JUnit", "计划新增 make e2e-gate（尚未实现）"];
const row = (values = cells) => `| ${values.join(" | ")} |`;
const plan = ["## E2E", header, separator, row()].join("\n");
const existingRow = row(["`E2E-119-COLLECTION-GH61-001`", "#61", ...cells.slice(2)]);
const matrix = ["## E2E", header, separator, existingRow].join("\n");
const issue = { number: 65, body: "## Acceptance criteria\n\n一期必选场景都必须有结果。" };

test("评论使用验收矩阵表头，支持新增双版本 ID 和复用已有场景", () => {
  validateE2EPlan(plan, 65, matrix);
  validateE2EPlan(["## E2E", header, separator, existingRow].join("\n"), 65, matrix);
  validateE2EPlan(plan.replace("1. 执行场景", "1. 使用 pattern=a\\|b 执行场景"), 65, matrix);
});

test("逐列拒绝空字段、占位内容和截断行", () => {
  for (let index = 0; index < cells.length; index += 1) {
    const incomplete = [...cells];
    incomplete[index] = " \t ";
    assert.throws(() => validateE2EPlan(plan.replace(row(), row(incomplete)), 65, matrix), /完整填写/);
  }
  assert.throws(() => validateE2EPlan(plan.replace("证明阻断 gate 不漏报", "待补充"), 65, matrix), /完整填写/);
  assert.throws(() => validateE2EPlan(plan.replace(row(), row(cells.slice(0, -1))), 65, matrix), /完整填写/);
});

test("拒绝旧表头、缺少分隔行、错误来源、其他 Issue 新 ID 和重复 ID", () => {
  assert.throws(() => validateE2EPlan(plan.replace("验收目的", "动作与精确断言"), 65, matrix), /表头/);
  assert.throws(() => validateE2EPlan("```markdown\n" + plan + "\n```", 65, matrix), /代码块/);
  assert.throws(() => validateE2EPlan(plan.replace(separator, ""), 65, matrix), /分隔行/);
  assert.throws(() => validateE2EPlan(plan.replace("#65", "#650"), 65, matrix), /来源/);
  assert.throws(() => validateE2EPlan(plan.replaceAll("GH65", "GH99"), 65, matrix), /当前 Issue/);
  assert.throws(() => validateE2EPlan(`${plan}\n${row()}`, 65, matrix), /重复/);
  assert.throws(() => validateE2EPlan(plan.replace("E2E-119-GATE-GH65-001", "E2E-UNSUPPORTED-GATE-GH65-001"), 65, matrix), /非法/);
});

test("无相关 E2E 必须发布空表并说明原因和替代验证", () => {
  const empty = ["## E2E", header, separator].join("\n");
  assert.throws(() => validateE2EPlan(empty, 65, matrix), /依据/);
  assert.throws(() => validateE2EPlan(`${empty}\n\n## 无相关 E2E 的依据\n\n待补充`, 65, matrix), /依据/);
  validateE2EPlan(`${empty}\n\n## 无相关 E2E 的依据\n\n仅修改静态文档链接，使用链接检查验证，无运行时行为变化。`, 65, matrix);
});

test("planner 输出必须有完整草稿，且规划不得修改 HEAD 或工作树", () => {
  assert.equal(extractE2EPlan(`日志\n<e2e-plan>\n${plan}\n</e2e-plan>`), plan);
  assert.throws(() => extractE2EPlan("只有完成信号"), /草稿/);
  assertE2EPlanningReadOnly([], "head", "head", "");
  for (const [commits, before, after, status] of [
    [[{ sha: "new" }], "head", "head", ""],
    [[], "head", "new", ""],
    [[], "head", "head", " M internal/controller.go"],
  ] as const) {
    assert.throws(() => assertE2EPlanningReadOnly(commits, before, after, status), /规划阶段/);
  }
});

function operations(events: string[], comments: E2EPlanComment[] = []): E2EPlanOperations {
  let published = comments.at(-1);
  return {
    async listComments() { events.push("list"); return comments; },
    async draftPlan() { events.push("plan"); return plan; },
    async publishComment(body) {
      events.push("publish");
      published = { id: 123, body, url: "https://github.com/fjsxy/monitor-in-hand/issues/65#issuecomment-123" };
      return published;
    },
    async readComment() { events.push("read-back"); return published!; },
  };
}

test("发布并回读 E2E 评论成功后才开始实现", async () => {
  const events: string[] = [];
  const result = await runWithE2EPlan(issue, matrix, operations(events), async (comment) => {
    events.push("implement");
    assert.match(comment.body, /^<!-- monitor-in-hand:e2e-plan issue=65 scope=[a-f0-9]{64} -->/);
    return "完成";
  });
  assert.equal(result, "完成");
  assert.deepEqual(events, ["list", "plan", "publish", "read-back", "implement"]);
});

test("重试复用当前范围的有效评论，范围变化后重新规划", async () => {
  const comment = { id: 123, body: `${e2ePlanMarker(issue)}\n\n${plan}`, url: "https://github.com/fjsxy/monitor-in-hand/issues/65#issuecomment-123" };
  const events: string[] = [];
  await runWithE2EPlan(issue, matrix, operations(events, [comment]), async () => { events.push("implement"); });
  assert.deepEqual(events, ["list", "read-back", "implement"]);

  events.length = 0;
  await runWithE2EPlan({ ...issue, body: issue.body + "新增验收条件" }, matrix, operations(events, [comment]), async () => { events.push("implement"); });
  assert.deepEqual(events, ["list", "plan", "publish", "read-back", "implement"]);
});

test("规划失败、格式错误、发布失败或回读错误都不启动实现", async () => {
  for (const failure of ["planning", "format", "publish", "read-error", "read-body", "read-url"] as const) {
    const events: string[] = [];
    const ops = operations(events);
    if (failure === "planning") ops.draftPlan = async () => { throw new Error("规划失败"); };
    if (failure === "format") ops.draftPlan = async () => "未填写 case";
    if (failure === "publish") ops.publishComment = async () => { throw new Error("发布失败"); };
    if (failure === "read-error") ops.readComment = async () => { throw new Error("GitHub 不可用"); };
    if (failure === "read-body" || failure === "read-url") {
      const read = ops.readComment;
      ops.readComment = async (id) => ({ ...(await read(id)), ...(failure === "read-body" ? { body: "正文丢失" } : { url: "" }) });
    }
    await assert.rejects(runWithE2EPlan(issue, matrix, ops, async () => { events.push("implement"); }));
    assert.equal(events.includes("implement"), false, failure);
  }
});

test("GitHub 发布保留 Markdown 原文并清理临时文件，支持分页回读", async () => {
  const literalBody = `${e2ePlanMarker(issue)}\n\n${plan}\n命令示例包含字面反引号、$() 与换行。\n`;
  let bodyFile = "";
  const ops = createGitHubE2EPlanOperations(65, process.cwd(), async () => plan, (args) => {
    if (args[0] === "issue") {
      bodyFile = args.at(-1)!;
      assert.equal(readFileSync(bodyFile, "utf8"), literalBody);
      return "https://github.com/fjsxy/monitor-in-hand/issues/65#issuecomment-123";
    }
    const comment = { id: 123, body: literalBody, html_url: "https://github.com/fjsxy/monitor-in-hand/issues/65#issuecomment-123" };
    return JSON.stringify(args.includes("--paginate") ? [[], [comment]] : comment);
  });
  const published = await ops.publishComment(literalBody);
  assert.equal(existsSync(bodyFile), false);
  assert.deepEqual(await ops.readComment(published.id), published);
  assert.deepEqual(await ops.listComments(), [published]);
});

test("GitHub 发布失败也清理临时文件", async () => {
  let bodyFile = "";
  const ops = createGitHubE2EPlanOperations(65, process.cwd(), async () => plan, (args) => {
    bodyFile = args.at(-1)!;
    assert.equal(existsSync(bodyFile), true);
    throw new Error("GitHub 拒绝发布");
  });
  await assert.rejects(ops.publishComment(plan), /拒绝发布/);
  assert.equal(existsSync(bodyFile), false);
});
