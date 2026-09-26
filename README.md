# monitor-in-hand

面向单个 Kubernetes 集群的可观测组件管控 Operator。当前提供可安装、可探活的控制面，以及 cluster-scoped `KubeStateMetrics` API 的默认单副本 Deployment 调谐。

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

`make test` 会下载固定的 Kubernetes 1.28 envtest 资产并执行 controller integration tests。默认 Deployment 的真实集群验收复用双版本测试环境：

```bash
make e2e-default-deployment KUBERNETES_VERSION=1.19 TEST_RUN_ID=local-119 HELM=helm
```

该命令需要对应 `test-env-up` 创建的集群以及可用的 Docker socket；CI 在 Kubernetes 1.19 与 1.28 两个 blocking matrix legs 中运行它。

`make smoke-install` 创建独立的临时 kind v1.28.15 集群，构建并加载本地镜像，验证 Deployment、leader-election Lease、`/healthz`、`/readyz`、JSON 日志和单实例安装约束，完成后删除该临时集群。传入 `KIND_NODE_IMAGE` 可验证其他目标版本。

## 双版本外部测试环境

一期外部测试环境通过相同 fixture 分别创建 Kubernetes `v1.19.16` 与 `v1.28.15` 独立 kind 集群。每次运行必须提供唯一、最长 32 字符的 DNS label 作为 `TEST_RUN_ID`：

```bash
make test-env-up KUBERNETES_VERSION=1.19 TEST_RUN_ID=local-119
make test-env-verify KUBERNETES_VERSION=1.19 TEST_RUN_ID=local-119
make test-env-down KUBERNETES_VERSION=1.19 TEST_RUN_ID=local-119
```

`test-env-up` 安装并等待以下固定基线：

- Prometheus Operator 与 config-reloader `v0.83.0`
- Prometheus `v2.55.1`
- VictoriaMetrics Cluster `v1.152.0`
- node-exporter `v1.12.0` 测试 target
- blackbox-exporter `v0.28.0` Probe prober

所有容器使用 multi-arch digest；Prometheus Operator 使用官方完整 `bundle.yaml`，下载后必须匹配仓库记录的 SHA-256。固定版本、digest 与 node image 位于 `test/fixtures/testenv/versions.env`。

`test-env-verify` 从 Kubernetes 公共接口验证精确 server patch、Linux 节点架构与 Ready、API ready、Operator watch scope/RBAC、五类 CRD 的 served version 和所需 schema、Agent runtime RBAC、最小 `PrometheusAgent` 的 `Reconciled`/`Available` conditions、生成 StatefulSet 的固定镜像，以及 VictoriaMetrics、target 和 prober endpoints。

清理前会核对 `kube-system/monitor-in-hand-test-run` 中的 test-run 身份和 Kubernetes minor；不匹配时拒绝删除。失败后先运行以下命令保存诊断，再清理：

```bash
make test-env-evidence KUBERNETES_VERSION=1.19 TEST_RUN_ID=local-119
```

证据写入 `artifacts/test-env/`。CI 的两个 matrix legs 独立并行，单个集群内的有状态步骤保持串行；失败时先上传证据，再按身份清理集群。

## KubeStateMetrics 最小声明

`targetNamespace` 必须预先存在；Operator 不创建或删除该 Namespace。省略其他字段时，API 默认启用实例、不挂起，并使用固定单副本 Deployment：

```yaml
apiVersion: monitoring.fjsxy.github.io/v1alpha1
kind: KubeStateMetrics
metadata:
  name: default
spec:
  targetNamespace: monitoring
```

Kubernetes 1.19.x 固定选择 kube-state-metrics 2.3.0，Kubernetes 1.28.x 固定选择 2.11.0。其他 minor 或缺失目标 Namespace 只更新 CR status，不创建受管资源。

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
