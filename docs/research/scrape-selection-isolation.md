# PrometheusAgent 采集配置选择与隔离事实

## 研究范围

本研究只核对已经锁定的一期依赖组合：

- Prometheus Operator `v0.83.0`，源码提交 `5cf2f5d37906d64b2259a262e319df4c74639e31`；
- Prometheus `v2.55.1`，源码提交 `6d7569113f1ca814f1e149f74176656540043b8d`；
- Kubernetes `1.19.x` 与 `1.28.x`；
- 集群管理员直接创建原生 `PrometheusAgent`；自研 Operator 不 watch、不代理、不修改 `PrometheusAgent` 及四类采集配置。

目标是给 Wayfinder ticket“定义采集配置选择与隔离语义”提供事实，不替该 ticket 作产品决策。

## 结论摘要

1. `serviceMonitorSelector`、`podMonitorSelector`、`probeSelector`、`scrapeConfigSelector` 是四个相互独立的对象 selector。对象 selector 为 `null` 时该类型选中 0 个对象，为 `{}` 时选中可见 namespace 中的全部对象，非空时按 Kubernetes `LabelSelector` 匹配。
2. 四个 namespace selector 也彼此独立：`null` 只看 `PrometheusAgent` 所在 namespace，`{}` 看 Prometheus Operator watch scope 内的全部 namespace，非空时按 Namespace label 匹配。它们不能越过外部 Prometheus Operator 的 `--namespaces` / `--deny-namespaces`。
3. **需要修正此前研究中的一项结论：**`CommonPrometheusFields` 注释称四类对象 selector 全为 `null` 会进入 unmanaged configuration，但 `v0.83.0` 只有 `Prometheus` server controller 实现了这个特殊分支；`PrometheusAgent` controller 没有。对 `PrometheusAgent` 而言，四者全为 `null` 会继续生成配置 Secret，只是四类对象均选中 0 个，不能作为 raw config 接管方式。
4. Agent selector 只隔离“采集配置对象”，不隔离“采集目标”。被选中的 `ServiceMonitor`、`PodMonitor`、`Probe` 和 `ScrapeConfig` 仍可扩大目标发现范围；尤其 `ScrapeConfig.kubernetesSDConfigs[].namespaces` 省略时默认发现所有 namespace。`ignoreNamespaceSelectors=true` 只收紧前三类 monitor/probe，不约束 `ScrapeConfig`。
5. 多个 `PrometheusAgent` 可以同时选中同一配置，没有 claim、owner 或跨实例去重。每个实例都会生成自己的 scrape job 并独立抓取、remote write。默认 external labels 会用 `prometheus=<namespace>/<agent-name>` 和 `prometheus_replica=<pod-name>` 区分实例/副本，但这不是采集去重协议。
6. `PrometheusAgent.status` 不展示选中/拒绝的配置名称或数量。Operator debug log 展示选中名称，warning log 与被拒绝对象的 Event 展示无效配置；`prometheus_operator_managed_resources` 只给出 controller 级聚合数量。生成的配置 Secret、Agent `/api/v1/status/config`、`/api/v1/scrape_pools` 和 `/api/v1/targets` 可反查已接受的 job，不包含被拒绝对象的完整清单。
7. 默认分片键为：`ServiceMonitor`、`PodMonitor`、`ScrapeConfig` 使用 `__address__`，`Probe` 使用 `__param_target`。用户 relabel 可写 `__tmp_hash` 改键，或写非空 `__tmp_disable_sharding` 让目标被所有 shard 抓取。
8. 官方 Agent ClusterRole 足以覆盖 Kubernetes 1.19 上默认 `ServiceMonitor` 的 Endpoints discovery、`PodMonitor`、Ingress `Probe` 和对应的 pod/service/ingress 发现；但 Kubernetes 1.28 上 Operator 自动为 `ServiceMonitor` 生成 EndpointSlice discovery，官方 Agent ClusterRole 缺少 `discovery.k8s.io/endpointslices`，必须补权。`attachMetadata.node=true` 或 `ScrapeConfig` 的 Node role 还需要 core `nodes`。
9. `ScrapeConfig` 在 v0.83.0 暴露 23 类 target/discovery source；其中 Kubernetes SD 支持 `Pod`、`Service`、`Endpoints`、`EndpointSlice`、`Node`、`Ingress` 六种 role。除 Kubernetes SD 外，其他 source 不需要 Kubernetes discovery RBAC，但仍可能需要网络、文件或云凭证。
10. Secret/ConfigMap key selector 没有 namespace 字段。Operator 始终按“引用它的采集配置对象所在 namespace”读取凭证；`PrometheusAgent` 自身的 remote write、`secrets`、`configMaps` 等则从 Agent 所在 namespace 读取。跨 namespace 选中采集配置因此要求 Prometheus Operator 自身能读取那些 namespace 的 Secret/ConfigMap，但不允许一个采集配置用 key selector 直接引用另一个 namespace。

## 1. 两层 selector 不能混为一谈

### 1.1 第一层：`PrometheusAgent` 选择采集配置对象

四类对象 selector 与四类 namespace selector 的语义相同，但状态相互独立：

| 字段状态 | 对象 selector | namespace selector |
| --- | --- | --- |
| `null` / 省略 | 该类型匹配 0 个对象 | 只从 `PrometheusAgent` 所在 namespace 读取该类型对象 |
| `{}` | 匹配可见范围内该类型全部对象 | 匹配 Operator watch scope 内全部 Namespace |
| 非空 | `matchLabels` 与 `matchExpressions` 按 Kubernetes `LabelSelector` 语义共同过滤 | 对 Namespace 对象的 labels 做相同过滤 |

字段注释见 [`CommonPrometheusFields`](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/apis/monitoring/v1/prometheus_types.go#L128-L202)。实现上，Kubernetes `LabelSelectorAsSelector(nil)` 返回 `labels.Nothing()`，空结构返回 `labels.Everything()`；非空 selector 的多个 requirement 最终共同组成 selector。[Kubernetes apimachinery `LabelSelectorAsSelector`](https://github.com/kubernetes/apimachinery/blob/173776a0582da70432e19d16eca025c451600ae8/pkg/apis/meta/v1/helpers.go#L33-L70)

每种资源的选择器、namespace selector 和结果 map 都独立计算：[`ServiceMonitor`](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/prometheus/resource_selector.go#L74-L125)、[`PodMonitor`](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/prometheus/resource_selector.go#L393-L444)、[`Probe`](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/prometheus/resource_selector.go#L547-L596)、[`ScrapeConfig`](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/prometheus/resource_selector.go#L743-L793)。因此：

- 把 `serviceMonitorNamespaceSelector` 设为 `{}` 不会扩大其他三类资源的 namespace 范围；
- namespace selector 非空但对应对象 selector 为 `null` 时，该类型仍选中 0 个；
- 至少一个 selector 非 `null` 并不会给其他 `null` selector 补默认值。

### 1.2 四类 selector 全 `null`：Agent 不是 unmanaged

API 注释把“四类对象 selector 全 `null`”描述为 deprecated unmanaged configuration。[字段注释](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/apis/monitoring/v1/prometheus_types.go#L128-L202) 但 controller 实现并不一致：

- `Prometheus` server controller 明确检查四者全 `nil`，只保证空 Secret 存在并让用户提供 raw `prometheus.yaml.gz`。[server controller 特殊分支](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/prometheus/server/operator.go#L1112-L1137)
- `PrometheusAgent` controller 没有这段检查。它总是逐一调用四种 `Select*`，生成 Agent 配置，再创建或更新配置 Secret。[Agent 配置调谐](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/prometheus/agent/operator.go#L837-L905)

结合 `nil` 对象 selector 等于 `labels.Nothing()`，**v0.83.0 的实际 Agent 行为是“生成配置但四类对象均为 0”**。不能把 server controller 的 unmanaged 行为外推到 `PrometheusAgent`。

### 1.3 第二层：采集配置选择目标

Agent 的 namespace selector 只决定去哪里读取 `ServiceMonitor` 等 CR，不限制这些 CR 能发现哪些目标：

- `ServiceMonitor.spec.namespaceSelector` 和 `PodMonitor.spec.namespaceSelector` 默认只发现各自 CR 所在 namespace；`any=true` 可发现全部 namespace，`matchNames` 可指定一组 namespace。[ServiceMonitor](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/apis/monitoring/v1/servicemonitor_types.go#L84-L100)、[PodMonitor](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/apis/monitoring/v1/podmonitor_types.go#L79-L95)、[`NamespaceSelector` 规则](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/apis/monitoring/v1/types.go#L943-L958)
- `Probe` 的 static targets 可以是任意地址；Ingress targets 也有自己的 `namespaceSelector`。[Probe target 类型](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/apis/monitoring/v1/probe_types.go#L175-L201)
- `ScrapeConfig.kubernetesSDConfigs[].namespaces` 省略时，Prometheus 默认跨全部 namespace 发现目标，不是“当前 ScrapeConfig namespace”。[`KubernetesSDConfig` 字段说明](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/apis/monitoring/v1alpha1/scrapeconfig_types.go#L429-L456)

`PrometheusAgent.spec.ignoreNamespaceSelectors=true` 会把 ServiceMonitor、PodMonitor 和 Probe 的 target namespace 强制为各自 CR namespace，[字段契约](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/apis/monitoring/v1/prometheus_types.go#L552-L566) 与[配置生成逻辑](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/prometheus/promcfg.go#L2180-L2192) 都没有包含 `ScrapeConfig`。因此它不是四类配置的统一 namespace isolation 开关。

最终能否发现目标还取决于 Agent runtime ServiceAccount 的 RBAC。selector 本身不授予也不收回 API 权限。

## 2. 外部 Prometheus Operator watch scope

v0.83.0 的相关 flags 是：

- `--namespaces`：common CR 的 namespace allow list；
- `--deny-namespaces`：common CR 的 namespace deny list，与 `--namespaces` 互斥；
- `--prometheus-instance-namespaces`：仅覆盖 `Prometheus` / `PrometheusAgent` CR 及其 Secret、ConfigMap、StatefulSet 的 namespace 范围；
- `--prometheus-instance-selector`：按 label 过滤要调谐的 `Prometheus` / `PrometheusAgent` 实例；
- `--controller-id`：按 `operator.prometheus.io/controller-id` annotation 选择实例。

flag 定义见 [namespace flags 与 controller ID](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/cmd/operator/main.go#L163-L170) 和 [`--prometheus-instance-selector`](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/cmd/operator/main.go#L178-L184)。

Agent controller 的 informer 也证明两类范围不同：`PrometheusAgent` 使用 `PrometheusAllowList`，四类采集配置使用 common `AllowList` / `DenyList`，namespace label 匹配也只在 common namespace informer 可见范围内发生。[Agent 与四类采集配置 informer](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/prometheus/agent/operator.go#L171-L259)、[namespace informer](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/prometheus/agent/operator.go#L328-L360)

所以 CR 中的 namespace selector `{}` 只代表“Operator 当前能 watch 到的全部 namespace”，不是无条件集群全局。

## 3. 多 Agent 重叠选择没有所有权或去重

资源选择发生在每个 `PrometheusAgent` 的 reconcile 内，结果用于该实例自己的配置 Secret；配置 Secret owner 也是该 Agent。[Agent 每实例配置调谐](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/prometheus/agent/operator.go#L837-L905)、[配置 Secret owner 与命名](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/prometheus/common.go#L125-L150)

`ServiceMonitor`、`PodMonitor`、`Probe`、`ScrapeConfig` 不会被某个 Agent claim，也不会增加指向 Agent 的 ownerReference。因此多个 Agent selector 重叠时，每个 Agent 都会生成并执行同一配置对应的 job；Prometheus Operator 不做跨实例目标去重。

默认情况下，Operator 会给 remote write 样本增加：

- `prometheus=<agent-namespace>/<agent-name>`；
- `prometheus_replica=<pod-name>`。

见 [external labels 生成](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/prometheus/promcfg.go#L702-L730)。这通常使不同 Agent 的样本成为不同 series，但仍然发生了重复抓取和发送；后端是否按这些 labels 做 HA dedup 是后端自己的能力，Operator 不提供保证。管理员还可以禁用或改写这些 external label，使重叠实例发生 timestamp/series 冲突。

## 4. 选中和拒绝结果在哪里可见

| 观察面 | 名称 | 数量 | 说明 |
| --- | --- | --- | --- |
| `PrometheusAgent.status` | 否 | 否 | 只有 paused、replicas、available/updated/unavailable replicas、conditions、shard statuses、shards、pod selector |
| Kubernetes Events | 仅被拒绝对象 | 否 | Operator 具备 Events 写权限时，invalid configuration 会在对应 ServiceMonitor/PodMonitor/Probe/ScrapeConfig 上产生 `InvalidConfiguration` Warning Event；正常选中不发 Event |
| Prometheus Operator logs | 是 | 间接 | 选中名称只在 debug log；拒绝名称与原因是 warning log |
| Operator `/metrics` | 否 | 是，但仅 controller 聚合 | `prometheus_operator_managed_resources{controller="prometheus-agent",resource=...,state="selected|rejected"}`；实现会把所有 Agent 的 per-object map 求和，没有 Agent name/namespace label |
| 生成的配置 Secret | 仅已接受对象 | 可由 job 数推导 | `prom-agent-<name>` Secret 的 `prometheus.yaml.gz`；job name 编码类型、namespace、name，ServiceMonitor/PodMonitor 还含 endpoint index |
| Agent HTTP API | 仅已加载/运行 job | job/target 数 | Agent 模式可用 `/api/v1/status/config`、`/api/v1/scrape_pools`、`/api/v1/targets`；它们不知道被 Operator 拒绝而未进入配置的对象 |

Status 字段见 [`PrometheusStatus`](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/apis/monitoring/v1/prometheus_types.go#L1272-L1301)。四类资源的 selected/rejected log、Event 和计数模式分别见 [`ServiceMonitor`](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/prometheus/resource_selector.go#L126-L235)、[`PodMonitor`](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/prometheus/resource_selector.go#L445-L544)、[`Probe`](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/prometheus/resource_selector.go#L597-L701)、[`ScrapeConfig`](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/prometheus/resource_selector.go#L794-L981)。Operator metric 的无实例 label 聚合实现见 [`prometheus_operator_managed_resources`](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/operator/operator.go#L47-L57) 与[收集逻辑](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/operator/operator.go#L320-L379)。

生成 job name 分别是 [`podMonitor/<ns>/<name>/<endpoint-index>`](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/prometheus/promcfg.go#L1285-L1300)、[`probe/<ns>/<name>`](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/prometheus/promcfg.go#L1547-L1558)、[`serviceMonitor/<ns>/<name>/<endpoint-index>`](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/prometheus/promcfg.go#L1785-L1801)、[`scrapeConfig/<ns>/<name>`](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/prometheus/promcfg.go#L3126-L3138)。

Prometheus v2.55.1 的 Agent mode 只屏蔽 query、rules、alerts 等 server API；targets、scrape pools 和 status/config 没有经过 `wrapAgent`，仍然可用。[HTTP API 路由](https://github.com/prometheus/prometheus/blob/6d7569113f1ca814f1e149f74176656540043b8d/web/api/v1/api.go#L345-L407)

## 5. 分片键

`CommonPrometheusFields.shards` 的上游契约明确规定：

- `ServiceMonitor`、`PodMonitor`、`ScrapeConfig` 默认按 target metadata 的 `__address__` hash；
- `Probe` 默认按 `__param_target` hash；
- relabel 写入 `__tmp_hash` 可替换默认 key；
- relabel 写入非空 `__tmp_disable_sharding` 会让所有 shard 保留该 target。

字段契约见 [`shards`](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/apis/monitoring/v1/prometheus_types.go#L245-L272)，具体生成规则见 [`appendShardingRelabeling*`](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/prometheus/promcfg.go#L2100-L2142)。

这只是同一个 Agent 的 target sharding；不会在多个独立 Agent 之间协调。

## 6. Runtime ServiceAccount RBAC

### 6.1 官方 Agent ClusterRole 的固定基线

v0.83.0 官方 Agent 示例授予：

- core `services`、`endpoints`、`pods`：`get/list/watch`；
- core `configmaps`：`get`；
- `networking.k8s.io/ingresses`：`get/list/watch`；
- non-resource `/metrics`：`get`。

它没有 `nodes`，也没有 `discovery.k8s.io/endpointslices`。[官方 Agent ClusterRole](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/example/rbac/prometheus-agent/prometheus-cluster-role.yaml#L1-L23)

### 6.2 按采集配置类型拆解

| 配置/发现方式 | Prometheus runtime 需要的 Kubernetes read 权限 | 相对官方 Agent ClusterRole |
| --- | --- | --- |
| `PodMonitor` | core `pods` | 已覆盖；`attachMetadata.node=true` 另需 core `nodes` |
| `ServiceMonitor` on K8s 1.19 | core `endpoints`、`services`、`pods` | 已覆盖；node metadata 另需 `nodes` |
| `ServiceMonitor` on K8s 1.28 | `discovery.k8s.io/endpointslices` + core `services`、`pods` | **缺 `endpointslices`**；node metadata 另需 `nodes` |
| `Probe` static targets | 无 Kubernetes discovery RBAC | 目标/prober 自身仍可能要求网络认证 |
| `Probe` ingress targets | `networking.k8s.io/ingresses` | 已覆盖 |
| `ScrapeConfig` static/file/HTTP/DNS/cloud/其他外部 SD | 无 Kubernetes discovery RBAC | 文件挂载、网络与外部凭证另算 |
| `ScrapeConfig` Kubernetes role `Pod` | core `pods` | 已覆盖；node metadata 另需 `nodes` |
| role `Service` | core `services` | 已覆盖 |
| role `Endpoints` | core `endpoints`、`services`、`pods` | 已覆盖；node metadata 另需 `nodes` |
| role `EndpointSlice` | `discovery.k8s.io/endpointslices` + core `services`、`pods` | **缺 `endpointslices`**；node metadata 另需 `nodes` |
| role `Node` | core `nodes` | **缺 `nodes`** |
| role `Ingress` | `networking.k8s.io/ingresses` | 已覆盖 |

Prometheus v2.55.1 的 Kubernetes discovery 实现直接展示了各 role 创建的 informer：[`EndpointSlice` 与 `Endpoints`](https://github.com/prometheus/prometheus/blob/6d7569113f1ca814f1e149f74176656540043b8d/discovery/kubernetes/kubernetes.go#L403-L557)、[`Pod`、`Service`、`Ingress`、`Node`](https://github.com/prometheus/prometheus/blob/6d7569113f1ca814f1e149f74176656540043b8d/discovery/kubernetes/kubernetes.go#L558-L681)。这里列的是 discovery 所需读权限；抓取目标 URL 所需的目标侧认证或 Kubernetes API proxy 权限必须按实际 endpoint 另行授权。

### 6.3 Kubernetes 1.19 与 1.28 的 EndpointSlice 差异

Prometheus Operator v0.83.0 以 Kubernetes server version `>=1.21` 为条件启用 EndpointSlice support。[能力判断](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/cmd/operator/main.go#L351-L357) 配置生成器在 support 开启且 Prometheus `>=2.21` 时，把 `ServiceMonitor` 的 Kubernetes SD role 从 `endpoints` 切换成 `endpointslice`。[role 选择](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/prometheus/promcfg.go#L167-L175)

因此本项目两个目标集群的实际共同点和差异是：

- Kubernetes 1.19：自动生成的 `ServiceMonitor` job 使用 core/v1 Endpoints；
- Kubernetes 1.28：自动生成的 `ServiceMonitor` job 使用 discovery.k8s.io/v1 EndpointSlice；
- 两边使用同一份官方 Agent ClusterRole 会导致 1.28 缺 EndpointSlice read 权限。

`ScrapeConfig` 显式写 `role: EndpointSlice` 是另一条路径，不受 Operator 的自动 role 选择约束。Prometheus v2.55.1 会先探测 discovery/v1，不存在时回退 discovery/v1beta1；两者 RBAC 都是 `apiGroups: ["discovery.k8s.io"]`、resource `endpointslices`。[Prometheus EndpointSlice API fallback](https://github.com/prometheus/prometheus/blob/6d7569113f1ca814f1e149f74176656540043b8d/discovery/kubernetes/kubernetes.go#L403-L480)

## 7. `ScrapeConfig` 的 discovery 能力面

v0.83.0 的 `ScrapeConfigSpec` 暴露 23 类 source：

`static`、`file`、`HTTP`、`Kubernetes`、`Consul`、`DNS`、`EC2`、`Azure`、`GCE`、`OpenStack`、`DigitalOcean`、`Kuma`、`Eureka`、`Docker`、`Linode`、`Hetzner`、`Nomad`、`DockerSwarm`、`PuppetDB`、`Lightsail`、`OVHcloud`、`Scaleway`、`IONOS`。

完整字段列表见 [`ScrapeConfigSpec`](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/apis/monitoring/v1alpha1/scrapeconfig_types.go#L147-L228)。Kubernetes SD 的 role 枚举是 `Pod`、`Endpoints`、`Ingress`、`Service`、`Node`、`EndpointSlice`，[枚举定义](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/apis/monitoring/v1alpha1/scrapeconfig_types.go#L81-L103)；v0.83.0 还会校验 role selector 只能引用与 discovery role 兼容的 Kubernetes resources。[校验逻辑](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/prometheus/resource_selector.go#L983-L1048)

因此“允许选择任意带 label 的 `ScrapeConfig`”并不等于一个可静态穷举的最小 runtime RBAC。管理员必须知道每个被选中 `ScrapeConfig` 的 discovery source/role，并为 Agent ServiceAccount 补足对应权限；非 Kubernetes source 则需要相应网络出口和凭证。

## 8. Secret、ConfigMap 与文件边界

### 8.1 Key selector 只能引用当前采集配置 namespace

`SecretKeySelector` / `ConfigMapKeySelector` 本身没有 namespace 字段。Prometheus Operator 读取 asset 时，调用者显式传入 namespace；store 的 key 也是 `type/namespace/name`。[asset store 读取实现](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/assets/store.go#L215-L315)

四类资源的调用方式都是用采集配置对象自己的 namespace，例如 ServiceMonitor、PodMonitor、Probe 和 ScrapeConfig 各自在 `Select*` 阶段把 `object.GetNamespace()` 传给 store。[ServiceMonitor](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/prometheus/resource_selector.go#L126-L225)、[PodMonitor](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/prometheus/resource_selector.go#L445-L544)、[Probe](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/prometheus/resource_selector.go#L597-L701)、[ScrapeConfig](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/prometheus/resource_selector.go#L794-L981)

所以：

- `team-a` namespace 的 `ServiceMonitor` 只能用 key selector 引用 `team-a` 的 Secret/ConfigMap；
- Agent 可以位于另一个 namespace，Operator 会读取 `team-a` 的 asset，把生成结果写进 Agent namespace 的配置/TLS Secret；
- Prometheus Operator 的 ServiceAccount 因而必须有读取所有被选中配置 namespace 中 Secret/ConfigMap 的权限；
- Agent runtime ServiceAccount 通常不直接读取这些 Secret/ConfigMap API 对象，凭证已由 Operator 渲染或复制到 Agent Pod 使用的 Secret。

`PrometheusAgent` 自己的 remote write、`additionalScrapeConfigs`、`spec.secrets`、`spec.configMaps` 等引用则以 Agent namespace 为边界。`spec.secrets` / `spec.configMaps` 的字段契约明确限定同 namespace 并挂载到固定目录。[Agent 共用字段](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/apis/monitoring/v1/prometheus_types.go#L411-L433)

### 8.2 文件访问仍是隔离缺口

`ScrapeConfig.fileSDConfigs[].files` 是 Pod 内文件路径，文件必须通过 Agent 的 `spec.configMaps` 或 `spec.secrets` 挂载。[`FileSDConfig`](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/apis/monitoring/v1alpha1/scrapeconfig_types.go#L370-L388) 因为挂载由 Agent 决定，而 file path 来自被选中的 ScrapeConfig，跨 namespace 选择不天然形成文件级租户隔离。

另外，`ServiceMonitor` 的 legacy `bearerTokenFile` / TLS file 字段可引用 Prometheus Pod 文件系统；`spec.arbitraryFSAccessThroughSMs.deny=true` 会拒绝这类 ServiceMonitor。[文件访问防护字段](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/apis/monitoring/v1/prometheus_types.go#L535-L551)、[拒绝逻辑](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/prometheus/resource_selector.go#L149-L168) 该开关不约束 `ScrapeConfig.fileSDConfigs`。

所以“只面向集群管理员”可以降低威胁模型，但不能把 label selector 当作安全边界。真正隔离仍来自 Operator watch scope、namespace/RBAC、Agent runtime RBAC、文件挂载集合和管理员准入流程。

## 9. 可供 Wayfinder 决策直接使用的事实边界

以下结论已经由源码确定：

- 四种对象/namespace selector 可以使用同一个稳定 label 约定，但必须逐字段显式填写；不存在一个全局 selector。
- 若目标是“默认不误选”，四种对象 selector 都应显式设置非空 label selector；不能依赖全 `null` 的所谓 unmanaged 语义。
- 若目标是跨 namespace 选择配置，namespace selector `{}` 仍受外部 Operator watch scope 限制。
- 选择同一配置的 Agent 之间没有排他或去重；避免重叠只能靠 label/namespace 规划或外部 admission/GitOps 检查。
- 选中/拒绝清单不在 Agent status；无需自研 controller 的前提下，只能组合 Operator Event/log/metric、生成 Secret 与 Agent HTTP API 排障。
- Kubernetes 1.28 的 ServiceMonitor runtime RBAC 必须比官方 Agent 示例多 `discovery.k8s.io/endpointslices`；Kubernetes 1.19 保持 Endpoints。
- 若允许任意 `ScrapeConfig`，不能声明一份“覆盖全部 discovery 且最小权限”的 runtime ClusterRole；额外 RBAC、网络和凭证由管理员按配置负责。
- `ignoreNamespaceSelectors` 无法约束 `ScrapeConfig`；label 选择也不是 target namespace isolation。

## 10. 尚未由上游事实替项目决定的问题

这些需要 Wayfinder 由人作决定，不能从源码自动推出：

1. 一期是否同时支持四类采集配置，还是仅保证其中一个子集；
2. 是否规定四类对象都使用同一稳定 label key/value，以及 value 如何区分多套 Agent；
3. namespace selector 是统一使用 `{}`，还是要求显式 namespace labels 以降低误选；
4. 是否允许多个 Agent 有意重叠选择；若允许，谁负责 remote backend 的重复序列/HA dedup 契约；
5. 是否把 `ignoreNamespaceSelectors=true`、`arbitraryFSAccessThroughSMs.deny=true` 纳入一期验收模板；
6. 是否支持全部 `ScrapeConfig` source，还是只声明经过验收的 source/role 子集；
7. runtime ClusterRole 是为两个 Kubernetes 版本统一加入 `endpointslices`/`nodes`，还是按实际配置由管理员增量授权；
8. 选中 0 个配置是正常空状态还是配置错误，以及验收时用哪一观察面判断；
9. 外部 Prometheus Operator 的 `--namespaces` / `--deny-namespaces` 是否必须为空，还是允许受限 watch scope 并将其记录为集群依赖配置。
