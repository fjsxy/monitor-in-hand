package collection_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const (
	gh63AgentName        = "gh63-multi-remote"
	gh63PrimaryAccount   = "63"
	gh63SecondaryAccount = "630"
)

type gh63RemoteTarget struct {
	Name string
	URL  string
}

type gh63QueueSnapshot struct {
	Sent           float64
	Retried        float64
	Pending        float64
	HighestSent    float64
	EnqueueRetries float64
}

var (
	gh63PrimaryTarget   = gh63RemoteTarget{Name: "gh63-primary", URL: "http://vminsert.monitor-in-hand-test.svc:8480/insert/63/prometheus/api/v1/write"}
	gh63SecondaryTarget = gh63RemoteTarget{Name: "gh63-secondary", URL: "http://gh63-secondary.monitor-in-hand-test.svc:8480/insert/630/prometheus/api/v1/write"}
	gh63V2Target        = gh63RemoteTarget{Name: "gh63-v2", URL: "http://vminsert.monitor-in-hand-test.svc:8480/insert/633/prometheus/api/v1/write"}
)

type gh63QueryResponse struct {
	Status string `json:"status"`
	Data   struct {
		Result []struct {
			Metric map[string]string `json:"metric"`
			Value  []any             `json:"value"`
			Values [][]any           `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

var _ = Describe("多目标 remote_write 故障语义", Serial, func() {
	It(fmt.Sprintf("[%s] 隔离第二目标故障并从共享 WAL 追赶故障期样本", gh63CollectionScenarioID("001")), func(ctx SpecContext) {
		scenarioID := gh63CollectionScenarioID("001")
		runLabel := "gh63-" + config.testRunID
		baseQuery := fmt.Sprintf(`node_uname_info{mih_gh63_run=%q}`, runLabel)

		By(scenarioID + " 创建可切换 alias、429 responder、采集配置与单 WAL 双队列 Agent")
		DeferCleanup(func(cleanupCtx SpecContext) {
			deleteResource(cleanupCtx, "prometheusagent", gh63AgentName, testNamespace)
			deleteResource(cleanupCtx, "persistentvolumeclaim", "prom-agent-gh63-multi-remote-db-prom-agent-gh63-multi-remote-0", testNamespace)
			deleteResource(cleanupCtx, "servicemonitor", "gh63-outage", testNamespace)
			deleteResource(cleanupCtx, "servicemonitor", "gh63-base", testNamespace)
			deleteResource(cleanupCtx, "service", "gh63-secondary", testNamespace)
			deleteResource(cleanupCtx, "deployment", "gh63-rate-limit", testNamespace)
			deleteResource(cleanupCtx, "configmap", "gh63-rate-limit", testNamespace)
		})
		applyManifest(ctx, gh63RateLimiterManifest)
		waitForGH63Rollout(ctx, "deployment/gh63-rate-limit")
		applyManifest(ctx, gh63SecondaryServiceManifest)
		applyManifest(ctx, gh63BaseServiceMonitorManifest)
		applyManifest(ctx, gh63AgentManifest(runLabel))
		waitForAgentConditions(ctx, gh63AgentName)

		By(scenarioID + " 验证显式一期字段、无默认注入、版本门控字段与 WAL 压缩参数")
		generated := waitForGeneratedConfig(ctx, gh63AgentName)
		AddReportEntry("gh63-generated-config", generated, ReportEntryVisibilityFailureOrVerbose)
		Expect(validateGH63GeneratedConfig(generated)).To(Succeed())
		statefulSet := kubectl(ctx, "--namespace", testNamespace, "get", "statefulset", "prom-agent-"+gh63AgentName, "--output", "json")
		Expect(statefulSet).To(ContainSubstring("--storage.agent.wal-compression"))
		AddReportEntry("gh63-agent-statefulset", statefulSet, ReportEntryVisibilityFailureOrVerbose)

		By(scenarioID + " 从两个 accountID 查询同一序列，证明 remote_write 是复制而非负载均衡或 failover")
		primaryInitial := waitForGH63InstantSeries(ctx, gh63PrimaryAccount, baseQuery)
		secondaryInitial := waitForGH63InstantSeries(ctx, gh63SecondaryAccount, baseQuery)
		AddReportEntry("gh63-primary-initial-query", primaryInitial, ReportEntryVisibilityFailureOrVerbose)
		AddReportEntry("gh63-secondary-initial-query", secondaryInitial, ReportEntryVisibilityFailureOrVerbose)

		By(scenarioID + " 记录故障前两条 remote_write 队列基线")
		pod := "prom-agent-" + gh63AgentName + "-0"
		initialMetrics := waitForGH63AgentMetrics(ctx, pod, func(contents string) bool {
			primary, primaryFound := gh63ReadQueueSnapshot(contents, gh63PrimaryTarget)
			secondary, secondaryFound := gh63ReadQueueSnapshot(contents, gh63SecondaryTarget)
			return primaryFound && secondaryFound && primary.Sent > 0 && secondary.Sent > 0
		})
		initialPrimary, _ := gh63ReadQueueSnapshot(initialMetrics, gh63PrimaryTarget)
		initialSecondary, _ := gh63ReadQueueSnapshot(initialMetrics, gh63SecondaryTarget)
		initialSecondaryLag := float64(time.Now().Unix()) - initialSecondary.HighestSent

		By(scenarioID + " 切断第二目标 alias，并验证第一目标持续前进而第二队列独立积压、重试与增加 lag")
		setGH63SecondarySelector(ctx, map[string]string{"monitoring.fjsxy.github.io/gh63-route": "disconnected"})
		outageMetrics := waitForGH63AgentMetrics(ctx, pod, func(contents string) bool {
			primary, primaryFound := gh63ReadQueueSnapshot(contents, gh63PrimaryTarget)
			secondary, secondaryFound := gh63ReadQueueSnapshot(contents, gh63SecondaryTarget)
			now := float64(time.Now().Unix())
			return primaryFound && secondaryFound && primary.Sent > initialPrimary.Sent && primary.Retried == initialPrimary.Retried &&
				secondary.Retried > initialSecondary.Retried && secondary.Retried > primary.Retried &&
				secondary.Pending > initialSecondary.Pending && secondary.Pending > primary.Pending && secondary.EnqueueRetries > 0 &&
				now-secondary.HighestSent > initialSecondaryLag+3 && now-secondary.HighestSent > now-primary.HighestSent+3
		})
		AddReportEntry("gh63-outage-queue-metrics", outageMetrics, ReportEntryVisibilityFailureOrVerbose)
		primaryDuringOutage := waitForGH63InstantSeries(ctx, gh63PrimaryAccount, baseQuery)
		AddReportEntry("gh63-primary-during-outage-query", primaryDuringOutage, ReportEntryVisibilityFailureOrVerbose)

		By(scenarioID + " 将 alias 指向 429 responder，验证 retryOnRateLimit 使 retry 计数继续增长")
		outageRetry, _ := gh63QueueMetric(outageMetrics, "prometheus_remote_storage_samples_retried_total", gh63SecondaryTarget)
		setGH63SecondarySelector(ctx, map[string]string{"app.kubernetes.io/name": "gh63-rate-limit"})
		rateLimitedMetrics := waitForGH63AgentMetrics(ctx, pod, func(contents string) bool {
			retried, retryFound := gh63QueueMetric(contents, "prometheus_remote_storage_samples_retried_total", gh63SecondaryTarget)
			dropped, droppedFound := gh63QueueMetric(contents, "prometheus_remote_storage_samples_dropped_total", gh63SecondaryTarget)
			return retryFound && droppedFound && retried > outageRetry && dropped > 0
		})
		var rateLimiterLogs string
		Eventually(func(g Gomega) {
			var err error
			rateLimiterLogs, err = kubectlResult(ctx, "--namespace", testNamespace, "logs", "deployment/gh63-rate-limit")
			g.Expect(err).NotTo(HaveOccurred(), rateLimiterLogs)
			g.Expect(rateLimiterLogs).To(ContainSubstring(" 429 "))
		}, 2*time.Minute, 2*time.Second).Should(Succeed())
		AddReportEntry("gh63-rate-limited-queue-metrics", rateLimitedMetrics, ReportEntryVisibilityFailureOrVerbose)
		AddReportEntry("gh63-rate-limiter-logs", rateLimiterLogs, ReportEntryVisibilityFailureOrVerbose)

		By(scenarioID + " 仅在故障期创建唯一序列，并先从健康 tenant 确认样本已经进入 Agent WAL")
		outageLabel := "outage-" + config.testRunID
		outageQuery := fmt.Sprintf(`node_uname_info{mih_gh63_run=%q,mih_gh63_outage=%q}`, runLabel, outageLabel)
		outageStarted := time.Now().Add(-5 * time.Second)
		applyManifest(ctx, gh63OutageServiceMonitorManifest(outageLabel))
		primaryOutageSample := waitForGH63InstantSeries(ctx, gh63PrimaryAccount, outageQuery)
		deleteResource(ctx, "servicemonitor", "gh63-outage", testNamespace)
		recoveryCutoff := time.Now()
		AddReportEntry("gh63-primary-outage-sample", primaryOutageSample, ReportEntryVisibilityFailureOrVerbose)

		By(scenarioID + " 恢复 alias 并验证第二队列从共享 WAL 发送恢复前时间戳的样本")
		setGH63SecondarySelector(ctx, map[string]string{"app.kubernetes.io/name": "vminsert"})
		secondaryRecovered := waitForGH63HistoricalSeries(ctx, gh63SecondaryAccount, outageQuery, outageStarted, recoveryCutoff)
		primaryHistorical := waitForGH63HistoricalSeries(ctx, gh63PrimaryAccount, outageQuery, outageStarted, recoveryCutoff)
		AddReportEntry("gh63-secondary-recovered-query", secondaryRecovered, ReportEntryVisibilityFailureOrVerbose)
		AddReportEntry("gh63-primary-historical-query", primaryHistorical, ReportEntryVisibilityFailureOrVerbose)

		By(scenarioID + " 等待两 tenant 数据面恢复并保存每目标成功、失败、重试、丢弃、pending、lag、shards、enqueue retry 与 reload 指标")
		finalPrimary := waitForGH63InstantSeries(ctx, gh63PrimaryAccount, baseQuery)
		finalSecondary := waitForGH63InstantSeries(ctx, gh63SecondaryAccount, baseQuery)
		finalMetrics := waitForGH63AgentMetrics(ctx, pod, func(contents string) bool {
			secondaryPending, pendingFound := gh63QueueMetric(contents, "prometheus_remote_storage_samples_pending", gh63SecondaryTarget)
			secondaryHighest, highestFound := gh63QueueMetric(contents, "prometheus_remote_storage_queue_highest_sent_timestamp_seconds", gh63SecondaryTarget)
			reloadOK, reloadFound := gh63GlobalMetric(contents, "prometheus_config_last_reload_successful")
			return pendingFound && highestFound && reloadFound && secondaryPending == 0 && float64(time.Now().Unix())-secondaryHighest < 15 && reloadOK == 1 &&
				gh63HasQueueMetricFamilies(contents, gh63PrimaryTarget) && gh63HasQueueMetricFamilies(contents, gh63SecondaryTarget)
		})
		AddReportEntry("gh63-primary-final-query", finalPrimary, ReportEntryVisibilityFailureOrVerbose)
		AddReportEntry("gh63-secondary-final-query", finalSecondary, ReportEntryVisibilityFailureOrVerbose)
		AddReportEntry("gh63-final-queue-and-reload-metrics", finalMetrics, ReportEntryVisibilityFailureOrVerbose)
		// 最终只断言故障期样本存在，不比较两 tenant 的样本次数，因此不承诺 exactly-once。
	}, SpecTimeout(12*time.Minute))

	It(fmt.Sprintf("[%s] 拒绝重名与非法 URL 整配置并继续运行旧配置", gh63CollectionScenarioID("002")), func(ctx SpecContext) {
		scenarioID := gh63CollectionScenarioID("002")
		runLabel := "reload-" + config.testRunID
		query := fmt.Sprintf(`node_uname_info{mih_gh63_reload=%q}`, runLabel)
		pod := "prom-agent-gh63-reload-0"

		By(scenarioID + " 创建两个稳定唯一名称的有效目标并记录成功 reload 基线")
		DeferCleanup(func(cleanupCtx SpecContext) {
			deleteResource(cleanupCtx, "prometheusagent", "gh63-reload", testNamespace)
		})
		applyManifest(ctx, gh63ReloadAgentManifest(runLabel, gh63ReloadValid))
		waitForAgentConditions(ctx, "gh63-reload")
		statefulSet := kubectl(ctx, "--namespace", testNamespace, "get", "statefulset", "prom-agent-gh63-reload", "--output", "json")
		Expect(statefulSet).NotTo(ContainSubstring("--storage.agent.wal-compression"))
		Expect(statefulSet).NotTo(ContainSubstring("--no-storage.agent.wal-compression"))
		waitForGH63InstantSeries(ctx, "631", query)
		waitForGH63InstantSeries(ctx, "632", query)
		baselineMetrics := waitForGH63AgentMetrics(ctx, pod, func(contents string) bool {
			success, found := gh63GlobalMetric(contents, "prometheus_config_last_reload_successful")
			return found && success == 1
		})
		baselineReloadTimestamp, found := gh63GlobalMetric(baselineMetrics, "prometheus_config_last_reload_success_timestamp_seconds")
		Expect(found).To(BeTrue())

		By(scenarioID + " 提交重复 name，等待 reload 失败并验证活动配置与两个数据面保持旧值")
		applyManifest(ctx, gh63ReloadAgentManifest(runLabel, gh63ReloadDuplicate))
		waitForGH63GeneratedSubstring(ctx, "gh63-reload", "name: gh63-duplicate")
		duplicateMetrics := waitForGH63FailedReload(ctx, pod, baselineReloadTimestamp)
		duplicateLogs := waitForGH63PrometheusLog(ctx, pod, `found multiple remote write configs with job name "gh63-duplicate"`)
		activeAfterDuplicate := waitForGH63ActiveConfig(ctx, pod, "gh63-reload-primary", "gh63-reload-secondary")
		waitForGH63InstantSeries(ctx, "631", query)
		waitForGH63InstantSeries(ctx, "632", query)
		AddReportEntry("gh63-duplicate-reload-metrics", duplicateMetrics, ReportEntryVisibilityFailureOrVerbose)
		AddReportEntry("gh63-duplicate-prometheus-logs", duplicateLogs, ReportEntryVisibilityFailureOrVerbose)
		AddReportEntry("gh63-active-config-after-duplicate", activeAfterDuplicate, ReportEntryVisibilityFailureOrVerbose)

		By(scenarioID + " 提交非法 URL，验证整份新配置仍不生效且旧目标继续发送")
		applyManifest(ctx, gh63ReloadAgentManifest(runLabel, gh63ReloadInvalidURL))
		waitForGH63GeneratedSubstring(ctx, "gh63-reload", "http://[::1")
		invalidURLMetrics := waitForGH63FailedReload(ctx, pod, baselineReloadTimestamp)
		invalidURLLogs := waitForGH63PrometheusLog(ctx, pod, "missing ']' in host")
		activeAfterInvalidURL := waitForGH63ActiveConfig(ctx, pod, "gh63-reload-primary", "gh63-reload-secondary")
		waitForGH63InstantSeries(ctx, "631", query)
		waitForGH63InstantSeries(ctx, "632", query)
		AddReportEntry("gh63-invalid-url-reload-metrics", invalidURLMetrics, ReportEntryVisibilityFailureOrVerbose)
		AddReportEntry("gh63-invalid-url-prometheus-logs", invalidURLLogs, ReportEntryVisibilityFailureOrVerbose)
		AddReportEntry("gh63-active-config-after-invalid-url", activeAfterInvalidURL, ReportEntryVisibilityFailureOrVerbose)

		By(scenarioID + " 恢复有效声明并等待 reload 指标重新成功")
		applyManifest(ctx, gh63ReloadAgentManifest(runLabel, gh63ReloadValid))
		recoveredMetrics := waitForGH63AgentMetrics(ctx, pod, func(contents string) bool {
			success, successFound := gh63GlobalMetric(contents, "prometheus_config_last_reload_successful")
			timestamp, timestampFound := gh63GlobalMetric(contents, "prometheus_config_last_reload_success_timestamp_seconds")
			return successFound && timestampFound && success == 1 && timestamp > baselineReloadTimestamp
		})
		AddReportEntry("gh63-recovered-reload-metrics", recoveredMetrics, ReportEntryVisibilityFailureOrVerbose)
	}, SpecTimeout(8*time.Minute))

	It(fmt.Sprintf("[%s] 将 V2.0 保持为 VictoriaMetrics 当前版本的负向场景", gh63CollectionScenarioID("003")), func(ctx SpecContext) {
		scenarioID := gh63CollectionScenarioID("003")
		runLabel := "v2-" + config.testRunID
		query := fmt.Sprintf(`node_uname_info{mih_gh63_v2=%q}`, runLabel)
		pod := "prom-agent-gh63-v2-0"

		By(scenarioID + " 创建上游接受的 messageVersion=V2.0 Agent")
		DeferCleanup(func(cleanupCtx SpecContext) {
			deleteResource(cleanupCtx, "prometheusagent", "gh63-v2", testNamespace)
		})
		applyManifest(ctx, gh63V2AgentManifest(runLabel))
		waitForAgentConditions(ctx, "gh63-v2")
		generated := waitForGeneratedConfig(ctx, "gh63-v2")
		Expect(generated).To(ContainSubstring("protobuf_message: io.prometheus.write.v2.Request"))
		AddReportEntry("gh63-v2-generated-config", generated, ReportEntryVisibilityFailureOrVerbose)

		By(scenarioID + " 等待 V2 发送尝试后持续确认当前 VictoriaMetrics tenant 无正确摄入结果")
		metrics := waitForGH63AgentMetrics(ctx, pod, func(contents string) bool {
			sent, found := gh63QueueMetric(contents, "prometheus_remote_storage_samples_total", gh63V2Target)
			return found && sent > 0
		})
		noSeries := consistentlyNoGH63Series(ctx, "633", query, 20*time.Second)
		AddReportEntry("gh63-v2-queue-metrics", metrics, ReportEntryVisibilityFailureOrVerbose)
		AddReportEntry("gh63-v2-negative-query", noSeries, ReportEntryVisibilityFailureOrVerbose)
	}, SpecTimeout(5*time.Minute))
})

func gh63CollectionScenarioID(sequence string) string {
	ids := map[string]map[string]string{
		"1.19": {
			"001": "E2E-119-COLLECTION-GH63-001",
			"002": "E2E-119-COLLECTION-GH63-002",
			"003": "E2E-119-COLLECTION-GH63-003",
		},
		"1.28": {
			"001": "E2E-128-COLLECTION-GH63-001",
			"002": "E2E-128-COLLECTION-GH63-002",
			"003": "E2E-128-COLLECTION-GH63-003",
		},
	}
	if versionIDs, supported := ids[config.kubernetesMinor]; supported {
		if id, found := versionIDs[sequence]; found {
			return id
		}
	}
	return "E2E-UNSUPPORTED-COLLECTION-GH63-" + sequence
}

func waitForGH63Rollout(ctx context.Context, resource string) {
	GinkgoHelper()
	output, err := kubectlResult(ctx, "--namespace", testNamespace, "rollout", "status", resource, "--timeout=3m")
	Expect(err).NotTo(HaveOccurred(), output)
}

func setGH63SecondarySelector(ctx context.Context, selector map[string]string) {
	GinkgoHelper()
	value, err := json.Marshal([]map[string]any{{"op": "replace", "path": "/spec/selector", "value": selector}})
	Expect(err).NotTo(HaveOccurred())
	output, err := kubectlResult(ctx, "--namespace", testNamespace, "patch", "service", "gh63-secondary", "--type=json", "--patch", string(value))
	Expect(err).NotTo(HaveOccurred(), output)
}

func waitForGH63AgentMetrics(ctx context.Context, pod string, accepted func(string) bool) string {
	GinkgoHelper()
	return waitForGH61Metrics(ctx, "pod/"+pod, 9090, accepted)
}

func gh63QueueMetric(contents, metricName string, target gh63RemoteTarget) (float64, bool) {
	value, found, err := metricSumWithLabels(contents, metricName, map[string]string{"remote_name": target.Name, "url": target.URL})
	if err != nil {
		return 0, false
	}
	return value, found
}

func gh63ReadQueueSnapshot(contents string, target gh63RemoteTarget) (gh63QueueSnapshot, bool) {
	// 将故障前后反复比较的队列维度一次读取，确保每个值都来自同一 remote_name/url 身份。
	snapshot := gh63QueueSnapshot{}
	metrics := []struct {
		name        string
		destination *float64
	}{
		{name: "prometheus_remote_storage_samples_total", destination: &snapshot.Sent},
		{name: "prometheus_remote_storage_samples_retried_total", destination: &snapshot.Retried},
		{name: "prometheus_remote_storage_samples_pending", destination: &snapshot.Pending},
		{name: "prometheus_remote_storage_queue_highest_sent_timestamp_seconds", destination: &snapshot.HighestSent},
		{name: "prometheus_remote_storage_enqueue_retries_total", destination: &snapshot.EnqueueRetries},
	}
	for _, metric := range metrics {
		value, found := gh63QueueMetric(contents, metric.name, target)
		if !found {
			return gh63QueueSnapshot{}, false
		}
		*metric.destination = value
	}
	return snapshot, true
}

func gh63GlobalMetric(contents, metricName string) (float64, bool) {
	value, found, err := metricSumWithLabels(contents, metricName, nil)
	if err != nil {
		return 0, false
	}
	return value, found
}

func gh63HasQueueMetricFamilies(contents string, target gh63RemoteTarget) bool {
	for _, metricName := range []string{
		"prometheus_remote_storage_samples_total",
		"prometheus_remote_storage_samples_failed_total",
		"prometheus_remote_storage_samples_retried_total",
		"prometheus_remote_storage_samples_pending",
		"prometheus_remote_storage_queue_highest_sent_timestamp_seconds",
		"prometheus_remote_storage_shards",
		"prometheus_remote_storage_enqueue_retries_total",
	} {
		if _, found := gh63QueueMetric(contents, metricName, target); !found {
			return false
		}
	}
	return true
}

func waitForGH63InstantSeries(ctx context.Context, accountID, query string) string {
	GinkgoHelper()
	path := fmt.Sprintf("/select/%s/prometheus/api/v1/query?query=%s", accountID, url.QueryEscape(query))
	return waitForGH61HTTPBody(ctx, "service/vmselect", 8481, path, 3*time.Minute, func(g Gomega, contents []byte) {
		var response gh63QueryResponse
		g.Expect(json.Unmarshal(contents, &response)).To(Succeed())
		g.Expect(response.Status).To(Equal("success"), string(contents))
		g.Expect(response.Data.Result).NotTo(BeEmpty(), string(contents))
	})
}

func waitForGH63HistoricalSeries(ctx context.Context, accountID, query string, start, cutoff time.Time) string {
	GinkgoHelper()
	end := time.Now().Add(30 * time.Second)
	path := fmt.Sprintf("/select/%s/prometheus/api/v1/query_range?query=%s&start=%d&end=%d&step=1", accountID, url.QueryEscape(query), start.Unix(), end.Unix())
	return waitForGH61HTTPBody(ctx, "service/vmselect", 8481, path, 3*time.Minute, func(g Gomega, contents []byte) {
		var response gh63QueryResponse
		g.Expect(json.Unmarshal(contents, &response)).To(Succeed())
		g.Expect(response.Status).To(Equal("success"), string(contents))
		g.Expect(gh63HasSampleAtOrBefore(response, cutoff)).To(BeTrue(), string(contents))
	})
}

func gh63HasSampleAtOrBefore(response gh63QueryResponse, cutoff time.Time) bool {
	for _, series := range response.Data.Result {
		for _, sample := range series.Values {
			if len(sample) < 2 {
				continue
			}
			timestamp, ok := sample[0].(float64)
			if ok && timestamp <= float64(cutoff.Unix()) {
				return true
			}
		}
	}
	return false
}

func consistentlyNoGH63Series(ctx context.Context, accountID, query string, duration time.Duration) string {
	GinkgoHelper()
	// 先建立独立端口转发，并把子进程回收绑定到当前场景清理。
	port, stop := startPortForwardTarget(ctx, testNamespace, "service/vmselect", 8481)
	DeferCleanup(stop)
	client := &http.Client{Timeout: 5 * time.Second}
	path := fmt.Sprintf("/select/%s/prometheus/api/v1/query?query=%s", accountID, url.QueryEscape(query))
	var body string
	// 再持续观察完整时间窗口，确认负向 tenant 不是因为查询早于异步写入而短暂为空。
	Consistently(func(g Gomega) {
		response, err := client.Get("http://127.0.0.1:" + strconv.Itoa(port) + path)
		g.Expect(err).NotTo(HaveOccurred())
		defer response.Body.Close()
		contents, err := io.ReadAll(response.Body)
		g.Expect(err).NotTo(HaveOccurred())
		body = string(contents)
		g.Expect(response.StatusCode).To(Equal(http.StatusOK), body)
		var queryResponse gh63QueryResponse
		g.Expect(json.Unmarshal(contents, &queryResponse)).To(Succeed())
		g.Expect(queryResponse.Status).To(Equal("success"), body)
		g.Expect(queryResponse.Data.Result).To(BeEmpty(), body)
	}, duration, 2*time.Second).Should(Succeed())
	return body
}

func waitForGH63GeneratedSubstring(ctx context.Context, agent, expected string) string {
	GinkgoHelper()
	var generated string
	Eventually(func(g Gomega) {
		generated = waitForGeneratedConfig(ctx, agent)
		g.Expect(generated).To(ContainSubstring(expected))
	}, 2*time.Minute, 2*time.Second).Should(Succeed())
	return generated
}

func waitForGH63FailedReload(ctx context.Context, pod string, successfulTimestamp float64) string {
	GinkgoHelper()
	return waitForGH63AgentMetrics(ctx, pod, func(contents string) bool {
		success, successFound := gh63GlobalMetric(contents, "prometheus_config_last_reload_successful")
		timestamp, timestampFound := gh63GlobalMetric(contents, "prometheus_config_last_reload_success_timestamp_seconds")
		return successFound && timestampFound && success == 0 && timestamp == successfulTimestamp
	})
}

func waitForGH63PrometheusLog(ctx context.Context, pod, expected string) string {
	GinkgoHelper()
	var logs string
	Eventually(func(g Gomega) {
		var err error
		logs, err = kubectlResult(ctx, "--namespace", testNamespace, "logs", pod, "--container", "prometheus")
		g.Expect(err).NotTo(HaveOccurred(), logs)
		g.Expect(logs).To(ContainSubstring(expected))
	}, 2*time.Minute, 2*time.Second).Should(Succeed())
	return logs
}

func waitForGH63ActiveConfig(ctx context.Context, pod string, expected ...string) string {
	GinkgoHelper()
	return waitForGH61HTTPBody(ctx, "pod/"+pod, 9090, "/api/v1/status/config", 2*time.Minute, func(g Gomega, contents []byte) {
		for _, value := range expected {
			g.Expect(string(contents)).To(ContainSubstring(value))
		}
	})
}

func gh63AgentManifest(runLabel string) string {
	return fmt.Sprintf(`
apiVersion: monitoring.coreos.com/v1alpha1
kind: PrometheusAgent
metadata:
  name: gh63-multi-remote
  namespace: monitor-in-hand-test
spec:
  version: v2.55.1
  image: quay.io/prometheus/prometheus@sha256:2659f4c2ebb718e7695cb9b25ffa7d6be64db013daba13e05c875451cf51b0d3
  serviceAccountName: prometheus-agent
  replicas: 1
  shards: 1
  walCompression: true
  storage:
    volumeClaimTemplate:
      spec:
        accessModes: [ReadWriteOnce]
        resources:
          requests:
            storage: 64Mi
  externalLabels:
    mih_gh63_run: %s
  serviceMonitorSelector:
    matchLabels:
      monitoring.fjsxy.github.io/gh63: "true"
  remoteWrite:
    - name: gh63-primary
      url: http://vminsert.monitor-in-hand-test.svc:8480/insert/63/prometheus/api/v1/write
      messageVersion: V1.0
    - name: gh63-secondary
      url: http://gh63-secondary.monitor-in-hand-test.svc:8480/insert/630/prometheus/api/v1/write
      messageVersion: V1.0
      remoteTimeout: 5s
      roundRobinDNS: true
      queueConfig:
        capacity: 10
        minShards: 1
        maxShards: 1
        maxSamplesPerSend: 5
        batchSendDeadline: 1s
        maxRetries: 7
        minBackoff: 100ms
        maxBackoff: 1s
        retryOnRateLimit: true
        sampleAgeLimit: 30s
`, runLabel)
}

const gh63BaseServiceMonitorManifest = `
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: gh63-base
  namespace: monitor-in-hand-test
  labels:
    monitoring.fjsxy.github.io/gh63: "true"
spec:
  selector:
    matchLabels:
      app.kubernetes.io/name: metrics-target
  endpoints:
    - port: metrics
      interval: 2s
`

func gh63OutageServiceMonitorManifest(outageLabel string) string {
	return fmt.Sprintf(`
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: gh63-outage
  namespace: monitor-in-hand-test
  labels:
    monitoring.fjsxy.github.io/gh63: "true"
spec:
  selector:
    matchLabels:
      app.kubernetes.io/name: metrics-target
  endpoints:
    - port: metrics
      interval: 2s
      metricRelabelings:
        - targetLabel: mih_gh63_outage
          replacement: %s
`, outageLabel)
}

const gh63SecondaryServiceManifest = `
apiVersion: v1
kind: Service
metadata:
  name: gh63-secondary
  namespace: monitor-in-hand-test
spec:
  selector:
    app.kubernetes.io/name: vminsert
  ports:
    - name: http
      port: 8480
      targetPort: 8480
`

const gh63RateLimiterManifest = `
apiVersion: v1
kind: ConfigMap
metadata:
  name: gh63-rate-limit
  namespace: monitor-in-hand-test
data:
  nginx.conf: |
    events {}
    http {
      access_log /dev/stdout;
      server {
        listen 8480;
        location = /health { return 200 "ok\n"; }
        location / { return 429; }
      }
    }
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: gh63-rate-limit
  namespace: monitor-in-hand-test
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: gh63-rate-limit
  template:
    metadata:
      labels:
        app.kubernetes.io/name: gh63-rate-limit
    spec:
      containers:
        - name: nginx
          image: docker.io/library/nginx@sha256:65645c7bb6a0661892a8b03b89d0743208a18dd2f3f17a54ef4b76fb8e2f2a10
          ports:
            - name: http
              containerPort: 8480
          readinessProbe:
            httpGet:
              path: /health
              port: http
            periodSeconds: 2
          volumeMounts:
            - name: config
              mountPath: /etc/nginx/nginx.conf
              subPath: nginx.conf
              readOnly: true
      volumes:
        - name: config
          configMap:
            name: gh63-rate-limit
`

type gh63ReloadMode string

const (
	gh63ReloadValid      gh63ReloadMode = "valid"
	gh63ReloadDuplicate  gh63ReloadMode = "duplicate"
	gh63ReloadInvalidURL gh63ReloadMode = "invalid-url"
)

func gh63ReloadAgentManifest(runLabel string, mode gh63ReloadMode) string {
	firstName := "gh63-reload-primary"
	secondName := "gh63-reload-secondary"
	secondURL := "http://vminsert.monitor-in-hand-test.svc:8480/insert/632/prometheus/api/v1/write"
	if mode == gh63ReloadDuplicate {
		firstName = "gh63-duplicate"
		secondName = "gh63-duplicate"
	}
	if mode == gh63ReloadInvalidURL {
		secondURL = "http://[::1"
	}
	return fmt.Sprintf(`
apiVersion: monitoring.coreos.com/v1alpha1
kind: PrometheusAgent
metadata:
  name: gh63-reload
  namespace: monitor-in-hand-test
spec:
  version: v2.55.1
  image: quay.io/prometheus/prometheus@sha256:2659f4c2ebb718e7695cb9b25ffa7d6be64db013daba13e05c875451cf51b0d3
  serviceAccountName: prometheus-agent
  replicas: 1
  shards: 1
  externalLabels:
    mih_gh63_reload: %s
  serviceMonitorSelector:
    matchLabels:
      monitoring.fjsxy.github.io/test-fixture: "true"
  remoteWrite:
    - name: %s
      url: http://vminsert.monitor-in-hand-test.svc:8480/insert/631/prometheus/api/v1/write
    - name: %s
      url: %q
`, runLabel, firstName, secondName, secondURL)
}

func gh63V2AgentManifest(runLabel string) string {
	return fmt.Sprintf(`
apiVersion: monitoring.coreos.com/v1alpha1
kind: PrometheusAgent
metadata:
  name: gh63-v2
  namespace: monitor-in-hand-test
spec:
  version: v2.55.1
  image: quay.io/prometheus/prometheus@sha256:2659f4c2ebb718e7695cb9b25ffa7d6be64db013daba13e05c875451cf51b0d3
  serviceAccountName: prometheus-agent
  replicas: 1
  shards: 1
  externalLabels:
    mih_gh63_v2: %s
  serviceMonitorSelector:
    matchLabels:
      monitoring.fjsxy.github.io/test-fixture: "true"
  remoteWrite:
    - name: gh63-v2
      url: http://vminsert.monitor-in-hand-test.svc:8480/insert/633/prometheus/api/v1/write
      messageVersion: V2.0
`, runLabel)
}
