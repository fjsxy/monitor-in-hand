# PrometheusAgent API 与 Prometheus Operator 合约事实

## 研究范围

本研究只核对一期已锁定的依赖组合：

- Prometheus Operator `v0.83.0`，源码提交 `5cf2f5d37906d64b2259a262e319df4c74639e31`；
- Prometheus `v2.55.1`，源码提交 `6d7569113f1ca814f1e149f74176656540043b8d`；
- Kubernetes `1.19.x` 与 `1.28.x`。

结论用于 Wayfinder ticket“定义采集实例 API 与 Prometheus Operator 合约”，不等于最终 API 决策。

## 结论摘要

1. 一个逻辑采集实例只需要创建一个 `PrometheusAgent` CR。默认 `StatefulSet` 模式下，Prometheus Operator 按 `shards` 创建同等数量的 `StatefulSet`，每个 `StatefulSet` 的副本数为 `replicas`，总 Pod 数为 `replicas × shards`。同一 shard 内的 replicas 抓取相同目标，是高可用副本；不同 shard 通过 relabel hash 分摊目标。
2. `replicas=0` 在 v0.83.0 中可用，会把每个 shard 的 `StatefulSet` 缩到 0；`paused=true` 只冻结调谐，已有 Pod 仍会继续采集和 remote write，因此不能用作“挂起”。
3. `PrometheusAgent` 没有 `retention`、rules、alerting、query、remote read、Thanos 等 server-only 字段；但它继承了 `storage` 和 `walCompression`。Agent 会把待发送样本写入本地 WAL，默认使用 `EmptyDir`，并不是“完全无本地数据”。
4. 应显式设置 `spec.version: v2.55.1` 和固定的 `spec.image`。v0.83.0 未指定版本时默认的是当时最新 Prometheus `v3.4.0`，而 `image` 与 `version` 是两个相互独立的输入，Operator 会信任 `version` 来生成配置。
5. 对跨 namespace 的四类采集配置，应给四个对象 selector 都写入同一稳定 label selector，并把四个 namespace selector 设为空对象 `{}`。`null` 对象 selector 表示不选择；四者全为 `null` 还会进入已废弃的 unmanaged configuration 模式。
6. runtime `ServiceAccount` 的固定最小 RBAC 可以覆盖 `ServiceMonitor`、`PodMonitor` 和 ingress/static `Probe`；但任意 `ScrapeConfig` 能声明不同的 Kubernetes SD role，所以不存在既支持所有 `ScrapeConfig` 又保持最小权限的静态 ClusterRole。这个边界必须由产品契约决定。
7. CRD annotation/schema 或 server-side dry-run 只能证明 API capability，不能证明当前 Prometheus Operator controller 正在运行。一次成功的、带当前 `observedGeneration` 的 `Reconciled=True` 加受管 `StatefulSet` 能证明该实例曾被实际调谐，但 status 没有 heartbeat，不能长期证明 controller 存活。

## 1. `PrometheusAgent` 的 API 结构

### 1.1 继承关系决定了可用字段

`PrometheusAgentSpec` 只有自己的 `mode`，其余能力全部来自内嵌的 `CommonPrometheusFields`。[PrometheusAgentSpec 源码](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/apis/monitoring/v1alpha1/prometheusagent_types.go#L92-L114)

一期相关字段如下：

| 字段 | v0.83.0 行为 | API 设计含义 |
| --- | --- | --- |
| `replicas` | 每个 shard 的副本数；未设置为 1 | wrapper 可暴露，但应自己限制 `>=1`，挂起单独表达 |
| `shards` | 目标分片数；未设置为 1 | wrapper 应限制 `>=1` |
| `resources` | 仅作用于 `prometheus` container；未设置时 requests/limits 都为空 | 可直接映射用户配置，但不是整个 Pod 的总资源 |
| `serviceAccountName` | 未设置时 Pod 使用 namespace 的 `default` ServiceAccount | wrapper 应始终生成并显式引用独立 ServiceAccount |
| `version` | 未设置时采用 Operator 自带默认版本 | wrapper 必须固定为 `v2.55.1` |
| `image` | 非空时直接覆盖镜像地址；仍需同时填写 `version` | 若按 digest 固定镜像，仍必须设置匹配的 `version` |
| `paused` | 仅停止 Operator 对底层对象的新增/更新动作，删除仍由 ownerReferences 处理 | 不能映射为“挂起” |
| `storage` | 支持 `EmptyDir`、ephemeral volume 或 PVC template | 一期若不暴露，保持 `nil` 即为 `EmptyDir` |
| `walCompression` | `nil` 时由 Prometheus 默认决定；v2.55.1 默认开启 | 一期可不暴露 |
| `remoteWrite` | 数组，允许多个 remote write target | 可映射一期多目标需求 |

字段原始定义见 [`CommonPrometheusFields`](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/apis/monitoring/v1/prometheus_types.go#L204-L263)、[`storage/resources/serviceAccountName/remoteWrite`](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/apis/monitoring/v1/prometheus_types.go#L365-L444) 和 [`walCompression`](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/apis/monitoring/v1/prometheus_types.go#L737-L758)。

### 1.2 `replicas`、`shards` 的真实默认与边界

v0.83.0 的 CRD schema 只声明二者为 `int32`，没有 `minimum`。controller 的归一化规则是：

- `replicas=nil` → 1；
- `replicas<0` → 1；
- `replicas=0` → 0；
- `shards=nil`、`shards<=1` → 1；
- `shards>1` → 原值。

证据见 [`ReplicasNumberPtr` 与 `shardsNumber`](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/prometheus/common.go#L70-L105) 和[生成 CRD 中没有 minimum 的 `replicas`/`shards`](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/example/prometheus-operator-crd/monitoring.coreos.com_prometheusagents.yaml#L6259-L6270)。

因此 wrapper 不应把上游的宽松归一化暴露为自己的稳定契约：正常运行时建议校验 `replicas>=1`、`shards>=1`；挂起时由 controller 内部临时渲染 `replicas=0`。

### 1.3 `paused` 不是停机

Prometheus Operator 在读取到 `spec.paused=true` 后直接跳过 reconcile，不会缩容或删除已存在的 `StatefulSet`。[同步逻辑](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/prometheus/agent/operator.go#L565-L587)

相反，`replicas=0` 会原样进入每个生成的 `StatefulSet.spec.replicas`；status reporter 还把“desired replicas 为 0 且 ready 为 0”视为 `Available=True`。[StatefulSet replicas 映射](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/prometheus/agent/statefulset.go#L292-L320)；[status 判定](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/prometheus/operator.go#L139-L233)

这意味着 wrapper 自己必须用 `spec.suspended` 区分“预期为 0”和异常不可用，不能直接把上游 `Available=True` 转译成业务可用。

### 1.4 版本和镜像必须同时固定

v0.83.0 的 `DefaultPrometheusVersion` 是 compatibility matrix 最后一个版本，即 `v3.4.0`；`v2.55.1` 只是矩阵中的受支持 Prometheus 2.x 版本。[默认版本与兼容矩阵](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/operator/defaults.go#L40-L90)

`spec.image` 非空时直接决定容器镜像，但 `spec.version` 仍独立用于配置生成与 feature compatibility 判断。[镜像构造逻辑](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/operator/image.go#L21-L66)；[字段说明](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/apis/monitoring/v1/prometheus_types.go#L204-L227)

只固定 image、不固定 version 会让 Operator 按错误版本生成配置；只固定 version、不固定 image 则仍依赖默认 registry/tag。两者需要来自同一个内部 version profile。

## 2. Agent 模式不做什么，以及仍会落盘什么

### 2.1 明确缺失的 server 能力

`PrometheusAgentSpec` 没有 `PrometheusSpec` 额外增加的这些字段：

- `retention`、`retentionSize`、server block compaction；
- `rules`、`ruleSelector`、`ruleNamespaceSelector`、`evaluationInterval`；
- `query`、`queryLogFile`；
- `alerting`、Alertmanager 与 alert relabel 配置；
- `remoteRead`；
- `thanos`；
- server admin API。

对照证据见 [`PrometheusSpec` 的 server-only 字段](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/apis/monitoring/v1/prometheus_types.go#L1055-L1178)。Prometheus v2.55.1 本身也会在 Agent 模式拒绝 `alerting`、`rule_files` 和 `remote_read` 配置。[Agent 配置校验](https://github.com/prometheus/prometheus/blob/6d7569113f1ca814f1e149f74176656540043b8d/config/config.go#L115-L140)

remote write 并非 Prometheus Agent 的语法必填项；没有 `remote_write` 的 Agent 仍可启动。[官方测试用例](https://github.com/prometheus/prometheus/blob/6d7569113f1ca814f1e149f74176656540043b8d/config/config_test.go#L2136-L2156) 但一期产品目标是采集后发送到外部系统，因此 wrapper 应要求至少一个 remote write target。

### 2.2 Agent 仍使用本地 WAL

Agent 模式的官方定义是“discovery、scrape、remote write”，不提供本地查询。[Prometheus Agent feature 文档](https://github.com/prometheus/prometheus/blob/6d7569113f1ca814f1e149f74176656540043b8d/docs/feature_flags.md#L72-L81)

实现上，它仍将样本写入本地 WAL，让 remote write queue 可以重放和重试；querier 接口明确返回 `ErrUnsupported`。[Agent WAL 打开与回放](https://github.com/prometheus/prometheus/blob/6d7569113f1ca814f1e149f74176656540043b8d/tsdb/agent/db.go#L254-L319)；[查询接口不可用](https://github.com/prometheus/prometheus/blob/6d7569113f1ca814f1e149f74176656540043b8d/tsdb/agent/db.go#L727-L746)

v2.55.1 WAL 默认参数是：

- truncate frequency：2h；
- minimum WAL age：5m；
- maximum WAL age：4h；
- WAL compression：CLI 默认开启。

证据见 [`agent.DefaultOptions`](https://github.com/prometheus/prometheus/blob/6d7569113f1ca814f1e149f74176656540043b8d/tsdb/agent/db.go#L52-L101) 和 [Agent storage flags](https://github.com/prometheus/prometheus/blob/6d7569113f1ca814f1e149f74176656540043b8d/cmd/prometheus/main.go#L411-L438)。当 remote endpoint 长期不可达时，WAL 不会无限增长；truncate 会以 max age 作为强制上限。[WAL truncate 逻辑](https://github.com/prometheus/prometheus/blob/6d7569113f1ca814f1e149f74176656540043b8d/tsdb/agent/db.go#L600-L626)

PrometheusAgent CR 没有一等字段暴露 `storage.agent.retention.min-time/max-time`。虽然通用的 `additionalArgs` 技术上能传这些 flag，但它绕过类型化 API 和版本安全，不适合一期稳定契约。

### 2.3 `storage` 的实际行为

虽然 v0.83.0 的 `PrometheusAgent` 顶部注释仍称“不支持 persistent storage”，实际 Go 类型已经通过 `CommonPrometheusFields` 暴露 `storage`，controller 也会生成 PVC template；该注释与实现不一致，应以类型和 controller 为准。[注释与内嵌类型](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/apis/monitoring/v1alpha1/prometheusagent_types.go#L56-L68)；[storage 渲染](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/prometheus/agent/statefulset.go#L76-L126)

未设置 `storage` 时，Operator 给每个 Pod 使用 `EmptyDir`。它能跨 container restart 保留 WAL，但 Pod 被删除、重建或迁移后数据消失。若一期不承诺本地 retry buffer 的跨 Pod 持久性，不暴露 storage 是更小且一致的契约。

## 3. remote write 可映射能力

一个 `PrometheusAgent` 可配置多个 `remoteWrite` 项。每项至少要求 `url`，并支持独立认证、TLS、headers、write relabel、timeout、queue 与 metadata 参数。[`RemoteWriteSpec`](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/apis/monitoring/v1/prometheus_types.go#L1523-L1687)

与一期“性能参数”直接相关的 `queueConfig` 字段为：

- `capacity`；
- `minShards`、`maxShards`；
- `maxSamplesPerSend`；
- `batchSendDeadline`；
- `minBackoff`、`maxBackoff`；
- `retryOnRateLimit`；
- `sampleAgeLimit`。

类型定义见 [`QueueConfig`](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/apis/monitoring/v1/prometheus_types.go#L1693-L1730)。Prometheus v2.55.1 默认值为：`remoteTimeout=30s`、`minShards=1`、`maxShards=50`、`maxSamplesPerSend=2000`、`capacity=10000`、`batchSendDeadline=5s`、`minBackoff=30ms`、`maxBackoff=5s`。[Prometheus 默认值](https://github.com/prometheus/prometheus/blob/6d7569113f1ca814f1e149f74176656540043b8d/config/config.go#L186-L219)

注意这里有两种完全不同的 shard：

- `PrometheusAgent.spec.shards`：把 scrape targets 分给不同 Prometheus Pod；
- `remoteWrite[].queueConfig.minShards/maxShards`：单个 remote write queue 的发送并发度。

wrapper API 应避免把两者都简称为“分片数”。

## 4. 采集配置选择语义

四类 selector 的上游语义一致：

- 对象 selector 为 `null`：不选择该类型；
- 对象 selector 为 `{}`：选择该范围内全部对象；
- namespace selector 为 `null`：只看 PrometheusAgent 所在 namespace；
- namespace selector 为 `{}`：看全部 namespaces。

若 `serviceMonitorSelector`、`podMonitorSelector`、`probeSelector`、`scrapeConfigSelector` 全为 `null`，Operator 进入已废弃的 unmanaged configuration 模式，不会替用户生成 scrape config。[selector 字段说明](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/apis/monitoring/v1/prometheus_types.go#L128-L202)

因此，一期“选择带特殊 label 的采集配置”若包含四种 CR，应在同一个 PrometheusAgent 上设置：

```yaml
serviceMonitorSelector: <稳定 label selector>
serviceMonitorNamespaceSelector: {}
podMonitorSelector: <稳定 label selector>
podMonitorNamespaceSelector: {}
probeSelector: <稳定 label selector>
probeNamespaceSelector: {}
scrapeConfigSelector: <稳定 label selector>
scrapeConfigNamespaceSelector: {}
```

这只决定 Prometheus Operator 读取哪些采集配置，不授予 Prometheus Pod 读取目标对象的权限。

还有一层外部约束：Prometheus Operator 进程本身可通过 `--namespaces` / `--deny-namespaces` 限制它读取采集配置的 namespace，并可用 `--prometheus-instance-namespaces` 单独限制它调谐 `PrometheusAgent` 的 namespace。[v0.83.0 启动参数](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/cmd/operator/main.go#L165-L170) 即使 CR 中的 namespace selector 是 `{}`，也无法越过外部 Operator 自身的 watch scope。官方 bundle 未设置这些限制，但复用任意预装实例时必须把它列为依赖契约。

## 5. RBAC 与 ServiceAccount 边界

### 5.1 三个身份不能混为一谈

1. **本项目 Operator 的 ServiceAccount**：需要管理 wrapper CR、`PrometheusAgent`、其 runtime ServiceAccount 和 RBAC；若读取 CRD 做 capability gate，还需 `get` 相关 CRD。它不需要 watch 用户的四类采集配置，因为 label/namespace selector 已委托给 Prometheus Operator。
2. **预装 Prometheus Operator 的 ServiceAccount**：负责 watch 四类采集配置、读取其引用的 Secrets、生成配置和 workload。该权限属于外部依赖，不能由本项目悄悄修补。
3. **PrometheusAgent runtime ServiceAccount**：由生成的 Prometheus Pod 使用，负责 Kubernetes service discovery 和抓取目标。

### 5.2 官方 Agent RBAC 基线

v0.83.0 官方 Agent 示例授予 runtime ServiceAccount：

- `get/list/watch` core `services`、`endpoints`、`pods`；
- `get` core `configmaps`；
- `get/list/watch` `networking.k8s.io/ingresses`；
- `get` non-resource URL `/metrics`。

见[官方 `prometheus-agent` ClusterRole](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/example/rbac/prometheus-agent/prometheus-cluster-role.yaml#L1-L23) 和[官方 ClusterRoleBinding](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/example/rbac/prometheus-agent/prometheus-cluster-role-binding.yaml#L1-L14)。跨 namespace discovery 要用 ClusterRoleBinding，而不是仅绑定一个 namespace 的 Role。

这个基线覆盖默认 `Endpoints` 型 ServiceMonitor、PodMonitor、ingress Probe 与静态目标，但有两个限制：

- 若 wrapper 选择 `serviceDiscoveryRole: EndpointSlice`，runtime 还需要 `discovery.k8s.io/endpointslices`；Kubernetes 1.19 与 1.28 的可用 API version 不同，所以一期维持默认 `Endpoints` 更稳。
- `ScrapeConfig` 能表达多种 service discovery，特别是 Kubernetes SD 的 node、service、endpoints、endpointslice、ingress、pod 等 role。支持“任意用户 ScrapeConfig”就意味着固定 RBAC 无法同时做到最小权限。可以选择官方 Prometheus 较宽的 ClusterRole，或把一期支持的 ScrapeConfig discovery 类型列为契约并拒绝/报告超界配置；后者需要本项目实际读取 ScrapeConfig，和当前“只选择、不管控采集配置”的方向冲突。

因此最诚实的契约是：wrapper 创建一个固定、文档化的 runtime ClusterRole；用户选择的 ScrapeConfig 若需要额外权限，由集群管理员扩展授权，wrapper 通过状态暴露抓取失败但不改写用户采集配置。具体固定权限集合仍需 Wayfinder 决策。

## 6. Prometheus Operator 生成的资源与 ownerReferences

默认 `StatefulSet` 模式下，一个 `PrometheusAgent` 会触发：

- 每个 shard 一个 `StatefulSet`，命名为 `prometheus-agent-<name>`、`prometheus-agent-<name>-shard-1` 等；每个有 `replicas` 个 Pod；
- 配置 Secret `prometheus-agent-<name>`；
- 一个或多个 TLS assets Secret `prometheus-agent-<name>-tls-assets-<index>`；
- web config Secret `prometheus-agent-<name>-web-config`；
- 未指定 `serviceName` 时，共享 headless Service `prometheus-agent-operated`。

StatefulSet 数量与命名见 [`ExpectedStatefulSetShardNames`](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/prometheus/common.go#L70-L115)，Secret 名称见[命名函数](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/prometheus/common.go#L148-L165)，同步顺序见 [`PrometheusAgent` reconcile](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/prometheus/agent/operator.go#L588-L629)。

StatefulSet 和 Secrets 使用 `controller=true`、`blockOwnerDeletion=true` 的 PrometheusAgent ownerReference；默认 governing Service 使用非-controller ownerReference，因为同 namespace 的多个 PrometheusAgent 可能共享它。[owner helper](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/operator/factory.go#L33-L69)；[StatefulSet owner](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/prometheus/agent/statefulset.go#L60-L81)；[Service owner](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/prometheus/common.go#L464-L496)

对于允许同 namespace 多实例的 wrapper，建议自己创建唯一 headless Service，并把名称写入 `PrometheusAgent.spec.serviceName`。此时 Prometheus Operator 只校验 Service 已存在且 selector 能选中该实例 Pod，不接管该 Service。[`serviceName` 契约](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/apis/monitoring/v1/prometheus_types.go#L899-L907)；[校验逻辑](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/k8sutil/k8sutil.go#L584-L611)

## 7. 能力与运行状态检测

### 7.1 能可靠证明什么

建议按层检测，而不是只比较版本号：

1. **API discovery**：`monitoring.coreos.com/v1alpha1` 正在 served，存在 `prometheusagents` 与 `scrapeconfigs`；`monitoring.coreos.com/v1` 存在 `servicemonitors`、`podmonitors`、`probes`。
2. **CRD Established/schema**：CRD 为 `Established=True`，需要的字段路径存在且类型符合预期。v0.83.0 的 CRD 带 `operator.prometheus.io/version: 0.83.0` annotation，可作为安装资产版本证据。[PrometheusAgent CRD metadata](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/example/prometheus-operator-crd/monitoring.coreos.com_prometheusagents.yaml#L1-L18)
3. **server-side dry-run**：提交最小 PrometheusAgent，可验证当前 apiserver 的 schema、defaulting/admission 与调用者写权限。Kubernetes 1.19 的 server-side dry-run 会经过 admission、defaulting 和 schema validation，但不会持久化。[Kubernetes 1.19 dry-run 文档](https://github.com/kubernetes/website/blob/1c8fbbcc9cbd381070c8f73d10756544be68d0fa/content/en/docs/reference/using-api/api-concepts.md#L380-L407)
4. **真实 reconcile 证明**：创建实际 PrometheusAgent 后，等待其 `Reconciled=True` 且 condition 的 `observedGeneration` 等于当前 generation，并验证预期 StatefulSet 的 owner UID 指向该 CR。这样才能证明 controller 对该对象真实工作过。

### 7.2 不能可靠证明什么

- **CRD annotation 不是 running binary version**：CRD 和 Deployment 可被独立升级，annotation 只能说明 CRD 资产来源。
- **Deployment label/image 不是通用发现协议**：官方 bundle 有 `app.kubernetes.io/version` 与标准 image，但外部集群可改 namespace、name、label 或镜像 registry；Pod `imageID` 能证明实际镜像 digest，却不能在不知道 controller workload 位置时通用发现。
- **server-side dry-run 不执行 controller reconcile**：它不能证明 Operator 存活、RBAC 正确或能生成 StatefulSet。并且 Kubernetes 1.19 会按 structural schema 剪除未知 CR 字段，不能依赖“提交没报错”判断字段一定被保留；要读取 dry-run 返回对象或直接检查 CRD schema。[Kubernetes 1.19 CRD pruning](https://github.com/kubernetes/website/blob/1c8fbbcc9cbd381070c8f73d10756544be68d0fa/content/en/docs/tasks/extend-kubernetes/custom-resources/custom-resource-definitions.md#L312-L364)
- **PrometheusAgent status 不是 heartbeat**：`Reconciled` 记录最近一次 reconcile 结果，稳定对象不会周期性改变 generation。外部 Operator 在成功调谐后退出时，旧 status 和已有 workload 都可能继续存在。
- **CR selector 不能证明外部 watch scope**：即使四类 namespace selector 都是 `{}`，外部 Operator 的 `--namespaces` / `--deny-namespaces` 仍可能排除部分采集配置 namespace；若 `--prometheus-instance-namespaces` 排除实例 namespace，该 CR 甚至不会被调谐。

因此“最低版本 + 实际能力”可落实为“CRD capability gate + 每实例 reconcile proof”，但无法在不约定外部 Operator workload 定位方式的前提下，持续、即时证明 controller 存活。这是仍需决策的运维契约。

## 8. 生命周期建议

### 8.1 创建与正常运行

wrapper 先创建并确认唯一 headless Service、runtime ServiceAccount、ClusterRole/ClusterRoleBinding，再创建一个 PrometheusAgent。PrometheusAgent 应显式写入：

- `version: v2.55.1`；
- 固定 digest 的 `image`；
- `serviceAccountName`；
- `serviceName`；
- `replicas`、`shards`；
- 四类对象 selector 与 namespace selector；
- 至少一个 `remoteWrite`；
- 用户允许配置的 `resources`。

### 8.2 挂起

安全映射是保留 PrometheusAgent 和其非-workload 依赖，把渲染给下游的 `replicas` 设为 0，并等待所有 shard 的 `StatefulSet.spec.replicas=0`、Pod 数归零。不要设置 `paused=true`。

这会保留配置 Secrets、headless Service、ServiceAccount 和 RBAC；恢复时重新写回用户期望 replicas。由于 Agent 默认使用 `EmptyDir`，缩零会丢弃 Pod 本地尚未发送的 WAL，这是挂起语义必须明确接受的结果。

### 8.3 停用与删除 wrapper CR

停用应删除 PrometheusAgent，再删除 wrapper 自己管理的 ServiceAccount/RBAC/Service。删除 PrometheusAgent 后，其 StatefulSet/Secrets/默认 Service 依赖 ownerReferences 由 Kubernetes garbage collector 级联清理，Prometheus Operator 缺席也不妨碍 GC。[Operator 对删除的依赖说明](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/prometheus/agent/operator.go#L550-L559)

如果 wrapper CR 是 cluster-scoped，它可以合法成为 namespaced PrometheusAgent/ServiceAccount/Service 与 cluster-scoped ClusterRole/ClusterRoleBinding 的 owner；反过来，namespaced owner 不能拥有 cluster-scoped dependent。[Kubernetes 1.19 ownerReference scope 规则](https://github.com/kubernetes/website/blob/1c8fbbcc9cbd381070c8f73d10756544be68d0fa/content/en/docs/concepts/workloads/controllers/garbage-collection.md#L62-L66)

### 8.4 外部 Prometheus Operator 缺席

- **创建前缺席/能力不满足**：不创建 PrometheusAgent，保留 wrapper 声明与必要非-workload 资源，报告 `DependencyMissing`，写入量为 0。
- **创建后 controller 消失**：已有 Agent Pod 会继续运行和 remote write；仅凭 CRD/status 无法及时确认 controller 已消失。若产品承诺“DependencyMissing 必须零写入”，就必须额外约定一种可靠的外部 controller workload 定位/健康检测方式，并在确认缺席后删除 PrometheusAgent，让 GC 停止 Pod。否则只能承诺“不创建新的 Agent，已有 workload 可能继续运行”。
- **依赖恢复**：重新通过 capability gate 后重建 PrometheusAgent；不要修改或接管 Prometheus Operator 生成的 StatefulSet/Secrets。

这一项是当前事实边界中最重要的未决点：没有通用 API 能同时保证外部 controller liveness 检测与安装无关。

## 9. 一个 PrometheusAgent 是否足够

在一期已知需求下，一个 wrapper 采集实例对应一个 PrometheusAgent 足够：

- Prometheus Operator 会按 `shards` 自动创建多个 StatefulSet；
- 每个 StatefulSet 使用 `replicas`；
- 默认 sharding 对 ServiceMonitor、PodMonitor、ScrapeConfig 使用 `__address__`，对 Probe 使用 `__param_target__`，生成 hashmod/keep relabel 规则。[sharding 规则](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/prometheus/promcfg.go#L2106-L2144)

多个 PrometheusAgent 只在这些场景必要：需要不同的 selector、remote write targets、runtime RBAC/ServiceAccount、资源规格或故障域。不要为了实现同一实例的 shard 而创建多个 PrometheusAgent，否则会把目标分配、状态聚合和生命周期原子性重新实现一遍。

## 10. 仍需 Wayfinder 决策的问题

1. runtime ClusterRole 采用官方 Agent 最小基线，还是采用覆盖任意 Kubernetes SD 的较宽权限；若选择前者，如何向用户表达 ScrapeConfig 自行补充 RBAC 的责任。
2. 外部 Prometheus Operator 的持续 liveness 是否属于一期承诺；若属于，如何配置或发现其 Deployment/Pod，而不是只验证 CRD。
3. 产品是否要求预装 Prometheus Operator watch 全部 namespaces；如果允许受限 watch scope，如何配置或检测它对实例 namespace 与采集配置 namespaces 的覆盖。
4. wrapper remote write API 暴露全部 queue 参数，还是只暴露 `capacity`、`minShards`、`maxShards`、`maxSamplesPerSend`、`batchSendDeadline`、backoff 与 `sampleAgeLimit` 的稳定子集。
5. 是否接受挂起、滚动更新、缩分片会丢失 `EmptyDir` 中未发送 WAL；若不接受，就必须把 persistent storage 纳入一期并明确 PVC 清理策略。
6. 是否支持 `ScrapeConfig` 的任意认证 Secret 引用；Prometheus Operator 会读取这些 Secret，因此错误通常出现在下游 `Reconciled=False`，wrapper 只能转译状态，不能预先完整验证。
