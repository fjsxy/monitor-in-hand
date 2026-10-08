package phase1gate_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/fjsxy/monitor-in-hand/internal/phase1gate"
)

var _ = Describe("一期发布门禁", func() {
	It("UT-GATE-GH65-001 从验收矩阵提取唯一的全部 blocking ID", func() {
		By("准备同时包含 UT、IT、HELM 与双版本 E2E 场景的矩阵")
		unitID := "UT-GATE-GH" + "65-999"
		integrationID := "IT-KSM-GH" + "65-999"
		helmID := "HELM-GH" + "65-999"
		matrix := []byte(strings.Join([]string{
			"| 场景 ID | 来源 |",
			"| --- | --- |",
			"| `UT-GATE-GH" + "65-999` | #65 |",
			"| `IT-KSM-GH" + "65-999` | #65 |",
			"| `HELM-GH" + "65-999` | #65 |",
			"| `" + e2eID("119", "001") + "` / `" + e2eID("128", "001") + "` | #65 |",
		}, "\n"))

		By("提取并验证按字典序排列的唯一 blocking ID")
		ids, err := phase1gate.BlockingIDs(matrix)
		Expect(err).NotTo(HaveOccurred())
		Expect(ids).To(Equal([]string{
			e2eID("119", "001"), e2eID("128", "001"),
			helmID, integrationID, unitID,
		}))
		e2eIDs, err := phase1gate.BlockingE2EIDs(matrix)
		Expect(err).NotTo(HaveOccurred())
		Expect(e2eIDs).To(Equal([]string{e2eID("119", "001"), e2eID("128", "001")}))

		By("验证重复 ID 与空矩阵都被明确拒绝")
		_, err = phase1gate.BlockingIDs(append(matrix, []byte("\n| `"+unitID+"` | #65 |")...))
		Expect(err).To(MatchError(ContainSubstring("重复")))

		_, err = phase1gate.BlockingIDs([]byte("| 场景 ID | 来源 |"))
		Expect(err).To(MatchError(ContainSubstring("没有 blocking ID")))
	})

	It("UT-GATE-GH65-002 仅在本次 JUnit 覆盖全部 blocking ID 且 E2E 结果唯一时放行", func() {
		unitID := "UT-GATE-GH" + "65-999"
		integrationID := "IT-KSM-GH" + "65-999"
		helmID := "HELM-GH" + "65-999"
		By("准备两份分别来自 Kubernetes 1.19 和 1.28 KSM lane 的成功 JUnit")
		reports := GinkgoT().TempDir()
		writeJUnit(reports, "ci-run-119-ksm-1.19/kubestatemetrics", []junitCase{
			{name: "[" + e2eID("119", "001") + "] first", className: e2eID("119", "001")},
		})
		writeJUnit(reports, "ci-run-128-ksm-1.28/kubestatemetrics", []junitCase{
			{name: "[" + e2eID("128", "001") + "] second"},
		})
		writeJUnit(reports, "quality/unit", []junitCase{
			{name: unitID + " unit"},
			{name: integrationID + " integration"},
			{name: helmID + " chart"},
		})
		Expect(os.Rename(
			filepath.Join(reports, "quality/unit/junit.xml"),
			filepath.Join(reports, "quality/unit/internal_phase1gate_junit.xml"),
		)).To(Succeed())

		By("验证全部预期 ID 都作为成功结果被计数")
		result, err := phase1gate.VerifyJUnitResults(
			[]string{e2eID("119", "001"), e2eID("128", "001"), unitID, integrationID, helmID},
			reports,
		)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Passed).To(Equal(5))
		Expect(result.ReportFiles).To(Equal(3))
	})

	It("UT-GATE-GH65-003 拒绝各层缺失或非成功以及 E2E 重复和错误版本/lane", func() {
		skippedUnitID := "UT-GATE-GH" + "65-997"
		By("准备覆盖所有拒绝分类的混合 JUnit")
		reports := GinkgoT().TempDir()
		writeJUnit(reports, "ci-run-119-ksm-1.19/kubestatemetrics", []junitCase{
			{name: "[" + e2eID("119", "002") + "] failed", outcome: "failure"},
			{name: "[" + e2eID("119", "003") + "] error", outcome: "error"},
			{name: "[" + e2eID("119", "004") + "] skipped", outcome: "skipped"},
			{name: "[" + e2eID("119", "005") + "] duplicate"},
			{name: "[" + e2eID("119", "005") + "] duplicate again"},
			{name: "[" + e2eID("128", "999") + "] outside matrix"},
		})
		writeJUnit(reports, "ci-run-119-agent-1.19/collection", []junitCase{
			{name: "[" + e2eID("119", "006") + "] wrong lane"},
		})
		writeJUnit(reports, "quality/unit", []junitCase{
			{name: skippedUnitID + " skipped unit", outcome: "skipped"},
		})

		By("一次性验证各拒绝分类都保留对应的稳定场景 ID")
		_, err := phase1gate.VerifyJUnitResults([]string{
			e2eID("119", "001"),
			e2eID("119", "002"),
			e2eID("119", "003"),
			e2eID("119", "004"),
			e2eID("119", "005"),
			e2eID("119", "006"),
			skippedUnitID,
		}, reports)
		Expect(err).To(MatchError(SatisfyAll(
			ContainSubstring(e2eID("119", "001")+" 缺失"),
			ContainSubstring(e2eID("119", "002")+" 失败"),
			ContainSubstring(e2eID("119", "003")+" 错误"),
			ContainSubstring(e2eID("119", "004")+" 被跳过"),
			ContainSubstring(e2eID("119", "005")+" 出现 2 次"),
			ContainSubstring(e2eID("119", "006")+" JUnit 来源不匹配"),
			ContainSubstring(skippedUnitID+" 被跳过"),
		)))

		By("验证非法 expected 集合在读取报告前失败")
		_, err = phase1gate.VerifyJUnitResults(nil, reports)
		Expect(err).To(MatchError(ContainSubstring("没有待验证")))
		_, err = phase1gate.VerifyJUnitResults([]string{"not-an-id"}, reports)
		Expect(err).To(MatchError(ContainSubstring("非法 blocking ID")))
		_, err = phase1gate.VerifyJUnitResults([]string{e2eID("119", "001"), e2eID("119", "001")}, reports)
		Expect(err).To(MatchError(ContainSubstring("重复")))

		By("验证损坏或不存在的 JUnit 目录返回可定位的读取错误")
		brokenReports := GinkgoT().TempDir()
		Expect(os.WriteFile(filepath.Join(brokenReports, "junit.xml"), []byte("<testsuite>"), 0o600)).To(Succeed())
		_, err = phase1gate.VerifyJUnitResults([]string{e2eID("119", "001")}, brokenReports)
		Expect(err).To(MatchError(ContainSubstring("解析")))
		_, err = phase1gate.VerifyJUnitResults([]string{e2eID("119", "001")}, filepath.Join(brokenReports, "missing"))
		Expect(err).To(MatchError(ContainSubstring("读取 JUnit")))
	})

	It("UT-GATE-GH65-004 同时执行 unit statement coverage 总体 80% 与逐核心包 70% 阈值", func() {
		By("准备总体 85%、每包至少 80% 的独立 coverage profile")
		profile := []byte(strings.Join([]string{
			"mode: atomic",
			"example.test/internal/alpha/a.go:1.1,2.1 8 1",
			"example.test/internal/alpha/a.go:3.1,4.1 2 0",
			"example.test/internal/beta/b.go:1.1,2.1 9 1",
			"example.test/internal/beta/b.go:3.1,4.1 1 0",
		}, "\n"))

		By("验证达标 profile 返回总体和逐包精确比例")
		summary, err := phase1gate.VerifyCoverage(profile, 80, 70)
		Expect(err).NotTo(HaveOccurred())
		Expect(summary.TotalPercent).To(BeNumerically("==", 85))
		Expect(summary.PackagePercent).To(HaveKeyWithValue("example.test/internal/alpha", BeNumerically("==", 80)))

		By("验证总体与逐包阈值同时失败时汇总全部问题")
		_, err = phase1gate.VerifyCoverage(profile, 90, 85)
		Expect(err).To(MatchError(SatisfyAll(
			ContainSubstring("总体 statement coverage 85.000% 低于 90.0%"),
			ContainSubstring("example.test/internal/alpha statement coverage 80.000% 低于 85.0%"),
		)))

		By("验证真实比例略低于阈值时不能通过显示值舍入放行")
		boundaryProfile := []byte(strings.Join([]string{
			"mode: atomic",
			"example.test/internal/alpha/a.go:1.1,2.1 7999999 1",
			"example.test/internal/alpha/a.go:3.1,4.1 2000001 0",
		}, "\n"))
		_, err = phase1gate.VerifyCoverage(boundaryProfile, 80, 70)
		Expect(err).To(MatchError(ContainSubstring("80.000% 低于 80.0%")))

		By("验证畸形 profile 在计算阈值前被拒绝")
		_, err = phase1gate.VerifyCoverage([]byte("missing header"), 80, 70)
		Expect(err).To(MatchError(ContainSubstring("缺少 mode header")))
		_, err = phase1gate.VerifyCoverage([]byte("mode: atomic"), 80, 70)
		Expect(err).To(MatchError(ContainSubstring("没有 statement")))
		_, err = phase1gate.VerifyCoverage([]byte("mode: atomic\ninvalid"), 80, 70)
		Expect(err).To(MatchError(ContainSubstring("格式错误")))
		_, err = phase1gate.VerifyCoverage([]byte("mode: atomic\ninvalid/path.go:1.1,2.1 nope 1"), 80, 70)
		Expect(err).To(MatchError(ContainSubstring("statement 数无效")))
		_, err = phase1gate.VerifyCoverage([]byte("mode: atomic\ninvalid/path.go:1.1,2.1 1 nope"), 80, 70)
		Expect(err).To(MatchError(ContainSubstring("count 无效")))

		By("验证多个测试二进制产生的同一 block 只计一次且合并覆盖状态")
		mergedProfile := []byte(strings.Join([]string{
			"mode: atomic",
			"example.test/internal/alpha/a.go:1.1,2.1 10 0",
			"example.test/internal/alpha/a.go:1.1,2.1 10 1",
		}, "\n"))
		merged, err := phase1gate.VerifyCoverage(mergedProfile, 100, 100)
		Expect(err).NotTo(HaveOccurred())
		Expect(merged.TotalPercent).To(BeNumerically("==", 100))
	})

	It("UT-GATE-GH65-006 核心 unit test 只保留 Ginkgo suite 入口", func() {
		By("扫描核心 unit 目录中的全部 Go 测试声明")
		root, err := filepath.Abs(filepath.Join("..", ".."))
		Expect(err).NotTo(HaveOccurred())

		var nonGinkgoTests []string
		for _, directory := range []string{"internal", "cmd"} {
			err := filepath.WalkDir(filepath.Join(root, directory), func(path string, entry fs.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if entry.IsDir() || !strings.HasSuffix(entry.Name(), "_test.go") {
					return nil
				}
				file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
				if err != nil {
					return err
				}
				for _, declaration := range file.Decls {
					function, ok := declaration.(*ast.FuncDecl)
					if !ok || !strings.HasPrefix(function.Name.Name, "Test") {
						continue
					}
					usesRunSpecs := false
					ast.Inspect(function.Body, func(node ast.Node) bool {
						identifier, ok := node.(*ast.Ident)
						if ok && identifier.Name == "RunSpecs" {
							usesRunSpecs = true
						}
						return true
					})
					if !usesRunSpecs {
						nonGinkgoTests = append(nonGinkgoTests, path+":"+function.Name.Name)
					}
				}
				return nil
			})
			Expect(err).NotTo(HaveOccurred())
		}
		sort.Strings(nonGinkgoTests)

		By("验证每个 Test 入口都调用 RunSpecs 进入统一 Ginkgo 报告")
		Expect(nonGinkgoTests).To(BeEmpty(), "核心 unit test 必须通过 Ginkgo suite 进入统一报告")
	})
})

type junitCase struct {
	name      string
	className string
	outcome   string
}

func e2eID(version, sequence string) string {
	return "E2E-" + version + "-KSM-GH65-" + sequence
}

func writeJUnit(root, lane string, cases []junitCase) {
	directory := filepath.Join(root, lane)
	Expect(os.MkdirAll(directory, 0o755)).To(Succeed())
	var body strings.Builder
	body.WriteString(`<?xml version="1.0" encoding="UTF-8"?><testsuite>`)
	for _, testCase := range cases {
		body.WriteString(`<testcase name="` + testCase.name + `" classname="` + testCase.className + `">`)
		if testCase.outcome != "" {
			body.WriteString(`<` + testCase.outcome + ` message="synthetic"/>`)
		}
		body.WriteString(`</testcase>`)
	}
	body.WriteString(`</testsuite>`)
	Expect(os.WriteFile(filepath.Join(directory, "junit.xml"), []byte(body.String()), 0o600)).To(Succeed())
}
