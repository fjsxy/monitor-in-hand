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

## 部署预检

在创建原生 `PrometheusAgent` 前，管理员必须用实际 Agent 计划使用的 namespace 和 runtime ServiceAccount 运行严格预检：

```bash
hack/preflight.sh \
  --agent-namespace monitoring \
  --agent-service-account prometheus-agent \
  --config-namespaces monitoring,shared-monitoring \
  --discovery-namespaces default,applications \
  --operator-namespace prometheus-operator \
  --operator-deployment prometheus-operator
```

`--config-namespaces` 是四类采集配置可能被选中的 namespace；`--discovery-namespaces` 是 Agent 一期 Kubernetes discovery 可能读取的 namespace。两者均接受逗号分隔列表，或单独的 `*` 表示要求覆盖全部 namespace。默认查找 `prometheus-operator/prometheus-operator` Deployment；可用 `--context` 选择 kubeconfig context，用 `--timeout-seconds` 调整 1–900 秒的 reconcile 上限。完整输入见：

```bash
hack/preflight.sh --help
```

`make preflight` 是便利包装，参数分别为 `AGENT_NAMESPACE`、`AGENT_SERVICE_ACCOUNT`、`CONFIG_NAMESPACES`、`DISCOVERY_NAMESPACES`、`OPERATOR_NAMESPACE`、`OPERATOR_DEPLOYMENT`、`KUBE_CONTEXT` 和 `PREFLIGHT_TIMEOUT_SECONDS`。GNU Make 只保留成功/失败；需要区分精确退出码的自动化应直接调用 `hack/preflight.sh`。

预检依次验证 Kubernetes 1.19.x/1.28.x 版本事实、五类 CRD/API/schema、Prometheus Operator watch scope 与 RBAC、非 `default` runtime ServiceAccount 的 discovery RBAC，以及最小 reconcile。最后一步仅创建一个名为 `mih-preflight-*` 的零副本 Agent；无论成功或失败，都使用 UID precondition 仅删除该 Agent，由 ownerReference 前台级联清理它的外部生成资源。预检不安装、升级、修改或接管 Prometheus Operator 及采集配置，也不会让可观测组件管控 Operator 依赖 Prometheus Operator。

标准输出是 JSON Lines：每行包含 `level`、`check`、`status`、`reason`、`message` 和 `details`。退出码为 `0` 表示通过，`1` 表示环境不具备支持资格或检查无法完成，`2` 表示输入无效。任一失败都是阻断性的，不会静默降级。

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
