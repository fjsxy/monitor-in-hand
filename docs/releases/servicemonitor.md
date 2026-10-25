# 配套 ServiceMonitor 候选发布

本候选交付 #87 的可安装基线，尚未因为创建 prerelease 就宣称验收通过。最终以同一源提交的完整 CI 和 Issue #87 验收评论为准；正式稳定发布需要后续合并和发布决定。

产物包含 `release.json`、`SHA256SUMS`、当前 chart tgz、独立 CRD/RBAC、`servicemonitor-contract.json`，以及两条升级路径的对照 chart。镜像 index 与 amd64/arm64 manifest digest 均在 `release.json`，新 chart 默认钉住 index digest。

历史 `0.1.0` 镜像不含 KSM controller，且没有可核对的 VCS revision；只记录实际 registry digest，不推定其源提交。`0.1.0-pre-sm.2b2a8b7` 是从功能前源提交新构建并发布的对照候选，不冒充历史正式版本。它用于验证已经正常运行的 KSM CR/Service/workload 身份保持，两条路径各有独立 JUnit。

## 生成候选

在已提交的干净源提交执行，先本地验证，再推送同一源分支：

```sh
make release-image SOURCE_COMMIT="$(git rev-parse HEAD)"
make release-package SOURCE_COMMIT="$(git rev-parse HEAD)"
make release-publish SOURCE_COMMIT="$(git rev-parse HEAD)"
```

依赖既有 Docker credential helper、Docker buildx、Go 1.22+、Helm 3.22.0、gh 登录。本流程不保存凭据。已存在且属于同源提交的镜像可复用；其他来源的 tag 必须拒绝。修复时递增 RC，不覆盖候选或历史 `0.1.0`。

## 消费与升级

从 Issue #87 / 平台 #28 的最终验收评论复制精确 VERSION、SOURCE_COMMIT 及 release.json 的 SHA256，不能用 latest。私有 GitHub release 使用已有 gh 登录下载：

```sh
gh release download "v${VERSION}" --repo fjsxy/monitor-in-hand --dir "bundle-${VERSION}"
(cd "bundle-${VERSION}" && shasum -a 256 -c SHA256SUMS)
make release-verify BUNDLE="bundle-${VERSION}" SOURCE_COMMIT="${SOURCE_COMMIT}"
make release-upgrade BUNDLE="bundle-${VERSION}" SOURCE_COMMIT="${SOURCE_COMMIT}" KUBE_CONTEXT="${KUBE_CONTEXT}"
```

在任意 CR 新字段写入前，平台用只读权限获取已部署的 KSM CRD，按机器契约检查完整字段类型、served version、enabled 缺省关闭及 status 引用能力。也可用 `go run ./cmd/release capability --file deployed-crd.yaml`。旧 schema 必须明确拒绝，不能通过先发送再读取被裁剪的 CR 猜测能力。

升级入口验证 bundle 后，先用独立 field manager 对既有 CRD 执行 server-side apply，再用同包 chart 执行 Helm upgrade/install `--skip-crds`。Helm 的 `crds/` 不会自动更新已存在 schema。入口不删除 CRD、不批量重建 CR、不 force 覆盖冲突；CRD apply 失败即停止后续升级，管理员应处理具体冲突后重复执行同一包。

升级后旧 CR 保持关闭采集。显式发布 `spec.serviceMonitor.enabled=true` 后，Operator 才按当前 CR UID 创建配套对象。平台只需 CR 管理及 SM 只读权限，不能据此取得 SM 写权限；`ServiceMonitorReady` 只代表配套配置收敛，真实采集须另查 Agent targets 和接收端样本。

降级不属于无损回滚承诺：启用新字段后不能用旧 schema 覆盖并声称数据保留。#62/#63/#64 延期范围不变。安全卸载仍须先清理本项目 CR 并等待 finalizer。

CI 的 `rel` lane 按 Chart appVersion 对应的轻量 Git tag 锁定候选源提交，下载同一发布包；测试报告记录该候选源提交。其他 lane 验证 CI 当前提交。发布验收时须确认 tag/source/testedCommit 为拟交付的提交，并在同一个 run/attempt 汇总；后续 main rebase 不改写候选来源。

候选完整验收仅引用该版本 tag 触发的 CI（所有 job checkout 同一候选提交，rel 额外断言 tag 源等于 GITHUB_SHA）；日常 PR/main 对冻结候选的回归与当前提交测试分开解释，不拼成候选发布证据。
