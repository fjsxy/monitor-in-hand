package collection_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("collection helpers", func() {
	It("[UT-COLLECTION-GH59-001] 将 kubectl 警告与授权结果分流", func() {
		By("重放 kubectl auth can-i 拒绝请求时分离的 stdout 与 stderr")
		// 模拟 kubectl auth can-i 拒绝请求时 stdout 返回 no、stderr 返回 scope 警告。
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, os.Args[0], "-test.run=TestCollectionCommandFixture")
		command.Env = append(os.Environ(), "GO_WANT_COLLECTION_COMMAND_FIXTURE=1")
		output, err := commandResult(command)

		By("验证拒绝状态不丢失且机器可判定输出只包含 stdout")
		// 授权拒绝仍须保留非零退出，同时机器可判定输出只能包含 stdout。
		Expect(err).To(HaveOccurred())
		Expect(output).To(Equal("no\n"))
		Expect(err.Error()).To(ContainSubstring("resource 'nodes' is not namespace scoped"))
	})

	It("[UT-COLLECTION-GH59-002] 从格式化 Event JSON 读取拒绝原因", func() {
		By("重放 pretty-printed EventList 并查找结构化 reason")
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
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue())

		By("验证畸形 EventList 显式报错")
		_, err = eventHasReason(`{"items":`, "InvalidConfiguration")
		Expect(err).To(HaveOccurred())
	})

	It("[UT-COLLECTION-GH61-001] 按 metric family 和 remote name 读取数据面证据", func() {
		By("重放标签顺序可变且包含科学计数法的 Prometheus exposition")
		// 重放 Prometheus text exposition 中标签顺序可变、标签逗号后带空格且 sample 使用科学计数法的形态。
		metrics := `# HELP prometheus_remote_storage_samples_total Total number of samples sent.
prometheus_remote_storage_samples_total{remote_name="other",url="http://other"} 99
prometheus_remote_storage_samples_total{url="http://vminsert",remote_name="gh61-victoriametrics-primary"} 1.25e+03
vm_http_requests_total{path="/insert/{}/prometheus/", protocol="promremotewrite"} 148
`
		value, found := metricSample(metrics, "prometheus_remote_storage_samples_total", `remote_name="gh61-victoriametrics-primary"`)
		Expect(found).To(BeTrue())
		Expect(value).To(Equal(1250.0))

		By("验证其他 metric family 并拒绝误命中其他 remote")
		value, found = metricSample(metrics, "vm_http_requests_total", `protocol="promremotewrite"`)
		Expect(found).To(BeTrue())
		Expect(value).To(Equal(148.0))
		_, found = metricSample(metrics, "prometheus_remote_storage_samples_total", `remote_name="missing"`)
		Expect(found).To(BeFalse())
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
