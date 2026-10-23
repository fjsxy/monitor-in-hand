# 由 exporter Operator 管理可选配套 ServiceMonitor

2026-10-23 根据 monitor-mgr#23 与本仓库 #86，配套采集配置和 exporter 共享生命周期，统一由 monitor-op 持续调谐。平台只声明 CR 中的采集意图并关联原生 Agent，不复制 Service 标签、命名或端口规则，不直接写配套资源。此决定仅修订旧规格 #48 对本实例配套 ServiceMonitor 的边界，其他原生采集实例与独立采集配置仍为外部资源。

为保证未安装可选 CRD 时仍可启动并管理纯 exporter，采用动态客户端直读与有界周期重查，不向 manager 注册可选 informer；以额外 API 读取换取缺 CRD/权限时的故障隔离和依赖恢复后无需重启。只按当前 CR UID 的 ownerReference 修改资源，独立回报配套配置观察，保留 workload 可用性语义。
