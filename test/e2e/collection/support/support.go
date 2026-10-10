package collectionsupport

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

// Observation 描述一个可独立采集的 E2E 证据源。
type Observation struct {
	Name    string
	Collect func(context.Context) (string, error)
}

// Evidence 保留单项观察结果，不让采集失败中断其他证据。
type Evidence struct {
	Name string
	Body string
}

// DeletePrometheusAgent 通过 Kubernetes API 删除原生采集实例并等待对象消失。
func DeletePrometheusAgent(ctx context.Context, restConfig *rest.Config, name, namespace string) error {
	client, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("创建 Kubernetes dynamic client: %w", err)
	}
	resource := client.Resource(schema.GroupVersionResource{
		Group: "monitoring.coreos.com", Version: "v1alpha1", Resource: "prometheusagents",
	}).Namespace(namespace)
	foreground := metav1.DeletePropagationForeground
	if err := resource.Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &foreground}); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("foreground 删除 PrometheusAgent %s/%s: %w", namespace, name, err)
	}

	// 删除请求成功后持续读取对象，避免外部生成资源仍在回收时提前清理 Namespace。
	if err := wait.PollUntilContextCancel(ctx, 2*time.Second, true, func(ctx context.Context) (bool, error) {
		_, err := resource.Get(ctx, name, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			return true, nil
		case err != nil:
			return false, err
		default:
			return false, nil
		}
	}); err != nil {
		return fmt.Errorf("等待 PrometheusAgent %s/%s 删除: %w", namespace, name, err)
	}
	return nil
}

// DeleteNamespacedResourceBeforeNamespace 先等待外部控制器对象删除，再回收承载它的 namespace。
func DeleteNamespacedResourceBeforeNamespace(
	ctx context.Context,
	resource, name, namespace string,
	deleteResource func(context.Context, string, string, string),
) {
	deleteResource(ctx, resource, name, namespace)
	deleteResource(ctx, "namespace", namespace, "")
}

// DecodeGeneratedConfig 解码 Operator Secret 中的压缩配置，明确报告缺失或损坏的内容。
func DecodeGeneratedConfig(encoded string) (string, error) {
	if strings.TrimSpace(encoded) == "" {
		return "", fmt.Errorf("generated config 尚未生成")
	}
	compressed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return "", fmt.Errorf("解码 generated config: %w", err)
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return "", fmt.Errorf("解压 generated config: %w", err)
	}
	contents, err := io.ReadAll(reader)
	closeErr := reader.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return string(contents), fmt.Errorf("读取 generated config: %w", err)
	}
	return string(contents), nil
}

// CommandResult 分离机器可判定的 stdout 与命令诊断。
func CommandResult(command *exec.Cmd) (string, error) {
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

// EventHasReason 结构化读取 EventList，不依赖 kubectl 的 JSON 格式。
func EventHasReason(events, reason string) (bool, error) {
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

// MetricSample 按 metric family 和精确标签片段查找 Prometheus sample。
func MetricSample(contents, metricName, requiredLabel string) (float64, bool) {
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

// CollectAll 即使单个来源失败也会执行全部证据观察。
func CollectAll(ctx context.Context, observations []Observation) []Evidence {
	type result struct {
		index int
		body  string
		err   error
	}
	results := make(chan result, len(observations))
	// 并行执行各观察点，避免一个有界慢源串行拖延其余失败证据。
	for index, observation := range observations {
		go func(index int, observation Observation) {
			body, err := observation.Collect(ctx)
			results <- result{index: index, body: body, err: err}
		}(index, observation)
	}

	// 按声明顺序返回证据，避免并发完成顺序让报告不稳定。
	collected := make([]result, len(observations))
	for range observations {
		current := <-results
		collected[current.index] = current
	}
	evidence := make([]Evidence, len(observations))
	for index, observation := range observations {
		evidence[index] = Evidence{Name: observation.Name, Body: evidenceBody(collected[index].body, collected[index].err)}
	}
	return evidence
}

// CollectBeforeCleanup 保证先尝试取证，并在取证 panic 时仍删除 fixture。
func CollectBeforeCleanup(ctx context.Context, collect, cleanup func(context.Context)) {
	defer cleanup(ctx)
	collect(ctx)
}

func evidenceBody(body string, err error) string {
	if err == nil {
		return body
	}
	return fmt.Sprintf("error: %v\n%s", err, body)
}
