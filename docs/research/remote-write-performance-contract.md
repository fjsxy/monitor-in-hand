# PrometheusAgent `remote_write` 与采集性能契约调研

## 1. 范围与结论

本文只核实以下固定组合：

- Prometheus Operator `v0.83.0`（peeled commit `5cf2f5d37906d64b2259a262e319df4c74639e31`）；
- Prometheus `v2.55.1`（peeled commit `6d7569113f1ca814f1e149f74176656540043b8d`），以 Agent mode 运行；
- 一期 remote backend 为同一 Kubernetes 集群内、无认证的 VictoriaMetrics Cluster；
- VictoriaMetrics 未指定固定版本，因此其能力以 2026-09-25 可访问的官方文档为依据，属于需要在测试环境再次确认的外部依赖。

最重要的事实是：

1. `PrometheusAgentSpec` 内嵌 `CommonPrometheusFields`，因此原生 `spec.remoteWrite` 是 `[]RemoteWriteSpec`，天然允许多个目标；项目不需要再包装一层 CR 才能表达多个 remote backend。[PrometheusAgentSpec](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/apis/monitoring/v1alpha1/prometheusagent_types.go#L92-L105) [CommonPrometheusFields.remoteWrite](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/apis/monitoring/v1/prometheus_types.go#L439-L445)
2. VictoriaMetrics Cluster 的 Prometheus Remote Write v1 标准写入地址为 `http://<vminsert>:8480/insert/<accountID>/prometheus/api/v1/write`；单租户通常使用 `accountID=0`。[VictoriaMetrics API examples](https://docs.victoriametrics.com/victoriametrics/url-examples/#apiv1write) [VMCluster Services and URLs](https://docs.victoriametrics.com/operator/resources/vmcluster/#services-and-urls)
3. VictoriaMetrics 官方当前明确写明 Prometheus Remote Write 2.0 尚不支持；一期应省略 `messageVersion`（Prometheus 2.55.1 默认 V1）或显式使用 `V1.0`，不能使用 `V2.0`。[VictoriaMetrics Prometheus integration](https://docs.victoriametrics.com/victoriametrics/integrations/prometheus/) [Prometheus v2.55.1 default](https://github.com/prometheus/prometheus/blob/v2.55.1/config/config.go#L186-L193)
4. 每个成功加载的 `remote_write` 条目拥有独立的 client、queue manager、WAL watcher、重试状态与带 `remote_name`/`url` 标签的指标；一个远端运行时失败不会停止其他队列发送，但所有队列共享同一 Agent WAL，最慢目标会影响 WAL 截断进度。[WriteStorage.ApplyConfig](https://github.com/prometheus/prometheus/blob/v2.55.1/storage/remote/write.go#L137-L236) [LowestSentTimestamp](https://github.com/prometheus/prometheus/blob/v2.55.1/storage/remote/write.go#L247-L264)
5. 远端不可达不会直接改变 `PrometheusAgent.status`。该 status 只汇总 Operator 最近一次 reconcile 结果、StatefulSet/Pod 数量和 Pod readiness；Prometheus 的 readiness probe 只检查 `/-/ready`，并不检查所有 remote backend 是否成功写入。[StatusReporter.Process](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/prometheus/operator.go#L138-L248) [Agent probes](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/prometheus/promcfg.go#L1202-L1239)
6. `RemoteWriteSpec` 中的 Secret/ConfigMap selector 没有 namespace 字段。Operator 始终用 `PrometheusAgent` 自身 namespace 读取这些引用；不能跨 namespace 引用。[Agent store call](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/prometheus/agent/operator.go#L858-L868) [asset lookup](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/assets/store.go#L222-L301)
7. Operator API 中仍存在 `queueConfig.maxRetries`，但生成配置时只允许 Prometheus `<=2.11.0`；固定版本 `2.55.1` 下该字段不会出现在生成配置中，因此对本项目无效。[Operator translation](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/prometheus/promcfg.go#L2752-L2796)

## 2. VictoriaMetrics Cluster 目标契约

### 2.1 URL 与 tenant

推荐按完整路径表达一期目标：

```text
http://<vminsert-service>.<namespace>.svc:8480/insert/<accountID>/prometheus/api/v1/write
```

官方定义如下：

- `vminsert` 默认 HTTP 端口为 `8480`；Kubernetes Operator 创建的主 `vminsert-<name>` Service 是 `ClusterIP`。[VMCluster Services and URLs](https://docs.victoriametrics.com/operator/resources/vmcluster/#services-and-urls)
- Cluster 写入路径格式为 `/insert/<accountID>/<suffix>`；Prometheus Remote Write 的 suffix 可为 `prometheus` 或 `prometheus/api/v1/write`。为便于审计和与官方 API 示例一致，本文使用完整的 `prometheus/api/v1/write`。[VictoriaMetrics Cluster URL format](https://docs.victoriametrics.com/victoriametrics/cluster-victoriametrics/#url-format)
- `accountID` 是标识 tenant 的任意 32-bit 整数；也可写为 `accountID:projectID`，未指定 `projectID` 时为 `0`。单租户示例使用 `0`。[VictoriaMetrics Cluster URL format](https://docs.victoriametrics.com/victoriametrics/cluster-victoriametrics/#url-format)
- URL 中的 tenant 决定数据写入哪个逻辑 namespace；Prometheus 的 external labels 不能替代 URL tenant。

### 2.2 HTTP、HTTPS 与认证边界

VictoriaMetrics Cluster 默认假设内部组件运行在受保护的私有网络中，`vminsert` 默认在 `8480` 接受 HTTP；面向不可信网络时，官方建议在前面部署 `vmauth`/其他 auth proxy，并通过 HTTPS 接收认证 token。Enterprise 版本还可以直接为组件启用 TLS/mTLS。[VictoriaMetrics Cluster security](https://docs.victoriametrics.com/victoriametrics/cluster-victoriametrics/#security) [VictoriaMetrics Cluster mTLS](https://docs.victoriametrics.com/victoriametrics/cluster-victoriametrics/#mtls-protection)

因此，对“同集群、无认证”的一期边界：

- 可使用 `ClusterIP` Service 上的明文 `http://...:8480`；
- 该做法成立的前提是集群网络被视为受保护边界，且 `vminsert` 不直接暴露到不可信网络；
- 原生 `RemoteWriteSpec` 的认证、TLS、proxy 字段仍记录在本文，但不应纳入一期受支持配置；
- 如果未来流量跨越不可信网络，必须重新设计为 `vmauth`/网关、HTTPS 和认证，而不是沿用一期的无认证 URL。

### 2.3 协议版本

Prometheus Operator `v0.83.0` 能把 `messageVersion: V1.0|V2.0` 转成 Prometheus 的 `prometheus.WriteRequest` 或 `io.prometheus.write.v2.Request`；配置 V2 时还会为 Prometheus `>=2.54.0` 自动加 `--enable-feature=metadata-wal-records`。[message translation](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/prometheus/promcfg.go#L2595-L2631) [feature flag translation](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/prometheus/promcfg.go#L1135-L1139)

但 VictoriaMetrics 官方当前说明 Remote Write 2.0 仍不受支持。[VictoriaMetrics Prometheus integration](https://docs.victoriametrics.com/victoriametrics/integrations/prometheus/)

所以固定组合的有效边界是：

```yaml
spec:
  remoteWrite:
    - name: vm-primary
      url: http://vminsert-<name>.<namespace>.svc:8480/insert/0/prometheus/api/v1/write
      messageVersion: V1.0 # 也可省略；Prometheus 2.55.1 默认 V1
```

### 2.4 健康与成功验证

单看 Kubernetes Service 存在或 `PrometheusAgent.status.Available=True` 都不能证明数据已成功写入。建议测试分四层：

1. **vminsert HTTP readiness**：请求 `http://<vminsert>:8480/-/ready`，成功时返回 `VictoriaMetrics is Ready.`；`/health` 也提供组件健康状态，关机 grace period 会返回非 OK。[VictoriaMetrics health endpoints](https://docs.victoriametrics.com/victoriametrics/url-examples/#health)
2. **Prometheus 发送队列**：检查指定 `remote_name`/`url` 的 `prometheus_remote_storage_samples_total` 持续增长，`samples_failed_total`、`samples_retried_total`、`samples_pending` 和发送时间戳 lag 符合预期。
3. **VictoriaMetrics 接收量**：观察 `vm_vminsert_metrics_read_total` 的增长；官方容量规划文档将其作为包含 replication 的 ingestion rate 指标。[VictoriaMetrics sizing](https://docs.victoriametrics.com/guides/understand-your-setup-size/#ingestion-rate)
4. **tenant 内端到端查询**：经 `vmselect` 对同一 `accountID` 调用 `http://<vmselect>:8481/select/<accountID>/prometheus/api/v1/query`，查询一条带可识别 label 的测试序列。[VictoriaMetrics query API](https://docs.victoriametrics.com/victoriametrics/url-examples/#apiv1query)

`/-/ready` 只能证明 vminsert HTTP 服务 ready；第 2、3、4 层才分别证明发送、接收和可查询。

## 3. 原生 `RemoteWriteSpec` 能力

完整字段定义见 [Prometheus Operator v0.83.0 RemoteWriteSpec](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/apis/monitoring/v1/prometheus_types.go#L1520-L1681)。

| 类别 | CR 字段 | 固定组合中的事实 |
| --- | --- | --- |
| 目标 | `url` | required，CRD 只声明最小长度 1；最终 URL 解析和协议配置仍由 Prometheus 加载配置时校验 |
| 标识 | `name` | 可选；非空时必须在所有 remote write 条目中唯一，并用于日志与 `remote_name` 指标标签；为空时 Prometheus 使用配置 hash 的前 6 位 |
| 协议 | `messageVersion` | `V1.0`/`V2.0`；一期 VictoriaMetrics 只能用 V1 |
| 数据类型 | `sendExemplars`、`sendNativeHistograms` | Prometheus 2.55.1 可用；默认均为 `false`，但不是一期后端契约的一部分 |
| 请求 | `remoteTimeout` | 未设置时 Prometheus 默认 `30s` |
| 请求 | `headers` | 明文 `map[string]string`，不是 Secret selector；Prometheus 自己设置的保留 header 不能覆盖 |
| 过滤 | `writeRelabelConfigs` | 每个 remote target 独立；在 external labels 加入后、发送前执行 |
| 认证 | `basicAuth`、`oauth2`、`authorization`、`sigv4`、`azureAd` | 原生支持但五者最多一个；一期全部排除 |
| 已弃用认证 | `bearerToken`、`bearerTokenFile` | 仍可表达但已弃用；前者明文出现在 CR，均不应进入一期 |
| TLS | `tlsConfig` | 支持 Secret/ConfigMap selector 或容器内文件路径；一期排除 |
| proxy | `proxyUrl`、`noProxy`、`proxyFromEnvironment`、`proxyConnectHeader` | 原生支持；一期排除 |
| HTTP | `followRedirects`、`enableHTTP2` | 未设置时 Prometheus 默认均为 `true` |
| 队列 | `queueConfig` | 见下一节 |
| metadata | `metadataConfig` | V1 的 metadata 发送配置；一期建议不承诺，见默认值陷阱 |
| DNS | `roundRobinDNS` | CRD 中存在，但 Operator 只给 Prometheus `>=3.1.0` 生成；固定 `2.55.1` 下无效 |

Prometheus v2.55.1 的配置文档列出了 remote timeout、header、认证、TLS、proxy、HTTP 与 queue 默认值。[Prometheus 2.55 configuration](https://prometheus.io/docs/prometheus/2.55/configuration/configuration/#remote_write)

### 3.1 校验发生在哪一层

不能把“字段存在于 CRD”理解为“该组合必然能运行”：

- **Kubernetes CRD schema**：保证 `url` 非空、enum、部分格式等静态约束。
- **Prometheus Operator reconcile**：
  - `basicAuth`、`oauth2`、`authorization`、`sigv4`、`azureAd` 最多一个；
  - AzureAD 必须且只能选择 `managedIdentity`、`oauth`、`sdk` 之一，OAuth `clientID` 必须是 UUID；
  - SigV4 的 `accessKey` 与 `secretKey` 必须同时提供或同时不提供；
  - TLS cert/key 配对、Secret/ConfigMap 互斥、proxy 组合与 Secret 引用会被校验。
  [Operator remote write validation](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/prometheus/operator.go#L85-L136) [asset validation](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/assets/store.go#L84-L219) [TLS validation](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/assets/tls.go#L134-L186)
- **Prometheus config load/reload**：校验 URL、保留 header、HTTP client 配置、认证互斥和 remote write `name` 唯一性；例如重复非空 `name` 会让整份新配置加载失败，而不是只忽略一个目标。[RemoteWriteConfig.UnmarshalYAML](https://github.com/prometheus/prometheus/blob/v2.55.1/config/config.go#L1142-L1227) [unique names](https://github.com/prometheus/prometheus/blob/v2.55.1/config/config.go#L377-L399)
- **远端运行时**：DNS、连接、HTTP 状态码、协议兼容和 VictoriaMetrics tenant/path 只能在实际发送时验证。

### 3.2 Secret/ConfigMap namespace

所有 `remoteWrite` 引用都以 `PrometheusAgent.metadata.namespace` 为读取范围：

- BasicAuth username/password；
- OAuth2 client ID、client secret、OAuth2 TLS/proxy secret；
- Authorization credentials；
- SigV4 access key/secret key；
- Azure OAuth client secret；
- TLS CA/cert/key；
- proxy connect headers。

`SecretKeySelector`/`ConfigMapKeySelector` 本身没有 namespace 字段，Operator 的 `AddRemoteWritesToStore(..., p.GetNamespace(), ...)` 把 Agent namespace 传给所有读取操作。[AddRemoteWritesToStore](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/prometheus/store.go#L25-L61) [Agent call site](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/prometheus/agent/operator.go#L858-L868)

因此，不存在跨 namespace 引用；Operator ServiceAccount 必须有权读取 Agent namespace 中的引用对象。

### 3.3 认证、TLS、header 与 proxy 的精确约束

虽然这些字段不进入一期支持面，原生事实如下：

- `basicAuth.username` 与 `basicAuth.password` 都是 Secret key selector；Operator 会把值展开为 Prometheus 配置中的 `basic_auth.username/password`。[BasicAuth type](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/apis/monitoring/v1/types.go#L760-L773) [rendering](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/prometheus/promcfg.go#L581-L606)
- `oauth2.clientId` 可来自 Secret 或 ConfigMap，二者互斥；`clientSecret` 只能来自 Secret；`tokenUrl` 必须非空。OAuth2 还可单独设置 scopes、endpoint params、TLS 和 proxy。[OAuth2 type/validation](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/apis/monitoring/v1/types.go#L687-L758)
- `authorization.credentials` 是 Secret selector；`credentialsFile` 是容器内文件路径，二者互斥；authorization type 不能写 `basic`，Basic Auth 必须使用 `basicAuth`。[Authorization validation](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/apis/monitoring/v1/prometheus_types.go#L2200-L2258)
- `sigv4` 可以完全不提供 key，交给 AWS default credentials chain；如果显式提供，`accessKey` 与 `secretKey` 必须同时存在。`region`、`profile`、`roleArn` 是明文字段。[SigV4 type](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/apis/monitoring/v1/prometheus_types.go#L1728-L1752) [SigV4 store validation](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/assets/store.go#L185-L205)
- `azureAd` 必须且只能选择 `managedIdentity`、`oauth`、`sdk` 中一个；OAuth client ID 由 Operator 按 UUID 解析，client secret 来自 Secret；cloud enum 为 `AzurePublic`、`AzureChina`、`AzureGovernment`。[AzureAD type](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/apis/monitoring/v1/prometheus_types.go#L1754-L1814) [Operator validation](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/prometheus/operator.go#L110-L133)
- `tlsConfig` 的 CA/cert 可选 Secret/ConfigMap selector 或容器内 file，但同一项不能同时指定 selector 和 file；client cert 与 client key 必须成对；`keySecret` 与 `keyFile` 互斥；`minVersion` 不能高于 `maxVersion`。[TLSConfig validation](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/apis/monitoring/v1/types.go#L860-L939)
- `headers` 是 CR 内明文 map。Prometheus 拒绝用它设置 `Authorization` 或覆盖自身保留 headers；需要秘密值的 proxy CONNECT header 才使用 `proxyConnectHeader` 的 Secret selectors。[Prometheus header validation](https://github.com/prometheus/prometheus/blob/v2.55.1/config/config.go#L1230-L1251) [ProxyConfig type](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/apis/monitoring/v1/types.go#L93-L124)
- `proxyConnectHeader` 非空时必须同时设置 `proxyUrl` 或 `proxyFromEnvironment=true`；`proxyFromEnvironment=true` 与 `proxyUrl`、`noProxy` 互斥。[ProxyConfig.Validate](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/apis/monitoring/v1/types.go#L125-L153)
- Operator 的“五选一”校验只枚举 `basicAuth`、`oauth2`、`authorization`、`sigv4`、`azureAd`；deprecated `bearerToken`/`bearerTokenFile` 不在这段预检中。它们仍会被直接写入生成配置，并继续受到 Prometheus config load 校验，因此不能把未被 Operator 拦截的组合视为受支持组合。[Operator auth check](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/prometheus/operator.go#L90-L108) [deprecated field rendering](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/prometheus/promcfg.go#L2680-L2698)

## 4. Queue、WAL 与性能字段

### 4.1 Prometheus 2.55.1 默认值与 Operator 转译

Prometheus 的精确默认值来自 [`DefaultRemoteWriteConfig` 与 `DefaultQueueConfig`](https://github.com/prometheus/prometheus/blob/v2.55.1/config/config.go#L186-L219)。Operator 的生成逻辑见 [`GenerateRemoteWriteConfig`](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/prometheus/promcfg.go#L2609-L2820)。

| `RemoteWriteSpec` 字段 | Prometheus 2.55.1 默认值 | Operator v0.83.0 对 2.55.1 的转译 |
| --- | ---: | --- |
| `remoteTimeout` | `30s` | 非 `nil` 才生成 `remote_timeout` |
| `followRedirects` | `true` | 非 `nil` 才生成 |
| `enableHTTP2` | `true` | 非 `nil` 才生成 |
| `queueConfig.capacity` | `10000`/shard | 非 0 才生成；CR 里的 0 等价于未指定，不能用 0 覆盖默认值 |
| `queueConfig.minShards` | `1` | 非 0 才生成 |
| `queueConfig.maxShards` | `50` | 非 0 才生成 |
| `queueConfig.maxSamplesPerSend` | `2000` | 非 0 才生成 |
| `queueConfig.batchSendDeadline` | `5s` | 非 `nil` 才生成 |
| `queueConfig.minBackoff` | `30ms` | 非 `nil` 才生成 |
| `queueConfig.maxBackoff` | `5s` | 非 `nil` 才生成 |
| `queueConfig.retryOnRateLimit` | `false` | 只有 `true` 才生成 `retry_on_http_429` |
| `queueConfig.sampleAgeLimit` | `0s`，不按年龄丢弃 | 非 `nil` 且 Prometheus `>=2.50.0` 才生成 |
| `queueConfig.maxRetries` | Prometheus 2.55.1 已无此配置 | Operator 只在 `<=2.11.0` 生成；固定版本下无效 |

恢复性错误会按 `minBackoff` 指数退避到 `maxBackoff` 并持续重试；不存在 `maxRetries` 上限。429 只有在 `retryOnRateLimit=true` 时才按恢复性错误处理。[retry loop](https://github.com/prometheus/prometheus/blob/v2.55.1/storage/remote/queue_manager.go#L1993-L2057) [Remote Write 1.0 retry semantics](https://prometheus.io/docs/specs/prw/remote_write_spec/#retries--backoff)

性能关系：

- 每个 remote target 有自己的 queue；
- queue 会按负载在 `minShards` 与 `maxShards` 之间自适应调整并发；
- shard 内存与 `shards × (capacity + maxSamplesPerSend)` 成正比；
- 官方建议 `capacity` 通常保持为 `maxSamplesPerSend` 的 3–10 倍，调大 capacity/batch 时同时关注 `maxShards`，否则可能显著增大内存；
- `prometheus_remote_storage_samples_pending` 增长时还应检查 CPU、网络和远端吞吐，而不是只继续加 shards。

来源：[Prometheus remote write tuning](https://prometheus.io/docs/practices/remote_write/)。

### 4.2 Metadata 默认值陷阱

Prometheus v2.55.1 源码默认：

- `send=true`；
- `sendInterval=1m`；
- `maxSamplesPerSend=2000`。

见 [`DefaultMetadataConfig`](https://github.com/prometheus/prometheus/blob/v2.55.1/config/config.go#L214-L219)。同 tag 的 Markdown 配置文档却写 `max_samples_per_send=500`，与源码不一致；固定二进制行为应以源码初始化值 `2000` 为准。[v2.55.1 configuration source](https://github.com/prometheus/prometheus/blob/v2.55.1/docs/configuration/configuration.md#L4052-L4064)

另一个陷阱是 Operator 的 `MetadataConfig.Send` 为非指针 bool；当整个 `metadataConfig` 缺省时，Prometheus 使用上述默认值，但写成 `metadataConfig: {}` 时 Operator 会显式生成 `send: false`。[Operator metadata translation](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/prometheus/promcfg.go#L2798-L2807)

由于一期固定 VictoriaMetrics Remote Write v1、VictoriaMetrics 版本又未固定，metadata tuning 不宜作为一期稳定承诺；应单独兼容性测试后再开放。

### 4.3 Agent WAL

Operator 原生专用字段只有 `spec.walCompression`：

- `nil` 时不生成 CLI flag，Prometheus 2.55.1 自身默认启用 WAL compression；
- `true`/`false` 时分别生成 `--storage.agent.wal-compression`/`--no-storage.agent.wal-compression`。

来源：[CommonPrometheusFields.walCompression](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/apis/monitoring/v1/prometheus_types.go#L750-L759) [Operator agent args](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/prometheus/agent/statefulset.go#L322-L345) [Prometheus CLI default](https://github.com/prometheus/prometheus/blob/v2.55.1/cmd/prometheus/main.go#L411-L434)。

Prometheus 2.55.1 Agent WAL 的源码默认值为：

- truncate frequency `2h`；
- minimum WAL time `5m`；
- maximum WAL time `4h`；
- WAL compression CLI 默认 `true`，算法默认 Snappy。

来源：[Agent WAL defaults](https://github.com/prometheus/prometheus/blob/v2.55.1/tsdb/agent/db.go#L55-L101) [Prometheus flags](https://github.com/prometheus/prometheus/blob/v2.55.1/cmd/prometheus/main.go#L411-L434)。

`storage.agent.retention.min-time`、`storage.agent.retention.max-time`、`storage.agent.wal-truncate-frequency` 没有 PrometheusAgent 专用 CR 字段；理论上可通过通用 `additionalArgs` 传入，但那是直接暴露 Prometheus CLI 的逃生口，不是 Operator 提供的稳定高层 API。[AdditionalArgs contract](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/apis/monitoring/v1/prometheus_types.go#L731-L749)

WAL 截断使用所有 remote queues 中最低的已发送时间戳，先减去 `MinWALTime`，同时以 `now-MaxWALTime` 作为不允许无限落后的下限。也就是说：

- 一个目标落后会保留共享 WAL，其他健康目标仍可继续发送；
- 落后超过最大保留窗口后，失败目标仍未发送的数据会随 WAL 截断而丢失；
- 多目标不是无限可靠复制，PVC 也不能消除最大 WAL time 的数据丢失边界。

来源：[Agent WAL truncation](https://github.com/prometheus/prometheus/blob/v2.55.1/tsdb/agent/db.go#L590-L623) [lowest queue timestamp](https://github.com/prometheus/prometheus/blob/v2.55.1/storage/remote/write.go#L247-L264)。

## 5. 多 remote target 与部分失败

### 5.1 已成功加载配置后的运行时

Prometheus 对每个 remote write 配置计算 hash，创建独立 `Client` 与 `QueueManager`；每个 QueueManager 启动自己的 shards 和 WAL watcher。[WriteStorage.ApplyConfig](https://github.com/prometheus/prometheus/blob/v2.55.1/storage/remote/write.go#L137-L236) [QueueManager.Start](https://github.com/prometheus/prometheus/blob/v2.55.1/storage/remote/queue_manager.go#L922-L942)

由此可得：

- 每个目标都会收到一份经过该目标 `writeRelabelConfigs` 处理的数据；这叫复制，不是 failover；
- 目标 A 的网络/5xx/429 重试在 A 的队列内进行，不会直接停止目标 B 的 worker；
- 每个目标独立产生 pending/retry/failure/lag 指标；
- 队列虽独立，WAL 文件和其截断边界仍共享；
- Prometheus 不提供“至少一个目标成功即整体成功”的聚合状态。

### 5.2 配置阶段不是部分成功

如果新配置中任一条目存在非法 URL、重复非空 `name`、重复完整配置、认证冲突或其他 Prometheus 配置错误，整个新配置加载/应用失败，不会只禁用错误条目再加载其余条目。[config validation](https://github.com/prometheus/prometheus/blob/v2.55.1/config/config.go#L377-L399) [duplicate configs](https://github.com/prometheus/prometheus/blob/v2.55.1/storage/remote/write.go#L148-L185)

所以必须区分：

- **配置错误**：新配置整体不能生效；
- **配置已加载后的远端运行时错误**：按 target 独立重试/失败，其他 target 继续工作。

为便于逐目标观测，多个条目应配置稳定且唯一的 `name`；否则指标中的 `remote_name` 是由配置 hash 生成的短值，配置变化后不稳定。

## 6. 原生可观察面

### 6.1 `PrometheusAgent.status`

`PrometheusStatus` 只包含：

- `paused`；
- `replicas`、`updatedReplicas`、`availableReplicas`、`unavailableReplicas`；
- `conditions`；
- 每 shard 的 Pod 汇总、`shards` 与 Pod selector。

没有 remote target、queue、最后发送时间、错误数或 backend health 字段。[PrometheusStatus](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/apis/monitoring/v1/prometheus_types.go#L1268-L1301)

`Reconciled` 只反映 Operator 最近一次 reconcile 是否成功；`Available` 由 StatefulSet 是否存在及 ready Pods 数量决定。[condition semantics](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/apis/monitoring/v1/types.go#L225-L281) [StatusReporter](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/prometheus/operator.go#L138-L248)

因此：

- Secret 缺失、认证字段冲突等导致 Operator reconcile 失败时，`Reconciled=False` 且 message 包含错误；
- 配置已加载后 VictoriaMetrics 不可达时，Operator 不读取 Prometheus queue health，不会据此把 `Reconciled` 或 `Available` 置为 false；
- 远端故障期间只要 Prometheus 进程的 `/-/ready` 仍成功，Pod 仍可 Ready，`Available=True` 仍可能成立。

### 6.2 Prometheus 自身 metrics 与 logs

Prometheus 2.55.1 的 remote storage 指标均带常量标签：

- `remote_name`：显式 `name`，或配置 hash 前 6 位；
- `url`：去除 URL password 后的 endpoint。

主要指标：

| 目的 | 指标 |
| --- | --- |
| 成功发送 | `prometheus_remote_storage_samples_total`、`bytes_total` |
| 不可恢复失败 | `prometheus_remote_storage_samples_failed_total` |
| 恢复性重试 | `prometheus_remote_storage_samples_retried_total` |
| 丢弃 | `prometheus_remote_storage_samples_dropped_total{reason=...}` |
| backlog | `prometheus_remote_storage_samples_pending` |
| lag | `prometheus_remote_storage_queue_highest_sent_timestamp_seconds`，与当前时间或输入最高时间对比 |
| 并发 | `prometheus_remote_storage_shards`、`shards_desired`、`shards_min`、`shards_max` |
| queue 满 | `prometheus_remote_storage_enqueue_retries_total` |
| 请求耗时 | `prometheus_remote_storage_sent_batch_duration_seconds` |

完整定义见 [queue manager metrics](https://github.com/prometheus/prometheus/blob/v2.55.1/storage/remote/queue_manager.go#L98-L317)。运行时发送错误也由对应 QueueManager 写 Prometheus 日志，并带 remote name/URL 上下文。[send retry loop](https://github.com/prometheus/prometheus/blob/v2.55.1/storage/remote/queue_manager.go#L1993-L2057)

另应观测：

- `prometheus_config_last_reload_successful`；
- `prometheus_config_last_reload_success_timestamp_seconds`。

它们用于区分“新配置没有加载”与“配置已加载但远端写失败”。[config reload metrics](https://github.com/prometheus/prometheus/blob/v2.55.1/cmd/prometheus/main.go#L86-L96)

### 6.3 Prometheus Operator metrics、logs 与 Events

Operator 侧：

- reconcile 失败会写 operator error log、递增 `prometheus_operator_reconcile_errors_total`，并进入 `Reconciled=False` message；这些 reconcile metrics 没有 Agent name/namespace 标签，只是 controller 聚合计数。[reconcile loop](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/operator/resource_reconciler.go#L533-L560) [reconcile metrics](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/operator/resource_reconciler.go#L107-L143)
- Operator 不探测远端 endpoint，因此 VictoriaMetrics 运行时故障不会产生 Operator remote-write Event 或专门 metric。
- v0.83.0 的 `InvalidConfiguration` Warning Events 用于被拒绝的 ServiceMonitor、PodMonitor、Probe、ScrapeConfig 等被选择资源；remote write validate/store 失败走 reconcile error path，源码中没有针对 PrometheusAgent remote write 发 Event 的路径。[resource events](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/prometheus/resource_selector.go#L128-L141) [Agent remote store error path](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/prometheus/agent/operator.go#L858-L868)

所以一期“明确错误信息”应理解为：配置/引用错误看 Agent `Reconciled` message 和 Operator logs；运行时远端错误看 Prometheus logs/metrics。不能承诺 Kubernetes Event 覆盖 remote backend 故障。

## 7. 敏感信息的落点

虽然一期无认证，原生链路仍需要明确：

1. Operator 用自身权限从 Agent namespace 的源 Secret/ConfigMap 读取值。
2. BasicAuth、OAuth2、Authorization、SigV4、Azure OAuth 和 proxy header 的 secret value 会被展开为生成的 Prometheus YAML；Operator 将 gzip 后的 YAML 放入名为 `prom-agent-<agent-name>` 的 Kubernetes Secret，key 为 `prometheus.yaml.gz`。[credential rendering](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/prometheus/promcfg.go#L581-L698) [configuration Secret](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/prometheus/common.go#L116-L176)
3. TLS CA/cert/key 作为单独的 sharded TLS assets Secret 管理，生成配置引用容器内路径。[TLS asset collection](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/assets/tls.go#L162-L186) [TLS Secret name](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/prometheus/common.go#L148-L154)
4. config-reloader 把替换后的配置写到 memory-backed `config-out` EmptyDir，以避免把敏感值写到节点磁盘；原始生成配置仍存在 Kubernetes Secret 中。[config-out tmpfs](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/prometheus/common.go#L201-L223)
5. Prometheus `/api/v1/status/config` 使用解析后配置的 `String()`；`config.Secret` 在 YAML marshal 时显示 `<secret>`，不会返回真实认证值。[status config API](https://github.com/prometheus/prometheus/blob/v2.55.1/web/api/v1/api.go#L1554-L1558) [secret redaction test](https://github.com/prometheus/prometheus/blob/v2.55.1/config/config_test.go#L1565-L1580) [prometheus/common Secret](https://github.com/prometheus/common/blob/v0.59.1/config/config.go#L25-L44)

这不意味着凭据不落地：能 `get` 生成配置 Secret、进入 Agent container 或读取其 mounted files 的主体仍可取得真实值。无认证的一期应完全不创建这些 credential 字段，避免无意义地扩大秘密传播面。

## 8. External labels 与 write relabel 边界

Operator 默认给所有发往外部系统的序列增加：

- `prometheus=<agent-namespace>/<agent-name>`；
- `prometheus_replica=<pod-name>`。

这两个 label 名可通过 `replicaExternalLabelName`、`prometheusExternalLabelName` 改名，设为空字符串可关闭；同名 `externalLabels` 会被忽略，默认实例/副本 label 优先。[API contract](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/apis/monitoring/v1/prometheus_types.go#L273-L321) [generation](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/prometheus/promcfg.go#L701-L734)

`writeRelabelConfigs` 的边界：

- 在 external labels 加入之后执行；
- 每个 remote target 独立配置；
- 可以筛掉整条 series，也可以修改/删除 label；
- 因而能删除默认 `prometheus`/`prometheus_replica` label，但这会改变多实例、replica 在 VictoriaMetrics 中的序列身份；
- 它只影响发送到该 remote target 的数据，不改变 scrape target 选择和抓取本身。

来源：[Prometheus v2.55 remote_write](https://prometheus.io/docs/prometheus/2.55/configuration/configuration/#remote_write)。

多个 PrometheusAgent、replica 或 shard 写入同一 VictoriaMetrics tenant 时，external labels 只是序列身份信息，不是 remote-write 层的 leader election、claim 或 deduplication 协议。去重策略需要与 VictoriaMetrics 单独约定，不能从 `PrometheusAgent.status` 或 `remoteWrite.name` 推导。

## 9. 对一期支持面的事实约束

以下不是新增 CRD，而是基于固定后端与版本组合得到的最小可测试边界，供 Wayfinder 决策：

1. remote backend 只接受同集群 VictoriaMetrics Cluster `vminsert` Service；URL 必须包含显式 `accountID` 和 `/prometheus/api/v1/write`。
2. 只承诺 Remote Write V1；省略 `messageVersion` 或设为 `V1.0`。`V2.0` 明确不支持。
3. 一期不承诺 `basicAuth`、`oauth2`、`authorization`、`sigv4`、`azureAd`、deprecated bearer token、`tlsConfig`、custom headers、proxy 和 file-based credentials。
4. 多目标可支持，但每个目标应有稳定且唯一的 `name`；配置错误是整体失败，运行时失败才是逐队列隔离。
5. 可稳定映射到 Prometheus 2.55.1 的性能字段是 `remoteTimeout`、`capacity`、`minShards`、`maxShards`、`maxSamplesPerSend`、`batchSendDeadline`、`minBackoff`、`maxBackoff`、`retryOnRateLimit`、`sampleAgeLimit`；`maxRetries` 必须排除。
6. WAL compression 可保留原生字段；Agent WAL retention 只能通过 `additionalArgs` 触达，不应误写为 Operator 稳定字段。
7. backend runtime health 必须以 Prometheus per-target metrics/logs 和 VictoriaMetrics end-to-end query 验收；`PrometheusAgent.status` 与 Kubernetes Events 不提供这一层健康结论。
8. 如果允许 `writeRelabelConfigs`，验收必须覆盖它在 external labels 之后执行、按 remote target 独立生效；去重和 external label 最终策略应由多实例隔离决策另行确定。

## 10. 一手资料索引

- [Prometheus Operator v0.83.0 `RemoteWriteSpec`](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/apis/monitoring/v1/prometheus_types.go#L1520-L1726)
- [Prometheus Operator v0.83.0 remote write config generation](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/prometheus/promcfg.go#L2609-L2820)
- [Prometheus Operator v0.83.0 remote write validation/store](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/pkg/prometheus/store.go#L25-L61)
- [Prometheus v2.55.1 remote write defaults and validation](https://github.com/prometheus/prometheus/blob/v2.55.1/config/config.go#L186-L219)
- [Prometheus v2.55.1 remote write configuration](https://prometheus.io/docs/prometheus/2.55/configuration/configuration/#remote_write)
- [Prometheus remote write tuning](https://prometheus.io/docs/practices/remote_write/)
- [Prometheus v2.55.1 queue implementation and metrics](https://github.com/prometheus/prometheus/blob/v2.55.1/storage/remote/queue_manager.go)
- [Prometheus v2.55.1 Agent WAL](https://github.com/prometheus/prometheus/blob/v2.55.1/tsdb/agent/db.go)
- [VictoriaMetrics Cluster URL format](https://docs.victoriametrics.com/victoriametrics/cluster-victoriametrics/#url-format)
- [VictoriaMetrics API examples](https://docs.victoriametrics.com/victoriametrics/url-examples/)
- [VictoriaMetrics Prometheus integration](https://docs.victoriametrics.com/victoriametrics/integrations/prometheus/)
- [VictoriaMetrics VMCluster Services and URLs](https://docs.victoriametrics.com/operator/resources/vmcluster/#services-and-urls)
