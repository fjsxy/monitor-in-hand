# Issue #87 实施前测试计划与发布契约

Issue：https://github.com/fjsxy/monitor-in-hand/issues/87

已领取 fjsxy；前置 PR #88 已合并为 `653c9b1b645b451cd5d822fe50805ce1f95fca00`，#86 已关闭。Issue 正文 SHA256：`021269767e7b69eaf820ee4f9383d4c06dc3b7fe0bc955a3affd166b25adb344`。本计划尚未执行；发布并精确回读后才修改业务代码或测试。

## 发布与升级边界

- 交付候选发布 `0.2.0-rc.1`（后续修复递增 RC，不覆盖旧候选）；同一干净源提交生成 amd64/arm64 Operator OCI index、Helm tgz、独立 CRD/RBAC、机器契约、release.json 与 SHA256SUMS。记录 source/testedCommit、index/platform digest、asset hash、GitHub prerelease 安装位置。最终证据绑定同一提交、CI run/attempt；候选发布不冒充正式稳定版。
- chart 支持固定 repository 的 `image.digest`，发布包默认锁定 index digest；不通过 latest 或本地 tag 交付。复用现有 Make/Docker buildx/Helm/GitHub release，不引入凭据存储或部署平台代码。
- 原 `0.1.0` 镜像 index 固定 `sha256:c64fda8d39b6653f74ede950d8ce3e37a479e25c137fd78f956442d641f41d6a`。已检查真实 arm64 binary：仅早期 Operator runtime，无 KSM controller，且无 VCS revision。单独验证该历史版本升级后的 CR 保留与开始收敛，不声称其原本管理 KSM workload。
- 另从 #86 前源提交 `2b2a8b79a52070fb89a08f5afae2b7698901c999` 发布 `0.1.0-pre-sm.2b2a8b7` 对照候选（明确非历史正式 0.1.0），用于证明运行中旧 KSM CR/workload/Service UID、finalizer、targetNamespace、owner 关系保留。旧 CRD 摘要保留，两种旧版路径分开产生 JUnit。
- 升级入口先验证整个已下载 bundle 的 manifest/文件 hash、版本/来源、KSM CRD 名称及能力，再以显式 context 对已存在 CRD 做 server-side apply（不 delete/recreate、不 force 接管），随后 Helm upgrade/install 同一 tgz；重复执行可收敛。能力缺失在平台 CR 写入之前明确拒绝，不能靠新字段被静默裁剪后的对象猜测支持。
- 机器契约包含 #86 的 spec/status/defaults/validation/reserved labels、stable name/owner、条件与 UID 观察、schema 能力判定；平台读取状态，不复制 Operator Service selector 算法。
- 受限平台 SA 只具有 exporter CR 管理和 SM 只读权限；以真实 impersonation API 请求证明 SM 写被 403 拒绝，而显式 exporter 更新由实际 Operator SA 生成配套 SM。Operator 的权限证据使用其自身身份和实际写入，不复用平台身份的 SSAR。
- #62/#63/#64 延期范围不变；测试中才安装外部 Prometheus Operator/原生 Agent。只有最终候选完整通过、版本和 digest 回填并回读后，才向平台 #28 宣告可消费；本 PR 不合并 #87，也不改平台产品代码。

## 测试边界

新增入口拟为 `make release-image`、`make release-package`、`make release-publish`、`make release-upgrade`、`make e2e-release`，当前均尚未实现/执行。核心业务断言全部 Ginkgo/Gomega，shell/Make 仅编排。真实双版本独立 kind，串行本地运行保护现有集群；CI 新增 `rel` lane，纳入最终 JUnit 来源检查和阻断门禁。已有稳定 ID 不改名。

## E2E

| 场景 ID | 来源 | 层级 | 目标版本 | 验收目的 | 执行步骤 | 执行预期 | 失败证据 | 测试入口 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| E2E-119-RELEASE-GH87-001 / E2E-128-RELEASE-GH87-001 | #87 | kind/发布产物新装 | 1.19.16 / 1.28.15 | 新装与缺可选 API 启动 | 下载并校验同候选 bundle；不装 SM CRD；用发布 tgz 和远程 digest 安装；创建关闭/开启 CR；安装固定 SM CRD | Operator/workload 正常；缺 API 独立观察；不改 CR 即恢复；运行 imageID 匹配当前 node 架构的发布 digest | manifest/Pod imageID/CR owner/UID、JUnit | 新增 make e2e-release |
| E2E-119-RELEASE-GH87-002 / E2E-128-RELEASE-GH87-002 | #87 | kind/历史版本升级 | 1.19.16 / 1.28.15 | 真实历史 0.1.0 的兼容边界 | 安装固定历史镜像与旧 chart/schema；创建旧 CR，证明新能力检查拒绝；执行 bundle 升级入口并重复执行 | 保留 CR UID/namespace/finalizer；升级后旧 CR 不自动启用采集，开始产生 workload；schema/RBAC/image 均为新候选 | 前后快照、schema/hash、命令顺序、JUnit | 新增 make e2e-release |
| E2E-119-RELEASE-GH87-003 / E2E-128-RELEASE-GH87-003 | #87 | kind/运行实例升级 | 1.19.16 / 1.28.15 | 功能前候选的原资源身份保持 | 安装已发布的 pre-SM 对照候选，等待两个旧 KSM 正常；记录 CR/Service/workload UID/owner；先升级 CRD 再升级 Operator | CR/Service/workload UID、targetNamespace、finalizer 和归属保持；旧 CR 仍关闭采集；仅新显式修改启用 | 两候选 source/image digest、UID、JUnit | 新增 make e2e-release |
| E2E-119-RELEASE-GH87-004 / E2E-128-RELEASE-GH87-004 | #87 | kind/真实权限与恢复 | 1.19.16 / 1.28.15 | 平台无 SM 写权限也可由 Operator 配套 | 创建受限 SA；实际 SM create/update/delete 请求均拒绝；用该身份更新 exporter；暂撤 Operator SM 权限后恢复 | 平台只写 CR；Operator 自身身份有 SM 权限并创建；故障不删 workload，恢复后不重发 CR 自动收敛 | 两身份 API 响应/SSAR、SM owner、status、JUnit | 新增 make e2e-release |
| E2E-119-RELEASE-GH87-005 / E2E-128-RELEASE-GH87-005 | #87 | kind/契约消费与数据面 | 1.19.16 / 1.28.15 | 发布产物与样本来自同环境 | 同一发布 Operator 在命名 namespace 管理两实例；原生 Agent 选择配套 SM；比对必要 targets，向 VM remote_write 后查对应样本；对照外部四类配置/Agent/生成资源 | 每个必要端点有真实样本；namespace/实例隔离正确；外部 UID/spec 不变；实际运行 imageID 与候选一致 | targets、query、对象快照、运行 digest、JUnit | 新增 make e2e-release |
| E2E-119-RELEASE-GH87-006 / E2E-128-RELEASE-GH87-006 | #87 | kind/清理与外部边界 | 1.19.16 / 1.28.15 | 逐例与全局清理可核对 | 删除一个实例，确认另一实例继续采集；删除全部本轮 CR 并等 finalizer；清理测试 Agent/SA，Helm uninstall；最后校验外部对象并删除隔离环境 | 本轮 CR/配套 SM/workload 消失；外部四类配置、Agent 与其原生生成资源身份/配置保持；无跳过 | 清理 JUnit、owner 图、剩余资源、环境删除日志 | 新增 make e2e-release |

## UT / Helm 与既有回归

| 场景 ID | 来源 | 层级 | 目标版本 | 验收目的 | 执行步骤 | 执行预期 | 失败证据 | 测试入口 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| UT-RELEASE-GH87-001 | #87 | Ginkgo/公开能力检查 | 通用 | schema 明确识别支持与旧版拒绝 | 读取真实新/旧 CRD，删改类型/default/status 引用字段，调用公开检查 | 仅完整支持的新 schema 通过；旧/缺失/不兼容字段明确拒绝 | Ginkgo failure/JUnit | make test-unit |
| UT-RELEASE-GH87-002 | #87 | Ginkgo/公开 bundle 校验 | 通用 | 来源、digest、文件完整性可复核 | 构造包含 chart/CRD/RBAC/contract 的独立 bundle；注入 hash 错、路径逃逸、缺架构、错 SHA/版本、错 CRD | 合法包通过；不完整/篡改包在任何集群写入前拒绝 | 独立字面期望、JUnit | make test-unit |
| UT-RELEASE-GH87-003 | #87 | Ginkgo/升级入口 | 通用 | 显式 context 与升级顺序 | 调用公开升级入口并记录外部执行边界，注入校验/CRD apply 失败 | 校验在前、CRD apply 在 Helm 前；显式 context；失败停止后续写；不 delete/recreate/force | 请求/命令记录、JUnit | make test-unit |
| HELM-GH87-001 | #87 | Ginkgo/实际 package/render | 通用 | chart 版本和 digest 钉住 | 实际打包/解包并 render 新 values，注入非法 digest；核对 CRD/RBAC 与源 | 同候选版本；image 使用 index digest；非法值拒绝；只含 KSM CRD/SM 管理 RBAC | tgz/hash/render、JUnit | make test-helm |
| UT-GATE-GH65-002（复用） | #65 / #87 | Ginkgo/JUnit 门禁 | 1.19 / 1.28 | 新 rel lane 的版本来源核对 | 提交两个版本 RELEASE 通过结果及错 lane/版本反例 | 正确 rel 来源放行；错误来源拒绝；ID 不重编号 | JUnit | make test-unit |
| 既有 GH86 与一期全部 blocking ID（复用） | #86 / #87 | 完整 CI | 1.19.16 / 1.28.15 | 发布变更不破坏功能和支持边界 | 同源提交执行既有 UT/IT/Helm、sm/ksm/agent lane，并增加 rel lane | 既有门禁维持；新增 16 个 ID 后总计 225 个，E2E 83 个，八个 E2E lane 清理成功 | 全部同 run/attempt JUnit | CI / make test |

## 结果记录

当前未编码、未执行、未发布候选。最终补充 source/testedCommit、候选 tag、镜像 index/platform digest、bundle hash、下载位置、双版本逐例 JUnit、实际 imageID/UID、清理及已知限制。独立历史镜像路径和 pre-SM 对照路径不得混称同一旧版。
