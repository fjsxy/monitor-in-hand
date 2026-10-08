import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { resolve } from "node:path";
import type { IssueRecord } from "./orchestration.mts";

export interface E2EPlanComment {
  id: number;
  body: string;
  url: string;
}

export interface E2EPlanOperations {
  listComments(): Promise<E2EPlanComment[]>;
  draftPlan(): Promise<string>;
  publishComment(body: string): Promise<E2EPlanComment>;
  readComment(id: number): Promise<E2EPlanComment>;
}

export function createGitHubE2EPlanOperations(
  issueNumber: number,
  repoRoot: string,
  draftPlan: () => Promise<string>,
  gh: (args: string[]) => string = (args) => execFileSync("gh", args, {
    cwd: repoRoot, encoding: "utf8", stdio: ["ignore", "pipe", "pipe"],
  }).trim(),
): E2EPlanOperations {
  const endpoint = "repos/fjsxy/monitor-in-hand/issues";
  const parseComment = (value: { id: number; body: string; html_url: string }): E2EPlanComment => ({
    id: value.id, body: value.body, url: value.html_url,
  });
  return {
    async listComments() {
      const pages = JSON.parse(gh(["api", "--paginate", "--slurp", `${endpoint}/${issueNumber}/comments`])) as Array<Array<{ id: number; body: string; html_url: string }>>;
      return pages.flat().map(parseComment);
    },
    draftPlan,
    async publishComment(body) {
      const directory = mkdtempSync(resolve(tmpdir(), "monitor-op-e2e-plan-"));
      try {
        const bodyFile = resolve(directory, "comment.md");
        writeFileSync(bodyFile, body, { mode: 0o600 });
        const url = gh(["issue", "comment", String(issueNumber), "--body-file", bodyFile]);
        const id = /#issuecomment-([0-9]+)$/.exec(url)?.[1];
        if (!id) throw new Error(`Issue #${issueNumber} 未返回可回读的 E2E 评论 URL。`);
        return { id: Number(id), body, url };
      } finally {
        rmSync(directory, { recursive: true, force: true });
      }
    },
    async readComment(id) {
      return parseComment(JSON.parse(gh(["api", `${endpoint}/comments/${id}`])));
    },
  };
}

const stableE2EID = /E2E-(?:119|128)-[A-Z0-9]+-(?:GH[1-9][0-9]*-)?[0-9]{3}/g;

function tableCells(line: string): string[] {
  // Markdown 表格中的 regex 可以包含转义的竖线；只拆分真正的列边界。
  return line.trim().replace(/^\||\|$/g, "").split(/(?<!\\)\|/)
    .map((cell) => cell.trim());
}

export function e2ePlanMarker(issue: Pick<IssueRecord, "number" | "body">): string {
  const scope = createHash("sha256").update(issue.body).digest("hex");
  return `<!-- monitor-in-hand:e2e-plan issue=${issue.number} scope=${scope} -->`;
}

export function extractE2EPlan(stdout: string): string {
  const plans = [...stdout.matchAll(/<e2e-plan>([\s\S]*?)<\/e2e-plan>/g)];
  const plan = plans.at(-1)?.[1]?.trim();
  if (!plan) throw new Error("E2E planner 未输出完整的 <e2e-plan> 评论草稿。");
  return plan;
}

export function validateE2EPlan(plan: string, issueNumber: number, matrix: string): void {
  // 从当前矩阵读取格式，评论不能使用旧表头或把表格放进代码块。
  const e2eSection = matrix.split(/^## E2E\s*$/m)[1];
  const header = e2eSection?.split("\n").find((line) => line.startsWith("| 场景 ID |"));
  if (!header) throw new Error("验收矩阵缺少 E2E 表头。");
  const columns = tableCells(header);
  if (!["验收目的", "执行步骤", "执行预期"].every((field) => columns.includes(field))) {
    throw new Error("验收矩阵 E2E 表头缺少三个必填部分。");
  }

  const lines = plan.split("\n");
  const headerIndex = lines.findIndex((line) => line.trim().startsWith("| 场景 ID |"));
  if (headerIndex < 0 || JSON.stringify(tableCells(lines[headerIndex]!)) !== JSON.stringify(columns)) {
    throw new Error("E2E 评论表头必须与验收矩阵 E2E 部分一致。");
  }
  if (lines.slice(0, headerIndex).filter((line) => /^\s*(?:```|~~~)/.test(line)).length % 2 !== 0) {
    throw new Error("E2E 评论必须渲染为 Markdown 表格，不能放在代码块中。");
  }
  const separator = tableCells(lines[headerIndex + 1] ?? "");
  if (separator.length !== columns.length || !separator.every((cell) => /^:?-{3,}:?$/.test(cell))) {
    throw new Error("E2E 评论缺少完整的 Markdown 表格分隔行。");
  }

  // 校验全部 case 的字段、稳定身份与来源；已有场景可以复用，新场景归属当前 Issue。
  const knownIDs = new Set(e2eSection!.match(stableE2EID) ?? []);
  const seen = new Set<string>();
  let rowCount = 0;
  for (const line of lines.slice(headerIndex + 2)) {
    if (!line.trim().startsWith("|")) break;
    const cells = tableCells(line);
    if (cells.length !== columns.length || cells.some((cell) => !cell || /^(?:TODO|TBD|待补充|\.\.\.|…|-|—)$/i.test(cell))) {
      throw new Error("每个 E2E case 必须完整填写验收矩阵的全部字段。");
    }
    const ids = cells[0]!.match(stableE2EID) ?? [];
    if (ids.length === 0 || cells[0]!.replace(stableE2EID, "").replace(/[`\s/]/g, "")) {
      throw new Error("E2E 评论包含非法稳定场景 ID。");
    }
    for (const id of ids) {
      if (seen.has(id)) throw new Error(`E2E 评论重复场景 ID：${id}`);
      seen.add(id);
      const owner = /-GH([1-9][0-9]*)-/.exec(id)?.[1];
      if (!knownIDs.has(id) && owner !== String(issueNumber)) {
        throw new Error(`新增场景必须归属当前 Issue：${id}`);
      }
      if (owner && !new RegExp(`#${owner}(?![0-9])`).test(cells[columns.indexOf("来源")] ?? "")) {
        throw new Error(`场景来源必须包含所属 Issue：${id}`);
      }
    }
    rowCount += 1;
  }
  const noE2EReason = /^## 无相关 E2E 的依据[ \t]*\n+([^\n]+)$/m.exec(plan)?.[1]?.trim();
  if (rowCount === 0 && (!noE2EReason || /^(?:TODO|TBD|待补充)$/i.test(noE2EReason))) {
    throw new Error("没有相关 E2E 时，必须保留表头并说明无相关 E2E 的依据和替代验证。");
  }
}

export function assertE2EPlanningReadOnly(commits: readonly unknown[], beforeHead: string, afterHead: string, status: string): void {
  if (commits.length > 0 || beforeHead !== afterHead || status.trim()) {
    throw new Error("E2E 规划阶段修改了工作树或提交；评论发布前不能进入实现。");
  }
}

export async function runWithE2EPlan<T>(
  issue: Pick<IssueRecord, "number" | "body">,
  matrix: string,
  operations: E2EPlanOperations,
  implement: (comment: E2EPlanComment) => Promise<T>,
): Promise<T> {
  const marker = e2ePlanMarker(issue);
  // 仅复用与当前 Issue 正文一致的已发布计划；需求变化后重新整理并保留旧评论作为历史。
  const existing = (await operations.listComments()).filter((comment) => comment.body.startsWith(marker)).at(-1);
  let published: E2EPlanComment;
  let expectedBody: string;
  if (existing) {
    validateE2EPlan(existing.body, issue.number, matrix);
    published = existing;
    expectedBody = existing.body;
  } else {
    const plan = await operations.draftPlan();
    validateE2EPlan(plan, issue.number, matrix);
    expectedBody = `${marker}\n\n${plan.trim()}\n`;
    published = await operations.publishComment(expectedBody);
  }

  // GitHub 回读必须与预期正文完全一致；发布或验证失败会在 implement 调用前抛出。
  const verified = await operations.readComment(published.id);
  if (verified.id !== published.id || verified.body.trim() !== expectedBody.trim() || verified.url !== published.url || !verified.url) {
    throw new Error(`Issue #${issue.number} 的 E2E 评论发布回读不一致。`);
  }
  validateE2EPlan(verified.body, issue.number, matrix);
  return implement(verified);
}
