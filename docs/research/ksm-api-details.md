# kube-state-metrics v2.3.0 / v2.11.0 API 与部署事实核查

## 结论摘要

一期可以在两个版本之上提供同一组稳定字段，但不能把 kube-state-metrics 的原始 CLI 原样透传为 CRD：

1. 两版的默认 resource collector 集合完全相同，共 28 项，恰好也是两版“可用 collector catalog”的交集。因此一期可以把这 28 项固化为版本无关的 `collectors` 枚举；空列表表示使用该共同默认集合。`v2.3.0` 独有的 `verticalpodautoscalers` 与 `v2.11.0` 新增的 RBAC、EndpointSlice 等 collector 不应进入一期公共 API。
2. metric allowlist、denylist、opt-in list 在两版中语义一致，均接受 Go `regexp`，匹配没有自动加 `^...$`。allowlist 与 denylist 互斥；错误正则导致进程启动失败；合法但匹配不到任何 metric 的表达式不会报错。因此 Operator 应在写入 workload 前编译正则并校验互斥，但不能承诺“metric 名一定存在”。
3. namespace allowlist 与 denylist 不是互斥关系：allowlist 为空表示所有 namespace，denylist 通过字段选择器继续做排除；两者同时存在时是“allow 集合再减 deny 集合”。它们是 namespace 名列表，不是正则。
4. 官方 standard manifests 在两版中均创建 `ServiceAccount`、`ClusterRole`、`ClusterRoleBinding`、`Deployment`、headless `Service`。端口和 probes 相同；`v2.11.0` 扩大了 RBAC，并强化了 ServiceAccount token 与容器 `securityContext`。
5. `replicas > 1` 不等于 sharding。standard `Deployment` 没有传 sharding 参数；headless `Service` 会把所有匹配 Pod 暴露为 endpoints。因而多个普通 replica 各自保有完整集群快照并导出同一批对象指标，采集端会看到语义重复的样本。若一期仍暴露 `replicas`，必须把这一行为写进 API 文档，默认值保持 `1`，不能将其描述为扩容分片。

本文只使用 kube-state-metrics 官方仓库的两个不可变 commit：

- `v2.3.0`：[`b761b5382bdd85d7af915516f48cba1c46859c1d`](https://github.com/kubernetes/kube-state-metrics/tree/b761b5382bdd85d7af915516f48cba1c46859c1d)
- `v2.11.0`：[`5e4e7d0bb727ac4c50097770ce0dd7979eb04b77`](https://github.com/kubernetes/kube-state-metrics/tree/5e4e7d0bb727ac4c50097770ce0dd7979eb04b77)

## 1. Resource collector：默认值、catalog 与未知值

### 1.1 共同默认集合

两版 `DefaultResources` 源码逐项相同：[`v2.3.0 pkg/options/resource.go`](https://github.com/kubernetes/kube-state-metrics/blob/b761b5382bdd85d7af915516f48cba1c46859c1d/pkg/options/resource.go#L23-L57)、[`v2.11.0 pkg/options/resource.go`](https://github.com/kubernetes/kube-state-metrics/blob/5e4e7d0bb727ac4c50097770ce0dd7979eb04b77/pkg/options/resource.go#L23-L57)。共同的 28 项是：

```text
certificatesigningrequests
configmaps
cronjobs
daemonsets
deployments
endpoints
horizontalpodautoscalers
ingresses
jobs
leases
limitranges
mutatingwebhookconfigurations
namespaces
networkpolicies
nodes
persistentvolumeclaims
persistentvolumes
poddisruptionbudgets
pods
replicasets
replicationcontrollers
resourcequotas
secrets
services
statefulsets
storageclasses
validatingwebhookconfigurations
volumeattachments
```

两版都在 `--resources` 未提供任何非空项时回退到 `DefaultResources`：`v2.3.0` 的入口见 [`main.go`](https://github.com/kubernetes/kube-state-metrics/blob/b761b5382bdd85d7af915516f48cba1c46859c1d/main.go#L106-L117)，`v2.11.0` 见 [`pkg/app/server.go`](https://github.com/kubernetes/kube-state-metrics/blob/5e4e7d0bb727ac4c50097770ce0dd7979eb04b77/pkg/app/server.go#L183-L203)。CLI 的逗号列表解析会 trim 每项并忽略空字符串，因此空字符串与“未配置”最终都落入默认集合；两版实现相同，见 [`v2.3.0 ResourceSet.Set`](https://github.com/kubernetes/kube-state-metrics/blob/b761b5382bdd85d7af915516f48cba1c46859c1d/pkg/options/types.go#L70-L100) 与 [`v2.11.0 ResourceSet.Set`](https://github.com/kubernetes/kube-state-metrics/blob/5e4e7d0bb727ac4c50097770ce0dd7979eb04b77/pkg/options/types.go#L71-L101)。

### 1.2 “可用”不等于“默认”，两版 catalog 不同

`v2.3.0` 的可用 catalog 是共同 28 项再加 `verticalpodautoscalers`；完整注册表见 [`internal/store/builder.go`](https://github.com/kubernetes/kube-state-metrics/blob/b761b5382bdd85d7af915516f48cba1c46859c1d/internal/store/builder.go#L222-L252)。`v2.11.0` 不再包含 `verticalpodautoscalers`，但增加：

```text
clusterrolebindings
clusterroles
endpointslices
ingressclasses
rolebindings
roles
serviceaccounts
```

完整注册表见 [`v2.11.0 internal/store/builder.go`](https://github.com/kubernetes/kube-state-metrics/blob/5e4e7d0bb727ac4c50097770ce0dd7979eb04b77/internal/store/builder.go#L313-L349)。所以“共同默认 28 项”也正好是两版 catalog 的交集，可以形成稳定 catalog；任一版本的额外 collector 都需要未来按能力/version 扩展，不能现在伪装成通用字段值。

### 1.3 Unknown resource 是启动失败，不是忽略

两版的 `WithEnabledResources` 都逐项查询内部注册表；发现未知项就返回包含 available resources 的错误：[`v2.3.0`](https://github.com/kubernetes/kube-state-metrics/blob/b761b5382bdd85d7af915516f48cba1c46859c1d/internal/store/builder.go#L89-L104)、[`v2.11.0`](https://github.com/kubernetes/kube-state-metrics/blob/5e4e7d0bb727ac4c50097770ce0dd7979eb04b77/internal/store/builder.go#L108-L123)。`v2.3.0` 随后 `Fatal` 退出，`v2.11.0` 将错误返回到启动链路：[`v2.3.0 main.go`](https://github.com/kubernetes/kube-state-metrics/blob/b761b5382bdd85d7af915516f48cba1c46859c1d/main.go#L115-L117)、[`v2.11.0 server.go`](https://github.com/kubernetes/kube-state-metrics/blob/5e4e7d0bb727ac4c50097770ce0dd7979eb04b77/pkg/app/server.go#L201-L203)。

对 Operator 的直接约束是：CRD/CEL 或 controller 必须在创建 Deployment 前拒绝共同 catalog 之外的值，并以 `InvalidSpec` 报告；不能把错误留给 Pod crash loop。

## 2. Filter 参数的精确语义

### 2.1 Metric allowlist / denylist

两版行为相同：

- allowlist 与 denylist 只要同时非空就报错；两者都为空时采用“空 denylist”，即所有非 opt-in metric 均通过。[`v2.3.0 allowdenylist.New`](https://github.com/kubernetes/kube-state-metrics/blob/b761b5382bdd85d7af915516f48cba1c46859c1d/pkg/allowdenylist/allowdenylist.go#L35-L60) 与 [`v2.11.0`](https://github.com/kubernetes/kube-state-metrics/blob/5e4e7d0bb727ac4c50097770ce0dd7979eb04b77/pkg/allowdenylist/allowdenylist.go#L34-L59)。
- 每个列表项直接传给 Go `regexp.Compile`，再用 `MatchString(metricName)`；实现不会自动锚定表达式。若用户要精确名匹配，应由 Operator 生成或要求 `^name$`。[`v2.3.0 Parse/IsIncluded`](https://github.com/kubernetes/kube-state-metrics/blob/b761b5382bdd85d7af915516f48cba1c46859c1d/pkg/allowdenylist/allowdenylist.go#L62-L117) 与 [`v2.11.0`](https://github.com/kubernetes/kube-state-metrics/blob/5e4e7d0bb727ac4c50097770ce0dd7979eb04b77/pkg/allowdenylist/allowdenylist.go#L61-L116)。
- 无效正则在启动阶段报错；合法但匹配不到任何 metric 的表达式不会报错，因为该路径只编译表达式，不查询 metric catalog。这一点是由上述 `Parse`/`IsIncluded` 源码直接推得。
- CLI parser 会 trim、去重并忽略空项，所以空数组/空字符串不应映射成一个空正则。两版 `MetricSet.Set` 相同：[`v2.3.0`](https://github.com/kubernetes/kube-state-metrics/blob/b761b5382bdd85d7af915516f48cba1c46859c1d/pkg/options/types.go#L33-L63)、[`v2.11.0`](https://github.com/kubernetes/kube-state-metrics/blob/5e4e7d0bb727ac4c50097770ce0dd7979eb04b77/pkg/options/types.go#L34-L64)。
- CLI 在编译正则前先按逗号拆分，所以单个表达式不能包含逗号，例如 `{1,3}` 会被拆坏。若 CRD 的数组最终仍 join 成 CLI 逗号列表，Operator 应拒绝单项中的逗号，不能声称支持全部 Go regexp 表达式。

### 2.2 Metric opt-in list

opt-in list 只影响被 generator 标为 `OptIn` 的 metric；普通 metric 总是通过这一层。列表项同样按 Go `regexp` 编译并以 `MatchString` 匹配；空列表意味着所有 opt-in metric 都关闭。两版代码一致：[`v2.3.0 pkg/optin/optin.go`](https://github.com/kubernetes/kube-state-metrics/blob/b761b5382bdd85d7af915516f48cba1c46859c1d/pkg/optin/optin.go#L27-L71)、[`v2.11.0`](https://github.com/kubernetes/kube-state-metrics/blob/5e4e7d0bb727ac4c50097770ce0dd7979eb04b77/pkg/optin/optin.go#L27-L71)。

allow/deny filter 与 opt-in filter 是 AND 组合：任一 filter 不通过就不暴露该 metric，见两版共同的 [`CompositeFamilyGeneratorFilter`](https://github.com/kubernetes/kube-state-metrics/blob/5e4e7d0bb727ac4c50097770ce0dd7979eb04b77/pkg/metric_generator/filter.go#L28-L42)。因此设置 allowlist 时，想开启的 opt-in metric 必须同时匹配 allowlist 与 opt-in list；denylist 仍可排除已 opt-in 的 metric。

### 2.3 Namespace allowlist / denylist

两版 `NamespaceList` 都是 trim 后的字面字符串列表，不编译正则；allowlist 为空时转换为 `NamespaceAll`，denylist 为空时不生成排除 selector：[`v2.3.0 NamespaceList`](https://github.com/kubernetes/kube-state-metrics/blob/b761b5382bdd85d7af915516f48cba1c46859c1d/pkg/options/types.go#L107-L161)、[`v2.11.0 NamespaceList`](https://github.com/kubernetes/kube-state-metrics/blob/5e4e7d0bb727ac4c50097770ce0dd7979eb04b77/pkg/options/types.go#L198-L252)。

allowlist 与 denylist 可以同时设置：入口先取 allow namespaces，再将 deny namespaces 变成 `metadata.namespace!=...` 字段选择器。`v2.3.0` 见 [`main.go`](https://github.com/kubernetes/kube-state-metrics/blob/b761b5382bdd85d7af915516f48cba1c46859c1d/main.go#L119-L121)，`v2.11.0` 见 [`server.go`](https://github.com/kubernetes/kube-state-metrics/blob/5e4e7d0bb727ac4c50097770ce0dd7979eb04b77/pkg/app/server.go#L205-L213)。最终语义是 `allow - deny`；同名项出现在两个列表时，deny 生效。

## 3. Standard manifests 的对象与版本差异

### 3.1 共同对象与网络接口

两版 `examples/standard` 均包含：

- namespaced `ServiceAccount`
- cluster-scoped `ClusterRole`
- cluster-scoped `ClusterRoleBinding`
- namespaced `Deployment`
- namespaced headless `Service`

两版 Deployment 都默认 `replicas: 1`，只使用 `kubernetes.io/os: linux` node selector；未设置 affinity、tolerations、topology spread 或 Pod/container resources。container 同时暴露 `http-metrics:8080` 与 `telemetry:8081`；liveness 是 `8080/healthz`，readiness 是 `8081/`，初始延迟与超时均为 5 秒。证据：[`v2.3.0 deployment.yaml`](https://github.com/kubernetes/kube-state-metrics/blob/b761b5382bdd85d7af915516f48cba1c46859c1d/examples/standard/deployment.yaml#L1-L46)、[`v2.11.0 deployment.yaml`](https://github.com/kubernetes/kube-state-metrics/blob/5e4e7d0bb727ac4c50097770ce0dd7979eb04b77/examples/standard/deployment.yaml#L1-L55)。

两版 Service 都是 `clusterIP: None`，按 `app.kubernetes.io/name: kube-state-metrics` 选 Pod，并同时发布 `8080`、`8081`：[`v2.3.0 service.yaml`](https://github.com/kubernetes/kube-state-metrics/blob/b761b5382bdd85d7af915516f48cba1c46859c1d/examples/standard/service.yaml#L1-L20)、[`v2.11.0 service.yaml`](https://github.com/kubernetes/kube-state-metrics/blob/5e4e7d0bb727ac4c50097770ce0dd7979eb04b77/examples/standard/service.yaml#L1-L20)。

### 3.2 差异必须留在版本 adapter 内部

| 项目 | v2.3.0 | v2.11.0 | Operator 约束 |
| --- | --- | --- | --- |
| image registry | `k8s.gcr.io/...:v2.3.0` | `registry.k8s.io/...:v2.11.0` | 按 Kubernetes 版本固定，CR 不开放 image override |
| ServiceAccount token | SA 未设置 `automountServiceAccountToken` | SA 设为 `false`，Pod 显式设为 `true` | 生成各版本对应的 manifest 语义 |
| container securityContext | 仅 `runAsUser: 65534` | 另有 `allowPrivilegeEscalation: false`、drop `ALL`、`readOnlyRootFilesystem`、`runAsNonRoot`、`seccompProfile: RuntimeDefault` | 不作为用户字段；随 adapter 固定 |
| RBAC | 共同默认 collector 所需的旧资源 | 额外允许 `serviceaccounts`、`endpointslices`、`ingressclasses` 与 RBAC resources | 不能把 v2.11.0 ClusterRole 无条件复用到 v2.3.0 方案 |

Deployment 差异见上节两份 deployment；ServiceAccount 差异见 [`v2.3.0`](https://github.com/kubernetes/kube-state-metrics/blob/b761b5382bdd85d7af915516f48cba1c46859c1d/examples/standard/service-account.yaml#L1-L9) 与 [`v2.11.0`](https://github.com/kubernetes/kube-state-metrics/blob/5e4e7d0bb727ac4c50097770ce0dd7979eb04b77/examples/standard/service-account.yaml#L1-L10)；RBAC 差异见 [`v2.3.0 ClusterRole`](https://github.com/kubernetes/kube-state-metrics/blob/b761b5382bdd85d7af915516f48cba1c46859c1d/examples/standard/cluster-role.yaml#L1-L109) 与 [`v2.11.0 ClusterRole`](https://github.com/kubernetes/kube-state-metrics/blob/5e4e7d0bb727ac4c50097770ce0dd7979eb04b77/examples/standard/cluster-role.yaml#L1-L128)。

两版的 `ClusterRoleBinding` 都把 cluster-scoped role 绑定到 `kube-system/kube-state-metrics` ServiceAccount：[`v2.3.0`](https://github.com/kubernetes/kube-state-metrics/blob/b761b5382bdd85d7af915516f48cba1c46859c1d/examples/standard/cluster-role-binding.yaml#L1-L16)、[`v2.11.0`](https://github.com/kubernetes/kube-state-metrics/blob/5e4e7d0bb727ac4c50097770ce0dd7979eb04b77/examples/standard/cluster-role-binding.yaml#L1-L16)。多实例 Operator 必须为每个实例生成无冲突的 ServiceAccount/Role/Binding/Deployment/Service 名称；这一点是项目多实例要求带来的设计约束，不是上游 standard manifest 已解决的能力。

## 4. `replicas > 1` 的真实语义

standard Deployment 没有任何 args，因此每个 Pod 使用默认 `--shard=0 --total-shards=1`；CLI 默认值见 [`v2.3.0 cli-arguments.md`](https://github.com/kubernetes/kube-state-metrics/blob/b761b5382bdd85d7af915516f48cba1c46859c1d/docs/cli-arguments.md#L48-L60) 与 [`v2.11.0 cli-arguments.md`](https://github.com/kubernetes/kube-state-metrics/blob/5e4e7d0bb727ac4c50097770ce0dd7979eb04b77/docs/cli-arguments.md#L64-L78)。结合 headless Service 对 Pod label 的选择，调大 Deployment replicas 后，每个 Pod 都会成为一个可发现 endpoint，但每个进程仍处理完整对象集合。

官方对 sharding 的定义是：只有显式配置 `--shard` 与 `--total-shards`，各实例才按 Kubernetes Object UID 的 hash/modulo 决定由哪个 shard 负责；即使 sharded，每个实例仍接收所有对象的网络流量并完成反序列化。[`v2.3.0 README`](https://github.com/kubernetes/kube-state-metrics/blob/b761b5382bdd85d7af915516f48cba1c46859c1d/README.md#L218-L237)、[`v2.11.0 README`](https://github.com/kubernetes/kube-state-metrics/blob/5e4e7d0bb727ac4c50097770ce0dd7979eb04b77/README.md#L229-L248)。官方还明确建议为 kube-state-metrics 建立能发现两个 metrics endpoints 的专用 scrape config：[`v2.3.0 README`](https://github.com/kubernetes/kube-state-metrics/blob/b761b5382bdd85d7af915516f48cba1c46859c1d/README.md#L260-L270)。

由这些一手事实可以直接推导：

- `replicas > 1` 且不设置 sharding 时，各 endpoint 暴露同一批 Kubernetes 对象指标；目标发现标签可能令底层 Prometheus series 不完全同标签，但指标在业务语义上重复。
- 每个 replica 都建立自己的 list/watch 与内存快照，不能视为按 replica 数分摊 collector 工作量。
- 若采集系统抓取所有 endpoints，重复采集与 remote_write 开销随 replica 数增加；若只抓一个 endpoint，则 replica 只是潜在冗余，切换行为由采集发现链路决定，kube-state-metrics 自身不提供 leader election。
- 自动 sharding 需要 StatefulSet、`--pod` 和 `--pod-namespace`，且上游明确标为 experimental，不应暗中绑定到一期的 `replicas` 字段。

因此一期若必须暴露 `replicas`，建议约束 `>=1`、默认 `1`，字段说明明确写成“完整副本数量，不启用 sharding”；是否允许大于 `1` 应由上层采集/去重策略承担，而不是由 Operator 宣称实现 HA 或扩容。

## 5. 适合一期 CRD 的最小稳定集合

以下是两版共同支持、且能满足已知需求的最小字段。命名是建议，不是上游已有 API。

| 建议字段 | 类型/默认 | 校验与映射 |
| --- | --- | --- |
| `spec.enabled` | `bool`，默认 `true` | 公共生命周期字段；`false` 删除受管资源、保留 CR |
| `spec.replicas` | `int32`，默认 `1` | `>=1`；映射 Deployment replicas；明确“不启用 sharding” |
| `spec.podResources` | Kubernetes `ResourceRequirements` | 直接映射 container requests/limits；避免与 collector `resources` 混名 |
| `spec.collectors` | 共同 28 项的去重枚举列表；空表示共同默认集合 | 映射 `--resources`；拒绝 unknown；不包含任一版本独有 collector |
| `spec.metricFilter.allowList` | `[]string`，默认空 | 与 `denyList` 互斥；逐项按 Go regexp 预编译；映射 `--metric-allowlist` |
| `spec.metricFilter.denyList` | `[]string`，默认空 | 与 `allowList` 互斥；逐项按 Go regexp 预编译；映射 `--metric-denylist` |
| `spec.metricFilter.optInList` | `[]string`，默认空 | 逐项按 Go regexp 预编译；映射 `--metric-opt-in-list` |
| `spec.namespaceFilter.allowList` | `[]string`，默认空即全部 | 使用 Kubernetes namespace name 校验；映射 `--namespaces` |
| `spec.namespaceFilter.denyList` | `[]string`，默认空 | 可与 allow 同时存在，语义为排除；映射 `--namespaces-denylist` |

版本 adapter 内部固定：

- `Kubernetes 1.19 -> kube-state-metrics v2.3.0` 的 image、RBAC、ServiceAccount/Pod token、container securityContext、probes、ports、Service 与内部 scheduling；
- `Kubernetes 1.28 -> kube-state-metrics v2.11.0` 的对应版本对象；
- 基础模式在两版均生成 `Deployment`；水平/节点分片的额外模式与版本约束见第 7 节；
- namespace、实例前缀与 owner labels 用于多实例资源命名和归属，但不暴露原始 PodTemplate 或 `extraArgs`。

一期暂不应暴露：image/tag/registry override、`extraArgs`、端口、Service type、PodTemplate、affinity/tolerations/nodeSelector、custom resource state、label/annotation allowlist、TLS/config file。分片不能通过 `extraArgs` 透传，应按第 7 节建模成类型化部署模式。

## 6. 对“定义 kube-state-metrics 实例 API 与版本适配”的决策影响

1. **collector catalog 可以现在锁定**：使用共同 28 项，空列表语义明确为这 28 项；不把上游版本差异泄漏到 CRD。
2. **metric 名不能做可靠 enum**：两版 generator catalog 与 opt-in 状态会演进。CRD 保存正则字符串，Operator 只保证语法和互斥正确，不保证表达式命中；未命中应是合法配置而非 `InvalidSpec`。
3. **namespace 过滤模型应同时保留 allow/deny**：二者不是互斥模式；交集/排除语义应写入 API 注释和测试。
4. **`replicas` 必须与 sharding 解耦**：一期若允许 `>1`，status/文档不能称其为 shard 或线性扩容，也不承诺不重复采集。
5. **版本适配不只是镜像 tag**：至少还包括 RBAC、ServiceAccount token 与 container securityContext；应以两个显式 adapter/manifest builder 的 golden tests 固定，而不是一个通用模板只替换 image。
6. **所有可在调谐前发现的错误都应前置**：unknown collector、allow/deny 同时非空、错误 regex、非法 namespace name、`replicas < 1` 都应先产生 `InvalidSpec`，避免生成必然 crash 的 Pod。

## 7. 部署模式补充核查

### 7.1 能力矩阵

| 模式 | v2.3.0 | v2.11.0 | 上游状态 | workload 语义 |
| --- | --- | --- | --- | --- |
| 单副本 Deployment | 支持 | 支持 | standard example | 一个完整 collector 副本，默认不分片 |
| Manual horizontal sharding | 支持 | 支持 | horizontal sharding 本身未标 experimental | 每个进程显式配置唯一 `--shard=i --total-shards=N`；上游没有“manual sharding StatefulSet”示例 |
| Automated horizontal sharding | 支持 | 支持 | **experimental** | 一个 StatefulSet，Pod ordinal 是 shard，StatefulSet replicas 是 total shards |
| DaemonSet node sharding | 不支持 | 支持 | `--node` 明确标为 **experimental** | 每个节点一个 KSM，只采集该节点上的 Pod；需要 companion Deployment 覆盖其他 collectors，并可再加一个 Deployment 覆盖未调度 Pod |

`v2.3.0` 与 `v2.11.0` 都记录了 manual/automated horizontal sharding；两版 README 分别见 [`v2.3.0`](https://github.com/kubernetes/kube-state-metrics/blob/b761b5382bdd85d7af915516f48cba1c46859c1d/README.md#L218-L237) 与 [`v2.11.0`](https://github.com/kubernetes/kube-state-metrics/blob/5e4e7d0bb727ac4c50097770ce0dd7979eb04b77/README.md#L229-L248)。DaemonSet node sharding 只存在于 `v2.11.0` 的 [`README`](https://github.com/kubernetes/kube-state-metrics/blob/5e4e7d0bb727ac4c50097770ce0dd7979eb04b77/README.md#L250-L296)、[`--node` CLI 帮助](https://github.com/kubernetes/kube-state-metrics/blob/5e4e7d0bb727ac4c50097770ce0dd7979eb04b77/docs/cli-arguments.md#L62-L69) 和 [`examples/daemonsetsharding`](https://github.com/kubernetes/kube-state-metrics/tree/5e4e7d0bb727ac4c50097770ce0dd7979eb04b77/examples/daemonsetsharding)；`v2.3.0` 的 Options/CLI 没有 `Node`/`--node`，见其完整 flags 定义 [`pkg/options/options.go`](https://github.com/kubernetes/kube-state-metrics/blob/b761b5382bdd85d7af915516f48cba1c46859c1d/pkg/options/options.go#L29-L114)。`v2.11.0` 自带 changelog 进一步确认 per-node FieldSelector sharding 在 `v2.7.0` 才加入，而采集尚未分配节点的 Pod 是 `v2.11.0` 才补齐：[`CHANGELOG v2.7.0`](https://github.com/kubernetes/kube-state-metrics/blob/5e4e7d0bb727ac4c50097770ce0dd7979eb04b77/CHANGELOG.md#L115-L140)、[`CHANGELOG v2.11.0`](https://github.com/kubernetes/kube-state-metrics/blob/5e4e7d0bb727ac4c50097770ce0dd7979eb04b77/CHANGELOG.md#L1-L17)。因此在固定 `Kubernetes 1.19 -> KSM v2.3.0` 的矩阵下，DaemonSet node sharding 不能成为两个集群都可用的共同能力。

### 7.2 Manual horizontal sharding

Manual sharding 的进程参数是：

```text
--shard=<zero-based ordinal>
--total-shards=<N>
```

两版参数与默认值相同；`--shard` 默认 `0`，`--total-shards` 默认 `1`，此组合明确走不分片路径：[`v2.3.0 flags`](https://github.com/kubernetes/kube-state-metrics/blob/b761b5382bdd85d7af915516f48cba1c46859c1d/pkg/options/options.go#L93-L111)、[`v2.11.0 flags`](https://github.com/kubernetes/kube-state-metrics/blob/5e4e7d0bb727ac4c50097770ce0dd7979eb04b77/pkg/options/options.go#L120-L148)、两版相同的 [`NewShardedListWatch`](https://github.com/kubernetes/kube-state-metrics/blob/5e4e7d0bb727ac4c50097770ce0dd7979eb04b77/pkg/sharding/listwatch.go#L35-L45)。

精确实现不是 README 所说的简单 “md5/modulo”：两个 tag 的源码都是以对象 UID 计算 FNV-64a，再调用 jump consistent hash，保留 hash 结果等于本进程 shard ordinal 的对象：[`v2.3.0`](https://github.com/kubernetes/kube-state-metrics/blob/b761b5382bdd85d7af915516f48cba1c46859c1d/pkg/sharding/listwatch.go#L94-L103)、[`v2.11.0`](https://github.com/kubernetes/kube-state-metrics/blob/5e4e7d0bb727ac4c50097770ce0dd7979eb04b77/pkg/sharding/listwatch.go#L94-L103)。List 与 Watch 都在 client 收到完整响应/事件后本地过滤，见 [`List/Watch`](https://github.com/kubernetes/kube-state-metrics/blob/5e4e7d0bb727ac4c50097770ce0dd7979eb04b77/pkg/sharding/listwatch.go#L47-L91)；所以分片减少每个进程保存和导出的对象，但不会按 `1/N` 减少 API list/watch 网络输入。

Manual 模式没有 Pod identity 参数；Operator 必须创建 N 个能携带不同固定 `--shard=i` 的进程。若用 Deployment 表达，每个 shard workload 应只有一个 replica；同一 shard workload 调成多个 replicas 会重复导出该 shard 的全部对象。上游没有一个 Pod template 内能给 StatefulSet 每个 Pod 注入不同固定 `--shard` 值的 manual 示例；一个 StatefulSet 要从 ordinal 派生 shard，只能使用下一节的 auto detection。因而“manual sharding StatefulSet”不是这两个 tag 已提供的原生部署形态。

上游 Options 没有完整验证 `N >= 1` 和 `0 <= i < N`；hash 结果直接与 `shard` 比较。因此 Operator 必须前置验证：`shards >= 1`，并保证恰好生成 `0..N-1` 每个 ordinal 一次。漏掉 ordinal 会漏指标，重复 ordinal 会重复指标；官方也要求监控所有 total shards 是否齐全：[`v2.11.0 README`](https://github.com/kubernetes/kube-state-metrics/blob/5e4e7d0bb727ac4c50097770ce0dd7979eb04b77/README.md#L231-L238)。

### 7.3 Automated horizontal sharding StatefulSet

两版官方 manifest 均使用一个 StatefulSet，并只传：

```text
--pod=$(POD_NAME)
--pod-namespace=$(POD_NAMESPACE)
```

`POD_NAME=metadata.name`、`POD_NAMESPACE=metadata.namespace` 都来自 downward API。证据：[`v2.3.0 statefulset.yaml`](https://github.com/kubernetes/kube-state-metrics/blob/b761b5382bdd85d7af915516f48cba1c46859c1d/examples/autosharding/statefulset.yaml#L1-L59)、[`v2.11.0 statefulset.yaml`](https://github.com/kubernetes/kube-state-metrics/blob/5e4e7d0bb727ac4c50097770ce0dd7979eb04b77/examples/autosharding/statefulset.yaml#L1-L68)。两参数只要都非空就启用 auto path，并优先于静态 `--shard/--total-shards`：[`v2.3.0 Run`](https://github.com/kubernetes/kube-state-metrics/blob/b761b5382bdd85d7af915516f48cba1c46859c1d/pkg/metricshandler/metrics_handler.go#L89-L107)、[`v2.11.0 Run`](https://github.com/kubernetes/kube-state-metrics/blob/5e4e7d0bb727ac4c50097770ce0dd7979eb04b77/pkg/metricshandler/metrics_handler.go#L92-L111)。

运行时先读取自己的 Pod ownerReference 找到 StatefulSet；Pod 名的末尾 ordinal 被解析为 shard，`StatefulSet.spec.replicas` 被读取为 total shards。StatefulSet 更新时各 Pod 重新配置 sharding：[`v2.3.0 detection`](https://github.com/kubernetes/kube-state-metrics/blob/b761b5382bdd85d7af915516f48cba1c46859c1d/pkg/metricshandler/metrics_handler.go#L211-L256)、[`v2.11.0 detection`](https://github.com/kubernetes/kube-state-metrics/blob/5e4e7d0bb727ac4c50097770ce0dd7979eb04b77/pkg/metricshandler/metrics_handler.go#L236-L281)。所以这个模式里 `replicas` 的业务含义就是 shard count，不是每个 shard 的冗余副本数。

除 collector 的 ClusterRole/ClusterRoleBinding 外，官方 auto manifests 还创建 namespaced Role/RoleBinding：允许 `get` 自己的 Pod，以及 `get` 特定 StatefulSet；StatefulSet informer 所需的 `list/watch` 已在 ClusterRole 内。两版局部 RBAC 拓扑相同：[`v2.3.0 role.yaml`](https://github.com/kubernetes/kube-state-metrics/blob/b761b5382bdd85d7af915516f48cba1c46859c1d/examples/autosharding/role.yaml#L1-L24)、[`v2.11.0 role.yaml`](https://github.com/kubernetes/kube-state-metrics/blob/5e4e7d0bb727ac4c50097770ce0dd7979eb04b77/examples/autosharding/role.yaml#L1-L24)。两版也都用 headless Service 选择全部 shard Pods：[`v2.3.0 service.yaml`](https://github.com/kubernetes/kube-state-metrics/blob/b761b5382bdd85d7af915516f48cba1c46859c1d/examples/autosharding/service.yaml#L1-L20)、[`v2.11.0 service.yaml`](https://github.com/kubernetes/kube-state-metrics/blob/5e4e7d0bb727ac4c50097770ce0dd7979eb04b77/examples/autosharding/service.yaml#L1-L20)。采集端必须发现并抓取所有 shard endpoints；只抓一个 Service VIP/一个 Pod 会漏掉其他 shard 的对象。

该能力在两个版本都明确标为 experimental。官方还指出 StatefulSet rollout 逐个终止再创建 Pod，会造成单 shard 短暂不可用，恰逢 scrape 时会漏指标；证据见两版 README 上述 automated sharding 段落。因此 API 不能把它承诺成无损 HA。

### 7.4 DaemonSet node sharding 与 companion Deployments

此模式只在 `v2.11.0` 可用。官方示例实际是三个 workload 角色：

1. `kube-state-metrics-shard` DaemonSet：固定 `--resources=pods`，通过 downward API 将 `spec.nodeName` 注入 `NODE_NAME`，再传 `--node=$(NODE_NAME)`。每个 KSM 用 `spec.nodeName` FieldSelector 只 list/watch 同节点 Pod。[`daemonset.yaml`](https://github.com/kubernetes/kube-state-metrics/blob/5e4e7d0bb727ac4c50097770ce0dd7979eb04b77/examples/daemonsetsharding/daemonset.yaml#L1-L63)、[`README`](https://github.com/kubernetes/kube-state-metrics/blob/5e4e7d0bb727ac4c50097770ce0dd7979eb04b77/README.md#L250-L278)。
2. `kube-state-metrics` Deployment：`replicas: 1`，`--resources` 显式列出共同默认集合中除 `pods` 以外的 27 项；它负责全部非 Pod collectors。[`deployment.yaml`](https://github.com/kubernetes/kube-state-metrics/blob/5e4e7d0bb727ac4c50097770ce0dd7979eb04b77/examples/daemonsetsharding/deployment.yaml#L1-L57)。
3. 可选的 `kube-state-metrics-pods` Deployment：`replicas: 1`，传 `--resources=pods --node=""`，负责尚未分配 `spec.nodeName` 的 Pod；官方 README 明确说没有它就无法覆盖 unassigned Pods。[`deployment-no-node-pods.yaml`](https://github.com/kubernetes/kube-state-metrics/blob/5e4e7d0bb727ac4c50097770ce0dd7979eb04b77/examples/daemonsetsharding/deployment-no-node-pods.yaml#L1-L58)、[`README`](https://github.com/kubernetes/kube-state-metrics/blob/5e4e7d0bb727ac4c50097770ce0dd7979eb04b77/README.md#L280-L296)。这里的字面参数 `--node=""` 不能被渲染器擅自规范化成“未传 `--node`”或普通空值：单元测试确认显式空 node 产生 `spec.nodeName=`，而未配置 node 不产生 selector，[`TestNodeFieldSelector`](https://github.com/kubernetes/kube-state-metrics/blob/5e4e7d0bb727ac4c50097770ce0dd7979eb04b77/pkg/options/types_test.go#L158-L200)。

README 的短片段把 resource flag 误写成单数 `--resource=pods`，但实际 CLI 与官方完整 manifest 都是复数 `--resources=pods`；实现应以 manifest/Options 为准，不能复制 README 拼写。

KSM 会拒绝非空 `--node` 与 `pods` 之外的 resource 组合，见 [`Options.Validate`](https://github.com/kubernetes/kube-state-metrics/blob/5e4e7d0bb727ac4c50097770ce0dd7979eb04b77/pkg/options/options.go#L162-L173)。namespace deny selector 与 node selector 在启动时做 AND 合并，再交给 store builder，见 [`server.go`](https://github.com/kubernetes/kube-state-metrics/blob/5e4e7d0bb727ac4c50097770ce0dd7979eb04b77/pkg/app/server.go#L205-L213)；Pod List/Watch 将该 FieldSelector 直接发给 API server，而不是先收全量 Pod 再在进程内过滤，[`internal/store/pod.go`](https://github.com/kubernetes/kube-state-metrics/blob/5e4e7d0bb727ac4c50097770ce0dd7979eb04b77/internal/store/pod.go#L1752-L1763)。因此 namespace allow/deny 和 metric filters 仍能用于 node-sharded Pod collector，但 DaemonSet 自身的 resource 集合只能是 `pods`。

官方为两类 endpoint 分别创建两个 headless Service：`kube-state-metrics-shard` 选择 DaemonSet Pods，[`daemonset-service.yaml`](https://github.com/kubernetes/kube-state-metrics/blob/5e4e7d0bb727ac4c50097770ce0dd7979eb04b77/examples/daemonsetsharding/daemonset-service.yaml#L1-L20)；`kube-state-metrics` 选择 companion Deployment Pods，[`deployment-service.yaml`](https://github.com/kubernetes/kube-state-metrics/blob/5e4e7d0bb727ac4c50097770ce0dd7979eb04b77/examples/daemonsetsharding/deployment-service.yaml#L1-L20)。后一个 Service 实际同时选择主非 Pod Deployment 与 unassigned-Pod Deployment，因而 endpoints 暴露的 metric families 是异构的，而不是每个 endpoint 内容相同。采集端必须覆盖两类 Service 的全部 endpoints。若 companion Deployment 未排除 `pods`，就会与 DaemonSet 重复 Pod metrics；官方通过显式 27 项资源列表避免这一点。

三个 workload 共用同一个 ServiceAccount/ClusterRole/ClusterRoleBinding；官方没有按角色拆最小权限。[`service-account.yaml`](https://github.com/kubernetes/kube-state-metrics/blob/5e4e7d0bb727ac4c50097770ce0dd7979eb04b77/examples/daemonsetsharding/service-account.yaml#L1-L10)、[`cluster-role-binding.yaml`](https://github.com/kubernetes/kube-state-metrics/blob/5e4e7d0bb727ac4c50097770ce0dd7979eb04b77/examples/daemonsetsharding/cluster-role-binding.yaml#L1-L16)、[`cluster-role.yaml`](https://github.com/kubernetes/kube-state-metrics/blob/5e4e7d0bb727ac4c50097770ce0dd7979eb04b77/examples/daemonsetsharding/cluster-role.yaml#L1-L128)。所以 `--node` 是请求过滤，不是 RBAC 隔离；DaemonSet Pod 在官方例子中仍持有整套 cluster-wide 权限。

这个 DaemonSet 没有 `replicas` 字段；官方 KSM CLI 也没有“暂停 DaemonSet”或“scale to zero”参数。上游提供的唯一实例数量语义是一台符合调度条件的节点对应一个 Pod。因而 `replicas=0` 不能表达该模式的停机；在本项目已经确定的生命周期契约下，`enabled=false` 必须由 Operator 删除 DaemonSet 及两个 companion Deployments/Services，`enabled=true` 再重建。利用虚假 nodeSelector 驱逐所有 Pod 不是 kube-state-metrics 原生机制，也不应成为公共 API。官方 DaemonSet 只有 `kubernetes.io/os: linux` nodeSelector、没有 tolerations；若某目标节点无法调度该 DaemonSet Pod，该节点上的 Pod metrics 就会形成缺口，这属于 Operator 内部 scheduling 策略必须处理的风险。

### 7.5 Filters、重复与发现的跨模式约束

- **单 Deployment**：`replicas=1` 无重复；`replicas>1` 是完整重复副本，沿用第 4 节结论。
- **Manual/auto horizontal sharding**：resource、namespace、metric allow/deny/opt-in 必须在所有 shards 上完全相同。resource/namespace filters 先限定输入范围，之后每种已启用对象再按 UID hash 分片；若某 shard 使用不同 filters，就会造成不完整或不可预测的整体视图。所有 shard endpoints 都必须被抓取。构建顺序可由两版 store builder 复核：[`v2.3.0`](https://github.com/kubernetes/kube-state-metrics/blob/b761b5382bdd85d7af915516f48cba1c46859c1d/internal/store/builder.go#L383-L428)、[`v2.11.0`](https://github.com/kubernetes/kube-state-metrics/blob/5e4e7d0bb727ac4c50097770ce0dd7979eb04b77/internal/store/builder.go#L504-L607)。
- **DaemonSet node sharding**：DaemonSet 与 unassigned-Pod Deployment 只能启用 `pods`；非 Pod collectors 进入单独 Deployment。相同 namespace/metric filters 应复制到三个角色。漏抓任一节点 endpoint 会漏该节点 Pod；漏掉 unassigned endpoint 会漏尚未调度 Pod；让非 Pod companion 再启用 `pods` 会重复。
- **Service discovery**：三种分片模式都不是由 Service 聚合 metrics。headless Service 只是暴露多个独立 endpoints；采集配置必须发现每个 endpoint。该项目又明确不创建 ServiceMonitor/PodMonitor，因此 Operator 至少要给所有相关 Services 一致、可选择的实例 labels，并在 status 中列出它们，但是否完整抓取仍由用户创建的采集配置负责。

### 7.6 能表达三类模式的稳定字段

不要把 workload kind、flags 和 replica 的多重含义塞进一个裸 `replicas`。建议使用有判别字段的 union；名称可调整，但语义应固定：

```yaml
spec:
  deployment:
    mode: Deployment | ManualSharding | AutoSharding | DaemonSetSharding

    # mode=Deployment
    replicas: 1

    # mode=ManualSharding 或 AutoSharding
    shards: 2

    # mode=DaemonSetSharding，仅 v2.11.0/Kubernetes 1.28 可用
    collectUnscheduledPods: true
```

校验规则：

- `Deployment`：`replicas >= 1`，不得设置 `shards`、`collectUnscheduledPods`；`replicas>1` 明确是重复完整副本。
- `ManualSharding`：两版可用；`shards >= 1`；controller 生成 `shards` 个单副本 workload，并固定唯一 ordinal。公共 API 不暴露 `shard` ordinal。
- `AutoSharding`：两版可用但 experimental；`shards >= 1` 映射 StatefulSet replicas；不再接受 `replicas`，因为 replicas 就是 shard count。
- `DaemonSetSharding`：只允许 KSM `v2.11.0`；collector 集合必须包含 `pods`。DaemonSet 固定负责 `pods`，其余已选 collectors 自动进入 companion Deployment；`collectUnscheduledPods=true` 时创建 `--node=""` companion。不得设置 `replicas`/`shards`。
- 模式切换会替换 workload kind/集合，应沿用公共生命周期契约：先清理旧模式的直接受管资源，再创建新模式，避免切换窗口中两套 endpoints 重复暴露。

若不愿把 experimental 能力固化进一期 API，更稳妥的公共 union 只保留 `Deployment | ManualSharding`；`AutoSharding` 与 `DaemonSetSharding` 留到版本矩阵扩大或明确接受 experimental 风险后再增加。尤其 `DaemonSetSharding` 无法在 Kubernetes 1.19/KSM v2.3.0 上兑现，不能宣称为一期双版本共同能力。
