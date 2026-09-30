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
governing Service 路径，不检查默认名称。已有 retained PVC 会按计划中的 accessModes、
volumeMode、容量和显式 StorageClass 核对；名称尚未存在时，预检不会额外收窄上游
storage 能力。预检是创建前的时点检查，不代替 GitOps 变更控制。

一期正常运行仅支持省略 `replicas` 或设为 `1`；`replicas: 0` 挂起所有 shards；
`replicas > 1` 仍是上游能力，但不能通过一期预检。扩展吞吐时使用至少为 `1`
的 `shards`，每个 shard 会获得独立 StatefulSet、Pod 和 WAL。

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
