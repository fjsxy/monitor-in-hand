package collection_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestCollectionHelpers(t *testing.T) {
	t.Run("[UT-COLLECTION-GH59-001] 将 kubectl 警告与授权结果分流", func(t *testing.T) {
		// 模拟 kubectl auth can-i 拒绝请求时 stdout 返回 no、stderr 返回 scope 警告。
		command := exec.CommandContext(context.Background(), os.Args[0], "-test.run=TestCollectionCommandFixture")
		command.Env = append(os.Environ(), "GO_WANT_COLLECTION_COMMAND_FIXTURE=1")
		output, err := commandResult(command)

		// 授权拒绝仍须保留非零退出，同时机器可判定输出只能包含 stdout。
		if err == nil {
			t.Fatal("expected denied authorization to return an error")
		}
		if output != "no\n" {
			t.Fatalf("expected stdout-only authorization decision, got %q", output)
		}
		if !strings.Contains(err.Error(), "resource 'nodes' is not namespace scoped") {
			t.Fatalf("expected stderr warning in returned error, got %q", err)
		}
	})

	t.Run("[UT-COLLECTION-GH59-002] 从格式化 Event JSON 读取拒绝原因", func(t *testing.T) {
		// 重放两个失败 job 都捕获到的 pretty-printed Event 形态。
		events := `{
  "items": [
    {
      "reason": "InvalidConfiguration",
      "type": "Warning"
    }
  ]
}`
		found, err := eventHasReason(events, "InvalidConfiguration")
		if err != nil {
			t.Fatalf("parse events: %v", err)
		}
		if !found {
			t.Fatal("expected InvalidConfiguration event reason")
		}
		if _, err := eventHasReason(`{"items":`, "InvalidConfiguration"); err == nil {
			t.Fatal("expected malformed event JSON to return an error")
		}
	})

	t.Run("[UT-COLLECTION-GH61-001] 按 metric family 和 remote name 读取数据面证据", func(t *testing.T) {
		// 重放 Prometheus text exposition 中标签顺序可变、标签逗号后带空格且 sample 使用科学计数法的形态。
		metrics := `# HELP prometheus_remote_storage_samples_total Total number of samples sent.
prometheus_remote_storage_samples_total{remote_name="other",url="http://other"} 99
prometheus_remote_storage_samples_total{url="http://vminsert",remote_name="gh61-victoriametrics-primary"} 1.25e+03
vm_http_requests_total{path="/insert/{}/prometheus/", protocol="promremotewrite"} 148
`
		value, found := metricSample(metrics, "prometheus_remote_storage_samples_total", `remote_name="gh61-victoriametrics-primary"`)
		if !found || value != 1250 {
			t.Fatalf("expected matching remote sample value 1250, got value=%v found=%v", value, found)
		}
		value, found = metricSample(metrics, "vm_http_requests_total", `protocol="promremotewrite"`)
		if !found || value != 148 {
			t.Fatalf("expected captured vminsert request value 148, got value=%v found=%v", value, found)
		}
		if _, found := metricSample(metrics, "prometheus_remote_storage_samples_total", `remote_name="missing"`); found {
			t.Fatal("unexpected sample for missing remote")
		}
	})

	t.Run("[UT-COLLECTION-GH63-001] 按 remote_name 和 url 精确汇总队列指标", func(t *testing.T) {
		// 重放同一 metric family 中多目标、多 drop reason 与可变标签顺序的形态。
		metrics := `# HELP prometheus_remote_storage_samples_dropped_total Total number of samples dropped.
# TYPE prometheus_remote_storage_samples_dropped_total counter
prometheus_remote_storage_samples_dropped_total{reason="too_old",remote_name="gh63-secondary",url="http://secondary"} 3
prometheus_remote_storage_samples_dropped_total{url="http://secondary", reason="dropped_series", remote_name="gh63-secondary"} 2
prometheus_remote_storage_samples_dropped_total{remote_name="gh63-primary",url="http://primary",reason="too_old"} 11
`
		// 按次目标完整身份执行汇总，不把主目标的同名 family 算入。
		value, found, err := metricSumWithLabels(metrics, "prometheus_remote_storage_samples_dropped_total", map[string]string{
			"remote_name": "gh63-secondary",
			"url":         "http://secondary",
		})
		if err != nil {
			t.Fatalf("parse metrics: %v", err)
		}
		// 验证多 reason 求和值，并以不存在的 remote 确认不会假阳性命中。
		if !found || value != 5 {
			t.Fatalf("expected secondary drop sum 5, got value=%v found=%v", value, found)
		}
		if _, found, err := metricSumWithLabels(metrics, "prometheus_remote_storage_samples_dropped_total", map[string]string{"remote_name": "missing"}); err != nil || found {
			t.Fatalf("expected missing remote to stay absent, found=%v err=%v", found, err)
		}
	})

	t.Run("[UT-COLLECTION-GH63-002] 生成配置只保留 Prometheus 2.55.1 支持子集", func(t *testing.T) {
		// 准备一份与 Issue 显式输入逐项对齐的 Operator 生成配置。
		valid := `
remote_write:
  - name: gh63-primary
    url: http://vminsert.monitor-in-hand-test.svc:8480/insert/63/prometheus/api/v1/write
    protobuf_message: prometheus.WriteRequest
  - name: gh63-secondary
    url: http://gh63-secondary.monitor-in-hand-test.svc:8480/insert/630/prometheus/api/v1/write
    remote_timeout: 5s
    protobuf_message: prometheus.WriteRequest
    queue_config:
      capacity: 10
      min_shards: 1
      max_shards: 1
      max_samples_per_send: 5
      batch_send_deadline: 1s
      min_backoff: 100ms
      max_backoff: 1s
      retry_on_http_429: true
      sample_age_limit: 30s
`
		// 先验证支持子集的正向产物可被接受。
		if err := validateGH63GeneratedConfig(valid); err != nil {
			t.Fatalf("expected supported generated config: %v", err)
		}
		// 再分别注入版本不兼容字段和额外默认值，确认精确性检查会拒绝。
		for _, unsupported := range []string{
			strings.Replace(valid, "      min_backoff: 100ms\n", "      max_retries: 7\n      min_backoff: 100ms\n", 1),
			strings.Replace(valid, "    remote_timeout: 5s\n", "    remote_timeout: 5s\n    round_robin_dns: true\n", 1),
			strings.Replace(valid, "    protobuf_message: prometheus.WriteRequest\n", "    protobuf_message: prometheus.WriteRequest\n    send_exemplars: false\n", 1),
		} {
			if err := validateGH63GeneratedConfig(unsupported); err == nil {
				t.Fatal("expected unsupported Prometheus 2.55.1 field to be rejected")
			}
		}
	})

	t.Run("[UT-COLLECTION-GH63-003] reload 每种声明都是可提交的 Kubernetes YAML", func(t *testing.T) {
		// 枚举正向、重名和非法 URL 三种 reload 输入，避免反向行为被 manifest 格式短路。
		for _, mode := range []gh63ReloadMode{gh63ReloadValid, gh63ReloadDuplicate, gh63ReloadInvalidURL} {
			var object map[string]any
			if err := yaml.Unmarshal([]byte(gh63ReloadAgentManifest("gh63-test", mode)), &object); err != nil {
				t.Fatalf("mode %s is not valid YAML: %v", mode, err)
			}
			// 每种输入不仅要可解析，还必须保留可提交的 spec 根。
			if object["spec"] == nil {
				t.Fatalf("mode %s has no spec", mode)
			}
		}
	})
}

func TestCollectionCommandFixture(t *testing.T) {
	if os.Getenv("GO_WANT_COLLECTION_COMMAND_FIXTURE") != "1" {
		return
	}
	fmt.Fprintln(os.Stderr, "Warning: resource 'nodes' is not namespace scoped")
	fmt.Fprintln(os.Stdout, "no")
	os.Exit(1)
}
