# 原生 PrometheusAgent 一期示例

此目录展示一期支持的采集配置边界。先为 `monitoring` 和采集配置所在
namespace 添加 `monitoring.fjsxy.github.io/collection-scope=example` label，再按顺序应用：

```bash
kubectl apply -f runtime-rbac.yaml
kubectl apply -f collection-configs.yaml
hack/preflight.sh \
  --agent-namespace monitoring \
  --agent-service-account prometheus-agent-example \
  --config-namespaces monitoring \
  --discovery-namespaces applications \
  --agent-manifest agent.yaml \
  --collection-config-manifest collection-configs.yaml
kubectl apply -f agent.yaml
```

上述 `agent.yaml` 是 EmptyDir 路径：它故意省略 `spec.storage`，因此每个
Agent Pod 的 WAL 使用 Pod 级 EmptyDir。Pod 被替换后会获得新卷，旧卷中尚未
发送的 WAL 可能丢失；实际是否丢样本取决于故障前的 remote_write 进度，
不应对单次 Pod 替换作“必然丢失”的时序敏感断言。

需要跨 Pod 故障保留 WAL 时，把 `agent-pvc.yaml` 中的
`replace-with-storage-class` 替换为集群的 StorageClass 后，改用：

```bash
hack/preflight.sh \
  --agent-namespace monitoring \
  --agent-service-account prometheus-agent-example \
  --config-namespaces monitoring \
  --discovery-namespaces applications \
  --agent-manifest agent-pvc.yaml \
  --collection-config-manifest collection-configs.yaml
kubectl apply -f agent-pvc.yaml
```

PVC 示例固定 `ReadWriteOnce`、`Filesystem` 与 `requests.storage: 10Gi`。
`storageClassName` 是可选字段：删除该字段表示使用创建 PVC 当时的默认
StorageClass；显式空字符串则是 Kubernetes 的“不使用 StorageClass”语义，
两者不等价。每个 shard 是独立 StatefulSet，每个 replica ordinal 都由
`volumeClaimTemplate` 获得独立 PVC，不在 Pod 之间共享 WAL。一期预检的运行子集
仍只接受 `replicas: 1` 或用 `replicas: 0` 挂起。

PVC 的生命周期故意独立于 Agent workload：缩减 replicas（包括用
`replicas: 0` 挂起）、缩减 shards、删除 StatefulSet 或删除 Agent 都不删除
已有 PVC。这一边界依赖 StatefulSet
volumeClaimTemplate 原有的保留语义，不配置 Kubernetes 1.19 不支持的
PVC retention policy。在同一 Namespace 以同一 Agent name 重建时，相同
shard/ordinal 会复用同名 PVC，这被定义为恢复操作。要创建全新实例，
必须使用新 Agent name，或由管理员先明确删除旧 PVC 并确认底层 PV 的
StorageClass reclaim policy。

`volumeClaimTemplate` 首次创建后视为不可变；严格预检会在调谐前拒绝
对现存 Agent 修改或移除模板。PVC 扩容、存储迁移、PVC 删除与底层
PV 回收都属于集群管理员和 StorageClass policy 的责任，不由本项目自动执行。

`prometheus-agent-runtime` ClusterRole 是一期 Agent 共享的 discovery 权限集。
每个 Agent 仍要在自己 namespace 中使用独立 ServiceAccount，并用同时包含
namespace/name 身份的独立 ClusterRoleBinding 绑定该共享角色。这些管理员资源
不设置指向 Agent 的 ownerReference，也不随 Agent 删除而清理。

预检以 `namespace/name` 作为 Agent 身份，按 Prometheus Operator v0.83.0 的精确规则
检查每个 shard 的 `prom-agent-*` StatefulSet/Pod、配置/TLS/web Secrets 和可选
retained PVC。无关对象占用任一保留名时必须先由管理员处理，预检不会修改、
删除或接管它。默认 `prometheus-agent-operated` 是例外：它是 namespace 内所有
Agent 的共享 headless governing Service，只要结构和普通 Agent ownerReferences 合法
就可复用；结构中的端口名取自 Agent 的 `portName`。显式 `serviceName` 使用上游自定义
governing Service 路径，不检查默认名称。已有 retained PVC 会按预期名称、
项目/Agent/shard labels、删除状态、accessModes、volumeMode、Kubernetes Quantity
等价容量和显式 StorageClass 核对；无法确认来源、正在删除或存储属性不匹配时
都会非零退出。计划未显式声明 StorageClass 时，预检保留创建 PVC 时由默认
StorageClass 决定的上游语义；名称尚未存在时，也不额外收窄上游 storage 能力。
预检是创建前的时点检查，不代替 GitOps 变更控制。

一期正常运行仅支持省略 `replicas` 或设为 `1`；`replicas: 0` 挂起所有 shards；
`replicas > 1` 仍是上游能力，但不能通过一期预检。扩展吞吐时使用至少为 `1`
的 `shards`，每个 shard 会获得独立 StatefulSet、Pod 和 WAL。
公开示例固定 `replicas: 1`、`shards: 2` 和 `walCompression: true`，且不通过
`additionalArgs` 改写 WAL retention 或 truncate frequency。

示例的 `victoriametrics-primary` 目标使用集群内 HTTP VictoriaMetrics Cluster
URL、accountID `0`、无认证且无 TLS，并显式选择 remote write `V1.0`。实际部署时
应把 URL 中的 accountID 与 VictoriaMetrics tenant 规划保持一致，不得把 userinfo、token
或密码写入 URL。`mih_cluster_id` 必须替换为集群内一致、跨集群唯一且在集群
生命周期内稳定的值；预检会拒绝通过采集配置 labels、target label 传播或
relabeling 产生和修改该标签的计划。

示例中的四类 object selector 都是明确的非空 `LabelSelector`。上游允许 `{}` 选择
可见范围内的全部对象，但一期预检会拒绝这种开放选择；省略某类 object selector
则表示不选择该类，四类都省略也是合法零配置。

每类 namespace selector 独立生效：省略表示 Agent 当前 namespace，`{}` 表示
Prometheus Operator watch scope 内的全部 namespace，非空 selector 按 Namespace
label 选择。object/namespace selector 只决定哪些采集配置对象进入 Agent 配置，不能
限制这些配置最终发现的目标。真正的安全边界是 Prometheus Operator watch scope 与
读取 Secret/ConfigMap 的权限、Agent runtime RBAC、网络策略和目标凭据。

统一 runtime ClusterRole 覆盖 services、endpoints、pods、configmaps、ingresses、
endpointslices 和 `/metrics`，故同一份角色可用于 Kubernetes 1.19 的 Endpoints 与
1.28 的 EndpointSlice 路径。它刻意不授予 `nodes`。若管理员使用 Node discovery、
node metadata 或一期外 discovery，必须自行审查并追加对应的权限、网络和凭据；预检
不会把这些能力报告为一期支持。

Secret/ConfigMap key selector 没有 namespace 字段，引用始终解析到采集配置对象所在
namespace；Agent 自身的 Secret/ConfigMap 引用则解析到 Agent namespace。预检会按这
一原生边界检查对象与读取覆盖，不能用跨 namespace 同名对象替代缺失引用。

排障时先解码 Operator 生成的 `prom-agent-<name>` Secret，确认配置对象对应的 job
是否存在；再查看 Agent 的 `/api/v1/status/config` 与 `/api/v1/targets`，确认配置已经
加载并产生真实 target。被拒绝的对象不会进入这些观察面，应转而查看该对象上的
`InvalidConfiguration` Event 和 Prometheus Operator logs。`PrometheusAgent.status`
只表达调谐/副本状态，不提供选中或拒绝对象的完整清单，也不能替代 targets 与最终
数据面验证。
