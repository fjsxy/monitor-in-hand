# 一期验收矩阵

本文件记录已经自动化并作为 blocking gate 执行的稳定场景。已有 ID 保持不变；后续 ticket 追加自身场景时使用 `docs/agents/development-standards.md` 规定的 Issue-scoped ID，不再分配全局递增编号。

## UT

| 场景 ID | 来源 | 层级 | 目标版本 | 动作与精确断言 | 失败证据 | 测试入口 |
| --- | --- | --- | --- | --- | --- | --- |
| `UT-KSM-001` | #51 | 纯规划 | 1.19、1.28 | 最小 CR 选择固定 profile，并规划单副本 Deployment、headless Service、独立 SA/ClusterRole/ClusterRoleBinding、稳定 labels 与当前 UID ownerReference | Ginkgo failure | `go test ./internal/kubestatemetrics` |
| `UT-KSM-002` | #51 | 纯规划 | 其他 minor | 返回 `UnsupportedKubernetesVersion`，资源计划为空 | Ginkgo failure | `go test ./internal/kubestatemetrics` |
| `UT-KSM-003` | #51 | 纯规划 | 1.28 | 目标 Namespace 不存在时返回 `DependencyMissing`，资源计划为空 | Ginkgo failure | `go test ./internal/kubestatemetrics` |
| `UT-KSM-004` | #51 | 纯规划 | 1.28 | 首次应用后 targetNamespace 变化返回 `InvalidSpec`，资源计划为空 | Ginkgo failure | `go test ./internal/kubestatemetrics` |
| `UT-KSM-005` | #51 | 纯规划/RBAC | 1.19、1.28 | Operator ClusterRole 覆盖两个固定 profile 下放给 kube-state-metrics 的全部权限 | Ginkgo failure、RBAC manifest | `go test ./internal/kubestatemetrics` |
| `UT-KSM-006` | #54 | 纯规划 | 1.19、1.28 | `Default` 固定解析为两个版本共同的 28 项 collector catalog，并显式生成 `--resources`；省略 `podResources` 时不注入资源默认值 | Ginkgo failure | `go test ./internal/kubestatemetrics` |
| `UT-KSM-007` | #52 | 纯逻辑 | 通用 | phase 按 Deleting、Disabling、Disabled、Degraded、Reconciling、Ready、Pending 的优先级从删除状态与 conditions 派生 | Ginkgo failure | `go test ./internal/kubestatemetrics` |
| `UT-KSM-008` | #54 | 纯规划 | 1.19、1.28 | `Include` 的显式 collector 同时成为实际参数与 `effectiveCollectors` | Ginkgo failure | `go test ./internal/kubestatemetrics` |
| `UT-KSM-009` | #54 | 纯规划 | 1.19、1.28 | 拒绝 Default 携带 names、Include 空集合、重复项与共同 catalog 外名称，返回 `InvalidSpec` 且零资源 | Ginkgo failure | `go test ./internal/kubestatemetrics` |
| `UT-KSM-010` | #54 | 纯规划 | 1.19、1.28 | Allow/Deny 分别映射 metric allowlist/denylist，pattern 不被隐式锚定 | Ginkgo failure | `go test ./internal/kubestatemetrics` |
| `UT-KSM-011` | #54 | 纯规划 | 1.19、1.28 | 拒绝 mode/list 冲突、空项、非法 Go regexp 与含逗号项；合法零命中 pattern 不要求预知指标 catalog | Ginkgo failure | `go test ./internal/kubestatemetrics` |
| `UT-KSM-012` | #54 | 纯规划 | 1.19、1.28 | 显式 `podResources` 完整下发 requests/limits，不伪造项目默认值 | Ginkgo failure | `go test ./internal/kubestatemetrics` |
| `UT-KSM-013` | #54 | 纯规划 | 1.19、1.28 | 非空 `podResources.claims` 因缺少 Pod `resourceClaims` 声明入口而在资源规划前返回 `InvalidSpec` | Ginkgo failure | `go test ./internal/kubestatemetrics` |
| `UT-KSM-014` | #55 | 纯规划 | 1.19、1.28 | 字面名称与完整 RE2 匹配结果组成排序去重并集，substring 不命中，并统一生成 namespace 参数 | Ginkgo failure | `go test ./internal/kubestatemetrics` |
| `UT-KSM-015` | #55 | 纯规划 | 1.19、1.28 | 省略 namespaceFilter 时按 All 展开全部当前 Namespace | Ginkgo failure | `go test ./internal/kubestatemetrics` |
| `UT-KSM-016` | #55 | 纯规划 | 1.19、1.28 | 拒绝 All 携带过滤项、空 Include、未知 mode、非法字面名称与非法 RE2，返回 InvalidSpec 且零资源 | Ginkgo failure | `go test ./internal/kubestatemetrics` |
| `UT-KSM-017` | #55 | 纯规划 | 1.19、1.28 | 零匹配只规划非 workload 资源，同时保留待删除 workload 的稳定身份 | Ginkgo failure | `go test ./internal/kubestatemetrics` |
| `UT-KSM-018` | #55 | 纯规划 | 通用 | 非法 namespace regex 优先于缺失 targetNamespace 报告 InvalidSpec | Ginkgo failure | `go test ./internal/kubestatemetrics` |
| `UT-KSM-019` | #55 | 纯规划 | 1.19、1.28 | Namespace 集合增删使计划在零匹配、扩展、归零与恢复之间确定性切换 | Ginkgo failure | `go test ./internal/kubestatemetrics` |
| `UT-KSM-GH53-001` | #53 | 纯规划/命名 | 通用 | CR name 是唯一业务身份；点号替换或超长时使用可读前缀与原始名称 SHA-256 前 8 位，基础名最长 47 字符且 targetNamespace 不参与计算 | Ginkgo failure | `go test ./internal/kubestatemetrics` |
| `UT-KSM-GH53-002` | #53 | 纯规划/资源元数据 | 1.28 | 全部直接受管资源和 Pod 使用完整稳定 labels；Service selector 仅使用 name、instance 与 endpoint-group，不使用 version 或 role | Ginkgo failure | `go test ./internal/kubestatemetrics` |
| `UT-KSM-GH53-003` | #53 | controller/Kubernetes I/O | 通用 | finalizer 删除带有已分类对象 UID 前置条件，竞态窗口中的同名冲突替换对象不被误删；managedResources 不参与所有权判定 | Go test failure | `go test ./internal/controller` |
| `UT-KSM-GH53-004` | #53 | controller/Kubernetes I/O | 通用 | 模糊部分写入返回 `ApplyFailed`，已持久化资源不回滚，后续调谐从当前状态继续收敛 | Go test failure | `go test ./internal/controller` |
| `UT-KSM-GH53-005` | #53 | controller/Kubernetes I/O | 通用 | 同 Namespace 多实例的稳定身份和 owner UID 隔离；已拥有 Service 漂移恢复；旧 UID 冲突保持原样并可恢复；停用一个实例不影响另一个 | Go test failure | `go test ./internal/controller` |
| `UT-KSM-GH56-001` | #56 | 纯规划 | 1.19、1.28 | 判别 union 拒绝 mode/分支不匹配与 `shards < 2`，返回 `InvalidSpec` 且零资源 | Ginkgo failure | `go test ./internal/kubestatemetrics` |
| `UT-KSM-GH56-002` | #56 | 纯规划 | 1.19、1.28 | 规划指定副本数的 StatefulSet、自动分片参数、单一 headless Service、Role/RoleBinding，并统一继承 collector/metric/namespace/resources | Ginkgo failure | `go test ./internal/kubestatemetrics` |
| `UT-KSM-GH56-003` | #56 | controller 边界 | 通用 | 旧 Deployment 删除失败时不创建 StatefulSet，status 同时报告 `SwitchingDeploymentMode` 阶段、`DeleteFailed` 与真实旧 workload 可用性 | Go test failure、status | `go test ./internal/controller` |
| `UT-KSM-GH56-004` | #56 | controller 边界 | 通用 | 旧 workload 读取失败时保留上次观测事实，status 以当前 generation 报告 `SwitchingDeploymentMode` 和 `ObservationFailed` | Go test failure、status | `go test ./internal/controller` |
| `UT-KSM-GH56-005` | #56 | controller 边界 | 通用 | 切回 Deployment 时读取待清理分片 RBAC 失败，status 仍报告 `SwitchingDeploymentMode` 和 `ObservationFailed` | Go test failure、status | `go test ./internal/controller` |
| `UT-KSM-GH56-006` | #56 | controller 边界 | 通用 | 资源写入后读取 StatefulSet 可用副本失败，status 报告当前 generation 的 `ObservationFailed` 而非留下成功或空白状态 | Go test failure、status | `go test ./internal/controller` |
| `UT-KSM-GH56-007` | #56 | controller 边界 | 通用 | topology 所有权预检的 API 读取失败时，status 仍报告当前 generation 的 `SwitchingDeploymentMode` 和 `ObservationFailed` | Go test failure、status | `go test ./internal/controller` |
| `UT-KSM-GH56-008` | #56 | controller 边界 | 通用 | 检查终止中受管资源的 API 读取失败时，status 仍报告当前 generation 的 `SwitchingDeploymentMode` 和 `ObservationFailed` | Go test failure、status | `go test ./internal/controller` |
| `UT-KSM-GH56-009` | #56 | controller 边界 | 通用 | 资源问题已识别但回写 status 前的实际 topology 观测又失败时，以 `ObservationFailed` 落盘切换阶段 | Go test failure、status | `go test ./internal/controller` |
| `UT-KSM-GH57-001` | #57 | 纯规划 | Kubernetes 1.19 | 合法 DaemonSetSharded 返回 `UnsupportedMode` 且零资源计划 | Ginkgo failure | `go test ./internal/kubestatemetrics` |
| `UT-KSM-GH57-002` | #57 | 纯规划 | Kubernetes 1.28 | 规划 node DaemonSet、cluster/unscheduled companion Deployments、两组 Service，并统一过滤结果与整体 resources 覆盖 | Ginkgo failure | `go test ./internal/kubestatemetrics` |
| `UT-KSM-GH57-003` | #57 | 纯规划 | 1.19、1.28 | 判别 union、collector 组合与三角色 resourceClaims 静态错误优先返回 `InvalidSpec` | Ginkgo failure | `go test ./internal/kubestatemetrics` |
| `UT-KSM-GH57-004` | #57 | E2E 编排 | 本地 gate | Operator 先由宿主机工具链构建 Linux 二进制，再由 runtime-only Dockerfile 打包并导入 kind；镜像准备在 Ginkgo 进程前完成且不拉取编译器镜像 | Make dry-run 命令顺序 | `go test ./test/e2e/kubestatemetrics -run '^TestKubeStateMetricsE2EOrchestration$'` |
| `UT-EXAMPLE-GH59-001` | #59 | 示例/RBAC | 通用 | 原生 Agent 示例为四类配置使用非空 object/namespace selector，ScrapeConfig 覆盖 static 与五种一期 Kubernetes discovery；统一 runtime ClusterRole 包含所需资源与 `/metrics` 且不含 nodes | Ginkgo failure、示例 manifest | `go test ./test/testenv` |
| `UT-COLLECTION-GH59-001` | #59 | E2E 支撑 | 通用 | kubectl 将授权结果写入 stdout、将集群级资源 scope warning 写入 stderr 时，两条流保持分离且拒绝退出状态不丢失 | Go testing failure、stdout/stderr、退出码 | `go test ./test/e2e/collection -run '^TestCollectionHelpers$'` |
| `UT-COLLECTION-GH59-002` | #59 | E2E 支撑 | 通用 | 从格式化 EventList JSON 结构化读取 `InvalidConfiguration` reason，且畸形 JSON 显式报错 | Go testing failure、Event JSON | `go test ./test/e2e/collection -run '^TestCollectionHelpers$'` |
| `UT-PREFLIGHT-GH59-001` | #59 | 预检 CLI | 通用 | 空 object selector 保持上游合法能力，但严格预检以非零退出拒绝将其报告为一期支持 | NDJSON diagnostics、退出码 | `go test ./test/testenv` |
| `UT-PREFLIGHT-GH59-002` | #59 | 预检 CLI | 通用 | Node、node metadata、FileSD、DNS、LightSail 代表的一期外 ScrapeConfig discovery 均被非零拒绝 | NDJSON diagnostics、退出码 | `go test ./test/testenv` |
| `UT-PREFLIGHT-GH59-003` | #59 | 预检 CLI | 通用 | 空 namespace selector 解析为 Operator watch scope 内全部 namespace，声明的配置范围覆盖不足时非零退出 | NDJSON diagnostics、退出码 | `go test ./test/testenv` |
| `UT-PREFLIGHT-GH59-004` | #59 | 预检 CLI | 通用 | 采集配置的 Secret key selector 只在配置自身 namespace 检查，引用缺失时非零退出 | NDJSON diagnostics、退出码 | `go test ./test/testenv` |
| `UT-PREFLIGHT-GH59-005` | #59 | 预检 CLI | 通用 | Namespace LabelSelector 的 NotIn 保持 Kubernetes 原生语义，label 不存在时仍匹配 | NDJSON diagnostics | `go test ./test/testenv` |
| `UT-PREFLIGHT-GH59-006` | #59 | 预检 CLI | 通用 | 空 namespace selector 的覆盖集合与 Prometheus Operator 实际 common watch scope 求交，不误要求 scope 外 namespace | NDJSON diagnostics | `go test ./test/testenv` |
| `UT-PREFLIGHT-GH59-007` | #59 | 预检 CLI | 通用 | ConfigMap key selector 只查询采集配置自身 namespace，跨 namespace 同名对象不能满足引用 | NDJSON diagnostics、退出码 | `go test ./test/testenv` |
| `UT-PREFLIGHT-GH59-008` | #59 | 预检 CLI | 通用 | Secret/ConfigMap 对象存在但缺少 key selector 引用的 key 时非零退出 | NDJSON diagnostics、退出码 | `go test ./test/testenv` |
| `UT-PREFLIGHT-GH59-009` | #59 | 预检 CLI | 通用 | static config 的普通 labels 即使同时包含 `name`/`key` 也不被误判为 Secret/ConfigMap key selector | NDJSON diagnostics | `go test ./test/testenv` |
| `UT-PREFLIGHT-GH59-010` | #59 | 预检 CLI | 通用 | ScrapeConfig Kubernetes discovery 省略 `namespaces` 时保持全 namespace 原生语义，单 namespace runtime 覆盖以非零退出拒绝 | NDJSON diagnostics、退出码 | `go test ./test/testenv` |
| `UT-PREFLIGHT-GH59-011` | #59 | 预检 CLI | 通用 | ScrapeConfig Kubernetes discovery 的 `ownNamespace` 指向 Agent namespace，而不是 ScrapeConfig 对象所在 namespace | NDJSON diagnostics | `go test ./test/testenv` |
| `UT-PREFLIGHT-GH60-001` | #60 | 预检 CLI | 通用 | `replicas>1` 保持上游可用但在任何集群访问前被一期预检非零拒绝 | NDJSON diagnostics、退出码 | `go test ./test/testenv` |
| `UT-PREFLIGHT-GH60-002` | #60 | 预检 CLI | 通用 | v0.83.0 配置 Secret 保留名被无关对象占用时报告 `ReservedResourceConflict` 且在 reconcile probe 前退出 | NDJSON diagnostics、退出码 | `go test ./test/testenv` |
| `UT-PREFLIGHT-GH60-003` | #60 | 预检 CLI | 通用 | 同 namespace 其他 Agent 的普通 ownerReferences 与精确 headless 结构被识别为合法共享 `prometheus-agent-operated`；尊重自定义 `serviceName` 与 `portName` | NDJSON diagnostics | `go test ./test/testenv` |
| `UT-PREFLIGHT-GH60-004` | #60 | 预检 CLI/PVC | 通用 | retained PVC 名称、Agent/shard labels、计划 accessModes/volumeMode、Kubernetes Quantity 等价容量与显式 StorageClass 匹配时允许恢复；缺省与显式空 StorageClass 保持不同语义，属性不匹配时非零拒绝；不存在 retained claim 时不收窄上游 storage 能力 | NDJSON diagnostics、退出码 | `go test ./test/testenv` |
| `UT-PREFLIGHT-GH60-005` | #60 | 预检 CLI | 通用 | `shards` 必须是至少为 1 的整数，非法值在资源名规划和任何集群访问前非零拒绝 | NDJSON diagnostics、退出码 | `go test ./test/testenv` |
| `UT-PREFLIGHT-GH60-006` | #60 | 预检 CLI | 通用 | Pod owner 名称相同但 StatefulSet UID 已过期时仍报告 `ReservedResourceConflict`，不把遗留 Pod 误认为当前 Agent 资源 | NDJSON diagnostics、退出码 | `go test ./test/testenv` |
| `UT-EXAMPLE-GH60-001` | #60 | 示例/RBAC | 通用 | 每 Agent 独立 namespaced ServiceAccount 和独立 ClusterRoleBinding 绑定共享 runtime ClusterRole，三类管理员资源都无 Agent ownerReference | Ginkgo failure、示例 manifest | `go test ./test/testenv` |
| `UT-BOUNDARY-GH60-001` | #60 | 安装/RBAC 边界 | 通用 | Helm 机械同步的 manager ClusterRole 对 `monitoring.coreos.com` 整个 API group 无权限，不能读写 Agent 或四类采集配置 | Ginkgo failure、RBAC manifest | `go test ./test/testenv` |
| `UT-BOUNDARY-GH60-002` | #60 | 双版本 CI/反向边界 | 通用 | blocking CI 先完成无副作用预检，再安装当前工作树 Operator，最后在运行中控制器下执行 Agent 资源快照断言 | Ginkgo failure、workflow 阶段顺序 | `go test ./test/testenv` |
| `UT-BOUNDARY-GH60-003` | #60 | manager cache/反向读取边界 | 通用 | Deployment/StatefulSet/Service 与 RBAC 的 Owns informer 只 list/watch `managed-by=monitor-in-hand`；精确 KSM 身份 Get 直读以保留无标签同名冲突检测，不把 Agent workload/runtime RBAC 载入缓存 | Ginkgo failure、manager cache/client options | `go test ./internal/operator` |
| `UT-PREFLIGHT-GH59-012` | #59 | 预检 CLI | 通用 | ServiceMonitor、PodMonitor 与 Probe ingress 的 target namespace 超出声明的 runtime discovery 范围时非零退出 | NDJSON diagnostics、退出码 | `go test ./test/testenv` |

## HELM

| 场景 ID | 来源 | 层级 | 目标版本 | 动作与精确断言 | 失败证据 | 测试入口 |
| --- | --- | --- | --- | --- | --- | --- |
| `HELM-001` | #49 | Helm/控制面 | Chart | 渲染唯一单副本 Operator Deployment，并固定滚动策略、Linux 约束、安全上下文、leader election 与 probes | Ginkgo failure、rendered manifest | `make test-helm HELM=helm` |
| `HELM-002` | #49 | Helm/RBAC | Chart | Chart 使用生成的最小 ClusterRole，cluster-scoped binding 正确，且不包含 Prometheus Operator API 权限 | Ginkgo failure、rendered manifest | `make test-helm HELM=helm` |
| `HELM-003` | #49 | Helm/健康端点 | Chart | health listener、container port、liveness 与 readiness 使用同一可配置端口 | Ginkgo failure、rendered manifest | `make test-helm HELM=helm` |
| `HELM-004` | #49 | Helm/values schema | Chart | render 阶段拒绝特权或越界端口、不支持的日志级别、Windows selector 冲突与未声明 values | Ginkgo failure、Helm error | `make test-helm HELM=helm` |
| `HELM-005` | #49 | Helm/配置面 | Chart | 仅开放约定的 scheduling、resources、health port 与 log level，且自定义值准确下发 | Ginkgo failure、rendered manifest | `make test-helm HELM=helm` |
| `HELM-006` | #51、#52、#54、#56、#57 | Helm/生成物 | Chart | Helm CRD 与 Kubebuilder 生成源逐字同步，并包含 lifecycle、deployment 判别 union、collectors、metricFilter、podResources 的 schema/defaults 且不暴露原始参数透传 | Ginkgo failure、rendered manifest | `make test-helm HELM=helm` |
| `HELM-007` | #51、#52、#56、#57 | Helm/RBAC | Chart | manager 具有 CR status/finalizer、Deployment/StatefulSet/DaemonSet、cluster/namespaced RBAC 与生命周期清理所需权限，且不含 Prometheus Operator API 权限 | Ginkgo failure、rendered manifest | `make test-helm HELM=helm` |
| `HELM-GH57-001` | #57 | Helm/CRD | Chart | 发布 CRD 暴露 DaemonSetSharded 判别分支和三个角色级 resources 整体覆盖字段 | Ginkgo failure、rendered manifest | `make test-helm HELM=helm` |

## IT

| 场景 ID | 来源 | 层级 | 目标版本 | 动作与精确断言 | 失败证据 | 测试入口 |
| --- | --- | --- | --- | --- | --- | --- |
| `IT-KSM-001` | #51 | envtest | API Server 1.28 | 调谐五类直接受管资源；API defaults、ownerReference、managedResources 与 Deployment availability 正确反映到 status/conditions | Ginkgo failure、API object | `make test-integration` |
| `IT-KSM-002` | #51 | envtest | 1.27 拒绝、1.28 缺依赖 | observedGeneration 与稳定 reason 正确，受管身份零写入 | Ginkgo failure、API object | `make test-integration` |
| `IT-KSM-003` | #51 | envtest | API Server 1.28 | 首次成功后修改 targetNamespace 返回 `InvalidSpec`，新 Namespace 零写入 | Ginkgo failure、API object | `make test-integration` |
| `IT-KSM-004` | #51 | envtest | API Server 1.28 | 同名非受管身份返回 `ResourceConflict`；冲突对象保持原样且其余计划资源零写入 | Ginkgo failure、API object | `make test-integration` |
| `IT-KSM-005` | #51 | envtest | API Server 1.28 | admission 拒绝缺少整个 `spec` 或缺少必填 `targetNamespace` 的声明 | Ginkgo failure、API error | `make test-integration` |
| `IT-KSM-006` | #52 | envtest | API Server 1.28 | 停用优先于无效普通声明，删除全部五类直接受管资源后进入 Disabled，并按最新 generation 恢复 | Ginkgo failure、API object | `make test-integration` |
| `IT-KSM-007` | #52 | envtest | API Server 1.28 | 挂起只删除 workload，保留 Service、ServiceAccount 与 RBAC，取消挂起后重建 workload | Ginkgo failure、API object | `make test-integration` |
| `IT-KSM-008` | #52 | envtest | API Server 1.28 | 删除优先于无效声明，finalizer 阻塞直到全部直接受管资源确认消失 | Ginkgo failure、API object | `make test-integration` |
| `IT-KSM-009` | #52 | envtest | API Server 1.28 | 停用删除发出后立刻重新启用时先等待旧资源消失，再按最新 generation 重建 | Ginkgo failure、API object | `make test-integration` |
| `IT-KSM-010` | #54 | envtest | API Server 1.28；目标 profile 1.19、1.28 | API 配置贯通 Deployment args/resources 与 status effective collectors；已应用配置变为非法后 workload 不被重写 | Ginkgo failure、API object | `make test-integration` |
| `IT-KSM-011` | #54 | envtest | API Server 1.28 | 非法配置优先于缺失 Namespace 返回 InvalidSpec；零资源、无定时重试、相同 generation 不重复写 status，其他实例不受影响，新 generation 可恢复 | Ginkgo failure、API object | `make test-integration` |
| `IT-KSM-012` | #54 | envtest | API Server 1.28；目标 profile 1.19、1.28 | Deny filter 映射为两个版本 workload 的 metric denylist，effective collectors 与参数一致 | Ginkgo failure、API object | `make test-integration` |
| `IT-KSM-013` | #55 | envtest | API Server 1.28 | 从集群 Namespace 解析字面与完整 regex 并集，workload 参数与 effectiveNamespaceCount 一致 | Ginkgo failure、API object | `make test-integration` |
| `IT-KSM-014` | #55 | envtest | API Server 1.28 | 零匹配删除 workload、保留 Service/ServiceAccount/RBAC 并报告 NoNamespaceMatched，匹配恢复后重建 | Ginkgo failure、API object | `make test-integration` |
| `IT-KSM-015` | #55 | envtest | API Server 1.28 | 已应用配置变为非法 regex 时只更新 InvalidSpec status，不部分修改既有 workload | Ginkgo failure、API object | `make test-integration` |
| `IT-KSM-016` | #55 | envtest | API Server 1.28 | Namespace watch 自动触发范围扩展、rollout、归零和恢复 | Ginkgo failure、API object | `make test-integration` |
| `IT-KSM-017` | #55 | envtest | API Server 1.28 | workload 仍在终止时持续 requeue，不提前报告稳定零匹配 | Ginkgo failure、API object | `make test-integration` |
| `IT-KSM-GH53-001` | #53 | envtest | API Server 1.28 | 同一与不同 targetNamespace 的三实例保持 workload、Service selector、RBAC、status 和停用生命周期隔离 | Ginkgo failure、API object | `make test-integration` |
| `IT-KSM-GH53-002` | #53 | envtest | API Server 1.28 | 无 owner、其他 owner 与同名旧 CR UID 对象均报告 `ResourceConflict`，不修改、删除或接管；冲突消失后恢复收敛 | Ginkgo failure、API object | `make test-integration` |
| `IT-KSM-GH53-003` | #53 | envtest | API Server 1.28 | finalizer 只清理当前 CR UID 的稳定直接身份，保留旧 UID 同名对象和仅出现于 managedResources 的对象 | Ginkgo failure、API object | `make test-integration` |
| `IT-KSM-GH53-004` | #53 | envtest | API Server 1.28 | 真实 API Server 中的多资源部分写入保留成功对象、报告 `ApplyFailed` 并在后续调谐补齐剩余资源 | Ginkgo failure、API object | `make test-integration` |
| `IT-KSM-GH53-005` | #53 | envtest/manager watch | API Server 1.28 | 修改已拥有 Service 或删除已拥有 Deployment 时由直接资源 watch 自动触发调谐、恢复字段并以新 UID 重建 | Ginkgo failure、API object | `make test-integration` |
| `IT-KSM-GH56-001` | #56 | envtest | API Server 1.28；目标 profile 1.19、1.28 | 实际写入 StatefulSet、Service、Role/RoleBinding，并从 StatefulSet availability 回写七个受管资源与 Pod status | Ginkgo failure、API object | `make test-integration` |
| `IT-KSM-GH56-002` | #56 | envtest | API Server 1.28 | 用 workload finalizer 验证 Deployment/StatefulSet 往返 delete-before-create，等待期间不改 Service selector 或模式 RBAC | Ginkgo failure、API object | `make test-integration` |
| `IT-KSM-GH56-003` | #56 | envtest | API Server 1.28 | `shards=1` 被 controller 报告为 `InvalidSpec`，不写入受管资源 | Ginkgo failure、API object | `make test-integration` |
| `IT-KSM-GH56-004` | #56 | envtest | API Server 1.28 | 切回 Deployment 时如果分片 Role 已不属于当前 UID，则预检报告 `ResourceConflict`、保留 RoleBinding 并不创建 workload | Ginkgo failure、API object | `make test-integration` |
| `IT-KSM-GH56-005` | #56 | envtest | API Server 1.28 | 全新默认 Deployment 未应用分片拓扑时，无关的同名 Role/RoleBinding 不会触发分片清理或阻断 workload | Ginkgo failure、API object | `make test-integration` |
| `IT-KSM-GH57-001` | #57 | envtest | API Server 1.28 | 实际写入三职责、两组 Service，并聚合 DaemonSet 与两个 companion Deployment 的真实 Pod 可用性 | Ginkgo failure、API object、status | `make test-integration` |
| `IT-KSM-GH57-002` | #57 | envtest | 目标 profile 1.19 | 报告 `UnsupportedMode`，保留占据预期身份的哨兵对象且不创建其他受管资源 | Ginkgo failure、API object、status | `make test-integration` |
| `IT-KSM-GH57-003` | #57 | envtest | API Server 1.28 | DaemonSetSharded 与 Deployment 往返切换先删除全部旧 workload，再清理模式 Service 并创建新拓扑 | Ginkgo failure、API object、status | `make test-integration` |

## E2E

| 场景 ID | 来源 | 层级 | 目标版本 | 动作与精确断言 | 失败证据 | 测试入口 |
| --- | --- | --- | --- | --- | --- | --- |
| `E2E-119-KSM-001` | #51 | kind E2E | Kubernetes 1.19.16 | 最小 CR 实际运行 kube-state-metrics 2.3.0；验证 Deployment/Service/RBAC/labels/owner/status | 完整集群对象、Events 与 Operator logs | `make e2e-default-deployment KUBERNETES_VERSION=1.19 ...` |
| `E2E-128-KSM-001` | #51 | kind E2E | Kubernetes 1.28.15 | 最小 CR 实际运行 kube-state-metrics 2.11.0；验证 Deployment/Service/RBAC/labels/owner/status | 完整集群对象、Events 与 Operator logs | `make e2e-default-deployment KUBERNETES_VERSION=1.28 ...` |
| `E2E-119-KSM-002` | #52 | kind E2E | Kubernetes 1.19.16 | 真实集群覆盖停用、恢复、挂起、取消挂起和 finalizer 删除阻塞 | 完整集群对象、conditions、Events 与 Operator logs | `make e2e-default-deployment KUBERNETES_VERSION=1.19 ...` |
| `E2E-128-KSM-002` | #52 | kind E2E | Kubernetes 1.28.15 | 真实集群覆盖停用、恢复、挂起、取消挂起和 finalizer 删除阻塞 | 完整集群对象、conditions、Events 与 Operator logs | `make e2e-default-deployment KUBERNETES_VERSION=1.28 ...` |
| `E2E-119-KSM-003` / `E2E-128-KSM-003` | #54 | kind E2E | Kubernetes 1.19.16、1.28.15 | Include pods、未锚定 Allow pattern 和显式 podResources 作用于真实 workload 与 scrape | 完整集群对象、Events、metrics 与 Operator logs | `make e2e-default-deployment KUBERNETES_VERSION=<minor> ...` |
| `E2E-119-KSM-004` / `E2E-128-KSM-004` | #54 | kind E2E | Kubernetes 1.19.16、1.28.15 | Deny pattern 从真实 scrape 排除匹配指标，同时保留其他 collector 指标 | 完整集群对象、Events、metrics 与 Operator logs | `make e2e-default-deployment KUBERNETES_VERSION=<minor> ...` |
| `E2E-119-KSM-005` / `E2E-128-KSM-005` | #54 | kind E2E | Kubernetes 1.19.16、1.28.15 | 非法 regexp 仅降级当前实例、无 workload/status 重写，并保持 Operator 和合法实例可用 | 完整集群对象、Events 与 Operator logs | `make e2e-default-deployment KUBERNETES_VERSION=<minor> ...` |
| `E2E-119-KSM-006` / `E2E-128-KSM-006` | #55 | kind E2E | Kubernetes 1.19.16、1.28.15 | 字面名称、完整 regex、substring 非命中、集合扩展、零匹配及自动恢复作用于真实可用 workload 与 KSM 指标 | 完整集群对象、conditions、Events、metrics 与 Operator logs | `make e2e-default-deployment KUBERNETES_VERSION=<minor> ...` |
| `E2E-119-KSM-GH53-001` / `E2E-128-KSM-GH53-001` | #53 | kind E2E | Kubernetes 1.19.16、1.28.15 | 点号名称的稳定摘要身份；同/不同 Namespace 多实例的 workload、Service endpoints、Pod labels、RBAC 与生命周期隔离；已拥有资源修改/删除漂移自动修复；无 owner、错误 owner 与旧 UID 预占均报告 `ResourceConflict` 并恢复；finalizer 保留旧 UID 对象；真实 RBAC 失败下保留部分写入并继续收敛 | 完整集群对象、endpoints、Pods、conditions、Events 与 Operator logs | `make e2e-default-deployment KUBERNETES_VERSION=<minor> ...` |
| `E2E-119-KSM-GH56-001` / `E2E-128-KSM-GH56-001` | #56 | kind E2E | Kubernetes 1.19.16、1.28.15 | 真实两分片 StatefulSet 暴露两个 endpoints，逐 Pod telemetry 报告唯一 ordinal/total-shards，每个 Pod 继承过滤/资源，RBAC/status 正确并完成模式往返 | 完整集群对象、conditions、Endpoints、telemetry 与 Operator logs | `make e2e-kubestatemetrics KUBERNETES_VERSION=<minor> ...` |
| `E2E-119-KSM-GH56-002` / `E2E-128-KSM-GH56-002` | #56 | kind E2E | Kubernetes 1.19.16、1.28.15 | `shards=1` 在真实集群报告 `InvalidSpec` 且零受管资源 | 完整集群对象、conditions 与 Operator logs | `make e2e-kubestatemetrics KUBERNETES_VERSION=<minor> ...` |
| `E2E-119-KSM-GH57-001` / `E2E-128-KSM-GH57-001` | #57 | kind E2E | Kubernetes 1.19.16、1.28.15 | 1.19 对 DaemonSetSharded 报告 `UnsupportedMode` 且零写入；1.28 验证两组 endpoints、三角色参数与资源覆盖、指标职责和模式往返切换 | 完整集群对象、endpoints、metrics、conditions 与 Operator logs | `make e2e-kubestatemetrics KUBERNETES_VERSION=<minor> ...` |
| `E2E-119-PREFLIGHT-001` | #58 | kind E2E/预检 | Kubernetes 1.19.16 | 一期支持环境使用显式 runtime ServiceAccount 时预检成功，并报告 Kubernetes 版本事实与 `PreflightPassed` | NDJSON diagnostics、集群对象 | `make e2e-preflight KUBERNETES_VERSION=1.19 ...` |
| `E2E-128-PREFLIGHT-001` | #58 | kind E2E/预检 | Kubernetes 1.28.15 | 一期支持环境使用显式 runtime ServiceAccount 时预检成功，并报告 Kubernetes 版本事实与 `PreflightPassed` | NDJSON diagnostics、集群对象 | `make e2e-preflight KUBERNETES_VERSION=1.28 ...` |
| `E2E-119-PREFLIGHT-002` | #58 | kind E2E/预检 | Kubernetes 1.19.16 | runtime ServiceAccount 缺失时预检非零退出，并报告 `RuntimeServiceAccountNotFound` 与 `PreflightFailed` | NDJSON diagnostics、退出码 | `make e2e-preflight KUBERNETES_VERSION=1.19 ...` |
| `E2E-128-PREFLIGHT-002` | #58 | kind E2E/预检 | Kubernetes 1.28.15 | runtime ServiceAccount 缺失时预检非零退出，并报告 `RuntimeServiceAccountNotFound` 与 `PreflightFailed` | NDJSON diagnostics、退出码 | `make e2e-preflight KUBERNETES_VERSION=1.28 ...` |
| `E2E-119-PREFLIGHT-GH59-001` / `E2E-128-PREFLIGHT-GH59-001` | #59 | kind E2E/计划预检 | Kubernetes 1.19.16、1.28.15 | 四类显式非空 selector 与 static、Pod、Service、Endpoints、EndpointSlice、Ingress discovery 计划在全 namespace runtime 覆盖下通过严格预检 | NDJSON diagnostics、退出码 | `make e2e-preflight KUBERNETES_VERSION=<minor> ...` |
| `E2E-119-PREFLIGHT-GH59-002` / `E2E-128-PREFLIGHT-GH59-002` | #59 | kind E2E/计划预检 | Kubernetes 1.19.16、1.28.15 | 上游合法空 object selector 被非零拒绝且报告 `EmptyObjectSelectorUnsupported` | NDJSON diagnostics、退出码 | `make e2e-preflight KUBERNETES_VERSION=<minor> ...` |
| `E2E-119-COLLECTION-GH59-001` / `E2E-128-COLLECTION-GH59-001` | #59 | kind E2E/配置选择 | Kubernetes 1.19.16、1.28.15 | 四类 object selector 相互独立；全 nil Agent 保持 Reconciled/Available 且生成零 scrape job；四类各自用 namespace selector 的 nil、空对象和非空 label 分别选择当前、全部和标签 namespace | generated config、Agent status | `make e2e-collection KUBERNETES_VERSION=<minor> ...` |
| `E2E-119-COLLECTION-GH59-002` / `E2E-128-COLLECTION-GH59-002` | #59 | kind E2E/真实 targets | Kubernetes 1.19.16、1.28.15 | ServiceMonitor、PodMonitor、Probe 与 ScrapeConfig 均产生真实 up target；ScrapeConfig 覆盖 static、Pod、Service、Endpoints、EndpointSlice、Ingress，并验证 ServiceMonitor 的 Endpoints/EndpointSlice 版本差异 | generated config、Agent status API、targets API | `make e2e-collection KUBERNETES_VERSION=<minor> ...` |
| `E2E-119-COLLECTION-GH59-003` / `E2E-128-COLLECTION-GH59-003` | #59 | kind E2E/拒绝诊断 | Kubernetes 1.19.16、1.28.15 | 上游接受但 selector role 不兼容的 ScrapeConfig 不进入 generated config，并通过 InvalidConfiguration Event 与 Prometheus Operator logs 定位 | generated config、Events、Operator logs | `make e2e-collection KUBERNETES_VERSION=<minor> ...` |
| `E2E-119-PREFLIGHT-GH60-001` / `E2E-128-PREFLIGHT-GH60-001` | #60 | kind E2E/资源身份预检 | Kubernetes 1.19.16、1.28.15 | 两分片 Agent 计划在保留名空闲且已有默认 Service 由同 namespace 其他 Agent 合法普通拥有时通过严格预检 | NDJSON diagnostics、退出码、集群对象 | `make e2e-preflight KUBERNETES_VERSION=<minor> ...` |
| `E2E-119-PREFLIGHT-GH60-002` / `E2E-128-PREFLIGHT-GH60-002` | #60 | kind E2E/冲突预检 | Kubernetes 1.19.16、1.28.15 | StatefulSet、Pod、配置/TLS/web Secrets 保留名、冲突 governing Service 与属性不匹配 retained PVC 均非零拒绝，占用对象的 UID、spec/data 与 owner graph 全部不变 | NDJSON diagnostics、退出码、对象内容快照 | `make e2e-preflight KUBERNETES_VERSION=<minor> ...` |
| `E2E-119-COLLECTION-GH60-001` / `E2E-128-COLLECTION-GH60-001` | #60 | kind E2E/Agent 多实例 | Kubernetes 1.19.16、1.28.15 | 当前工作树 Operator 实际运行时，同 namespace 不同名、跨 namespace 同名 Agent 的 StatefulSet/Pod/Secrets/PVC 隔离与共享 Service；独立 SA/CRB 共享 runtime ClusterRole；1/2 shards 默认分片真实 targets；重叠配置产生两套 remote_write series；挂起和删除不影响其他实例；快照明确验证 Agent、配置、runtime RBAC 与外部生成资源未被本 Operator 修改、拥有或清理 | Agent status/API、Kubernetes 对象身份与内容快照、targets API、VictoriaMetrics query | `make e2e-collection KUBERNETES_VERSION=<minor> ...` |
| `E2E-119-COLLECTION-GH60-002` / `E2E-128-COLLECTION-GH60-002` | #60 | kind E2E/副本边界 | Kubernetes 1.19.16、1.28.15 | 同一份 `replicas=2` Agent 由 Prometheus Operator v0.83.0 实际收敛为两个 Ready Pod，但一期严格预检以 `UnsupportedAgentReplicas` 非零拒绝 | Agent conditions、StatefulSet/Pod、NDJSON diagnostics、退出码 | `make e2e-collection KUBERNETES_VERSION=<minor> ...` |
| `E2E-119-BOUNDARY-GH60-001` / `E2E-128-BOUNDARY-GH60-001` | #60 | kind E2E/反向边界 | Kubernetes 1.19.16、1.28.15 | 在当前工作树 Operator 启动前快照 Agent、四类采集配置、runtime RBAC 及外部 StatefulSet/Pod/Secrets/PVC/Service；启动后持续验证 UID、spec/data 与 owner graph 不变 | Operator Deployment、启动前后对象内容快照 | `make e2e-default-deployment KUBERNETES_VERSION=<minor> ...` |
