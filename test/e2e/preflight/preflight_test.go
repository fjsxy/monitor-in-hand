package preflight_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

type diagnostic struct {
	Check   string         `json:"check"`
	Status  string         `json:"status"`
	Reason  string         `json:"reason"`
	Details map[string]any `json:"details"`
}

type e2eConfig struct {
	repoRoot        string
	kubernetesMinor string
	testRunID       string
	kubeContext     string
	versionReason   string
}

var config e2eConfig

var _ = BeforeSuite(func() {
	By("读取双版本 CI 传入的真实集群身份")
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	Expect(err).NotTo(HaveOccurred())

	kubernetesMinor := os.Getenv("KUBERNETES_VERSION")
	testRunID := os.Getenv("TEST_RUN_ID")
	Expect(testRunID).To(MatchRegexp(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`), "TEST_RUN_ID 必须是非空 DNS label")

	config = e2eConfig{
		repoRoot:        root,
		kubernetesMinor: kubernetesMinor,
		testRunID:       testRunID,
		kubeContext:     "kind-mih-e2e-" + strings.ReplaceAll(kubernetesMinor, ".", "") + "-" + testRunID,
	}

	// 将 Kubernetes minor 映射到稳定场景 ID 和预检必须报告的版本事实。
	switch kubernetesMinor {
	case "1.19":
		config.versionReason = "SupportedKubernetes119"
	case "1.28":
		config.versionReason = "SupportedKubernetes128"
	default:
		Fail(fmt.Sprintf("KUBERNETES_VERSION 必须是 1.19 或 1.28，当前为 %q", kubernetesMinor))
	}
})

var _ = Describe("真实集群部署预检", Serial, func() {
	It(fmt.Sprintf("[%s] 在一期支持环境中返回成功和版本事实", stableScenarioID("001")), func(ctx SpecContext) {
		scenarioID := stableScenarioID("001")
		By(scenarioID + " 使用显式 runtime ServiceAccount 执行预检")
		output, err := runPreflight(ctx, "prometheus-agent")
		AddReportEntry("preflight-output", output, ReportEntryVisibilityFailureOrVerbose)
		Expect(err).NotTo(HaveOccurred(), output)

		By(scenarioID + " 验证版本事实和最终成功诊断")
		diagnostics := parseDiagnostics(output)
		Expect(diagnostics).To(ContainElement(SatisfyAll(
			WithTransform(func(item diagnostic) string { return item.Check }, Equal("kubernetes-version")),
			WithTransform(func(item diagnostic) string { return item.Reason }, Equal(config.versionReason)),
		)))
		Expect(diagnostics).To(ContainElement(SatisfyAll(
			WithTransform(func(item diagnostic) string { return item.Check }, Equal("summary")),
			WithTransform(func(item diagnostic) string { return item.Status }, Equal("pass")),
			WithTransform(func(item diagnostic) string { return item.Reason }, Equal("PreflightPassed")),
		)))
	}, SpecTimeout(10*time.Minute))

	It(fmt.Sprintf("[%s] 在 runtime ServiceAccount 缺失时非零退出并输出可定位原因", stableScenarioID("002")), func(ctx SpecContext) {
		scenarioID := stableScenarioID("002")
		By(scenarioID + " 对不存在的 runtime ServiceAccount 执行预检")
		output, err := runPreflight(ctx, "missing-preflight-sa")
		AddReportEntry("preflight-output", output, ReportEntryVisibilityFailureOrVerbose)
		Expect(err).To(HaveOccurred(), "缺少 runtime ServiceAccount 时预检必须非零退出")

		By(scenarioID + " 验证失败原因和最终摘要")
		diagnostics := parseDiagnostics(output)
		Expect(diagnostics).To(ContainElement(SatisfyAll(
			WithTransform(func(item diagnostic) string { return item.Check }, Equal("runtime-service-account")),
			WithTransform(func(item diagnostic) string { return item.Status }, Equal("fail")),
			WithTransform(func(item diagnostic) string { return item.Reason }, Equal("RuntimeServiceAccountNotFound")),
		)))
		Expect(diagnostics).To(ContainElement(SatisfyAll(
			WithTransform(func(item diagnostic) string { return item.Check }, Equal("summary")),
			WithTransform(func(item diagnostic) string { return item.Status }, Equal("fail")),
			WithTransform(func(item diagnostic) string { return item.Reason }, Equal("PreflightFailed")),
		)))
	}, SpecTimeout(5*time.Minute))

	It(fmt.Sprintf("[%s] 用计划 manifest 验证四类 selector、支持 discovery 与引用范围", stableScenarioID("GH59-001")), func(ctx SpecContext) {
		scenarioID := stableScenarioID("GH59-001")
		By(scenarioID + " 准备包含四类配置和六种 ScrapeConfig discovery 的计划")
		manifestDir := GinkgoT().TempDir()
		agentPath := filepath.Join(manifestDir, "agent.yaml")
		configsPath := filepath.Join(manifestDir, "configs.yaml")
		Expect(os.WriteFile(agentPath, []byte(plannedAgentManifest), 0o600)).To(Succeed())
		Expect(os.WriteFile(configsPath, []byte(plannedCollectionConfigs), 0o600)).To(Succeed())

		By(scenarioID + " 通过公开 Make 入口执行严格计划预检")
		output, err := runPreflight(ctx, "prometheus-agent",
			"AGENT_MANIFEST="+agentPath,
			"COLLECTION_CONFIG_MANIFEST="+configsPath,
			"DISCOVERY_NAMESPACES=*",
		)
		AddReportEntry("planned-preflight-output", output, ReportEntryVisibilityFailureOrVerbose)
		Expect(err).NotTo(HaveOccurred(), output)

		By(scenarioID + " 验证四类配置覆盖与最终成功诊断")
		diagnostics := parseDiagnostics(output)
		Expect(diagnostics).To(ContainElement(SatisfyAll(
			WithTransform(func(item diagnostic) string { return item.Check }, Equal("collection-configuration")),
			WithTransform(func(item diagnostic) string { return item.Reason }, Equal("CollectionConfigsSelected")),
		)))
		Expect(diagnostics).To(ContainElement(SatisfyAll(
			WithTransform(func(item diagnostic) string { return item.Check }, Equal("collection-configuration")),
			WithTransform(func(item diagnostic) string { return item.Reason }, Equal("DiscoveryNamespaceScopeCovered")),
		)))
		Expect(diagnostics).To(ContainElement(SatisfyAll(
			WithTransform(func(item diagnostic) string { return item.Check }, Equal("summary")),
			WithTransform(func(item diagnostic) string { return item.Reason }, Equal("PreflightPassed")),
		)))
	}, SpecTimeout(10*time.Minute))

	It(fmt.Sprintf("[%s] 不把上游合法的空 object selector 报告为一期支持", stableScenarioID("GH59-002")), func(ctx SpecContext) {
		scenarioID := stableScenarioID("GH59-002")
		By(scenarioID + " 准备上游合法但一期不支持的空 object selector")
		manifestPath := filepath.Join(GinkgoT().TempDir(), "agent.yaml")
		Expect(os.WriteFile(manifestPath, []byte(emptySelectorAgentManifest), 0o600)).To(Succeed())

		By(scenarioID + " 对空 object selector 执行严格计划预检")
		output, err := runPreflight(ctx, "prometheus-agent", "AGENT_MANIFEST="+manifestPath)
		AddReportEntry("empty-selector-preflight-output", output, ReportEntryVisibilityFailureOrVerbose)
		Expect(err).To(HaveOccurred(), "空 object selector 不得通过一期计划预检")

		By(scenarioID + " 验证支持资格失败原因")
		diagnostics := parseDiagnostics(output)
		Expect(diagnostics).To(ContainElement(SatisfyAll(
			WithTransform(func(item diagnostic) string { return item.Check }, Equal("collection-configuration")),
			WithTransform(func(item diagnostic) string { return item.Reason }, Equal("EmptyObjectSelectorUnsupported")),
		)))
	}, SpecTimeout(2*time.Minute))

	It(fmt.Sprintf("[%s] 通过两分片计划与已有多 owner governing Service 身份检查", stableScenarioID("GH60-001")), func(ctx SpecContext) {
		scenarioID := stableScenarioID("GH60-001")
		By(scenarioID + " 准备与既有 fixture Agent 同 namespace 的两分片计划")
		manifestPath := filepath.Join(GinkgoT().TempDir(), "agent.yaml")
		Expect(os.WriteFile(manifestPath, []byte(plannedGH60AgentManifest), 0o600)).To(Succeed())

		By(scenarioID + " 通过公开 Make 入口执行资源身份与最小 reconcile 预检")
		output, err := runPreflight(ctx, "prometheus-agent", "AGENT_MANIFEST="+manifestPath)
		AddReportEntry("gh60-identity-preflight-output", output, ReportEntryVisibilityFailureOrVerbose)
		Expect(err).NotTo(HaveOccurred(), output)

		By(scenarioID + " 验证保留名、共享 Service 和最终成功诊断")
		diagnostics := parseDiagnostics(output)
		for _, reason := range []string{"ReservedResourceNamesAvailable", "SharedGoverningServiceCompatible", "PreflightPassed"} {
			Expect(diagnostics).To(ContainElement(WithTransform(func(item diagnostic) string { return item.Reason }, Equal(reason))))
		}
	}, SpecTimeout(10*time.Minute))

	It(fmt.Sprintf("[%s] 无关对象占用保留名、共享 Service 或 PVC 时非零退出且不修改对象", stableScenarioID("GH60-002")), func(ctx SpecContext) {
		scenarioID := stableScenarioID("GH60-002")
		By(scenarioID + " 准备覆盖保留资源、共享 Service 与 retained PVC 的冲突计划")
		manifestDir := GinkgoT().TempDir()
		agentPath := filepath.Join(manifestDir, "agent.yaml")
		Expect(os.WriteFile(agentPath, []byte(conflictingGH60AgentManifest), 0o600)).To(Succeed())

		reservedConflicts := []struct {
			name, resource, objectName, manifest string
		}{
			{name: "StatefulSet", resource: "statefulset", objectName: "prom-agent-gh60-conflict", manifest: gh60ConflictStatefulSet},
			{name: "Pod", resource: "pod", objectName: "prom-agent-gh60-conflict-0", manifest: gh60ConflictPod},
			{name: "config Secret", resource: "secret", objectName: "prom-agent-gh60-conflict", manifest: gh60ConflictConfigSecret},
			{name: "TLS Secret", resource: "secret", objectName: "prom-agent-gh60-conflict-tls-assets-1", manifest: gh60ConflictTLSSecret},
			{name: "web Secret", resource: "secret", objectName: "prom-agent-gh60-conflict-web-config", manifest: gh60ConflictWebSecret},
		}
		for _, conflict := range reservedConflicts {
			By(fmt.Sprintf("%s 创建占用 %s 保留身份的无关对象", scenarioID, conflict.name))
			applyPreflightManifest(ctx, conflict.manifest)
			objectContent := resourceContent(ctx, conflict.resource, conflict.objectName, "monitor-in-hand-test")
			DeferCleanup(func(cleanupCtx SpecContext) {
				deletePreflightResource(cleanupCtx, conflict.resource, conflict.objectName, "monitor-in-hand-test")
			})

			By(fmt.Sprintf("%s 对 %s 冲突执行公开预检入口", scenarioID, conflict.name))
			output, err := runPreflight(ctx, "prometheus-agent", "AGENT_MANIFEST="+agentPath)
			AddReportEntry("gh60-"+strings.ReplaceAll(conflict.name, " ", "-")+"-output", output, ReportEntryVisibilityFailureOrVerbose)
			By(fmt.Sprintf("%s 验证 %s 冲突非零退出且原对象身份与内容快照不变", scenarioID, conflict.name))
			Expect(err).To(HaveOccurred(), output)
			Expect(parseDiagnostics(output)).To(ContainElement(WithTransform(func(item diagnostic) string { return item.Reason }, Equal("ReservedResourceConflict"))))
			Expect(resourceContent(ctx, conflict.resource, conflict.objectName, "monitor-in-hand-test")).To(Equal(objectContent))

			By(fmt.Sprintf("%s 清理 %s 冲突对象，隔离下一组保留名验证", scenarioID, conflict.name))
			deletePreflightResource(ctx, conflict.resource, conflict.objectName, "monitor-in-hand-test")
		}

		By(scenarioID + " 在独立 namespace 创建结构冲突的 prometheus-agent-operated Service")
		applyPreflightManifest(ctx, gh60ConflictServiceResources)
		DeferCleanup(func(cleanupCtx SpecContext) {
			deletePreflightResource(cleanupCtx, "namespace", "gh60-preflight-conflict", "")
		})
		serviceAgentPath := filepath.Join(manifestDir, "service-agent.yaml")
		Expect(os.WriteFile(serviceAgentPath, []byte(conflictingGH60ServiceAgentManifest), 0o600)).To(Succeed())
		serviceContent := resourceContent(ctx, "service", "prometheus-agent-operated", "gh60-preflight-conflict")
		By(scenarioID + " 对共享 Service 结构冲突执行公开预检入口")
		serviceOutput, serviceErr := runPreflight(ctx, "gh60-agent",
			"AGENT_NAMESPACE=gh60-preflight-conflict", "CONFIG_NAMESPACES=gh60-preflight-conflict",
			"DISCOVERY_NAMESPACES=gh60-preflight-conflict", "AGENT_MANIFEST="+serviceAgentPath)
		By(scenarioID + " 验证共享 Service 冲突非零退出且原对象身份与内容快照不变")
		Expect(serviceErr).To(HaveOccurred(), serviceOutput)
		Expect(parseDiagnostics(serviceOutput)).To(ContainElement(WithTransform(func(item diagnostic) string { return item.Reason }, Equal("GoverningServiceConflict"))))
		Expect(resourceContent(ctx, "service", "prometheus-agent-operated", "gh60-preflight-conflict")).To(Equal(serviceContent))

		By(scenarioID + " 等待成功预检创建的临时 Agent ownerReference 从共享 Service 完成 GC")
		waitForPreflightServiceOwnerGC(ctx, "monitor-in-hand-test")

		By(scenarioID + " 创建存储属性与 Agent 计划不匹配的同名 retained PVC")
		applyPreflightManifest(ctx, gh60ConflictPVC)
		DeferCleanup(func(cleanupCtx SpecContext) {
			deletePreflightResource(cleanupCtx, "persistentvolumeclaim", "prom-agent-gh60-pvc-db-prom-agent-gh60-pvc-0", "monitor-in-hand-test")
		})
		pvcAgentPath := filepath.Join(manifestDir, "pvc-agent.yaml")
		Expect(os.WriteFile(pvcAgentPath, []byte(conflictingGH60PVCAgentManifest), 0o600)).To(Succeed())
		pvcContent := resourceContent(ctx, "persistentvolumeclaim", "prom-agent-gh60-pvc-db-prom-agent-gh60-pvc-0", "monitor-in-hand-test")
		By(scenarioID + " 对 retained PVC 属性冲突执行公开预检入口")
		pvcOutput, pvcErr := runPreflight(ctx, "prometheus-agent", "AGENT_MANIFEST="+pvcAgentPath)
		By(scenarioID + " 验证 retained PVC 冲突非零退出且原对象身份与内容快照不变")
		Expect(pvcErr).To(HaveOccurred(), pvcOutput)
		Expect(parseDiagnostics(pvcOutput)).To(ContainElement(WithTransform(func(item diagnostic) string { return item.Reason }, Equal("RetainedPVCConflict"))))
		Expect(resourceContent(ctx, "persistentvolumeclaim", "prom-agent-gh60-pvc-db-prom-agent-gh60-pvc-0", "monitor-in-hand-test")).To(Equal(pvcContent))
	}, SpecTimeout(8*time.Minute))
})

func stableScenarioID(sequence string) string {
	ids := map[string]map[string]string{
		"1.19": {
			"001":      "E2E-119-PREFLIGHT-001",
			"002":      "E2E-119-PREFLIGHT-002",
			"GH59-001": "E2E-119-PREFLIGHT-GH59-001",
			"GH59-002": "E2E-119-PREFLIGHT-GH59-002",
			"GH60-001": "E2E-119-PREFLIGHT-GH60-001",
			"GH60-002": "E2E-119-PREFLIGHT-GH60-002",
		},
		"1.28": {
			"001":      "E2E-128-PREFLIGHT-001",
			"002":      "E2E-128-PREFLIGHT-002",
			"GH59-001": "E2E-128-PREFLIGHT-GH59-001",
			"GH59-002": "E2E-128-PREFLIGHT-GH59-002",
			"GH60-001": "E2E-128-PREFLIGHT-GH60-001",
			"GH60-002": "E2E-128-PREFLIGHT-GH60-002",
		},
	}
	if versionIDs, supported := ids[os.Getenv("KUBERNETES_VERSION")]; supported {
		if id, found := versionIDs[sequence]; found {
			return id
		}
	}
	return "E2E-UNSUPPORTED-PREFLIGHT-" + sequence
}

func runPreflight(ctx context.Context, serviceAccount string, extraMakeArgs ...string) (string, error) {
	// 始终通过项目公开 Make 入口执行预检，确保 E2E 覆盖用户实际调用路径。
	makeArgs := []string{"preflight",
		"AGENT_NAMESPACE=monitor-in-hand-test",
		"AGENT_SERVICE_ACCOUNT=" + serviceAccount,
		"CONFIG_NAMESPACES=monitor-in-hand-test",
		"DISCOVERY_NAMESPACES=monitor-in-hand-test",
		"OPERATOR_NAMESPACE=prometheus-operator",
		"OPERATOR_DEPLOYMENT=prometheus-operator",
		"KUBE_CONTEXT=" + config.kubeContext,
		"PREFLIGHT_TIMEOUT_SECONDS=180"}
	makeArgs = append(makeArgs, extraMakeArgs...)
	command := exec.CommandContext(ctx, "make", makeArgs...)
	command.Dir = config.repoRoot
	output, err := command.CombinedOutput()
	return string(output), err
}

const plannedAgentManifest = `
apiVersion: monitoring.coreos.com/v1alpha1
kind: PrometheusAgent
metadata: {name: planned, namespace: monitor-in-hand-test}
spec:
  serviceAccountName: prometheus-agent
  serviceMonitorSelector: {matchLabels: {monitoring.fjsxy.github.io/gh59: selected}}
  podMonitorSelector: {matchLabels: {monitoring.fjsxy.github.io/gh59: selected}}
  probeSelector: {matchLabels: {monitoring.fjsxy.github.io/gh59: selected}}
  scrapeConfigSelector: {matchLabels: {monitoring.fjsxy.github.io/gh59: selected}}
`

const plannedCollectionConfigs = `
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata: {name: planned, namespace: monitor-in-hand-test, labels: {monitoring.fjsxy.github.io/gh59: selected}}
spec: {selector: {}, endpoints: [{port: metrics}]}
---
apiVersion: monitoring.coreos.com/v1
kind: PodMonitor
metadata: {name: planned, namespace: monitor-in-hand-test, labels: {monitoring.fjsxy.github.io/gh59: selected}}
spec: {selector: {}, podMetricsEndpoints: [{port: metrics}]}
---
apiVersion: monitoring.coreos.com/v1
kind: Probe
metadata: {name: planned, namespace: monitor-in-hand-test, labels: {monitoring.fjsxy.github.io/gh59: selected}}
spec: {prober: {url: blackbox-exporter:9115}, targets: {staticConfig: {static: [http://example]}}}
---
apiVersion: monitoring.coreos.com/v1alpha1
kind: ScrapeConfig
metadata: {name: planned, namespace: monitor-in-hand-test, labels: {monitoring.fjsxy.github.io/gh59: selected}}
spec:
  staticConfigs: [{targets: [metrics-target:9100]}]
  kubernetesSDConfigs:
    - {role: Pod}
    - {role: Service}
    - {role: Endpoints}
    - {role: EndpointSlice}
    - {role: Ingress}
`

const emptySelectorAgentManifest = `
apiVersion: monitoring.coreos.com/v1alpha1
kind: PrometheusAgent
metadata: {name: planned, namespace: monitor-in-hand-test}
spec:
  serviceAccountName: prometheus-agent
  serviceMonitorSelector: {}
`

const plannedGH60AgentManifest = `
apiVersion: monitoring.coreos.com/v1alpha1
kind: PrometheusAgent
metadata: {name: gh60-planned, namespace: monitor-in-hand-test}
spec:
  version: v2.55.1
  image: quay.io/prometheus/prometheus:v2.55.1
  serviceAccountName: prometheus-agent
  replicas: 1
  shards: 2
  replicaExternalLabelName: ""
  externalLabels: {mih_cluster_id: gh60-preflight}
  remoteWrite: [{name: fixture-vm, url: "http://vminsert.monitor-in-hand-test.svc:8480/insert/0/prometheus/api/v1/write"}]
`

const conflictingGH60AgentManifest = `
apiVersion: monitoring.coreos.com/v1alpha1
kind: PrometheusAgent
metadata: {name: gh60-conflict, namespace: monitor-in-hand-test}
spec:
  serviceAccountName: prometheus-agent
  replicas: 1
  shards: 1
`

const gh60ConflictStatefulSet = `
apiVersion: apps/v1
kind: StatefulSet
metadata: {name: prom-agent-gh60-conflict, namespace: monitor-in-hand-test}
spec:
  serviceName: unrelated
  replicas: 0
  selector: {matchLabels: {app: unrelated}}
  template:
    metadata: {labels: {app: unrelated}}
    spec:
      containers:
        - {name: unrelated, image: "quay.io/prometheus/prometheus:v2.55.1"}
`

const gh60ConflictPod = `
apiVersion: v1
kind: Pod
metadata: {name: prom-agent-gh60-conflict-0, namespace: monitor-in-hand-test}
spec:
  containers:
    - {name: unrelated, image: "quay.io/prometheus/prometheus:v2.55.1", args: ["--version"]}
`

const gh60ConflictConfigSecret = `
apiVersion: v1
kind: Secret
metadata: {name: prom-agent-gh60-conflict, namespace: monitor-in-hand-test}
stringData: {sentinel: config}
`

const gh60ConflictTLSSecret = `
apiVersion: v1
kind: Secret
metadata: {name: prom-agent-gh60-conflict-tls-assets-1, namespace: monitor-in-hand-test}
stringData: {sentinel: tls}
`

const gh60ConflictWebSecret = `
apiVersion: v1
kind: Secret
metadata: {name: prom-agent-gh60-conflict-web-config, namespace: monitor-in-hand-test}
stringData: {sentinel: web}
`

const gh60ConflictServiceResources = `
apiVersion: v1
kind: Namespace
metadata: {name: gh60-preflight-conflict}
---
apiVersion: v1
kind: ServiceAccount
metadata: {name: gh60-agent, namespace: gh60-preflight-conflict}
---
apiVersion: v1
kind: Service
metadata: {name: prometheus-agent-operated, namespace: gh60-preflight-conflict}
spec:
  clusterIP: None
  selector: {app: unrelated}
  ports: [{name: web, port: 9090, targetPort: web}]
`

const conflictingGH60ServiceAgentManifest = `
apiVersion: monitoring.coreos.com/v1alpha1
kind: PrometheusAgent
metadata: {name: gh60-service, namespace: gh60-preflight-conflict}
spec: {serviceAccountName: gh60-agent, replicas: 1, shards: 1}
`

const gh60ConflictPVC = `
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: prom-agent-gh60-pvc-db-prom-agent-gh60-pvc-0
  namespace: monitor-in-hand-test
  labels:
    app.kubernetes.io/name: prometheus-agent
    app.kubernetes.io/managed-by: prometheus-operator
    app.kubernetes.io/instance: gh60-pvc
    operator.prometheus.io/name: gh60-pvc
    operator.prometheus.io/shard: "0"
spec:
  accessModes: [ReadWriteOnce]
  volumeMode: Filesystem
  storageClassName: unrelated
  resources: {requests: {storage: 1Gi}}
`

const conflictingGH60PVCAgentManifest = `
apiVersion: monitoring.coreos.com/v1alpha1
kind: PrometheusAgent
metadata: {name: gh60-pvc, namespace: monitor-in-hand-test}
spec:
  serviceAccountName: prometheus-agent
  replicas: 0
  shards: 1
  storage:
    volumeClaimTemplate:
      spec:
        accessModes: [ReadWriteOnce]
        volumeMode: Filesystem
        storageClassName: expected
        resources: {requests: {storage: 1Gi}}
`

func applyPreflightManifest(ctx context.Context, manifest string) {
	GinkgoHelper()
	command := exec.CommandContext(ctx, "kubectl", "--context", config.kubeContext, "apply", "--filename", "-")
	command.Stdin = strings.NewReader(manifest)
	output, err := command.CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), string(output))
}

func resourceContent(ctx context.Context, resource, name, namespace string) string {
	GinkgoHelper()
	args := []string{"--context", config.kubeContext}
	if namespace != "" {
		args = append(args, "--namespace", namespace)
	}
	args = append(args, "get", resource, name, "--output", "json")
	output, err := exec.CommandContext(ctx, "kubectl", args...).CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), string(output))
	var object map[string]any
	Expect(json.Unmarshal(output, &object)).To(Succeed())
	metadata := object["metadata"].(map[string]any)
	fingerprint := map[string]any{
		"uid":             metadata["uid"],
		"labels":          metadata["labels"],
		"annotations":     metadata["annotations"],
		"ownerReferences": metadata["ownerReferences"],
		"finalizers":      metadata["finalizers"],
	}
	for _, field := range []string{"spec", "data", "roleRef", "subjects", "rules", "secrets"} {
		if value, found := object[field]; found {
			fingerprint[field] = value
		}
	}
	encoded, err := json.Marshal(fingerprint)
	Expect(err).NotTo(HaveOccurred())
	return string(encoded)
}

func deletePreflightResource(ctx context.Context, resource, name, namespace string) {
	GinkgoHelper()
	args := []string{"--context", config.kubeContext}
	if namespace != "" {
		args = append(args, "--namespace", namespace)
	}
	args = append(args, "delete", resource, name, "--ignore-not-found", "--wait=true", "--timeout=2m")
	output, err := exec.CommandContext(ctx, "kubectl", args...).CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), string(output))
}

func waitForPreflightServiceOwnerGC(ctx context.Context, namespace string) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		serviceArgs := []string{"--context", config.kubeContext, "--namespace", namespace, "get", "service", "prometheus-agent-operated", "--output", "json"}
		serviceOutput, err := exec.CommandContext(ctx, "kubectl", serviceArgs...).CombinedOutput()
		g.Expect(err).NotTo(HaveOccurred(), string(serviceOutput))
		agentArgs := []string{"--context", config.kubeContext, "--namespace", namespace, "get", "prometheusagents", "--output", "json"}
		agentOutput, err := exec.CommandContext(ctx, "kubectl", agentArgs...).CombinedOutput()
		g.Expect(err).NotTo(HaveOccurred(), string(agentOutput))
		var service struct {
			Metadata struct {
				OwnerReferences []struct {
					Name, UID string
				} `json:"ownerReferences"`
			} `json:"metadata"`
		}
		var agents struct {
			Items []struct {
				Metadata struct{ Name, UID string } `json:"metadata"`
			} `json:"items"`
		}
		g.Expect(json.Unmarshal(serviceOutput, &service)).To(Succeed())
		g.Expect(json.Unmarshal(agentOutput, &agents)).To(Succeed())
		for _, owner := range service.Metadata.OwnerReferences {
			g.Expect(agents.Items).To(ContainElement(HaveField("Metadata", SatisfyAll(
				HaveField("Name", owner.Name), HaveField("UID", owner.UID),
			))))
		}
	}, 2*time.Minute, time.Second).Should(Succeed())
}

func parseDiagnostics(output string) []diagnostic {
	// Make 会回显 recipe；只将可解析的 NDJSON 行作为预检诊断记录。
	diagnostics := make([]diagnostic, 0)
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		var item diagnostic
		if json.Unmarshal(scanner.Bytes(), &item) == nil && item.Check != "" {
			diagnostics = append(diagnostics, item)
		}
	}
	Expect(scanner.Err()).NotTo(HaveOccurred())
	Expect(diagnostics).NotTo(BeEmpty(), "预检输出中缺少 NDJSON 诊断")
	return diagnostics
}
