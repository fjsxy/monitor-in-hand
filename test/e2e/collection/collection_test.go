package collection_test

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"sigs.k8s.io/yaml"
)

const testNamespace = "monitor-in-hand-test"

type e2eConfig struct {
	kubernetesMinor string
	kubeContext     string
	testRunID       string
}

type prometheusConfig struct {
	ScrapeConfigs []struct {
		JobName             string `json:"job_name"`
		KubernetesSDConfigs []struct {
			Role string `json:"role"`
		} `json:"kubernetes_sd_configs"`
	} `json:"scrape_configs"`
}

type targetsResponse struct {
	Status string `json:"status"`
	Data   struct {
		ActiveTargets []struct {
			Health     string `json:"health"`
			ScrapePool string `json:"scrapePool"`
		} `json:"activeTargets"`
	} `json:"data"`
}

var config e2eConfig

var fixtureJobs = []string{
	"serviceMonitor/monitor-in-hand-test/fixture/0",
	"podMonitor/monitor-in-hand-test/fixture/0",
	"probe/monitor-in-hand-test/fixture",
	"scrapeConfig/monitor-in-hand-test/fixture",
	"scrapeConfig/monitor-in-hand-test/fixture-pod",
	"scrapeConfig/monitor-in-hand-test/fixture-service",
	"scrapeConfig/monitor-in-hand-test/fixture-endpoints",
	"scrapeConfig/monitor-in-hand-test/fixture-endpointslice",
	"scrapeConfig/monitor-in-hand-test/fixture-ingress",
}

type agentAPIResults struct {
	Targets      string
	StatusConfig string
}

var _ = BeforeSuite(func() {
	By("读取双版本 CI 提供的隔离测试集群身份")
	minor := os.Getenv("KUBERNETES_VERSION")
	runID := os.Getenv("TEST_RUN_ID")
	Expect([]string{"1.19", "1.28"}).To(ContainElement(minor))
	Expect(runID).To(MatchRegexp(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`))
	config = e2eConfig{
		kubernetesMinor: minor,
		kubeContext:     "kind-mih-e2e-" + strings.ReplaceAll(minor, ".", "") + "-" + runID,
		testRunID:       runID,
	}
})

var _ = Describe("原生 PrometheusAgent 采集配置", Serial, func() {
	It(fmt.Sprintf("[%s] 保持四类 object/namespace selector 的独立原生语义", stableScenarioID("001")), func(ctx SpecContext) {
		scenarioID := stableScenarioID("001")
		By(scenarioID + " 创建四类 object selector 全 nil 的合法零配置 Agent")
		applyManifest(ctx, zeroConfigurationAgent)
		DeferCleanup(func(cleanupCtx SpecContext) {
			deleteResource(cleanupCtx, "prometheusagent", "gh59-zero", testNamespace)
		})

		By(scenarioID + " 等待零配置 Agent 收敛并验证生成配置没有 scrape job")
		waitForAgentConditions(ctx, "gh59-zero")
		zeroConfig := waitForGeneratedConfig(ctx, "gh59-zero")
		AddReportEntry("zero-generated-config", zeroConfig, ReportEntryVisibilityFailureOrVerbose)
		parsedZero := parsePrometheusConfig(zeroConfig)
		Expect(parsedZero.ScrapeConfigs).To(BeEmpty())

		By(scenarioID + " 准备四类配置在 Agent 当前 namespace 与另一个带 label 的 namespace 中的对象")
		applyManifest(ctx, selectorConfigFixtures)
		DeferCleanup(func(cleanupCtx SpecContext) {
			for _, resource := range []string{"servicemonitor", "podmonitor", "probe", "scrapeconfig"} {
				deleteResource(cleanupCtx, resource, "gh59-"+resource+"-current", testNamespace)
			}
			deleteResource(cleanupCtx, "namespace", "gh59-selector-other", "")
		})

		selectorTypes := []struct {
			name, objectField, namespaceField, jobPrefix string
		}{
			{name: "servicemonitor", objectField: "serviceMonitorSelector", namespaceField: "serviceMonitorNamespaceSelector", jobPrefix: "serviceMonitor"},
			{name: "podmonitor", objectField: "podMonitorSelector", namespaceField: "podMonitorNamespaceSelector", jobPrefix: "podMonitor"},
			{name: "probe", objectField: "probeSelector", namespaceField: "probeNamespaceSelector", jobPrefix: "probe"},
			{name: "scrapeconfig", objectField: "scrapeConfigSelector", namespaceField: "scrapeConfigNamespaceSelector", jobPrefix: "scrapeConfig"},
		}
		for _, selectorType := range selectorTypes {
			for _, namespaceState := range []string{"nil", "empty", "labeled"} {
				agentName := "gh59-" + selectorType.name + "-" + namespaceState
				By(fmt.Sprintf("%s 验证 %s 的 %s namespace selector 且其余三类 object selector 为 nil", scenarioID, selectorType.name, namespaceState))
				applyManifest(ctx, selectorAgentManifest(agentName, selectorType.name, selectorType.objectField, selectorType.namespaceField, namespaceState))
				DeferCleanup(func(cleanupCtx SpecContext) { deleteResource(cleanupCtx, "prometheusagent", agentName, testNamespace) })

				By(fmt.Sprintf("%s 等待 %s/%s 生成配置并验证精确 job 集合", scenarioID, selectorType.name, namespaceState))
				generated := waitForGeneratedConfig(ctx, agentName)
				AddReportEntry(agentName+"-generated-config", generated, ReportEntryVisibilityFailureOrVerbose)
				actualJobs := gh59JobNames(parsePrometheusConfig(generated))
				expectedJobs := expectedSelectorJobs(selectorType.jobPrefix, selectorType.name, namespaceState)
				Expect(actualJobs).To(ConsistOf(expectedJobs))

				By(fmt.Sprintf("%s 清理 %s/%s 临时 Agent", scenarioID, selectorType.name, namespaceState))
				deleteResource(ctx, "prometheusagent", agentName, testNamespace)
			}
		}
	}, SpecTimeout(10*time.Minute))

	It(fmt.Sprintf("[%s] 从 generated config、status 和 targets 验证四类真实 target 与六种 ScrapeConfig discovery", stableScenarioID("002")), func(ctx SpecContext) {
		scenarioID := stableScenarioID("002")
		By(scenarioID + " 等待固定 Agent 控制面可用并读取生成配置")
		waitForAgentConditions(ctx, "fixture")
		generated := waitForGeneratedConfig(ctx, "fixture")
		AddReportEntry("generated-config", generated, ReportEntryVisibilityFailureOrVerbose)

		By(scenarioID + " 验证四类配置与六种 ScrapeConfig discovery 均进入 generated config")
		parsed := parsePrometheusConfig(generated)
		jobs := jobNames(parsed)
		for _, job := range fixtureJobs {
			Expect(jobs).To(ContainElement(job))
		}

		By(scenarioID + " 验证 ServiceMonitor 在两个 Kubernetes minor 使用各自原生 discovery role")
		expectedRole := "endpoints"
		if config.kubernetesMinor == "1.28" {
			expectedRole = "endpointslice"
		}
		Expect(jobRoles(parsed, "serviceMonitor/monitor-in-hand-test/fixture/0")).To(Equal([]string{expectedRole}))

		By(scenarioID + " 验证统一 runtime RBAC 覆盖一期 discovery 且不授予 nodes")
		agentSubject := "system:serviceaccount:monitor-in-hand-test:prometheus-agent"
		for _, resource := range []string{"services", "endpoints", "pods", "ingresses.networking.k8s.io", "endpointslices.discovery.k8s.io"} {
			for _, verb := range []string{"get", "list", "watch"} {
				allowed := kubectl(ctx, "auth", "can-i", verb, resource, "--all-namespaces", "--as", agentSubject)
				Expect(strings.TrimSpace(allowed)).To(Equal("yes"), verb+" "+resource)
			}
		}
		Expect(strings.TrimSpace(kubectl(ctx, "auth", "can-i", "get", "configmaps", "--all-namespaces", "--as", agentSubject))).To(Equal("yes"))
		nodesOutput, nodesErr := kubectlResult(ctx, "auth", "can-i", "get", "nodes", "--as", agentSubject)
		Expect(nodesErr).To(HaveOccurred(), "nodes 权限必须被拒绝")
		Expect(strings.TrimSpace(nodesOutput)).To(Equal("no"))

		By(scenarioID + " 从 Agent targets API 等待每类 job 至少一个真实 up target")
		apiResults := waitForAgentAPIs(ctx, fixtureJobs)
		AddReportEntry("targets", apiResults.Targets, ReportEntryVisibilityFailureOrVerbose)
		AddReportEntry("agent-status-config", apiResults.StatusConfig, ReportEntryVisibilityFailureOrVerbose)
		Expect(apiResults.StatusConfig).To(ContainSubstring("serviceMonitor/monitor-in-hand-test/fixture/0"))
	}, SpecTimeout(10*time.Minute))

	It(fmt.Sprintf("[%s] 通过 InvalidConfiguration Event 与 Operator logs 定位被拒绝配置", stableScenarioID("003")), func(ctx SpecContext) {
		scenarioID := stableScenarioID("003")
		By(scenarioID + " 创建上游 API 接受但 selector role 不兼容的 ScrapeConfig")
		applyManifest(ctx, invalidScrapeConfig)
		DeferCleanup(func(cleanupCtx SpecContext) {
			deleteResource(cleanupCtx, "scrapeconfig", "gh59-invalid", testNamespace)
		})

		By(scenarioID + " 等待配置对象上的 InvalidConfiguration Event")
		var events string
		Eventually(func(g Gomega) {
			var err error
			events, err = kubectlResult(ctx, "--namespace", testNamespace, "get", "events", "--field-selector",
				"involvedObject.kind=ScrapeConfig,involvedObject.name=gh59-invalid", "--output", "json")
			g.Expect(err).NotTo(HaveOccurred(), events)
			found, err := eventHasReason(events, "InvalidConfiguration")
			g.Expect(err).NotTo(HaveOccurred(), events)
			g.Expect(found).To(BeTrue(), events)
		}, 3*time.Minute, 2*time.Second).Should(Succeed())
		AddReportEntry("invalid-configuration-events", events, ReportEntryVisibilityFailureOrVerbose)

		By(scenarioID + " 验证拒绝对象不进入 generated config 且 Operator logs 可定位名称")
		generated := waitForGeneratedConfig(ctx, "fixture")
		Expect(jobNames(parsePrometheusConfig(generated))).NotTo(ContainElement("scrapeConfig/monitor-in-hand-test/gh59-invalid"))
		var operatorLogs string
		Eventually(func(g Gomega) {
			var err error
			operatorLogs, err = kubectlResult(ctx, "--namespace", "prometheus-operator", "logs", "deployment/prometheus-operator", "--all-containers")
			g.Expect(err).NotTo(HaveOccurred(), operatorLogs)
			g.Expect(operatorLogs).To(ContainSubstring("gh59-invalid"))
		}, 2*time.Minute, 2*time.Second).Should(Succeed())
		AddReportEntry("prometheus-operator-log", operatorLogs, ReportEntryVisibilityFailureOrVerbose)
	}, SpecTimeout(5*time.Minute))
})

func stableScenarioID(sequence string) string {
	ids := map[string]map[string]string{
		"1.19": {
			"001": "E2E-119-COLLECTION-GH59-001",
			"002": "E2E-119-COLLECTION-GH59-002",
			"003": "E2E-119-COLLECTION-GH59-003",
		},
		"1.28": {
			"001": "E2E-128-COLLECTION-GH59-001",
			"002": "E2E-128-COLLECTION-GH59-002",
			"003": "E2E-128-COLLECTION-GH59-003",
		},
	}
	// Ginkgo 在 BeforeSuite 之前注册测试树，因此场景名必须直接读取进程环境。
	if versionIDs, supported := ids[os.Getenv("KUBERNETES_VERSION")]; supported {
		if id, found := versionIDs[sequence]; found {
			return id
		}
	}
	return "E2E-UNSUPPORTED-COLLECTION-GH59-" + sequence
}

func applyManifest(ctx context.Context, manifest string) {
	GinkgoHelper()
	command := exec.CommandContext(ctx, "kubectl", "--context", config.kubeContext, "apply", "--filename", "-")
	command.Stdin = strings.NewReader(manifest)
	output, err := command.CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), string(output))
}

func deleteResource(ctx context.Context, resource, name, namespace string) {
	GinkgoHelper()
	args := []string{"--context", config.kubeContext}
	if namespace != "" {
		args = append(args, "--namespace", namespace)
	}
	args = append(args, "delete", resource, name, "--ignore-not-found", "--wait=true", "--timeout=2m")
	command := exec.CommandContext(ctx, "kubectl", args...)
	output, err := command.CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), string(output))
}

func kubectl(ctx context.Context, args ...string) string {
	GinkgoHelper()
	output, err := kubectlResult(ctx, args...)
	Expect(err).NotTo(HaveOccurred(), output)
	return output
}

func kubectlResult(ctx context.Context, args ...string) (string, error) {
	commandArgs := append([]string{"--context", config.kubeContext}, args...)
	return commandResult(exec.CommandContext(ctx, "kubectl", commandArgs...))
}

func commandResult(command *exec.Cmd) (string, error) {
	// 将机器可判定的 stdout 与诊断 stderr 分流，避免 kubectl warning 污染授权结果或 JSON。
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	if err != nil && stderr.Len() > 0 {
		return stdout.String(), fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), err
}

func eventHasReason(events, reason string) (bool, error) {
	// 结构化读取 EventList，确保断言不依赖 kubectl 的 JSON 缩进格式。
	var eventList struct {
		Items []struct {
			Reason string `json:"reason"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(events), &eventList); err != nil {
		return false, fmt.Errorf("parse event list: %w", err)
	}
	for _, event := range eventList.Items {
		if event.Reason == reason {
			return true, nil
		}
	}
	return false, nil
}

func waitForAgentConditions(ctx context.Context, name string) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		output, err := kubectlResult(ctx, "--namespace", testNamespace, "get", "prometheusagent", name, "--output", "json")
		g.Expect(err).NotTo(HaveOccurred(), output)
		var object struct {
			Metadata struct {
				Generation int64 `json:"generation"`
			} `json:"metadata"`
			Status struct {
				Conditions []struct {
					Type               string `json:"type"`
					Status             string `json:"status"`
					ObservedGeneration int64  `json:"observedGeneration"`
				} `json:"conditions"`
			} `json:"status"`
		}
		g.Expect(json.Unmarshal([]byte(output), &object)).To(Succeed())
		for _, conditionType := range []string{"Reconciled", "Available"} {
			g.Expect(object.Status.Conditions).To(ContainElement(SatisfyAll(
				HaveField("Type", conditionType), HaveField("Status", "True"),
				HaveField("ObservedGeneration", object.Metadata.Generation),
			)))
		}
	}, 5*time.Minute, 2*time.Second).Should(Succeed())
}

func waitForGeneratedConfig(ctx context.Context, agent string) string {
	GinkgoHelper()
	var generated string
	Eventually(func(g Gomega) {
		output, err := kubectlResult(ctx, "--namespace", testNamespace, "get", "secret", "prom-agent-"+agent, "--output", "json")
		g.Expect(err).NotTo(HaveOccurred(), output)
		var secret struct {
			Data map[string]string `json:"data"`
		}
		g.Expect(json.Unmarshal([]byte(output), &secret)).To(Succeed())
		encoded := secret.Data["prometheus.yaml.gz"]
		g.Expect(encoded).NotTo(BeEmpty())
		compressed, err := base64.StdEncoding.DecodeString(encoded)
		g.Expect(err).NotTo(HaveOccurred())
		reader, err := gzip.NewReader(bytes.NewReader(compressed))
		g.Expect(err).NotTo(HaveOccurred())
		decompressed, err := io.ReadAll(reader)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(reader.Close()).To(Succeed())
		generated = string(decompressed)
	}, 3*time.Minute, 2*time.Second).Should(Succeed())
	return generated
}

func parsePrometheusConfig(contents string) prometheusConfig {
	GinkgoHelper()
	var parsed prometheusConfig
	Expect(yaml.Unmarshal([]byte(contents), &parsed)).To(Succeed())
	return parsed
}

func jobNames(config prometheusConfig) []string {
	result := make([]string, 0, len(config.ScrapeConfigs))
	for _, job := range config.ScrapeConfigs {
		result = append(result, job.JobName)
	}
	return result
}

func jobRoles(config prometheusConfig, jobName string) []string {
	for _, job := range config.ScrapeConfigs {
		if job.JobName != jobName {
			continue
		}
		roles := make([]string, 0, len(job.KubernetesSDConfigs))
		for _, discovery := range job.KubernetesSDConfigs {
			roles = append(roles, discovery.Role)
		}
		return roles
	}
	return nil
}

func gh59JobNames(config prometheusConfig) []string {
	jobs := []string{}
	for _, job := range jobNames(config) {
		if strings.Contains(job, "/gh59-") {
			jobs = append(jobs, job)
		}
	}
	return jobs
}

func expectedSelectorJobs(jobPrefix, resourceName, namespaceState string) []string {
	namespaces := []string{testNamespace}
	if namespaceState == "empty" {
		namespaces = append(namespaces, "gh59-selector-other")
	} else if namespaceState == "labeled" {
		namespaces = []string{"gh59-selector-other"}
	}
	jobs := make([]string, 0, len(namespaces))
	for _, namespace := range namespaces {
		location := "other"
		if namespace == testNamespace {
			location = "current"
		}
		job := fmt.Sprintf("%s/%s/gh59-%s-%s", jobPrefix, namespace, resourceName, location)
		if jobPrefix == "serviceMonitor" || jobPrefix == "podMonitor" {
			job += "/0"
		}
		jobs = append(jobs, job)
	}
	return jobs
}

func selectorAgentManifest(name, selectorValue, objectField, namespaceField, namespaceState string) string {
	namespaceSelector := ""
	switch namespaceState {
	case "empty":
		namespaceSelector = fmt.Sprintf("  %s: {}\n", namespaceField)
	case "labeled":
		namespaceSelector = fmt.Sprintf("  %s:\n    matchLabels:\n      monitoring.fjsxy.github.io/gh59-selected: \"true\"\n", namespaceField)
	}
	return fmt.Sprintf(`
apiVersion: monitoring.coreos.com/v1alpha1
kind: PrometheusAgent
metadata:
  name: %s
  namespace: monitor-in-hand-test
spec:
  version: v2.55.1
  image: quay.io/prometheus/prometheus@sha256:2659f4c2ebb718e7695cb9b25ffa7d6be64db013daba13e05c875451cf51b0d3
  serviceAccountName: prometheus-agent
  replicas: 0
  shards: 1
  %s:
    matchLabels:
      monitoring.fjsxy.github.io/gh59-type: %s
%s  remoteWrite:
    - name: fixture-vm
      url: http://vminsert.monitor-in-hand-test.svc:8480/insert/0/prometheus/api/v1/write
`, name, objectField, selectorValue, namespaceSelector)
}

func waitForAgentAPIs(ctx context.Context, requiredJobs []string) agentAPIResults {
	GinkgoHelper()
	// 先建立只在当前场景存活的端口转发，使两个原生 Agent API 共享同一观察窗口。
	port, stop := startPortForward(ctx)
	DeferCleanup(stop)
	client := &http.Client{Timeout: 5 * time.Second}
	var body string

	// 等待每个预期 scrape pool 都产生真实 up target，并保留完整响应作为失败证据。
	Eventually(func(g Gomega) {
		response, err := client.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/api/v1/targets")
		g.Expect(err).NotTo(HaveOccurred())
		defer response.Body.Close()
		contents, err := io.ReadAll(response.Body)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(response.StatusCode).To(Equal(http.StatusOK), string(contents))
		body = string(contents)
		var targets targetsResponse
		g.Expect(json.Unmarshal(contents, &targets)).To(Succeed())
		g.Expect(targets.Status).To(Equal("success"))
		for _, job := range requiredJobs {
			found := false
			for _, target := range targets.Data.ActiveTargets {
				found = found || (target.ScrapePool == job && target.Health == "up")
			}
			g.Expect(found).To(BeTrue(), "job %s 没有 up target", job)
		}
	}, 5*time.Minute, 3*time.Second).Should(Succeed())
	var statusConfig string

	// targets 收敛后读取运行时已加载配置，避免只依赖 Operator 生成的 Secret 判断成功。
	Eventually(func(g Gomega) {
		response, err := client.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/api/v1/status/config")
		g.Expect(err).NotTo(HaveOccurred())
		defer response.Body.Close()
		contents, err := io.ReadAll(response.Body)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(response.StatusCode).To(Equal(http.StatusOK), string(contents))
		statusConfig = string(contents)
		g.Expect(statusConfig).To(ContainSubstring(`"status":"success"`))
	}, time.Minute, 2*time.Second).Should(Succeed())
	return agentAPIResults{Targets: body, StatusConfig: statusConfig}
}

func startPortForward(ctx context.Context) (int, func()) {
	GinkgoHelper()
	return startPortForwardTarget(ctx, testNamespace, "pod/prom-agent-fixture-0", 9090)
}

func startPortForwardTarget(ctx context.Context, namespace, target string, remotePort int) (int, func()) {
	GinkgoHelper()
	// 启动 kubectl 子进程并同时接收 stdout/stderr，因为不同 kubectl 版本的监听提示流不同。
	command := exec.CommandContext(ctx, "kubectl", "--context", config.kubeContext, "--namespace", namespace,
		"port-forward", target, fmt.Sprintf("0:%d", remotePort))
	stdout, err := command.StdoutPipe()
	Expect(err).NotTo(HaveOccurred())
	stderr, err := command.StderrPipe()
	Expect(err).NotTo(HaveOccurred())
	Expect(command.Start()).To(Succeed())
	lines := make(chan string, 8)

	// 并发汇聚两条输出流，直到取得 kubectl 动态分配的本地 IPv4 端口。
	readLines := func(reader io.Reader) {
		scanner := bufio.NewScanner(reader)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
	}
	go readLines(stdout)
	go readLines(stderr)
	pattern := regexp.MustCompile(`Forwarding from 127\.0\.0\.1:(\d+)`)
	var port int
	Eventually(lines, 30*time.Second).Should(Receive(WithTransform(func(line string) int {
		match := pattern.FindStringSubmatch(line)
		if len(match) != 2 {
			return 0
		}
		value, _ := strconv.Atoi(match[1])
		return value
	}, Satisfy(func(value int) bool {
		port = value
		return value > 0
	}))))

	// 把子进程终止与回收封装成场景清理函数，防止残留端口转发影响后续断言。
	return port, func() {
		if command.Process != nil {
			_ = command.Process.Kill()
		}
		_ = command.Wait()
	}
}

const zeroConfigurationAgent = `
apiVersion: monitoring.coreos.com/v1alpha1
kind: PrometheusAgent
metadata:
  name: gh59-zero
  namespace: monitor-in-hand-test
spec:
  version: v2.55.1
  image: quay.io/prometheus/prometheus@sha256:2659f4c2ebb718e7695cb9b25ffa7d6be64db013daba13e05c875451cf51b0d3
  serviceAccountName: prometheus-agent
  replicas: 1
  shards: 1
  remoteWrite:
    - name: fixture-vm
      url: http://vminsert.monitor-in-hand-test.svc:8480/insert/0/prometheus/api/v1/write
`

const selectorConfigFixtures = `
apiVersion: v1
kind: Namespace
metadata:
  name: gh59-selector-other
  labels:
    monitoring.fjsxy.github.io/gh59-selected: "true"
---
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: gh59-servicemonitor-current
  namespace: monitor-in-hand-test
  labels: {monitoring.fjsxy.github.io/gh59-type: servicemonitor}
spec:
  selector: {matchLabels: {app.kubernetes.io/name: metrics-target}}
  endpoints: [{port: metrics}]
---
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: gh59-servicemonitor-other
  namespace: gh59-selector-other
  labels: {monitoring.fjsxy.github.io/gh59-type: servicemonitor}
spec:
  namespaceSelector: {matchNames: [monitor-in-hand-test]}
  selector: {matchLabels: {app.kubernetes.io/name: metrics-target}}
  endpoints: [{port: metrics}]
---
apiVersion: monitoring.coreos.com/v1
kind: PodMonitor
metadata:
  name: gh59-podmonitor-current
  namespace: monitor-in-hand-test
  labels: {monitoring.fjsxy.github.io/gh59-type: podmonitor}
spec:
  selector: {matchLabels: {app.kubernetes.io/name: metrics-target}}
  podMetricsEndpoints: [{port: metrics}]
---
apiVersion: monitoring.coreos.com/v1
kind: PodMonitor
metadata:
  name: gh59-podmonitor-other
  namespace: gh59-selector-other
  labels: {monitoring.fjsxy.github.io/gh59-type: podmonitor}
spec:
  namespaceSelector: {matchNames: [monitor-in-hand-test]}
  selector: {matchLabels: {app.kubernetes.io/name: metrics-target}}
  podMetricsEndpoints: [{port: metrics}]
---
apiVersion: monitoring.coreos.com/v1
kind: Probe
metadata:
  name: gh59-probe-current
  namespace: monitor-in-hand-test
  labels: {monitoring.fjsxy.github.io/gh59-type: probe}
spec:
  prober: {url: blackbox-exporter.monitor-in-hand-test.svc:9115}
  targets: {staticConfig: {static: [http://metrics-target.monitor-in-hand-test.svc:9100/metrics]}}
---
apiVersion: monitoring.coreos.com/v1
kind: Probe
metadata:
  name: gh59-probe-other
  namespace: gh59-selector-other
  labels: {monitoring.fjsxy.github.io/gh59-type: probe}
spec:
  prober: {url: blackbox-exporter.monitor-in-hand-test.svc:9115}
  targets: {staticConfig: {static: [http://metrics-target.monitor-in-hand-test.svc:9100/metrics]}}
---
apiVersion: monitoring.coreos.com/v1alpha1
kind: ScrapeConfig
metadata:
  name: gh59-scrapeconfig-current
  namespace: monitor-in-hand-test
  labels: {monitoring.fjsxy.github.io/gh59-type: scrapeconfig}
spec:
  staticConfigs: [{targets: [metrics-target.monitor-in-hand-test.svc:9100]}]
---
apiVersion: monitoring.coreos.com/v1alpha1
kind: ScrapeConfig
metadata:
  name: gh59-scrapeconfig-other
  namespace: gh59-selector-other
  labels: {monitoring.fjsxy.github.io/gh59-type: scrapeconfig}
spec:
  staticConfigs: [{targets: [metrics-target.monitor-in-hand-test.svc:9100]}]
`

const invalidScrapeConfig = `
apiVersion: monitoring.coreos.com/v1alpha1
kind: ScrapeConfig
metadata:
  name: gh59-invalid
  namespace: monitor-in-hand-test
  labels:
    monitoring.fjsxy.github.io/test-fixture: "true"
spec:
  kubernetesSDConfigs:
    - role: Pod
      selectors:
        - role: Service
          label: app.kubernetes.io/name=metrics-target
`
