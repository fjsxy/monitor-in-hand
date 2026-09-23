# 一期版本与 Prometheus Agent 能力边界

## 结论

一期可以在两个目标 Kubernetes 版本上采用同一套 `PrometheusAgent` 合约，但必须把下面三项写成硬前置条件：

1. 预装 Prometheus Operator 至少为 `v0.75.0`，并且安装、服务了 `PrometheusAgent`、`ScrapeConfig`、`ServiceMonitor`、`PodMonitor`、`Probe` 五类 CRD；Operator RBAC 必须包含这些资源。`v0.65.2` 只是字段能力下限，`v0.75.0` 才修复跨 namespace 选择 `ScrapeConfig` 的已知问题；不是“仅看 Operator Pod 正常运行”即可判定能力。
2. `PrometheusAgent` 使用的 Prometheus 二进制至少为 `v2.32.0`。Prometheus 官方在该版本引入 Agent mode；它只保留 discovery、scrape、WAL 与 remote write，不支持本地查询、recording rules 或 alerting。[Prometheus Agent 设计说明](https://prometheus.io/blog/2021/11/16/agent/)、[Prometheus Operator Agent 设计](https://github.com/prometheus-operator/prometheus-operator/blob/v0.65.2/Documentation/designs/prometheus-agent.md#L1-L15)
3. `Kubernetes 1.19 + kube-state-metrics 2.3.0` 不能宣称为上游完整支持组合。`2.3.0` 实际编译于 `client-go v0.23.0`，而 kube-state-metrics 明确把 client-go 之外的兼容性定义为 best effort；一期只能通过本项目双版本验收，把它定义成“本项目验证支持”。`Kubernetes 1.28 + kube-state-metrics 2.11.0` 则与上游声明的 `client-go v1.28` 对齐。[v2.3.0 go.mod](https://github.com/kubernetes/kube-state-metrics/blob/v2.3.0/go.mod#L19-L22)、[v2.3.0 版本政策](https://github.com/kubernetes/kube-state-metrics/blob/v2.3.0/README.md#L57-L80)、[v2.11.0 兼容矩阵](https://github.com/kubernetes/kube-state-metrics/blob/v2.11.0/README.md#L62-L82)

`PrometheusAgent` 与 `ScrapeConfig` 在这个可靠性基线中仍为 `monitoring.coreos.com/v1alpha1`。Prometheus Operator `v0.64.0` 的发布说明还明确表示当时不建议将 `PrometheusAgent` 用于生产；因此这是一期必须显式接受和测试的外部 API 风险，不能包装成稳定能力。[Prometheus Operator v0.64.0 release note](https://github.com/prometheus-operator/prometheus-operator/blob/v0.65.2/CHANGELOG.md#L39-L52)

## 1. kube-state-metrics

### 1.1 版本事实

| 目标组合 | 官方事实 | 一期结论 |
| --- | --- | --- |
| Kubernetes `1.19` + kube-state-metrics `2.3.0` | `2.3.0` 依赖 `k8s.io/client-go v0.23.0`。该 tag 的 README 只明确列到 `2.2.4`，没有给出 `2.3.0 + 1.19` 的完整支持声明；额外兼容仅为 best effort。 | 可以进入一期，但必须由集成测试建立项目自己的兼容保证；不能仅凭上游文档标记为“可靠支持”。 |
| Kubernetes `1.28` + kube-state-metrics `2.11.0` | `2.11.0` 依赖 `client-go v0.28.4`，README 明确映射到 Kubernetes/client-go `1.28`。[go.mod](https://github.com/kubernetes/kube-state-metrics/blob/v2.11.0/go.mod#L20-L22) | 版本对齐成立；仍需对本项目启用的 resource/metric 集合做验收，不能把版本对齐等同于每个 collector 都经过本项目测试。 |

client-go 的兼容说明只保证共有 API 通常可以工作，并提醒跨版本存在客户端拥有而服务端不存在、或反之的 API 差异；它不能替代指定组合的测试。[client-go v0.23.0 compatibility](https://github.com/kubernetes/client-go/blob/v0.23.0/README.md#L67-L100)

### 1.2 部署 API

两个版本的官方 standard manifests 使用同一组稳定 API：

- `Deployment`: `apps/v1`；
- `Service`、`ServiceAccount`: `v1`；
- `ClusterRole`、`ClusterRoleBinding`: `rbac.authorization.k8s.io/v1`。

它们可作为一期生成资源的 API 基线，不需要为 Kubernetes `1.19` 与 `1.28` 分叉对象 `apiVersion`。证据见 [v2.3.0 Deployment](https://github.com/kubernetes/kube-state-metrics/blob/v2.3.0/examples/standard/deployment.yaml#L1-L24)、[v2.11.0 Deployment](https://github.com/kubernetes/kube-state-metrics/blob/v2.11.0/examples/standard/deployment.yaml#L1-L25)、[v2.3.0 ClusterRole](https://github.com/kubernetes/kube-state-metrics/blob/v2.3.0/examples/standard/cluster-role.yaml)、[v2.11.0 ClusterRole](https://github.com/kubernetes/kube-state-metrics/blob/v2.11.0/examples/standard/cluster-role.yaml)。

版本适配的重点因此不是 Kubernetes workload API，而是镜像、可用 collector、RBAC resource 列表、启动参数和最终指标集合。

### 1.3 可稳定抽象的过滤能力

两版共同拥有以下 CLI 能力，可以映射为 Operator 的类型化字段：[v2.3.0 CLI](https://github.com/kubernetes/kube-state-metrics/blob/v2.3.0/docs/cli-arguments.md#L40-L60)、[v2.11.0 CLI](https://github.com/kubernetes/kube-state-metrics/blob/v2.11.0/docs/cli-arguments.md#L57-L78)

| 领域能力 | 两版参数 | 边界 |
| --- | --- | --- |
| resource 允许集合 | `--resources` | 只有 allowlist 语义；值是逗号分隔的复数 resource 名。Operator 应按目标版本校验可用 resource，不透传任意字符串。 |
| metric 允许集合 | `--metric-allowlist` | 支持精确名称和 regex；与 denylist 互斥。 |
| metric 拒绝集合 | `--metric-denylist` | 支持精确名称和 regex；与 allowlist 互斥。 |
| opt-in metric | `--metric-opt-in-list` | 在 allow/deny 之外显式开启默认关闭的指标；不同版本的实际 opt-in 指标集合可能不同。 |
| namespace 允许集合 | `--namespaces` | 空值在两版实现中都会转成 `NamespaceAll`；一期 API 仍应明确表达“所有 namespace”，不要依赖空字符串产生业务语义。 |
| namespace 拒绝集合 | `--namespaces-denylist` | 两版都把每个拒绝项转成 `metadata.namespace!=<name>` 的 field selector；与允许集合同时出现时，效果是从允许范围中排除 denylist。 |
| 对象 label/annotation 暴露 | `--metric-labels-allowlist`、`--metric-annotations-allowlist` | 通配符会显著增加基数与资源消耗。若一期纳入，应单独限制通配符；不应混入“选择 metric family”的字段。 |

namespace 的实现证据见 [v2.3.0 NamespaceList](https://github.com/kubernetes/kube-state-metrics/blob/v2.3.0/pkg/options/types.go#L107-L160) 与 [v2.11.0 NamespaceList](https://github.com/kubernetes/kube-state-metrics/blob/v2.11.0/pkg/options/types.go#L198-L251)。两版参数相同不代表参数接受的每个 resource 或 metric 名称相同。类型化 API 应表达用户意图，版本适配层负责把共同字段翻译成目标版本参数，并对该版本未知的名称拒绝调谐。

### 1.4 replicas 不是 sharding

两版官方 Deployment 都默认 `replicas: 1`。kube-state-metrics 的分片由 `--shard` 与 `--total-shards` 明确控制，`--total-shards=1` 表示未分片；自动分片还依赖 `--pod`、`--pod-namespace` 与 StatefulSet 身份，并被标记为 experimental。[v2.3.0 Deployment](https://github.com/kubernetes/kube-state-metrics/blob/v2.3.0/examples/standard/deployment.yaml#L10-L23)、[v2.11.0 Deployment](https://github.com/kubernetes/kube-state-metrics/blob/v2.11.0/examples/standard/deployment.yaml#L10-L24)、[v2.11.0 sharding](https://github.com/kubernetes/kube-state-metrics/blob/v2.11.0/README.md#L229-L248)

因此，一期若只暴露 `replicas` 而不暴露 sharding：

- `replicas > 1` 是相同采集范围的冗余副本，不会自动分摊资源；
- 通过同一个 Service 抓取全部副本会产生重复的对象状态指标流；
- 它不能作为容量扩展承诺，容量扩展需要后续单独设计 sharding；
- 验收必须验证副本数和重复采集语义，避免把“Pod 数增加”误判为“单副本压力下降”。

这是由默认 shard 参数与官方分片模型推导出的产品边界，不是上游对普通 Deployment replicas 的 HA 保证。

## 2. Prometheus Operator 与 PrometheusAgent

### 2.1 最低版本

能力的引入顺序如下：

- Prometheus Operator `v0.64.0` 首次提供 `PrometheusAgent` CRD；
- `v0.65.0` 首次提供 `ScrapeConfig` CRD；
- `v0.65.1` 修复缺少 `ScrapeConfig` CRD 时的 panic；
- `v0.65.2` 修复 `ScrapeConfig` relabeling 问题；
- `v0.75.0` 修复跨 namespace 选择 `ScrapeConfig` 的问题，以及 PrometheusAgent 引用其他 namespace Secret/ConfigMap 时的调谐问题。

官方 ScrapeConfig 文档把功能前置条件写为 Prometheus Operator `>v0.65.1`，所以 `>=v0.65.2` 是字段能力下限；但一期明确要求跨 namespace selector，`v0.75.0` 的 changelog 证明此前存在跨 namespace 选择问题。因此一期“可靠支持”下限必须提高到 `>=v0.75.0`。[v0.65.x changelog](https://github.com/prometheus-operator/prometheus-operator/blob/v0.65.2/CHANGELOG.md#L1-L31)、[ScrapeConfig prerequisites](https://prometheus-operator.dev/docs/developer/scrapeconfig/#prerequisites)、[v0.75.0 fixes](https://github.com/prometheus-operator/prometheus-operator/blob/v0.75.0/CHANGELOG.md#L54-L62)

Prometheus Operator 当前兼容性文档说明：`v0.84.0` 之前的版本要求 Kubernetes `>=1.16.0`，而 `v0.84.0` 起因 CRD 使用 CEL，默认要求 Kubernetes `>=1.25.0`。因此在 Kubernetes `1.19` 上可声明的 Operator 版本窗口是 `>=0.75.0,<0.84.0`；Kubernetes `1.28` 无这个上界，但两边若要保持同一 CRD 合约，仍应选择经过共同验收的版本。[Prometheus Operator Kubernetes compatibility](https://prometheus-operator.dev/docs/getting-started/compatibility/#kubernetes)

最低版本只是必要条件。`v0.64.0` 与 `v0.65.0` 的发布说明都要求额外安装 CRD 并更新 Operator RBAC；缺少任一项时 Operator 进程可以继续运行，却不会调谐相应资源。[PrometheusAgent release note](https://github.com/prometheus-operator/prometheus-operator/blob/v0.65.2/CHANGELOG.md#L39-L52)、[ScrapeConfig release note](https://github.com/prometheus-operator/prometheus-operator/blob/v0.65.2/CHANGELOG.md#L9-L31)

### 2.2 CRD 与字段能力

可靠性基线 `v0.75.0` 中：

| CR | API | 一期所需能力 |
| --- | --- | --- |
| `PrometheusAgent` | `monitoring.coreos.com/v1alpha1` | 选择四类采集配置，`replicas`、`shards`、多个 `remoteWrite` 目标、资源和存储等运行参数。[CRD](https://github.com/prometheus-operator/prometheus-operator/blob/v0.75.0/example/prometheus-operator-crd/monitoring.coreos.com_prometheusagents.yaml#L18-L52) |
| `ScrapeConfig` | `monitoring.coreos.com/v1alpha1` | 由 label 与 namespace selector 选入 Agent 配置。[CRD](https://github.com/prometheus-operator/prometheus-operator/blob/v0.75.0/example/prometheus-operator-crd/monitoring.coreos.com_scrapeconfigs.yaml#L1-L28) |
| `ServiceMonitor` | `monitoring.coreos.com/v1` | 支持 label 与 namespace selector。 |
| `PodMonitor` | `monitoring.coreos.com/v1` | 支持 label 与 namespace selector。 |
| `Probe` | `monitoring.coreos.com/v1` | 支持 label 与 namespace selector。 |

`PrometheusAgentSpec` 内嵌 `CommonPrometheusFields`；在 `v0.75.0` 源码中，四组 resource/namespace selector、`replicas`、`shards` 与 `remoteWrite []RemoteWriteSpec` 已同时存在。[PrometheusAgentSpec](https://github.com/prometheus-operator/prometheus-operator/blob/v0.75.0/pkg/apis/monitoring/v1alpha1/prometheusagent_types.go#L90-L104)、[CommonPrometheusFields selectors](https://github.com/prometheus-operator/prometheus-operator/blob/v0.75.0/pkg/apis/monitoring/v1/prometheus_types.go#L94-L184)、[replicas、shards 与 remoteWrite](https://github.com/prometheus-operator/prometheus-operator/blob/v0.75.0/pkg/apis/monitoring/v1/prometheus_types.go#L220-L240)

### 2.3 selector 语义

四类采集配置都能用 Kubernetes `LabelSelector` 选对象，并分别用 namespace selector 选择这些对象所在的 namespace。基线源码说明，namespace selector 为 `null` 时只查找 PrometheusAgent 自身 namespace；空 selector 按 Kubernetes `LabelSelector` 语义匹配所有 namespace。[v0.75.0 selector fields](https://github.com/prometheus-operator/prometheus-operator/blob/v0.75.0/pkg/apis/monitoring/v1/prometheus_types.go#L94-L184)、[Kubernetes LabelSelector](https://kubernetes.io/docs/reference/kubernetes-api/definitions/label-selector-v1-meta/)

必须防止两个容易误配的状态：

- object selector `{}` 匹配作用域内所有对象；它不是“未选择”；
- 四个 object selector 全为 `null` 时，Prometheus Operator 进入 unmanaged configuration 语义，要求用户自行提供原始 `prometheus.yaml.gz`；这与一期“只选择带特殊 label 的采集配置”冲突。

所以一期生成的 PrometheusAgent CR 必须始终写入明确、非空的 label 条件。跨 namespace 选择只是发现范围，实际抓取还要求 PrometheusAgent 使用的 ServiceAccount 对 `services`、`endpoints`/`endpointslices`、`pods`、`ingresses` 等目标发现资源具有 `get/list/watch` 权限。Prometheus Operator 官方把 Operator RBAC 与 Prometheus Pod RBAC 明确区分为两套权限。[Prometheus Operator RBAC](https://prometheus-operator.dev/docs/platform/rbac/)

### 2.4 shards 与 replicas

`v0.75.0` 中 `replicas * shards` 是创建的 Agent Pod 总数；`shards` 负责分配目标，`replicas` 是每个 shard 的副本数。[字段定义](https://github.com/prometheus-operator/prometheus-operator/blob/v0.75.0/pkg/apis/monitoring/v1/prometheus_types.go#L220-L240)

该版本对 `ServiceMonitor`、`PodMonitor` 使用目标 `__address__` 做 hash，对 `Probe` 使用 `__param_target__`；生成 `ScrapeConfig` 时也会在 `shards != 1` 时补充基于 address 的 sharding relabeling，除非配置已经提供 hashmod 规则。[sharding 字段说明](https://github.com/prometheus-operator/prometheus-operator/blob/v0.75.0/pkg/apis/monitoring/v1/prometheus_types.go#L226-L240)、[ScrapeConfig sharding](https://github.com/prometheus-operator/prometheus-operator/blob/v0.75.0/pkg/prometheus/promcfg.go#L4048-L4053)

一期需要固定以下语义：

- `shards=1, replicas=1`：每个目标抓取一次；
- `shards=N, replicas=1`：目标按 shard 分配，用于分摊抓取负载；
- `shards=N, replicas=M`：总计 `N*M` 个 Pod，每个 shard 的目标由 `M` 个副本重复抓取、重复 remote write；远端是否去重不属于 Prometheus Operator 的保证。

因此 `replicas` 不能当作无副作用的 HA 开关。若远端不按 external labels 去重，副本会形成重复样本或并列时间序列；一期验收应使用真实 remote write 接收端确认期望行为。

### 2.5 多 remote_write 与性能参数

`remoteWrite` 是数组，所以一个 Agent 可以配置多个目标；Prometheus 会为每个 destination 建立独立队列并把样本发送到各目标。Agent mode 的官方定义本身也明确支持一个或多个 Remote Write 位置。[Prometheus Agent](https://prometheus.io/docs/prometheus/latest/prometheus_agent/)、[RemoteWriteSpec](https://github.com/prometheus-operator/prometheus-operator/blob/v0.75.0/pkg/apis/monitoring/v1/prometheus_types.go#L1277-L1398)

`v0.75.0` 可以承载的一期性能调节基线为：

- 目标级：`remoteTimeout`、`writeRelabelConfigs`、`queueConfig`、`metadataConfig`；
- queue 级：`capacity`、`minShards`、`maxShards`、`maxSamplesPerSend`、`batchSendDeadline`、`maxRetries`、`minBackoff`、`maxBackoff`、`retryOnRateLimit`；
- 认证/TLS：`basicAuth`、`authorization`、`oauth2`、`tlsConfig` 等，但本 Operator 对外 API 应只暴露 Secret 引用，不透传明文凭据字段。

字段证据见 [QueueConfig](https://github.com/prometheus-operator/prometheus-operator/blob/v0.75.0/pkg/apis/monitoring/v1/prometheus_types.go#L1400-L1434)。Prometheus 官方说明每个 remote write 目标都有独立 queue，队列内存近似与 `shards * (capacity + max_samples_per_send)` 成正比；调整 `capacity` 或 batch 时必须同时评估 `max_shards`，不能把参数逐个孤立开放。[Remote write tuning](https://prometheus.io/docs/practices/remote_write/)

`sampleAgeLimit` 在 `v0.75.0` 已存在，但还要求 Prometheus `>=v2.50.0`；若一期只保证 Prometheus `>=v2.32.0`，该字段不能进入共同基线。Remote Write 2.0 到 Prometheus Operator `v0.78.0` 才加入，也不能当作 `v0.75.0` 基线能力。[sampleAgeLimit](https://github.com/prometheus-operator/prometheus-operator/blob/v0.75.0/pkg/apis/monitoring/v1/prometheus_types.go#L1424-L1433)、[Remote Write 2.0](https://github.com/prometheus-operator/prometheus-operator/blob/v0.78.0/CHANGELOG.md#L1-L8)

## 3. 运行时能力门禁

本 Operator 在创建 PrometheusAgent CR 前，至少要验证：

1. Kubernetes 版本精确为一期支持的 `1.19.x` 或 `1.28.x`；
2. 五类 CRD 存在且服务所需版本：
   - `prometheusagents.monitoring.coreos.com/v1alpha1`；
   - `scrapeconfigs.monitoring.coreos.com/v1alpha1`；
   - `servicemonitors.monitoring.coreos.com/v1`；
   - `podmonitors.monitoring.coreos.com/v1`；
   - `probes.monitoring.coreos.com/v1`；
3. Prometheus Operator 镜像版本满足 `>=0.75.0`；在 Kubernetes `1.19` 上还必须 `<0.84.0`；
4. Operator 的 RBAC 能读取四类采集配置并调谐 PrometheusAgent；PrometheusAgent ServiceAccount 具备目标发现所需 RBAC；
5. PrometheusAgent 使用的 Prometheus 版本 `>=2.32.0`，且 `spec.version` 与实际镜像版本一致；
6. PrometheusAgent CRD schema 中实际存在本 Operator 准备写入的 selectors、`replicas`、`shards`、`remoteWrite` 与 queue 参数。

CRD 存在、Operator 版本、RBAC 三者必须分别检查。官方发布说明已经证明“Operator 正常运行但缺 CRD/RBAC，相关 controller 不工作”是合法状态，不能只通过 Pod Ready 判断兼容。

能力不满足时，应拒绝创建或更新下游 PrometheusAgent，并通过上层 CR `status`、Kubernetes Event 和明确错误信息报告缺失项；不能尝试安装或升级预装 Prometheus Operator。

## 4. 双版本最低验收矩阵

| 验收项 | Kubernetes 1.19 / KSM 2.3.0 | Kubernetes 1.28 / KSM 2.11.0 |
| --- | --- | --- |
| workload/RBAC API 创建 | 必测 | 必测 |
| resource allowlist | 对一期允许的每个 resource 做启动与 `/metrics` 验证 | 同左 |
| metric allowlist、denylist 互斥 | 必测 | 必测 |
| namespace allow/deny 组合 | 必测，确认生成参数与“允许范围减 denylist”的源码语义一致 | 同左 |
| `replicas=2` | 验证两个副本均暴露相同范围，记录重复抓取语义 | 同左 |
| PrometheusAgent 五类 CRD/字段 | 预装环境盘点并做 smoke test | 同左 |
| 四类采集配置 label selector | 每类至少一份匹配、一份不匹配 | 同左 |
| namespace selector | 本 namespace、指定 namespace、全部 namespace 各一例 | 同左 |
| `shards=2, replicas=1` | 验证目标只落入一个 shard | 同左 |
| `shards=2, replicas=2` | 验证 4 Pod 及每 shard 双份抓取 | 同左 |
| 两个 remote_write 目标 | 两端均收到预期样本；单端故障不改变另一端队列 | 同左 |
| queue 性能参数 | 验证生成配置，并观察 pending、failed、retry 与资源占用指标 | 同左 |

对 Kubernetes `1.19 + kube-state-metrics 2.3.0`，还要把所有一期允许的 resource 名称逐一纳入验证。只验证 Pod Ready 或 `/healthz` 不足以证明 collector 与 API 兼容。

## 5. 对后续决策票的输入

- “定义 kube-state-metrics 实例 API 与版本适配”必须把 `1.19 + 2.3.0` 定义为项目验证组合，并决定一期允许的 resource/metric 名称集合；普通 `replicas` 不得描述成分片。
- “定义采集实例 API 与 Prometheus Operator 合约”必须决定是否接受 `PrometheusAgent`、`ScrapeConfig` 的 `v1alpha1` 风险，并采用 `>=0.75.0`、CRD、RBAC、Prometheus `>=2.32.0` 四重门禁。
- “盘点两个目标集群的 Prometheus Operator 能力”必须读取真实 Operator 镜像、CRD served versions/schema 和两套 ServiceAccount RBAC；仅看 Helm release 或 Deployment Ready 不够。
- “定义 remote_write 与采集性能配置契约”应把一期参数限定在 `v0.75.0` 已有且 Prometheus `>=2.32.0` 可用的共同字段；若选择 `sampleAgeLimit`，Prometheus 下限要提高到 `v2.50.0`；若选择 Remote Write 2.0，Operator 下限要提高到 `v0.78.0`，并继续核对 Prometheus 版本。

本研究没有新增独立 decision ticket：已暴露的精确问题分别落在上述既有 ticket 中。
