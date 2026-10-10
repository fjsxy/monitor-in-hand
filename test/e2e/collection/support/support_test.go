package collectionsupport_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"

	collectionsupport "github.com/fjsxy/monitor-in-hand/test/e2e/collection/support"
)

var _ = Describe("collection helpers", func() {
	It("[UT-COLLECTION-GH82-004] 通过 API foreground 删除原生 Agent 并等待对象消失", func() {
		const agentPath = "/apis/monitoring.coreos.com/v1alpha1/namespaces/monitor-in-hand-test/prometheusagents/gh82"
		type observedRequest struct {
			method string
			path   string
			body   []byte
		}

		By("验证目标 API 路径、Foreground DeleteOptions，并在 GET 仍存在时继续等待到 NotFound")
		var requests []observedRequest
		getCount := 0
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			body, _ := io.ReadAll(request.Body)
			requests = append(requests, observedRequest{method: request.Method, path: request.URL.Path, body: body})
			writer.Header().Set("Content-Type", "application/json")
			if request.Method == http.MethodDelete {
				_, _ = writer.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Success","code":200}`))
				return
			}
			getCount++
			if getCount == 1 {
				_, _ = writer.Write([]byte(`{"apiVersion":"monitoring.coreos.com/v1alpha1","kind":"PrometheusAgent","metadata":{"name":"gh82","namespace":"monitor-in-hand-test"}}`))
				return
			}
			writeAPIError(writer, http.StatusNotFound, "NotFound", "prometheusagents.monitoring.coreos.com \"gh82\" not found")
		}))
		DeferCleanup(server.Close)

		err := collectionsupport.DeletePrometheusAgent(
			context.Background(),
			&rest.Config{Host: server.URL},
			"gh82",
			"monitor-in-hand-test",
		)
		Expect(err).NotTo(HaveOccurred())
		Expect(requests).To(HaveLen(3))
		Expect(requests[0].method).To(Equal(http.MethodDelete))
		Expect(requests[0].path).To(Equal(agentPath))
		Expect(requests[1].method).To(Equal(http.MethodGet))
		Expect(requests[1].path).To(Equal(agentPath))
		Expect(requests[2].method).To(Equal(http.MethodGet))
		Expect(requests[2].path).To(Equal(agentPath))
		deleteOptions := metav1.DeleteOptions{}
		Expect(json.Unmarshal(requests[0].body, &deleteOptions)).To(Succeed())
		Expect(deleteOptions.PropagationPolicy).NotTo(BeNil())
		Expect(*deleteOptions.PropagationPolicy).To(Equal(metav1.DeletePropagationForeground))

		By("验证 DELETE 返回 NotFound 时幂等成功")
		var notFoundRequest observedRequest
		notFoundServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			notFoundRequest = observedRequest{method: request.Method, path: request.URL.Path}
			writeAPIError(writer, http.StatusNotFound, "NotFound", "already absent")
		}))
		DeferCleanup(notFoundServer.Close)
		Expect(collectionsupport.DeletePrometheusAgent(
			context.Background(), &rest.Config{Host: notFoundServer.URL}, "gh82", "monitor-in-hand-test",
		)).To(Succeed())
		Expect(notFoundRequest.method).To(Equal(http.MethodDelete))
		Expect(notFoundRequest.path).To(Equal(agentPath))

		By("验证 403 等非 NotFound API 错误原样传播")
		forbiddenServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writeAPIError(writer, http.StatusForbidden, "Forbidden", "deletion forbidden")
		}))
		DeferCleanup(forbiddenServer.Close)
		err = collectionsupport.DeletePrometheusAgent(
			context.Background(), &rest.Config{Host: forbiddenServer.URL}, "gh82", "monitor-in-hand-test",
		)
		Expect(err).To(HaveOccurred())
		Expect(apierrors.IsForbidden(err)).To(BeTrue())
		Expect(err.Error()).To(ContainSubstring("deletion forbidden"))

		By("验证等待 GET 的非 NotFound API 错误也立即传播")
		readForbiddenServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			writer.Header().Set("Content-Type", "application/json")
			if request.Method == http.MethodDelete {
				_, _ = writer.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Success","code":200}`))
				return
			}
			writeAPIError(writer, http.StatusForbidden, "Forbidden", "read forbidden")
		}))
		DeferCleanup(readForbiddenServer.Close)
		err = collectionsupport.DeletePrometheusAgent(
			context.Background(), &rest.Config{Host: readForbiddenServer.URL}, "gh82", "monitor-in-hand-test",
		)
		Expect(apierrors.IsForbidden(err)).To(BeTrue())
		Expect(err.Error()).To(ContainSubstring("read forbidden"))

		By("验证等待期间的 context 取消与超时均传播给调用者")
		cancelledContext, cancel := context.WithCancel(context.Background())
		cancellationServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			writer.Header().Set("Content-Type", "application/json")
			if request.Method == http.MethodDelete {
				_, _ = writer.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Success","code":200}`))
				return
			}
			_, _ = writer.Write([]byte(`{"apiVersion":"monitoring.coreos.com/v1alpha1","kind":"PrometheusAgent","metadata":{"name":"gh82","namespace":"monitor-in-hand-test"}}`))
			cancel()
		}))
		DeferCleanup(cancellationServer.Close)
		err = collectionsupport.DeletePrometheusAgent(
			cancelledContext, &rest.Config{Host: cancellationServer.URL}, "gh82", "monitor-in-hand-test",
		)
		Expect(errors.Is(err, context.Canceled)).To(BeTrue())

		waitingServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			writer.Header().Set("Content-Type", "application/json")
			if request.Method == http.MethodDelete {
				_, _ = writer.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Success","code":200}`))
				return
			}
			_, _ = writer.Write([]byte(`{"apiVersion":"monitoring.coreos.com/v1alpha1","kind":"PrometheusAgent","metadata":{"name":"gh82","namespace":"monitor-in-hand-test"}}`))
		}))
		DeferCleanup(waitingServer.Close)
		timedContext, stopTimeout := context.WithTimeout(context.Background(), 25*time.Millisecond)
		DeferCleanup(stopTimeout)
		err = collectionsupport.DeletePrometheusAgent(
			timedContext, &rest.Config{Host: waitingServer.URL}, "gh82", "monitor-in-hand-test",
		)
		Expect(errors.Is(err, context.DeadlineExceeded)).To(BeTrue())
	})

	It("[UT-COLLECTION-GH81-001] 在删除 namespace 前先收敛其中的外部控制器对象", func() {
		type deletion struct {
			resource  string
			name      string
			namespace string
		}
		var deletions []deletion

		By("记录跨 namespace Agent fixture 的清理顺序")
		collectionsupport.DeleteNamespacedResourceBeforeNamespace(
			context.Background(),
			"prometheusagent",
			"gh60-a",
			"gh60-agent-other",
			func(_ context.Context, resource, name, namespace string) {
				deletions = append(deletions, deletion{resource: resource, name: name, namespace: namespace})
			},
		)

		By("验证先等待 Agent 删除，再删除承载它的 namespace")
		Expect(deletions).To(Equal([]deletion{
			{resource: "prometheusagent", name: "gh60-a", namespace: "gh60-agent-other"},
			{resource: "namespace", name: "gh60-agent-other"},
		}))
	})

	It("[UT-COLLECTION-GH59-001] 将 kubectl 警告与授权结果分流", func() {
		By("重放 kubectl auth can-i 拒绝请求时分离的 stdout 与 stderr")
		command := exec.Command(os.Args[0], "-test.run=TestCollectionCommandFixture")
		command.Env = append(os.Environ(), "GO_WANT_COLLECTION_COMMAND_FIXTURE=1")
		output, err := collectionsupport.CommandResult(command)

		By("验证拒绝状态不丢失且机器可判定输出只包含 stdout")
		Expect(err).To(HaveOccurred())
		Expect(output).To(Equal("no\n"))
		Expect(err.Error()).To(ContainSubstring("resource 'nodes' is not namespace scoped"))
	})

	It("[UT-COLLECTION-GH59-002] 从格式化 Event JSON 读取拒绝原因", func() {
		By("重放 pretty-printed EventList 并查找结构化 reason")
		events := `{
  "items": [
    {
      "reason": "InvalidConfiguration",
      "type": "Warning"
    }
  ]
}`
		found, err := collectionsupport.EventHasReason(events, "InvalidConfiguration")
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue())

		By("验证畸形 EventList 显式报错")
		_, err = collectionsupport.EventHasReason(`{"items":`, "InvalidConfiguration")
		Expect(err).To(HaveOccurred())
	})

	It("[UT-COLLECTION-GH61-001] 按 metric family 和 remote name 读取数据面证据", func() {
		By("重放标签顺序可变且包含科学计数法的 Prometheus exposition")
		metrics := `# HELP prometheus_remote_storage_samples_total Total number of samples sent.
prometheus_remote_storage_samples_total{remote_name="other",url="http://other"} 99
prometheus_remote_storage_samples_total{url="http://vminsert",remote_name="gh61-victoriametrics-primary"} 1.25e+03
vm_http_requests_total{path="/insert/{}/prometheus/", protocol="promremotewrite"} 148
`
		value, found := collectionsupport.MetricSample(metrics, "prometheus_remote_storage_samples_total", `remote_name="gh61-victoriametrics-primary"`)
		Expect(found).To(BeTrue())
		Expect(value).To(Equal(1250.0))

		By("验证其他 metric family 并拒绝误命中其他 remote")
		value, found = collectionsupport.MetricSample(metrics, "vm_http_requests_total", `protocol="promremotewrite"`)
		Expect(found).To(BeTrue())
		Expect(value).To(Equal(148.0))
		_, found = collectionsupport.MetricSample(metrics, "prometheus_remote_storage_samples_total", `remote_name="missing"`)
		Expect(found).To(BeFalse())
	})

	It("UT-GATE-GH65-008 在删除 remote_write fixture 前独立尽力保全失败证据", func() {
		By("验证清理阶段可读取配置，并显式保留尚未生成或损坏的配置错误")
		var compressed bytes.Buffer
		writer := gzip.NewWriter(&compressed)
		_, err := writer.Write([]byte("global:\n  external_labels:\n    mih_cluster_id: gh61\n"))
		Expect(err).NotTo(HaveOccurred())
		Expect(writer.Close()).To(Succeed())
		generated, err := collectionsupport.DecodeGeneratedConfig(base64.StdEncoding.EncodeToString(compressed.Bytes()))
		Expect(err).NotTo(HaveOccurred())
		Expect(generated).To(ContainSubstring("mih_cluster_id: gh61"))
		for _, encoded := range []string{"", "invalid-base64!", base64.StdEncoding.EncodeToString([]byte("invalid gzip"))} {
			_, err := collectionsupport.DecodeGeneratedConfig(encoded)
			Expect(err).To(HaveOccurred())
		}

		By("准备一个失败和两个成功的独立观察点")
		calls := make(chan string, 3)
		observations := []collectionsupport.Observation{
			{Name: "vmselect", Collect: func(context.Context) (string, error) {
				calls <- "vmselect"
				return "query body", errors.New("query failed")
			}},
			{Name: "statefulset-pvc", Collect: func(context.Context) (string, error) { calls <- "statefulset-pvc"; return "persistent state", nil }},
			{Name: "remote-write-metrics", Collect: func(context.Context) (string, error) { calls <- "remote-write-metrics"; return "samples 1", nil }},
		}

		By("验证单点失败仍保留全部观察结果且返回顺序稳定")
		evidence := collectionsupport.CollectAll(context.Background(), observations)
		Expect(evidence).To(Equal([]collectionsupport.Evidence{
			{Name: "vmselect", Body: "error: query failed\nquery body"},
			{Name: "statefulset-pvc", Body: "persistent state"},
			{Name: "remote-write-metrics", Body: "samples 1"},
		}))
		Expect(calls).To(Receive())
		Expect(calls).To(Receive())
		Expect(calls).To(Receive())

		By("验证清理契约先取证再删除，并在取证 panic 时仍执行删除")
		var order []string
		Expect(func() {
			collectionsupport.CollectBeforeCleanup(context.Background(), func(context.Context) {
				order = append(order, "evidence")
				panic("evidence panic")
			}, func(context.Context) {
				order = append(order, "cleanup")
			})
		}).To(PanicWith("evidence panic"))
		Expect(order).To(Equal([]string{"evidence", "cleanup"}))
	})
})

func writeAPIError(writer http.ResponseWriter, statusCode int, reason, message string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(statusCode)
	_, _ = fmt.Fprintf(writer, `{"kind":"Status","apiVersion":"v1","status":"Failure","message":%q,"reason":%q,"code":%d}`,
		message, reason, statusCode)
}

func TestCollectionCommandFixture(t *testing.T) {
	if os.Getenv("GO_WANT_COLLECTION_COMMAND_FIXTURE") != "1" {
		return
	}
	fmt.Fprintln(os.Stderr, "Warning: resource 'nodes' is not namespace scoped")
	fmt.Fprintln(os.Stdout, "no")
	os.Exit(1)
}
