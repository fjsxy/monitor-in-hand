package release_test

import (
	"os"
	"strings"
	"testing"

	product "github.com/fjsxy/monitor-in-hand/internal/release"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestRelease(t *testing.T) { RegisterFailHandler(Fail); RunSpecs(t, "发布契约与升级") }

var _ = Describe("发布能力识别", func() {
	It("UT-RELEASE-GH87-001 在写入 CR 前识别完整 schema 并拒绝旧版和不兼容字段", func() {
		By("读取真实产品 CRD，不从实现函数生成期望")
		data, err := os.ReadFile("../../config/crd/bases/monitoring.fjsxy.github.io_kubestatemetrics.yaml")
		Expect(err).NotTo(HaveOccurred())
		Expect(product.CheckCapability(data)).To(Succeed())
		By("旧 schema、非预期类型和默认值都不能静默通过")
		for _, replacement := range [][2]string{{"serviceMonitor:", "legacyMonitor:"}, {"default: false", "default: true"}, {"type: string", "type: integer"}, {"uid:", "legacyUID:"}, {"scope: Cluster", "scope: Namespaced"}} {
			changed := strings.ReplaceAll(string(data), replacement[0], replacement[1])
			Expect(changed).NotTo(Equal(string(data)))
			Expect(product.CheckCapability([]byte(changed))).NotTo(Succeed(), replacement[0])
		}
		Expect(product.CheckCapability([]byte("invalid: ["))).NotTo(Succeed())
	})
})
