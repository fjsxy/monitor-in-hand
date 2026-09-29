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
