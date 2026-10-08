package collection_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"sigs.k8s.io/yaml"
)

const (
	gh61AgentName       = "gh61-remote-write"
	gh61KSMName         = "gh61-remote-write"
	gh61RemoteWriteName = "gh61-victoriametrics-primary"
	gh61AccountID       = "61"
)

type gh61QueryResponse struct {
	Status string `json:"status"`
	Data   struct {
		Result []struct {
			Metric map[string]string `json:"metric"`
		} `json:"result"`
	} `json:"data"`
}

var _ = Describe("KubeStateMetrics remote_write 数据链路", Serial, func() {
	It(fmt.Sprintf("[%s] 从两分片 KubeStateMetrics 经 Agent 写入指定 VictoriaMetrics tenant", gh61CollectionScenarioID("001")), func(ctx SpecContext) {
		scenarioID := gh61CollectionScenarioID("001")
		clusterID := "gh61-" + config.testRunID
		testLabel := "gh61-" + config.testRunID
		agentManifest := gh61AgentManifest(clusterID)
		serviceMonitorManifest := gh61ServiceMonitorManifest(testLabel)

		By(scenarioID + " 在创建 Agent 前执行包含保护标签扫描的一期预检")
		preflightOutput := runGH61Preflight(ctx, agentManifest, serviceMonitorManifest)
		AddReportEntry("preflight", preflightOutput, ReportEntryVisibilityFailureOrVerbose)
		Expect(preflightOutput).To(SatisfyAll(
			ContainSubstring(`"reason":"ProtectedExternalLabelPreserved"`),
			ContainSubstring(`"reason":"PreflightPassed"`),
		))

		By(scenarioID + " 创建两分片 KubeStateMetrics、ServiceMonitor 和两 shard Agent")
		DeferCleanup(func(cleanupCtx SpecContext) {
			deleteResource(cleanupCtx, "prometheusagent", gh61AgentName, testNamespace)
			deleteResource(cleanupCtx, "servicemonitor", gh61AgentName, testNamespace)
			deleteResource(cleanupCtx, "kubestatemetrics", gh61KSMName, "")
		})
		DeferCleanup(func(cleanupCtx SpecContext) {
			collectGH61Evidence(cleanupCtx, testLabel, clusterID)
		})
		applyManifest(ctx, gh61KSMManifest)
		applyManifest(ctx, serviceMonitorManifest)
		applyManifest(ctx, agentManifest)

		By(scenarioID + " 等待 KubeStateMetrics 和 Agent 管控面收敛")
		ksmStatus := waitForGH61KSM(ctx)
		waitForAgentConditions(ctx, gh61AgentName)
		agentStatus := kubectl(ctx, "--namespace", testNamespace, "get", "prometheusagent", gh61AgentName, "--output", "json")
		AddReportEntry("ksm-status", ksmStatus, ReportEntryVisibilityFailureOrVerbose)
		AddReportEntry("agent-status", agentStatus, ReportEntryVisibilityFailureOrVerbose)

		By(scenarioID + " 保存 Operator 生成配置并验证默认 prometheus 与稳定集群标签")
		generated := waitForGeneratedConfig(ctx, gh61AgentName)
		AddReportEntry("generated-config", generated, ReportEntryVisibilityFailureOrVerbose)
		assertGH61GeneratedConfig(generated, clusterID)

		By(scenarioID + " 通过两个 shard 的 targets API 确认 ServiceMonitor 覆盖所有 KubeStateMetrics endpoints")
		shard0 := waitForPodTargets(ctx, testNamespace, "prom-agent-"+gh61AgentName+"-0")
		shard1 := waitForPodTargets(ctx, testNamespace, "prom-agent-"+gh61AgentName+"-shard-1-0")
		AddReportEntry("shard-0-targets", shard0.Body, ReportEntryVisibilityFailureOrVerbose)
		AddReportEntry("shard-1-targets", shard1.Body, ReportEntryVisibilityFailureOrVerbose)
		endpointIPs := waitForGH61EndpointIPs(ctx)
		Expect(targetHosts(union(shard0.URLs, shard1.URLs))).To(ConsistOf(endpointIPs))
		Expect(intersection(shard0.URLs, shard1.URLs)).To(BeEmpty())

		By(scenarioID + " 从正确 accountID 的 vmselect 查询唯一测试标签和稳定集群身份")
		queryBody := waitForGH61Series(ctx, gh61AccountID, testLabel, clusterID)
		AddReportEntry("vmselect-query", queryBody, ReportEntryVisibilityFailureOrVerbose)
		wrongTenantBody := waitForNoGH61Series(ctx, "0", testLabel, clusterID)
		AddReportEntry("vmselect-wrong-tenant-query", wrongTenantBody, ReportEntryVisibilityFailureOrVerbose)

		By(scenarioID + " 保存两个 Agent shard 的 remote_write 输出指标")
		for _, pod := range []string{"prom-agent-" + gh61AgentName + "-0", "prom-agent-" + gh61AgentName + "-shard-1-0"} {
			metrics := waitForGH61Metrics(ctx, "pod/"+pod, 9090, func(contents string) bool {
				value, found := metricSample(contents, "prometheus_remote_storage_samples_total", `remote_name="`+gh61RemoteWriteName+`"`)
				return found && value > 0
			})
			AddReportEntry(pod+"-remote-write-metrics", metrics, ReportEntryVisibilityFailureOrVerbose)
		}

		By(scenarioID + " 保存 VictoriaMetrics 已接收 Prometheus remote_write 请求的指标")
		vminsertMetrics := waitForGH61Metrics(ctx, "service/vminsert", 8480, func(contents string) bool {
			value, found := metricSample(contents, "vm_http_requests_total", `protocol="promremotewrite"`)
			return found && value > 0
		})
		AddReportEntry("vminsert-receive-metrics", vminsertMetrics, ReportEntryVisibilityFailureOrVerbose)
	}, SpecTimeout(15*time.Minute))
})

func gh61CollectionScenarioID(sequence string) string {
	ids := map[string]map[string]string{
		"1.19": {"001": "E2E-119-COLLECTION-GH61-001"},
		"1.28": {"001": "E2E-128-COLLECTION-GH61-001"},
	}
	// Ginkgo 在 BeforeSuite 之前注册测试树，因此场景名必须直接读取进程环境。
	if versionIDs, supported := ids[os.Getenv("KUBERNETES_VERSION")]; supported {
		if id, found := versionIDs[sequence]; found {
			return id
		}
	}
	return "E2E-UNSUPPORTED-COLLECTION-GH61-" + sequence
}

const gh61KSMManifest = `
apiVersion: monitoring.fjsxy.github.io/v1alpha1
kind: KubeStateMetrics
metadata:
  name: gh61-remote-write
spec:
  targetNamespace: monitor-in-hand-test
  deployment:
    mode: StatefulSetSharded
    statefulSetSharded:
      shards: 2
`

func gh61ServiceMonitorManifest(testLabel string) string {
	return fmt.Sprintf(`
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: gh61-remote-write
  namespace: monitor-in-hand-test
  labels:
    monitoring.fjsxy.github.io/gh61: "true"
spec:
  selector:
    matchLabels:
      app.kubernetes.io/name: kube-state-metrics
      app.kubernetes.io/instance: gh61-remote-write
  endpoints:
    - port: http-metrics
      interval: 5s
      relabelings:
        - sourceLabels: [__meta_kubernetes_pod_name]
          regex: .+-(0|1)
          targetLabel: __tmp_hash
          replacement: $1
      metricRelabelings:
        - targetLabel: mih_test_run_id
          replacement: %s
`, testLabel)
}

func gh61AgentManifest(clusterID string) string {
	return fmt.Sprintf(`
apiVersion: monitoring.coreos.com/v1alpha1
kind: PrometheusAgent
metadata:
  name: gh61-remote-write
  namespace: monitor-in-hand-test
spec:
  version: v2.55.1
  image: quay.io/prometheus/prometheus:v2.55.1
  serviceAccountName: prometheus-agent
  replicas: 1
  shards: 2
  walCompression: true
  replicaExternalLabelName: ""
  externalLabels:
    mih_cluster_id: %s
  serviceMonitorSelector:
    matchLabels:
      monitoring.fjsxy.github.io/gh61: "true"
  remoteWrite:
    - name: gh61-victoriametrics-primary
      url: http://vminsert.monitor-in-hand-test.svc:8480/insert/61/prometheus/api/v1/write
      messageVersion: V1.0
`, clusterID)
}

func runGH61Preflight(ctx context.Context, agentManifest, serviceMonitorManifest string) string {
	GinkgoHelper()
	// 先把动态集群身份与测试标签落为临时 manifest，使预检扫描的内容与后续实际应用完全一致。
	repoRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	Expect(err).NotTo(HaveOccurred())
	directory := GinkgoT().TempDir()
	agentPath := filepath.Join(directory, "agent.yaml")
	serviceMonitorPath := filepath.Join(directory, "service-monitor.yaml")
	Expect(os.WriteFile(agentPath, []byte(agentManifest), 0o600)).To(Succeed())
	Expect(os.WriteFile(serviceMonitorPath, []byte(serviceMonitorManifest), 0o600)).To(Succeed())

	// 再通过公开 CLI 执行完整预检，保留 NDJSON 作为数据链路创建前证据。
	command := exec.CommandContext(ctx, filepath.Join(repoRoot, "hack", "preflight.sh"),
		"--context", config.kubeContext,
		"--agent-namespace", testNamespace,
		"--agent-service-account", "prometheus-agent",
		"--config-namespaces", testNamespace,
		"--discovery-namespaces", testNamespace,
		"--agent-manifest", agentPath,
		"--collection-config-manifest", serviceMonitorPath,
	)
	output, err := command.CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), string(output))
	return string(output)
}

func waitForGH61KSM(ctx context.Context) string {
	GinkgoHelper()
	var body string
	// 持续读取 CR 的最新 generation，直到模式、期望 Pod 和可用 Pod 三类事实同时收敛。
	Eventually(func(g Gomega) {
		var err error
		body, err = kubectlResult(ctx, "get", "kubestatemetrics", gh61KSMName, "--output", "json")
		g.Expect(err).NotTo(HaveOccurred(), body)
		var object struct {
			Metadata struct {
				Generation int64 `json:"generation"`
			} `json:"metadata"`
			Status struct {
				ObservedGeneration int64  `json:"observedGeneration"`
				Phase              string `json:"phase"`
				DeploymentMode     string `json:"deploymentMode"`
				DesiredPods        int32  `json:"desiredPods"`
				AvailablePods      int32  `json:"availablePods"`
			} `json:"status"`
		}
		g.Expect(json.Unmarshal([]byte(body), &object)).To(Succeed())
		g.Expect(object.Status.ObservedGeneration).To(Equal(object.Metadata.Generation))
		g.Expect(object.Status.Phase).To(Equal("Ready"))
		g.Expect(object.Status.DeploymentMode).To(Equal("StatefulSetSharded"))
		g.Expect(object.Status.DesiredPods).To(Equal(int32(2)))
		g.Expect(object.Status.AvailablePods).To(Equal(int32(2)))
	}, 5*time.Minute, 2*time.Second).Should(Succeed())
	return body
}

func assertGH61GeneratedConfig(contents, clusterID string) {
	GinkgoHelper()
	// 结构化解析 Operator 产物，同时验证默认 Agent 身份、保护标签和指定 tenant 写入端点。
	var generated struct {
		Global struct {
			ExternalLabels map[string]string `json:"external_labels"`
		} `json:"global"`
		RemoteWrite []struct {
			Name string `json:"name"`
			URL  string `json:"url"`
		} `json:"remote_write"`
	}
	Expect(yaml.Unmarshal([]byte(contents), &generated)).To(Succeed())
	Expect(generated.Global.ExternalLabels).To(SatisfyAll(
		HaveKeyWithValue("mih_cluster_id", clusterID),
		HaveKeyWithValue("prometheus", testNamespace+"/"+gh61AgentName),
		Not(HaveKey("prometheus_replica")),
	))
	Expect(generated.RemoteWrite).To(ContainElement(SatisfyAll(
		HaveField("Name", gh61RemoteWriteName),
		HaveField("URL", "http://vminsert.monitor-in-hand-test.svc:8480/insert/61/prometheus/api/v1/write"),
	)))
}

func waitForGH61EndpointIPs(ctx context.Context) []string {
	GinkgoHelper()
	var addresses []string
	// 等待分片 StatefulSet 的 Service 发布两个完整 metrics endpoint，再返回可与 Agent targets 对比的稳定 IP 集合。
	Eventually(func(g Gomega) {
		body, err := kubectlResult(ctx, "--namespace", testNamespace, "get", "endpoints", "ksm-"+gh61KSMName, "--output", "json")
		g.Expect(err).NotTo(HaveOccurred(), body)
		var endpoints struct {
			Subsets []struct {
				Addresses []struct {
					IP string `json:"ip"`
				} `json:"addresses"`
				Ports []struct {
					Name string `json:"name"`
				} `json:"ports"`
			} `json:"subsets"`
		}
		g.Expect(json.Unmarshal([]byte(body), &endpoints)).To(Succeed())
		addresses = addresses[:0]
		for _, subset := range endpoints.Subsets {
			g.Expect(subset.Ports).To(ContainElement(HaveField("Name", "http-metrics")))
			for _, address := range subset.Addresses {
				addresses = append(addresses, address.IP)
			}
		}
		g.Expect(addresses).To(HaveLen(2))
	}, 3*time.Minute, 2*time.Second).Should(Succeed())
	sort.Strings(addresses)
	return append([]string(nil), addresses...)
}

func targetHosts(targets []string) []string {
	GinkgoHelper()
	hosts := make([]string, 0, len(targets))
	for _, target := range targets {
		parsed, err := url.Parse(target)
		Expect(err).NotTo(HaveOccurred())
		hosts = append(hosts, parsed.Hostname())
	}
	sort.Strings(hosts)
	return hosts
}

func waitForGH61Series(ctx context.Context, accountID, testLabel, clusterID string) string {
	GinkgoHelper()
	return waitForGH61QueryResult(ctx, accountID, testLabel, clusterID, func(g Gomega, result gh61QueryResponse, body string) {
		g.Expect(result.Data.Result).NotTo(BeEmpty(), body)
		for _, series := range result.Data.Result {
			g.Expect(series.Metric).To(SatisfyAll(
				HaveKeyWithValue("mih_test_run_id", testLabel),
				HaveKeyWithValue("mih_cluster_id", clusterID),
				HaveKeyWithValue("prometheus", testNamespace+"/"+gh61AgentName),
				Not(HaveKey("prometheus_replica")),
			))
		}
	})
}

func waitForNoGH61Series(ctx context.Context, accountID, testLabel, clusterID string) string {
	GinkgoHelper()
	return waitForGH61QueryResult(ctx, accountID, testLabel, clusterID, func(g Gomega, result gh61QueryResponse, body string) {
		g.Expect(result.Data.Result).To(BeEmpty(), body)
	})
}

func waitForGH61QueryResult(ctx context.Context, accountID, testLabel, clusterID string, verify func(Gomega, gh61QueryResponse, string)) string {
	GinkgoHelper()
	// 把唯一测试标签与集群身份同时纳入 PromQL，避免其他 tenant 或历史样本造成假阳性。
	query := fmt.Sprintf(`kube_pod_info{mih_test_run_id=%q,mih_cluster_id=%q}`, testLabel, clusterID)
	path := fmt.Sprintf("/select/%s/prometheus/api/v1/query?query=%s", accountID, url.QueryEscape(query))
	return waitForGH61HTTPBody(ctx, "service/vmselect", 8481, path, 5*time.Minute, func(g Gomega, contents []byte) {
		var result gh61QueryResponse
		g.Expect(json.Unmarshal(contents, &result)).To(Succeed())
		g.Expect(result.Status).To(Equal("success"), string(contents))
		verify(g, result, string(contents))
	})
}

func waitForGH61Metrics(ctx context.Context, target string, remotePort int, accepted func(string) bool) string {
	GinkgoHelper()
	return waitForGH61HTTPBody(ctx, target, remotePort, "/metrics", 2*time.Minute, func(g Gomega, contents []byte) {
		g.Expect(accepted(string(contents))).To(BeTrue(), string(contents))
	})
}

func waitForGH61HTTPBody(ctx context.Context, target string, remotePort int, path string, timeout time.Duration, verify func(Gomega, []byte)) string {
	GinkgoHelper()
	// 先为当前观察面建立独立端口转发，并把子进程回收绑定到场景清理。
	port, stop := startPortForwardTarget(ctx, testNamespace, target, remotePort)
	DeferCleanup(stop)
	client := &http.Client{Timeout: 5 * time.Second}
	var body string

	// 再以有界轮询等待 HTTP 观察面可用且业务断言成立，保留最后一次完整响应作为证据。
	Eventually(func(g Gomega) {
		response, err := client.Get("http://127.0.0.1:" + strconv.Itoa(port) + path)
		g.Expect(err).NotTo(HaveOccurred())
		defer response.Body.Close()
		contents, err := io.ReadAll(response.Body)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(response.StatusCode).To(Equal(http.StatusOK), string(contents))
		body = string(contents)
		verify(g, contents)
	}, timeout, 2*time.Second).Should(Succeed())
	return body
}

func collectGH61Evidence(ctx context.Context, testLabel, clusterID string) {
	GinkgoHelper()
	// 先在删除 fixture 前独立保存持久化工作负载、PVC 和控制面对象，单项失败不中断其他证据。
	commands := []struct {
		name string
		args []string
	}{
		{name: "final-statefulset-pvc", args: []string{"--namespace", testNamespace, "get", "statefulset,pvc", "--output", "yaml"}},
		{name: "final-ksm", args: []string{"get", "kubestatemetrics", gh61KSMName, "--output", "yaml"}},
		{name: "final-agent", args: []string{"--namespace", testNamespace, "get", "prometheusagent", gh61AgentName, "--output", "yaml"}},
	}
	for _, command := range commands {
		output, err := kubectlResult(ctx, command.args...)
		AddReportEntry(command.name, evidenceBody(output, err), ReportEntryVisibilityFailureOrVerbose)
	}

	// 再对每个数据面观察点做一次有界尽力读取，确保 vmselect 查询失败时仍会尝试保存 remote-write 指标。
	query := fmt.Sprintf(`kube_pod_info{mih_test_run_id=%q,mih_cluster_id=%q}`, testLabel, clusterID)
	observations := []struct {
		name       string
		target     string
		remotePort int
		path       string
	}{
		{name: "final-vmselect-query", target: "service/vmselect", remotePort: 8481, path: "/select/" + gh61AccountID + "/prometheus/api/v1/query?query=" + url.QueryEscape(query)},
		{name: "final-wrong-tenant-query", target: "service/vmselect", remotePort: 8481, path: "/select/0/prometheus/api/v1/query?query=" + url.QueryEscape(query)},
		{name: "final-shard-0-remote-write-metrics", target: "pod/prom-agent-" + gh61AgentName + "-0", remotePort: 9090, path: "/metrics"},
		{name: "final-shard-1-remote-write-metrics", target: "pod/prom-agent-" + gh61AgentName + "-shard-1-0", remotePort: 9090, path: "/metrics"},
		{name: "final-vminsert-metrics", target: "service/vminsert", remotePort: 8480, path: "/metrics"},
	}
	type observationResult struct {
		index int
		body  string
		err   error
	}
	results := make(chan observationResult, len(observations))
	for index, observation := range observations {
		go func(index int, observation struct {
			name       string
			target     string
			remotePort int
			path       string
		}) {
			body, err := bestEffortGH61HTTPBody(ctx, observation.target, observation.remotePort, observation.path)
			results <- observationResult{index: index, body: body, err: err}
		}(index, observation)
	}
	collected := make([]observationResult, len(observations))
	for range observations {
		result := <-results
		collected[result.index] = result
	}
	for index, observation := range observations {
		AddReportEntry(observation.name, evidenceBody(collected[index].body, collected[index].err), ReportEntryVisibilityFailureOrVerbose)
	}
}

func bestEffortGH61HTTPBody(ctx context.Context, target string, remotePort int, path string) (string, error) {
	// 启动阶段：为单个观察点建立短生命周期端口转发，并注册无断言的子进程清理。
	evidenceCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	command := exec.CommandContext(evidenceCtx, "kubectl", "--context", config.kubeContext, "--namespace", testNamespace,
		"port-forward", target, fmt.Sprintf("0:%d", remotePort))
	stdout, err := command.StdoutPipe()
	if err != nil {
		return "", err
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		return "", err
	}
	if err := command.Start(); err != nil {
		return "", err
	}
	defer func() {
		if command.Process != nil {
			_ = command.Process.Kill()
		}
		_ = command.Wait()
	}()

	// 就绪阶段：并行读取 stdout/stderr，等待 kubectl 报告实际绑定的本地端口。
	lines := make(chan string, 8)
	readLines := func(reader io.Reader) {
		scanner := bufio.NewScanner(reader)
		for scanner.Scan() {
			select {
			case lines <- scanner.Text():
			case <-evidenceCtx.Done():
				return
			}
		}
	}
	go readLines(stdout)
	go readLines(stderr)
	pattern := regexp.MustCompile(`Forwarding from 127\.0\.0\.1:(\d+)`)
	port := 0
	for port == 0 {
		select {
		case line := <-lines:
			match := pattern.FindStringSubmatch(line)
			if len(match) == 2 {
				port, _ = strconv.Atoi(match[1])
			}
		case <-evidenceCtx.Done():
			return "", fmt.Errorf("端口转发 %s 未就绪: %w", target, evidenceCtx.Err())
		}
	}

	// 取证阶段：对已就绪的观察面执行一次有界 HTTP 请求，原样保留非 200 响应体。
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Get("http://127.0.0.1:" + strconv.Itoa(port) + path)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	contents, err := io.ReadAll(response.Body)
	if err != nil {
		return "", err
	}
	if response.StatusCode != http.StatusOK {
		return string(contents), fmt.Errorf("HTTP status %s", response.Status)
	}
	return string(contents), nil
}

func evidenceBody(body string, err error) string {
	if err == nil {
		return body
	}
	return fmt.Sprintf("error: %v\n%s", err, body)
}

func metricSample(contents, metricName, requiredLabel string) (float64, bool) {
	for _, line := range strings.Split(contents, "\n") {
		if !strings.HasPrefix(line, metricName+"{") || !strings.Contains(line, requiredLabel) {
			continue
		}
		closingBrace := strings.LastIndexByte(line, '}')
		if closingBrace == -1 {
			continue
		}
		fields := strings.Fields(line[closingBrace+1:])
		if len(fields) == 0 {
			continue
		}
		value, err := strconv.ParseFloat(fields[0], 64)
		if err == nil {
			return value, true
		}
	}
	return 0, false
}
