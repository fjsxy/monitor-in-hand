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
- 两版均只生成 `Deployment`，不自动切换为 StatefulSet/DaemonSet；
- namespace、实例前缀与 owner labels 用于多实例资源命名和归属，但不暴露原始 PodTemplate 或 `extraArgs`。

一期暂不应暴露：image/tag/registry override、`extraArgs`、端口、Service type、PodTemplate、affinity/tolerations/nodeSelector、sharding、custom resource state、label/annotation allowlist、TLS/config file。这些能力要么已被需求明确设为内部策略，要么跨版本扩展面明显大于当前必要范围；等出现明确需求后再增加类型化字段。

## 6. 对“定义 kube-state-metrics 实例 API 与版本适配”的决策影响

1. **collector catalog 可以现在锁定**：使用共同 28 项，空列表语义明确为这 28 项；不把上游版本差异泄漏到 CRD。
2. **metric 名不能做可靠 enum**：两版 generator catalog 与 opt-in 状态会演进。CRD 保存正则字符串，Operator 只保证语法和互斥正确，不保证表达式命中；未命中应是合法配置而非 `InvalidSpec`。
3. **namespace 过滤模型应同时保留 allow/deny**：二者不是互斥模式；交集/排除语义应写入 API 注释和测试。
4. **`replicas` 必须与 sharding 解耦**：一期若允许 `>1`，status/文档不能称其为 shard 或线性扩容，也不承诺不重复采集。
5. **版本适配不只是镜像 tag**：至少还包括 RBAC、ServiceAccount token 与 container securityContext；应以两个显式 adapter/manifest builder 的 golden tests 固定，而不是一个通用模板只替换 image。
6. **所有可在调谐前发现的错误都应前置**：unknown collector、allow/deny 同时非空、错误 regex、非法 namespace name、`replicas < 1` 都应先产生 `InvalidSpec`，避免生成必然 crash 的 Pod。
