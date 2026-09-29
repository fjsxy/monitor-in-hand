# 一期验收矩阵

本文件记录已经自动化并作为 blocking gate 执行的稳定场景。后续 ticket 追加自身场景，不改写已有 ID。

| 场景 ID | 来源 | 层级 | 目标版本 | 动作与精确断言 | 失败证据 | 测试入口 |
| --- | --- | --- | --- | --- | --- | --- |
| `HELM-001` | #49 | Helm/控制面 | Chart | 渲染唯一单副本 Operator Deployment，并固定滚动策略、Linux 约束、安全上下文、leader election 与 probes | Ginkgo failure、rendered manifest | `make test-helm HELM=helm` |
| `HELM-002` | #49 | Helm/RBAC | Chart | Chart 使用生成的最小 ClusterRole，cluster-scoped binding 正确，且不包含 Prometheus Operator API 权限 | Ginkgo failure、rendered manifest | `make test-helm HELM=helm` |
| `HELM-003` | #49 | Helm/健康端点 | Chart | health listener、container port、liveness 与 readiness 使用同一可配置端口 | Ginkgo failure、rendered manifest | `make test-helm HELM=helm` |
| `HELM-004` | #49 | Helm/values schema | Chart | render 阶段拒绝特权或越界端口、不支持的日志级别、Windows selector 冲突与未声明 values | Ginkgo failure、Helm error | `make test-helm HELM=helm` |
| `HELM-005` | #49 | Helm/配置面 | Chart | 仅开放约定的 scheduling、resources、health port 与 log level，且自定义值准确下发 | Ginkgo failure、rendered manifest | `make test-helm HELM=helm` |
| `UT-KSM-001` | #51 | 纯规划 | 1.19、1.28 | 最小 CR 选择固定 profile，并规划单副本 Deployment、headless Service、独立 SA/ClusterRole/ClusterRoleBinding、稳定 labels 与当前 UID ownerReference | Ginkgo failure | `go test ./internal/kubestatemetrics` |
| `UT-KSM-002` | #51 | 纯规划 | 其他 minor | 返回 `UnsupportedKubernetesVersion`，资源计划为空 | Ginkgo failure | `go test ./internal/kubestatemetrics` |
| `UT-KSM-003` | #51 | 纯规划 | 1.28 | 目标 Namespace 不存在时返回 `DependencyMissing`，资源计划为空 | Ginkgo failure | `go test ./internal/kubestatemetrics` |
| `UT-KSM-004` | #51 | 纯规划 | 1.28 | 首次应用后 targetNamespace 变化返回 `InvalidSpec`，资源计划为空 | Ginkgo failure | `go test ./internal/kubestatemetrics` |
| `UT-KSM-005` | #51 | 纯规划/RBAC | 1.19、1.28 | Operator ClusterRole 覆盖两个固定 profile 下放给 kube-state-metrics 的全部权限 | Ginkgo failure、RBAC manifest | `go test ./internal/kubestatemetrics` |
| `IT-KSM-001` | #51 | envtest | API Server 1.28 | 调谐五类直接受管资源；API defaults、ownerReference、managedResources 与 Deployment availability 正确反映到 status/conditions | Ginkgo failure、API object | `make test-integration` |
| `IT-KSM-002` | #51 | envtest | 1.27 拒绝、1.28 缺依赖 | observedGeneration 与稳定 reason 正确，受管身份零写入 | Ginkgo failure、API object | `make test-integration` |
| `IT-KSM-003` | #51 | envtest | API Server 1.28 | 首次成功后修改 targetNamespace 返回 `InvalidSpec`，新 Namespace 零写入 | Ginkgo failure、API object | `make test-integration` |
| `IT-KSM-004` | #51 | envtest | API Server 1.28 | 同名非受管身份返回 `ResourceConflict`；冲突对象保持原样且其余计划资源零写入 | Ginkgo failure、API object | `make test-integration` |
| `IT-KSM-005` | #51 | envtest | API Server 1.28 | admission 拒绝缺少整个 `spec` 或缺少必填 `targetNamespace` 的声明 | Ginkgo failure、API error | `make test-integration` |
| `HELM-006` | #51、#52、#54 | Helm/生成物 | Chart | Helm CRD 与 Kubebuilder 生成源逐字同步，并包含 lifecycle、deployment、collectors、metricFilter、podResources 的 schema/defaults 且不暴露原始参数透传 | Ginkgo failure、rendered manifest | `make test-helm HELM=helm` |
| `HELM-007` | #51、#52 | Helm/RBAC | Chart | manager 具有 CR status/finalizer、默认 Deployment 与生命周期清理所需权限，且不含 Prometheus Operator API 权限 | Ginkgo failure、rendered manifest | `make test-helm HELM=helm` |
| `E2E-119-KSM-001` | #51 | kind E2E | Kubernetes 1.19.16 | 最小 CR 实际运行 kube-state-metrics 2.3.0；验证 Deployment/Service/RBAC/labels/owner/status | 完整集群对象、Events 与 Operator logs | `make e2e-default-deployment KUBERNETES_VERSION=1.19 ...` |
| `E2E-128-KSM-001` | #51 | kind E2E | Kubernetes 1.28.15 | 最小 CR 实际运行 kube-state-metrics 2.11.0；验证 Deployment/Service/RBAC/labels/owner/status | 完整集群对象、Events 与 Operator logs | `make e2e-default-deployment KUBERNETES_VERSION=1.28 ...` |
| `UT-KSM-007` | #52 | 纯逻辑 | 通用 | phase 按 Deleting、Disabling、Disabled、Degraded、Reconciling、Ready、Pending 的优先级从删除状态与 conditions 派生 | Ginkgo failure | `go test ./internal/kubestatemetrics` |
| `IT-KSM-006` | #52 | envtest | API Server 1.28 | 停用优先于无效普通声明，删除全部五类直接受管资源后进入 Disabled，并按最新 generation 恢复 | Ginkgo failure、API object | `make test-integration` |
| `IT-KSM-007` | #52 | envtest | API Server 1.28 | 挂起只删除 workload，保留 Service、ServiceAccount 与 RBAC，取消挂起后重建 workload | Ginkgo failure、API object | `make test-integration` |
| `IT-KSM-008` | #52 | envtest | API Server 1.28 | 删除优先于无效声明，finalizer 阻塞直到全部直接受管资源确认消失 | Ginkgo failure、API object | `make test-integration` |
| `IT-KSM-009` | #52 | envtest | API Server 1.28 | 停用删除发出后立刻重新启用时先等待旧资源消失，再按最新 generation 重建 | Ginkgo failure、API object | `make test-integration` |
| `E2E-119-KSM-002` | #52 | kind E2E | Kubernetes 1.19.16 | 真实集群覆盖停用、恢复、挂起、取消挂起和 finalizer 删除阻塞 | 完整集群对象、conditions、Events 与 Operator logs | `make e2e-default-deployment KUBERNETES_VERSION=1.19 ...` |
| `E2E-128-KSM-002` | #52 | kind E2E | Kubernetes 1.28.15 | 真实集群覆盖停用、恢复、挂起、取消挂起和 finalizer 删除阻塞 | 完整集群对象、conditions、Events 与 Operator logs | `make e2e-default-deployment KUBERNETES_VERSION=1.28 ...` |
| `UT-KSM-006` | #54 | 纯规划 | 1.19、1.28 | `Default` 固定解析为两个版本共同的 28 项 collector catalog，并显式生成 `--resources`；省略 `podResources` 时不注入资源默认值 | Ginkgo failure | `go test ./internal/kubestatemetrics` |
| `UT-KSM-008` | #54 | 纯规划 | 1.19、1.28 | `Include` 的显式 collector 同时成为实际参数与 `effectiveCollectors` | Ginkgo failure | `go test ./internal/kubestatemetrics` |
| `UT-KSM-009` | #54 | 纯规划 | 1.19、1.28 | 拒绝 Default 携带 names、Include 空集合、重复项与共同 catalog 外名称，返回 `InvalidSpec` 且零资源 | Ginkgo failure | `go test ./internal/kubestatemetrics` |
| `UT-KSM-010` | #54 | 纯规划 | 1.19、1.28 | Allow/Deny 分别映射 metric allowlist/denylist，pattern 不被隐式锚定 | Ginkgo failure | `go test ./internal/kubestatemetrics` |
| `UT-KSM-011` | #54 | 纯规划 | 1.19、1.28 | 拒绝 mode/list 冲突、空项、非法 Go regexp 与含逗号项；合法零命中 pattern 不要求预知指标 catalog | Ginkgo failure | `go test ./internal/kubestatemetrics` |
| `UT-KSM-012` | #54 | 纯规划 | 1.19、1.28 | 显式 `podResources` 完整下发 requests/limits，不伪造项目默认值 | Ginkgo failure | `go test ./internal/kubestatemetrics` |
| `UT-KSM-013` | #54 | 纯规划 | 1.19、1.28 | 非空 `podResources.claims` 因缺少 Pod `resourceClaims` 声明入口而在资源规划前返回 `InvalidSpec` | Ginkgo failure | `go test ./internal/kubestatemetrics` |
| `IT-KSM-010` | #54 | envtest | API Server 1.28；目标 profile 1.19、1.28 | API 配置贯通 Deployment args/resources 与 status effective collectors；已应用配置变为非法后 workload 不被重写 | Ginkgo failure、API object | `make test-integration` |
| `IT-KSM-011` | #54 | envtest | API Server 1.28 | 非法配置优先于缺失 Namespace 返回 InvalidSpec；零资源、无定时重试、相同 generation 不重复写 status，其他实例不受影响，新 generation 可恢复 | Ginkgo failure、API object | `make test-integration` |
| `IT-KSM-012` | #54 | envtest | API Server 1.28；目标 profile 1.19、1.28 | Deny filter 映射为两个版本 workload 的 metric denylist，effective collectors 与参数一致 | Ginkgo failure、API object | `make test-integration` |
| `E2E-119-KSM-003` / `E2E-128-KSM-003` | #54 | kind E2E | Kubernetes 1.19.16、1.28.15 | Include pods、未锚定 Allow pattern 和显式 podResources 作用于真实 workload 与 scrape | 完整集群对象、Events、metrics 与 Operator logs | `make e2e-default-deployment KUBERNETES_VERSION=<minor> ...` |
| `E2E-119-KSM-004` / `E2E-128-KSM-004` | #54 | kind E2E | Kubernetes 1.19.16、1.28.15 | Deny pattern 从真实 scrape 排除匹配指标，同时保留其他 collector 指标 | 完整集群对象、Events、metrics 与 Operator logs | `make e2e-default-deployment KUBERNETES_VERSION=<minor> ...` |
| `E2E-119-KSM-005` / `E2E-128-KSM-005` | #54 | kind E2E | Kubernetes 1.19.16、1.28.15 | 非法 regexp 仅降级当前实例、无 workload/status 重写，并保持 Operator 和合法实例可用 | 完整集群对象、Events 与 Operator logs | `make e2e-default-deployment KUBERNETES_VERSION=<minor> ...` |
| `UT-KSM-014` | #55 | 纯规划 | 1.19、1.28 | 字面名称与完整 RE2 匹配结果组成排序去重并集，substring 不命中，并统一生成 namespace 参数 | Ginkgo failure | `go test ./internal/kubestatemetrics` |
| `UT-KSM-015` | #55 | 纯规划 | 1.19、1.28 | 省略 namespaceFilter 时按 All 展开全部当前 Namespace | Ginkgo failure | `go test ./internal/kubestatemetrics` |
| `UT-KSM-016` | #55 | 纯规划 | 1.19、1.28 | 拒绝 All 携带过滤项、空 Include、未知 mode、非法字面名称与非法 RE2，返回 InvalidSpec 且零资源 | Ginkgo failure | `go test ./internal/kubestatemetrics` |
| `UT-KSM-017` | #55 | 纯规划 | 1.19、1.28 | 零匹配只规划非 workload 资源，同时保留待删除 workload 的稳定身份 | Ginkgo failure | `go test ./internal/kubestatemetrics` |
| `UT-KSM-018` | #55 | 纯规划 | 通用 | 非法 namespace regex 优先于缺失 targetNamespace 报告 InvalidSpec | Ginkgo failure | `go test ./internal/kubestatemetrics` |
| `UT-KSM-019` | #55 | 纯规划 | 1.19、1.28 | Namespace 集合增删使计划在零匹配、扩展、归零与恢复之间确定性切换 | Ginkgo failure | `go test ./internal/kubestatemetrics` |
| `IT-KSM-013` | #55 | envtest | API Server 1.28 | 从集群 Namespace 解析字面与完整 regex 并集，workload 参数与 effectiveNamespaceCount 一致 | Ginkgo failure、API object | `make test-integration` |
| `IT-KSM-014` | #55 | envtest | API Server 1.28 | 零匹配删除 workload、保留 Service/ServiceAccount/RBAC 并报告 NoNamespaceMatched，匹配恢复后重建 | Ginkgo failure、API object | `make test-integration` |
| `IT-KSM-015` | #55 | envtest | API Server 1.28 | 已应用配置变为非法 regex 时只更新 InvalidSpec status，不部分修改既有 workload | Ginkgo failure、API object | `make test-integration` |
| `IT-KSM-016` | #55 | envtest | API Server 1.28 | Namespace watch 自动触发范围扩展、rollout、归零和恢复 | Ginkgo failure、API object | `make test-integration` |
| `IT-KSM-017` | #55 | envtest | API Server 1.28 | workload 仍在终止时持续 requeue，不提前报告稳定零匹配 | Ginkgo failure、API object | `make test-integration` |
| `E2E-119-KSM-006` / `E2E-128-KSM-006` | #55 | kind E2E | Kubernetes 1.19.16、1.28.15 | 字面名称、完整 regex、substring 非命中、集合扩展、零匹配及自动恢复作用于真实可用 workload 与 KSM 指标 | 完整集群对象、conditions、Events、metrics 与 Operator logs | `make e2e-default-deployment KUBERNETES_VERSION=<minor> ...` |
| `E2E-119-PREFLIGHT-001` | #58 | kind E2E/预检 | Kubernetes 1.19.16 | 一期支持环境使用显式 runtime ServiceAccount 时预检成功，并报告 Kubernetes 版本事实与 `PreflightPassed` | NDJSON diagnostics、集群对象 | `make e2e-preflight KUBERNETES_VERSION=1.19 ...` |
| `E2E-128-PREFLIGHT-001` | #58 | kind E2E/预检 | Kubernetes 1.28.15 | 一期支持环境使用显式 runtime ServiceAccount 时预检成功，并报告 Kubernetes 版本事实与 `PreflightPassed` | NDJSON diagnostics、集群对象 | `make e2e-preflight KUBERNETES_VERSION=1.28 ...` |
| `E2E-119-PREFLIGHT-002` | #58 | kind E2E/预检 | Kubernetes 1.19.16 | runtime ServiceAccount 缺失时预检非零退出，并报告 `RuntimeServiceAccountNotFound` 与 `PreflightFailed` | NDJSON diagnostics、退出码 | `make e2e-preflight KUBERNETES_VERSION=1.19 ...` |
| `E2E-128-PREFLIGHT-002` | #58 | kind E2E/预检 | Kubernetes 1.28.15 | runtime ServiceAccount 缺失时预检非零退出，并报告 `RuntimeServiceAccountNotFound` 与 `PreflightFailed` | NDJSON diagnostics、退出码 | `make e2e-preflight KUBERNETES_VERSION=1.28 ...` |
