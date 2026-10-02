package operator_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	monitoringv1alpha1 "github.com/fjsxy/monitor-in-hand/api/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	operatorNamespace  = "monitor-in-hand-system"
	operatorRelease    = "monitor-in-hand"
	operatorDeployment = "monitor-in-hand-monitor-in-hand"
	operatorBinding    = "monitor-in-hand-monitor-in-hand-manager"
	operatorLease      = "monitor-in-hand.monitoring.fjsxy.github.io"
	testNamespace      = "monitor-in-hand-test"
)

type e2eConfig struct {
	kubernetesMinor string
	testRunID       string
	repoRoot        string
	kubeContext     string
	clusterName     string
	kubectl         string
	helm            string
	docker          string
}

var config e2eConfig

func scenarioID(sequence string) string {
	ids := map[string]map[string]string{
		"1.19": {
			"001": "E2E-119-OPERATOR-GH64-001",
			"002": "E2E-119-OPERATOR-GH64-002",
			"003": "E2E-119-OPERATOR-GH64-003",
			"004": "E2E-119-OPERATOR-GH64-004",
			"005": "E2E-119-OPERATOR-GH64-005",
			"006": "E2E-119-OPERATOR-GH64-006",
			"007": "E2E-119-OPERATOR-GH64-007",
			"008": "E2E-119-OPERATOR-GH64-008",
			"009": "E2E-119-OPERATOR-GH64-009",
			"010": "E2E-119-OPERATOR-GH64-010",
			"011": "E2E-119-OPERATOR-GH64-011",
		},
		"1.28": {
			"001": "E2E-128-OPERATOR-GH64-001",
			"002": "E2E-128-OPERATOR-GH64-002",
			"003": "E2E-128-OPERATOR-GH64-003",
			"004": "E2E-128-OPERATOR-GH64-004",
			"005": "E2E-128-OPERATOR-GH64-005",
			"006": "E2E-128-OPERATOR-GH64-006",
			"007": "E2E-128-OPERATOR-GH64-007",
			"008": "E2E-128-OPERATOR-GH64-008",
			"009": "E2E-128-OPERATOR-GH64-009",
			"010": "E2E-128-OPERATOR-GH64-010",
			"011": "E2E-128-OPERATOR-GH64-011",
		},
	}
	minor := config.kubernetesMinor
	if minor == "" {
		minor = os.Getenv("KUBERNETES_VERSION")
	}
	if versionIDs, ok := ids[minor]; ok {
		if id, ok := versionIDs[sequence]; ok {
			return id
		}
	}
	return "E2E-UNSUPPORTED-OPERATOR-GH64-" + sequence
}

func installOperator(ctx context.Context) {
	GinkgoHelper()
	manifest := fmt.Sprintf("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: %s\n", operatorNamespace)
	runWithInput(ctx, manifest, config.kubectl, "--context", config.kubeContext, "apply", "--filename", "-")
	args := []string{"upgrade", "--install", operatorRelease, filepath.Join(config.repoRoot, "charts", "monitor-in-hand"),
		"--kube-context", config.kubeContext, "--namespace", operatorNamespace, "--wait", "--timeout", "180s"}
	run(ctx, config.helm, args...)
}

func uninstallOperator(ctx context.Context) {
	GinkgoHelper()
	run(ctx, config.helm, "uninstall", operatorRelease, "--kube-context", config.kubeContext,
		"--namespace", operatorNamespace, "--wait", "--timeout", "180s")
}

func applyManifest(ctx context.Context, manifest string) {
	GinkgoHelper()
	runWithInput(ctx, manifest, config.kubectl, "--context", config.kubeContext, "apply", "--filename", "-")
}

func run(ctx context.Context, name string, args ...string) string {
	GinkgoHelper()
	output, err := runResult(ctx, name, args...)
	Expect(err).NotTo(HaveOccurred(), "命令失败: %s %s\n%s", name, strings.Join(args, " "), output)
	return output
}

func runResult(ctx context.Context, name string, args ...string) (string, error) {
	GinkgoHelper()
	command := exec.CommandContext(ctx, name, args...)
	command.Dir = config.repoRoot
	output, err := command.CombinedOutput()
	_, _ = fmt.Fprintf(GinkgoWriter, "$ %s %s\n%s", name, strings.Join(args, " "), output)
	return string(output), err
}

func runWithInput(ctx context.Context, input, name string, args ...string) string {
	GinkgoHelper()
	command := exec.CommandContext(ctx, name, args...)
	command.Dir = config.repoRoot
	command.Stdin = strings.NewReader(input)
	output, err := command.CombinedOutput()
	_, _ = fmt.Fprintf(GinkgoWriter, "$ %s %s\n%s", name, strings.Join(args, " "), output)
	Expect(err).NotTo(HaveOccurred(), "命令失败: %s %s\n%s", name, strings.Join(args, " "), output)
	return string(output)
}

func getObject[T any](ctx context.Context, namespace, kind, name string) (*T, error) {
	GinkgoHelper()
	args := []string{"--context", config.kubeContext}
	if namespace != "" {
		args = append(args, "--namespace", namespace)
	}
	args = append(args, "get", kind+"/"+name, "--output", "json")
	output, err := runResult(ctx, config.kubectl, args...)
	if err != nil {
		return nil, fmt.Errorf("kubectl get %s/%s: %w: %s", kind, name, err, output)
	}
	object := new(T)
	if err := json.Unmarshal([]byte(output), object); err != nil {
		return nil, fmt.Errorf("解析 %s/%s: %w", kind, name, err)
	}
	return object, nil
}

func getList[T any](ctx context.Context, namespace, kind string, selectors ...string) (*T, error) {
	GinkgoHelper()
	args := []string{"--context", config.kubeContext}
	if namespace != "" {
		args = append(args, "--namespace", namespace)
	}
	args = append(args, "get", kind)
	for _, selector := range selectors {
		args = append(args, "--selector", selector)
	}
	args = append(args, "--output", "json")
	output, err := runResult(ctx, config.kubectl, args...)
	if err != nil {
		return nil, fmt.Errorf("kubectl get %s: %w: %s", kind, err, output)
	}
	list := new(T)
	if err := json.Unmarshal([]byte(output), list); err != nil {
		return nil, fmt.Errorf("解析 %s list: %w", kind, err)
	}
	return list, nil
}

func objectAbsent(ctx context.Context, namespace, kind, name string) error {
	GinkgoHelper()
	args := []string{"--context", config.kubeContext}
	if namespace != "" {
		args = append(args, "--namespace", namespace)
	}
	args = append(args, "get", kind+"/"+name)
	output, err := runResult(ctx, config.kubectl, args...)
	if err == nil {
		return fmt.Errorf("%s/%s 仍然存在", kind, name)
	}
	if !strings.Contains(output, "NotFound") && !strings.Contains(output, "not found") {
		return fmt.Errorf("检查 %s/%s: %w: %s", kind, name, err, output)
	}
	return nil
}

func waitForInstanceReady(ctx context.Context, instance, deployment string) *monitoringv1alpha1.KubeStateMetrics {
	GinkgoHelper()
	current := waitForInstanceStatusReady(ctx, instance)
	Eventually(func() error {
		workload, err := getObject[appsv1.Deployment](ctx, testNamespace, "deployment", deployment)
		if err != nil {
			return err
		}
		if workload.Status.AvailableReplicas != 1 {
			return fmt.Errorf("Deployment %s availableReplicas=%d", deployment, workload.Status.AvailableReplicas)
		}
		return nil
	}, 5*time.Minute, 2*time.Second).Should(Succeed())
	return current
}

func waitForInstanceStatusReady(ctx context.Context, instance string) *monitoringv1alpha1.KubeStateMetrics {
	GinkgoHelper()
	var current *monitoringv1alpha1.KubeStateMetrics
	Eventually(func() error {
		var err error
		current, err = getObject[monitoringv1alpha1.KubeStateMetrics](ctx, "", "kubestatemetrics", instance)
		if err != nil {
			return err
		}
		if current.Status.Phase != monitoringv1alpha1.KubeStateMetricsPhaseReady ||
			current.Status.ObservedGeneration != current.Generation || current.Status.AvailablePods != current.Status.DesiredPods {
			return fmt.Errorf("实例未就绪: phase=%s observed=%d generation=%d available=%d desired=%d",
				current.Status.Phase, current.Status.ObservedGeneration, current.Generation,
				current.Status.AvailablePods, current.Status.DesiredPods)
		}
		return nil
	}, 5*time.Minute, 2*time.Second).Should(Succeed())
	return current
}

func operatorPods(ctx context.Context) ([]corev1.Pod, error) {
	GinkgoHelper()
	pods, err := getList[corev1.PodList](ctx, operatorNamespace, "pods", "app.kubernetes.io/instance="+operatorRelease)
	if err != nil {
		return nil, err
	}
	return pods.Items, nil
}

func activeOperatorPod(ctx context.Context) (*corev1.Pod, string, error) {
	GinkgoHelper()
	// 先读取稳定 Lease 的单一 holder，再把 controller-runtime identity 映射回实际 Pod。
	lease, err := getObject[coordinationv1.Lease](ctx, operatorNamespace, "lease", operatorLease)
	if err != nil {
		return nil, "", err
	}
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity == "" {
		return nil, "", fmt.Errorf("Lease 没有 holderIdentity")
	}
	holder := *lease.Spec.HolderIdentity
	pods, err := operatorPods(ctx)
	if err != nil {
		return nil, holder, err
	}
	for index := range pods {
		if strings.HasPrefix(holder, pods[index].Name+"_") || holder == pods[index].Name {
			return &pods[index], holder, nil
		}
	}
	return nil, holder, fmt.Errorf("Lease holder %q 不对应当前 Operator Pod", holder)
}

func managerStatus(pod *corev1.Pod) (corev1.ContainerStatus, error) {
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == "manager" {
			return status, nil
		}
	}
	return corev1.ContainerStatus{}, fmt.Errorf("Pod %s 缺少 manager container status", pod.Name)
}

func podReady(pod corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

type portForward struct {
	command *exec.Cmd
	logs    lockedBuffer
	port    int
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(contents []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(contents)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func startPortForward(ctx context.Context, namespace, target string, remotePort int) *portForward {
	GinkgoHelper()
	// 先预留一个宿主端口，再启动由 SpecContext 约束生命周期的 kubectl 转发进程。
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	Expect(err).NotTo(HaveOccurred())
	port := listener.Addr().(*net.TCPAddr).Port
	Expect(listener.Close()).To(Succeed())

	forward := &portForward{port: port}
	forward.command = exec.CommandContext(ctx, config.kubectl, "--context", config.kubeContext, "--namespace", namespace,
		"port-forward", target, fmt.Sprintf("%d:%d", port, remotePort))
	forward.command.Stdout = &forward.logs
	forward.command.Stderr = &forward.logs
	Expect(forward.command.Start()).To(Succeed())
	// 在业务请求前等待监听建立，失败时携带 port-forward 日志定位连接问题。
	Eventually(func() error {
		connection, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 500*time.Millisecond)
		if err != nil {
			return fmt.Errorf("port-forward 未就绪: %w: %s", err, forward.logs.String())
		}
		return connection.Close()
	}, 15*time.Second, 200*time.Millisecond).Should(Succeed())
	return forward
}

func (p *portForward) stop() {
	GinkgoHelper()
	// 清理只终止本测试创建的进程，并回收子进程以避免跨场景泄漏。
	if p == nil || p.command == nil || p.command.Process == nil {
		return
	}
	_ = p.command.Process.Kill()
	_, _ = p.command.Process.Wait()
}

func (p *portForward) status(path string) (int, error) {
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d%s", p.port, path))
	if err != nil {
		return 0, fmt.Errorf("GET %s: %w: %s", path, err, p.logs.String())
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	return response.StatusCode, nil
}

func (p *portForward) metricSample(metric, labelFragment string) (float64, error) {
	// 通过公开 metrics endpoint 定位指定 label 的 sample，并独立解析其数值。
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/metrics", p.port))
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return 0, err
	}
	if response.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("metrics HTTP status=%d: %s", response.StatusCode, body)
	}
	for _, line := range strings.Split(string(body), "\n") {
		if !strings.HasPrefix(line, metric+"{") || !strings.Contains(line, labelFragment) {
			continue
		}
		separator := strings.LastIndexByte(line, ' ')
		if separator < 0 {
			return 0, fmt.Errorf("metric sample 缺少数值: %s", line)
		}
		value, err := strconv.ParseFloat(strings.TrimSpace(line[separator+1:]), 64)
		if err != nil {
			return 0, fmt.Errorf("解析 metric sample %q: %w", line, err)
		}
		return value, nil
	}
	return 0, fmt.Errorf("未找到 %s{%s}", metric, labelFragment)
}

func blockPodAPIAccess(ctx context.Context, podIP string) func() {
	GinkgoHelper()
	// 在 kind node 的 DNAT 后 INPUT 链阻断指定 Pod，宿主 kubectl 仍可观测集群并负责恢复规则。
	args := []string{"exec", config.clusterName + "-control-plane", "iptables", "-I", "INPUT", "1",
		"-s", podIP, "-p", "tcp", "--dport", "6443", "-j", "REJECT"}
	run(ctx, config.docker, args...)
	var once sync.Once
	return func() {
		once.Do(func() {
			// 无论场景成功或失败，都用独立超时上下文精确删除同一条防火墙规则。
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			deleteArgs := []string{"exec", config.clusterName + "-control-plane", "iptables", "-D", "INPUT",
				"-s", podIP, "-p", "tcp", "--dport", "6443", "-j", "REJECT"}
			output, err := runResult(cleanupCtx, config.docker, deleteArgs...)
			Expect(err).NotTo(HaveOccurred(), output)
		})
	}
}

type managedResourceKind struct {
	key       string
	namespace string
	kind      string
}

func directManagedResourceKinds() []managedResourceKind {
	return []managedResourceKind{
		{"deployment", testNamespace, "deployment"},
		{"service", testNamespace, "service"},
		{"serviceaccount", testNamespace, "serviceaccount"},
		{"clusterrole", "", "clusterrole"},
		{"clusterrolebinding", "", "clusterrolebinding"},
	}
}

func allManagedResourceKinds() []managedResourceKind {
	return append(directManagedResourceKinds(), []managedResourceKind{
		{"statefulset", testNamespace, "statefulset"},
		{"daemonset", testNamespace, "daemonset"},
		{"role", testNamespace, "role"},
		{"rolebinding", testNamespace, "rolebinding"},
	}...)
}

func managedUIDs(ctx context.Context, managed string) (map[string]types.UID, error) {
	GinkgoHelper()
	result := map[string]types.UID{}
	for _, item := range directManagedResourceKinds() {
		object, err := getObject[metav1.PartialObjectMetadata](ctx, item.namespace, item.kind, managed)
		if err != nil {
			return nil, err
		}
		result[item.key] = object.UID
	}
	return result, nil
}

func deploymentNamespaces(deployment *appsv1.Deployment) ([]string, error) {
	arguments, err := managerArguments(deployment)
	if err != nil {
		return nil, err
	}
	for _, argument := range arguments {
		if strings.HasPrefix(argument, "--namespaces=") {
			values := strings.Split(strings.TrimPrefix(argument, "--namespaces="), ",")
			sort.Strings(values)
			return values, nil
		}
	}
	return nil, fmt.Errorf("Deployment 缺少 --namespaces")
}

type vmQueryResponse struct {
	Status string `json:"status"`
	Data   struct {
		Result []struct {
			Metric map[string]string `json:"metric"`
			Value  []any             `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

func querySeriesTimestamp(ctx context.Context, testRunID string) float64 {
	GinkgoHelper()
	// 先建立到 VictoriaMetrics 查询面的转发，再用本次 run 唯一标签构造 PromQL。
	forward := startPortForward(ctx, testNamespace, "service/vmselect", 8481)
	defer forward.stop()
	query := fmt.Sprintf(`kube_pod_info{mih_gh64_run_id=%q}`, testRunID)
	endpoint := fmt.Sprintf("http://127.0.0.1:%d/select/0/prometheus/api/v1/query?query=%s", forward.port, url.QueryEscape(query))
	var timestamp float64
	// 在有界时间内等待 remote-write series 出现，并结构化提取服务端返回的样本时间戳。
	Eventually(func() error {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return err
		}
		response, err := (&http.Client{Timeout: 3 * time.Second}).Do(request)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			return err
		}
		var result vmQueryResponse
		if err := json.Unmarshal(body, &result); err != nil {
			return fmt.Errorf("解析 VictoriaMetrics response: %w: %s", err, body)
		}
		if result.Status != "success" || len(result.Data.Result) == 0 || len(result.Data.Result[0].Value) != 2 {
			return fmt.Errorf("series 尚不可用: %s", body)
		}
		switch value := result.Data.Result[0].Value[0].(type) {
		case float64:
			timestamp = value
		case string:
			timestamp, err = strconv.ParseFloat(value, 64)
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("未知 timestamp 类型 %T", value)
		}
		return nil
	}, 3*time.Minute, 2*time.Second).Should(Succeed())
	return timestamp
}

func eventCount(ctx context.Context, instance, reason string) (int32, error) {
	GinkgoHelper()
	// 跨 namespace 读取目标 CR 的 Events，并把聚合 Event.count 归一为实际转换次数。
	output, err := runResult(ctx, config.kubectl, "--context", config.kubeContext, "--all-namespaces",
		"get", "events", "--field-selector", "involvedObject.kind=KubeStateMetrics,involvedObject.name="+instance, "--output", "json")
	if err != nil {
		return 0, fmt.Errorf("读取 KubeStateMetrics %s 的 Event: %w: %s", instance, err, output)
	}
	events := &corev1.EventList{}
	if err := json.Unmarshal([]byte(output), events); err != nil {
		return 0, fmt.Errorf("解析 KubeStateMetrics %s 的 Event: %w", instance, err)
	}
	var total int32
	for _, event := range events.Items {
		if event.Reason == reason {
			if event.Count == 0 {
				total++
			} else {
				total += event.Count
			}
		}
	}
	return total, nil
}

func latestHelmRevision(ctx context.Context) int {
	GinkgoHelper()
	output := run(ctx, config.helm, "history", operatorRelease, "--kube-context", config.kubeContext,
		"--namespace", operatorNamespace, "--output", "json")
	var history []struct {
		Revision int `json:"revision"`
	}
	Expect(json.Unmarshal([]byte(output), &history)).To(Succeed())
	Expect(history).NotTo(BeEmpty())
	return history[len(history)-1].Revision
}

func operatorLogLevel(deployment *appsv1.Deployment) string {
	arguments, err := managerArguments(deployment)
	if err != nil {
		return ""
	}
	for _, argument := range arguments {
		if strings.HasPrefix(argument, "--log-level=") {
			return strings.TrimPrefix(argument, "--log-level=")
		}
	}
	return ""
}

func managerArguments(deployment *appsv1.Deployment) ([]string, error) {
	if len(deployment.Spec.Template.Spec.Containers) != 1 {
		return nil, fmt.Errorf("Deployment container 数量=%d", len(deployment.Spec.Template.Spec.Containers))
	}
	return deployment.Spec.Template.Spec.Containers[0].Args, nil
}

func conditionMatches(instance *monitoringv1alpha1.KubeStateMetrics, conditionType string, status metav1.ConditionStatus, reason string) bool {
	for _, condition := range instance.Status.Conditions {
		if condition.Type == conditionType && condition.Status == status && condition.Reason == reason {
			return true
		}
	}
	return false
}
