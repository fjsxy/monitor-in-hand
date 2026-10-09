package collectionsupport_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	collectionsupport "github.com/fjsxy/monitor-in-hand/test/e2e/collection/support"
)

var _ = Describe("collection helpers", func() {
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

func TestCollectionCommandFixture(t *testing.T) {
	if os.Getenv("GO_WANT_COLLECTION_COMMAND_FIXTURE") != "1" {
		return
	}
	fmt.Fprintln(os.Stderr, "Warning: resource 'nodes' is not namespace scoped")
	fmt.Fprintln(os.Stdout, "no")
	os.Exit(1)
}
