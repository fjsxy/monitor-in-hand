package testenv_test

import (
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"sigs.k8s.io/yaml"
)

var _ = Describe("双版本一期验收门禁", func() {
	It("并行运行两个固定版本并在失败时先保存证据再清理", func() {
		By("读取 CI workflow 并定位双版本验收 job")
		root, err := filepath.Abs(filepath.Join("..", ".."))
		Expect(err).NotTo(HaveOccurred())
		contents, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "ci.yml"))
		Expect(err).NotTo(HaveOccurred())

		workflow := map[string]any{}
		Expect(yaml.Unmarshal(contents, &workflow)).To(Succeed())
		jobs := workflow["jobs"].(map[string]any)
		job, found := jobs["test-environment"].(map[string]any)
		Expect(found).To(BeTrue(), "缺少 test-environment blocking job")

		By("验证 Kubernetes 1.19 和 1.28 使用独立矩阵入口")
		strategy := job["strategy"].(map[string]any)
		Expect(strategy["fail-fast"]).To(BeFalse())
		matrix := strategy["matrix"].(map[string]any)
		entries := matrix["include"].([]any)
		Expect(entries).To(HaveLen(2))
		Expect(entries).To(ConsistOf(
			SatisfyAll(
				HaveKeyWithValue("kubernetes", "1.19"),
				HaveKeyWithValue("kubectl", "v1.19.16"),
				HaveKeyWithValue("run_id_suffix", "119"),
			),
			SatisfyAll(
				HaveKeyWithValue("kubernetes", "1.28"),
				HaveKeyWithValue("kubectl", "v1.28.15"),
				HaveKeyWithValue("run_id_suffix", "128"),
			),
		))

		By("验证环境准备、Ginkgo E2E、报告上传和最终清理都是 blocking 步骤")
		steps := job["steps"].([]any)
		Expect(steps).To(ContainElement(SatisfyAll(
			HaveKeyWithValue("uses", "azure/setup-kubectl@v4"),
			HaveKeyWithValue("with", HaveKeyWithValue("version", "${{ matrix.kubectl }}")),
		)))
		Expect(steps).To(ContainElement(SatisfyAll(
			HaveKeyWithValue("uses", "azure/setup-helm@v4"),
			HaveKeyWithValue("with", HaveKeyWithValue("version", "v3.22.0")),
		)))
		Expect(steps).To(ContainElement(SatisfyAll(
			HaveKeyWithValue("name", "创建并验收外部测试环境"),
			HaveKeyWithValue("run", And(
				ContainSubstring("make test-env-up"),
				ContainSubstring("make test-env-verify"),
			)),
		)))
		Expect(steps).To(ContainElement(SatisfyAll(
			HaveKeyWithValue("name", "E2E 默认 KubeStateMetrics Deployment"),
			HaveKeyWithValue("run", ContainSubstring("make e2e-default-deployment")),
		)))
		Expect(steps).To(ContainElement(SatisfyAll(
			HaveKeyWithValue("name", "上传 E2E 测试报告"),
			HaveKeyWithValue("if", "${{ always() }}"),
			HaveKeyWithValue("uses", "actions/upload-artifact@v4"),
			HaveKeyWithValue("with", HaveKeyWithValue("path", "artifacts/e2e/")),
		)))
		Expect(steps).To(ContainElement(SatisfyAll(
			HaveKeyWithValue("name", "失败时保存测试环境证据"),
			HaveKeyWithValue("if", "${{ failure() }}"),
		)))
		Expect(steps).To(ContainElement(SatisfyAll(
			HaveKeyWithValue("uses", "actions/upload-artifact@v4"),
			HaveKeyWithValue("if", "${{ failure() }}"),
		)))
		Expect(steps).To(ContainElement(SatisfyAll(
			HaveKeyWithValue("name", "清理本次测试环境"),
			HaveKeyWithValue("if", "${{ always() }}"),
			Not(HaveKey("continue-on-error")),
		)))
	})

	It("使用 Ginkgo 执行 E2E 业务断言而不由 shell 代替", func() {
		By("检查 Make 公共入口调用 Ginkgo 并生成 JUnit")
		root, err := filepath.Abs(filepath.Join("..", ".."))
		Expect(err).NotTo(HaveOccurred())
		makefile, err := os.ReadFile(filepath.Join(root, "Makefile"))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(makefile)).To(ContainSubstring("go run github.com/onsi/ginkgo/v2/ginkgo"))
		Expect(string(makefile)).To(ContainSubstring("--junit-report"))
		Expect(string(makefile)).NotTo(ContainSubstring("./hack/e2e-default-deployment.sh"))

		By("确认旧 shell 业务断言入口已被删除")
		_, err = os.Stat(filepath.Join(root, "hack", "e2e-default-deployment.sh"))
		Expect(os.IsNotExist(err)).To(BeTrue())
	})
})
