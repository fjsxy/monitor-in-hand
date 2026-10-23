package testenv_test

import (
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"sigs.k8s.io/yaml"
)

var _ = Describe("双版本一期验收门禁", func() {
	It("UT-GATE-GH65-005 汇总本次全部 blocking JUnit 并作为 PR、main 与版本发布的最终阻断门禁", func() {
		By("读取 workflow 并确认版本 tag 与日常变更使用同一套 CI")
		root, err := filepath.Abs(filepath.Join("..", ".."))
		Expect(err).NotTo(HaveOccurred())
		contents, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "ci.yml"))
		Expect(err).NotTo(HaveOccurred())

		workflow := map[string]any{}
		Expect(yaml.Unmarshal(contents, &workflow)).To(Succeed())
		Expect(string(contents)).To(ContainSubstring("tags:\n      - 'v*'"))

		By("验证最终门禁无论上游成功与否都会把本次 E2E artifact 保存在独立来源目录")
		jobs := workflow["jobs"].(map[string]any)
		job, found := jobs["phase1-release-gate"].(map[string]any)
		Expect(found).To(BeTrue(), "缺少一期最终发布门禁 job")
		Expect(job["if"]).To(Equal("${{ always() }}"))
		Expect(job["needs"]).To(ConsistOf("quality", "smoke", "test-environment"))
		steps := job["steps"].([]any)
		Expect(steps).To(ContainElement(SatisfyAll(
			HaveKeyWithValue("uses", "actions/download-artifact@v4"),
			HaveKeyWithValue("with", SatisfyAll(
				HaveKeyWithValue("pattern", "e2e-${{ github.run_id }}-*"),
				HaveKeyWithValue("path", "artifacts/e2e"),
				HaveKeyWithValue("merge-multiple", false),
			)),
		)))
		Expect(steps).To(ContainElement(SatisfyAll(
			HaveKeyWithValue("uses", "actions/download-artifact@v4"),
			HaveKeyWithValue("with", SatisfyAll(
				HaveKeyWithValue("name", "blocking-quality-${{ github.run_id }}"),
				HaveKeyWithValue("path", "artifacts/quality"),
			)),
		)))

		By("验证矩阵全部 209 个 ID 与其中 71 个版本化 E2E 都受本次结果门禁约束")
		Expect(steps).To(ContainElement(SatisfyAll(
			HaveKeyWithValue("name", "校验一期全部 blocking JUnit"),
			HaveKeyWithValue("run", And(
				ContainSubstring("go run ./cmd/phase1-gate"),
				ContainSubstring("--reports artifacts"),
				ContainSubstring("--expected-blocking-count 209"),
				ContainSubstring("--expected-e2e-count 71"),
			)),
		)))

		By("确认最终门禁还会拒绝任一上游 blocking job 的非成功状态")
		Expect(steps).To(ContainElement(SatisfyAll(
			HaveKeyWithValue("name", "校验全部上游 blocking job"),
			HaveKeyWithValue("env", SatisfyAll(
				HaveKeyWithValue("QUALITY_RESULT", "${{ needs.quality.result }}"),
				HaveKeyWithValue("SMOKE_RESULT", "${{ needs.smoke.result }}"),
				HaveKeyWithValue("E2E_RESULT", "${{ needs.test-environment.result }}"),
			)),
			HaveKeyWithValue("run", And(
				ContainSubstring(`test "${QUALITY_RESULT}" = success`),
				ContainSubstring(`test "${SMOKE_RESULT}" = success`),
				ContainSubstring(`test "${E2E_RESULT}" = success`),
			)),
		)))

		By("验证 quality job 同时保存 UT、IT、Helm JUnit 与 unit coverage profile")
		quality := jobs["quality"].(map[string]any)
		qualitySteps := quality["steps"].([]any)
		Expect(qualitySteps).To(ContainElement(SatisfyAll(
			HaveKeyWithValue("uses", "actions/upload-artifact@v4"),
			HaveKeyWithValue("with", SatisfyAll(
				HaveKeyWithValue("name", "blocking-quality-${{ github.run_id }}"),
				HaveKeyWithValue("path", "artifacts/test-results/"),
			)),
		)))

		By("验证成功与失败证据显式保存 Pod digest、StatefulSet 与 PVC 状态")
		evidenceScript, err := os.ReadFile(filepath.Join(root, "hack", "test-env.sh"))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(evidenceScript)).To(ContainSubstring(`get pods --all-namespaces --output yaml`))
		Expect(string(evidenceScript)).To(ContainSubstring(`get statefulset,pvc --all-namespaces --output yaml`))
		Expect(string(evidenceScript)).To(ContainSubstring(`persistent-workloads.yaml`))
		Expect(strings.Count(string(contents), "paths:")).To(Equal(0), "blocking job 不得按 changed paths 跳过")
	})

	It("按两个固定版本和三个 E2E lane 并行运行，并在失败时先保存证据再清理", func() {
		By("读取 CI workflow 并定位双版本双 lane 验收 job")
		root, err := filepath.Abs(filepath.Join("..", ".."))
		Expect(err).NotTo(HaveOccurred())
		contents, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "ci.yml"))
		Expect(err).NotTo(HaveOccurred())

		workflow := map[string]any{}
		Expect(yaml.Unmarshal(contents, &workflow)).To(Succeed())
		jobs := workflow["jobs"].(map[string]any)
		job, found := jobs["test-environment"].(map[string]any)
		Expect(found).To(BeTrue(), "缺少 test-environment blocking job")
		Expect(job).To(HaveKeyWithValue("name", "e2e-${{ matrix.lane }} (Kubernetes ${{ matrix.kubernetes }})"))

		By("验证 Kubernetes 1.19 和 1.28 的 KSM、Agent、ServiceMonitor lane 使用六个独立矩阵入口")
		strategy := job["strategy"].(map[string]any)
		Expect(strategy["fail-fast"]).To(BeFalse())
		matrix := strategy["matrix"].(map[string]any)
		entries := matrix["include"].([]any)
		Expect(entries).To(HaveLen(6))
		Expect(entries).To(ConsistOf(
			SatisfyAll(
				HaveKeyWithValue("kubernetes", "1.19"),
				HaveKeyWithValue("kubectl", "v1.19.16"),
				HaveKeyWithValue("run_id_suffix", "119"),
				HaveKeyWithValue("lane", "ksm"),
				HaveKeyWithValue("target", "e2e-kubestatemetrics"),
			),
			SatisfyAll(
				HaveKeyWithValue("kubernetes", "1.19"),
				HaveKeyWithValue("kubectl", "v1.19.16"),
				HaveKeyWithValue("run_id_suffix", "119"),
				HaveKeyWithValue("lane", "agent"),
				HaveKeyWithValue("target", "e2e-agent"),
			),
			SatisfyAll(
				HaveKeyWithValue("kubernetes", "1.28"),
				HaveKeyWithValue("kubectl", "v1.28.15"),
				HaveKeyWithValue("run_id_suffix", "128"),
				HaveKeyWithValue("lane", "ksm"),
				HaveKeyWithValue("target", "e2e-kubestatemetrics"),
			),
			SatisfyAll(
				HaveKeyWithValue("kubernetes", "1.28"),
				HaveKeyWithValue("kubectl", "v1.28.15"),
				HaveKeyWithValue("run_id_suffix", "128"),
				HaveKeyWithValue("lane", "agent"),
				HaveKeyWithValue("target", "e2e-agent"),
			),
			SatisfyAll(
				HaveKeyWithValue("kubernetes", "1.19"),
				HaveKeyWithValue("kubectl", "v1.19.16"),
				HaveKeyWithValue("run_id_suffix", "119"),
				HaveKeyWithValue("lane", "sm"),
				HaveKeyWithValue("target", "e2e-servicemonitor"),
			),
			SatisfyAll(
				HaveKeyWithValue("kubernetes", "1.28"),
				HaveKeyWithValue("kubectl", "v1.28.15"),
				HaveKeyWithValue("run_id_suffix", "128"),
				HaveKeyWithValue("lane", "sm"),
				HaveKeyWithValue("target", "e2e-servicemonitor"),
			),
		))
		By("长 GitHub run ID 与每个 lane 组合后仍符合环境身份的 32 字符上限")
		for _, entry := range entries {
			item := entry.(map[string]any)
			runID := "ci-39949292548-" + item["run_id_suffix"].(string) + "-" + item["lane"].(string)
			Expect(len(runID)).To(BeNumerically("<=", 32), runID)
		}
		Expect(job["env"]).To(HaveKeyWithValue(
			"TEST_RUN_ID",
			"ci-${{ github.run_id }}-${{ matrix.run_id_suffix }}-${{ matrix.lane }}",
		))

		By("验证环境准备、Ginkgo E2E、报告上传、成功或失败证据与最终清理")
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
			HaveKeyWithValue("name", "E2E ${{ matrix.lane }}"),
			HaveKeyWithValue("run", "make ${{ matrix.target }} HELM=helm"),
		)))
		Expect(steps).To(ContainElement(SatisfyAll(
			HaveKeyWithValue("name", "上传 E2E 测试报告"),
			HaveKeyWithValue("if", "${{ always() }}"),
			HaveKeyWithValue("uses", "actions/upload-artifact@v4"),
			HaveKeyWithValue("with", SatisfyAll(
				HaveKeyWithValue("name", "e2e-${{ github.run_id }}-${{ matrix.run_id_suffix }}-${{ matrix.lane }}-${{ matrix.kubernetes }}"),
				HaveKeyWithValue("path", "artifacts/e2e/"),
			)),
		)))
		Expect(steps).To(ContainElement(SatisfyAll(
			HaveKeyWithValue("name", "保存测试环境证据"),
			HaveKeyWithValue("if", "${{ always() }}"),
			HaveKeyWithValue("continue-on-error", true),
		)))
		Expect(steps).To(ContainElement(SatisfyAll(
			HaveKeyWithValue("uses", "actions/upload-artifact@v4"),
			HaveKeyWithValue("if", "${{ always() }}"),
			HaveKeyWithValue("with", HaveKeyWithValue(
				"name",
				"test-environment-${{ matrix.run_id_suffix }}-${{ matrix.lane }}-${{ github.run_id }}",
			)),
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
		Expect(string(makefile)).To(ContainSubstring("e2e-preflight:"))
		Expect(string(makefile)).To(ContainSubstring(`--output-dir="$(E2E_REPORT_DIR)/preflight"`))
		Expect(string(makefile)).To(ContainSubstring(`--output-dir="$(E2E_REPORT_DIR)/collection"`))
		Expect(string(makefile)).To(ContainSubstring(`--output-dir="$(E2E_REPORT_DIR)/kubestatemetrics"`))
		Expect(string(makefile)).NotTo(ContainSubstring("./hack/e2e-default-deployment.sh"))

		By("确认旧 shell 业务断言入口已被删除")
		_, err = os.Stat(filepath.Join(root, "hack", "e2e-default-deployment.sh"))
		Expect(os.IsNotExist(err)).To(BeTrue())
	})
})
