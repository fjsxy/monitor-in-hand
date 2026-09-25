# PrometheusAgent 多实例资源身份与冲突边界调研

## 范围与版本

本文只核实以下固定组合的上游事实，不给项目 API 做最终设计：

- Prometheus Operator `v0.83.0`，tag 对应 commit `5cf2f5d37906d64b2259a262e319df4c74639e31`；
- `monitoring.coreos.com/v1alpha1` `PrometheusAgent`；
- Kubernetes `v1.19.16` 与 `v1.28.15`；
- StatefulSet 模式。`DaemonSet` 模式不产生 StatefulSet governing Service 或 PVC，本文仅在资源冲突行为相同时顺带提及。

所有结论来自固定 tag 的源码、固定 Kubernetes tag 的 StatefulSet controller 源码或 Kubernetes 官方文档。

## 结论摘要

1. `PrometheusAgent` 的资源前缀在 `v0.83.0` 中明确是 **`prom-agent-`**，不是 `prometheus-agent-`。`prometheus-agent-operated` 只是默认 governing Service；`app.kubernetes.io/name=prometheus-agent` 只是 selector label。[命名函数源码](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/prometheus/common.go#L148-L176)
2. 一个名为 `<agent>` 的 Agent 在 namespace `<ns>` 中创建：每 shard 一个 StatefulSet、每 StatefulSet `replicas` 个 Pod、一个实例级配置 Secret、至少一个 TLS assets Secret、一个 web config Secret。未指定 `serviceName` 时，同一 namespace 内所有 Agent 共享一个 `prometheus-agent-operated` headless Service。
3. StatefulSet、配置 Secret、TLS assets Secret、web config Secret 以 Agent 为 managing owner：`controller=true`、`blockOwnerDeletion=true`；默认 Service 对每个 Agent 仅添加普通 ownerReference，两个布尔值均未设置。Pod 的 managing owner 是 StatefulSet；默认 Retain 语义下 PVC 没有 Agent/StatefulSet/Pod ownerReference。
4. 同 namespace 同名 Agent 不可能并存，因为 Kubernetes 对象身份就是 `namespace/name`。不同 namespace 同名 Agent 的 namespaced 资源完全隔离；同 namespace 不同名 Agent 除默认 Service 外，自动生成名称均不冲突。
5. 默认 PVC template 名是 `prom-agent-<agent>-db`；PVC 名是 `<claim-template>-<statefulset>-<ordinal>`。默认 Retain 下，缩 replica、缩 shard、删 Agent 后 PVC 都保留；在原 namespace 以原名重建 Agent 会命中并复用同名 PVC。
6. **StatefulSet controller 只按 PVC 名查找既有 claim，不校验 labels、StorageClass、accessModes、requests 或 data source。** 1.19 与 1.28 源码都保留了 “TODO: Check resource requirements and accessmodes”。因此“同名重建复用”也是“同名错误 PVC 被直接挂载”的风险。
7. Operator 对同名资源不存在统一的 ownership guard：
   - StatefulSet/DaemonSet：更新为期望对象；遇到不可变字段导致 HTTP 422 时，前台删除再重建；
   - 配置/TLS/web Secret：覆盖 data、期望 labels/annotations 和 ownerReference；immutable Secret 会令 reconcile 失败；
   - 默认 Service：更新 mutable spec，但保留已有 cluster IP family 字段，并合并而非替换 ownerReferences；
   - 自定义 `serviceName`：只检查对象存在且 selector 能选中 Agent Pod，不修改、不拥有；
   - PVC：StatefulSet controller 按同名直接复用，不接管 spec；
   - TLS assets 的多 shard 清理甚至会从期望末尾开始，连续删除同前缀、递增序号的 Secret，直到第一次 NotFound，且不校验 ownerReference。
8. Agent runtime ServiceAccount、ClusterRole、ClusterRoleBinding 均不是 Operator 生成资源。Operator 只把 `spec.serviceAccountName` 原样放入 PodSpec；这些 RBAC 对象由集群管理员创建和维护。
9. 默认 external labels 为 `prometheus=<namespace>/<agent-name>` 与 `prometheus_replica=<pod-name>`。Pod 名含 shard 与 replica ordinal，因此同一集群内能区分各 shard/replica；它们没有 cluster identity，所以不同集群采用相同 namespace/name/topology 时仍会碰撞。
10. VictoriaMetrics `-dedup.minScrapeInterval` 只在**完整 label set 相同**的同一 time series 内去重，不识别、忽略或特殊处理 `prometheus_replica`。保留默认 `prometheus_replica=<pod-name>` 会把 HA replicas 写成不同 series，无法靠该参数合并。

## 资源名称与数量

设：

- Agent 名为 `A`；
- shard 编号为 `S`，从 `0` 开始；
- replica ordinal 为 `R`，从 `0` 开始。

Operator 将 `PrometheusAgent` 的 prefix 固定为 `prom-agent`，shard 0 不带 shard 后缀，其余带 `-shard-S`。[源码](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/prometheus/common.go#L70-L114) [prefix 源码](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/prometheus/common.go#L148-L176)

| 资源 | 精确名称/规则 | 数量 |
| --- | --- | --- |
| shard 0 StatefulSet | `prom-agent-A` | 1 |
| shard S>0 StatefulSet | `prom-agent-A-shard-S` | `shards-1` |
| shard 0 Pod | `prom-agent-A-R` | `replicas` |
| shard S>0 Pod | `prom-agent-A-shard-S-R` | 每 shard `replicas` |
| 生成配置 Secret | `prom-agent-A` | 1 |
| TLS assets Secret | `prom-agent-A-tls-assets-N`，`N` 从 0 开始 | 至少 1，可因数据量分片 |
| web config Secret | `prom-agent-A-web-config` | 1 |
| 默认 governing Service | `prometheus-agent-operated` | 每 namespace 1 个，所有未指定 `serviceName` 的 Agent 共享 |
| 默认 PVC template | `prom-agent-A-db` | 每 StatefulSet 1 个 template |
| shard 0 PVC | `prom-agent-A-db-prom-agent-A-R` | `replicas` |
| shard S>0 PVC | `prom-agent-A-db-prom-agent-A-shard-S-R` | 每 shard `replicas` |

Secret 和 volume 名称函数见 [`common.go`](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/prometheus/common.go#L125-L176)。TLS assets 以 `-N` 分片，即使 data 为空也会创建第 0 片，因为 `shard()` 无条件 append 当前 shard。[源码](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/operator/sharded_secret.go#L44-L90) web config Secret 由 web config reconciler 写入固定名称。[源码](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/prometheus/agent/operator.go#L984-L1011)

如用户在 `volumeClaimTemplate.metadata.name` 指定 `C`，上述 PVC 的 claim-template 前缀改成 `C`，但 StatefulSet 名仍保留，因此不同 Agent 即便复用同一个 `C` 也不会自动冲突。未指定时，Operator 将其设为 `prom-agent-A-db`；`accessModes` 未指定时设为 `ReadWriteOnce`。[源码](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/prometheus/agent/statefulset.go#L86-L130)

## labels 与 ownerReferences

### OwnerReference 构造规则

Prometheus Operator 的 `WithManagingOwner` 明确写入 Agent 的 `apiVersion/kind/name/uid`，并设置 `controller=true`、`blockOwnerDeletion=true`；`WithOwner` 只写四元身份，不设置这两个布尔值。[源码](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/operator/factory.go#L37-L71)

| 资源 | 直接 owner | `controller` | `blockOwnerDeletion` | 删除 Agent 后 |
| --- | --- | --- | --- | --- |
| StatefulSet | PrometheusAgent | `true` | `true` | Kubernetes GC 删除 |
| 配置 Secret | PrometheusAgent | `true` | `true` | Kubernetes GC 删除 |
| TLS assets Secret shards | PrometheusAgent | `true` | `true` | Kubernetes GC 删除 |
| web config Secret | PrometheusAgent | `true` | `true` | Kubernetes GC 删除 |
| 默认 governing Service | 一个或多个 PrometheusAgent | 未设置 | 未设置 | 还有有效 owner 时保留；所有 owners 均不存在后可被 GC |
| 自定义 governing Service | Operator 不添加 | — | — | 不受 Agent 删除影响 |
| Pod | StatefulSet | `true` | `true` | 随 StatefulSet 管理/级联删除 |
| PVC，默认 Retain | 无 | — | — | 保留 |

StatefulSet/Secrets 的 managing owner 分别在 StatefulSet 构造、配置 Secret、TLS template 与 web Secret 构造时写入。[StatefulSet](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/prometheus/agent/statefulset.go#L63-L80) [配置 Secret](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/prometheus/common.go#L125-L145) [TLS template](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/prometheus/operator.go#L68-L82) [web Secret](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/prometheus/agent/operator.go#L984-L1011)

Kubernetes 1.19 与 1.28 的 StatefulSet controller 都通过 `metav1.NewControllerRef(set, ...)` 构造 Pod；该 helper 将 `controller`、`blockOwnerDeletion` 都设为 true。[1.19 Pod 构造](https://github.com/kubernetes/kubernetes/blob/v1.19.16/pkg/controller/statefulset/stateful_set_utils.go#L249-L255) [1.19 NewControllerRef](https://github.com/kubernetes/kubernetes/blob/v1.19.16/staging/src/k8s.io/apimachinery/pkg/apis/meta/v1/controller_ref.go#L53-L64) [1.28 Pod 构造](https://github.com/kubernetes/kubernetes/blob/v1.28.15/pkg/controller/statefulset/stateful_set_utils.go#L522-L528)

Kubernetes 官方 GC 文档说明 namespaced owner 必须与 dependent 同 namespace，并且 dependent 在所有 owner 均被确认不存在后才进入删除条件。[官方文档](https://kubernetes.io/docs/concepts/architecture/garbage-collection/#owners-and-dependents) 因此默认 Service 的多 ownerReferences 是共享生命周期，而不是“删除任意一个 Agent 就删除 Service”。其 ownerRef 不含 `blockOwnerDeletion=true`，所以 Service 不阻塞 Agent 本身完成删除。

### 稳定 labels

`operator.UpdateObject` 总会先加 legacy label `managed-by=prometheus-operator`。[源码](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/operator/factory.go#L25-L28) [写入点](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/operator/factory.go#L144-L150)

StatefulSet 对象额外带：

- `operator.prometheus.io/name=A`
- `operator.prometheus.io/mode=agent`
- `operator.prometheus.io/shard=S`
- Agent 本身 labels、Operator 全局 labels，以及 input hash annotation。

来源见 [StatefulSet 构造](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/prometheus/agent/statefulset.go#L63-L80)。

Pod selector/template 的基础 labels 为：

- `app.kubernetes.io/name=prometheus-agent`
- `app.kubernetes.io/managed-by=prometheus-operator`
- `app.kubernetes.io/instance=A`
- `operator.prometheus.io/name=A`
- `operator.prometheus.io/shard=S`

前三项及 instance/name 的构造见 [`makeSelectorLabels`](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/prometheus/agent/operator.go#L1176-L1183)，shard label 在 StatefulSet spec 构造时追加。[源码](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/prometheus/agent/statefulset.go#L192-L205)

PVC 从 volumeClaimTemplate 复制 metadata，然后 StatefulSet controller 将 StatefulSet selector 的 `matchLabels` 合入；selector labels 对同名 template label 生效。[Kubernetes 1.19](https://github.com/kubernetes/kubernetes/blob/v1.19.16/pkg/controller/statefulset/stateful_set_utils.go#L135-L153) [Kubernetes 1.28](https://github.com/kubernetes/kubernetes/blob/v1.28.15/pkg/controller/statefulset/stateful_set_utils.go#L390-L408)

配置 Secret、TLS assets Secret、web Secret 没有 instance/name 专属 label；基线只有 `managed-by=prometheus-operator` 和可选 Operator 全局 labels。它们的可靠实例归属是名称和 ownerReference，不是 `app.kubernetes.io/instance`。

默认 Service 的 selector 只有 `app.kubernetes.io/name=prometheus-agent`，所以会选中 namespace 内所有 Agent Pod。Service labels 为 `managed-by=prometheus-operator`、`operated-prometheus=true` 与可选全局 labels。[Service 构造](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/prometheus/common.go#L464-L495) [调用点](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/prometheus/agent/operator.go#L691-L712)

> 注意：Operator 的全局 `--labels` 能覆盖同名生成 label，因为 `Map.Merge` 规定 receiver 优先，代码以全局 labels 为 receiver 合并 selector labels。部署方不应在全局 labels 中覆写上述身份键。[Merge 源码](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/operator/config.go#L184-L215) [调用点](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/prometheus/agent/statefulset.go#L192-L205)

## 默认与自定义 governing Service

### 默认 Service

未设置 `spec.serviceName` 时，每个 Agent reconcile 同一个 `prometheus-agent-operated` headless Service：`clusterIP: None`、port 9090，selector 为 `app.kubernetes.io/name=prometheus-agent`。[Service spec](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/prometheus/common.go#L464-L495) [Agent 调用](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/prometheus/agent/operator.go#L691-L712)

`CreateOrUpdateService` 在每次更新时：

1. 保留现有 Service 的 cluster IP family 等不可变字段；
2. **合并**现有与本次 Agent ownerReferences；
3. 合并 metadata；
4. 以期望 Service spec 更新对象。

源码见 [`CreateOrUpdateService`](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/k8sutil/k8sutil.go#L235-L264) 与 [`mergeOwnerReferences`](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/k8sutil/k8sutil.go#L496-L507)。因此同 namespace 多 Agent 会逐步成为该 Service 的普通 owners；并发 reconcile 通过 `RetryOnConflict` 重读，避免简单覆盖掉另一 Agent 的 ownerRef。

### 自定义 Service

设置 `spec.serviceName` 时，Service 必须由外部先在同 namespace 创建。Operator 只读取它，并验证该 Service selector 能匹配 Agent 的四个基础 selector labels；它不要求 selector 完全相等，因此较宽 selector 也能通过。验证成功后，Operator 不更新 Service，也不添加 ownerReference。[源码](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/k8sutil/k8sutil.go#L580-L600)

API 注释明确建议同 namespace 多实例为每个实例指定不同 `serviceName`。[源码](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/apis/monitoring/v1/prometheus_types.go#L899-L907) 这是隔离 Service endpoints 和 stable network domain 的建议，不是 admission 强制规则。

## namespace/name 隔离边界

- Kubernetes namespaced 资源不能存在两个相同 `namespace/name` 的 PrometheusAgent，因此“同 namespace 同名两套 Agent”不是合法状态；第二次 create 得到 AlreadyExists，update 则是在修改原对象。
- 同 namespace 不同名：StatefulSet、Pod、Secrets、默认 PVC 都含 Agent 名，因此互不重名；默认 governing Service 是唯一共享自动资源。
- 不同 namespace 同名：StatefulSet、Pod、Secret、Service、PVC 全是 namespaced，因此 API 身份隔离；ownerReference 也不能跨 namespace。[Kubernetes 官方文档](https://kubernetes.io/docs/concepts/architecture/garbage-collection/#owners-and-dependents)
- ClusterRole 与 ClusterRoleBinding 是 cluster-scoped，不从 Agent namespace 获得名称隔离；如果管理员为每实例创建 binding，其 metadata.name 必须全局唯一，subject 则必须写准 namespace。

## PVC 生命周期与“同名复用”

### 名称生成

Kubernetes 1.19 和 1.28 使用相同公式：

```text
<volumeClaimTemplate.name>-<statefulSet.name>-<ordinal>
```

见 [1.19 源码](https://github.com/kubernetes/kubernetes/blob/v1.19.16/pkg/controller/statefulset/stateful_set_utils.go#L91-L96) 与 [1.28 源码](https://github.com/kubernetes/kubernetes/blob/v1.28.15/pkg/controller/statefulset/stateful_set_utils.go#L118-L123)。

### 默认删除策略

- Kubernetes 1.19 的这条 StatefulSet controller 路径没有 PVC retention policy ownerRef 管理；由 volumeClaimTemplate 创建的 PVC 没有 StatefulSet/Pod ownerReference，因此 StatefulSet 删除或缩容不会自动删除 PVC。
- Kubernetes 1.28 将未指定 policy 解释为 `WhenDeleted=Retain`、`WhenScaled=Retain`；该组合要求 PVC 不带当前 StatefulSet/Pod ownerReference。[默认 policy 源码](https://github.com/kubernetes/kubernetes/blob/v1.28.15/pkg/controller/statefulset/stateful_set_utils.go#L162-L171) [ownerRef 规则](https://github.com/kubernetes/kubernetes/blob/v1.28.15/pkg/controller/statefulset/stateful_set_utils.go#L249-L351)
- Prometheus Operator 只有在 CR 显式给出 `persistentVolumeClaimRetentionPolicy` 时才把它复制到 StatefulSet。[源码](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/prometheus/agent/statefulset.go#L127-L133) 对本项目既定默认 Retain 契约而言，两版结果一致：缩 replica、删 shard StatefulSet、删 Agent 后 PVC 保留。

缩 shard 时，Operator 会对不再期望且带当前 Agent name/mode labels 的 StatefulSet 执行 foreground delete；它没有直接删除 PVC。[源码](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/prometheus/agent/operator.go#L808-L829)

### 旧 PVC 的复用检查

两个版本的 `createPersistentVolumeClaims` 行为一致：

- 按期望精确名称 GET PVC；
- NotFound 才创建；
- 已存在只检查 `deletionTimestamp`；
- 明确留下 `TODO: Check resource requirements and accessmodes, update if necessary`。

证据：[Kubernetes 1.19](https://github.com/kubernetes/kubernetes/blob/v1.19.16/pkg/controller/statefulset/stateful_pod_control.go#L177-L207) [Kubernetes 1.28](https://github.com/kubernetes/kubernetes/blob/v1.28.15/pkg/controller/statefulset/stateful_pod_control.go#L337-L369)

因此同 namespace、同 Agent 名、同 shard、同 claim template 名、同 ordinal 的 Agent 重建会复用旧 PVC，且：

- 不校验旧 PVC 是否由旧 Agent/StatefulSet 创建；
- 不校验 labels；
- 不校验 StorageClass、accessModes、requested storage、selector 或 data source；
- 不用新 template 覆盖旧 PVC spec；
- 旧 PVC 正在删除时阻塞 Pod 创建；
- 在 1.28 非默认 Delete retention policy 下会额外校验/调整 ownerReferences；默认 Retain/Retain 下 `PodClaimIsStale` 直接允许复用。[源码](https://github.com/kubernetes/kubernetes/blob/v1.28.15/pkg/controller/statefulset/stateful_pod_control.go#L266-L302)

所以“原名重建可恢复 WAL”是精确名称带来的结果，不是 owner 或 schema 兼容性保证。若重建时改变 storage template，旧 PVC 仍胜出；是否能挂载以及应用数据是否可用，交给 storage backend/kubelet/Prometheus 运行时决定。

## 预先存在同名资源时的精确行为

| 预存对象 | v0.83.0 行为 | 是否验证旧 owner | 主要风险 |
| --- | --- | --- | --- |
| StatefulSet `prom-agent-A[-shard-S]` | informer 能看到所有 StatefulSet；Operator 生成期望对象并 update。HTTP 422 immutable 失败时 foreground delete，随后 reconcile 重建 | 否 | 接管或删除无关 workload |
| DaemonSet `prom-agent-A` | 同 StatefulSet；422 时 foreground delete/recreate | 否 | 接管或删除无关 workload |
| 配置 Secret `prom-agent-A` | 保留未知 metadata，但 data、期望 labels/annotations、ownerRefs 按期望对象 update | 否 | 覆盖用户 Secret；immutable 时持续失败 |
| TLS Secret `prom-agent-A-tls-assets-N` | 当前需要的 shard 被 update；多余连续 shard 被 delete | 否 | 覆盖或删除无关 Secret |
| web Secret `prom-agent-A-web-config` | 同配置 Secret | 否 | 覆盖用户 Secret；immutable 时持续失败 |
| 默认 Service `prometheus-agent-operated` | 原地更新成期望 spec，保留 clusterIP family 字段，合并旧 ownerRefs 和 Agent ownerRef | 否 | 改写已有 Service selector/ports/type；API 不允许时 reconcile 失败 |
| 自定义 `serviceName` Service | 只读；不存在或 selector 不匹配则 reconcile 失败 | 不接管 | 管理员负责其完整生命周期 |
| PVC 精确期望名 | 直接复用；不 update spec | 否 | 挂载无关数据，或模板与实际容量/StorageClass 不一致 |

StatefulSet informer 创建时没有 label selector，因此预存同名对象不是“看不见后 create 得到 AlreadyExists”，而是会进入 update 路径。[源码](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/prometheus/agent/operator.go#L296-L308) Operator 的 update helper 只合并 metadata，不保留旧 ownerReferences；期望 StatefulSet 自带当前 Agent managing owner，因此 update 会替换旧 owners。[update helper](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/k8sutil/k8sutil.go#L314-L330) [metadata merge](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/k8sutil/k8sutil.go#L509-L518) 422 删除重建路径见 [Agent controller](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/prometheus/agent/operator.go#L764-L805)。

Secret helper 同样只合并 metadata，不合并 ownerReferences，然后以整个 desired Secret 做 update；因此会替换 data 与 ownerRefs。[源码](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/k8sutil/k8sutil.go#L350-L372)

TLS assets 的清理从最后一个期望 shard 的下一个序号开始逐个 delete，直到第一次 NotFound，未读取或校验 ownerReference。[源码](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/operator/sharded_secret.go#L93-L115) 例如当前只需 `...-0`，预存无关 `...-1`、`...-2` 且二者连续存在，会被依次删除；若 `...-1` 不存在，则不会继续尝试 `...-2`。

## runtime ServiceAccount 与 RBAC 的所有权

Agent controller 只把 `spec.serviceAccountName` 复制到 PodSpec，并默认 `automountServiceAccountToken=true`；没有创建 ServiceAccount、Role、ClusterRole、RoleBinding 或 ClusterRoleBinding 的 reconcile 路径。[PodSpec 构造](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/prometheus/agent/statefulset.go#L274-L291)

官方 Agent 指南也把顺序写成：先由部署者创建 ServiceAccount、ClusterRole、ClusterRoleBinding，再创建引用该 ServiceAccount 的 PrometheusAgent。[官方源码文档](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/Documentation/platform/prometheus-agent.md#L132-L196)

因此：

- 这些 RBAC 对象是管理员对象，不属于 Agent owner graph，Agent 删除不会清理它们；
- 多 Agent 可共用一个 ServiceAccount，也可每实例独立；这是管理员的权限隔离决策，不是 Operator 自动行为；
- 同名 ServiceAccount 位于不同 namespace 时是不同身份；ClusterRoleBinding 的 subject 必须同时匹配 name 与 namespace；
- ClusterRole 可以共享；ClusterRoleBinding 为 cluster-scoped，名称必须全局唯一。

## external labels 与多实例身份风险

Operator 默认生成：

```yaml
external_labels:
  prometheus: <namespace>/<agent-name>
  prometheus_replica: <pod-name>
```

默认键和值的构造见 [`buildExternalLabels`](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/prometheus/promcfg.go#L701-L734)。`POD_NAME` 由 Downward API 的 `metadata.name` 注入 config-reloader，再用于配置变量替换。[源码](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/operator/config_reloader.go#L32-L42) [注入点](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/operator/config_reloader.go#L201-L210)

含义与风险：

- 同 namespace 不同 Agent：`prometheus` 值不同；
- 不同 namespace 同名 Agent：`prometheus` 值不同；
- 同一 Agent 的不同 shard/replica：Pod 名分别为 `prom-agent-A[-shard-S]-R`，所以 `prometheus_replica` 不同；没有单独默认 `shard` external label；
- 不同 cluster 中相同 namespace/name/shard/replica：两个默认 external labels 完全相同，无法提供 cluster identity；
- `externalLabels` map 不能覆盖这两个保留键；专用的 `prometheusExternalLabelName` / `replicaExternalLabelName` 可改名，设为空字符串会禁用。[API 字段](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/apis/monitoring/v1/prometheus_types.go#L273-L321)
- 默认 labels 只表达来源身份，不是 HA 去重协议，也不能阻止两个 Agent 重叠采集同一 target。接收端如何去重或保留重复序列仍是 backend 契约。

## VictoriaMetrics Cluster 的 HA 去重语义

> 本项目尚未固定 VictoriaMetrics 版本。以下事实核对自 VictoriaMetrics 官方仓库 commit [`018bc7808882fc8c10b150486de90b22e75ac504`](https://github.com/VictoriaMetrics/VictoriaMetrics/tree/018bc7808882fc8c10b150486de90b22e75ac504)（2026-09-24）；接入时仍应对实际部署版本复核 flags。

### `prometheus_replica` 会形成不同 series

VictoriaMetrics 官方文档明确要求：只有 raw samples 的 labels **完全相同**才会 deduplicate；HA Prometheus/vmagent 的 `external_labels` 也必须完全相同，才能写入同一 time series。[官方源码文档](https://github.com/VictoriaMetrics/VictoriaMetrics/blob/018bc7808882fc8c10b150486de90b22e75ac504/docs/victoriametrics/README.md#L1379-L1406)

Prometheus Operator 默认给每个 Pod 生成不同的 `prometheus_replica=<pod-name>`。因此对于同一个被 replicas 重复抓取的原始 target series：

```text
metric{..., prometheus="ns/A", prometheus_replica="prom-agent-A-0"}
metric{..., prometheus="ns/A", prometheus_replica="prom-agent-A-1"}
```

在 VictoriaMetrics 中是两个 label set 不同的 time series。`-dedup.minScrapeInterval` 在每条 series 内按时间窗口选一个 sample，**不会跨这两条 series 合并**。该 flag 的实现只接收一条 series 的 timestamp/value 数组，没有任何 replica-label 参数或专用 label 名。[dedup 实现](https://github.com/VictoriaMetrics/VictoriaMetrics/blob/018bc7808882fc8c10b150486de90b22e75ac504/lib/storage/dedup.go#L9-L90)

换言之，VictoriaMetrics 不像某些 query layer 那样配置一个 “replica label” 并在查询时忽略它；它没有内置识别 `prometheus_replica` 这个名字。要让 HA replicas 合并，进入 storage 前的最终 label set 就必须相同。

### PrometheusAgent 应如何配置

对于同一 `PrometheusAgent` CR 内的 `replicas`，若它们被定义为 HA duplicates 并希望 VictoriaMetrics 合并，应：

```yaml
spec:
  replicaExternalLabelName: ""
```

Prometheus Operator API 明确规定空字符串会禁止添加 replica external label。[上游 API](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/apis/monitoring/v1/prometheus_types.go#L273-L286) 应保留默认 `prometheus=<namespace>/<agent-name>`，因为同一 CR 的所有 replicas/shards 该值一致，同时仍可区分不同 Agent；其他 `externalLabels` 也必须在所有 HA replicas 间相同。

如果需要多集群来源身份，应增加一个对同一集群内 HA replicas 相同、不同集群之间不同的 cluster label。它不会破坏集群内 HA dedup，但能防止不同集群恰好使用相同 namespace/Agent 名时误合并。

另一条技术路径是在写入前用 relabeling 删除 `prometheus_replica`；VictoriaMetrics 支持 `labeldrop`。[官方 relabeling 文档](https://github.com/VictoriaMetrics/VictoriaMetrics/blob/018bc7808882fc8c10b150486de90b22e75ac504/docs/victoriametrics/relabeling.md#L870-L900) 但对本项目而言，直接设置 `replicaExternalLabelName: ""` 更小、更明确，也不需要引入此前未纳入一期保证的 `writeRelabelConfigs` 或 backend ingestion relabel 配置。

### backend 必须如何配置

VictoriaMetrics Cluster 官方说明：

- 只有底层 `vmstorage` replication 去重时，`vmselect -dedup.minScrapeInterval=1ms` 用于合并复制到不同 vmstorage 的同 timestamp 副本；
- 如果重复数据来自 identically configured Prometheus/vmagent HA instances，`-dedup.minScrapeInterval` 应等于 scrape config 的 `scrape_interval`；
- 为保证 storage background merge 尚未完成时的查询结果一致，应在 `vmstorage` 与 `vmselect` 设置**相同**的 `-dedup.minScrapeInterval`。[Cluster replication 说明](https://github.com/VictoriaMetrics/VictoriaMetrics/blob/018bc7808882fc8c10b150486de90b22e75ac504/docs/victoriametrics/Cluster-VictoriaMetrics.md#L342-L365) [Cluster dedup 说明](https://github.com/VictoriaMetrics/VictoriaMetrics/blob/018bc7808882fc8c10b150486de90b22e75ac504/docs/victoriametrics/Cluster-VictoriaMetrics.md#L914-L926)

所以若 Agent 的统一 `scrape_interval=30s`，HA 写入的直接配置约束是：

```text
vmstorage: -dedup.minScrapeInterval=30s
vmselect:  -dedup.minScrapeInterval=30s
vminsert:  无对应 storage dedup flag
```

官方同时建议所有 scrape targets 使用单一 `scrape_interval`。[dedup 文档](https://github.com/VictoriaMetrics/VictoriaMetrics/blob/018bc7808882fc8c10b150486de90b22e75ac504/docs/victoriametrics/README.md#L1396-L1406) 若一期允许不同 target 使用不同 scrape intervals，就不能声称一个 backend interval 对所有 target 都实现项目定义的精确 HA dedup；需要额外限定统一 interval 或另做验收。

`-dedup.minScrapeInterval` 为 0 时 dedup 禁用，正值时才启用；vmstorage flag 会调用 storage 的全局 dedup interval。[flag 源码](https://github.com/VictoriaMetrics/VictoriaMetrics/blob/018bc7808882fc8c10b150486de90b22e75ac504/app/vmstorage/main.go#L53-L61) [初始化源码](https://github.com/VictoriaMetrics/VictoriaMetrics/blob/018bc7808882fc8c10b150486de90b22e75ac504/app/vmstorage/main.go#L121-L134)

## 对后续决策的直接约束

这些是由上游行为推出、需要规划 ticket 明确选择的边界，不是本文替项目代做决定：

1. 是否要求同 namespace 每个 Agent 显式使用独立 `serviceName`，避免默认 Service endpoint 聚合；若要求，Service 必须由管理员先创建。
2. 是否把全部自动名称和 expected PVC 名列为管理员不得预占的 reserved name；上游没有冲突保护，单靠 ownerReference 不能防止接管。
3. 是否固定默认 Retain 且禁止/不承诺 `persistentVolumeClaimRetentionPolicy`，以保持 1.19/1.28 一致行为。
4. 是否要求重建前校验旧 PVC 的 StorageClass、accessModes、capacity、labels 与业务归属；StatefulSet controller 不替项目完成这项校验。
5. 是否将 Agent `replicas` 定义为 VictoriaMetrics HA duplicates；若是，必须禁用 `prometheus_replica`，并要求 VM `vmstorage`/`vmselect` 以统一 scrape interval 开启 dedup。
6. 多集群写向同一 backend 时，是否强制增加项目定义的 cluster external label；默认 external labels 不含 cluster identity。
7. 是否强制所有 scrape targets 使用同一 `scrape_interval`；否则单一 VM dedup interval 无法覆盖所有目标的精确 HA 语义。
8. runtime RBAC 是按实例独立还是允许共享；无论哪种，都是 Helm/管理员资源，不是 Prometheus Operator 生成资源。
