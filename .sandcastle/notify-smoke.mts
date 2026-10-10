import { resolve } from "node:path";
import { readLocalEnv } from "./env.mts";
import { createFeishuNotifications, resolveFeishuConfig } from "./notifications.mts";

const localEnv = readLocalEnv();
const notifications = createFeishuNotifications({
  ...resolveFeishuConfig(process.env, localEnv),
  logDirectory: resolve(".sandcastle/logs"),
  secrets: [process.env.GH_TOKEN ?? localEnv.GH_TOKEN ?? ""],
});
if (!notifications.enabled) throw new Error("请先在 .sandcastle/.env 设置 SANDCASTLE_FEISHU_WEBHOOK。");
notifications.sendTest();
const result = await notifications.flush();
if (result.failed) process.exitCode = 1;
console.log(result.accepted ? "飞书已接受测试消息；请在机器人所在群确认收到。" : `测试通知失败，记录：${notifications.logPath}`);
