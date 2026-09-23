# 双版本测试集群的 Prometheus Operator 基线

## 结论

两个目标集群应使用同一套基线：

| 组件 | 固定版本 / 镜像 | 选择理由 |
| --- | --- | --- |
| Prometheus Operator | `v0.83.0` / `quay.io/prometheus-operator/prometheus-operator:v0.83.0` | 它是 `<v0.84.0` 窗口内最后一个 release，仍声明 Kubernetes 最低版本为 `v1.16.0`；从 `v0.84.0` 起 CRD 使用 CEL，默认要求 Kubernetes `>=v1.25.0`，不再适合 `v1.19.16`。[v0.83.0 compatibility](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/Documentation/getting-started/compatibility.md#L14-L26)、[当前安装前置条件](https://prometheus-operator.dev/docs/getting-started/installation/#pre-requisites) |
| config-reloader | `quay.io/prometheus-operator/prometheus-config-reloader:v0.83.0` | 与 Operator 同 release；`v0.83.0` 源码把该镜像版本直接绑定到 Operator 版本。[defaults.go](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/operator/defaults.go#L41-L52) |
| Prometheus | `v2.55.1` / `quay.io/prometheus/prometheus:v2.55.1` | 它是 `v0.83.0` E2E compatibility matrix 中最后一个 Prometheus 2.x 版本，满足 Agent mode `>=v2.32.0`，同时不把一期测试基线扩大到 Prometheus 3 的行为变化。[v0.83.0 compatibility](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/Documentation/getting-started/compatibility.md#L28-L70)、[Agent mode 自 v2.32.0 引入](https://github.com/prometheus/prometheus/blob/6d7569113f1ca814f1e149f74176656540043b8d/CHANGELOG.md#L710-L720)、[v2.55.1 release](https://github.com/prometheus/prometheus/releases/tag/v2.55.1) |

统一版本优于按 Kubernetes 版本拆分：一期依赖的是同一个 `PrometheusAgent` 合约；拆成两套 Operator 会引入 CRD schema、默认行为与生成配置差异，却不能消除 Kubernetes 1.19 的版本偏差风险。`v0.75.2` 可作为最低线候选，但不应作为首选：它使用 `client-go v0.30.2`，同样不与 Kubernetes 1.19 对齐，选择更老版本没有换来上游的 client-go skew 保证。[v0.75.2 go.mod](https://github.com/prometheus-operator/prometheus-operator/blob/35b0f457d8f705f1ac8282b4e22caa68a19c6c43/go.mod#L39-L46)

这不是“上游完整支持 Kubernetes 1.19”的结论。`v0.83.0` 文档一方面声明 Operator 需要 Kubernetes `>=v1.16.0`，另一方面明确说明它使用 `client-go v0.33.1`，超出对应版本矩阵的组合只按 best effort 处理。因此 Kubernetes `v1.19.16` 必须定义为**本项目验收支持**；Kubernetes `v1.28.15` 也应执行同一套验收，不能只凭最低版本声明跳过测试。[v0.83.0 compatibility](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/Documentation/getting-started/compatibility.md#L16-L26)、[client-go compatibility matrix](https://github.com/kubernetes/client-go/blob/v0.33.1/README.md#compatibility-matrix)

## 版本候选比较

| 组合 | 判断 |
| --- | --- |
| Operator `v0.83.0` + Prometheus `v2.55.1` | **一期推荐基线。** Operator 是 Kubernetes 1.19 可接受窗口的末端；Prometheus 版本在该 Operator 的 E2E matrix 内，且保持 2.x。 |
| Operator `v0.83.0` + Prometheus `v3.4.0` | 可行备选。`v3.4.0` 是该 Operator 的默认版本，也是 E2E 主要测试版本；只有团队明确接受 Prometheus 3 行为边界时才选。[defaults.go](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/operator/defaults.go#L41-L48)、[compatibility](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/Documentation/getting-started/compatibility.md#L54-L70) |
| Operator `v0.75.2` + 其 matrix 内 Prometheus | 可作为平台只能提供 `0.75.x` 时的降级候选，但不是更可靠的 Kubernetes 1.19 解法；仍需相同的项目验收。 |

Prometheus `v2.55.1` 自身使用 `client-go v0.31.0`。它对 Kubernetes 1.19 的 service discovery 同样不具备同 minor 的 client-go 保证，所以验收必须覆盖实际 discovery，不可只验证 Pod Ready。[v2.55.1 go.mod](https://github.com/prometheus/prometheus/blob/6d7569113f1ca814f1e149f74176656540043b8d/go.mod#L82-L95)

## CRD 与能力基线

`v0.83.0` 的以下五个 CRD 都是 namespaced，且各自只有一个 `served: true`、`storage: true` 版本：

| Kind | 必需 API | 稳定性 |
| --- | --- | --- |
| `PrometheusAgent` | `monitoring.coreos.com/v1alpha1` | Alpha |
| `ScrapeConfig` | `monitoring.coreos.com/v1alpha1` | Alpha |
| `ServiceMonitor` | `monitoring.coreos.com/v1` | Stable API version |
| `PodMonitor` | `monitoring.coreos.com/v1` | Stable API version |
| `Probe` | `monitoring.coreos.com/v1` | Stable API version |

证据分别来自固定 commit 的 [PrometheusAgent CRD](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/example/prometheus-operator-crd/monitoring.coreos.com_prometheusagents.yaml)、[ScrapeConfig CRD](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/example/prometheus-operator-crd/monitoring.coreos.com_scrapeconfigs.yaml)、[ServiceMonitor CRD](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/example/prometheus-operator-crd/monitoring.coreos.com_servicemonitors.yaml)、[PodMonitor CRD](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/example/prometheus-operator-crd/monitoring.coreos.com_podmonitors.yaml) 与 [Probe CRD](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/example/prometheus-operator-crd/monitoring.coreos.com_probes.yaml)。

所需能力在同一 API 合约中同时存在：

- `PrometheusAgentSpec` 内嵌 `CommonPrometheusFields`；后者包含四组 object selector 与 namespace selector，覆盖 `ServiceMonitor`、`PodMonitor`、`Probe`、`ScrapeConfig`。[PrometheusAgentSpec](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/apis/monitoring/v1alpha1/prometheusagent_types.go#L92-L104)、[selectors](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/apis/monitoring/v1/prometheus_types.go#L128-L202)
- `replicas` 与 `shards` 同时存在，Pod 总数为两者乘积；四类采集配置有明确的默认分片键。[replicas and shards](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/apis/monitoring/v1/prometheus_types.go#L237-L271)
- `remoteWrite` 是数组；每个目标支持 `queueConfig` 等参数。`v2.55.1` 也满足该 schema 中 Remote Write 2.0 `messageVersion` 的最低版本 `v2.54.0`，但是否暴露 V2.0 仍应由上层 API 单独决定。[remoteWrite list](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/apis/monitoring/v1/prometheus_types.go#L442-L444)、[RemoteWriteSpec](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/apis/monitoring/v1/prometheus_types.go#L1520-L1551)、[queueConfig](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/apis/monitoring/v1/prometheus_types.go#L1654-L1690)
- Agent CRD 明确去掉 rule evaluation、持久化存储和 Thanos sidecar，符合一期“只抓取并 remote_write”的边界。[PrometheusAgent type](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/apis/monitoring/v1alpha1/prometheusagent_types.go#L56-L68)

风险不会因统一版本而消失：`PrometheusAgent` 与 `ScrapeConfig` 仍是 `v1alpha1`。一期 Operator 必须按 capability 检查 CRD 的 served version 与字段，而不是只读取 Prometheus Operator Pod 的镜像 tag。

## 安装物边界

测试环境应固定使用官方 release 资产：

```text
https://github.com/prometheus-operator/prometheus-operator/releases/download/v0.83.0/bundle.yaml
sha256: da07af933fda2dbb48e5bc44d4beafff93937e16fc777e19e0d43f332aac500c
size:   4154043 bytes
```

上游安装文档把 `bundle.yaml` 定义为 CRD、Operator、RBAC 的成套安装物。[v0.83.0 installation](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/Documentation/getting-started/installation.md#L28-L47) 上述 SHA-256 是本次下载后计算的校验值，不是上游单独发布的 checksum；本地核对表明 release asset 与 tag commit 中的 `bundle.yaml` 字节完全一致。

边界应固定为：

1. 测试基础设施安装完整的上游 `bundle.yaml`，不从中手工摘取五个 CRD；完整 bundle 还会安装其他 Operator CRD，但这些只是上游安装物的附带内容，不扩大本项目的一期能力合约。
2. bundle 只安装 Operator 本身及其 CRD/RBAC/Service，不创建 `Prometheus`、`PrometheusAgent`、recording rules、alerts 或 Grafana；不要改用会额外创建整套监控 workload 的 kube-prometheus。上游安装文档也明确说明 kube-prometheus 默认会创建 Prometheus、alerts 与 rules。[installation](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/Documentation/getting-started/installation.md#L56-L60)
3. 生产环境仍是“复用预装 Prometheus Operator”：本项目 Operator 只做 capability gate，不负责安装或升级它。上述 bundle 只属于两个 kind 测试集群的可复现前置条件。
4. 若不安装到 `default` namespace，应使用同 tag 的 `kustomization.yaml` 修改 namespace，不能只改 Deployment；`ClusterRoleBinding.subjects[].namespace` 也必须同步。[v0.83.0 kustomization](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/kustomization.yaml)

## ARM64 与不可变镜像事实

两个 kind 集群的全部节点均报告 `linux/arm64`。对官方 Quay registry 执行 OCI manifest inspection，三个选定 tag 均存在 ARM64 manifest：

| 镜像 tag | multi-arch index digest | ARM64 manifest digest |
| --- | --- | --- |
| `quay.io/prometheus-operator/prometheus-operator:v0.83.0` | `sha256:b6a89b8ec08f4cca759b2d579e8545f97ffb897973fcd68148c153f2e936c8b3` | `sha256:d3b8e8d7d5888fb6917e58c0bebae6b5f98ec3db36252db34dd876bb0a5b1cc0` |
| `quay.io/prometheus-operator/prometheus-config-reloader:v0.83.0` | `sha256:78aec597d87aa2b4ba947ab9190538dae93a58a67b8e930aefea1086534b02ef` | `sha256:2be5466844cb5e4e9230e025ed3ae96080c664ce0c0a0a0fca4e3bc1e8e81c26` |
| `quay.io/prometheus/prometheus:v2.55.1` | `sha256:2659f4c2ebb718e7695cb9b25ffa7d6be64db013daba13e05c875451cf51b0d3` | `sha256:f4def6b3b61109a6eeea59945d578bb7e926c36cb0e036a23e3ceb8b6de024ad` (`linux/arm64/v8`) |

这与 release 源码一致：Operator 的发布脚本为 `amd64 arm64 arm ppc64le s390x` 构建，[架构列表](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/scripts/push-docker-image.sh#L1-L20)，然后合成并签名官方 manifest，[manifest 发布](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/scripts/push-docker-image.sh#L84-L113)；Prometheus `v2.55.1` 的 Docker 构建列表包含 `arm64`。[Makefile](https://github.com/prometheus/prometheus/blob/6d7569113f1ca814f1e149f74176656540043b8d/Makefile#L14-L16)

registry 核对命令为：

```bash
docker buildx imagetools inspect quay.io/prometheus-operator/prometheus-operator:v0.83.0
docker buildx imagetools inspect quay.io/prometheus-operator/prometheus-config-reloader:v0.83.0
docker buildx imagetools inspect quay.io/prometheus/prometheus:v2.55.1
```

测试配置至少应固定 tag；若镜像供应链要求完全可复现，可进一步固定上表的 multi-arch index digest。`PrometheusAgent.spec.image` 使用 digest 时仍必须同时写 `spec.version: v2.55.1`，否则 Operator 无法按正确版本生成配置。[image/version semantics](https://github.com/prometheus-operator/prometheus-operator/blob/5cf2f5d37906d64b2259a262e319df4c74639e31/pkg/apis/monitoring/v1/prometheus_types.go#L204-L227)

## 对真实集群的只读验证

验证时间：2026-09-23。

| context | API Server | 节点平台 | 当前 Prometheus Operator / CRD | `v0.83.0 bundle.yaml` server dry-run |
| --- | --- | --- | --- | --- |
| `kind-k8s-1-19` | `v1.19.16` | `linux/arm64` | 未安装 | 10 个 CRD、ClusterRole、ClusterRoleBinding、Deployment、ServiceAccount、Service 均通过 |
| `kind-k8s-1-28` | `v1.28.15` | `linux/arm64` | 未安装 | 同上，均通过 |

执行的是不落盘的 API Server 校验：

```bash
kubectl --context kind-k8s-1-19 create --dry-run=server -f bundle.yaml
kubectl --context kind-k8s-1-28 create --dry-run=server -f bundle.yaml
```

这证明安装物 schema 能被两个当前 API Server 接受；它不证明 controller 能成功启动、watch、生成配置或完成采集。

## 必须保留的兼容性门禁

安装后的双集群 smoke test 至少需要验证：

1. Operator 与 config-reloader ARM64 镜像成功拉取，Operator Ready。
2. 五个必需 CRD 的上述 API version 均为 `served: true`，并检查实际 schema 存在 selectors、`replicas`、`shards`、`remoteWrite` 与将使用的 queue 字段。
3. 创建最小 `PrometheusAgent`，明确写入 `spec.version: v2.55.1` 和非空 label selector；四类采集配置各做一份匹配、一份不匹配。
4. Kubernetes service discovery 在 `v1.19.16`、`v1.28.15` 上都能发现并抓取真实目标；这一步覆盖 Operator `client-go v0.33.1` 与 Prometheus `client-go v0.31.0` 的版本偏差风险。
5. `shards=2` 的目标分配、多个 remote write 目标、队列参数和失败重试都应实际验证；只看 StatefulSet/Pod Ready 不足以接受基线。

## 未解决事实

- 上游没有给出 `Prometheus Operator v0.83.0 + Kubernetes v1.19.16` 或 `Prometheus v2.55.1 Kubernetes SD + Kubernetes v1.19.16` 的精确 E2E 保证；现有官方事实只能支持“最低 Kubernetes 版本声明允许、跨 client-go minor 为 best effort”。
- 本次没有把 bundle 真正安装进集群，也没有做 runtime smoke test；只完成了两个 API Server 的 server-side dry-run，因此该组合仍须通过上述验收才能升级为本项目支持基线。
- `PrometheusAgent`、`ScrapeConfig` 的 Alpha API 风险无法通过选取 `<v0.84.0` 的其他版本消除；一期必须接受并锁定 schema。
