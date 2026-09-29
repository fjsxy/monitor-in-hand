# 一期验收矩阵

本文件记录已经自动化并作为 blocking gate 执行的稳定场景。当前仅列出已由 GitHub Issue #51 交付的默认 `KubeStateMetrics` Deployment 切片；后续 ticket 追加自身场景，不改写已有 ID。

| 场景 ID | 来源 | 层级 | 目标版本 | 动作与精确断言 | 失败证据 | 测试入口 |
| --- | --- | --- | --- | --- | --- | --- |
| `UT-KSM-001` | #51 | 纯规划 | 1.19、1.28 | 最小 CR 选择固定 profile，并规划单副本 Deployment、headless Service、独立 SA/ClusterRole/ClusterRoleBinding、稳定 labels 与当前 UID ownerReference | Ginkgo failure | `go test ./internal/kubestatemetrics` |
| `UT-KSM-002` | #51 | 纯规划 | 其他 minor | 返回 `UnsupportedKubernetesVersion`，资源计划为空 | Ginkgo failure | `go test ./internal/kubestatemetrics` |
| `UT-KSM-003` | #51 | 纯规划 | 1.28 | 目标 Namespace 不存在时返回 `DependencyMissing`，资源计划为空 | Ginkgo failure | `go test ./internal/kubestatemetrics` |
| `UT-KSM-004` | #51 | 纯规划 | 1.28 | 首次应用后 targetNamespace 变化返回 `InvalidSpec`，资源计划为空 | Ginkgo failure | `go test ./internal/kubestatemetrics` |
| `IT-KSM-001` | #51 | envtest | API Server 1.28 | 调谐五类直接受管资源；API defaults、ownerReference、managedResources 与 Deployment availability 正确反映到 status/conditions | Ginkgo failure、API object | `make test-integration` |
| `IT-KSM-002` | #51 | envtest | 1.27 拒绝、1.28 缺依赖 | observedGeneration 与稳定 reason 正确，受管身份零写入 | Ginkgo failure、API object | `make test-integration` |
| `IT-KSM-003` | #51 | envtest | API Server 1.28 | 首次成功后修改 targetNamespace 返回 `InvalidSpec`，新 Namespace 零写入 | Ginkgo failure、API object | `make test-integration` |
| `IT-KSM-004` | #51 | envtest | API Server 1.28 | 同名非受管身份返回 `ResourceConflict`；冲突对象保持原样且其余计划资源零写入 | Ginkgo failure、API object | `make test-integration` |
| `IT-KSM-005` | #51 | envtest | API Server 1.28 | admission 拒绝缺少整个 `spec` 或缺少必填 `targetNamespace` 的声明 | Ginkgo failure、API error | `make test-integration` |
| `HELM-006` | #51 | Helm/生成物 | Chart | Helm CRD 与 Kubebuilder 生成源逐字同步，并包含 cluster scope、v1alpha1 与三个 defaults | Ginkgo failure、rendered manifest | `make test-helm HELM=helm` |
| `HELM-007` | #51 | Helm/RBAC | Chart | manager 具有 CR status 与默认 Deployment 调谐权限，且不含 Prometheus Operator API 权限 | Ginkgo failure、rendered manifest | `make test-helm HELM=helm` |
| `E2E-119-KSM-001` | #51 | kind E2E | Kubernetes 1.19.16 | 最小 CR 实际运行 kube-state-metrics 2.3.0；验证 Deployment/Service/RBAC/labels/owner/status | 完整集群对象、Events 与 Operator logs | `make e2e-default-deployment KUBERNETES_VERSION=1.19 ...` |
| `E2E-128-KSM-001` | #51 | kind E2E | Kubernetes 1.28.15 | 最小 CR 实际运行 kube-state-metrics 2.11.0；验证 Deployment/Service/RBAC/labels/owner/status | 完整集群对象、Events 与 Operator logs | `make e2e-default-deployment KUBERNETES_VERSION=1.28 ...` |
