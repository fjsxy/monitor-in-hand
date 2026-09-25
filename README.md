# monitor-in-hand

面向单个 Kubernetes 集群的可观测组件管控 Operator。本阶段只提供可安装、可探活的控制面，不包含 `KubeStateMetrics` 业务调谐。

发布镜像位于 Docker Hub：`docker.io/fjsxy/monitor-in-hand:<version>`。Chart 使用 `appVersion` 选择固定镜像 tag。

## 工程脚手架

工程基线由 Kubebuilder `v3.13.0` 的 `go/v4` plugin 生成，初始化参数固定为：

```bash
kubebuilder init \
  --domain fjsxy.github.io \
  --repo github.com/fjsxy/monitor-in-hand \
  --project-name monitor-in-hand \
  --plugins go/v4 \
  --license none
```

该版本的 scaffold 使用 Kubernetes `v0.28` 与 `controller-runtime v0.16` 依赖线；项目只在同一依赖线内升级 patch 版本。`PROJECT`、`cmd/main.go` 中的 scaffold markers、`Makefile` 与 `config/` 均来自该生成基线并按运行契约定制。正式安装入口只有 Helm；`make deploy` 也只会转发到 Helm 安装目标。

## 开发验证

需要 Go 1.22、Helm 3、Docker、kind、kubectl、jq 与 curl。常用入口：

```bash
make typecheck
make test
make lint-chart
make verify-generated
make smoke-install
```

`make smoke-install` 创建独立的临时 kind v1.28.15 集群，构建并加载本地镜像，验证 Deployment、leader-election Lease、`/healthz`、`/readyz`、JSON 日志和单实例安装约束，完成后删除该临时集群。传入 `KIND_NODE_IMAGE` 可验证其他目标版本。

## Helm values

Chart 只开放以下配置：

- `resources`
- `nodeSelector`
- `affinity`
- `tolerations`
- `topologySpreadConstraints`
- `healthProbe.port`
- `logLevel`

Operator 镜像、单副本、leader election、滚动升级策略、安全上下文与 probe 时序均为固定运行契约。
