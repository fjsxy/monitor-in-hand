# Issue #86 实施前测试计划与冻结契约

Issue：https://github.com/fjsxy/monitor-in-hand/issues/86

正文 SHA256：`842d83b364704d11c1a9d14df6084e5fcc7436558e601643971e0e56403a6c57`。已领取并回读，直接 blocker 为零。本计划尚未执行；代码与测试仅在此计划发布、回读一致后开始。各必选版本均需独立实际结果；#87 的发布验收不替代本项。

## 冻结契约

- `spec.serviceMonitor`：`enabled` 默认 false，`labels` 为附加标签，`interval`/`scrapeTimeout` 默认 30s/10s，合法正时长且 timeout≤interval；保留 `app.kubernetes.io/*`、`monitor-in-hand/*` 身份标签，拒绝覆盖。不提供 selector/namespace/port 透传。
- 名称复用 `ksm-<instance-id>`，位于 spec.targetNamespace；SM controller ownerReference 指向 cluster-scoped KSM CR 当前 UID；selector 使用 name/instance/managed-by，覆盖本实例全部必要 Service，抓取 http-metrics、/metrics。
- `status.serviceMonitor` 回报 observedGeneration、state（Unknown/Disabled/Ready/Degraded/Deleting）、reason、message、resource（apiVersion/kind/namespace/name/uid）；`ServiceMonitorReady` condition 独立于现有 workload 的 Ready/Available，不将子对象存在当作采集成功。对象重建后引用 UID 更新，owner UID 不变。
- 可选 API 使用动态客户端精确直读和有界周期重查，不向 manager 注册可能缺失的 SM informer；因此 CRD 缺失或权限错误不阻塞全局启动，API 恢复不要求重启或重发 CR。已有核心资源 informer 的过滤边界不变；不观察或修改外部采集配置。
- 采集故障保留运行中的 exporter；挂起保留 SM，关闭采集仅删除当前 owner SM，停用/删除清理全部当前 owner 资源并等待确认。未知 UID/旧 UID/外部同名资源不接管；删除采用 UID/RV 条件。
- 新 schema 是增量能力：保留 v0.1.0 历史 CRD 基线，新建本功能 schema 基线并明确能力字段；不覆盖旧摘要伪称 patch 零漂移。#87 后续确定正式发布版本和升级产物。

## 已确认测试边界

| 边界 | 验证能力与限制 |
| --- | --- |
| BuildPlan 公共入口 | 类型化配置校验、稳定资源规划、三种拓扑及版本限制；不能证明真实 API/Pod/采集 |
| Reconcile + 真实 envtest API | schema 默认/裁剪、owner、状态、失败恢复、清理及 API 延迟出现；不能证明真实 kubelet/GC/网络 |
| Helm 真实渲染及生成物 | CRD/RBAC 同步、只授权配套 SM、历史 schema 基线；不能证明安装运行 |
| 独立真实 kind + 当前构建 Operator + 原生 Agent | 缺 CRD 启动、依赖恢复、多实例完整生命周期、真实目标和指标样本、清理；各版本独立环境 |

## E2E

复用现有 kind/Helm/镜像编排与 Ginkgo 边界；新增专用入口 `make e2e-servicemonitor KUBERNETES_VERSION=<minor> TEST_RUN_ID=<run>`（尚未实现）。1.19.16 不运行 DaemonSetSharded 正向场景，明确验证 UnsupportedMode 且零写入。保留既有稳定 ID；新增 ID 归属 GH86。

| 场景 ID | 来源 | 层级 | 目标版本 | 验收目的 | 执行步骤 | 执行预期 | 失败证据 | 测试入口 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| E2E-119-SM-GH86-001 / E2E-128-SM-GH86-001 | #86 | kind/启动与可选依赖 | 1.19.16 / 1.28.15 | 缺 CRD 不阻塞 Operator，安装后自动恢复 | 独立集群不装 SM CRD；Helm 安装当前镜像；创建关闭/开启实例；随后安装锁定 SM CRD | Operator 可用、关闭实例正常；开启实例报告依赖缺失；安装 CRD 后同 CR UID/不改 generation 自动生成 SM | Pod/log/status、CR/SM UID、JUnit | 新增 e2e-servicemonitor |
| E2E-119-SM-GH86-002 / E2E-128-SM-GH86-002 | #86 | kind/配置与隔离 | 1.19.16 / 1.28.15 | 默认、归属标签、稳定命名及多实例隔离 | 创建最小旧式 CR 与两个开启 CR（含长名称）；回读 SM/Service/owner/参数，修改其中一个参数 | 最小 CR 不生成 SM；每个开启实例仅有一个 owner 匹配的 SM，正确标签/端口/默认值，另一实例不变 | 对象快照、status、JUnit | 新增 e2e-servicemonitor |
| E2E-119-SM-GH86-003 / E2E-128-SM-GH86-003 | #86 | kind/自动修复与权限 | 1.19.16 / 1.28.15 | 漂移/缺失/权限故障不删除 exporter | 修改再删除受管 SM；临时撤销 Operator 的 SM 权限后修改采集声明；恢复权限并重启 Operator | 自动修复与新 UID 回报；故障期 workload 保留；恢复后无需重发 CR 收敛，其他实例可继续管理 | 请求/资源/状态、Operator logs、JUnit | 新增 e2e-servicemonitor |
| E2E-119-SM-GH86-004 / E2E-128-SM-GH86-004 | #86 | kind/冲突 | 1.19.16 / 1.28.15 | 外部同名/旧 owner 对象不接管 | 准备无管理标签/旧 CR UID owner 的同名 SM；启用对应实例；观察窗口后清理冲突 | 明确 ResourceConflict；外部 UID/spec/labels不变；解除冲突后自动恢复 | 对象前后指纹、status/Events、JUnit | 新增 e2e-servicemonitor |
| E2E-119-SM-GH86-005 / E2E-128-SM-GH86-005 | #86 | kind/生命周期 | 1.19.16 / 1.28.15 | 挂起/关闭/停用/恢复/删除及 finalizer | 对同一实例依次挂起/恢复、关闭采集/开启、停用/恢复，最后删除；另一个实例和外部配置作对照 | 挂起保留 SM；关闭仅删 SM；停用/删除清理所有 owner 资源，恢复重建；对照对象不变 | owner 图、终止对象、JUnit及清理 | 新增 e2e-servicemonitor |
| E2E-119-SM-GH86-006 / E2E-128-SM-GH86-006 | #86 | kind/拓扑和真实采集 | 1.19.16 / 1.28.15 | 所有必要 Service 均被实际采集 | 两版本创建 Deployment/StatefulSetSharded；1.28另建 DaemonSetSharded；用原生 Agent 选 Operator SM，检查 targets 与可关联样本 | 必要 Service 目标完整、正确 metrics；Agent 使用 Operator 对象；1.19 DaemonSetSharded 拒绝且零写入；不以 Ready/生成配置代替样本 | Agent targets、样本、SM owner、JUnit | 新增 e2e-servicemonitor |
| E2E-119-SM-GH86-007 / E2E-128-SM-GH86-007 | #86 | kind/零匹配和模式切换 | 1.19.16 / 1.28.15 | Service 拓扑变化与动态过滤不破坏配套关系 | 更新 namespaceFilter 至零匹配后恢复；Deployment切换StatefulSetSharded，1.28再切DaemonSetSharded | 零匹配无 workload但保留配套资源，恢复自动可采；模式切换后目标集合准确且不残留旧拓扑 | workload/Service/SM快照、targets、JUnit | 新增 e2e-servicemonitor |

## 其他自动化层

| 场景 ID | 来源 | 层级 | 目标版本 | 验收目的 | 执行步骤 | 执行预期 | 失败证据 | 测试入口 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| UT-SM-GH86-001 | #86 | Ginkgo/BuildPlan | 1.19/1.28 profiles | 默认、声明与资源映射 | 通过公共 BuildPlan 规划关闭/开启、长名、三拓扑、挂起/零匹配输入 | 独立字面期望验证 selector/端口/owner/默认值/隔离与版本限制 | Ginkgo报告 | make test-unit |
| UT-SM-GH86-004 | #86 | Ginkgo/BuildPlan | 1.19/1.28 profiles | 长名称与拓扑共享身份 | 规划长名称、Deployment/StatefulSet/DaemonSet 与版本拒绝路径 | 稳定名称不超长、同一 selector 覆盖必要 Service，1.19 拒绝不支持模式 | Ginkgo报告 | make test-unit |
| UT-SM-GH86-002 | #86 | Ginkgo/BuildPlan | 通用 | 非法配置零资源 | 表驱动负 duration/timeout>interval/保留或非法labels | 返回 InvalidSpec且不生成可写资源 | Ginkgo报告 | make test-unit |
| UT-SM-GH86-003 | #86 | Ginkgo/Reconcile API边界 | 通用 | 404/403/409/响应丢失、owner与清理 | 用真实 API 合约服务或 dynamic fake 在公开 Reconcile 注入结果 | 正确报告/重试/条件写，不接管或清理无关对象 | 请求、状态、JUnit | make test-unit |
| IT-SM-GH86-001 | #86 | envtest/schema | 1.28 API | 新字段不被裁剪且旧对象兼容 | 创建真实 CR并回读默认值、spec/status结构；非法输入经schema或planner拒绝 | 能按schema识别能力，旧字段/UID保留 | API响应、JUnit | make test-integration |
| IT-SM-GH86-002 | #86 | envtest/Reconcile | 1.28 API | 配套生命周期及故障隔离 | 使用真实 API执行创建、漂移、删除、冲突、关闭、停用和finalizer | owner/资源UID、观察generation和清理可核对；workload可用性独立 | API对象、JUnit | make test-integration |
| IT-SM-GH86-003 | #86 | envtest/可选API | 1.28 API | 缺失API延迟安装后收敛 | 在同一控制器生命周期中先无SM API，再安装官方CRD并恢复 | manager未被可选informer阻塞，无需重发CR即可收敛 | API/状态、JUnit | make test-integration |
| UT-GATE-GH65-002（复用） | #65 / #86 | Ginkgo/JUnit 门禁 | 1.19 / 1.28 | 新增 SM lane 的报告归属可核对 | 向公共 VerifyJUnitResults 提交两个版本 sm lane 的通过报告及错误 lane 反例 | 正确来源计入通过，跨版本或 lane 结果拒绝；不新增稳定 ID | Ginkgo报告、完整 CI 报告集 | make test-unit |
| HELM-GH86-001 | #86 | Helm/生成物 | 通用 | CRD与最小RBAC同步 | 实际render并比较generated schema/RBAC，检查历史和新增schema基线 | 仅新增配套SM权限，不带外部CRD/Agent管理；旧摘要保留 | render/diff、JUnit | make test-helm、verify-generated |

## 实际结果

未执行。后续证据绑定 testedCommit，列明成功、失败、环境失败和未执行，不混合不同版本、提交或CI尝试。新增案例必须进入对应UT/HELM/IT/E2E验收表，逐例和全局清理均需验证。

## 2026-10-23 门禁接线补充

六个真实 E2E lane 均通过后，最终汇总发现原有 lane 白名单尚未接纳 SM 领域。复用既有 `UT-GATE-GH65-002` 验证新增 sm lane 的双版本归属及错误来源拒绝；保留该 ID 和 #65 来源，矩阵追加 #86 来源。此补充只修复既定 CI 接线，不改变产品契约、测试总 ID 数或验收门槛。补充发布并回读后再修改门禁代码。此前运行的单项成功不替代修复后新提交的完整 CI。
