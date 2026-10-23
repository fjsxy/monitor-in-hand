# ServiceMonitor API 测试夹具

`monitoring.coreos.com_servicemonitors.yaml` 固定来自 Prometheus Operator `v0.83.0` 的 [官方 CRD](https://github.com/prometheus-operator/prometheus-operator/blob/v0.83.0/example/prometheus-operator-crd/monitoring.coreos.com_servicemonitors.yaml)。SHA256：`015d587a7d248c36d4d18fca87b92d2a82b5811371e361c2e5574f0a7d917acb`。

仅用于 envtest 和独立 kind 场景，先验证可选 API 缺失，再显式安装此固定 API。产品 Helm 不打包或安装该外部 CRD。其来源项目采用 Apache-2.0；升级夹具时需同步核对本仓库固定 Kubernetes 版本与测试环境基线。
