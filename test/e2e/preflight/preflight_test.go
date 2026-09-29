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
})

func stableScenarioID(sequence string) string {
	ids := map[string]map[string]string{
		"1.19": {
			"001":      "E2E-119-PREFLIGHT-001",
			"002":      "E2E-119-PREFLIGHT-002",
			"GH59-001": "E2E-119-PREFLIGHT-GH59-001",
			"GH59-002": "E2E-119-PREFLIGHT-GH59-002",
		},
		"1.28": {
			"001":      "E2E-128-PREFLIGHT-001",
			"002":      "E2E-128-PREFLIGHT-002",
			"GH59-001": "E2E-128-PREFLIGHT-GH59-001",
			"GH59-002": "E2E-128-PREFLIGHT-GH59-002",
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
