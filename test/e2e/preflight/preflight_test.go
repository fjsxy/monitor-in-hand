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
	scenarioPrefix  string
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
		config.scenarioPrefix = "E2E-119-PREFLIGHT"
		config.versionReason = "SupportedKubernetes119"
	case "1.28":
		config.scenarioPrefix = "E2E-128-PREFLIGHT"
		config.versionReason = "SupportedKubernetes128"
	default:
		Fail(fmt.Sprintf("KUBERNETES_VERSION 必须是 1.19 或 1.28，当前为 %q", kubernetesMinor))
	}
})

var _ = Describe("真实集群部署预检", Serial, func() {
	It(fmt.Sprintf("[%s-001] 在一期支持环境中返回成功和版本事实", stableScenarioPrefix()), func(ctx SpecContext) {
		scenarioID := config.scenarioPrefix + "-001"
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

	It(fmt.Sprintf("[%s-002] 在 runtime ServiceAccount 缺失时非零退出并输出可定位原因", stableScenarioPrefix()), func(ctx SpecContext) {
		scenarioID := config.scenarioPrefix + "-002"
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
})

func stableScenarioPrefix() string {
	switch os.Getenv("KUBERNETES_VERSION") {
	case "1.19":
		return "E2E-119-PREFLIGHT"
	case "1.28":
		return "E2E-128-PREFLIGHT"
	default:
		return "E2E-UNSUPPORTED-PREFLIGHT"
	}
}

func runPreflight(ctx context.Context, serviceAccount string) (string, error) {
	// 始终通过项目公开 Make 入口执行预检，确保 E2E 覆盖用户实际调用路径。
	command := exec.CommandContext(ctx, "make", "preflight",
		"AGENT_NAMESPACE=monitor-in-hand-test",
		"AGENT_SERVICE_ACCOUNT="+serviceAccount,
		"CONFIG_NAMESPACES=monitor-in-hand-test",
		"DISCOVERY_NAMESPACES=monitor-in-hand-test",
		"OPERATOR_NAMESPACE=prometheus-operator",
		"OPERATOR_DEPLOYMENT=prometheus-operator",
		"KUBE_CONTEXT="+config.kubeContext,
		"PREFLIGHT_TIMEOUT_SECONDS=180",
	)
	command.Dir = config.repoRoot
	output, err := command.CombinedOutput()
	return string(output), err
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
