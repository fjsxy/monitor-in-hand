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
3. `PrometheusAgent` 没有 `retention`、rules、alerting、query、remote read、Thanos 等 server-only 字段；但它继承了 `storage` 和 `walCompression`。Agent 会把待发送样本写入本地 WAL；一期既然要求 PVC，就必须同时定义 PVC 的保留、冲突、扩容和 namespace 删除语义，不能只把 `volumeClaimTemplate` 透传下去。
4. 应显式设置 `spec.version: v2.55.1` 和固定的 `spec.image`。v0.83.0 未指定版本时默认的是当时最新 Prometheus `v3.4.0`，而 `image` 与 `version` 是两个相互独立的输入，Operator 会信任 `version` 来生成配置。
5. 对跨 namespace 的四类采集配置，应给四个对象 selector 都写入同一稳定 label selector，并把四个 namespace selector 设为空对象 `{}`。`null` 对象 selector 表示不选择；四者全为 `null` 还会进入已废弃的 unmanaged configuration 模式。
6. runtime `ServiceAccount` 的固定最小 RBAC 可以覆盖 `ServiceMonitor`、`PodMonitor` 和 ingress/static `Probe`；但任意 `ScrapeConfig` 能声明不同的 Kubernetes SD role，所以不存在既支持所有 `ScrapeConfig` 又保持最小权限的静态 ClusterRole。这个边界必须由产品契约决定。
7. CRD annotation/schema 或 server-side dry-run 只能证明 API capability，不能证明当前 Prometheus Operator controller 正在运行。一次成功的、带当前 `observedGeneration` 的 `Reconciled=True` 加受管 `StatefulSet` 能证明该实例曾被实际调谐，但 status 没有 heartbeat，不能长期证明 controller 存活。
8. Kubernetes 1.19 没有 `StatefulSet.spec.persistentVolumeClaimRetentionPolicy`，所以不能把 1.28 的自动 PVC 删除能力当成跨版本契约。共同基线是 PVC 默认保留：`replicas=0`、replica/shard 缩容、StatefulSet 替换以及 `PrometheusAgent` 删除都不会删除 PVC；同名 StatefulSet 与 ordinal 会复用确定性命名的旧 PVC。

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
| `storage` | 支持 `EmptyDir`、ephemeral volume 或 PVC template | 一期使用受限的 PVC API 映射，不直接透传完整 `PersistentVolumeClaimSpec` |
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

未设置 `storage` 时，Operator 给每个 Pod 使用 `EmptyDir`。它能跨 container restart 保留 WAL，但 Pod 被删除、重建或迁移后数据消失。一期已经决定支持 PVC，因此 wrapper 需要生成 `volumeClaimTemplate`，并承担第 10 节所述的清理、冲突检测和状态聚合责任。

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

PVC 是例外：Kubernetes 1.19 的 StatefulSet controller 从 `volumeClaimTemplates` 复制 metadata/spec 并生成 PVC，不增加 StatefulSet 或 Pod ownerReference；v0.83.0 的嵌入式 PVC metadata 也只允许 name、labels、annotations，无法通过模板注入 ownerReference。[Kubernetes 1.19 PVC 构造](https://github.com/kubernetes/kubernetes/blob/e37e4ab4cc8dcda84f1344dda47a97bb1927d074/pkg/controller/statefulset/stateful_set_utils.go#L135-L154)；[Prometheus Operator 嵌入式 metadata](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/apis/monitoring/v1/types.go#L283-L325) 因此删除 PrometheusAgent 所触发的 ownerReference 级联不会覆盖 PVC。

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
- 用户允许配置的 `resources`；
- wrapper 生成的 `storage.volumeClaimTemplate`，其中带稳定的 claim template name、实例 UID label、`ReadWriteOnce`、`Filesystem`、容量和 `storageClassName`。

### 8.2 挂起

安全映射是保留 PrometheusAgent 和其非-workload 依赖，把渲染给下游的 `replicas` 设为 0，并等待所有 shard 的 `StatefulSet.spec.replicas=0`、Pod 数归零。不要设置 `paused=true`。

这会保留配置 Secrets、headless Service、ServiceAccount、RBAC 和 PVC；恢复时重新写回用户期望 replicas。相同 StatefulSet name/ordinal 会重新挂载同名 PVC，因此 WAL 文件可以延续，但这不构成端到端“零丢失”保证：Agent 自身 WAL 截断、remote endpoint 的时间窗口与去重/乱序策略仍在契约之外。

### 8.3 停用与删除 wrapper CR

停用应删除 PrometheusAgent，再删除 wrapper 自己管理的 ServiceAccount/RBAC/Service。删除 PrometheusAgent 后，其 StatefulSet/Secrets/默认 Service 依赖 ownerReferences 由 Kubernetes garbage collector 级联清理，Prometheus Operator 缺席也不妨碍 GC。[Operator 对删除的依赖说明](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/prometheus/agent/operator.go#L550-L559)

PVC 不在上述级联链中。wrapper 必须用 finalizer 明确实现所选策略：`Delete` 时先确保 Pods/StatefulSets 已消失，再只删除带当前实例 UID label 且名称属于预期 identity 集合的 PVC，等待删除完成后释放 finalizer；`Retain` 时保留 PVC。停用后再启用且 wrapper UID 未变时可安全复用自己的 retained PVC；wrapper 删除后若同名新对象 UID 不同，必须报告 `ResourceConflict`，不能静默采用旧 PVC。

如果 wrapper CR 是 cluster-scoped，它可以合法成为 namespaced PrometheusAgent/ServiceAccount/Service 与 cluster-scoped ClusterRole/ClusterRoleBinding 的 owner；反过来，namespaced owner 不能拥有 cluster-scoped dependent。[Kubernetes 1.19 ownerReference scope 规则](https://github.com/kubernetes/website/blob/1c8fbbcc9cbd381070c8f73d10756544be68d0fa/content/en/docs/concepts/workloads/controllers/garbage-collection.md#L62-L66)

不建议 wrapper 在 PVC 创建后补自己的 ownerReference。显式 finalizer + UID label 可以分别表达 `Delete`/`Retain`；ownerReference 会把 wrapper 删除固定成自动 GC，破坏 `Retain` 语义，也可能与 Kubernetes 1.28 的 StatefulSet PVC retention ownerReference 发生竞争。

### 8.4 外部 Prometheus Operator 缺席

- **创建前缺席/能力不满足**：不创建 PrometheusAgent，保留 wrapper 声明与必要非-workload 资源，报告 `DependencyMissing`，写入量为 0。
- **创建后 controller 消失**：已有 Agent Pod 会继续运行和 remote write；仅凭 CRD/status 无法及时确认 controller 已消失。若产品承诺“DependencyMissing 必须零写入”，就必须额外约定一种可靠的外部 controller workload 定位/健康检测方式，并在确认缺席后删除 PrometheusAgent，让 GC 停止 Pod。否则只能承诺“不创建新的 Agent，已有 workload 可能继续运行”。
- **依赖恢复**：重新通过 capability gate 后重建 PrometheusAgent；不要修改或接管 Prometheus Operator 生成的 StatefulSet/Secrets。只要 PVC 的实例 UID label 与预期一致，重建后的同名 StatefulSet 会复用它们。

这一项是当前事实边界中最重要的未决点：没有通用 API 能同时保证外部 controller liveness 检测与安装无关。

## 9. 一个 PrometheusAgent 是否足够

在一期已知需求下，一个 wrapper 采集实例对应一个 PrometheusAgent 足够：

- Prometheus Operator 会按 `shards` 自动创建多个 StatefulSet；
- 每个 StatefulSet 使用 `replicas`；
- persistent storage 下每个 shard/replica identity 由 StatefulSet 自动获得一个独立 PVC，当前运行所需 PVC 数是 `shards × replicas`；历史缩容后 retained PVC 数可以更大；
- 默认 sharding 对 ServiceMonitor、PodMonitor、ScrapeConfig 使用 `__address__`，对 Probe 使用 `__param_target__`，生成 hashmod/keep relabel 规则。[sharding 规则](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/prometheus/promcfg.go#L2106-L2144)

多个 PrometheusAgent 只在这些场景必要：需要不同的 selector、remote write targets、runtime RBAC/ServiceAccount、资源规格或故障域。不要为了实现同一实例的 shard 而创建多个 PrometheusAgent，否则会把目标分配、状态聚合和生命周期原子性重新实现一遍。

## 10. PVC 生命周期的跨版本事实

### 10.1 上游支持的字段与实际渲染

v0.83.0 的 `StorageSpec` 有三种互斥来源，优先级是 `emptyDir`、`ephemeral`、`volumeClaimTemplate`。`volumeClaimTemplate` 使用精简 metadata（name、labels、annotations）和完整的 Kubernetes `PersistentVolumeClaimSpec`，因此技术上能携带 access modes、resources、selector、`storageClassName`、`volumeMode`、`dataSource` 等字段。[`StorageSpec`](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/apis/monitoring/v1/prometheus_types.go#L1310-L1335)；[`EmbeddedPersistentVolumeClaim`](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/apis/monitoring/v1/types.go#L283-L325)

Prometheus Operator 把同一个 template 放进每个 shard 的 StatefulSet。template 未命名时默认使用 `prometheus-agent-<agent-name>-db`，access modes 未设置时默认 `ReadWriteOnce`；其余 PVC spec 由转换函数复制。[Agent StatefulSet storage 渲染](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/prometheus/agent/statefulset.go#L86-L130)；[template 转换函数](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/operator/types.go#L24-L38)；[默认 volume name](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/prometheus/common.go#L160-L165)

一期不应透传完整 `PersistentVolumeClaimSpec`。跨 1.19/1.28 的稳定子集建议只有：

- `size`：必填；第一期若不实现受控扩容，则创建后 immutable；
- `storageClassName`：可选或必填仍需决策；
- 内部固定 `accessModes: [ReadWriteOnce]`、`volumeMode: Filesystem`；
- template name、instance UID label 由 wrapper 生成且对用户不可配置；
- 不暴露 selector、`volumeName`、`dataSource`、generic ephemeral、`disableMountSubPath` 和原生 retention policy。

原因不是这些字段“上游不支持”，而是它们会把 PV 预绑定、快照/数据源版本差异、feature gate、不可变字段和清理责任泄露进稳定 API。

### 10.2 `persistentVolumeClaimRetentionPolicy` 不能作为共同基线

Prometheus Operator 的公共字段包含 `persistentVolumeClaimRetentionPolicy`，并在非空时原样写入 StatefulSet。[字段定义](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/apis/monitoring/v1/prometheus_types.go#L390-L396)；[渲染逻辑](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/prometheus/agent/statefulset.go#L127-L131) 但版本能力不同：

- Kubernetes 1.19 的 `StatefulSetSpec` 只有 `volumeClaimTemplates`，没有该字段。[1.19 API](https://github.com/kubernetes/kubernetes/blob/e37e4ab4cc8dcda84f1344dda47a97bb1927d074/staging/src/k8s.io/api/apps/v1/types.go#L120-L175)
- Kubernetes 1.28 有 `whenDeleted`/`whenScaled`，默认均为 `Retain`。[1.28 API 类型](https://github.com/kubernetes/kubernetes/blob/841856557ef0f6a399096c42635d114d6f2cf7f4/staging/src/k8s.io/api/apps/v1/types.go#L135-L165) 该 feature 在 v1.22 为 alpha、v1.27 为 beta，1.28 默认启用。[feature 历史](https://github.com/kubernetes/kubernetes/blob/841856557ef0f6a399096c42635d114d6f2cf7f4/pkg/features/kube_features.go#L817-L822)；[1.28 默认值](https://github.com/kubernetes/kubernetes/blob/841856557ef0f6a399096c42635d114d6f2cf7f4/pkg/features/kube_features.go#L1149-L1155)

所以不能依赖这个字段在两个目标版本产生相同行为。即使 CRD 接受它，1.19 的 StatefulSet API 也没有相应能力。跨版本契约应保持原生字段为空，由 wrapper 显式执行 PVC retention。

1.28 也只有在策略要求 `Delete` 时才通过 StatefulSet/Pod ownerReferences 驱动 GC；默认 `Retain/Retain` 明确不设置这些 ownerReferences。[1.28 ownerReference 策略矩阵](https://github.com/kubernetes/kubernetes/blob/841856557ef0f6a399096c42635d114d6f2cf7f4/pkg/controller/statefulset/stateful_set_utils.go#L238-L249)；[ownerReference 写入逻辑](https://github.com/kubernetes/kubernetes/blob/841856557ef0f6a399096c42635d114d6f2cf7f4/pkg/controller/statefulset/stateful_set_utils.go#L307-L350)

### 10.3 各生命周期动作的精确结果

StatefulSet PVC 名称是 `<claim-template-name>-<statefulset-name>-<ordinal>`。controller 发现同名 PVC 已存在时只检查它是否正在删除，不验证或更新 resources/access modes；源码中保留了对应 TODO。[1.19 命名与构造](https://github.com/kubernetes/kubernetes/blob/e37e4ab4cc8dcda84f1344dda47a97bb1927d074/pkg/controller/statefulset/stateful_set_utils.go#L91-L96)；[1.19 已存在 PVC 逻辑](https://github.com/kubernetes/kubernetes/blob/e37e4ab4cc8dcda84f1344dda47a97bb1927d074/pkg/controller/statefulset/stateful_pod_control.go#L177-L204)；[1.28 同样的逻辑](https://github.com/kubernetes/kubernetes/blob/841856557ef0f6a399096c42635d114d6f2cf7f4/pkg/controller/statefulset/stateful_pod_control.go#L337-L364)

在不设置原生 retention policy 的共同基线下：

| 动作 | StatefulSet/Pod | PVC | 再次扩容/重建 |
| --- | --- | --- | --- |
| `replicas: 0` | 每个 shard StatefulSet 保留，Pods 归零 | 全部保留 | 相同 ordinal 复用原 PVC |
| replicas 从 N 降到 M | 高 ordinal Pods 删除 | 高 ordinal PVC 保留 | 回到 N 时复用 |
| shards 从 N 降到 M | Prometheus Operator 删除多余 shard StatefulSets | 这些 shard 的 PVC 保留 | 回到 N 时同名 shard StatefulSet 复用 |
| shards/replicas 首次增加到新 identity | 创建新 StatefulSet/Pod | 不存在同名 claim 时按 template 新建 | 后续按 identity 复用 |
| StatefulSet immutable update | Prometheus Operator 收到 422 后 foreground 删除并在后续 reconcile 重建 | 保留 | 同名 StatefulSet/ordinal 复用 |
| 删除 PrometheusAgent | ownerReferences 级联删除 StatefulSets/Pods/Secrets | 保留 | 同 namespace 同名 Agent 会撞上并复用旧 PVC |
| 删除 target namespace | namespace controller 删除其中所有可删除的 namespaced resources，包括 PVC | PVC 对象被删除 | 后端数据是否仍在取决于 PV reclaim policy，不能自动复用 |

Prometheus Operator 缩 shards 时只删除 StatefulSet；遇到不可变更新时也只删除 StatefulSet，没有显式 PVC 删除路径。[shard 清理](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/prometheus/agent/operator.go#L808-L831)；[422 替换路径](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/prometheus/agent/operator.go#L782-L805) Kubernetes 1.19 又禁止直接更新 `volumeClaimTemplates` 等 StatefulSet 字段，因此改变 template 会触发该替换路径。[1.19 StatefulSet update validation](https://github.com/kubernetes/kubernetes/blob/e37e4ab4cc8dcda84f1344dda47a97bb1927d074/pkg/apis/apps/validation/validation.go#L143-L158)

namespace controller 会发现所有支持 delete 的 namespaced GVR 并逐类删除资源，因此 wrapper finalizer 无法把 target namespace 删除转换成普通的“保留 PVC”语义。[1.19 namespace 删除实现](https://github.com/kubernetes/kubernetes/blob/e37e4ab4cc8dcda84f1344dda47a97bb1927d074/pkg/controller/namespace/deletion/namespaced_resources_deleter.go#L491-L555) target namespace 已进入 `Terminating` 或消失时，wrapper 应停止重建 namespaced resources，报告 `TargetNamespaceUnavailable`；不能宣称数据已保留或已擦除。

### 10.4 PVC 删除不等于后端数据删除

动态 provision 的 PV 继承 StorageClass 的 `reclaimPolicy`，未设置时默认 `Delete`；`allowVolumeExpansion` 是另一个独立字段。[Kubernetes 1.19 StorageClass API](https://github.com/kubernetes/kubernetes/blob/e37e4ab4cc8dcda84f1344dda47a97bb1927d074/staging/src/k8s.io/api/storage/v1/types.go#L40-L67) PVC 删除后：

- PV `Delete` 表示 Kubernetes 删除 PV，并请求对应 storage backend 删除资产；
- PV `Retain` 表示 PV 进入 Released，等待管理员手工回收。

两者的 API 定义见 [Kubernetes 1.19 reclaim policy 类型](https://github.com/kubernetes/kubernetes/blob/e37e4ab4cc8dcda84f1344dda47a97bb1927d074/staging/src/k8s.io/api/core/v1/types.go#L375-L387)，外部 storage asset 的处理见[同版本官方文档](https://github.com/kubernetes/website/blob/1c8fbbcc9cbd381070c8f73d10756544be68d0fa/content/en/docs/concepts/storage/persistent-volumes.md#L124-L138)。因此 wrapper 的 `Delete` retention 最多承诺“PVC 对象已删除并触发底层 reclaim 流程”，不能承诺云盘/数据已物理擦除；后者属于 StorageClass、CSI provisioner 和基础设施的责任边界。

### 10.5 不可变字段与扩容

在 Kubernetes 1.19 的共同基线中，bound PVC 的 spec 除 `resources.requests.storage` 外不可变，`volumeMode` 也单独强制 immutable，容量不能缩小。[PVC update validation](https://github.com/kubernetes/kubernetes/blob/e37e4ab4cc8dcda84f1344dda47a97bb1927d074/pkg/apis/core/validation/validation.go#L2010-L2060) 增大容量还要求 PVC 已 Bound、StorageClass 一致且 `allowVolumeExpansion=true`。[resize admission](https://github.com/kubernetes/kubernetes/blob/e37e4ab4cc8dcda84f1344dda47a97bb1927d074/plugin/pkg/admission/storage/persistentvolume/resize/admission.go#L75-L130)

只修改 PrometheusAgent 的 template 不会扩已有 PVC：Prometheus Operator 会替换 StatefulSet，而 StatefulSet controller 会复用同名 PVC且不更新其 spec。结果可能是旧 identities 仍用旧容量，新 identity 按新 template 创建，形成混合容量。

一期有两个可行方案：

1. **建议：`size` 创建后 immutable。** API 最小、跨版本结果确定；用户要换容量时创建新实例并接受 WAL 重新开始。
2. **grow-only 工作流。** wrapper 必须先逐个 patch 当前及 retained PVC，等待 `status.capacity` 达标并处理 `Resizing`/`FileSystemResizePending`，再更新 PrometheusAgent template 供未来 identity 使用；任何 PVC 或 StorageClass 不支持时保持旧 desired state 并报告失败。不能仅修改 template，也不能支持缩容。

Kubernetes 1.19 可跨版本依赖的 PVC status 只有 phase、capacity 和 resize conditions；`Resizing` 与 `FileSystemResizePending` 的定义见 [PVC status API](https://github.com/kubernetes/kubernetes/blob/e37e4ab4cc8dcda84f1344dda47a97bb1927d074/staging/src/k8s.io/api/core/v1/types.go#L507-L555)。具体 CSI 是否支持 online filesystem resize 仍是外部能力，不能由 API schema 推断。

### 10.6 数据连续性、冲突与安全清理

PVC 能保留同一个 Pod identity 的 WAL 文件，但不是业务级 exactly-once 或零丢失保证。Agent 运行期间仍受第 2.2 节的 WAL truncate 参数约束；恢复后外部 remote endpoint 仍可能按自己的过期、重复或 out-of-order 规则接受/拒绝样本。

必须在 template labels 中写入不可变的 wrapper instance UID。原因是删除并创建同 namespace、同名 wrapper/PrometheusAgent 会生成完全相同的 StatefulSet 和 PVC 名；StatefulSet controller 会直接复用同名 PVC，不验证它是否属于新实例。安全判定如下：

- PVC label UID 等于当前 wrapper UID：可视为本实例的 retained PVC；
- 同名 PVC 无 UID label 或 UID 不同：报告 `ResourceConflict`，停止创建/恢复，不自动 adopt、不覆盖、不删除；
- template name、target namespace 或其他决定 PVC 名称的字段创建后 immutable。

`suspended=true` 永远保留 PVC。对于 component disabled 和 wrapper deletion，建议暴露或内部固定一个 retention policy：

- `Delete`：wrapper finalizer 删除 PrometheusAgent，等待 StatefulSets/Pods 消失，再删除当前 UID 标记的 PVC，最后删除其余资源并释放 finalizer；
- `Retain`：不删 PVC。相同 wrapper UID 的 disable→enable 可以复用；wrapper 本身删除后，PVC 成为明确的管理员资产，未来同名新实例因 UID 不同必须冲突退出。

wrapper 因此至少需要对目标 namespace 中 PVC 的 `get/list/watch/delete`；若实现 grow-only 再增加 `patch/update`。PVC 由 StatefulSet controller 创建，wrapper 不需要 `create`。还应读取 StatefulSet、Pod 和 Event 来完成删除等待与诊断。不要通过事后给 PVC 增加 wrapper ownerReference 代替 finalizer，因为那会丢失 `Retain` 选择并产生跨版本不同的 GC 行为。

### 10.7 status 与 Events

PVC 创建 API 失败时，StatefulSet controller 在 StatefulSet 上记录 `FailedCreate`；绑定/动态 provision 过程在 PVC 上记录 `FailedBinding`、`VolumeMismatch`、`ExternalProvisioning`、`ProvisioningFailed` 等 Event。[StatefulSet claim Event](https://github.com/kubernetes/kubernetes/blob/e37e4ab4cc8dcda84f1344dda47a97bb1927d074/pkg/controller/statefulset/stateful_pod_control.go#L160-L204)；[volume Event reasons](https://github.com/kubernetes/kubernetes/blob/e37e4ab4cc8dcda84f1344dda47a97bb1927d074/pkg/controller/volume/events/event.go#L19-L35)；[未绑定与 provision 事件](https://github.com/kubernetes/kubernetes/blob/e37e4ab4cc8dcda84f1344dda47a97bb1927d074/pkg/controller/volume/persistentvolume/pv_controller.go#L345-L364)

异步 binding 失败不一定让 PrometheusAgent `Reconciled=False`：StatefulSet 已创建时 reconcile 可以成功，而 Pod 因未绑定 PVC 无法调度。Prometheus Operator 的 `Available` 只汇总 ready Pods，并把 Pod condition message 带入 `NoPodReady`/`SomePodsNotReady`。[Prometheus status 汇总](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/prometheus/operator.go#L138-L245)；[Pod message 选择](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/operator/statefulset_reporter.go#L35-L67) Kubernetes 1.19 scheduler 的典型 message 是 `pod has unbound immediate PersistentVolumeClaims`。[scheduler volume binding](https://github.com/kubernetes/kubernetes/blob/e37e4ab4cc8dcda84f1344dda47a97bb1927d074/pkg/scheduler/framework/plugins/volumebinding/volume_binding.go#L133-L145)

wrapper 不能只转译 `PrometheusAgent.status`，应独立维护：

- `StorageReady=True`：所有当前期望 identity 的 PVC 均为 Bound，且容量符合 desired；
- `StorageReady=False, reason=PVCNotBound|PVCProvisioningFailed|PVCResizePending|PVCConflict`：message 写入 namespace/name、当前 phase 与最近可行动原因；
- `Ready` 必须同时要求 `Reconciled=True`、`Available=True` 和 `StorageReady=True`；suspended 时另用 `Suspended=True` 表达期望状态，不把零副本当成正常采集；
- 发出 wrapper 自己的 Kubernetes Event，但不能把 Event 当持久状态；Event 会过期，当前阻塞原因必须留在 CR status。

## 11. 仍需 Wayfinder 决策的问题

1. runtime ClusterRole 采用官方 Agent 最小基线，还是采用覆盖任意 Kubernetes SD 的较宽权限；若选择前者，如何向用户表达 ScrapeConfig 自行补充 RBAC 的责任。
2. 外部 Prometheus Operator 的持续 liveness 是否属于一期承诺；若属于，如何配置或发现其 Deployment/Pod，而不是只验证 CRD。
3. 产品是否要求预装 Prometheus Operator watch 全部 namespaces；如果允许受限 watch scope，如何配置或检测它对实例 namespace 与采集配置 namespaces 的覆盖。
4. wrapper remote write API 暴露全部 queue 参数，还是只暴露 `capacity`、`minShards`、`maxShards`、`maxSamplesPerSend`、`batchSendDeadline`、backoff 与 `sampleAgeLimit` 的稳定子集。
5. PVC retention 是只提供一个固定策略，还是暴露 `Delete`/`Retain`；若暴露，默认值应是什么，以及 `Retain` 后的人工恢复/清理入口由谁负责。
6. PVC 容量在一期保持 immutable，还是实现显式的 grow-only 工作流；后者必须逐个 patch 已存在 PVC、等待扩容状态，并处理 StorageClass/CSI 不支持在线扩容的情况。
7. `storageClassName` 是否强制显式设置；若允许省略，应在 status 记录 admission/defaulting 后每个 PVC 的实际 StorageClass，避免默认 StorageClass 变化造成不同 identity 使用不同后端。
8. 是否支持 `ScrapeConfig` 的任意认证 Secret 引用；Prometheus Operator 会读取这些 Secret，因此错误通常出现在下游 `Reconciled=False`，wrapper 只能转译状态，不能预先完整验证。
